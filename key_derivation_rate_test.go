// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package srtp

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixed master keying material from RFC 3711 Appendix B.3.
var (
	kdrTestMasterKey = []byte{
		0xE1, 0xF9, 0x7A, 0x0D, 0x3E, 0x01, 0x8B, 0xE0,
		0xD6, 0x4F, 0xA3, 0x2C, 0x06, 0xDE, 0x41, 0x39,
	}
	kdrTestMasterSalt = []byte{
		0x0E, 0xC6, 0x75, 0xAD, 0x49, 0x8A, 0xFE,
		0xEB, 0xB6, 0x96, 0x0B, 0x3A, 0xAB, 0xE6,
	}
)

const kdrTestSSRC = 0xdecafbad

func kdrTestRTPPacket(seq uint16) []byte {
	pkt := make([]byte, 12+4)
	pkt[0] = 0x80 // V=2
	pkt[1] = 0x60 // PT=96
	binary.BigEndian.PutUint16(pkt[2:], seq)
	binary.BigEndian.PutUint32(pkt[4:], 0x1234)
	binary.BigEndian.PutUint32(pkt[8:], kdrTestSSRC)
	binary.BigEndian.PutUint32(pkt[12:], uint32(seq))

	return pkt
}

func kdrTestRTCPPacket() []byte {
	return []byte{
		0x80, 0xc8, 0x00, 0x06, 0x66, 0xef, 0x91, 0xff,
		0xdf, 0x48, 0x80, 0xdd, 0x61, 0xa6, 0x2e, 0xd3,
		0xd8, 0xbc, 0xde, 0xbe, 0x00, 0x00, 0x00, 0x09,
		0x00, 0x00, 0x16, 0x04, 0x81, 0xca, 0x00, 0x06,
		0x66, 0xef, 0x91, 0xff, 0x01, 0x10, 0x52, 0x6e,
		0x54, 0x35, 0x43, 0x6d, 0x4a, 0x68, 0x7a, 0x79,
		0x65, 0x74, 0x41, 0x78, 0x77, 0x2b, 0x00, 0x00,
	}
}

func TestKeyDerivationRateOption(t *testing.T) {
	// Rates that are not a power of two or exceed 2^24 are rejected.
	for _, rate := range []uint32{3, 6, 1<<24 + 1, 1 << 25, 0xFFFFFFFF} {
		_, err := CreateContext(
			kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80, KeyDerivationRate(rate),
		)
		assert.ErrorIs(t, err, errInvalidKeyDerivationRate, "rate=%d", rate)
	}

	// Non AES-CM/HMAC-SHA1 profiles are rejected.
	for _, profile := range []ProtectionProfile{
		ProtectionProfileAeadAes128Gcm,
		ProtectionProfileAeadAes256Gcm,
		ProtectionProfileNullHmacSha1_80,
		ProtectionProfileNullHmacSha1_32,
	} {
		_, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profile, KeyDerivationRate(2))
		assert.ErrorIs(t, err, errUnsupportedKDRProfile, "profile=%v", profile)
	}

	// Zero (disabled) and powers of two up to 2^24 are accepted for all AES-CM profiles.
	for _, profile := range []ProtectionProfile{
		ProtectionProfileAes128CmHmacSha1_80,
		ProtectionProfileAes128CmHmacSha1_32,
		ProtectionProfileAes256CmHmacSha1_80,
		ProtectionProfileAes256CmHmacSha1_32,
	} {
		keyLen, err := profile.KeyLen()
		require.NoError(t, err)
		key := make([]byte, keyLen)
		for _, rate := range []uint32{0, 1, 2, 1 << 16, 1 << 24} {
			_, err = CreateContext(key, kdrTestMasterSalt, profile, KeyDerivationRate(rate))
			assert.NoError(t, err, "profile=%v rate=%d", profile, rate)
		}
	}

	// The option cannot be updated after construction.
	ctx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80)
	require.NoError(t, err)
	assert.ErrorIs(t, ctx.UpdateOptions(KeyDerivationRate(2)), ErrContextOptionNotUpdatable)
}

