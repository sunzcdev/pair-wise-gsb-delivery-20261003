// Package kencrypt provides an optional application-layer record envelope
// encryption layer on top of kgo.
//
// The layer encrypts only record values: keys stay in plaintext so that
// partitioning and routing are unaffected, and nil values (tombstones) are
// passed through untouched so delete semantics are preserved. Each envelope
// carries a format version and the key ID that was used, which allows keys
// to be rotated while old records remain decryptable. The record's topic,
// key, and a configurable set of headers are authenticated (but not
// encrypted), so a record that is copied to another topic, re-keyed, or has
// an authenticated header modified fails decryption.
//
// Encryption is idempotent: a record that already carries an envelope is
// left as-is, so retrying a produce of the same record never wraps it
// twice. Decryption is atomic: the plaintext is fully verified before the
// record is modified, so a failed decrypt never exposes partial plaintext
// to the application.
//
// The layer can be used directly with [Cipher.EncryptRecord] and
// [Cipher.DecryptRecord], or transparently via [Cipher.Hook], which
// implements kgo.HookProduceRecordBuffered and kgo.HookFetchRecordBuffered.
package kencrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/twmb/franz-go/pkg/kgo"
)

// ErrUnknownKeyID is returned (possibly wrapped) when a record references a
// key ID that the configured KeyProvider cannot resolve.
var ErrUnknownKeyID = errors.New("kencrypt: unknown key ID")

const (
	// Version1 is the only envelope version currently defined.
	Version1 = 1

	keySize   = 32 // AES-256
	nonceSize = 12 // AES-GCM standard nonce
)

// magic prefixes every envelope so that enveloped values can be told apart
// from plaintext values, and so that encryption is idempotent.
var magic = []byte{0xeb, 0xc1}

// KeyProvider supplies AES-256 keys (32 bytes) to a Cipher. Implementations
// must be safe for concurrent use.
type KeyProvider interface {
	// EncryptKey returns the key ID and key to encrypt new records with.
	EncryptKey() (keyID string, key []byte, err error)
	// DecryptKey returns the key for a previously used key ID. It must
	// return an error wrapping ErrUnknownKeyID if the ID is not known.
	DecryptKey(keyID string) ([]byte, error)
}

// MapProvider is a KeyProvider backed by an in-memory map of key IDs to
// keys. It supports rotation: AddKey registers a new key, Rotate changes
// which key encrypts new records, and old keys remain usable for decrypt.
type MapProvider struct {
	mu      sync.RWMutex
	current string
	keys    map[string][]byte
}

// NewMapProvider returns a MapProvider that encrypts with keys[current].
// All keys must be 32 bytes and current must exist in keys.
func NewMapProvider(current string, keys map[string][]byte) (*MapProvider, error) {
	m := &MapProvider{keys: make(map[string][]byte, len(keys))}
	for id, key := range keys {
		if err := m.set(id, key); err != nil {
			return nil, err
		}
	}
	if _, ok := m.keys[current]; !ok {
		return nil, fmt.Errorf("kencrypt: current key %q is not in the key set", current)
	}
	m.current = current
	return m, nil
}

func (m *MapProvider) set(id string, key []byte) error {
	if len(key) != keySize {
		return fmt.Errorf("kencrypt: key %q is %d bytes, want %d", id, len(key), keySize)
	}
	m.keys[id] = slices.Clone(key)
	return nil
}

// AddKey registers a key. It does not change which key encrypts new
// records; use Rotate for that.
func (m *MapProvider) AddKey(id string, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set(id, key)
}

// Rotate changes which key encrypts new records to a previously added key.
// Old keys remain usable for decryption.
func (m *MapProvider) Rotate(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[id]; !ok {
		return fmt.Errorf("kencrypt: cannot rotate to %q: %w", id, ErrUnknownKeyID)
	}
	m.current = id
	return nil
}

// EncryptKey implements KeyProvider.
func (m *MapProvider) EncryptKey() (string, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.keys[m.current]
	if !ok {
		return "", nil, fmt.Errorf("kencrypt: current key %q: %w", m.current, ErrUnknownKeyID)
	}
	return m.current, key, nil
}

// DecryptKey implements KeyProvider.
func (m *MapProvider) DecryptKey(id string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.keys[id]
	if !ok {
		return nil, fmt.Errorf("kencrypt: %w: %q", ErrUnknownKeyID, id)
	}
	return key, nil
}

// Opt configures a Cipher.
type Opt interface{ apply(*Cipher) }

type opt struct{ fn func(*Cipher) }

func (o opt) apply(c *Cipher) { o.fn(c) }

// AuthenticateHeaders marks the named headers as authenticated: their
// values are folded into the encryption's associated data, so modifying or
// removing them after encryption causes decryption to fail. Headers not
// listed here are neither encrypted nor authenticated.
func AuthenticateHeaders(keys ...string) Opt {
	return opt{func(c *Cipher) {
		c.headers = slices.Clone(keys)
		slices.Sort(c.headers)
		c.headers = slices.Compact(c.headers)
	}}
}

// Cipher encrypts and decrypts record values using envelopes. It is safe
// for concurrent use.
type Cipher struct {
	kp      KeyProvider
	headers []string
	rand    io.Reader
}

