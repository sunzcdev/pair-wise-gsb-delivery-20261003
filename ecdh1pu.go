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
	"crypto/ecdsa"
	"crypto/elliptic"
	"errors"
	"fmt"

	josecipher "github.com/go-jose/go-jose/v4/cipher"
	"github.com/go-jose/go-jose/v4/json"
)

// ECDH-1PU support in this package is EXPERIMENTAL. It implements the fixed
// interoperability target "Public Key Authenticated Encryption for JOSE:
// ECDH-1PU", draft-madden-jose-ecdh-1pu-04 (Madden, 2021). That document is
// an individual Internet-Draft that expired and was never published as an
// RFC; do not describe it as a published standard.
//
// The feature is strictly opt-in:
//
//   - ECDH-1PU "alg" values are never produced by NewEncrypter and never
//     consumed by Decrypt/DecryptMulti. Parsing still requires the caller to
//     explicitly list the algorithm in the key-algorithm allow list.
//   - Senders use NewEncrypterECDH1PU.
//   - Recipients use JSONWebEncryption.DecryptECDH1PU and MUST supply the
//     sender static public key they have approved out of band. The "jwk",
//     "skid", or "kid" headers carried in the message are hints only and are
//     never trusted to establish sender identity.

var (
	errECDH1PUNotEnabled = errors.New("go-jose/go-jose: ECDH-1PU is experimental and is not enabled; use the experimental ECDH-1PU API")
	errECDH1PUSenderKey  = errors.New("go-jose/go-jose: ECDH-1PU requires the sender's static private key")
	errECDH1PUKeyUse     = errors.New("go-jose/go-jose: ECDH-1PU key must be intended for encryption (use \"enc\"), not signing")
)

// isECDH1PU reports whether alg is one of the experimental ECDH-1PU
// algorithms.
func isECDH1PU(alg KeyAlgorithm) bool {
	switch alg {
	case ECDH1PU, ECDH1PU_A128KW, ECDH1PU_A192KW, ECDH1PU_A256KW:
		return true
	default:
		return false
	}
}

// ecdh1PUWrapKeySize returns the KEK size (in bytes) for a key-wrapping
// ECDH-1PU algorithm.
func ecdh1PUWrapKeySize(alg KeyAlgorithm) (int, bool) {
	switch alg {
	case ECDH1PU_A128KW:
		return 16, true
	case ECDH1PU_A192KW:
		return 24, true
	case ECDH1PU_A256KW:
		return 32, true
	default:
		return 0, false
	}
}

// supportedECDH1PUCurve reports whether the curve is enabled for the
// experimental ECDH-1PU implementation. Only the NIST prime-order curves
// listed in the draft are enabled; X25519/X448 are intentionally excluded
// for now.
func supportedECDH1PUCurve(c elliptic.Curve) bool {
	switch c {
	case elliptic.P256(), elliptic.P384(), elliptic.P521():
		return true
	default:
		return false
	}
}

// ensureECDH1PUCurve validates that the public key is non-nil, uses a
// supported curve, and is a valid point on that curve.
func ensureECDH1PUCurve(role string, key *ecdsa.PublicKey) error {
	if key == nil {
		return fmt.Errorf("go-jose/go-jose: ECDH-1PU %s key is nil", role)
	}
	if !supportedECDH1PUCurve(key.Curve) {
		return fmt.Errorf("%w: unsupported curve %q", ErrUnsupportedAlgorithm, key.Curve.Params().Name)
	}
	if !key.Curve.IsOnCurve(key.X, key.Y) {
		return errors.New("go-jose/go-jose: ECDH-1PU public key is not a valid point on its curve")
	}
	return nil
}

// ensureEncUse rejects keys explicitly marked for signing.
func ensureEncUse(use string) error {
	if use == "sig" {
		return errECDH1PUKeyUse
	}
	return nil
}

// isCBCContentEncryption reports whether enc is an AES-CBC-HMAC-SHA2
// algorithm. Key Agreement with Key Wrapping mode is restricted to these
// compactly-committing AEADs by Section 2.1 of the draft.
func isCBCContentEncryption(enc ContentEncryption) bool {
	switch enc {
	case A128CBC_HS256, A192CBC_HS384, A256CBC_HS512:
		return true
	default:
		return false
	}
}

