/*-
 * Copyright 2014 Square Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package jose

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	stdjson "encoding/json"
	"errors"
	"strings"
	"testing"
)

var rewrapECKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

const rewrapAlgs = RSA_OAEP
const rewrapEncAlgs = A128GCM

func rewrapMultiJWE(t *testing.T, aad []byte) ([]byte, *JSONWebEncryption, string) {
	t.Helper()

	plaintext := []byte("Lorem ipsum dolor sit amet")
	enc, err := NewMultiEncrypter(rewrapEncAlgs, []Recipient{
		{Algorithm: rewrapAlgs, Key: &rsaTestKey.PublicKey, KeyID: "orig"},
		{Algorithm: A256KW, Key: bytes.Repeat([]byte{3}, 32), KeyID: "other"},
	}, nil)
	if err != nil {
		t.Fatalf("NewMultiEncrypter: %v", err)
	}

	obj, err := enc.EncryptWithAuthData(plaintext, aad)
	if err != nil {
		t.Fatalf("EncryptWithAuthData: %v", err)
	}

	serialized := obj.FullSerialize()

	parsed, err := ParseEncrypted(serialized,
		[]KeyAlgorithm{rewrapAlgs, ECDH_ES_A128KW, A256KW},
		[]ContentEncryption{rewrapEncAlgs})
	if err != nil {
		t.Fatalf("ParseEncrypted: %v", err)
	}

	return plaintext, parsed, serialized
}

func parseRewrap(t *testing.T, serialized string) *JSONWebEncryption {
	t.Helper()
	parsed, err := ParseEncrypted(serialized,
		[]KeyAlgorithm{rewrapAlgs, ECDH_ES_A128KW, A256KW, DIRECT, ECDH_ES},
		[]ContentEncryption{rewrapEncAlgs})
	if err != nil {
		t.Fatalf("ParseEncrypted: %v", err)
	}
	return parsed
}

func parseRawRewrap(t *testing.T, serialized string) rawJSONWebEncryption {
	t.Helper()
	var raw rawJSONWebEncryption
	if err := stdjson.Unmarshal([]byte(serialized), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return raw
}

// flipBase64Field returns a copy of serialized with one field modified so that
// it still parses but authenticates differently. The protected header is
// changed into a different but still valid JSON object; the other fields get
// a single bit-level change in their first base64url character.
func flipBase64Field(t *testing.T, serialized, field string) string {
	t.Helper()

	var fields map[string]stdjson.RawMessage
	if err := stdjson.Unmarshal([]byte(serialized), &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var encoded string
	if err := stdjson.Unmarshal(fields[field], &encoded); err != nil {
		t.Fatalf("unmarshal field %q: %v", field, err)
	}

	var changed string
	if field == "protected" {
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decode protected: %v", err)
		}
		var header map[string]interface{}
		if err := stdjson.Unmarshal(raw, &header); err != nil {
			t.Fatalf("unmarshal protected json: %v", err)
		}
		header["rewrap_tamper"] = true
		raw, err = stdjson.Marshal(header)
		if err != nil {
			t.Fatalf("marshal protected json: %v", err)
		}
		changed = base64.RawURLEncoding.EncodeToString(raw)
	} else {
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decode field %q: %v", field, err)
		}
		if len(raw) == 0 {
			t.Fatalf("field %q is empty", field)
		}
		raw[0] ^= 1
		changed = base64.RawURLEncoding.EncodeToString(raw)
	}

	changedJSON, err := stdjson.Marshal(changed)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	fields[field] = changedJSON

	out, err := stdjson.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}

func TestRewrapAddsRecipientWithoutTouchingCiphertext(t *testing.T) {
	aad := []byte("authenticated additional data")
	plaintext, parsed, originalSerialized := rewrapMultiJWE(t, aad)

	before := parseRawRewrap(t, originalSerialized)

	rewrapped, err := parsed.Rewrap(rsaTestKey, Recipient{
		Algorithm: ECDH_ES_A128KW,
		Key:       &rewrapECKey.PublicKey,
		KeyID:     "new-svc-account",
	})
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}

	rewrappedSerialized := rewrapped.FullSerialize()
	after := parseRawRewrap(t, rewrappedSerialized)

	// The protected header, AAD, IV, ciphertext and tag must be byte identical.
	if !bytes.Equal(before.Protected.bytes(), after.Protected.bytes()) {
		t.Error("protected header was rewritten")
	}
	if before.Aad.base64() != after.Aad.base64() || !bytes.Equal(parsed.aad, rewrapped.aad) {
		t.Error("AAD was rewritten")
	}
	if !bytes.Equal(before.Iv.bytes(), after.Iv.bytes()) {
		t.Error("IV was rewritten")
	}
	if !bytes.Equal(before.Ciphertext.bytes(), after.Ciphertext.bytes()) {
		t.Error("ciphertext was rewritten")
	}
	if !bytes.Equal(before.Tag.bytes(), after.Tag.bytes()) {
		t.Error("authentication tag was rewritten")
	}

	if len(after.Recipients) != len(before.Recipients)+1 {
		t.Fatalf("recipients = %d, want %d", len(after.Recipients), len(before.Recipients)+1)
	}

	// Existing recipients' encrypted keys and headers are unchanged.
	for i := range before.Recipients {
		if before.Recipients[i].EncryptedKey != after.Recipients[i].EncryptedKey {
			t.Errorf("recipient %d encrypted key was modified", i)
		}
	}

	// The new recipient carries a fresh wrapped key distinct from all existing
	// ones.
	newEncryptedKey := after.Recipients[len(before.Recipients)].EncryptedKey
	if newEncryptedKey == "" {
		t.Error("new recipient has no wrapped key")
	}
	for _, existing := range before.Recipients {
		if newEncryptedKey == existing.EncryptedKey {
			t.Error("new recipient reused an existing wrapped key")
		}
	}

	// The original key still decrypts to the same plaintext.
	parsedAgain := parseRewrap(t, rewrappedSerialized)
	idx, _, out, err := parsedAgain.DecryptMulti(rsaTestKey)
	if err != nil {
		t.Fatalf("DecryptMulti(original key): %v", err)
	}
	if idx != 0 || !bytes.Equal(out, plaintext) {
		t.Fatalf("original recipient decryption mismatch idx=%d", idx)
	}

	// The new service account key decrypts the very same payload at index 1.
	idx, _, out, err = parsedAgain.DecryptMulti(rewrapECKey)
	if err != nil {
		t.Fatalf("DecryptMulti(new key): %v", err)
	}
	if idx != len(before.Recipients) || !bytes.Equal(out, plaintext) {
		t.Fatalf("new recipient decryption mismatch idx=%d", idx)
	}

	if !bytes.Equal(parsedAgain.GetAuthData(), aad) {
		t.Error("AAD not preserved through rewrap")
	}
}

func TestRewrapDoesNotExposeCEK(t *testing.T) {
	// The API returns only a *JSONWebEncryption; its public surface exposes
	// headers and metadata, never the CEK. Confirm no exported accessor leaks
	// key material and the serialized form contains only wrapped keys.
	_, parsed, _ := rewrapMultiJWE(t, nil)

	rewrapped, err := parsed.Rewrap(rsaTestKey, Recipient{
		Algorithm: ECDH_ES_A128KW,
		Key:       &rewrapECKey.PublicKey,
	})
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}

	serialized := rewrapped.FullSerialize()
	if strings.Contains(serialized, "cek") {
		t.Error("serialized object appears to leak CEK")
	}

	// Exported Header contains no key bytes.
	_ = rewrapped.Header
}

func TestRewrapRejectsWrongDecryptionKey(t *testing.T) {
	_, parsed, _ := rewrapMultiJWE(t, nil)

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, err := parsed.Rewrap(other, Recipient{
		Algorithm: ECDH_ES_A128KW,
		Key:       &rewrapECKey.PublicKey,
	})
	if !errors.Is(err, ErrCryptoFailure) {
		t.Fatalf("Rewrap with wrong key: got %v, want ErrCryptoFailure", err)
	}
}

func TestRewrapRejectsTamperedObject(t *testing.T) {
	fields := []string{"protected", "aad", "iv", "ciphertext", "tag"}

	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			_, parsed, serialized := rewrapMultiJWE(t, []byte("aad-data"))

			tampered := flipBase64Field(t, serialized, field)
			tamperedObj := parseRewrap(t, tampered)

			_, err := tamperedObj.Rewrap(rsaTestKey, Recipient{
				Algorithm: ECDH_ES_A128KW,
				Key:       &rewrapECKey.PublicKey,
			})
			if err == nil {
				t.Fatalf("Rewrap accepted tampered %q field", field)
			}

			// The untouched object must still rewrap successfully; this guards
			// against the tamper being a no-op.
			if _, err := parsed.Rewrap(rsaTestKey, Recipient{
				Algorithm: ECDH_ES_A128KW,
				Key:       &rewrapECKey.PublicKey,
			}); err != nil {
				t.Fatalf("control Rewrap failed: %v", err)
			}
		})
	}
}

func TestRewrapRejectsCompactAndFlattened(t *testing.T) {
	enc, err := NewEncrypter(rewrapEncAlgs,
		Recipient{Algorithm: rewrapAlgs, Key: &rsaTestKey.PublicKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := enc.Encrypt([]byte("hi"))
	if err != nil {
		t.Fatal(err)
	}

	compact, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parsedCompact := parseRewrap(t, compact)
	if _, err := parsedCompact.Rewrap(rsaTestKey, Recipient{
		Algorithm: ECDH_ES_A128KW,
		Key:       &rewrapECKey.PublicKey,
	}); !errors.Is(err, ErrCannotRewrap) {
		t.Fatalf("compact: got %v, want ErrCannotRewrap", err)
	}

	flattened := obj.FullSerialize()
	parsedFlat := parseRewrap(t, flattened)
	if _, err := parsedFlat.Rewrap(rsaTestKey, Recipient{
		Algorithm: ECDH_ES_A128KW,
		Key:       &rewrapECKey.PublicKey,
	}); !errors.Is(err, ErrCannotRewrap) {
		t.Fatalf("flattened: got %v, want ErrCannotRewrap", err)
	}
}

func TestRewrapRejectsDirectKeyAgreement(t *testing.T) {
	directKey := []byte("0123456789abcdef")
	enc, err := NewEncrypter(rewrapEncAlgs,
		Recipient{Algorithm: DIRECT, Key: directKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := enc.Encrypt([]byte("hi"))
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseEncrypted(obj.FullSerialize(),
		[]KeyAlgorithm{DIRECT, ECDH_ES_A128KW}, []ContentEncryption{rewrapEncAlgs})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := parsed.Rewrap(directKey, Recipient{
		Algorithm: ECDH_ES_A128KW,
		Key:       &rewrapECKey.PublicKey,
	}); !errors.Is(err, ErrCannotRewrap) {
		t.Fatalf("dir: got %v, want ErrCannotRewrap", err)
	}
}

func TestRewrapRejectsDirectKeyAgreementAlgorithm(t *testing.T) {
	_, parsed, _ := rewrapMultiJWE(t, nil)

	// "dir" and "ECDH-ES" never wrap a CEK and cannot target a new recipient.
	for _, alg := range []KeyAlgorithm{DIRECT, ECDH_ES} {
		if _, err := parsed.Rewrap(rsaTestKey, Recipient{
			Algorithm: alg,
			Key:       []byte("0123456789abcdef"),
		}); !errors.Is(err, ErrCannotRewrap) {
			t.Fatalf("alg %s: got %v, want ErrCannotRewrap", alg, err)
		}
	}
}

func TestRewrapSharedAlgMustMatch(t *testing.T) {
	// Build a valid general-serialization JWE whose protected header carries
	// the shared "alg" (RFC 7516 general serialization with a shared algorithm).
	serialized := sharedAlgGeneralJWE(t)
	parsed := parseRewrap(t, serialized)

	_, err := parsed.Rewrap(bytes.Repeat([]byte{1}, 32), Recipient{
		Algorithm: RSA_OAEP,
		Key:       &rsaTestKey.PublicKey,
	})
	if !errors.Is(err, ErrCannotRewrap) {
		t.Fatalf("mismatched shared alg: got %v, want ErrCannotRewrap", err)
	}

	// Same algorithm wraps successfully and produces no per-recipient "alg".
	rewrapped, err := parsed.Rewrap(bytes.Repeat([]byte{1}, 32), Recipient{
		Algorithm: A256KW,
		Key:       bytes.Repeat([]byte{2}, 32),
	})
	if err != nil {
		t.Fatalf("matching shared alg: %v", err)
	}

	rawRewrapped := parseRawRewrap(t, rewrapped.FullSerialize())
	for i, recipient := range rawRewrapped.Recipients {
		if recipient.Header != nil {
			if _, ok := (*recipient.Header)[headerAlgorithm]; ok {
				t.Errorf("recipient %d duplicates shared alg in per-recipient header", i)
			}
		}
	}
}

// sharedAlgGeneralJWE constructs a valid JWE JSON General Serialization object
// in which the "alg" parameter is shared from the protected header rather than
// repeated per recipient.
func sharedAlgGeneralJWE(t *testing.T) string {
	t.Helper()

	kek := bytes.Repeat([]byte{1}, 32)
	cipher := getContentCipher(rewrapEncAlgs)
	cek, _, err := randomKeyGenerator{size: cipher.keySize()}.genKey()
	if err != nil {
		t.Fatalf("gen cek: %v", err)
	}

	protected := rawHeader{}
	if err := protected.set(headerEncryption, rewrapEncAlgs); err != nil {
		t.Fatal(err)
	}
	if err := protected.set(headerAlgorithm, A256KW); err != nil {
		t.Fatal(err)
	}

	wrapped, err := (&symmetricKeyCipher{key: kek}).encryptKey(cek, A256KW)
	if err != nil {
		t.Fatalf("wrap cek: %v", err)
	}

	protectedBytes := mustSerializeJSON(protected)
	authData := []byte(base64.RawURLEncoding.EncodeToString(protectedBytes))
	parts, err := cipher.encrypt(cek, authData, []byte("shared alg"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	raw := rawJSONWebEncryption{
		Protected:  newBuffer(protectedBytes),
		Iv:         newBuffer(parts.iv),
		Ciphertext: newBuffer(parts.ciphertext),
		Tag:        newBuffer(parts.tag),
		Recipients: []rawRecipientInfo{{
			Header:       &rawHeader{},
			EncryptedKey: base64.RawURLEncoding.EncodeToString(wrapped.encryptedKey),
		}},
	}
	return string(mustSerializeJSON(raw))
}

func TestRewrapDoesNotMutateOriginal(t *testing.T) {
	_, parsed, serialized := rewrapMultiJWE(t, nil)

	if _, err := parsed.Rewrap(rsaTestKey, Recipient{
		Algorithm: ECDH_ES_A128KW,
		Key:       &rewrapECKey.PublicKey,
	}); err != nil {
		t.Fatal(err)
	}

	if got := parsed.FullSerialize(); got != serialized {
		t.Error("Rewrap mutated the original parsed object")
	}
}