// NewCipher returns a Cipher that encrypts and decrypts with keys from kp.
func NewCipher(kp KeyProvider, opts ...Opt) *Cipher {
	c := &Cipher{kp: kp, rand: rand.Reader}
	for _, o := range opts {
		o.apply(c)
	}
	return c
}

// EncryptRecord replaces r.Value with an authenticated envelope, unless
// r.Value is nil (a tombstone, which is left nil) or already enveloped
// (left as-is, so retrying a produce never wraps a record twice). The
// record's topic, key, and authenticated headers are bound into the
// envelope but left in plaintext.
func (c *Cipher) EncryptRecord(r *kgo.Record) error {
	if r.Value == nil {
		return nil
	}
	if hasMagic(r.Value) {
		if r.Value[2] == Version1 {
			return nil
		}
		return fmt.Errorf("kencrypt: record already enveloped with unsupported version %d", r.Value[2])
	}
	id, key, err := c.kp.EncryptKey()
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(c.rand, nonce); err != nil {
		return fmt.Errorf("kencrypt: generating nonce: %w", err)
	}
	env := make([]byte, 0, len(magic)+1+len(id)+nonceSize+len(r.Value)+gcm.Overhead())
	env = append(env, magic...)
	env = append(env, Version1)
	env = binary.AppendUvarint(env, uint64(len(id)))
	env = append(env, id...)
	env = append(env, nonce...)
	env = gcm.Seal(env, nonce, r.Value, c.aad(r))
	r.Value = env
	return nil
}

// DecryptRecord verifies and removes the envelope on r.Value, restoring the
// original plaintext. Values that are nil (tombstones) or not enveloped are
// passed through unchanged, which allows mixed plaintext/encrypted topics.
//
// Decryption is atomic: on any error -- unknown key ID, tampered topic, key
// or authenticated headers, corrupted ciphertext -- the record is left
// exactly as it was and an error is returned, so partial plaintext never
// reaches the application.
func (c *Cipher) DecryptRecord(r *kgo.Record) error {
	if r.Value == nil || !hasMagic(r.Value) {
		return nil
	}
	if r.Value[2] != Version1 {
		return fmt.Errorf("kencrypt: unsupported envelope version %d", r.Value[2])
	}
	rest := r.Value[len(magic)+1:]
	idLen, n := binary.Uvarint(rest)
	if n <= 0 || uint64(len(rest[n:])) < idLen {
		return errors.New("kencrypt: malformed envelope key ID")
	}
	id := string(rest[n : n+int(idLen)])
	rest = rest[n+int(idLen):]
	if len(rest) < nonceSize {
		return errors.New("kencrypt: malformed envelope nonce")
	}
	nonce, ciphertext := rest[:nonceSize], rest[nonceSize:]
	key, err := c.kp.DecryptKey(id)
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, c.aad(r))
	if err != nil {
		return fmt.Errorf("kencrypt: decrypting record in topic %q: %w", r.Topic, err)
	}
	if plain == nil {
		// Only non-nil values are ever enveloped, so an empty plaintext
		// must round trip as an empty non-nil value, not a tombstone.
		plain = []byte{}
	}
	r.Value = plain
	return nil
}

func hasMagic(v []byte) bool {
	return len(v) >= len(magic)+1 && v[0] == magic[0] && v[1] == magic[1]
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("kencrypt: key is %d bytes, want %d", len(key), keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("kencrypt: %w", err)
	}
	return cipher.NewGCM(block)
}

// aad builds the associated data that authenticates (without encrypting)
// the record's topic, key, and configured headers. Every field is length
// prefixed so the encoding is unambiguous.
func (c *Cipher) aad(r *kgo.Record) []byte {
	var b []byte
	b = binary.AppendUvarint(b, uint64(len(r.Topic)))
	b = append(b, r.Topic...)
	b = binary.AppendUvarint(b, uint64(len(r.Key)))
	b = append(b, r.Key...)
	for _, name := range c.headers {
		b = binary.AppendUvarint(b, uint64(len(name)))
		b = append(b, name...)
		var n uint64
		for _, h := range r.Headers {
			if h.Key == name {
				n++
			}
		}
		b = binary.AppendUvarint(b, n)
		for _, h := range r.Headers {
			if h.Key == name {
				b = binary.AppendUvarint(b, uint64(len(h.Value)))
				b = append(b, h.Value...)
			}
		}
	}
	return b
}

// Hook returns a kgo.Hook that transparently encrypts records as they are
// buffered for produce and decrypts records as they are buffered from
// fetches. The hook fires once per record as it enters the client, so
// client-internal produce retries never re-encrypt a record.
//
// Hook methods cannot return errors, so failures are reported to onError
// (which may be nil). A record that fails to decrypt is delivered still
// enveloped; a record that fails to encrypt is produced in plaintext, so
// applications that require encryption must treat onError as fatal for that
// record.
func (c *Cipher) Hook(onError func(error)) kgo.Hook {
	return &hook{c: c, onError: onError}
}

type hook struct {
	c       *Cipher
	onError func(error)
}

func (h *hook) OnProduceRecordBuffered(r *kgo.Record) {
	if err := h.c.EncryptRecord(r); err != nil && h.onError != nil {
		h.onError(err)
	}
}

func (h *hook) OnFetchRecordBuffered(r *kgo.Record) {
	if err := h.c.DecryptRecord(r); err != nil && h.onError != nil {
		h.onError(err)
	}
}
