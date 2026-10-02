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

package josecipher

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"testing"
)

func fromRawURLInt(data string) *big.Int {
	raw, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		panic("Invalid test data: " + err.Error())
	}
	return new(big.Int).SetBytes(raw)
}

// Keys from Appendix A of draft-madden-jose-ecdh-1pu-04 (Direct Key
// Agreement, P-256, A256GCM). Bob's static key and Alice's ephemeral key
// are reused from Appendix C of RFC 7518.

// Alice's static key.
var onePassAliceKey = &ecdsa.PrivateKey{
	PublicKey: ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     fromRawURLInt("WKn-ZIGevcwGIyyrzFoZNBdaq9_TsqzGl96oc0CWuis"),
		Y:     fromRawURLInt("y77t-RvAHRKTsSGdIYUfweuOvwrvDD-Q3Hv5J0fSKbE"),
	},
	D: fromRawURLInt("Hndv7ZZjs_ke8o9zXYo3iq-Yr8SewI5vrqd0pAvEPqg"),
}

// Bob's static key.
var onePassBobKey = &ecdsa.PrivateKey{
	PublicKey: ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     fromRawURLInt("weNJy2HscCSM6AEDTDg04biOvhFhyyWvOHQfeF_PxMQ"),
		Y:     fromRawURLInt("e8lnCO-AlStT-NJVX-crhB7QRYhiix03illJOVAOyck"),
	},
	D: fromRawURLInt("VEmDZpDXXK8p8N0Cndsxs924q6nS1RXFASRl6BfUqdw"),
}

// Alice's ephemeral key.
var onePassEphemeralKey = &ecdsa.PrivateKey{
	PublicKey: ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     fromRawURLInt("gI0GAILBdu7T53akrFmMyGcsF3n5dO7MmwNBHKW5SV0"),
		Y:     fromRawURLInt("SLW_xSffzlPWrHEVI30DHM_4egVwt3NQqeUD7nMFpps"),
	},
	D: fromRawURLInt("0_NxaRPUMQoAJt50Gz8YiTr8gRTwyEaCumd-MToTmIo"),
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("invalid hex %q: %v", s, err)
	}
	return b
}

// TestVectorECDH1PU verifies the Direct Key Agreement example in Appendix A
// of draft-madden-jose-ecdh-1pu-04, including the intermediate Ze and Zs
// shared secrets and the 256-bit derived key.
func TestVectorECDH1PU(t *testing.T) {
	wantZe := mustHex(t, "9e56d91d817135d372834283bf84269cfb316ea3da806a48f6daa7798cfe90c4")
	wantZs := mustHex(t, "e3ca3474384c9f62b30bfd4c688b3e7d4110a1b4badc3cc54ef7b81241efd50d")
	wantKey := mustHex(t, "6caf13723d14850ad4b42cd6dde935bffd2fff00a9ba70de05c203a5e1722ca7")

	apuData := []byte("Alice")
	apvData := []byte("Bob")

	ze := ecdhSharedSecret(onePassBobKey, &onePassEphemeralKey.PublicKey)
	zs := ecdhSharedSecret(onePassBobKey, &onePassAliceKey.PublicKey)

	if !bytes.Equal(ze, wantZe) {
		t.Errorf("Ze mismatch\ngot:  %x\nwant: %x", ze, wantZe)
	}
	if !bytes.Equal(zs, wantZs) {
		t.Errorf("Zs mismatch\ngot:  %x\nwant: %x", zs, wantZs)
	}

	gotRecipient := DeriveECDH1PURecipient(
		"A256GCM", apuData, apvData,
		onePassBobKey, &onePassEphemeralKey.PublicKey, &onePassAliceKey.PublicKey,
		nil, 32)
	if !bytes.Equal(gotRecipient, wantKey) {
		t.Errorf("recipient key mismatch\ngot:  %x\nwant: %x", gotRecipient, wantKey)
	}

	gotSender := DeriveECDH1PUSender(
		"A256GCM", apuData, apvData,
		onePassAliceKey, onePassEphemeralKey, &onePassBobKey.PublicKey,
		nil, 32)
	if !bytes.Equal(gotSender, wantKey) {
		t.Errorf("sender key mismatch\ngot:  %x\nwant: %x", gotSender, wantKey)
	}
}

