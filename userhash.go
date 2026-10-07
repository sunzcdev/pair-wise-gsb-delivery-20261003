// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"crypto/sha256"
	"fmt"
)

// Userhash represents USERHASH attribute.
//
// RFC 8489 Section 14.4.
type Userhash []byte

// NewUserhash returns Userhash computed from username and realm.
// Username and realm must be prepared using the OpaqueString profile
// of RFC 8265 as required by RFC 8489.
func NewUserhash(username, realm string) Userhash {
	h := sha256.Sum256([]byte(username + credentialsSep + realm))

	return Userhash(h[:])
}

func (u Userhash) String() string {
	return fmt.Sprintf("0x%x", []byte(u))
}

const userhashSize = 32

// AddTo adds USERHASH attribute to message.
func (u Userhash) AddTo(m *Message) error {
	if err := CheckSize(AttrUserhash, len(u), userhashSize); err != nil {
		return err
	}
	m.Add(AttrUserhash, u)

	return nil
}

// GetFrom gets USERHASH from message.
func (u *Userhash) GetFrom(m *Message) error {
	v, err := m.Get(AttrUserhash)
	if err != nil {
		return err
	}
	if err = CheckSize(AttrUserhash, len(v), userhashSize); err != nil {
		return err
	}
	*u = v

	return nil
}
