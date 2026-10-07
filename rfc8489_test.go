// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRFC8489(t *testing.T) { //nolint:cyclop
	// Test Vectors for Session Traversal Utilities for NAT (STUN)
	// see https://tools.ietf.org/html/rfc8489#appendix-B.1
	//
	// Sample Request with Long-Term Authentication with
	// MESSAGE-INTEGRITY-SHA256 and USERHASH. Username and password are
	// the ones from RFC 5769.
	var (
		username = "マトリックス"
		realm    = "example.org"
		password = "TheMatrIX"
		nonce    = "f//499k954d6OL34oL9FSTvy64sA"
	)
	t.Run("Request", func(t *testing.T) {
		m := &Message{
			Raw: []byte("\x00\x01\x00\x80" +
				"\x21\x12\xa4\x42" +
				"\x78\xad\x34\x33\xc6\xad\x72\xc0\x29\xda\x41\x2e" +
				"\x00\x1e\x00\x20" +
				"\x4a\x3c\xf3\x8f\xef\x69\x92\xbd\xa9\x52\xc6\x78" +
				"\x04\x17\xda\x0f\x24\x81\x94\x15\x56\x9e\x60\xb2" +
				"\x05\xc4\x6e\x41\x40\x7f\x17\x04" +
				"\x00\x15\x00\x1c" +
				"f//499k954d6OL34oL9FSTvy64sA" +
				"\x00\x14\x00\x0b" +
				"example.org\x00" +
				"\x00\x1d\x00\x04" +
				"\x00\x02\x00\x00" +
				"\x00\x1c\x00\x20" +
				"\xa9\x2c\xbf\xfb\xc5\x9d\x79\x17\x98\x90\xad\x25" +
				"\xe5\xde\x7a\x52\x74\xbd\x6f\x3d\x4c\xbc\xde\x56" +
				"\x9e\x8b\xa7\x7b\x20\x56\xbf\xf2"),
		}
		raw := append([]byte(nil), m.Raw...)
		assert.NoError(t, m.Decode())

		u := new(Userhash)
		assert.NoError(t, u.GetFrom(m))
		assert.Equal(t, NewUserhash(username, realm), *u)

		n := new(Nonce)
		assert.NoError(t, n.GetFrom(m))
		assert.Equal(t, nonce, n.String())

		r := new(Realm)
		assert.NoError(t, r.GetFrom(m))
		assert.Equal(t, realm, r.String())

		p := new(PasswordAlgorithm)
		assert.NoError(t, p.GetFrom(m))
		assert.Equal(t, PasswordAlgorithmSHA256, p.Type)
		assert.Empty(t, p.Parameters)

		// checking HMAC-SHA256
		i := NewLongTermIntegritySHA256(username, realm, password)
		assert.NoError(t, i.Check(m))
		// message must be unchanged after check
		assert.Equal(t, raw, m.Raw)
		assert.Equal(t, uint32(len(raw)-messageHeaderSize), m.Length)

		// MESSAGE-INTEGRITY-SHA256 must not be confused with
		// MESSAGE-INTEGRITY.
		_, err := m.Get(AttrMessageIntegrity)
		assert.Error(t, err)

		t.Run("Tamper", func(t *testing.T) {
			m.Raw[40]++ // USERHASH value
			assert.Error(t, i.Check(m))
			assert.Equal(t, raw[:40], m.Raw[:40])
			m.Raw[40]--
			assert.NoError(t, i.Check(m))
			assert.Equal(t, raw, m.Raw)
		})
	})
}
