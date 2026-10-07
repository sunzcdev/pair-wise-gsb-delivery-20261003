// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPasswordAlgorithmType_String(t *testing.T) {
	assert.Equal(t, "MD5", PasswordAlgorithmMD5.String())
	assert.Equal(t, "SHA-256", PasswordAlgorithmSHA256.String())
	assert.Equal(t, "0x3", PasswordAlgorithmType(3).String())
}

func TestPasswordAlgorithm(t *testing.T) {
	p := PasswordAlgorithm{Type: PasswordAlgorithmSHA256}
	assert.Equal(t, "SHA-256 (parameters: 0x)", p.String())

	m := new(Message)
	m.WriteHeader()
	assert.NoError(t, p.AddTo(m))
	got := new(PasswordAlgorithm)
	assert.NoError(t, got.GetFrom(m))
	assert.Equal(t, p.Type, got.Type)
	assert.Empty(t, got.Parameters)

	t.Run("WithParameters", func(t *testing.T) {
		p := PasswordAlgorithm{Type: PasswordAlgorithmMD5, Parameters: []byte{1, 2, 3}}
		m := new(Message)
		m.WriteHeader()
		assert.NoError(t, p.AddTo(m))
		got := new(PasswordAlgorithm)
		assert.NoError(t, got.GetFrom(m))
		assert.Equal(t, p.Type, got.Type)
		assert.Equal(t, p.Parameters, got.Parameters)
	})
	t.Run("NotFound", func(t *testing.T) {
		mBlank := new(Message)
		mBlank.WriteHeader()
		assert.ErrorIs(t, got.GetFrom(mBlank), ErrAttributeNotFound)
	})
	t.Run("BadSize", func(t *testing.T) {
		mShort := new(Message)
		mShort.WriteHeader()
		mShort.Add(AttrPasswordAlgorithm, []byte{1, 2, 3})
		assert.ErrorIs(t, got.GetFrom(mShort), ErrBadPasswordAlgorithmSize)

		mLong := new(Message)
		mLong.WriteHeader()
		mLong.Add(AttrPasswordAlgorithm, []byte{0, 2, 0, 5, 1})
		assert.ErrorIs(t, got.GetFrom(mLong), ErrBadPasswordAlgorithmSize)
	})
}

func TestPasswordAlgorithmEncoding(t *testing.T) {
	p := PasswordAlgorithm{Type: PasswordAlgorithmSHA256}
	m := new(Message)
	m.WriteHeader()
	assert.NoError(t, p.AddTo(m))
	assert.Equal(t, messageHeaderSize+attributeHeaderSize+4, len(m.Raw))

	encodedAttr := m.Raw[messageHeaderSize:]
	assert.Equal(t, AttrPasswordAlgorithm.Value(), bin.Uint16(encodedAttr[0:2]))
	assert.Equal(t, uint16(4), bin.Uint16(encodedAttr[2:4]))
	assert.Equal(t, uint16(PasswordAlgorithmSHA256), bin.Uint16(encodedAttr[4:6]))
	assert.Equal(t, uint16(0), bin.Uint16(encodedAttr[6:8]))
}

func TestPasswordAlgorithms(t *testing.T) {
	ps := PasswordAlgorithms{
		{Type: PasswordAlgorithmSHA256},
		{Type: PasswordAlgorithmMD5, Parameters: []byte{0xaa, 0xbb}},
	}
	assert.Equal(t, "SHA-256 (parameters: 0x), MD5 (parameters: 0xaabb)", ps.String())
	assert.Equal(t, "", (PasswordAlgorithms{}).String())

	m := new(Message)
	m.WriteHeader()
	assert.NoError(t, ps.AddTo(m))
	got := new(PasswordAlgorithms)
	assert.NoError(t, got.GetFrom(m))
	assert.Equal(t, ps, *got)

	t.Run("NotFound", func(t *testing.T) {
		mBlank := new(Message)
		mBlank.WriteHeader()
		assert.ErrorIs(t, got.GetFrom(mBlank), ErrAttributeNotFound)
	})
	t.Run("BadSize", func(t *testing.T) {
		mShort := new(Message)
		mShort.WriteHeader()
		mShort.Add(AttrPasswordAlgorithms, []byte{1, 2, 3})
		assert.ErrorIs(t, got.GetFrom(mShort), ErrBadPasswordAlgorithmSize)

		mLong := new(Message)
		mLong.WriteHeader()
		mLong.Add(AttrPasswordAlgorithms, []byte{0, 2, 0, 5, 1})
		assert.ErrorIs(t, got.GetFrom(mLong), ErrBadPasswordAlgorithmSize)
	})
}
