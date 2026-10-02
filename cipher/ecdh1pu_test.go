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

package josecipher

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/hex"
	"testing"
)

// Test vector for ECDH-1PU (draft-madden-jose-ecdh-1pu-04).
//
// The static keys are the ones from JWA (RFC 7518), Appendix C, which the
// draft reuses in its own Appendix A example: Alice's key is the sender's
// static key, Bob's key is the recipient's static key. The static-static
// shared secret Zs is therefore exactly the published RFC 7518 Appendix C
// shared secret Z, which anchors this vector to a published value. The
// ephemeral key is fixed (d = SHA-256 of "go-jose ecdh-1pu test ephemeral
// key") so the vector is deterministic. The expected KEKs below were
// computed with an independent implementation of the draft-04 derivation
// (Ze || Zs into the JWA Concat KDF) and cross-checked with this package.
var eph1puKey = &ecdsa.PrivateKey{
	PublicKey: ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     fromBase64Int("LymTj9M8ZP_Z2Z7A5ysTIxJh8MCVnRp-JeY_92cYs6o="),
		Y:     fromBase64Int("zrJnRxGRm-U-9bgwzqVW4QGszbMLt-PLoDiaE6gjTuc="),
	},
	D: fromBase64Int("znZubvibMtR6aG6Bmwh9nv-22VYrMhedKOPe9_eK4Es="),
}

// Shared secrets for the test vector above.
var ecdh1puZe = mustDecodeHex("52b015abaec8fdc85fc84d5e26bbe781687aff3c13b9aeaed020313d07766970")

// ecdh1puZs is the RFC 7518 Appendix C shared secret Z.
var ecdh1puZs = mustDecodeHex("9e56d91d817135d372834283bf84269cfb316ea3da806a48f6daa7798cfe90c4")

func mustDecodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("invalid test data: " + err.Error())
	}
	return b
}

func TestVectorECDH1PU(t *testing.T) {
	apuData := []byte("Alice")
	apvData := []byte("Bob")

	vectors := []struct {
		alg      string
		size     int
		expected string
	}{
		{"ECDH-1PU+A128KW", 16, "5e8ff734970d6e6f254858ed974b188a"},
		{"ECDH-1PU+A192KW", 24, "0003e4af93457519bf7dce0fdb59320a5ed0e3aeadbb5acc"},
		{"ECDH-1PU+A256KW", 32, "6e87507a582d320358d918142fd5cea27bc1292d2fd02afca8ac1d05a9ac4e28"},
	}

	for _, v := range vectors {
		// Sender side: Ze from ephemeral private key and recipient public key,
		// Zs from sender static private key and recipient public key.
		senderKEK := DeriveECDH1PU(v.alg, apuData, apvData, eph1puKey, &bobKey.PublicKey, aliceKey, &bobKey.PublicKey, v.size)
		if got := hex.EncodeToString(senderKEK); got != v.expected {
			t.Errorf("sender-side KEK for %s: got %s, wanted %s", v.alg, got, v.expected)
		}

		// Recipient side: Ze from recipient private key and ephemeral public
		// key, Zs from recipient private key and sender static public key.
		recipientKEK := DeriveECDH1PU(v.alg, apuData, apvData, bobKey, &eph1puKey.PublicKey, bobKey, &aliceKey.PublicKey, v.size)
		if !bytes.Equal(senderKEK, recipientKEK) {
			t.Errorf("recipient-side KEK for %s does not match sender-side KEK", v.alg)
		}
	}
}

func TestECDH1PUSharedSecrets(t *testing.T) {
	// Zs must be the published RFC 7518 Appendix C shared secret, since the
	// static keys are the RFC 7518 Appendix C keys.
	zs := deriveZ(aliceKey, &bobKey.PublicKey)
	if !bytes.Equal(zs, ecdh1puZs) {
		t.Error("Zs does not match RFC 7518 Appendix C shared secret, got", hex.EncodeToString(zs))
	}

	ze := deriveZ(eph1puKey, &bobKey.PublicKey)
	if !bytes.Equal(ze, ecdh1puZe) {
		t.Error("Ze does not match expected value, got", hex.EncodeToString(ze))
	}
}

// TestECDH1PUWrongKeys checks that replacing any of the keys (e.g. sender
// substitution or a wrong recipient key) yields a different derived key.
func TestECDH1PUWrongKeys(t *testing.T) {
	apuData := []byte("Alice")
	apvData := []byte("Bob")

	expected := DeriveECDH1PU("ECDH-1PU+A128KW", apuData, apvData, bobKey, &eph1puKey.PublicKey, bobKey, &aliceKey.PublicKey, 16)

	// Sender substitution: deriving with any static public key other than the
	// genuine sender's (here the ephemeral key stands in for an attacker's
	// key) must produce a different key.
	substituted := DeriveECDH1PU("ECDH-1PU+A128KW", apuData, apvData, bobKey, &eph1puKey.PublicKey, bobKey, &eph1puKey.PublicKey, 16)
	if bytes.Equal(expected, substituted) {
		t.Error("derived key did not change when sender key was substituted")
	}

	// Wrong recipient key: deriving with a different private key must produce
	// a different key.
	wrongRecipient := DeriveECDH1PU("ECDH-1PU+A128KW", apuData, apvData, aliceKey, &eph1puKey.PublicKey, aliceKey, &aliceKey.PublicKey, 16)
	if bytes.Equal(expected, wrongRecipient) {
		t.Error("derived key did not change when recipient key was wrong")
	}

	// Key confusion: using the Zs key pair for Ze and vice versa must not
	// produce the same key.
	confused := DeriveECDH1PU("ECDH-1PU+A128KW", apuData, apvData, bobKey, &aliceKey.PublicKey, bobKey, &eph1puKey.PublicKey, 16)
	if bytes.Equal(expected, confused) {
		t.Error("derived key did not change when Ze/Zs key pairs were swapped")
	}
}

// TestECDH1PUTamperedKDFContext checks that any modification of the KDF
// context (algorithm ID, apu, apv) yields a different derived key.
func TestECDH1PUTamperedKDFContext(t *testing.T) {
	apuData := []byte("Alice")
	apvData := []byte("Bob")

	expected := DeriveECDH1PU("ECDH-1PU+A128KW", apuData, apvData, bobKey, &eph1puKey.PublicKey, bobKey, &aliceKey.PublicKey, 16)

	tampered := []struct {
		name string
		alg  string
		apu  []byte
		apv  []byte
	}{
		{"algorithm", "ECDH-1PU+A256KW", apuData, apvData},
		{"apu", "ECDH-1PU+A128KW", []byte("Mallory"), apvData},
		{"apv", "ECDH-1PU+A128KW", apuData, []byte("Mallory")},
		{"empty apu", "ECDH-1PU+A128KW", nil, apvData},
		{"empty apv", "ECDH-1PU+A128KW", apuData, nil},
	}

	for _, tc := range tampered {
		out := DeriveECDH1PU(tc.alg, tc.apu, tc.apv, bobKey, &eph1puKey.PublicKey, bobKey, &aliceKey.PublicKey, 16)
		if bytes.Equal(expected, out) {
			t.Errorf("derived key did not change with tampered %s", tc.name)
		}
	}
}