// Packets are encrypted with the session keys of their own derivation period:
// period 0 must be byte-identical to KDR=0 output, later periods must differ.
func TestKeyDerivationRatePeriodBoundary(t *testing.T) {
	const kdr = 4

	encKDR, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80, KeyDerivationRate(kdr),
	)
	require.NoError(t, err)
	encRef, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80)
	require.NoError(t, err)
	dec, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80,
		KeyDerivationRate(kdr), SRTPReplayProtection(64),
	)
	require.NoError(t, err)

	for seq := uint16(0); seq < 3*kdr; seq++ {
		pkt := kdrTestRTPPacket(seq)

		ctKDR, err := encKDR.EncryptRTP(nil, pkt, nil)
		require.NoError(t, err)
		ctRef, err := encRef.EncryptRTP(nil, pkt, nil)
		require.NoError(t, err)

		if seq < kdr {
			assert.Equal(t, ctRef, ctKDR, "period 0 packet %d must match KDR=0 output", seq)
		} else {
			assert.NotEqual(t, ctRef, ctKDR, "period %d packet %d must use re-derived keys", seq/kdr, seq)
		}

		out, err := dec.DecryptRTP(nil, ctKDR, nil)
		require.NoError(t, err)
		assert.Equal(t, pkt, out)
	}
}

// The derivation period advances with the 48-bit packet index across
// sequence number rollover.
func TestKeyDerivationRateSequenceRollover(t *testing.T) {
	const kdr = 1 << 16 // derivation period == ROC

	encKDR, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80, KeyDerivationRate(kdr),
	)
	require.NoError(t, err)
	encRef, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80)
	require.NoError(t, err)
	dec, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80,
		KeyDerivationRate(kdr), SRTPReplayProtection(64),
	)
	require.NoError(t, err)

	for i, seq := range []uint16{65534, 65535, 0, 1} {
		pkt := kdrTestRTPPacket(seq)

		ctKDR, err := encKDR.EncryptRTP(nil, pkt, nil)
		require.NoError(t, err)
		ctRef, err := encRef.EncryptRTP(nil, pkt, nil)
		require.NoError(t, err)

		if i < 2 { // ROC 0, period 0
			assert.Equal(t, ctRef, ctKDR, "seq=%d", seq)
		} else { // ROC 1, period 1
			assert.NotEqual(t, ctRef, ctKDR, "seq=%d", seq)
		}

		out, err := dec.DecryptRTP(nil, ctKDR, nil)
		require.NoError(t, err)
		assert.Equal(t, pkt, out)
	}

	roc, ok := dec.ROC(kdrTestSSRC)
	assert.True(t, ok)
	assert.Equal(t, uint32(1), roc)
}

// An out-of-order packet from a previous derivation period is decrypted with
// the keys of that period and is still subject to replay protection.
func TestKeyDerivationRateLatePacket(t *testing.T) {
	const kdr = 4

	enc, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80, KeyDerivationRate(kdr),
	)
	require.NoError(t, err)
	dec, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80,
		KeyDerivationRate(kdr), SRTPReplayProtection(64),
	)
	require.NoError(t, err)

	encrypted := make([][]byte, 10)
	for seq := uint16(0); seq < uint16(len(encrypted)); seq++ {
		encrypted[seq], err = enc.EncryptRTP(nil, kdrTestRTPPacket(seq), nil)
		require.NoError(t, err)
	}

	// Decrypt periods 1 and 2 first.
	for seq := 4; seq < len(encrypted); seq++ {
		out, err := dec.DecryptRTP(nil, encrypted[seq], nil)
		require.NoError(t, err)
		assert.Equal(t, kdrTestRTPPacket(uint16(seq)), out) //nolint:gosec // G115
	}

	// Late packet from period 0 uses the cached period-0 keys.
	out, err := dec.DecryptRTP(nil, encrypted[1], nil)
	require.NoError(t, err)
	assert.Equal(t, kdrTestRTPPacket(1), out)

	// Replaying it is still rejected.
	_, err = dec.DecryptRTP(nil, encrypted[1], nil)
	assert.ErrorIs(t, err, errDuplicated)
}

