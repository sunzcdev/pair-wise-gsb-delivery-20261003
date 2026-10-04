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

const testOriginTimestamp = 0x1122334455667788

func testPacketTimestamps() ProtocolTimestamps {
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	return ProtocolTimestamps{
		ClientXmit: base,
		ServerRecv: base.Add(50 * time.Millisecond),
		ServerXmit: base.Add(60 * time.Millisecond),
		ClientRecv: base.Add(20 * time.Millisecond),
	}
}

func testServerPacket() *Packet {
	ts := testPacketTimestamps()
	return &Packet{
		Leap:           LeapNoWarning,
		Version:        4,
		Mode:           ModeServer,
		Stratum:        2,
		Poll:           6,
		Precision:      -20,
		RootDelay:      0x00008000,
		RootDispersion: 0x00010000,
		ReferenceID:    0x7f7f0101,
		ReferenceTime:  uint64(toTimestamp(ts.ClientXmit.Add(-time.Hour))),
		OriginTime:     testOriginTimestamp,
		ReceiveTime:    uint64(toTimestamp(ts.ServerRecv)),
		TransmitTime:   uint64(toTimestamp(ts.ServerXmit)),
	}
}

func testVerifyOptions() *VerifyOptions {
	return &VerifyOptions{
		OriginTimestamp: testOriginTimestamp,
		Timestamps:      testPacketTimestamps(),
	}
}

func TestPacketRoundTrip(t *testing.T) {
	p := testServerPacket()
	p.Leap = LeapAddSecond
	p.Trailer = []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03}

	wire, err := p.Encode()
	assert.Nil(t, err)
	assert.Equal(t, msgSize+len(p.Trailer), len(wire))

	// Check the first byte's bit fields.
	assert.Equal(t, byte(0x64), wire[0]) // LI=1, VN=4, Mode=4

	q, err := DecodePacket(wire)
	assert.Nil(t, err)
	assert.Equal(t, *p, *q)

	// Encoding the decoded packet reproduces the original wire bytes.
	wire2, err := q.Encode()
	assert.Nil(t, err)
	assert.Equal(t, wire, wire2)
}

func TestPacketRoundTripV3(t *testing.T) {
	p := testServerPacket()
	p.Version = 3

	wire, err := p.Encode()
	assert.Nil(t, err)
	assert.Equal(t, byte(0x1c), wire[0]) // LI=0, VN=3, Mode=4

	q, err := DecodePacket(wire)
	assert.Nil(t, err)
	assert.Equal(t, *p, *q)
}

func TestDecodePacketTruncated(t *testing.T) {
	wire, err := testServerPacket().Encode()
	assert.Nil(t, err)

	for _, n := range []int{0, 1, 47} {
		p, err := DecodePacket(wire[:n])
		assert.Nil(t, p)
		assert.Equal(t, ErrTruncatedPacket, err)
	}
}

func TestDecodePacketTooLong(t *testing.T) {
	wire, err := testServerPacket().Encode()
	assert.Nil(t, err)

	tooLong := append(wire, make([]byte, maxPacketSize-len(wire)+1)...)
	p, err := DecodePacket(tooLong)
	assert.Nil(t, p)
	assert.Equal(t, ErrPacketTooLong, err)
}

func TestDecodePacketInvalidVersion(t *testing.T) {
	wire, err := testServerPacket().Encode()
	assert.Nil(t, err)

	for _, v := range []byte{0, 1, 2, 5, 6, 7} {
		wire[0] = (wire[0] & 0xc7) | v<<3
		p, err := DecodePacket(wire)
		assert.Nil(t, p)
		assert.Equal(t, ErrInvalidProtocolVersion, err)
	}
}

func TestDecodePacketInvalidMode(t *testing.T) {
	wire, err := testServerPacket().Encode()
	assert.Nil(t, err)

	wire[0] &^= 0x07 // mode = ModeReserved
	p, err := DecodePacket(wire)
	assert.Nil(t, p)
	assert.Equal(t, ErrInvalidMode, err)
}

func TestDecodePacketTrailerPreserved(t *testing.T) {
	wire, err := testServerPacket().Encode()
	assert.Nil(t, err)

	// Append unknown trailing data; the codec must preserve it verbatim
	// without interpreting it as an extension field.
	trailer := []byte{0xf5, 0x01, 0x00, 0x04, 0xaa, 0xbb, 0xcc}
	wire = append(wire, trailer...)

	p, err := DecodePacket(wire)
	assert.Nil(t, err)
	assert.Equal(t, trailer, p.Trailer)

	// The trailer must not alias the input buffer.
	wire[msgSize] = 0x00
	assert.Equal(t, byte(0xf5), p.Trailer[0])
}

func TestEncodePacketInvalidVersion(t *testing.T) {
	p := testServerPacket()
	p.Version = 5
	wire, err := p.Encode()
	assert.Nil(t, wire)
	assert.Equal(t, ErrInvalidProtocolVersion, err)
}

func TestEncodePacketInvalidMode(t *testing.T) {
	p := testServerPacket()
	p.Mode = ModeReserved
	wire, err := p.Encode()
	assert.Nil(t, wire)
	assert.Equal(t, ErrInvalidMode, err)

	p.Mode = ModePrivate + 1
	wire, err = p.Encode()
	assert.Nil(t, wire)
	assert.Equal(t, ErrInvalidMode, err)
}

func TestEncodePacketTrailerTooLong(t *testing.T) {
	p := testServerPacket()
	p.Trailer = make([]byte, maxPacketSize-msgSize+1)
	wire, err := p.Encode()
	assert.Nil(t, wire)
	assert.Equal(t, ErrPacketTooLong, err)
}

