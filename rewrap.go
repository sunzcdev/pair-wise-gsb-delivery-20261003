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
	"errors"
	"fmt"
)

// ErrCannotRewrap indicates that a JWE object cannot be rewrapped. This
// happens when the object's header layout does not permit adding a recipient
// while leaving the encrypted content untouched, e.g. compact or flattened
// serializations, or key agreement algorithms such as "dir" and "ECDH-ES"
// that never wrap an independently usable content encryption key.
var ErrCannotRewrap = errors.New("go-jose/go-jose: JWE object cannot be rewrapped")

// Rewrap adds a recipient to a multi-recipient JWE without re-encrypting the
// payload. The caller authorizes the operation by proving possession of a
// valid decryption key for one of the existing recipients: the object's
// authentication tag is verified with the recovered content encryption key
// (CEK) before the same CEK is wrapped for the new recipient. The CEK is used
// only internally and is never returned.
//
// Only the JWE JSON General Serialization, with recipients that use key
// wrapping or key agreement with key wrapping, can be rewrapped. Objects using
// the compact or flattened JSON serialization, or the "dir" and "ECDH-ES"
// algorithms (which derive or share the CEK directly instead of wrapping it),
// are rejected with ErrCannotRewrap.
//
// The protected header, additional authenticated data, initialization
// vector, ciphertext and authentication tag are copied byte-for-byte from the
// original object; only the recipients array changes.
//
// Rewrapping only changes the object returned to the caller. It cannot revoke
// access of recipients who were removed, nor invalidate copies of older
// objects that were already distributed: anyone holding a valid object can
// still decrypt the payload with their copy.
func (obj JSONWebEncryption) Rewrap(decryptionKey interface{}, newRecipient Recipient) (*JSONWebEncryption, error) {
	// The general JSON serialization is the only layout that carries a
	// per-recipient "recipients" array. Compact and flattened objects place
	// recipient data in top-level fields; silently restructuring them would
	// rewrite the object rather than merely adding a recipient.
	if obj.original == nil || obj.original.Recipients == nil {
		return nil, fmt.Errorf("%w: object is not in JWE JSON General Serialization", ErrCannotRewrap)
	}

	if len(obj.recipients) == 0 {
		return nil, fmt.Errorf("%w: object has no recipients", ErrCannotRewrap)
	}

	sharedHeaders := obj.mergedHeaders(nil)

	// "dir" and "ECDH-ES" (without key wrapping) use the CEK directly and
	// never produce an independently wrapped CEK that can be re-shared.
	if sharedAlg := sharedHeaders.getAlgorithm(); sharedAlg == DIRECT || sharedAlg == ECDH_ES {
		return nil, fmt.Errorf("%w: key algorithm %q does not wrap a content encryption key", ErrCannotRewrap, sharedAlg)
	}
	for i, recipient := range obj.recipients {
		alg := obj.mergedHeaders(&recipient).getAlgorithm()
		if alg == DIRECT || alg == ECDH_ES {
			return nil, fmt.Errorf("%w: recipient %d uses key algorithm %q, which does not wrap a content encryption key", ErrCannotRewrap, i, alg)
		}
	}

	// If the shared header pins an algorithm or key ID, the new recipient must
	// be consistent with it. Per-recipient headers must not duplicate "alg" or
	// "kid" that are already present in the shared header.
	if sharedAlg := sharedHeaders.getAlgorithm(); sharedAlg != "" && newRecipient.Algorithm != sharedAlg {
		return nil, fmt.Errorf("%w: new recipient algorithm %q does not match shared header algorithm %q", ErrCannotRewrap, newRecipient.Algorithm, sharedAlg)
	}
	if sharedKid := sharedHeaders.getString(headerKeyID); sharedKid != "" && newRecipient.KeyID != "" && newRecipient.KeyID != sharedKid {
		return nil, fmt.Errorf("%w: new recipient key ID %q does not match shared header key ID %q", ErrCannotRewrap, newRecipient.KeyID, sharedKid)
	}

	switch newRecipient.Algorithm {
	case "", DIRECT, ECDH_ES:
		return nil, fmt.Errorf("%w: key algorithm %q does not wrap a content encryption key", ErrCannotRewrap, newRecipient.Algorithm)
	}

	// Authorize the operation by proving a valid decryption capability and, in
	// the same step, verify that the original object is authentic. The CEK is
	// recovered locally and never returned to the caller.
	cek, err := obj.unwrapContentKey(decryptionKey)
	if err != nil {
		return nil, err
	}

	newInfo, err := makeJWERecipient(newRecipient.Algorithm, newRecipient.Key)
	if err != nil {
		return nil, err
	}

	switch newRecipient.Algorithm {
	case PBES2_HS256_A128KW, PBES2_HS384_A192KW, PBES2_HS512_A256KW:
		if sr, ok := newInfo.keyEncrypter.(*symmetricKeyCipher); ok {
			sr.p2c = newRecipient.PBES2Count
			sr.p2s = newRecipient.PBES2Salt
		}
	}

	recipient, err := newInfo.keyEncrypter.encryptKey(cek, newRecipient.Algorithm)
	if err != nil {
		return nil, err
	}

	if recipient.header == nil {
		recipient.header = &rawHeader{}
	}

	if sharedHeaders.getAlgorithm() == "" {
		if err := recipient.header.set(headerAlgorithm, newRecipient.Algorithm); err != nil {
			return nil, err
		}
	}

	keyID := newInfo.keyID
	if newRecipient.KeyID != "" {
		keyID = newRecipient.KeyID
	}
	if keyID != "" && sharedHeaders.getString(headerKeyID) == "" {
		if err := recipient.header.set(headerKeyID, keyID); err != nil {
			return nil, err
		}
	}

	// Copy the existing recipients rather than mutating the parsed object, then
	// append the newly wrapped recipient.
	recipients := make([]recipientInfo, len(obj.recipients), len(obj.recipients)+1)
	copy(recipients, obj.recipients)
	recipients = append(recipients, recipient)

	out := obj
	out.recipients = recipients
	// Keep original so the authenticated protected segment is emitted
	// byte-for-byte identical when serializing. FullSerialize builds the
	// recipients array from out.recipients, not original.

	return &out, nil
}

