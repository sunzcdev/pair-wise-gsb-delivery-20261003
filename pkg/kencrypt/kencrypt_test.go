package kencrypt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
)

func testKey(b byte) []byte {
	return bytes.Repeat([]byte{b}, keySize)
}

func testProvider(t *testing.T, current string, keys map[string][]byte) *MapProvider {
	t.Helper()
	p, err := NewMapProvider(current, keys)
	if err != nil {
		t.Fatalf("NewMapProvider: %v", err)
	}
	return p
}

func TestRoundTrip(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}),
		AuthenticateHeaders("tenant", "trace-id"))
	r := &kgo.Record{
		Topic: "secure",
		Key:   []byte("routing-key"),
		Value: []byte("secret payload"),
		Headers: []kgo.RecordHeader{
			{Key: "tenant", Value: []byte("acme")},
			{Key: "unauthenticated", Value: []byte("plain")},
		},
	}
	if err := c.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(r.Value, []byte("secret payload")) {
		t.Fatal("value was not encrypted")
	}
	if !bytes.Equal(r.Key, []byte("routing-key")) {
		t.Fatal("key must stay plaintext for routing")
	}
	if err := c.DecryptRecord(r); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(r.Value, []byte("secret payload")) {
		t.Fatalf("round trip got %q", r.Value)
	}
}

func TestTombstonePreserved(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	r := &kgo.Record{Topic: "secure", Key: []byte("k"), Value: nil}
	if err := c.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if r.Value != nil {
		t.Fatal("tombstone was encrypted into a non-nil value")
	}
	if err := c.DecryptRecord(r); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if r.Value != nil {
		t.Fatal("tombstone did not survive decrypt")
	}
}

func TestEmptyValueRoundTrip(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	r := &kgo.Record{Topic: "secure", Value: []byte{}}
	if err := c.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := c.DecryptRecord(r); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if r.Value == nil || len(r.Value) != 0 {
		t.Fatalf("empty non-nil value round tripped as %v", r.Value)
	}
}

func TestEncryptIdempotentOnRetry(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	r := &kgo.Record{Topic: "secure", Value: []byte("payload")}
	if err := c.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	first := slices.Clone(r.Value)
	// Simulates a retry of the same pending record: the record must not be
	// wrapped a second time.
	for range 3 {
		if err := c.EncryptRecord(r); err != nil {
			t.Fatalf("re-encrypt: %v", err)
		}
		if !bytes.Equal(r.Value, first) {
			t.Fatal("retry re-encrypted an already enveloped record")
		}
	}
	if err := c.DecryptRecord(r); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(r.Value, []byte("payload")) {
		t.Fatalf("got %q", r.Value)
	}
}

func TestKeyRotation(t *testing.T) {
	p := testProvider(t, "k1", map[string][]byte{"k1": testKey(1)})
	c := NewCipher(p)

	old := &kgo.Record{Topic: "secure", Value: []byte("before rotation")}
	if err := c.EncryptRecord(old); err != nil {
		t.Fatalf("encrypt old: %v", err)
	}

	if err := p.AddKey("k2", testKey(2)); err != nil {
		t.Fatalf("add key: %v", err)
	}
	if err := p.Rotate("k2"); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	fresh := &kgo.Record{Topic: "secure", Value: []byte("after rotation")}
	if err := c.EncryptRecord(fresh); err != nil {
		t.Fatalf("encrypt fresh: %v", err)
	}

	oldID, _, err := parseEnvelope(old.Value)
	if err != nil {
		t.Fatalf("parse old envelope: %v", err)
	}
	freshID, _, err := parseEnvelope(fresh.Value)
	if err != nil {
		t.Fatalf("parse fresh envelope: %v", err)
	}
	if oldID != "k1" || freshID != "k2" {
		t.Fatalf("envelope key IDs = %q, %q; want k1, k2", oldID, freshID)
	}

	// Records encrypted before the rotation must still decrypt.
	if err := c.DecryptRecord(old); err != nil {
		t.Fatalf("decrypt old: %v", err)
	}
	if err := c.DecryptRecord(fresh); err != nil {
		t.Fatalf("decrypt fresh: %v", err)
	}
	if !bytes.Equal(old.Value, []byte("before rotation")) || !bytes.Equal(fresh.Value, []byte("after rotation")) {
		t.Fatal("rotation round trip mismatch")
	}
}

func TestUnknownKeyID(t *testing.T) {
	writer := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	r := &kgo.Record{Topic: "secure", Value: []byte("payload")}
	if err := writer.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	before := slices.Clone(r.Value)

	// A reader that only knows a different key set.
	reader := NewCipher(testProvider(t, "k2", map[string][]byte{"k2": testKey(2)}))
	err := reader.DecryptRecord(r)
	if !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("decrypt err = %v, want ErrUnknownKeyID", err)
	}
	if !bytes.Equal(r.Value, before) {
		t.Fatal("failed decrypt modified the record")
	}
}