// ECDH1PUOptions configures an experimental ECDH-1PU encrypter.
//
// EXPERIMENTAL: this API targets draft-madden-jose-ecdh-1pu-04 and may change
// or be removed.
type ECDH1PUOptions struct {
	// SenderKey is the sender's static EC private key. Required.
	SenderKey *ecdsa.PrivateKey

	// SenderKeyUse and RecipientKeyUse are the JWK "use" values ("enc" or
	// "sig") of the respective keys, when known. A value of "sig" is
	// rejected.
	SenderKeyUse    string
	RecipientKeyUse string

	// APU/APV are the raw (already decoded) agreement PartyUInfo/PartyVInfo.
	// When nil, the headers are omitted and the KDF uses empty party info.
	APU []byte
	APV []byte

	// SenderKeyID optionally sets the draft "skid" header (Sender Key ID).
	// It is a hint only and is never trusted by the recipient.
	SenderKeyID string

	// Compression and ExtraHeaders behave as in EncrypterOptions.
	Compression  CompressionAlgorithm
	ExtraHeaders map[HeaderKey]interface{}
}

// ec1puEncrypter is the experimental ECDH-1PU encrypter.
type ec1puEncrypter struct {
	alg          KeyAlgorithm
	enc          ContentEncryption
	cipher       contentCipher
	compression  CompressionAlgorithm
	senderPriv   *ecdsa.PrivateKey
	recipientPub *ecdsa.PublicKey
	apu, apv     []byte
	senderKeyID  string
	extraHeaders map[HeaderKey]interface{}
}

// NewEncrypterECDH1PU creates an EXPERIMENTAL encrypter using ECDH-1PU per
// draft-madden-jose-ecdh-1pu-04.
//
// Only explicitly enabled algorithm/curve combinations are accepted:
//
//   - algorithms ECDH-1PU, ECDH-1PU+A128KW, ECDH-1PU+A192KW, ECDH-1PU+A256KW
//   - NIST curves P-256, P-384, and P-521, identical for sender and
//     recipient keys
//   - in key-wrapping mode only the AES-CBC-HMAC-SHA2 content encryption
//     algorithms are permitted (A128CBC-HS256, A192CBC-HS384,
//     A256CBC-HS512), as required by Section 2.1 of the draft; Direct Key
//     Agreement (ECDH-1PU) allows any supported content encryption
//     algorithm.
//
// EXPERIMENTAL: the API and wire format target an expired individual
// Internet-Draft and may change or be removed.
func NewEncrypterECDH1PU(alg KeyAlgorithm, enc ContentEncryption, recipient *ecdsa.PublicKey, opts *ECDH1PUOptions) (Encrypter, error) {
	if !isECDH1PU(alg) {
		return nil, fmt.Errorf("%w: %q is not an ECDH-1PU algorithm", ErrUnsupportedAlgorithm, alg)
	}
	if opts == nil || opts.SenderKey == nil {
		return nil, errECDH1PUSenderKey
	}
	if err := ensureEncUse(opts.SenderKeyUse); err != nil {
		return nil, err
	}
	if err := ensureEncUse(opts.RecipientKeyUse); err != nil {
		return nil, err
	}

	contentCipher := getContentCipher(enc)
	if contentCipher == nil {
		return nil, ErrUnsupportedAlgorithm
	}

	if err := ensureECDH1PUCurve("sender", &opts.SenderKey.PublicKey); err != nil {
		return nil, err
	}
	if err := ensureECDH1PUCurve("recipient", recipient); err != nil {
		return nil, err
	}
	if opts.SenderKey.PublicKey.Curve != recipient.Curve {
		return nil, errors.New("go-jose/go-jose: ECDH-1PU sender and recipient keys must use the same curve")
	}

	if _, ok := ecdh1PUWrapKeySize(alg); !ok && alg != ECDH1PU {
		return nil, ErrUnsupportedAlgorithm
	}
	if _, wrap := ecdh1PUWrapKeySize(alg); wrap && !isCBCContentEncryption(enc) {
		return nil, fmt.Errorf("go-jose/go-jose: ECDH-1PU key wrapping requires an AES-CBC-HMAC-SHA2 content encryption algorithm, got %q", enc)
	}

	return &ec1puEncrypter{
		alg:          alg,
		enc:          enc,
		cipher:       contentCipher,
		compression:  opts.Compression,
		senderPriv:   opts.SenderKey,
		recipientPub: recipient,
		apu:          opts.APU,
		apv:          opts.APV,
		senderKeyID:  opts.SenderKeyID,
		extraHeaders: opts.ExtraHeaders,
	}, nil
}

