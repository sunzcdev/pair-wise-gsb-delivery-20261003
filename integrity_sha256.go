// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"crypto/md5" //nolint:gosec
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/pion/stun/v4/internal/hmac"
)

// NewLongTermIntegritySHA256 returns new MessageIntegritySHA256 with key for
// long-term credentials, deriving the key with SHA-256 as specified by
// RFC 8489:
//
//	key = SHA-256 ( username ":" realm ":" password )
//
// username, realm and password must be SASL-prepared.
func NewLongTermIntegritySHA256(username, realm, password string) MessageIntegritySHA256 {
	h := sha256.New()
	fmt.Fprint(h, strings.Join([]string{username, realm, password}, credentialsSep)) //nolint:errcheck

	return MessageIntegritySHA256(h.Sum(nil))
}

// NewShortTermIntegritySHA256 returns new MessageIntegritySHA256 with key for
// short-term credentials. Password must be SASL-prepared.
func NewShortTermIntegritySHA256(password string) MessageIntegritySHA256 {
	return MessageIntegritySHA256(password)
}

// MessageIntegritySHA256 represents the MESSAGE-INTEGRITY-SHA256 attribute.
//
// The attribute carries an HMAC-SHA256 value, the same size (32 bytes) as the
// output of SHA-256. A truncated HMAC of 16 bytes is also accepted when
// verifying messages, as permitted by RFC 8489 Section 14.4.
//
// RFC 8489 Section 14.4 and Section 15.5.
type MessageIntegritySHA256 []byte

func (i MessageIntegritySHA256) String() string {
	return fmt.Sprintf("SHA-256 KEY: 0x%x", []byte(i))
}

const (
	messageIntegritySHA256Size      = sha256.Size // 32 bytes
	messageIntegritySHA256TruncSize = 16          // truncated length
)

func newHMACSHA256(key, message, buf []byte) []byte {
	mac := hmac.AcquireSHA256(key)
	writeOrPanic(mac, message)
	defer hmac.PutSHA256(mac)

	return mac.Sum(buf)
}

// AddTo adds MESSAGE-INTEGRITY-SHA256 attribute to message.
//
// The HMAC is computed over the STUN message up to and including the
// attribute preceding MESSAGE-INTEGRITY-SHA256, with the message length field
// temporarily adjusted to also cover the MESSAGE-INTEGRITY-SHA256 attribute.
// When both MESSAGE-INTEGRITY and MESSAGE-INTEGRITY-SHA256 are present, the
// SHA-256 attribute MUST follow MESSAGE-INTEGRITY and its HMAC therefore
// covers MESSAGE-INTEGRITY.
func (i MessageIntegritySHA256) AddTo(msg *Message) error {
	for _, a := range msg.Attributes {
		// FINGERPRINT must be the final attribute, so no integrity
		// attribute can follow it.
		if a.Type == AttrFingerprint {
			return ErrFingerprintBeforeIntegrity
		}
	}

	// The text used as input to HMAC is the STUN message, including the
	// header, up to and including the attribute preceding the
	// MESSAGE-INTEGRITY-SHA256 attribute.
	length := msg.Length
	// Adjusting length to contain MESSAGE-INTEGRITY-SHA256 TLV.
	msg.Length += messageIntegritySHA256Size + attributeHeaderSize
	msg.WriteLength()
	v := newHMACSHA256(i, msg.Raw, msg.Raw[len(msg.Raw):])
	msg.Length = length

	// Copy the hmac value into a private buffer to protect it from being
	// reset while processing msg.Add.
	vBuf := make([]byte, messageIntegritySHA256Size)
	copy(vBuf, v)

	msg.Add(AttrMessageIntegritySHA256, vBuf)

	return nil
}

// Check checks MESSAGE-INTEGRITY-SHA256 attribute.
//
// The authenticated text is the STUN message up to and including the
// attribute preceding MESSAGE-INTEGRITY-SHA256, with the length field
// temporarily adjusted to point past MESSAGE-INTEGRITY-SHA256. The
// attribute itself and any following FINGERPRINT are excluded; when
// MESSAGE-INTEGRITY precedes it, the SHA-1 attribute is covered by the HMAC.
// Neither the message length field nor Raw is modified after the call, even
// on failure.
func (i MessageIntegritySHA256) Check(msg *Message) error {
	val, err := msg.Get(AttrMessageIntegritySHA256)
	if err != nil {
		return err
	}

	if len(val) != messageIntegritySHA256Size &&
		len(val) != messageIntegritySHA256TruncSize {
		return ErrAttributeSizeInvalid
	}

	// Compute the size of the message up to and including the
	// MESSAGE-INTEGRITY-SHA256 attribute.
	var (
		length      = msg.Length
		sizeReduced int
		foundSHA256 bool
	)
	for _, a := range msg.Attributes {
		if a.Type == AttrMessageIntegritySHA256 {
			// For the length adjustment and HMAC input, the integrity
			// attribute is counted by its full on-wire TLV size, which is
			// 36 bytes for the full HMAC and 20 bytes when truncated to 16.
			sizeReduced += attributeHeaderSize + nearestPaddedValueLength(int(a.Length))
			foundSHA256 = true

			break
		}
		sizeReduced += attributeHeaderSize
		sizeReduced += nearestPaddedValueLength(int(a.Length))
	}
	if !foundSHA256 {
		return ErrAttributeNotFound
	}

	// Always restore the header, regardless of the comparison result.
	defer func() {
		msg.Length = length
		msg.WriteLength()
	}()

	msg.Length = uint32(sizeReduced) //nolint:gosec // G115
	msg.WriteLength()
	startOfHMAC := messageHeaderSize + msg.Length -
		(attributeHeaderSize + uint32(len(val)))
	b := msg.Raw[:startOfHMAC] // data before integrity attribute
	expected := newHMACSHA256(i, b, msg.Raw[len(msg.Raw):])

	return checkHMAC(val, expected[:len(val)])
}

// ErrNoPasswordAlgorithm means that a PASSWORD-ALGORITHM(S) attribute could
// not be used to derive a long-term integrity key because no known algorithm
// was found.
var ErrNoPasswordAlgorithm = errors.New("no supported PASSWORD-ALGORITHM found")

// LongTermIntegrityForAlgorithm derives the long-term integrity key for the
// requested algorithm.
//
// PasswordAlgorithmMD5 yields the RFC 5389 key usable with MESSAGE-INTEGRITY,
// while PasswordAlgorithmSHA256 yields the RFC 8489 key usable with
// MESSAGE-INTEGRITY-SHA256. username, realm and password must be
// SASL-prepared.
func LongTermIntegrityForAlgorithm(
	algorithm PasswordAlgorithm,
	username, realm, password string,
) (MessageIntegrity, MessageIntegritySHA256, error) {
	credentials := []string{username, realm, password}
	switch algorithm {
	case PasswordAlgorithmMD5:
		h := md5.New()                                           //nolint:gosec
		fmt.Fprint(h, strings.Join(credentials, credentialsSep)) //nolint:errcheck

		return MessageIntegrity(h.Sum(nil)), nil, nil
	case PasswordAlgorithmSHA256:
		h := sha256.New()
		fmt.Fprint(h, strings.Join(credentials, credentialsSep)) //nolint:errcheck

		return nil, MessageIntegritySHA256(h.Sum(nil)), nil
	default:
		return nil, nil, ErrNoPasswordAlgorithm
	}
}