func TestHeaderTampered(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}),
		AuthenticateHeaders("tenant"))
	r := &kgo.Record{
		Topic:   "secure",
		Value:   []byte("payload"),
		Headers: []kgo.RecordHeader{{Key: "tenant", Value: []byte("acme")}},
	}
	if err := c.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	before := slices.Clone(r.Value)

	r.Headers[0].Value = []byte("evil-corp")
	if err := c.DecryptRecord(r); err == nil {
		t.Fatal("decrypt succeeded after authenticated header was modified")
	}
	if !bytes.Equal(r.Value, before) {
		t.Fatal("failed decrypt modified the record")
	}

	// Unauthenticated headers may change without breaking decryption.
	r.Headers[0].Value = []byte("acme")
	r.Headers = append(r.Headers, kgo.RecordHeader{Key: "trace", Value: []byte("abc")})
	if err := c.DecryptRecord(r); err != nil {
		t.Fatalf("decrypt with unauthenticated header added: %v", err)
	}
}

func TestKeyTampered(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	r := &kgo.Record{Topic: "secure", Key: []byte("user-1"), Value: []byte("payload")}
	if err := c.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	r.Key = []byte("user-2")
	if err := c.DecryptRecord(r); err == nil {
		t.Fatal("decrypt succeeded after the record key was modified")
	}
}

func TestCopiedToAnotherTopic(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	r := &kgo.Record{Topic: "secure", Value: []byte("payload")}
	if err := c.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// A downstream consumer copying the raw record to another topic must
	// not be able to replay it there.
	copied := &kgo.Record{Topic: "other-topic", Key: r.Key, Value: r.Value, Headers: r.Headers}
	if err := c.DecryptRecord(copied); err == nil {
		t.Fatal("decrypt succeeded for a record copied to another topic")
	}

	// The original still decrypts.
	if err := c.DecryptRecord(r); err != nil {
		t.Fatalf("decrypt original: %v", err)
	}
}

func TestCorruptedCiphertextAtomic(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	r := &kgo.Record{Topic: "secure", Value: []byte("payload")}
	if err := c.EncryptRecord(r); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	before := slices.Clone(r.Value)

	r.Value[len(r.Value)-1] ^= 0xff
	corrupted := slices.Clone(r.Value)
	if err := c.DecryptRecord(r); err == nil {
		t.Fatal("decrypt succeeded on corrupted ciphertext")
	}
	if !bytes.Equal(r.Value, corrupted) {
		t.Fatal("failed decrypt left a half-modified record")
	}
	r.Value = before
	if err := c.DecryptRecord(r); err != nil {
		t.Fatalf("decrypt of uncorrupted envelope: %v", err)
	}
}

func TestPlaintextPassthrough(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	r := &kgo.Record{Topic: "mixed", Value: []byte("not encrypted")}
	if err := c.DecryptRecord(r); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(r.Value, []byte("not encrypted")) {
		t.Fatalf("plaintext value changed to %q", r.Value)
	}
}

func TestBadKeySize(t *testing.T) {
	if _, err := NewMapProvider("k1", map[string][]byte{"k1": []byte("short")}); err == nil {
		t.Fatal("NewMapProvider accepted a non-32-byte key")
	}
}

func TestHook(t *testing.T) {
	c := NewCipher(testProvider(t, "k1", map[string][]byte{"k1": testKey(1)}))
	var hookErrs []error
	h := c.Hook(func(err error) { hookErrs = append(hookErrs, err) })

	produce, ok := h.(interface{ OnProduceRecordBuffered(*kgo.Record) })
	if !ok {
		t.Fatal("hook does not implement OnProduceRecordBuffered")
	}
	fetch, ok := h.(interface{ OnFetchRecordBuffered(*kgo.Record) })
	if !ok {
		t.Fatal("hook does not implement OnFetchRecordBuffered")
	}

	r := &kgo.Record{Topic: "secure", Value: []byte("payload")}
	produce.OnProduceRecordBuffered(r)
	if bytes.Equal(r.Value, []byte("payload")) {
		t.Fatal("produce hook did not encrypt")
	}
	fetch.OnFetchRecordBuffered(r)
	if !bytes.Equal(r.Value, []byte("payload")) {
		t.Fatalf("fetch hook got %q", r.Value)
	}
	if len(hookErrs) != 0 {
		t.Fatalf("unexpected hook errors: %v", hookErrs)
	}

	// A record that fails to decrypt stays enveloped and reports an error.
	bad := &kgo.Record{Topic: "secure", Value: []byte("payload")}
	produce.OnProduceRecordBuffered(bad)
	bad.Topic = "elsewhere"
	fetch.OnFetchRecordBuffered(bad)
	if len(hookErrs) != 1 {
		t.Fatalf("hook errors = %v, want exactly 1", hookErrs)
	}
	if !hasMagic(bad.Value) {
		t.Fatal("failed decrypt in hook delivered a modified record")
	}
}

// parseEnvelope splits an envelope into its key ID and nonce for tests.
func parseEnvelope(v []byte) (string, []byte, error) {
	if !hasMagic(v) {
		return "", nil, errors.New("missing magic")
	}
	rest := v[len(magic)+1:]
	idLen, n := binary.Uvarint(rest)
	if n <= 0 || uint64(len(rest[n:])) < idLen {
		return "", nil, errors.New("bad key ID")
	}
	id := string(rest[n : n+int(idLen)])
	rest = rest[n+int(idLen):]
	if len(rest) < nonceSize {
		return "", nil, errors.New("bad nonce")
	}
	return id, rest[:nonceSize], nil
}
