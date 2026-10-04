// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func testPacket() *Packet {
	return &Packet{
		Leap:           LeapAddSecond,
		Version:        4,
		Mode:           ModeServer,
		Stratum:        2,
		Poll:           6,
		Precision:      -20,
		RootDelay:      0x00008000,
		RootDispersion: 0x00010000,
		ReferenceID:    0x7f7f0101,
		ReferenceTime:  0xecaaaaaa00000000,
		OriginTime:     0x1122334455667788,
		ReceiveTime:    0xecc0000000000000,
		TransmitTime:   0xecc0000080000000,
		Trailer:        []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02},
	}
}

func TestPacketRoundTrip(t *testing.T) {
	p := testPacket()

	raw, err := p.Encode()
	assert.NoError(t, err)
	assert.Equal(t, PacketHeaderSize+len(p.Trailer), len(raw))

	q, err := DecodePacket(raw)
	assert.NoError(t, err)
	assert.Equal(t, p, q)

	// Encoding the decoded packet reproduces the original wire bytes.
	raw2, err := q.Encode()
	assert.NoError(t, err)
	assert.Equal(t, raw, raw2)
}

func TestPacketRoundTripNoTrailer(t *testing.T) {
	p := testPacket()
	p.Trailer = nil

	raw, err := p.Encode()
	assert.NoError(t, err)
	assert.Equal(t, PacketHeaderSize, len(raw))

	q, err := DecodePacket(raw)
	assert.NoError(t, err)
	assert.Equal(t, p, q)
	assert.Nil(t, q.Trailer)
}

func TestPacketDecodeTruncated(t *testing.T) {
	for _, n := range []int{0, 1, PacketHeaderSize - 1} {
		p, err := DecodePacket(make([]byte, n))
		assert.Nil(t, p)
		assert.Equal(t, ErrPacketTruncated, err)
	}
}

func TestPacketDecodeTooLong(t *testing.T) {
	p, err := DecodePacket(make([]byte, MaxPacketSize+1))
	assert.Nil(t, p)
	assert.Equal(t, ErrPacketTooLong, err)
}

func TestPacketDecodeInvalidMode(t *testing.T) {
	for _, mode := range []int{0, 6, 7} {
		raw := make([]byte, PacketHeaderSize)
		raw[0] = byte(4<<3) | byte(mode) // version 4
		p, err := DecodePacket(raw)
		assert.Nil(t, p)
		assert.Equal(t, ErrPacketMode, err)
	}
}

func TestPacketDecodeInvalidVersion(t *testing.T) {
	for _, version := range []int{0, 1, 2, 5, 6, 7} {
		raw := make([]byte, PacketHeaderSize)
		raw[0] = byte(version<<3) | byte(ModeServer)
		p, err := DecodePacket(raw)
		assert.Nil(t, p)
		assert.Equal(t, ErrPacketVersion, err)
	}
}

func TestPacketEncodeInvalid(t *testing.T) {
	p := testPacket()
	p.Version = 5
	raw, err := p.Encode()
	assert.Nil(t, raw)
	assert.Equal(t, ErrPacketVersion, err)

	p = testPacket()
	p.Mode = 7
	raw, err = p.Encode()
	assert.Nil(t, raw)
	assert.Equal(t, ErrPacketMode, err)

	p = testPacket()
	p.Trailer = make([]byte, MaxPacketSize-PacketHeaderSize+1)
	raw, err = p.Encode()
	assert.Nil(t, raw)
	assert.Equal(t, ErrPacketTooLong, err)
}

func TestPacketDecodeCopiesTrailer(t *testing.T) {
	p := testPacket()
	raw, err := p.Encode()
	assert.NoError(t, err)

	q, err := DecodePacket(raw)
	assert.NoError(t, err)

	// Mutating the input buffer must not affect the decoded packet.
	for i := range raw {
		raw[i] = 0
	}
	assert.Equal(t, p.Trailer, q.Trailer)
}

func testVerifyPacket(origin uint64, serverRecv, serverXmit time.Time) *Packet {
	return &Packet{
		Version:      4,
		Mode:         ModeServer,
		Stratum:      2,
		Poll:         6,
		Precision:    -20,
		OriginTime:   origin,
		ReceiveTime:  uint64(toTimestamp(serverRecv)),
		TransmitTime: uint64(toTimestamp(serverXmit)),
	}
}

