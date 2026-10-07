// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

const userhashSize = sha256.Size

// Userhash represents the USERHASH attribute.
//
// USERHASH contains the SHA-256 hash of the OpaqueString username followed
// by a single colon (":") followed by the OpaqueString realm:
//
//	userhash = SHA-256 ( username ":" realm )
//
// The username and realm used in this computation MUST be SASL-prepared and
// UTF-8 encoded.
//
// RFC 8489 Section 14.2 and Section 15.6.
type Userhash []byte

// NewUserhash returns the USERHASH value computed from username and realm.
//
// username and realm must be SASL-prepared.
func NewUserhash(username, realm string) Userhash {
	return Userhash(generateUserhash(username, realm))
}

func generateUserhash(username, realm string) []byte {
	h := sha256.New()
	fmt.Fprint(h, strings.Join([]string{username, realm}, credentialsSep)) //nolint:errcheck

	return h.Sum(nil)
}

// AddTo adds USERHASH attribute to message.
func (h Userhash) AddTo(m *Message) error {
	if err := CheckSize(AttrUserhash, len(h), userhashSize); err != nil {
		return err
	}
	m.Add(AttrUserhash, h)

	return nil
}

// GetFrom parses USERHASH attribute from message.
func (h *Userhash) GetFrom(m *Message) error {
	v, err := m.Get(AttrUserhash)
	if err != nil {
		return err
	}
	if err := CheckSize(AttrUserhash, len(v), userhashSize); err != nil {
		return err
	}
	*h = append((*h)[:0], v...)

	return nil
}

// UserhashChecker checks the USERHASH attribute against the expected
// username and realm.
//
// username and realm must be SASL-prepared.
//
// RFC 8489 Section 14.2.
type UserhashChecker struct {
	Username string
	Realm    string
}

// Check verifies that the USERHASH attribute equals
// SHA-256(username:realm).
func (c UserhashChecker) Check(m *Message) error {
	var got Userhash
	if err := got.GetFrom(m); err != nil {
		return err
	}

	return checkHMAC(got, generateUserhash(c.Username, c.Realm))
}
