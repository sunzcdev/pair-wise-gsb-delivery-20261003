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
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4/json"
)

// Static keys from JWA (RFC 7518), Appendix C, which are reused by the
// example in draft-madden-jose-ecdh-1pu-04, Appendix A. Alice is the sender,
// Bob is the recipient.
var ecdh1puAliceKey = &ecdsa.PrivateKey{
	PublicKey: ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     fromBase64Int("gI0GAILBdu7T53akrFmMyGcsF3n5dO7MmwNBHKW5SV0"),
		Y:     fromBase64Int("SLW_xSffzlPWrHEVI30DHM_4egVwt3NQqeUD7nMFpps"),
	},
	D: fromBase64Int("0_NxaRPUMQoAJt50Gz8YiTr8gRTwyEaCumd-MToTmIo"),
}

var ecdh1puBobKey = &ecdsa.PrivateKey{
	PublicKey: ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     fromBase64Int("weNJy2HscCSM6AEDTDg04biOvhFhyyWvOHQfeF_PxMQ"),
		Y:     fromBase64Int("e8lnCO-AlStT-NJVX-crhB7QRYhiix03illJOVAOyck"),
	},
	D: fromBase64Int("VEmDZpDXXK8p8N0Cndsxs924q6nS1RXFASRl6BfUqdw"),
}

// ecdh1puMalloryKey is a third party's key, used for sender substitution
// tests. It is the fixed ephemeral key from the cipher-level test vector.
var ecdh1puMalloryKey = &ecdsa.PrivateKey{
	PublicKey: ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     fromBase64Int("LymTj9M8ZP_Z2Z7A5ysTIxJh8MCVnRp-JeY_92cYs6o"),
		Y:     fromBase64Int("zrJnRxGRm-U-9bgwzqVW4QGszbMLt-PLoDiaE6gjTuc"),
	},
	D: fromBase64Int("znZubvibMtR6aG6Bmwh9nv-22VYrMhedKOPe9_eK4Es"),
}

// setECDH1PU switches the experimental ECDH-1PU feature gate for the duration
// of a test.
func setECDH1PU(t *testing.T, enabled bool) {
	t.Helper()
	previous := ecdh1puEnabled.Load()
	ecdh1puEnabled.Store(enabled)
	t.Cleanup(func() { ecdh1puEnabled.Store(previous) })
}

// ecdh1puTestVectorJWE is a complete JWE computed with an independent
// implementation of draft-madden-jose-ecdh-1pu-04 (ECDH-1PU+A128KW with
// A128GCM, apu="Alice", apv="Bob", skid="alice"), using the RFC 7518
// Appendix C static keys and a fixed ephemeral key. The plaintext is
// "Lorem ipsum dolor sit amet".
const ecdh1puTestVectorJWE = "eyJhbGciOiJFQ0RILTFQVStBMTI4S1ciLCJlbmMiOiJBMTI4R0NNIiwiYXB1IjoiUVd4cFkyVSIsImFwdiI6IlFtOWkiLCJza2lkIjoiYWxpY2UiLCJlcGsiOnsia3R5IjoiRUMiLCJjcnYiOiJQLTI1NiIsIngiOiJMeW1UajlNOFpQX1oyWjdBNXlzVEl4Smg4TUNWblJwLUplWV85MmNZczZvIiwieSI6InpySm5SeEdSbS1VLTliZ3d6cVZXNFFHc3piTUx0LVBMb0RpYUU2Z2pUdWMifX0.UQsDomV2MGOLQH_H-s7VV4cqsBrVJHx6.a9iFohV4_0MPMLN2.IBR-NIJ4NXSWzGIOQLQujqanHxl7nniR458.Pt2QjKFTskJ3ij99L1wkcg"

