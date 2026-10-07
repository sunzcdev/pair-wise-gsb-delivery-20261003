// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUserhash(t *testing.T) {
	u := NewUserhash("user", "realm")
	assert.Equal(t, userhashSize, len(u))
	assert.Equal(t, fmt.Sprintf("0x%x", []byte(u)), u.String())

	m := new(Message)
	m.WriteHeader()
	assert.NoError(t, u.AddTo(m))
	got := new(Userhash)
	assert.NoError(t, got.GetFrom(m))
	assert.Equal(t, u, *got)

	t.Run("BadSize", func(t *testing.T) {
		bad := Userhash([]byte{1, 2, 3})
		assert.ErrorIs(t, bad.AddTo(m), ErrAttributeSizeInvalid)
		mBad := new(Message)
		mBad.WriteHeader()
		mBad.Add(AttrUserhash, []byte{1, 2, 3})
		assert.ErrorIs(t, got.GetFrom(mBad), ErrAttributeSizeInvalid)
	})
	t.Run("NotFound", func(t *testing.T) {
		mBlank := new(Message)
		mBlank.WriteHeader()
		assert.ErrorIs(t, got.GetFrom(mBlank), ErrAttributeNotFound)
	})
}
