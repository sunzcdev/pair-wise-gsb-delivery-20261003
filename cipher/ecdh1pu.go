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
	"crypto"
	"crypto/ecdsa"
	"encoding/binary"
)

// DeriveECDH1PU derives a shared encryption key using ECDH-1PU as described in
// draft-madden-jose-ecdh-1pu-04 ("Public Key Authenticated Encryption for
// JOSE: ECDH-1PU"). Note that this document is an Internet-Draft, not a
// published RFC: the algorithm is experimental and the draft may still change
// in ways that break interoperability.
//
// The shared secret Z is the concatenation Ze || Zs, where Ze is the ECDH
// shared secret between the ephemeral key and the recipient's static key, and
// Zs is the ECDH shared secret between the sender's static key and the
// recipient's static key. Z is then fed into the Concat KDF (NIST SP 800-56A,
// as used by JWA) together with the algorithm ID and the "apu"/"apv" header
// values.
//
// The zePriv/zePub and zsPriv/zsPub arguments are the private/public key
// pairs used to compute Ze and Zs respectively:
//
//   - Sender: zePriv is the ephemeral private key, zePub is the recipient's
//     static public key; zsPriv is the sender's static private key, zsPub is
//     the recipient's static public key.
//   - Recipient: zePriv is the recipient's static private key, zePub is the
//     ephemeral public key from the "epk" header; zsPriv is the recipient's
//     static private key, zsPub is the sender's static public key (which must
//     have been authenticated out-of-band by the caller).
//
// It is an error to call this function with keys that are not on the same
// curve. Callers must ensure that the keys are valid before calling this
// function. Output size may be at most 1<<16 bytes (64 KiB).
func DeriveECDH1PU(alg string, apuData, apvData []byte, zePriv *ecdsa.PrivateKey, zePub *ecdsa.PublicKey, zsPriv *ecdsa.PrivateKey, zsPub *ecdsa.PublicKey, size int) []byte {
	if size > 1<<16 {
		panic("ECDH-1PU output size too large, must be less than or equal to 1<<16")
	}

	if !zePriv.PublicKey.Curve.IsOnCurve(zePub.X, zePub.Y) {
		panic("public key not on same curve as private key")
	}
	if !zsPriv.PublicKey.Curve.IsOnCurve(zsPub.X, zsPub.Y) {
		panic("public key not on same curve as private key")
	}

	// Ze is the shared secret between the ephemeral key and the recipient's
	// static key, Zs is the shared secret between the sender's static key and
	// the recipient's static key. Z is the concatenation Ze || Zs.
	ze := deriveZ(zePriv, zePub)
	zs := deriveZ(zsPriv, zsPub)

	z := make([]byte, 0, len(ze)+len(zs))
	z = append(z, ze...)
	z = append(z, zs...)

	// algId, partyUInfo, partyVInfo inputs must be prefixed with the length
	algID := lengthPrefixed([]byte(alg))
	ptyUInfo := lengthPrefixed(apuData)
	ptyVInfo := lengthPrefixed(apvData)

	// suppPubInfo is the encoded length of the output size in bits
	supPubInfo := make([]byte, 4)
	binary.BigEndian.PutUint32(supPubInfo, uint32(size)*8)

	reader := NewConcatKDF(crypto.SHA256, z, algID, ptyUInfo, ptyVInfo, supPubInfo, []byte{})
	key := make([]byte, size)

	// Read on the KDF will never fail
	_, _ = reader.Read(key)

	return key
}