func TestPacketTimestamps(t *testing.T) {
	ts := testPacketTimestamps()
	p := testServerPacket()
	assert.Equal(t, ts.ServerRecv, p.ReceiveTimestamp())
	assert.Equal(t, ts.ServerXmit, p.TransmitTimestamp())
	assert.Equal(t, ts.ClientXmit.Add(-time.Hour), p.ReferenceTimestamp())
}

func TestPacketVerify(t *testing.T) {
	p := testServerPacket()
	r, err := p.Verify(testVerifyOptions())
	assert.Nil(t, err)
	assert.NotNil(t, r)

	assert.Equal(t, 45*time.Millisecond, r.ClockOffset)
	assert.Equal(t, 10*time.Millisecond, r.RTT)
	assert.Equal(t, 4, r.Version)
	assert.Equal(t, uint8(2), r.Stratum)
	assert.Equal(t, uint32(0x7f7f0101), r.ReferenceID)
	assert.Equal(t, LeapNoWarning, r.Leap)
	assert.NotEqual(t, ResponseFlags(0), r.Flags&FlagSynchronized)
	assert.Equal(t, testPacketTimestamps(), r.Timestamps)
	assert.Nil(t, r.Validate())
}

func TestPacketVerifyErrors(t *testing.T) {
	cases := []struct {
		name   string
		modify func(p *Packet)
		err    error
	}{
		{
			name:   "client mode",
			modify: func(p *Packet) { p.Mode = ModeClient },
			err:    ErrInvalidMode,
		},
		{
			name:   "broadcast mode",
			modify: func(p *Packet) { p.Mode = ModeBroadcast },
			err:    ErrInvalidMode,
		},
		{
			name:   "origin mismatch",
			modify: func(p *Packet) { p.OriginTime++ },
			err:    ErrServerResponseMismatch,
		},
		{
			name:   "zero transmit time",
			modify: func(p *Packet) { p.TransmitTime = 0 },
			err:    ErrInvalidTransmitTime,
		},
		{
			name: "receive after transmit",
			modify: func(p *Packet) {
				p.ReceiveTime = p.TransmitTime + 1
			},
			err: ErrServerTickedBackwards,
		},
	}

	for _, c := range cases {
		p := testServerPacket()
		c.modify(p)
		r, err := p.Verify(testVerifyOptions())
		assert.Nil(t, r, c.name)
		assert.Equal(t, c.err, err, c.name)
	}
}

func TestPacketVerifyAuth(t *testing.T) {
	auth := AuthOptions{
		Type:  AuthMD5,
		Key:   "ASCII:cvuZyN4C8HX8hNcAWDWp",
		KeyID: 1,
	}
	key, err := decodeAuthKey(auth)
	assert.Nil(t, err)

	// Build an authenticated server packet by appending a key ID and MAC.
	p := testServerPacket()
	header, err := p.Encode()
	assert.Nil(t, err)
	digest := calcMAC(p.Version, auth.Type, key, header)
	trailer := make([]byte, 4, 4+len(digest))
	binary.BigEndian.PutUint32(trailer, auth.KeyID)
	p.Trailer = append(trailer, digest...)

	// Route the packet through the wire codec before verifying.
	wire, err := p.Encode()
	assert.Nil(t, err)
	decoded, err := DecodePacket(wire)
	assert.Nil(t, err)

	opt := testVerifyOptions()
	opt.Auth = auth
	r, err := decoded.Verify(opt)
	assert.Nil(t, err)
	assert.NotNil(t, r)
	assert.Nil(t, r.Validate())
}

func TestPacketVerifyAuthFailure(t *testing.T) {
	auth := AuthOptions{
		Type:  AuthMD5,
		Key:   "ASCII:cvuZyN4C8HX8hNcAWDWp",
		KeyID: 1,
	}
	key, err := decodeAuthKey(auth)
	assert.Nil(t, err)

	p := testServerPacket()
	header, err := p.Encode()
	assert.Nil(t, err)
	digest := calcMAC(p.Version, auth.Type, key, header)
	trailer := make([]byte, 4, 4+len(digest))
	binary.BigEndian.PutUint32(trailer, auth.KeyID)
	p.Trailer = append(trailer, digest...)

	cases := []struct {
		name string
		auth AuthOptions
		err  error
	}{
		{
			name: "wrong key",
			auth: AuthOptions{Type: AuthMD5, Key: "ASCII:XXXXXXXXXXXXXXXX", KeyID: 1},
			err:  ErrAuthFailed,
		},
		{
			name: "wrong key ID",
			auth: AuthOptions{Type: AuthMD5, Key: auth.Key, KeyID: 2},
			err:  ErrAuthFailed,
		},
		{
			name: "invalid key",
			auth: AuthOptions{Type: AuthMD5, Key: "", KeyID: 1},
			err:  ErrInvalidAuthKey,
		},
	}

	for _, c := range cases {
		opt := testVerifyOptions()
		opt.Auth = c.auth
		r, err := p.Verify(opt)
		assert.Nil(t, r, c.name) // no verified result on auth failure
		assert.Equal(t, c.err, err, c.name)
	}
}

func TestPacketVerifyAuthNAK(t *testing.T) {
	// A 4-byte zero trailer on an NTPv4 packet is a crypto-NAK.
	p := testServerPacket()
	p.Trailer = []byte{0, 0, 0, 0}

	opt := testVerifyOptions()
	opt.Auth = AuthOptions{
		Type:  AuthMD5,
		Key:   "ASCII:cvuZyN4C8HX8hNcAWDWp",
		KeyID: 1,
	}
	r, err := p.Verify(opt)
	assert.Nil(t, r)
	assert.Equal(t, ErrAuthNAK, err)
}