func ecdh1puMustEncrypt(t *testing.T, alg KeyAlgorithm, enc ContentEncryption, rcpt Recipient) string {
	t.Helper()
	encrypter, err := NewEncrypter(enc, rcpt, nil)
	if err != nil {
		t.Fatalf("failed to create encrypter: %v", err)
	}
	obj, err := encrypter.Encrypt([]byte("Lorem ipsum dolor sit amet"))
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}
	serialized, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("failed to serialize: %v", err)
	}
	return serialized
}

func ecdh1puMustParse(t *testing.T, serialized string) *JSONWebEncryption {
	t.Helper()
	obj, err := ParseEncrypted(serialized,
		[]KeyAlgorithm{ECDH1PU_A128KW, ECDH1PU_A192KW, ECDH1PU_A256KW},
		[]ContentEncryption{A128GCM, A256GCM, A128CBC_HS256})
	if err != nil {
		t.Fatalf("failed to parse JWE: %v", err)
	}
	return obj
}

func TestECDH1PURoundTrip(t *testing.T) {
	setECDH1PU(t, true)

	algs := []KeyAlgorithm{ECDH1PU_A128KW, ECDH1PU_A192KW, ECDH1PU_A256KW}
	plaintext := []byte("Lorem ipsum dolor sit amet")

	for _, alg := range algs {
		// Sender key as raw private key, no apu/apv.
		serialized := ecdh1puMustEncrypt(t, alg, A128GCM, Recipient{
			Algorithm: alg,
			Key:       &ecdh1puBobKey.PublicKey,
			SenderKey: ecdh1puAliceKey,
		})
		obj := ecdh1puMustParse(t, serialized)
		out, err := obj.Decrypt(ECDH1PUDecryptionKey{
			RecipientKey: ecdh1puBobKey,
			SenderKey:    &ecdh1puAliceKey.PublicKey,
		})
		if err != nil {
			t.Errorf("failed to decrypt %s: %v", alg, err)
		}
		if !bytes.Equal(plaintext, out) {
			t.Errorf("decrypted plaintext mismatch for %s", alg)
		}

		// Sender key as JWK (emits "skid"), with apu/apv set.
		serialized = ecdh1puMustEncrypt(t, alg, A128GCM, Recipient{
			Algorithm: alg,
			Key:       JSONWebKey{Key: &ecdh1puBobKey.PublicKey, KeyID: "bob"},
			SenderKey: JSONWebKey{Key: ecdh1puAliceKey, KeyID: "alice"},
			APU:       []byte("Alice"),
			APV:       []byte("Bob"),
		})
		obj = ecdh1puMustParse(t, serialized)
		if skid, ok := obj.Header.ExtraHeaders[HeaderKey("skid")]; !ok || skid != "alice" {
			t.Errorf("expected skid header %q, got %v", "alice", obj.Header.ExtraHeaders)
		}
		out, err = obj.Decrypt(ECDH1PUDecryptionKey{
			RecipientKey: ecdh1puBobKey,
			SenderKey:    JSONWebKey{Key: &ecdh1puAliceKey.PublicKey, KeyID: "alice"},
		})
		if err != nil {
			t.Errorf("failed to decrypt %s with apu/apv: %v", alg, err)
		}
		if !bytes.Equal(plaintext, out) {
			t.Errorf("decrypted plaintext mismatch for %s with apu/apv", alg)
		}
	}
}

func TestECDH1PUKnownAnswer(t *testing.T) {
	setECDH1PU(t, true)

	obj := ecdh1puMustParse(t, ecdh1puTestVectorJWE)
	expected := []byte("Lorem ipsum dolor sit amet")

	// Sender key as JWK with matching key ID ("skid" is consistent).
	out, err := obj.Decrypt(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puBobKey,
		SenderKey:    JSONWebKey{Key: &ecdh1puAliceKey.PublicKey, KeyID: "alice"},
	})
	if err != nil {
		t.Fatalf("failed to decrypt known-answer vector: %v", err)
	}
	if !bytes.Equal(expected, out) {
		t.Errorf("known-answer plaintext mismatch: got %q", out)
	}

	// Sender key as raw public key (no key ID to check "skid" against).
	out, err = obj.Decrypt(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puBobKey,
		SenderKey:    &ecdh1puAliceKey.PublicKey,
	})
	if err != nil {
		t.Fatalf("failed to decrypt known-answer vector with raw sender key: %v", err)
	}
	if !bytes.Equal(expected, out) {
		t.Errorf("known-answer plaintext mismatch with raw sender key: got %q", out)
	}
}

