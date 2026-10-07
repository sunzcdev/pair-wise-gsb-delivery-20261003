// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/pion/stun/v4/internal/hmac"
)

// NewLongTermIntegritySHA256 returns new MessageIntegritySHA256 with key for
// long-term credentials. Password, username, and realm must be prepared
// using the OpaqueString profile of RFC 8265 as required by RFC 8489.
//
// RFC 8489 Section 14.3.
func NewLongTermIntegritySHA256(username, realm, password string) MessageIntegritySHA256 {
	k := strings.Join([]string{username, realm, password}, credentialsSep)
	h := sha256.Sum256([]byte(k))

	return MessageIntegritySHA256(h[:])
}

// MessageIntegritySHA256 represents MESSAGE-INTEGRITY-SHA256 attribute.
//
// AddTo and Check methods are using zero-allocation version of hmac, see
// newHMACSHA256 function and internal/hmac/pool.go.
//
// RFC 8489 Section 14.6.
type MessageIntegritySHA256 []byte

func newHMACSHA256(key, message, buf []byte) []byte {
	mac := hmac.AcquireSHA256(key)
	writeOrPanic(mac, message)
	defer hmac.PutSHA256(mac)

	return mac.Sum(buf)
}

func (i MessageIntegritySHA256) String() string {
	return fmt.Sprintf("KEY: 0x%x", []byte(i))
}

const (
	messageIntegritySHA256Size = 32
	// messageIntegritySHA256MinSize is the minimum size to which
	// MESSAGE-INTEGRITY-SHA256 value may be truncated.
	messageIntegritySHA256MinSize = 16
)

// ErrFingerprintBeforeIntegritySHA256 means that FINGERPRINT attribute is
// already in message, so MESSAGE-INTEGRITY-SHA256 attribute cannot be added.
var ErrFingerprintBeforeIntegritySHA256 = errors.New("FINGERPRINT before MESSAGE-INTEGRITY-SHA256 attribute")

// AddTo adds MESSAGE-INTEGRITY-SHA256 attribute to message.
//
// CPU costly, see BenchmarkMessageIntegritySHA256_AddTo.
func (i MessageIntegritySHA256) AddTo(msg *Message) error {
	for _, a := range msg.Attributes {
		// Message should not contain FINGERPRINT attribute
		// before MESSAGE-INTEGRITY-SHA256.
		if a.Type == AttrFingerprint {
			return ErrFingerprintBeforeIntegritySHA256
		}
	}
	// The text used as input to HMAC is the STUN message,
	// including the header, up to and including the attribute preceding the
	// MESSAGE-INTEGRITY-SHA256 attribute.
	length := msg.Length
	// Adjusting m.Length to contain MESSAGE-INTEGRITY-SHA256 TLV.
	msg.Length += messageIntegritySHA256Size + attributeHeaderSize
	msg.WriteLength()                                      // writing length to m.Raw
	v := newHMACSHA256(i, msg.Raw, msg.Raw[len(msg.Raw):]) // calculating HMAC for adjusted m.Raw
	msg.Length = length                                    // changing m.Length back

	// Copy hmac value to temporary variable to protect it from resetting
	// while processing m.Add call.
	vBuf := make([]byte, sha256.Size)
	copy(vBuf, v)

	msg.Add(AttrMessageIntegritySHA256, vBuf)

	return nil
}

// ErrIntegritySHA256Size means that MESSAGE-INTEGRITY-SHA256 attribute value
// length is invalid: it must be at least 16 bytes, at most 32 bytes and
// a multiple of 4.
var ErrIntegritySHA256Size = errors.New("invalid MESSAGE-INTEGRITY-SHA256 size")

// Check checks MESSAGE-INTEGRITY-SHA256 attribute.
//
// CPU costly, see BenchmarkMessageIntegritySHA256_Check.
func (i MessageIntegritySHA256) Check(msg *Message) error {
	val, err := msg.Get(AttrMessageIntegritySHA256)
	if err != nil {
		return err
	}
	// RFC 8489 Section 14.6: the HMAC-SHA256 value can be truncated, but
	// it MUST be at least 16 bytes long and a multiple of 4 bytes.
	if len(val) < messageIntegritySHA256MinSize ||
		len(val) > messageIntegritySHA256Size ||
		len(val)%padding != 0 {
		return ErrIntegritySHA256Size
	}

	// Adjusting length in header to match m.Raw that was
	// used when computing HMAC.
	var (
		length      = msg.Length
		sizeReduced int
	)
	for _, a := range msg.Attributes {
		sizeReduced += attributeHeaderSize
		sizeReduced += nearestPaddedValueLength(int(a.Length))
		if a.Type == AttrMessageIntegritySHA256 {
			break
		}
	}
	msg.Length = uint32(sizeReduced) //nolint:gosec
	msg.WriteLength()
	// startOfHMAC should be first byte of integrity attribute.
	startOfHMAC := messageHeaderSize + msg.Length - (attributeHeaderSize + uint32(len(val))) //nolint:gosec
	b := msg.Raw[:startOfHMAC]                                                               // data before integrity attribute
	expected := newHMACSHA256(i, b, msg.Raw[len(msg.Raw):])
	msg.Length = length
	msg.WriteLength() // writing length back

	return checkHMAC(val, expected[:len(val)])
}