// unwrapContentKey recovers the CEK using one of the object's recipients and
// verifies the object's authentication tag. It mirrors DecryptMulti but never
// decompresses or returns the plaintext, and returns ErrCryptoFailure when no
// recipient yields a valid key or the tag does not verify.
func (obj JSONWebEncryption) unwrapContentKey(decryptionKey interface{}) ([]byte, error) {
	globalHeaders := obj.mergedHeaders(nil)

	if err := globalHeaders.checkNoCritical(); err != nil {
		return nil, err
	}

	key, err := tryJWKS(decryptionKey, obj.Header)
	if err != nil {
		return nil, err
	}
	decrypter, err := newDecrypter(key)
	if err != nil {
		return nil, err
	}

	cipher := getContentCipher(globalHeaders.getEncryption())
	if cipher == nil {
		return nil, fmt.Errorf("go-jose/go-jose: unsupported enc value '%s'", string(globalHeaders.getEncryption()))
	}

	generator := randomKeyGenerator{size: cipher.keySize()}
	parts := &aeadParts{
		iv:         obj.iv,
		ciphertext: obj.ciphertext,
		tag:        obj.tag,
	}
	authData := obj.computeAuthData()

	for _, recipient := range obj.recipients {
		recipientHeaders := obj.mergedHeaders(&recipient)

		cek, err := decrypter.decryptKey(recipientHeaders, &recipient, generator)
		if err != nil {
			continue
		}
		if err := validateContentEncryptionKeySize(cek, cipher); err != nil {
			continue
		}
		// Verifying the tag with the recovered CEK authenticates protected
		// header, AAD, IV and ciphertext of the original object.
		if _, err := cipher.decrypt(cek, authData, parts); err != nil {
			continue
		}
		return cek, nil
	}

	return nil, ErrCryptoFailure
}
