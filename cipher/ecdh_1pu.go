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
	"crypto"
	"crypto/ecdsa"
	"encoding/binary"
)

// DeriveECDH1PUSender derives a shared key for ECDH-1PU when acting as the
// sender, following Section 2 of draft-madden-jose-ecdh-1pu-04.
//
// The sender holds its own static private key (senderPriv) and the
// recipient's static public key (recipientPub), and generates the ephemeral
// key pair whose public part is published in the JWE "epk" header (epkPriv).
//
// The Concat KDF parameters follow RFC 7518 and always use SHA-256 as the
// auxiliary function, per Section 2.3 of the draft. alg is the "alg" header
// value in Key Agreement with Key Wrapping mode, or the "enc" header value
// in Direct Key Agreement mode. apuData/apvData are the decoded "apu"/"apv"
// header values. ccTag is the raw JWE Authentication Tag in Key Agreement
// with Key Wrapping mode (bound into the KDF as the cctag), and nil in
// Direct Key Agreement mode. Output size may be at most 1<<16 bytes
// (64 KiB).
//
// It is an error to provide keys on different curves. Callers must ensure
// that the keys are valid points before calling this function.
//
// This implements the fixed draft-madden-jose-ecdh-1pu-04 specification for
// interoperability; that document is an individual Internet-Draft, not a
// published RFC.
func DeriveECDH1PUSender(alg string, apuData, apvData []byte,
	senderPriv, epkPriv *ecdsa.PrivateKey, recipientPub *ecdsa.PublicKey,
	ccTag []byte, size int,
) []byte {
	ze := ecdhSharedSecret(epkPriv, recipientPub)
	zs := ecdhSharedSecret(senderPriv, recipientPub)

	return deriveECDH1PU(alg, apuData, apvData, ze, zs, ccTag, size)
}

// DeriveECDH1PURecipient derives a shared key for ECDH-1PU when acting as
// the recipient, following Section 2 of draft-madden-jose-ecdh-1pu-04.
//
// The recipient holds its own static private key (recipientPriv) and
// receives the sender's ephemeral public key in the JWE "epk" header
// (epkPub). The senderPub argument is the sender's static public key as
// approved by the caller out of band; the identity claimed inside the
// message (for example via the "jwk" or "skid" headers) must never be used
// to select this key.
//
// See DeriveECDH1PUSender for the remaining parameter semantics.
func DeriveECDH1PURecipient(alg string, apuData, apvData []byte,
	recipientPriv *ecdsa.PrivateKey, epkPub, senderPub *ecdsa.PublicKey,
	ccTag []byte, size int,
) []byte {
	ze := ecdhSharedSecret(recipientPriv, epkPub)
	zs := ecdhSharedSecret(recipientPriv, senderPub)

	return deriveECDH1PU(alg, apuData, apvData, ze, zs, ccTag, size)
}

// ecdhSharedSecret computes the ECDH shared secret between priv and pub,
// padded to the full coordinate length of the curve.
func ecdhSharedSecret(priv *ecdsa.PrivateKey, pub *ecdsa.PublicKey) []byte {
	if !priv.PublicKey.Curve.IsOnCurve(pub.X, pub.Y) {
		panic("public key not on same curve as private key")
	}

	z, _ := priv.Curve.ScalarMult(pub.X, pub.Y, priv.D.Bytes())
	zBytes := z.Bytes()

	// Note that calling z.Bytes() on a big.Int may strip leading zero bytes
	// from the returned byte array. This can lead to a problem where
	// zBytes will be shorter than expected which breaks the key derivation.
	// Therefore we must pad to the full length of the expected coordinate.
	octSize := dSize(priv.Curve)
	if len(zBytes) != octSize {
		zBytes = append(bytes.Repeat([]byte{0}, octSize-len(zBytes)), zBytes...)
	}

	return zBytes
}

func deriveECDH1PU(alg string, apuData, apvData, ze, zs, ccTag []byte, size int) []byte {
	if size > 1<<16 {
		panic("ECDH-1PU output size too large, must be less than or equal to 1<<16")
	}

	// algId, partyUInfo, partyVInfo inputs must be prefixed with the length
	algID := lengthPrefixed([]byte(alg))
	ptyUInfo := lengthPrefixed(apuData)
	ptyVInfo := lengthPrefixed(apvData)

	// suppPubInfo is the encoded length of the output size in bits,
	// followed in Key Wrapping mode by the cctag: the JWE Authentication
	// Tag prefixed by its length as a big-endian 32-bit integer
	// (Section 2.3 of the draft).
	supPubInfo := make([]byte, 4)
	binary.BigEndian.PutUint32(supPubInfo, uint32(size)*8)
	if len(ccTag) > 0 {
		supPubInfo = append(supPubInfo, lengthPrefixed(ccTag)...)
	}

	// Z is Ze followed by Zs.
	z := make([]byte, 0, len(ze)+len(zs))
	z = append(z, ze...)
	z = append(z, zs...)

	reader := NewConcatKDF(crypto.SHA256, z, algID, ptyUInfo, ptyVInfo, supPubInfo, []byte{})
	key := make([]byte, size)

	// Read on the KDF will never fail
	_, _ = reader.Read(key)

	return key
}
