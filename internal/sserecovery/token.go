// Package sserecovery implements the optional signed recovery cursor used by
// the SSE transport. A cursor carries the per-channel stream positions already
// delivered to a client, bound to the authenticated user and the exact set of
// channels the subscription covered. It is a stateless, integrity-protected
// hint: it never authorizes anything, and every channel it references still
// goes through the normal subscription permission checks on reconnect.
package sserecovery

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Version identifies the cursor format.
const Version byte = '1'

// futureSkewSeconds bounds how far in the future an accepted iat may be.
const futureSkewSeconds = 60

// Channel is a delivered stream position for a single channel.
type Channel struct {
	Channel string
	Epoch   string
	Offset  uint64
}

// Cursor is the verified content of a recovery token.
type Cursor struct {
	User     string
	Iat      int64 // Unix seconds the cursor was minted.
	Channels []Channel
}

// payload is the canonical, JSON-encoded body covered by the signature. Field
// names are short: a cursor may ride in a URL query param on every reconnect.
type payload struct {
	V   byte           `json:"v"`
	UID string         `json:"u"`
	Iat int64          `json:"i"`
	Sub []payloadEntry `json:"s"`
}

type payloadEntry struct {
	C string `json:"c"`
	E string `json:"e,omitempty"`
	O uint64 `json:"o"`
}

// Limits bounds the resources a single cursor may consume.
type Limits struct {
	// MaxTokenLength bounds the raw, encoded token length accepted on input.
	MaxTokenLength int
	// MaxChannels bounds the number of channels in a single cursor.
	MaxChannels int
	// MaxChannelLength bounds the byte length of a single channel name.
	MaxChannelLength int
	// TTL is how long a cursor is accepted after it was minted.
	TTL time.Duration
}

func (l Limits) withDefaults() Limits {
	if l.MaxTokenLength <= 0 {
		l.MaxTokenLength = 4096
	}
	if l.MaxChannels <= 0 {
		l.MaxChannels = 128
	}
	if l.MaxChannelLength <= 0 {
		l.MaxChannelLength = 255
	}
	if l.TTL <= 0 {
		l.TTL = 5 * time.Minute
	}
	return l
}

// Signer mints recovery cursors with the current signing key and verifies
// cursors against the current key and, for a bounded window after a rotation,
// recently retired keys.
type Signer struct {
	now    func() time.Time
	limits Limits

	currentID  string
	currentKey []byte

	oldIDs    []string
	oldKeys   [][]byte
	oldExpiry []int64
}

// Option configures a Signer.
type Option func(*Signer)

// WithNow overrides the clock (used in tests).
func WithNow(now func() time.Time) Option {
	return func(s *Signer) {
		if now != nil {
			s.now = now
		}
	}
}

// WithOldKey registers a retired signing key that remains acceptable until
// maxAge after the moment it was registered. Register the same key ID more
// than once to extend its window.
func WithOldKey(id string, key []byte, maxAge time.Duration) Option {
	return func(s *Signer) {
		if id == "" || len(key) == 0 || maxAge <= 0 {
			return
		}
		expiry := s.now().Add(maxAge).Unix()
		for i := range s.oldIDs {
			if s.oldIDs[i] == id {
				s.oldKeys[i] = append([]byte(nil), key...)
				s.oldExpiry[i] = expiry
				return
			}
		}
		s.oldIDs = append(s.oldIDs, id)
		s.oldKeys = append(s.oldKeys, append([]byte(nil), key...))
		s.oldExpiry = append(s.oldExpiry, expiry)
	}
}

