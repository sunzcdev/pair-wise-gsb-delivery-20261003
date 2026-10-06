// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package srtp

import (
	"errors"
	"testing"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func buildKDRTestRTPPacket(t *testing.T, sequenceNumber uint16, ssrc uint32) []byte {
	t.Helper()

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: sequenceNumber,
			Timestamp:      0x11223344,
			SSRC:           ssrc,
		},
		Payload: []byte{0x10, 0x20, 0x30, 0x40, byte(sequenceNumber >> 8), byte(sequenceNumber)}, //nolint:gosec // G115
	}
	raw, err := pkt.Marshal()
	require.NoError(t, err)

	return raw
}

func encryptKDRTestRTP(t *testing.T, ctx *Context, sequenceNumber uint16, ssrc uint32) []byte {
	t.Helper()

	encrypted, err := ctx.EncryptRTP(nil, buildKDRTestRTPPacket(t, sequenceNumber, ssrc), nil)
	require.NoError(t, err)

	return encrypted
}

func decryptKDRTestRTP(t *testing.T, ctx *Context, encrypted []byte) []byte {
	t.Helper()

	decrypted, err := ctx.DecryptRTP(nil, encrypted, nil)
	require.NoError(t, err)

	return decrypted
}

func TestKeyDerivationRateOption(t *testing.T) {
	key := make([]byte, 16)
	salt := make([]byte, 14)

	// The rate must be 0 or a power of two not greater than 2^24.
	for _, rate := range []uint64{3, 5, 6, 7, 1<<24 + 1, 1 << 25, 1 << 40} {
		_, err := CreateContext(key, salt, profileCTR, KeyDerivationRate(rate))
		assert.ErrorIs(t, err, errInvalidKeyDerivationRate, "rate %d must be rejected", rate)
	}

	for _, rate := range []uint64{0, 1, 2, 1 << 10, 1 << 24} {
		_, err := CreateContext(key, salt, profileCTR, KeyDerivationRate(rate))
		assert.NoError(t, err, "rate %d must be accepted", rate)
	}

	// Non-zero rates are only supported for AES-CM/HMAC-SHA1 (and NULL) profiles.
	for _, profile := range []ProtectionProfile{ProtectionProfileAeadAes128Gcm, ProtectionProfileAeadAes256Gcm} {
		keyLen, err := profile.KeyLen()
		require.NoError(t, err)
		saltLen, err := profile.SaltLen()
		require.NoError(t, err)

		_, err = CreateContext(make([]byte, keyLen), make([]byte, saltLen), profile, KeyDerivationRate(1<<10))
		assert.ErrorIs(t, err, errUnsupportedKDRProfile)
	}

	// The option is construction-only.
	ctx, err := CreateContext(key, salt, profileCTR)
	require.NoError(t, err)
	assert.ErrorIs(t, ctx.UpdateOptions(KeyDerivationRate(1<<10)), ErrContextOptionNotUpdatable)
}

// Packets before a key derivation period boundary must use the same keys as
// with kdr=0, packets at and after the boundary must use freshly derived keys,
// and both directions must agree on the period of every packet.
func TestSRTPKeyDerivationRatePeriodBoundary(t *testing.T) {
	const kdr = 1 << 4 // 16 packets per period
	const ssrc = 0xCAFEBABE

	encCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(kdr))
	require.NoError(t, err)
	decCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(kdr))
	require.NoError(t, err)

	// Reference context without periodic re-derivation.
	refCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR)
	require.NoError(t, err)

	for seq := uint16(0); seq < 3*kdr; seq++ {
		encrypted := encryptKDRTestRTP(t, encCtx, seq, ssrc)
		reference := encryptKDRTestRTP(t, refCtx, seq, ssrc)

		if seq < kdr {
			// Period 0 derives the same session keys as kdr=0.
			assert.Equal(t, reference, encrypted, "period 0 packet %d must match kdr=0 output", seq)
		} else {
			assert.NotEqual(t, reference, encrypted, "period %d packet %d must use re-derived keys", seq/kdr, seq)
		}

		decrypted := decryptKDRTestRTP(t, decCtx, encrypted)
		assert.Equal(t, buildKDRTestRTPPacket(t, seq, ssrc), decrypted)
	}
}