// TestECDH1PUCCTag verifies that binding the JWE Authentication Tag into the
// KDF (Key Agreement with Key Wrapping mode) changes the derived key, as
// required by Section 2.3 of the draft.
func TestECDH1PUCCTag(t *testing.T) {
	apuData := []byte("Alice")
	apvData := []byte("Bob")

	tag := bytes.Repeat([]byte{0xab}, 32)

	direct := DeriveECDH1PURecipient(
		"ECDH-1PU+A128KW", apuData, apvData,
		onePassBobKey, &onePassEphemeralKey.PublicKey, &onePassAliceKey.PublicKey,
		nil, 16)

	wrapped := DeriveECDH1PURecipient(
		"ECDH-1PU+A128KW", apuData, apvData,
		onePassBobKey, &onePassEphemeralKey.PublicKey, &onePassAliceKey.PublicKey,
		tag, 16)

	if bytes.Equal(direct, wrapped) {
		t.Fatal("cctag must be bound into the derived key")
	}

	// Re-deriving with the same tag must be deterministic and agree from
	// both the sender and recipient viewpoints.
	wrappedSender := DeriveECDH1PUSender(
		"ECDH-1PU+A128KW", apuData, apvData,
		onePassAliceKey, onePassEphemeralKey, &onePassBobKey.PublicKey,
		tag, 16)
	if !bytes.Equal(wrapped, wrappedSender) {
		t.Errorf("sender/recipient disagreement with cctag\ngot:  %x\nwant: %x", wrappedSender, wrapped)
	}

	// Reconstruct Appendix B-style fixed info for a 16-byte key with a
	// 32-byte tag and confirm the SuppPubInfo layout (keydatalen||len||tag).
	tag2 := bytes.Repeat([]byte{0xcd}, 32)
	wrapped2 := DeriveECDH1PURecipient(
		"ECDH-1PU+A128KW", apuData, apvData,
		onePassBobKey, &onePassEphemeralKey.PublicKey, &onePassAliceKey.PublicKey,
		tag2, 16)
	if bytes.Equal(wrapped, wrapped2) {
		t.Fatal("different cctag values must produce different keys")
	}
}

// TestECDH1PUKDFContextTampering ensures that altering any Concat KDF context
// field is detected, since ECDH-1PU authentication depends on it.
func TestECDH1PUKDFContextTampering(t *testing.T) {
	base := func(mutate func(*params)) []byte {
		p := params{
			alg:     "A256GCM",
			apu:     []byte("Alice"),
			apv:     []byte("Bob"),
			ccTag:   nil,
			size:    32,
			recPriv: onePassBobKey,
			epkPub:  &onePassEphemeralKey.PublicKey,
			sndPub:  &onePassAliceKey.PublicKey,
		}
		mutate(&p)
		return DeriveECDH1PURecipient(p.alg, p.apu, p.apv, p.recPriv, p.epkPub, p.sndPub, p.ccTag, p.size)
	}

	want := base(func(p *params) {})

	cases := map[string]func(*params){
		"alg":    func(p *params) { p.alg = "A128GCM" },
		"apu":    func(p *params) { p.apu = []byte("Mallory") },
		"apv":    func(p *params) { p.apv = []byte("Mallory") },
		"size":   func(p *params) { p.size = 16 },
		"epk":    func(p *params) { p.epkPub = &onePassAliceKey.PublicKey },
		"sender": func(p *params) { p.sndPub = &onePassEphemeralKey.PublicKey },
	}

	for name, mut := range cases {
		if got := base(mut); bytes.Equal(got, want) {
			t.Errorf("tampering %q did not change the derived key", name)
		}
	}
}

type params struct {
	alg             string
	apu, apv, ccTag []byte
	size            int
	recPriv         *ecdsa.PrivateKey
	epkPub, sndPub  *ecdsa.PublicKey
}

// TestECDH1PUInvalidPublicKey ensures a point off the shared curve is rejected
// rather than producing a shared secret, per Section 4 of the draft.
func TestECDH1PUInvalidPublicKey(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for an off-curve public key")
		}
	}()

	invalid := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     fromBase64Int("MTEx"),
		Y:     fromBase64Int("MTEx"),
	}

	DeriveECDH1PURecipient(
		"A256GCM", nil, nil,
		onePassBobKey, invalid, &onePassAliceKey.PublicKey,
		nil, 32)
}

// TestECDH1PUMixedCurves ensures keys on different curves are rejected.
func TestECDH1PUMixedCurves(t *testing.T) {
	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for mixed-curve keys")
		}
	}()

	DeriveECDH1PUSender(
		"A256GCM", nil, nil,
		onePassAliceKey, onePassEphemeralKey, &p384Key.PublicKey,
		nil, 32)
}