func TestECDH1PURequiresEnable(t *testing.T) {
	setECDH1PU(t, false)

	// Encryption must be rejected.
	_, err := NewEncrypter(A128GCM, Recipient{
		Algorithm: ECDH1PU_A128KW,
		Key:       &ecdh1puBobKey.PublicKey,
		SenderKey: ecdh1puAliceKey,
	}, nil)
	if !errors.Is(err, ErrECDH1PUNotEnabled) {
		t.Errorf("expected ErrECDH1PUNotEnabled from NewEncrypter, got %v", err)
	}

	// Parsing must be rejected even when the algorithm is explicitly allowed.
	_, err = ParseEncrypted(ecdh1puTestVectorJWE,
		[]KeyAlgorithm{ECDH1PU_A128KW},
		[]ContentEncryption{A128GCM})
	if !errors.Is(err, ErrECDH1PUNotEnabled) {
		t.Errorf("expected ErrECDH1PUNotEnabled from ParseEncrypted, got %v", err)
	}

	// Decryption must be rejected: parse with the feature enabled, then
	// disable it again before decrypting.
	ecdh1puEnabled.Store(true)
	obj := ecdh1puMustParse(t, ecdh1puTestVectorJWE)
	ecdh1puEnabled.Store(false)

	_, err = obj.Decrypt(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puBobKey,
		SenderKey:    &ecdh1puAliceKey.PublicKey,
	})
	if !errors.Is(err, ErrECDH1PUNotEnabled) {
		t.Errorf("expected ErrECDH1PUNotEnabled from Decrypt, got %v", err)
	}
}

func TestECDH1PUSenderSubstitution(t *testing.T) {
	setECDH1PU(t, true)

	// Mallory encrypts a message to Bob with her own sender key, but Bob only
	// accepts Alice as the sender.
	serialized := ecdh1puMustEncrypt(t, ECDH1PU_A128KW, A128GCM, Recipient{
		Algorithm: ECDH1PU_A128KW,
		Key:       &ecdh1puBobKey.PublicKey,
		SenderKey: ecdh1puMalloryKey,
	})
	obj := ecdh1puMustParse(t, serialized)
	_, err := obj.Decrypt(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puBobKey,
		SenderKey:    &ecdh1puAliceKey.PublicKey,
	})
	if !errors.Is(err, ErrCryptoFailure) {
		t.Errorf("expected ErrCryptoFailure when message sender is not the approved sender, got %v", err)
	}

	// Alice encrypts a message to Bob, but Bob is configured to only accept
	// Mallory as the sender.
	serialized = ecdh1puMustEncrypt(t, ECDH1PU_A128KW, A128GCM, Recipient{
		Algorithm: ECDH1PU_A128KW,
		Key:       &ecdh1puBobKey.PublicKey,
		SenderKey: ecdh1puAliceKey,
	})
	obj = ecdh1puMustParse(t, serialized)
	_, err = obj.Decrypt(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puBobKey,
		SenderKey:    &ecdh1puMalloryKey.PublicKey,
	})
	if !errors.Is(err, ErrCryptoFailure) {
		t.Errorf("expected ErrCryptoFailure when approved sender does not match, got %v", err)
	}
}

