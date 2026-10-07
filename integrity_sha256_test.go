// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMessageIntegritySHA256_AddTo(t *testing.T) {
	integrity := NewLongTermIntegritySHA256("user", "realm", "pass")
	assert.Equal(t, messageIntegritySHA256Size, len(integrity))
	m := new(Message)
	m.WriteHeader()
	assert.NoError(t, integrity.AddTo(m))
	NewSoftware("software").AddTo(m) //nolint:errcheck,gosec
	m.WriteHeader()
	dM := new(Message)
	dM.Raw = m.Raw
	assert.NoError(t, dM.Decode())
	assert.NoError(t, integrity.Check(dM))
	dM.Raw[24] += 12 // HMAC now invalid
	assert.Error(t, integrity.Check(dM))
}

func TestMessageIntegritySHA256_String(t *testing.T) {
	i := MessageIntegritySHA256([]byte("key"))
	assert.Equal(t, "KEY: 0x6b6579", i.String())
}

func TestMessageIntegritySHA256BeforeFingerprint(t *testing.T) {
	m := new(Message)
	m.WriteHeader()
	Fingerprint.AddTo(m) //nolint:errcheck,gosec
	i := NewLongTermIntegritySHA256("user", "realm", "pass")
	assert.ErrorIs(t, i.AddTo(m), ErrFingerprintBeforeIntegritySHA256)
}

func TestMessageIntegritySHA256_Truncated(t *testing.T) {
	integrity := NewLongTermIntegritySHA256("user", "realm", "pass")
	for _, size := range []int{16, 20, 24, 28, 32} {
		m := new(Message)
		m.WriteHeader()
		NewSoftware("software").AddTo(m) //nolint:errcheck,gosec
		// Computing HMAC with length adjusted for truncated value.
		length := m.Length
		m.Length += uint32(size + attributeHeaderSize) //nolint:gosec
		m.WriteLength()
		v := newHMACSHA256(integrity, m.Raw, nil)
		m.Length = length
		m.WriteLength()
		m.Add(AttrMessageIntegritySHA256, v[:size])

		raw := append([]byte(nil), m.Raw...)
		msgLen := m.Length
		assert.NoErrorf(t, integrity.Check(m), "size %d", size)
		assert.Equal(t, raw, m.Raw, "size %d", size)
		assert.Equal(t, msgLen, m.Length, "size %d", size)
	}
}

func TestMessageIntegritySHA256_InvalidSize(t *testing.T) {
	integrity := NewLongTermIntegritySHA256("user", "realm", "pass")
	for _, size := range []int{0, 4, 12, 15, 17, 18, 33, 36} {
		m := new(Message)
		m.WriteHeader()
		m.Add(AttrMessageIntegritySHA256, make([]byte, size))
		assert.ErrorIsf(t, integrity.Check(m), ErrIntegritySHA256Size, "size %d", size)
	}
}

func TestMessageIntegritySHA256_DualIntegrity(t *testing.T) { //nolint:cyclop
	var (
		integrity    = NewShortTermIntegrity("password")
		integrity256 = MessageIntegritySHA256("password")
	)
	build := func() *Message {
		m := new(Message)
		m.Type = BindingRequest
		m.WriteHeader()
		NewSoftware("software").AddTo(m) //nolint:errcheck,gosec
		assert.NoError(t, integrity.AddTo(m))
		assert.NoError(t, integrity256.AddTo(m))
		assert.NoError(t, Fingerprint.AddTo(m))

		// Re-decoding so that attribute values point into m.Raw.
		d := New()
		_, err := d.ReadFrom(bytes.NewReader(m.Raw))
		assert.NoError(t, err)

		return d
	}
	snapshot := func(m *Message) ([]byte, uint32) {
		return append([]byte(nil), m.Raw...), m.Length
	}
	t.Run("BothValid", func(t *testing.T) {
		m := build()
		raw, msgLen := snapshot(m)
		assert.NoError(t, integrity.Check(m))
		assert.NoError(t, integrity256.Check(m))
		assert.NoError(t, Fingerprint.Check(m))
		assert.Equal(t, raw, m.Raw)
		assert.Equal(t, msgLen, m.Length)
	})
	t.Run("TamperCoveredByBoth", func(t *testing.T) {
		m := build()
		raw, msgLen := snapshot(m)
		m.Raw[24]++ // SOFTWARE value, covered by both HMACs
		assert.Error(t, integrity.Check(m))
		assert.Equal(t, msgLen, m.Length)
		assert.Error(t, integrity256.Check(m))
		assert.Equal(t, msgLen, m.Length)
		assert.Equal(t, raw[:24], m.Raw[:24])
	})
	t.Run("TamperSHA256ValueOnly", func(t *testing.T) {
		m := build()
		raw, msgLen := snapshot(m)
		// Tampering the last byte of MESSAGE-INTEGRITY-SHA256 value,
		// which is not covered by MESSAGE-INTEGRITY.
		idx := bytes.Index(m.Raw, []byte{0x00, 0x1c, 0x00, 0x20})
		assert.NotEqual(t, -1, idx)
		m.Raw[idx+attributeHeaderSize+messageIntegritySHA256Size-1]++
		assert.NoError(t, integrity.Check(m))
		assert.Error(t, integrity256.Check(m))
		assert.Equal(t, msgLen, m.Length)
		assert.Equal(t, raw[:idx], m.Raw[:idx])
	})
}

func BenchmarkMessageIntegritySHA256_AddTo(b *testing.B) {
	m := new(Message)
	integrity := NewLongTermIntegritySHA256("user", "realm", "pass")
	m.WriteHeader()
	b.ReportAllocs()
	b.SetBytes(int64(len(m.Raw)))
	for i := 0; i < b.N; i++ {
		m.WriteHeader()
		assert.NoError(b, integrity.AddTo(m))
		m.Reset()
	}
}

func BenchmarkMessageIntegritySHA256_Check(b *testing.B) {
	m := new(Message)
	m.Raw = make([]byte, 0, 1024)
	NewSoftware("software").AddTo(m) //nolint:errcheck,gosec
	integrity := NewLongTermIntegritySHA256("user", "realm", "pass")
	b.ReportAllocs()
	m.WriteHeader()
	b.SetBytes(int64(len(m.Raw)))
	assert.NoError(b, integrity.AddTo(m))
	m.WriteLength()
	for i := 0; i < b.N; i++ {
		assert.NoError(b, integrity.Check(m))
	}
}