// The derivation cache is bounded; evicted period key material is cleared and
// re-derived on demand if a very late packet arrives.
func TestKeyDerivationRateBoundedCache(t *testing.T) {
	enc, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80, KeyDerivationRate(1),
	)
	require.NoError(t, err)
	dec, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80, KeyDerivationRate(1),
	)
	require.NoError(t, err)

	cipher, ok := dec.cipher.(*srtpCipherAesCmHmacSha1)
	require.True(t, ok)

	const packetCount = 2 * maxCachedKeyPeriods
	encrypted := make([][]byte, packetCount)
	for seq := uint16(0); seq < packetCount; seq++ {
		encrypted[seq], err = enc.EncryptRTP(nil, kdrTestRTPPacket(seq), nil)
		require.NoError(t, err)
	}

	// Decrypt the first packet and keep a reference to its period-0 keys.
	out, err := dec.DecryptRTP(nil, encrypted[0], nil)
	require.NoError(t, err)
	assert.Equal(t, kdrTestRTPPacket(0), out)
	period0Keys := cipher.derivedKeys[0]
	require.NotNil(t, period0Keys)

	for seq := 1; seq < packetCount; seq++ {
		out, err = dec.DecryptRTP(nil, encrypted[seq], nil)
		require.NoError(t, err)
		assert.Equal(t, kdrTestRTPPacket(uint16(seq)), out) //nolint:gosec // G115
		assert.LessOrEqual(t, len(cipher.derivedKeys), maxCachedKeyPeriods)
	}

	// Period 0 has been evicted and its key material cleared.
	_, ok = cipher.derivedKeys[0]
	assert.False(t, ok)
	assert.Nil(t, period0Keys.srtpBlock)
	assert.Nil(t, period0Keys.srtcpBlock)
	assert.Nil(t, period0Keys.srtpSessionSalt)
	assert.Nil(t, period0Keys.srtcpSessionSalt)
	assert.Nil(t, period0Keys.srtpSessionAuth)
	assert.Nil(t, period0Keys.srtcpSessionAuth)

	// A very late packet from an evicted period is re-derived and still decrypts.
	out, err = dec.DecryptRTP(nil, encrypted[0], nil)
	require.NoError(t, err)
	assert.Equal(t, kdrTestRTPPacket(0), out)
	assert.LessOrEqual(t, len(cipher.derivedKeys), maxCachedKeyPeriods)
}

// SRTCP selects the derivation period from the SRTCP index.
func TestKeyDerivationRateSRTCP(t *testing.T) {
	const kdr = 2

	encKDR, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80, KeyDerivationRate(kdr),
	)
	require.NoError(t, err)
	encRef, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80)
	require.NoError(t, err)
	dec, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, ProtectionProfileAes128CmHmacSha1_80,
		KeyDerivationRate(kdr), SRTCPReplayProtection(64),
	)
	require.NoError(t, err)

	// SRTCP indices 1..6 map to periods 0,1,1,2,3,3. Index 2 is kept back
	// and decrypted late below.
	encrypted := make([][]byte, 7)
	for index := uint32(1); index <= 6; index++ {
		ctKDR, err := encKDR.EncryptRTCP(nil, kdrTestRTCPPacket(), nil)
		require.NoError(t, err)
		ctRef, err := encRef.EncryptRTCP(nil, kdrTestRTCPPacket(), nil)
		require.NoError(t, err)

		if index/kdr == 0 {
			assert.Equal(t, ctRef, ctKDR, "index=%d", index)
		} else {
			assert.NotEqual(t, ctRef, ctKDR, "index=%d", index)
		}
		encrypted[index] = ctKDR

		if index == 2 {
			continue
		}

		out, err := dec.DecryptRTCP(nil, ctKDR, nil)
		require.NoError(t, err)
		assert.Equal(t, kdrTestRTCPPacket(), out)
	}

	// Late SRTCP packet from a previous period (index 2, period 1).
	out, err := dec.DecryptRTCP(nil, encrypted[2], nil)
	require.NoError(t, err)
	assert.Equal(t, kdrTestRTCPPacket(), out)

	// Replaying it is still rejected.
	_, err = dec.DecryptRTCP(nil, encrypted[2], nil)
	assert.ErrorIs(t, err, errDuplicated)
}