func TestPacketVerify(t *testing.T) {
	origin := uint64(0x1122334455667788)
	t1 := time.Date(2024, 5, 30, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(50 * time.Millisecond)
	t3 := t2.Add(10 * time.Millisecond)
	t4 := t1.Add(100 * time.Millisecond)

	p := testVerifyPacket(origin, t2, t3)
	r, err := p.Verify(VerifyOptions{
		OriginTime: origin,
		ClientXmit: t1,
		ClientRecv: t4,
	})
	assert.NoError(t, err)
	assert.NotNil(t, r)
	assert.Equal(t, 4, r.Version)
	assert.Equal(t, uint8(2), r.Stratum)
	assert.Equal(t, offset(t1, t2, t3, t4), r.ClockOffset)
	assert.Equal(t, rtt(t1, t2, t3, t4), r.RTT)
	assert.Equal(t, FlagSynchronized, r.Flags&FlagSynchronized)
}

func TestPacketVerifyOriginMismatch(t *testing.T) {
	t1 := time.Date(2024, 5, 30, 12, 0, 0, 0, time.UTC)
	p := testVerifyPacket(0x1122334455667788, t1, t1)

	r, err := p.Verify(VerifyOptions{
		OriginTime: 0x8877665544332211,
		ClientXmit: t1,
		ClientRecv: t1,
	})
	assert.Nil(t, r)
	assert.Equal(t, ErrServerResponseMismatch, err)
}

func TestPacketVerifyInvalidMode(t *testing.T) {
	t1 := time.Date(2024, 5, 30, 12, 0, 0, 0, time.UTC)
	p := testVerifyPacket(0x1122334455667788, t1, t1)
	p.Mode = ModeClient

	r, err := p.Verify(VerifyOptions{
		OriginTime: 0x1122334455667788,
		ClientXmit: t1,
		ClientRecv: t1,
	})
	assert.Nil(t, r)
	assert.Equal(t, ErrInvalidMode, err)
}

func TestPacketVerifyAuth(t *testing.T) {
	origin := uint64(0x1122334455667788)
	t1 := time.Date(2024, 5, 30, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(50 * time.Millisecond)
	t3 := t2.Add(10 * time.Millisecond)
	t4 := t1.Add(100 * time.Millisecond)

	auth := AuthOptions{Type: AuthMD5, Key: "ASCII:secretkey", KeyID: 42}
	key, err := decodeAuthKey(auth)
	assert.NoError(t, err)

	// Append a valid MAC to the packet's trailer.
	p := testVerifyPacket(origin, t2, t3)
	raw, err := p.Encode()
	assert.NoError(t, err)
	trailer := make([]byte, 4)
	binary.BigEndian.PutUint32(trailer, auth.KeyID)
	trailer = append(trailer, calcMAC(4, auth.Type, key, raw)...)
	p.Trailer = trailer

	r, err := p.Verify(VerifyOptions{
		OriginTime: origin,
		ClientXmit: t1,
		ClientRecv: t4,
		Auth:       auth,
	})
	assert.NoError(t, err)
	assert.NotNil(t, r)
	assert.Equal(t, offset(t1, t2, t3, t4), r.ClockOffset)
}

func TestPacketVerifyAuthFailure(t *testing.T) {
	origin := uint64(0x1122334455667788)
	t1 := time.Date(2024, 5, 30, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(50 * time.Millisecond)
	t3 := t2.Add(10 * time.Millisecond)
	t4 := t1.Add(100 * time.Millisecond)

	auth := AuthOptions{Type: AuthMD5, Key: "ASCII:secretkey", KeyID: 42}
	key, err := decodeAuthKey(auth)
	assert.NoError(t, err)

	p := testVerifyPacket(origin, t2, t3)
	raw, err := p.Encode()
	assert.NoError(t, err)
	trailer := make([]byte, 4)
	binary.BigEndian.PutUint32(trailer, auth.KeyID)
	trailer = append(trailer, calcMAC(4, auth.Type, key, raw)...)
	p.Trailer = trailer

	// Verifying with the wrong key must fail and return no response.
	badAuth := AuthOptions{Type: AuthMD5, Key: "ASCII:wrongkey", KeyID: 42}
	r, err := p.Verify(VerifyOptions{
		OriginTime: origin,
		ClientXmit: t1,
		ClientRecv: t4,
		Auth:       badAuth,
	})
	assert.Nil(t, r)
	assert.Equal(t, ErrAuthFailed, err)

	// Verifying with the wrong key ID must fail and return no response.
	badKeyID := AuthOptions{Type: AuthMD5, Key: "ASCII:secretkey", KeyID: 43}
	r, err = p.Verify(VerifyOptions{
		OriginTime: origin,
		ClientXmit: t1,
		ClientRecv: t4,
		Auth:       badKeyID,
	})
	assert.Nil(t, r)
	assert.Equal(t, ErrAuthFailed, err)
}