func (ctx *ec1puEncrypter) Options() EncrypterOptions {
	return EncrypterOptions{Compression: ctx.compression, ExtraHeaders: ctx.extraHeaders}
}

func (ctx *ec1puEncrypter) Encrypt(plaintext []byte) (*JSONWebEncryption, error) {
	return ctx.EncryptWithAuthData(plaintext, nil)
}

func (ctx *ec1puEncrypter) EncryptWithAuthData(plaintext, aad []byte) (*JSONWebEncryption, error) {
	obj := &JSONWebEncryption{}
	obj.aad = aad
	obj.protected = &rawHeader{}

	if err := obj.protected.set(headerEncryption, ctx.enc); err != nil {
		return nil, err
	}
	if err := obj.protected.set(headerAlgorithm, ctx.alg); err != nil {
		return nil, err
	}

	// A fresh ephemeral key is generated for every message (Section 2).
	epkPriv, err := ecdsa.GenerateKey(ctx.recipientPub.Curve, randReader)
	if err != nil {
		return nil, err
	}

	epkBytes, err := json.Marshal(&JSONWebKey{Key: &epkPriv.PublicKey})
	if err != nil {
		return nil, err
	}
	if err := obj.protected.set(headerEPK, makeRawMessage(epkBytes)); err != nil {
		return nil, err
	}
	if ctx.apu != nil {
		if err := obj.protected.set(headerAPU, newBuffer(ctx.apu)); err != nil {
			return nil, err
		}
	}
	if ctx.apv != nil {
		if err := obj.protected.set(headerAPV, newBuffer(ctx.apv)); err != nil {
			return nil, err
		}
	}
	if ctx.senderKeyID != "" {
		if err := obj.protected.set(headerSKID, ctx.senderKeyID); err != nil {
			return nil, err
		}
	}

	var cek []byte
	if ctx.alg == ECDH1PU {
		// Direct Key Agreement: cctag is empty; derived key is the CEK.
		cek = josecipher.DeriveECDH1PUSender(
			string(ctx.enc), ctx.apu, ctx.apv,
			ctx.senderPriv, epkPriv, ctx.recipientPub,
			nil, ctx.cipher.keySize())
		obj.recipients = []recipientInfo{{header: &rawHeader{}}}
	} else {
		// Key Wrapping mode: random CEK, wrapping is deferred until after
		// content encryption so the authentication tag can be bound in.
		random, _, err := randomKeyGenerator{size: ctx.cipher.keySize()}.genKey()
		if err != nil {
			return nil, err
		}
		cek = random
	}

	for k, v := range ctx.extraHeaders {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		(*obj.protected)[k] = makeRawMessage(b)
	}

	if ctx.compression != NONE {
		plaintext, err = compress(ctx.compression, plaintext)
		if err != nil {
			return nil, err
		}
		if err := obj.protected.set(headerCompression, ctx.compression); err != nil {
			return nil, err
		}
	}

	authData := obj.computeAuthData()
	parts, err := ctx.cipher.encrypt(cek, authData, plaintext)
	if err != nil {
		return nil, err
	}
	obj.iv = parts.iv
	obj.ciphertext = parts.ciphertext
	obj.tag = parts.tag

	if ctx.alg != ECDH1PU {
		kekSize, _ := ecdh1PUWrapKeySize(ctx.alg)
		kek := josecipher.DeriveECDH1PUSender(
			string(ctx.alg), ctx.apu, ctx.apv,
			ctx.senderPriv, epkPriv, ctx.recipientPub,
			parts.tag, kekSize)

		block, err := aes.NewCipher(kek)
		if err != nil {
			return nil, err
		}
		encryptedKey, err := josecipher.KeyWrap(block, cek)
		if err != nil {
			return nil, err
		}
		obj.recipients = []recipientInfo{{encryptedKey: encryptedKey, header: &rawHeader{}}}
	}

	obj.recipients[0].header = nil

	return obj, nil
}