func TestECDH1PUWrongRecipientKey(t *testing.T) {
	setECDH1PU(t, true)

	serialized := ecdh1puMustEncrypt(t, ECDH1PU_A128KW, A128GCM, Recipient{
		Algorithm: ECDH1PU_A128KW,
		Key:       &ecdh1puBobKey.PublicKey,
		SenderKey: ecdh1puAliceKey,
	})
	obj := ecdh1puMustParse(t, serialized)

	// Decrypting with a different recipient private key must fail.
	_, err := obj.Decrypt(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puMalloryKey,
		SenderKey:    &ecdh1puAliceKey.PublicKey,
	})
	if !errors.Is(err, ErrCryptoFailure) {
		t.Errorf("expected ErrCryptoFailure with wrong recipient key, got %v", err)
	}
}

func TestECDH1PUTamperedKDFContext(t *testing.T) {
	setECDH1PU(t, true)

	serialized := ecdh1puMustEncrypt(t, ECDH1PU_A128KW, A128GCM, Recipient{
		Algorithm: ECDH1PU_A128KW,
		Key:       &ecdh1puBobKey.PublicKey,
		SenderKey: ecdh1puAliceKey,
		APU:       []byte("Alice"),
		APV:       []byte("Bob"),
	})

	// Tamper with the "apu" value in the protected header.
	parts := strings.Split(serialized, ".")
	if len(parts) != 5 {
		t.Fatalf("expected compact JWE with 5 parts, got %d", len(parts))
	}
	rawProtected, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("failed to decode protected header: %v", err)
	}
	var protected map[string]interface{}
	if err := json.Unmarshal(rawProtected, &protected); err != nil {
		t.Fatalf("failed to unmarshal protected header: %v", err)
	}
	protected["apu"] = base64.RawURLEncoding.EncodeToString([]byte("Mallory"))
	rawProtected, err = json.Marshal(protected)
	if err != nil {
		t.Fatalf("failed to marshal protected header: %v", err)
	}
	parts[0] = base64.RawURLEncoding.EncodeToString(rawProtected)
	tampered := strings.Join(parts, ".")

	obj := ecdh1puMustParse(t, tampered)
	_, err = obj.Decrypt(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puBobKey,
		SenderKey:    &ecdh1puAliceKey.PublicKey,
	})
	if !errors.Is(err, ErrCryptoFailure) {
		t.Errorf("expected ErrCryptoFailure with tampered apu, got %v", err)
	}
}

func TestECDH1PUSenderKeyIDMismatch(t *testing.T) {
	setECDH1PU(t, true)

	// The message claims sender key ID "alice" via "skid", but the caller's
	// approved sender key has a different key ID: reject.
	obj := ecdh1puMustParse(t, ecdh1puTestVectorJWE)
	_, err := obj.Decrypt(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puBobKey,
		SenderKey:    JSONWebKey{Key: &ecdh1puAliceKey.PublicKey, KeyID: "mallory"},
	})
	if err == nil {
		t.Error("expected error when skid header does not match the approved sender key ID")
	}
}

func TestECDH1PUPlainECKeyRejected(t *testing.T) {
	setECDH1PU(t, true)

	// A plain ECDSA private key must not be sufficient to decrypt an ECDH-1PU
	// message: the sender key is mandatory.
	obj := ecdh1puMustParse(t, ecdh1puTestVectorJWE)
	_, err := obj.Decrypt(ecdh1puBobKey)
	if err == nil {
		t.Error("expected error when decrypting ECDH-1PU message without a sender key")
	}
}

