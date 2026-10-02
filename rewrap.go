package jose

import (
	"encoding/base64"
	"errors"
	"fmt"
)

var (
	// ErrCannotRewrap is returned when a JWE object's layout does not allow
	// adding or removing a recipient without re-encrypting the payload. That is
	// the case for compact/flattened serializations, objects without a
	// protected header, objects that share a key-management "alg" header, and
	// objects that use the "dir" or "ECDH-ES" direct key-agreement modes
	// (which do not wrap a shared content encryption key).
	ErrCannotRewrap = errors.New("go-jose/go-jose: object cannot be re-wrapped without re-encrypting the payload")

	// ErrInvalidRecipientIndex is returned when RemoveRecipient is given an
	// index that does not identify a recipient of the object.
	ErrInvalidRecipientIndex = errors.New("go-jose/go-jose: invalid recipient index")
)

// isDirectKeyAlgorithm reports whether alg establishes the content encryption
// key directly instead of wrapping a shared key. Such algorithms cannot share
// a single CEK between recipients and therefore cannot be re-wrapped.
func isDirectKeyAlgorithm(alg KeyAlgorithm) bool {
	return alg == DIRECT || alg == ECDH_ES
}

// validateRewrapLayout verifies that obj uses the general JWE JSON
// serialization in a layout that permits adding or removing a recipient while
// leaving the protected header, AAD, IV, ciphertext and authentication tag
// byte-for-byte untouched.
func (obj JSONWebEncryption) validateRewrapLayout() error {
	// The object must have been parsed from a general JWE JSON serialization
	// carrying a non-empty "recipients" array and a protected header. Compact
	// and flattened serializations encode a single recipient and cannot carry
	// an additional recipient without changing the header layout.
	if obj.original == nil ||
		obj.original.Protected == nil ||
		len(obj.original.Protected.bytes()) == 0 ||
		len(obj.original.Recipients) < 2 {
		return fmt.Errorf("%w: only multi-recipient JWE JSON serialization is supported", ErrCannotRewrap)
	}

	// A shared key-management "alg" lives in a header that applies to every
	// recipient. Adding a recipient that uses a different algorithm, or moving
	// "alg" into a per-recipient header, would require rewriting the shared
	// (possibly protected) header, so refuse rather than silently changing it.
	if obj.protected != nil && obj.protected.getAlgorithm() != "" {
		return fmt.Errorf("%w: shared key-management algorithm in protected header", ErrCannotRewrap)
	}
	if obj.unprotected != nil && obj.unprotected.getAlgorithm() != "" {
		return fmt.Errorf("%w: shared key-management algorithm in unprotected header", ErrCannotRewrap)
	}

	// Every existing recipient must wrap the shared content encryption key.
	// Direct encryption and direct key agreement derive a per-recipient key and
	// therefore cannot be re-wrapped to another recipient.
	for i := range obj.recipients {
		alg := obj.mergedHeaders(&obj.recipients[i]).getAlgorithm()
		if alg == "" {
			return fmt.Errorf("%w: recipient %d is missing key-management algorithm", ErrCannotRewrap, i)
		}
		if isDirectKeyAlgorithm(alg) {
			return fmt.Errorf("%w: direct key-management algorithm %q cannot be re-wrapped", ErrCannotRewrap, alg)
		}
	}

	return nil
}

// rewrapRaw rebuilds the object's raw JSON form, preserving the exact original
// protected header, AAD, IV, ciphertext and tag and substituting recipients
// with the given slice. It is re-parsed so that the returned object is a fully
// validated, independently serializable JWE.
func (obj JSONWebEncryption) rewrapRaw(recipients []rawRecipientInfo) (*JSONWebEncryption, error) {
	raw := &rawJSONWebEncryption{
		// Reuse the original, already-base64-encoded fields verbatim so that
		// none of them is silently re-encoded or otherwise rewritten.
		Protected:   obj.original.Protected,
		Unprotected: obj.original.Unprotected,
		Aad:         obj.original.Aad,
		Iv:          obj.original.Iv,
		Ciphertext:  obj.original.Ciphertext,
		Tag:         obj.original.Tag,
		Recipients:  recipients,
	}

	// Build the allow-lists used while re-parsing the rebuilt object.
	keyAlgs := make([]KeyAlgorithm, 0, len(recipients))
	seen := make(map[KeyAlgorithm]struct{}, len(recipients))
	for _, recipient := range recipients {
		var alg KeyAlgorithm
		if recipient.Header != nil {
			alg = recipient.Header.getAlgorithm()
		}
		if alg == "" && obj.unprotected != nil {
			alg = obj.unprotected.getAlgorithm()
		}
		if _, ok := seen[alg]; alg != "" && !ok {
			seen[alg] = struct{}{}
			keyAlgs = append(keyAlgs, alg)
		}
	}
	if len(keyAlgs) == 0 {
		return nil, fmt.Errorf("%w: no key-management algorithms present", ErrCannotRewrap)
	}

	contentAlgs := []ContentEncryption{obj.protected.getEncryption()}
	if contentAlgs[0] == "" {
		return nil, fmt.Errorf("%w: missing content encryption algorithm", ErrCannotRewrap)
	}

	parsed, err := raw.sanitized(keyAlgs, contentAlgs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCannotRewrap, err)
	}

	return parsed, nil
}

