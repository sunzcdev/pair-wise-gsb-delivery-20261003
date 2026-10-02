package jose

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// parseRewrapJSON decodes a full-serialized JWE into its raw JSON shape so the
// individual fields can be compared directly.
func parseRewrapJSON(t *testing.T, msg string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(msg), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return out
}

// tamperSerializedField flips one byte in the given base64url-encoded field of
// a full-serialized JWE and re-parses it.
func tamperSerializedField(t *testing.T, msg, field string, xor byte) *JSONWebEncryption {
	t.Helper()

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(msg), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var encoded string
	if err := json.Unmarshal(raw[field], &encoded); err != nil {
		t.Fatalf("read %s: %v", field, err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}
	decoded[0] ^= xor

	tampered, err := json.Marshal(base64.RawURLEncoding.EncodeToString(decoded))
	if err != nil {
		t.Fatal(err)
	}
	raw[field] = tampered

	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseEncryptedJSON(string(out),
		[]KeyAlgorithm{RSA_OAEP, A256GCMKW},
		[]ContentEncryption{A256GCM})
	if err != nil {
		t.Fatalf("parse tampered: %v", err)
	}
	return parsed
}

func multiRecipientRewrapJWE(t *testing.T) (*JSONWebEncryption, []byte, []byte) {
	t.Helper()

	sharedKey := bytes.Repeat([]byte{1, 2, 3, 4}, 8) // 32 bytes for A256GCMKW

	enc, err := NewMultiEncrypter(A256GCM, []Recipient{
		{Algorithm: RSA_OAEP, Key: &rsaTestKey.PublicKey},
		{Algorithm: A256GCMKW, Key: sharedKey},
	}, nil)
	if err != nil {
		t.Fatalf("NewMultiEncrypter: %v", err)
	}

	plaintext := []byte("service-account archive payload")
	aad := []byte("additional authenticated data")
	obj, err := enc.EncryptWithAuthData(plaintext, aad)
	if err != nil {
		t.Fatalf("EncryptWithAuthData: %v", err)
	}

	parsed, err := ParseEncrypted(obj.FullSerialize(),
		[]KeyAlgorithm{RSA_OAEP, A256GCMKW},
		[]ContentEncryption{A256GCM})
	if err != nil {
		t.Fatalf("ParseEncrypted: %v", err)
	}

	return parsed, sharedKey, plaintext
}

func TestAddRecipientPreservesCipherFields(t *testing.T) {
	obj, sharedKey, plaintext := multiRecipientRewrapJWE(t)

	original := parseRewrapJSON(t, obj.FullSerialize())

	// New service account uses a different EC key with ECDH-ES+A256KW.
	rewrapped, err := obj.AddRecipient(rsaTestKey, Recipient{
		Algorithm: ECDH_ES_A256KW,
		Key:       &ecTestKey256.PublicKey,
		KeyID:     "new-service-account",
	})
	if err != nil {
		t.Fatalf("AddRecipient: %v", err)
	}

	got := parseRewrapJSON(t, rewrapped.FullSerialize())

	// The authenticated envelope must be byte-for-byte identical.
	for _, field := range []string{"protected", "aad", "iv", "ciphertext", "tag"} {
		if got[field] != original[field] {
			t.Errorf("field %q changed during re-wrap:\n before=%v\n after =%v", field, original[field], got[field])
		}
	}

	originalRecipients, _ := original["recipients"].([]any)
	newRecipients, _ := got["recipients"].([]any)
	if len(newRecipients) != len(originalRecipients)+1 {
		t.Fatalf("recipients = %d, want %d", len(newRecipients), len(originalRecipients)+1)
	}

	// Existing encrypted keys are carried over untouched.
	for i, r := range originalRecipients {
		orig := r.(map[string]any)
		updated := newRecipients[i].(map[string]any)
		if orig["encrypted_key"] != updated["encrypted_key"] {
			t.Errorf("existing encrypted_key for recipient %d changed", i)
		}
	}

	// Existing recipients still decrypt.
	i, _, out, err := rewrapped.DecryptMulti(rsaTestKey)
	if err != nil {
		t.Fatalf("decrypt original RSA recipient: %v", err)
	}
	if i != 0 || !bytes.Equal(out, plaintext) {
		t.Fatalf("original RSA recipient mismatch (index %d)", i)
	}
	i, _, out, err = rewrapped.DecryptMulti(sharedKey)
	if err != nil {
		t.Fatalf("decrypt original shared-key recipient: %v", err)
	}
	if i != 1 || !bytes.Equal(out, plaintext) {
		t.Fatalf("original shared-key recipient mismatch (index %d)", i)
	}

	// The new recipient decrypts the same payload.
	i, headers, out, err := rewrapped.DecryptMulti(ecTestKey256)
	if err != nil {
		t.Fatalf("decrypt new EC recipient: %v", err)
	}
	if i != 2 || !bytes.Equal(out, plaintext) {
		t.Fatalf("new EC recipient mismatch (index %d)", i)
	}
	if headers.KeyID != "new-service-account" {
		t.Fatalf("new recipient kid = %q, want %q", headers.KeyID, "new-service-account")
	}

	// The serialized object carries AAD and the content key is never exposed.
	if got["aad"] == nil || got["aad"] == "" {
		t.Error("aad missing from re-wrapped object")
	}
}

func TestAddRecipientDoesNotExposeCEK(t *testing.T) {
	obj, _, _ := multiRecipientRewrapJWE(t)

	rewrapped, err := obj.AddRecipient(rsaTestKey, Recipient{
		Algorithm: RSA_OAEP_256,
		Key:       &rsaTestKey.PublicKey,
	})
	if err != nil {
		t.Fatalf("AddRecipient: %v", err)
	}

	msg := rewrapped.FullSerialize()
	raw := parseRewrapJSON(t, msg)
	recipients, _ := raw["recipients"].([]any)
	if len(recipients) != 3 {
		t.Fatalf("recipients = %d, want 3", len(recipients))
	}
	// Only encrypted keys must appear; there must be no plaintext key field.
	for _, key := range []string{"cek", "content_key", "key"} {
		if _, present := raw[key]; present {
			t.Errorf("content key leaked through field %q", key)
		}
	}
	if _, ok := recipients[2].(map[string]any)["encrypted_key"]; !ok {
		t.Error("new recipient missing encrypted_key")
	}
}

func TestAddRecipientCanChain(t *testing.T) {
	obj, sharedKey, plaintext := multiRecipientRewrapJWE(t)

	once, err := obj.AddRecipient(rsaTestKey, Recipient{
		Algorithm: ECDH_ES_A256KW, Key: &ecTestKey256.PublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	twice, err := once.AddRecipient(rsaTestKey, Recipient{
		Algorithm: A256KW, Key: sharedKey,
	})
	if err != nil {
		t.Fatalf("chained AddRecipient: %v", err)
	}

	original := parseRewrapJSON(t, obj.FullSerialize())
	got := parseRewrapJSON(t, twice.FullSerialize())
	for _, field := range []string{"protected", "aad", "iv", "ciphertext", "tag"} {
		if got[field] != original[field] {
			t.Errorf("field %q changed across chained re-wrap", field)
		}
	}
	if recipients, _ := got["recipients"].([]any); len(recipients) != 4 {
		t.Fatalf("recipients = %d, want 4", len(recipients))
	}
	if _, _, out, err := twice.DecryptMulti(ecTestKey256); err != nil || !bytes.Equal(out, plaintext) {
		t.Fatalf("chained EC recipient decrypt: %v", err)
	}
}

func TestAddRecipientRejectsTamperedProtectedHeader(t *testing.T) {
	obj, _, _ := multiRecipientRewrapJWE(t)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(obj.FullSerialize()), &raw); err != nil {
		t.Fatal(err)
	}
	var encoded string
	if err := json.Unmarshal(raw["protected"], &encoded); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the protected header with different, but still valid, JSON.
	var header map[string]any
	if err := json.Unmarshal(decoded, &header); err != nil {
		t.Fatal(err)
	}
	header["x"] = 1
	forged, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	forgedEncoded, err := json.Marshal(base64.RawURLEncoding.EncodeToString(forged))
	if err != nil {
		t.Fatal(err)
	}
	raw["protected"] = forgedEncoded
	msg, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}

	tampered, err := ParseEncryptedJSON(string(msg),
		[]KeyAlgorithm{RSA_OAEP, A256GCMKW}, []ContentEncryption{A256GCM})
	if err != nil {
		t.Fatalf("ParseEncryptedJSON: %v", err)
	}
	if _, err := tampered.AddRecipient(rsaTestKey, Recipient{
		Algorithm: RSA_OAEP,
		Key:       &rsaTestKey.PublicKey,
	}); err == nil {
		t.Fatal("AddRecipient accepted an object with a tampered protected header")
	}
}

func TestAddRecipientRejectsTamperedObject(t *testing.T) {
	obj, _, _ := multiRecipientRewrapJWE(t)

	tampered := tamperSerializedField(t, obj.FullSerialize(), "ciphertext", 0xFF)

	_, err := tampered.AddRecipient(rsaTestKey, Recipient{
		Algorithm: RSA_OAEP,
		Key:       &rsaTestKey.PublicKey,
	})
	if err == nil {
		t.Fatal("AddRecipient accepted a tampered object")
	}
}

func TestAddRecipientRejectsTamperedTag(t *testing.T) {
	obj, _, _ := multiRecipientRewrapJWE(t)

	tampered := tamperSerializedField(t, obj.FullSerialize(), "tag", 0x01)

	_, err := tampered.AddRecipient(rsaTestKey, Recipient{
		Algorithm: RSA_OAEP,
		Key:       &rsaTestKey.PublicKey,
	})
	if err == nil {
		t.Fatal("AddRecipient accepted an object with a tampered tag")
	}
}

func TestAddRecipientRejectsWrongKey(t *testing.T) {
	obj, _, _ := multiRecipientRewrapJWE(t)

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, err = obj.AddRecipient(other, Recipient{
		Algorithm: RSA_OAEP,
		Key:       &other.PublicKey,
	})
	if err == nil {
		t.Fatal("AddRecipient accepted an authorization key that cannot decrypt")
	}
}

func TestAddRecipientRejectsUnsupportedLayouts(t *testing.T) {
	sharedKey := bytes.Repeat([]byte{1}, 16)

	// Compact serialization (single recipient) cannot gain a recipient.
	enc, err := NewEncrypter(A128GCM, Recipient{Algorithm: A128GCMKW, Key: sharedKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	single, err := enc.Encrypt([]byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	compact, err := single.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parsedCompact, err := ParseEncryptedCompact(compact,
		[]KeyAlgorithm{A128GCMKW}, []ContentEncryption{A128GCM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsedCompact.AddRecipient(sharedKey, Recipient{
		Algorithm: RSA_OAEP, Key: &rsaTestKey.PublicKey,
	}); err == nil {
		t.Error("AddRecipient accepted a compact-serialized JWE")
	}

	// Flattened (single-recipient JSON) serialization cannot gain a recipient.
	parsedFlat, err := ParseEncryptedJSON(single.FullSerialize(),
		[]KeyAlgorithm{A128GCMKW}, []ContentEncryption{A128GCM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsedFlat.AddRecipient(sharedKey, Recipient{
		Algorithm: RSA_OAEP, Key: &rsaTestKey.PublicKey,
	}); err == nil {
		t.Error("AddRecipient accepted a flattened-serialized JWE")
	}

	// ECDH-ES (direct key agreement) cannot share a content encryption key.
	multi, _, _ := multiRecipientRewrapJWE(t)
	multi.recipients[0].header.set(headerAlgorithm, ECDH_ES)
	multi.original.Recipients[0].Header.set(headerAlgorithm, ECDH_ES)
	if _, err := multi.AddRecipient(sharedKey, Recipient{
		Algorithm: RSA_OAEP, Key: &rsaTestKey.PublicKey,
	}); err == nil {
		t.Error("AddRecipient accepted an ECDH-ES recipient layout")
	}
}

func TestAddRecipientRejectsDirectNewRecipient(t *testing.T) {
	obj, _, _ := multiRecipientRewrapJWE(t)

	_, err := obj.AddRecipient(rsaTestKey, Recipient{
		Algorithm: DIRECT,
		Key:       bytes.Repeat([]byte{0}, 32),
	})
	if err == nil {
		t.Fatal("AddRecipient accepted a direct-mode new recipient")
	}
}

func TestRemoveRecipient(t *testing.T) {
	obj, sharedKey, plaintext := multiRecipientRewrapJWE(t)

	original := parseRewrapJSON(t, obj.FullSerialize())

	// Remove the RSA recipient (index 0), authorizing with the shared key.
	reduced, err := obj.RemoveRecipient(sharedKey, 0)
	if err != nil {
		t.Fatalf("RemoveRecipient: %v", err)
	}

	got := parseRewrapJSON(t, reduced.FullSerialize())
	for _, field := range []string{"protected", "aad", "iv", "ciphertext", "tag"} {
		if got[field] != original[field] {
			t.Errorf("field %q changed during RemoveRecipient", field)
		}
	}

	// With a single remaining recipient the library uses the flattened JSON
	// serialization, so the "recipients" array is gone and the encrypted key
	// moves to the top level.
	if newRecipients, ok := got["recipients"].([]any); ok && len(newRecipients) != 0 {
		t.Fatalf("expected flattened serialization, got %d recipients", len(newRecipients))
	}
	if _, ok := got["encrypted_key"]; !ok {
		t.Fatal("flattened object missing top-level encrypted_key")
	}

	// Remaining recipient still decrypts.
	i, _, out, err := reduced.DecryptMulti(sharedKey)
	if err != nil {
		t.Fatalf("decrypt remaining recipient: %v", err)
	}
	if i != 0 || !bytes.Equal(out, plaintext) {
		t.Fatalf("remaining recipient mismatch (index %d)", i)
	}

	// The removed key can no longer open the reduced object.
	if _, _, _, err := reduced.DecryptMulti(rsaTestKey); err == nil {
		t.Error("removed RSA recipient could still decrypt reduced object")
	}

	// But removing a recipient does not revoke the original copy: the RSA key
	// still decrypts the object captured before removal.
	i, _, out, err = obj.DecryptMulti(rsaTestKey)
	if err != nil || !bytes.Equal(out, plaintext) {
		t.Fatalf("removed recipient should still open the pre-removal copy (err=%v)", err)
	}
}

func TestRemoveRecipientValidation(t *testing.T) {
	obj, sharedKey, _ := multiRecipientRewrapJWE(t)

	for _, index := range []int{-1, 2, 99} {
		if _, err := obj.RemoveRecipient(sharedKey, index); err == nil {
			t.Errorf("RemoveRecipient(%d) should fail", index)
		}
	}

	// Authorization is still required.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obj.RemoveRecipient(other, 0); err == nil {
		t.Error("RemoveRecipient accepted an unauthorized key")
	}

	// Cannot reduce a single-recipient object below one recipient.
	reduced, err := obj.RemoveRecipient(sharedKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reduced.RemoveRecipient(sharedKey, 0); err == nil {
		t.Error("RemoveRecipient removed the last recipient")
	}
}
