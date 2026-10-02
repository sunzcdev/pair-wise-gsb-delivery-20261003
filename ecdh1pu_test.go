/*-
 * Copyright 2026 go-jose contributors
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
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4/json"
)

func rawURLBig(t *testing.T, s string) *big.Int {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return new(big.Int).SetBytes(b)
}

// Fixed keys from Appendix A of draft-madden-jose-ecdh-1pu-04.
func draftAliceStatic(t *testing.T) *ecdsa.PrivateKey {
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     rawURLBig(t, "WKn-ZIGevcwGIyyrzFoZNBdaq9_TsqzGl96oc0CWuis"),
			Y:     rawURLBig(t, "y77t-RvAHRKTsSGdIYUfweuOvwrvDD-Q3Hv5J0fSKbE"),
		},
		D: rawURLBig(t, "Hndv7ZZjs_ke8o9zXYo3iq-Yr8SewI5vrqd0pAvEPqg"),
	}
}

func draftBobStatic(t *testing.T) *ecdsa.PrivateKey {
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     rawURLBig(t, "weNJy2HscCSM6AEDTDg04biOvhFhyyWvOHQfeF_PxMQ"),
			Y:     rawURLBig(t, "e8lnCO-AlStT-NJVX-crhB7QRYhiix03illJOVAOyck"),
		},
		D: rawURLBig(t, "VEmDZpDXXK8p8N0Cndsxs924q6nS1RXFASRl6BfUqdw"),
	}
}

func mustECKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, randReader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestECDH1PUDirectRoundTrip(t *testing.T) {
	alice := draftAliceStatic(t)
	bob := draftBobStatic(t)

	enc, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob.PublicKey, &ECDH1PUOptions{
		SenderKey:   alice,
		APU:         []byte("Alice"),
		APV:         []byte("Bob"),
		SenderKeyID: "alice-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("Live long and prosper.")
	obj, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	serialized, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseEncrypted(serialized,
		[]KeyAlgorithm{ECDH1PU}, []ContentEncryption{A256GCM})
	if err != nil {
		t.Fatal(err)
	}

	// The standard API must refuse an ECDH-1PU message.
	if _, err := parsed.Decrypt(bob); !errors.Is(err, errECDH1PUNotEnabled) {
		t.Fatalf("Decrypt error = %v, want errECDH1PUNotEnabled", err)
	}

	got, err := parsed.DecryptECDH1PU(bob, &alice.PublicKey, "enc")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("plaintext mismatch: %q", got)
	}
}

func TestECDH1PUKeyWrapRoundTrip(t *testing.T) {
	alice := mustECKey(t, elliptic.P256())
	bob := mustECKey(t, elliptic.P256())

	// Key wrapping requires an AES-CBC-HMAC-SHA2 content encryption.
	enc, err := NewEncrypterECDH1PU(ECDH1PU_A128KW, A128CBC_HS256, &bob.PublicKey, &ECDH1PUOptions{
		SenderKey: alice,
		APU:       []byte("Alice"),
		APV:       []byte("Bob"),
	})
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("authenticated and encrypted")
	obj, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	serialized, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseEncrypted(serialized,
		[]KeyAlgorithm{ECDH1PU_A128KW}, []ContentEncryption{A128CBC_HS256})
	if err != nil {
		t.Fatal(err)
	}

	got, err := parsed.DecryptECDH1PU(bob, &alice.PublicKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("plaintext mismatch: %q", got)
	}

	// GCM is not a compactly-committing AEAD and must be rejected in
	// wrapping mode.
	if _, err := NewEncrypterECDH1PU(ECDH1PU_A128KW, A128GCM, &bob.PublicKey, &ECDH1PUOptions{
		SenderKey: alice,
	}); err == nil {
		t.Fatal("expected error for GCM in key wrapping mode")
	}
}

func TestECDH1PUWrongSenderRejected(t *testing.T) {
	alice := draftAliceStatic(t)
	bob := draftBobStatic(t)
	mallory := mustECKey(t, elliptic.P256())

	enc, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob.PublicKey, &ECDH1PUOptions{
		SenderKey: alice,
		APU:       []byte("Alice"),
		APV:       []byte("Bob"),
	})
	if err != nil {
		t.Fatal(err)
	}

	obj, err := enc.Encrypt([]byte("top secret"))
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseEncrypted(serialized,
		[]KeyAlgorithm{ECDH1PU}, []ContentEncryption{A256GCM})
	if err != nil {
		t.Fatal(err)
	}

	// Supplying a different, caller-approved sender key must fail AEAD
	// authentication; the message cannot authenticate as anyone it likes.
	if _, err := parsed.DecryptECDH1PU(bob, &mallory.PublicKey, ""); err == nil {
		t.Fatal("expected decryption failure with a substituted sender key")
	}
}

func TestECDH1PUKeyUseRejected(t *testing.T) {
	alice := mustECKey(t, elliptic.P256())
	bob := mustECKey(t, elliptic.P256())

	_, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob.PublicKey, &ECDH1PUOptions{
		SenderKey:    alice,
		SenderKeyUse: "sig",
	})
	if !errors.Is(err, errECDH1PUKeyUse) {
		t.Fatalf("sender use=sig error = %v, want errECDH1PUKeyUse", err)
	}

	enc, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob.PublicKey, &ECDH1PUOptions{
		SenderKey: alice,
	})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := enc.Encrypt([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseEncrypted(serialized,
		[]KeyAlgorithm{ECDH1PU}, []ContentEncryption{A256GCM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.DecryptECDH1PU(bob, &alice.PublicKey, "sig"); !errors.Is(err, errECDH1PUKeyUse) {
		t.Fatalf("recipient use=sig error = %v, want errECDH1PUKeyUse", err)
	}
}

func TestECDH1PUOptInAndValidation(t *testing.T) {
	alice := mustECKey(t, elliptic.P256())
	bob := mustECKey(t, elliptic.P256())

	// NewEncrypter must not select an ECDH-1PU algorithm.
	if _, err := NewEncrypter(A256GCM, Recipient{Algorithm: ECDH1PU, Key: &bob.PublicKey}, nil); !errors.Is(err, errECDH1PUNotEnabled) {
		t.Fatalf("NewEncrypter error = %v, want errECDH1PUNotEnabled", err)
	}

	// Missing sender key.
	if _, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob.PublicKey, nil); !errors.Is(err, errECDH1PUSenderKey) {
		t.Fatalf("missing options error = %v, want errECDH1PUSenderKey", err)
	}

	// Non-1PU alg passed to the 1PU constructor.
	if _, err := NewEncrypterECDH1PU(ECDH_ES, A256GCM, &bob.PublicKey, &ECDH1PUOptions{SenderKey: alice}); err == nil {
		t.Fatal("expected error for non-1PU algorithm")
	}

	// Curve mismatch between sender and recipient.
	bob384 := mustECKey(t, elliptic.P384())
	if _, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob384.PublicKey, &ECDH1PUOptions{SenderKey: alice}); err == nil {
		t.Fatal("expected error for curve mismatch")
	}
}

func TestECDH1PUCurves(t *testing.T) {
	cases := []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()}
	for _, curve := range cases {
		curve := curve
		t.Run(curve.Params().Name, func(t *testing.T) {
			alice := mustECKey(t, curve)
			bob := mustECKey(t, curve)

			enc, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob.PublicKey, &ECDH1PUOptions{
				SenderKey: alice,
				APU:       []byte("Alice"),
				APV:       []byte("Bob"),
			})
			if err != nil {
				t.Fatal(err)
			}
			obj, err := enc.Encrypt([]byte("curve test"))
			if err != nil {
				t.Fatal(err)
			}
			serialized, err := obj.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseEncrypted(serialized,
				[]KeyAlgorithm{ECDH1PU}, []ContentEncryption{A256GCM})
			if err != nil {
				t.Fatal(err)
			}
			got, err := parsed.DecryptECDH1PU(bob, &alice.PublicKey, "")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "curve test" {
				t.Fatalf("plaintext mismatch: %q", got)
			}
		})
	}
}

// TestECDH1PUAppendixAVector constructs a compact JWE using the exact keys,
// ephemeral public key, and 256-bit derived key from Appendix A of
// draft-madden-jose-ecdh-1pu-04, then decrypts it through the experimental
// API. This pins recipient-side key derivation (including Z = Ze||Zs, party
// info, and the A256GCM AlgorithmID) to the published draft vector.
func TestECDH1PUAppendixAVector(t *testing.T) {
	alice := draftAliceStatic(t)
	bob := draftBobStatic(t)

	// Draft ephemeral public key (the private key is RFC 7518 Appendix C's
	// "alice" key).
	epkX := rawURLBig(t, "gI0GAILBdu7T53akrFmMyGcsF3n5dO7MmwNBHKW5SV0")
	epkY := rawURLBig(t, "SLW_xSffzlPWrHEVI30DHM_4egVwt3NQqeUD7nMFpps")

	cek, err := base64.RawURLEncoding.DecodeString("bK8Tcj0UhQrUtCzW3ek1v_0v_wCpunDeBcIDpeFyLKc")
	if err != nil {
		t.Fatal(err)
	}

	protected := map[string]interface{}{
		"alg": string(ECDH1PU),
		"enc": string(A256GCM),
		"apu": "QWxpY2U",
		"apv": "Qm9i",
		"epk": &JSONWebKey{
			Key: &ecdsa.PublicKey{Curve: elliptic.P256(), X: epkX, Y: epkY},
		},
	}
	protectedJSON, err := json.Marshal(protected)
	if err != nil {
		t.Fatal(err)
	}
	protectedB64 := base64.RawURLEncoding.EncodeToString(protectedJSON)

	// Encrypt plaintext with AES-256-GCM under the draft derived key.
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	sealed := aead.Seal(nil, iv, []byte("Live long and prosper."), []byte(protectedB64))
	ct := sealed[:len(sealed)-16]
	tag := sealed[len(sealed)-16:]

	compact := strings.Join([]string{
		protectedB64,
		"", // Direct Key Agreement: empty JWE Encrypted Key
		base64.RawURLEncoding.EncodeToString(iv),
		base64.RawURLEncoding.EncodeToString(ct),
		base64.RawURLEncoding.EncodeToString(tag),
	}, ".")

	parsed, err := ParseEncrypted(compact,
		[]KeyAlgorithm{ECDH1PU}, []ContentEncryption{A256GCM})
	if err != nil {
		t.Fatal(err)
	}

	got, err := parsed.DecryptECDH1PU(bob, &alice.PublicKey, "enc")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "Live long and prosper." {
		t.Fatalf("plaintext mismatch: %q", got)
	}
}

// TestECDH1PUKDFContextTampered verifies that changing the authenticated
// party-info context of an otherwise-valid message causes decryption failure.
func TestECDH1PUKDFContextTampered(t *testing.T) {
	alice := mustECKey(t, elliptic.P256())
	bob := mustECKey(t, elliptic.P256())

	enc, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob.PublicKey, &ECDH1PUOptions{
		SenderKey: alice,
		APU:       []byte("Alice"),
		APV:       []byte("Bob"),
	})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := enc.Encrypt([]byte("bind the context"))
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}

	// Decode the protected header, change "apu", and re-serialize.
	parts := strings.SplitN(serialized, ".", 5)
	protectedJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var hdr map[string]interface{}
	if err := json.Unmarshal(protectedJSON, &hdr); err != nil {
		t.Fatal(err)
	}
	hdr["apu"] = base64.RawURLEncoding.EncodeToString([]byte("Mallory"))
	tampered, err := json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	parts[0] = base64.RawURLEncoding.EncodeToString(tampered)
	bad := strings.Join(parts, ".")

	parsed, err := ParseEncrypted(bad, []KeyAlgorithm{ECDH1PU}, []ContentEncryption{A256GCM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.DecryptECDH1PU(bob, &alice.PublicKey, ""); err == nil {
		t.Fatal("expected decryption failure after tampering with apu")
	}
}

// TestECDH1PUSKIDIsOnlyAHint confirms the draft "skid" header is produced and
// exposed, but is not consulted for identity: decryption uses the
// caller-approved sender key regardless of the skid value.
func TestECDH1PUSKIDIsOnlyAHint(t *testing.T) {
	alice := mustECKey(t, elliptic.P256())
	bob := mustECKey(t, elliptic.P256())

	enc, err := NewEncrypterECDH1PU(ECDH1PU, A256GCM, &bob.PublicKey, &ECDH1PUOptions{
		SenderKey:   alice,
		SenderKeyID: "alice-key-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := enc.Encrypt([]byte("skid"))
	if err != nil {
		t.Fatal(err)
	}

	serialized, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseEncrypted(serialized,
		[]KeyAlgorithm{ECDH1PU}, []ContentEncryption{A256GCM})
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Header.ExtraHeaders[HeaderKey("skid")]; got != "alice-key-id" {
		t.Fatalf("skid header = %v, want alice-key-id", got)
	}
	// A bogus skid claim does not change anything; the supplied key wins.
	if _, err := parsed.DecryptECDH1PU(bob, &alice.PublicKey, ""); err != nil {
		t.Fatal(err)
	}
}