// copyRawRecipients returns a fresh slice containing the original recipient
// records, leaving the parsed object's backing array untouched.
func copyRawRecipients(obj JSONWebEncryption) []rawRecipientInfo {
	out := make([]rawRecipientInfo, len(obj.original.Recipients))
	copy(out, obj.original.Recipients)
	return out
}

// AddRecipient re-wraps the content encryption key of an already encrypted
// multi-recipient JWE for an additional recipient and returns the resulting
// object. Unlike re-encrypting the message, this never touches the payload:
// the protected header, AAD, IV, ciphertext and authentication tag of the
// original object are preserved exactly.
//
// decryptionKey must be a valid private or symmetric key for one of the
// existing recipients. It is used to authenticate the original object (the
// authentication tag is verified) and to recover the content encryption key so
// that it can be wrapped for recipient. The recovered content encryption key
// is used only internally and is never exposed through the returned value.
//
// Only objects using the general JWE JSON serialization with a wrapped, shared
// content encryption key can be extended. Objects in compact or flattened
// serialization, objects sharing a key-management "alg" header, and objects
// using "dir" or "ECDH-ES" cause ErrCannotRewrap to be returned. A tampered
// object fails authentication and cannot be re-wrapped.
func (obj JSONWebEncryption) AddRecipient(decryptionKey interface{}, recipient Recipient) (*JSONWebEncryption, error) {
	if err := obj.validateRewrapLayout(); err != nil {
		return nil, err
	}

	// Authorize and verify the original object. This recovers the CEK only
	// after the authentication tag has been checked, so a tampered object is
	// rejected before any wrapping takes place.
	_, _, cek, _, err := obj.recoverContentEncryptionKey(decryptionKey)
	if err != nil {
		return nil, err
	}

	// Build a key encrypter for the new recipient (this also rejects "dir" and
	// "ECDH-ES", which cannot wrap a shared key).
	ctx := &genericEncrypter{}
	if err := ctx.addRecipient(recipient); err != nil {
		return nil, err
	}
	info := ctx.recipients[0]

	newRecipient, err := info.keyEncrypter.encryptKey(cek, info.keyAlg)
	if err != nil {
		return nil, err
	}

	if newRecipient.header == nil {
		newRecipient.header = &rawHeader{}
	}
	if err := newRecipient.header.set(headerAlgorithm, info.keyAlg); err != nil {
		return nil, err
	}
	if info.keyID != "" {
		if err := newRecipient.header.set(headerKeyID, info.keyID); err != nil {
			return nil, err
		}
	}

	recipients := copyRawRecipients(obj)
	recipients = append(recipients, rawRecipientInfo{
		Header:       newRecipient.header,
		EncryptedKey: base64.RawURLEncoding.EncodeToString(newRecipient.encryptedKey),
	})

	return obj.rewrapRaw(recipients)
}

// RemoveRecipient drops the recipient at the given index from a multi-recipient
// JWE and returns the resulting object, again without re-encrypting the
// payload or changing the protected header, AAD, IV, ciphertext or
// authentication tag.
//
// decryptionKey must be a valid private or symmetric key for one of the
// remaining recipients and is used to authorize the operation and verify the
// original object's authentication tag.
//
// Removing a recipient only prevents that recipient from decrypting future
// copies produced from the returned object; it does not revoke access to the
// plaintext for anyone who already holds the removed JWE Encrypted Key or an
// earlier copy of the message.
func (obj JSONWebEncryption) RemoveRecipient(decryptionKey interface{}, index int) (*JSONWebEncryption, error) {
	if err := obj.validateRewrapLayout(); err != nil {
		return nil, err
	}

	if index < 0 || index >= len(obj.original.Recipients) {
		return nil, fmt.Errorf("%w: index %d", ErrInvalidRecipientIndex, index)
	}
	if len(obj.original.Recipients) < 2 {
		return nil, fmt.Errorf("%w: removing the last recipient is not allowed", ErrCannotRewrap)
	}

	// Authorize and verify the original object.
	_, _, _, _, err := obj.recoverContentEncryptionKey(decryptionKey)
	if err != nil {
		return nil, err
	}

	recipients := make([]rawRecipientInfo, 0, len(obj.original.Recipients)-1)
	for i, recipient := range obj.original.Recipients {
		if i == index {
			continue
		}
		recipients = append(recipients, recipient)
	}

	return obj.rewrapRaw(recipients)
}