func TestECDH1PUInvalidRecipients(t *testing.T) {
	setECDH1PU(t, true)

	// Missing sender key.
	_, err := NewEncrypter(A128GCM, Recipient{
		Algorithm: ECDH1PU_A128KW,
		Key:       &ecdh1puBobKey.PublicKey,
	}, nil)
	if err == nil {
		t.Error("expected error when sender key is missing")
	}

	// Direct key agreement ("ECDH-1PU" without key wrapping) is not supported.
	_, err = NewEncrypter(A128GCM, Recipient{
		Algorithm: KeyAlgorithm("ECDH-1PU"),
		Key:       &ecdh1puBobKey.PublicKey,
		SenderKey: ecdh1puAliceKey,
	}, nil)
	if !errors.Is(err, ErrUnsupportedAlgorithm) {
		t.Errorf("expected ErrUnsupportedAlgorithm for direct ECDH-1PU, got %v", err)
	}

	// Sender and recipient keys on different curves.
	sender384, err := ecdsa.GenerateKey(elliptic.P384(), randReader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	_, err = NewEncrypter(A128GCM, Recipient{
		Algorithm: ECDH1PU_A128KW,
		Key:       &ecdh1puBobKey.PublicKey,
		SenderKey: sender384,
	}, nil)
	if err == nil {
		t.Error("expected error when sender and recipient keys are on different curves")
	}

	// Wrong key types.
	_, err = NewEncrypter(A128GCM, Recipient{
		Algorithm: ECDH1PU_A128KW,
		Key:       []byte("symmetric"),
		SenderKey: ecdh1puAliceKey,
	}, nil)
	if !errors.Is(err, ErrUnsupportedKeyType) {
		t.Errorf("expected ErrUnsupportedKeyType for symmetric recipient key, got %v", err)
	}
}

func TestECDH1PUCurves(t *testing.T) {
	setECDH1PU(t, true)

	plaintext := []byte("Lorem ipsum dolor sit amet")
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		sender, err := ecdsa.GenerateKey(curve, randReader)
		if err != nil {
			t.Fatalf("failed to generate sender key: %v", err)
		}
		recipient, err := ecdsa.GenerateKey(curve, randReader)
		if err != nil {
			t.Fatalf("failed to generate recipient key: %v", err)
		}

		serialized := ecdh1puMustEncrypt(t, ECDH1PU_A256KW, A256GCM, Recipient{
			Algorithm: ECDH1PU_A256KW,
			Key:       &recipient.PublicKey,
			SenderKey: sender,
		})
		obj := ecdh1puMustParse(t, serialized)
		out, err := obj.Decrypt(ECDH1PUDecryptionKey{
			RecipientKey: recipient,
			SenderKey:    &sender.PublicKey,
		})
		if err != nil {
			t.Errorf("failed to decrypt on %s: %v", curve.Params().Name, err)
		}
		if !bytes.Equal(plaintext, out) {
			t.Errorf("decrypted plaintext mismatch on %s", curve.Params().Name)
		}
	}
}

func TestECDH1PUMultiRecipient(t *testing.T) {
	setECDH1PU(t, true)

	plaintext := []byte("Lorem ipsum dolor sit amet")

	encrypter, err := NewMultiEncrypter(A128GCM, []Recipient{
		{
			Algorithm: ECDH1PU_A128KW,
			Key:       &ecdh1puBobKey.PublicKey,
			SenderKey: ecdh1puAliceKey,
		},
		{
			Algorithm: ECDH1PU_A256KW,
			Key:       &ecTestKey256.PublicKey,
			SenderKey: ecdh1puMalloryKey,
		},
	}, nil)
	if err != nil {
		t.Fatalf("failed to create multi-encrypter: %v", err)
	}

	obj, err := encrypter.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	parsed, err := ParseEncrypted(obj.FullSerialize(),
		[]KeyAlgorithm{ECDH1PU_A128KW, ECDH1PU_A192KW, ECDH1PU_A256KW},
		[]ContentEncryption{A128GCM})
	if err != nil {
		t.Fatalf("failed to parse JWE: %v", err)
	}

	index, _, out, err := parsed.DecryptMulti(ECDH1PUDecryptionKey{
		RecipientKey: ecdh1puBobKey,
		SenderKey:    &ecdh1puAliceKey.PublicKey,
	})
	if err != nil {
		t.Fatalf("failed to decrypt: %v", err)
	}
	if index != 0 {
		t.Errorf("expected recipient index 0, got %d", index)
	}
	if !bytes.Equal(plaintext, out) {
		t.Errorf("decrypted plaintext mismatch: got %q", out)
	}
}