// The key derivation period is selected by the 48-bit SRTP packet index
// (2^16 * ROC + SEQ), so a sequence number rollover that crosses a period
// boundary must switch to the keys of the new period.
func TestSRTPKeyDerivationRateRollover(t *testing.T) {
	const kdr = 1 << 16 // one period per rollover counter value
	const ssrc = 0x00BADA55

	encCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(kdr))
	require.NoError(t, err)
	decCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(kdr))
	require.NoError(t, err)

	// Send packets across the sequence number wrap: 65530..65535 (period 0),
	// then 0..5 with ROC=1 (period 1).
	seqs := []uint16{65530, 65531, 65532, 65533, 65534, 65535, 0, 1, 2, 3, 4, 5}
	for _, seq := range seqs {
		encrypted := encryptKDRTestRTP(t, encCtx, seq, ssrc)
		decrypted := decryptKDRTestRTP(t, decCtx, encrypted)
		assert.Equal(t, buildKDRTestRTPPacket(t, seq, ssrc), decrypted)
	}

	roc, ok := decCtx.ROC(ssrc)
	require.True(t, ok)
	assert.Equal(t, uint32(1), roc)

	cipher, ok := decCtx.cipher.(*srtpCipherAesCmHmacSha1)
	require.True(t, ok)
	assert.Contains(t, cipher.periodKeys, uint64(0))
	assert.Contains(t, cipher.periodKeys, uint64(1))
}

// A late out-of-order packet from a previous key derivation period must be
// authenticated and decrypted with the keys of that period, and must still be
// subject to replay protection.
func TestSRTPKeyDerivationRateLatePacket(t *testing.T) {
	const kdr = 1 << 4
	const ssrc = 0x0D15EA5E

	encCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(kdr))
	require.NoError(t, err)
	decCtx, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, profileCTR,
		KeyDerivationRate(kdr), SRTPReplayProtection(256),
	)
	require.NoError(t, err)

	// Encrypt enough packets to evict the oldest periods from the bounded
	// derivation cache (period 0 is evicted after maxKDRPeriodsCached periods).
	const packetCount = 200
	encrypted := make([][]byte, packetCount)
	for seq := uint16(0); seq < packetCount; seq++ {
		encrypted[seq] = encryptKDRTestRTP(t, encCtx, seq, ssrc)
	}

	// Deliver everything except two withheld packets: one from period 0
	// (already evicted from the cache) and one from period 1.
	withheld := map[uint16]bool{5: true, 20: true}
	for seq := uint16(0); seq < packetCount; seq++ {
		if withheld[seq] {
			continue
		}
		decrypted := decryptKDRTestRTP(t, decCtx, encrypted[seq])
		assert.Equal(t, buildKDRTestRTPPacket(t, seq, ssrc), decrypted)
	}

	// Late packets from previous periods must still verify against the keys
	// of their own period, transparently re-derived if evicted.
	for seq := range withheld {
		decrypted := decryptKDRTestRTP(t, decCtx, encrypted[seq])
		assert.Equal(t, buildKDRTestRTPPacket(t, seq, ssrc), decrypted)
	}

	// Replayed late packets must be rejected.
	for seq := range withheld {
		_, err = decCtx.DecryptRTP(nil, encrypted[seq], nil)
		assert.ErrorIs(t, err, errDuplicated, "replayed packet %d must be rejected", seq)
	}
}

