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
	"errors"
	"fmt"

	josecipher "github.com/go-jose/go-jose/v4/cipher"
)

// DecryptECDH1PU decrypts a single-recipient JWE produced with the
// EXPERIMENTAL ECDH-1PU algorithms defined in draft-madden-jose-ecdh-1pu-04.
//
// recipientKey is the recipient's static EC private key. senderKey is the
// sender's static EC public key that the caller has approved out of band:
// it establishes the authenticated identity. The function never reads the
// "jwk", "skid", or "kid" headers to choose this key, so a message cannot
// assert its own sender. senderKeyUse is the JWK "use" of the approved
// sender key when known; "sig" is rejected.
//
// Decryption succeeds only when the message authenticates against the
// supplied sender key. Multi-recipient JWEs are not supported here.
//
// EXPERIMENTAL: targets an expired individual Internet-Draft and may change
// or be removed.
func (obj JSONWebEncryption) DecryptECDH1PU(recipientKey *ecdsa.PrivateKey, senderKey *ecdsa.PublicKey, senderKeyUse string) ([]byte, error) {
	headers := obj.mergedHeaders(nil)

	if len(obj.recipients) != 1 {
		return nil, errors.New("go-jose/go-jose: ECDH-1PU decryption expects exactly one recipient")
	}
	if err := headers.checkNoCritical(); err != nil {
		return nil, err
	}

	alg := headers.getAlgorithm()
	if !isECDH1PU(alg) {
		return nil, fmt.Errorf("%w: %q is not an ECDH-1PU algorithm", ErrUnsupportedAlgorithm, alg)
	}

	enc := headers.getEncryption()
	cipher := getContentCipher(enc)
	if cipher == nil {
		return nil, fmt.Errorf("go-jose/go-jose: unsupported enc value '%s'", string(enc))
	}

	if recipientKey == nil || senderKey == nil {
		return nil, errors.New("go-jose/go-jose: ECDH-1PU requires both a recipient private key and an approved sender public key")
	}
	if err := ensureEncUse(senderKeyUse); err != nil {
		return nil, err
	}
	if err := ensureECDH1PUCurve("recipient", &recipientKey.PublicKey); err != nil {
		return nil, err
	}
	if err := ensureECDH1PUCurve("sender", senderKey); err != nil {
		return nil, err
	}
	if recipientKey.Curve != senderKey.Curve {
		return nil, errors.New("go-jose/go-jose: ECDH-1PU recipient and sender keys must use the same curve")
	}

	recipient := obj.recipients[0]
	recipientHeaders := obj.mergedHeaders(&recipient)

	epkJWK, err := recipientHeaders.getEPK()
	if err != nil || epkJWK == nil {
		return nil, errors.New("go-jose/go-jose: invalid or missing epk header")
	}
	epkPub, ok := epkJWK.Key.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("go-jose/go-jose: epk header is not an EC public key")
	}
	if err := ensureECDH1PUCurve("ephemeral", epkPub); err != nil {
		return nil, err
	}
	if epkPub.Curve != recipientKey.Curve {
		return nil, errors.New("go-jose/go-jose: epk curve does not match recipient key curve")
	}

	apuData, err := recipientHeaders.getAPU()
	if err != nil {
		return nil, errors.New("go-jose/go-jose: invalid apu header")
	}
	apvData, err := recipientHeaders.getAPV()
	if err != nil {
		return nil, errors.New("go-jose/go-jose: invalid apv header")
	}

	parts := &aeadParts{iv: obj.iv, ciphertext: obj.ciphertext, tag: obj.tag}
	authData := obj.computeAuthData()

	var cek []byte
	if alg == ECDH1PU {
		cek = josecipher.DeriveECDH1PURecipient(
			string(enc), apuData.bytes(), apvData.bytes(),
			recipientKey, epkPub, senderKey,
			nil, cipher.keySize())
	} else {
		if !isCBCContentEncryption(enc) {
			return nil, fmt.Errorf("go-jose/go-jose: ECDH-1PU key wrapping requires an AES-CBC-HMAC-SHA2 content encryption algorithm, got %q", enc)
		}
		// The tag is attacker-controlled input to the KDF (cctag); require the
		// length mandated by the AES-CBC-HMAC-SHA2 content cipher before use.
		var expectedTag int
		switch enc {
		case A128CBC_HS256:
			expectedTag = 16
		case A192CBC_HS384:
			expectedTag = 24
		case A256CBC_HS512:
			expectedTag = 32
		}
		if len(parts.tag) != expectedTag {
			return nil, errors.New("go-jose/go-jose: invalid JWE Authentication Tag length")
		}
		if len(recipient.encryptedKey) == 0 {
			return nil, errors.New("go-jose/go-jose: missing JWE Encrypted Key")
		}
		kekSize, ok := ecdh1PUWrapKeySize(alg)
		if !ok {
			return nil, ErrUnsupportedAlgorithm
		}
		// The authentication tag is bound into the KDF (cctag).
		kek := josecipher.DeriveECDH1PURecipient(
			string(alg), apuData.bytes(), apvData.bytes(),
			recipientKey, epkPub, senderKey,
			parts.tag, kekSize)

		block, err := aes.NewCipher(kek)
		if err != nil {
			return nil, err
		}
		cek, err = josecipher.KeyUnwrap(block, recipient.encryptedKey)
		if err != nil {
			return nil, ErrCryptoFailure
		}
	}

	if err := validateContentEncryptionKeySize(cek, cipher); err != nil {
		return nil, ErrCryptoFailure
	}

	plaintext, err := cipher.decrypt(cek, authData, parts)
	if err != nil {
		return nil, ErrCryptoFailure
	}

	plaintext, err = obj.decompress(plaintext)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}