// NewSigner creates a Signer. The current key signs new cursors; old keys are
// only accepted for verification within their bounded windows.
func NewSigner(keyID string, key []byte, limits Limits, opts ...Option) (*Signer, error) {
	if keyID == "" {
		return nil, errors.New("sserecovery: signing key id required")
	}
	if len(key) == 0 {
		return nil, errors.New("sserecovery: signing key required")
	}
	s := &Signer{
		now:        time.Now,
		limits:     limits.withDefaults(),
		currentID:  keyID,
		currentKey: append([]byte(nil), key...),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

var (
	errMalformed       = errors.New("sserecovery: malformed cursor")
	errUnsupported     = errors.New("sserecovery: unsupported cursor version")
	errUnknownKey      = errors.New("sserecovery: unknown signing key")
	errBadSignature    = errors.New("sserecovery: cursor signature mismatch")
	errExpired         = errors.New("sserecovery: cursor expired")
	errTooLarge        = errors.New("sserecovery: cursor too large")
	errTooManyChannels = errors.New("sserecovery: too many channels in cursor")
	errChannelTooLong  = errors.New("sserecovery: channel name too long in cursor")
	errEmptyChannel    = errors.New("sserecovery: empty channel in cursor")
)

// IsRejected reports whether verification failed for a reason the client
// cannot repair by retrying the same token (bad shape, signature, or expiry).
func IsRejected(err error) bool {
	return errors.Is(err, errMalformed) || errors.Is(err, errUnsupported) ||
		errors.Is(err, errBadSignature) || errors.Is(err, errUnknownKey) ||
		errors.Is(err, errExpired) || errors.Is(err, errTooLarge) ||
		errors.Is(err, errTooManyChannels) || errors.Is(err, errChannelTooLong) ||
		errors.Is(err, errEmptyChannel)
}

func encodePayload(nowUnix int64, c Cursor) ([]byte, error) {
	p := payload{
		V:   Version,
		UID: c.User,
		Iat: nowUnix,
		Sub: make([]payloadEntry, 0, len(c.Channels)),
	}
	for _, ch := range c.Channels {
		p.Sub = append(p.Sub, payloadEntry{C: ch.Channel, E: ch.Epoch, O: ch.Offset})
	}
	// Deterministic encoding keeps the signature stable and lets Verify reject
	// reordered or duplicated entries.
	sort.Slice(p.Sub, func(i, j int) bool { return p.Sub[i].C < p.Sub[j].C })
	return json.Marshal(p)
}

func sign(key, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return mac.Sum(nil)
}

// Issue mints a signed, URL-safe cursor token for the given positions.
// Channels must be unique; they are sorted on encoding.
func (s *Signer) Issue(c Cursor) (string, error) {
	if len(c.Channels) > s.limits.MaxChannels {
		return "", errTooManyChannels
	}
	seen := make(map[string]struct{}, len(c.Channels))
	for _, ch := range c.Channels {
		if ch.Channel == "" {
			return "", errEmptyChannel
		}
		if len(ch.Channel) > s.limits.MaxChannelLength {
			return "", errChannelTooLong
		}
		if _, ok := seen[ch.Channel]; ok {
			return "", fmt.Errorf("sserecovery: duplicate channel %q", ch.Channel)
		}
		seen[ch.Channel] = struct{}{}
	}
	if c.Iat == 0 {
		c.Iat = s.now().Unix()
	}
	body, err := encodePayload(c.Iat, c)
	if err != nil {
		return "", err
	}
	sig := sign(s.currentKey, body)

	var out bytes.Buffer
	out.Grow(1 + len(s.currentID) + 1 + base64.RawURLEncoding.EncodedLen(len(body)) +
		1 + base64.RawURLEncoding.EncodedLen(len(sig)))
	out.WriteByte(Version)
	out.WriteString(s.currentID)
	out.WriteByte('.')
	out.WriteString(base64.RawURLEncoding.EncodeToString(body))
	out.WriteByte('.')
	out.WriteString(base64.RawURLEncoding.EncodeToString(sig))
	token := out.String()
	if len(token) > s.limits.MaxTokenLength {
		return "", errTooLarge
	}
	return token, nil
}

// Verify checks the token's shape, age, resource limits and signature, then
// returns its content. It never infers identity or permission from the token.
func (s *Signer) Verify(token string) (Cursor, error) {
	return s.verifyAt(token, s.now().Unix())
}

func (s *Signer) verifyAt(token string, nowUnix int64) (Cursor, error) {
	if len(token) == 0 || len(token) > s.limits.MaxTokenLength {
		return Cursor{}, errTooLarge
	}
	if token[0] != Version {
		return Cursor{}, errUnsupported
	}
	rest := token[1:]
	dot := bytes.IndexByte([]byte(rest), '.')
	if dot <= 0 {
		return Cursor{}, errMalformed
	}
	keyID := rest[:dot]
	tail := rest[dot+1:]
	dot2 := bytes.IndexByte([]byte(tail), '.')
	if dot2 <= 0 || dot2 == len(tail)-1 {
		return Cursor{}, errMalformed
	}
	bodyB64, sigB64 := tail[:dot2], tail[dot2+1:]

	body, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return Cursor{}, errMalformed
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return Cursor{}, errMalformed
	}

	key := s.keyForID(keyID, nowUnix)
	if key == nil {
		return Cursor{}, errUnknownKey
	}

	if subtle.ConstantTimeCompare(gotSig, sign(key, body)) != 1 {
		return Cursor{}, errBadSignature
	}

	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Cursor{}, errMalformed
	}
	if p.V != Version {
		return Cursor{}, errUnsupported
	}
	ttlSeconds := int64(s.limits.TTL / time.Second)
	if p.Iat == 0 || nowUnix-p.Iat > ttlSeconds || p.Iat-nowUnix > futureSkewSeconds {
		return Cursor{}, errExpired
	}
	if len(p.Sub) > s.limits.MaxChannels {
		return Cursor{}, errTooManyChannels
	}

	c := Cursor{User: p.UID, Iat: p.Iat, Channels: make([]Channel, 0, len(p.Sub))}
	var prev string
	for i, e := range p.Sub {
		if e.C == "" {
			return Cursor{}, errEmptyChannel
		}
		if len(e.C) > s.limits.MaxChannelLength {
			return Cursor{}, errChannelTooLong
		}
		if i > 0 && e.C <= prev {
			return Cursor{}, errMalformed
		}
		prev = e.C
		c.Channels = append(c.Channels, Channel{Channel: e.C, Epoch: e.E, Offset: e.O})
	}
	return c, nil
}

// keyForID returns the secret for an id, accepting a retired key only while its
// verification window is open.
func (s *Signer) keyForID(id string, nowUnix int64) []byte {
	if subtle.ConstantTimeCompare([]byte(id), []byte(s.currentID)) == 1 {
		return s.currentKey
	}
	for i, oldID := range s.oldIDs {
		if nowUnix > s.oldExpiry[i] {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(id), []byte(oldID)) == 1 {
			return s.oldKeys[i]
		}
	}
	return nil
}

// ChannelSetEqual reports whether the cursor's channel set exactly matches the
// given channels. Order does not matter.
func (c Cursor) ChannelSetEqual(channels []string) bool {
	if len(c.Channels) != len(channels) {
		return false
	}
	have := make(map[string]struct{}, len(c.Channels))
	for _, ch := range c.Channels {
		have[ch.Channel] = struct{}{}
	}
	for _, ch := range channels {
		if _, ok := have[ch]; !ok {
			return false
		}
	}
	return true
}

// Position returns the epoch/offset recorded for a channel, if present.
func (c Cursor) Position(channel string) (Channel, bool) {
	for _, ch := range c.Channels {
		if ch.Channel == channel {
			return ch, true
		}
	}
	return Channel{}, false
}