// SRTCP selects the key derivation period from the SRTCP packet index,
// independently of SRTP.
func TestSRTCPKeyDerivationRate(t *testing.T) {
	const kdr = 1 << 2 // 4 packets per period
	const ssrc = 0x11223344

	buildRTCP := func() []byte {
		pkt := &rtcp.ReceiverReport{
			SSRC: ssrc,
			Reports: []rtcp.ReceptionReport{
				{SSRC: 0x55667788, FractionLost: 1, TotalLost: 2},
			},
		}
		raw, err := pkt.Marshal()
		require.NoError(t, err)

		return raw
	}
	decrypted := buildRTCP()

	encCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(kdr))
	require.NoError(t, err)
	decCtx, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, profileCTR,
		KeyDerivationRate(kdr), SRTCPReplayProtection(128),
	)
	require.NoError(t, err)

	// SRTCP indices start at 1. Encrypt enough packets to cross several
	// period boundaries and to evict period 0 from the derivation cache.
	const packetCount = 40
	encrypted := make([][]byte, 0, packetCount)
	for i := 0; i < packetCount; i++ {
		out, err := encCtx.EncryptRTCP(nil, decrypted, nil)
		require.NoError(t, err)
		encrypted = append(encrypted, out)
	}

	// Deliver in order, except the packet with index 2 (period 0) which is
	// delivered last, long after its period has been evicted.
	for i, out := range encrypted {
		if i == 1 { // SRTCP index 2
			continue
		}
		got, err := decCtx.DecryptRTCP(nil, out, nil)
		require.NoError(t, err)
		assert.Equal(t, decrypted, got)
	}

	// Late packet from period 0 must use the keys of period 0.
	got, err := decCtx.DecryptRTCP(nil, encrypted[1], nil)
	require.NoError(t, err)
	assert.Equal(t, decrypted, got)

	// Replayed packet must be rejected.
	_, err = decCtx.DecryptRTCP(nil, encrypted[1], nil)
	assert.ErrorIs(t, err, errDuplicated)
}

// The derivation cache must stay bounded and must release the key material of
// evicted periods.
func TestKeyDerivationRateCacheBounded(t *testing.T) {
	const kdr = 1 << 1 // 2 packets per period
	const ssrc = 0xFEEDFACE

	encCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(kdr))
	require.NoError(t, err)

	cipher, ok := encCtx.cipher.(*srtpCipherAesCmHmacSha1)
	require.True(t, ok)

	period0Keys := cipher.periodKeys[0]
	require.NotNil(t, period0Keys)

	const packetCount = 4 * maxKDRPeriodsCached * kdr
	for seq := uint16(0); seq < packetCount; seq++ {
		encryptKDRTestRTP(t, encCtx, seq, ssrc)
		assert.LessOrEqual(t, len(cipher.periodKeys), maxKDRPeriodsCached, "derivation cache must be bounded")
	}

	// Period 0 must have been evicted and its key material released.
	_, ok = cipher.periodKeys[0]
	assert.False(t, ok, "oldest period must be evicted")
	assert.Nil(t, period0Keys.srtpBlock)
	assert.Nil(t, period0Keys.srtcpBlock)
	assert.Nil(t, period0Keys.srtpSessionAuth)
	assert.Nil(t, period0Keys.srtcpSessionAuth)
	assert.Nil(t, period0Keys.srtpSessionSalt)
	assert.Nil(t, period0Keys.srtcpSessionSalt)
}

// KeyDerivationRate(0) must produce exactly the same output as not setting
// the option at all.
func TestKeyDerivationRateZeroMatchesDefault(t *testing.T) {
	const ssrc = 0xABCDEF01

	defaultCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR)
	require.NoError(t, err)
	zeroCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(0))
	require.NoError(t, err)

	for seq := uint16(0); seq < 64; seq++ {
		assert.Equal(t,
			encryptKDRTestRTP(t, defaultCtx, seq, ssrc),
			encryptKDRTestRTP(t, zeroCtx, seq, ssrc),
		)
	}
}

// A tampered packet from a previous period must still fail authentication.
func TestSRTPKeyDerivationRateTamperedLatePacket(t *testing.T) {
	const kdr = 1 << 4
	const ssrc = 0x0BADC0DE

	encCtx, err := CreateContext(kdrTestMasterKey, kdrTestMasterSalt, profileCTR, KeyDerivationRate(kdr))
	require.NoError(t, err)
	decCtx, err := CreateContext(
		kdrTestMasterKey, kdrTestMasterSalt, profileCTR,
		KeyDerivationRate(kdr), SRTPReplayProtection(256),
	)
	require.NoError(t, err)

	late := encryptKDRTestRTP(t, encCtx, 3, ssrc)
	for seq := uint16(4); seq < 3*kdr; seq++ {
		decryptKDRTestRTP(t, decCtx, encryptKDRTestRTP(t, encCtx, seq, ssrc))
	}

	// Flip a payload bit of the withheld period-0 packet.
	tampered := append([]byte(nil), late...)
	tampered[13] ^= 0xFF

	_, err = decCtx.DecryptRTP(nil, tampered, nil)
	assert.True(t, errors.Is(err, ErrFailedToVerifyAuthTag), "tampered packet must fail authentication, got %v", err)
}
