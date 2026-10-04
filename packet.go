// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// This file implements a pure wire-format codec for NTPv3 and NTPv4
// packets. The codec performs no network I/O and never promotes a decoded
// packet to a trusted Response on its own; it is intended for packet
// capture inspection and offline testing. To validate a decoded packet as
// a server response, the caller must separately supply the original
// request's association value, the four protocol timestamps, and any
// authentication parameters. See Packet.Verify for details.

package ntp

import (
	"bytes"
	"encoding/binary"
	"time"
)

// maxPacketSize is the maximum accepted size of an NTPv3/NTPv4 packet on
// the wire, including its 48-byte header and any trailing bytes. It matches
// the size of the buffer used to read packets from the network.
const maxPacketSize = msgBufSize

// Mode is the association mode field of an NTPv3 or NTPv4 packet. See RFC
// 5905 section 7.3 for details.
type Mode uint8

const (
	// ModeReserved is a reserved mode value. It is never valid on the wire.
	ModeReserved Mode = 0

	// ModeSymmetricActive indicates a symmetric active association.
	ModeSymmetricActive Mode = 1

	// ModeSymmetricPassive indicates a symmetric passive association.
	ModeSymmetricPassive Mode = 2

	// ModeClient indicates a client association.
	ModeClient Mode = 3

	// ModeServer indicates a server association.
	ModeServer Mode = 4

	// ModeBroadcast indicates a broadcast association.
	ModeBroadcast Mode = 5

	// ModeControl indicates an NTP control message.
	ModeControl Mode = 6

	// ModePrivate indicates a mode reserved for private use.
	ModePrivate Mode = 7
)

// A Packet is a decoded NTPv3 or NTPv4 wire-format message. It preserves
// the fixed 48-byte header fields exactly as they appear on the wire, along
// with any trailing bytes (extension fields, MAC, or padding) in uninterpreted
// form. Unknown trailer content is retained as raw data; the codec never
// guesses at unsupported extension field types.
//
// All multi-byte integer fields use network (big-endian) byte order on the
// wire. RootDelay and RootDispersion are raw Q16.16 fixed-point values, and
// the four timestamp fields are raw Q32.32 fixed-point values, as defined by
// RFC 5905. Keeping these values in raw form guarantees that encoding a
// decoded packet reproduces the original wire bytes exactly.
type Packet struct {
	// Leap is the leap second indicator.
	Leap LeapIndicator

	// Version is the NTP protocol version number. Only 3 and 4 are
	// supported by this codec.
	Version int

	// Mode is the association mode.
	Mode Mode

	// Stratum is the stratum level of the responding server.
	Stratum uint8

	// Poll is the poll interval exponent, in log2 seconds.
	Poll int8

	// Precision is the clock precision exponent, in log2 seconds.
	Precision int8

	// RootDelay is the raw Q16.16 fixed-point root delay value.
	RootDelay uint32

	// RootDispersion is the raw Q16.16 fixed-point root dispersion value.
	RootDispersion uint32

	// ReferenceID is the 32-bit reference identifier. When Stratum is 0,
	// it contains the ASCII-encoded "kiss code".
	ReferenceID uint32

	// ReferenceTime is the raw Q32.32 fixed-point timestamp recording when
	// the server's clock was last updated.
	ReferenceTime uint64

	// OriginTime is the raw Q32.32 fixed-point timestamp copied from the
	// request's TransmitTime field. Clients use it to match responses to
	// requests.
	OriginTime uint64

	// ReceiveTime is the raw Q32.32 fixed-point timestamp recording when
	// the server received the request.
	ReceiveTime uint64

	// TransmitTime is the raw Q32.32 fixed-point timestamp recording when
	// the server transmitted the response.
	TransmitTime uint64

	// Trailer contains all bytes following the 48-byte header, preserved
	// verbatim. It may hold extension fields, a symmetric-key MAC, or
	// padding. The codec does not interpret its contents.
	Trailer []byte
}

// DecodePacket decodes an NTPv3 or NTPv4 wire-format message into a Packet.
// It returns ErrTruncatedPacket if the data is shorter than the 48-byte
// header, ErrPacketTooLong if the data exceeds the maximum packet size,
// ErrInvalidProtocolVersion if the packet's version field is not 3 or 4, and
// ErrInvalidMode if the packet's mode field is reserved. The returned
// packet's Trailer is a copy of the trailing bytes and does not alias the
// input slice.
func DecodePacket(data []byte) (*Packet, error) {
	if len(data) < msgSize {
		return nil, ErrTruncatedPacket
	}
	if len(data) > maxPacketSize {
		return nil, ErrPacketTooLong
	}

	p := &Packet{
		Leap:    LeapIndicator((data[0] >> 6) & 0x03),
		Version: int((data[0] >> 3) & 0x07),
		Mode:    Mode(data[0] & 0x07),
	}
	if p.Version != 3 && p.Version != 4 {
		return nil, ErrInvalidProtocolVersion
	}
	if p.Mode == ModeReserved {
		return nil, ErrInvalidMode
	}

	r := bytes.NewReader(data[1:msgSize])
	binary.Read(r, binary.BigEndian, &p.Stratum)
	binary.Read(r, binary.BigEndian, &p.Poll)
	binary.Read(r, binary.BigEndian, &p.Precision)
	binary.Read(r, binary.BigEndian, &p.RootDelay)
	binary.Read(r, binary.BigEndian, &p.RootDispersion)
	binary.Read(r, binary.BigEndian, &p.ReferenceID)
	binary.Read(r, binary.BigEndian, &p.ReferenceTime)
	binary.Read(r, binary.BigEndian, &p.OriginTime)
	binary.Read(r, binary.BigEndian, &p.ReceiveTime)
	binary.Read(r, binary.BigEndian, &p.TransmitTime)

	if len(data) > msgSize {
		p.Trailer = append([]byte(nil), data[msgSize:]...)
	}

	return p, nil
}

// Encode serializes the packet into its NTPv3/NTPv4 wire-format
// representation. It returns ErrInvalidProtocolVersion if the packet's
// Version field is not 3 or 4, ErrInvalidMode if the Mode field is not a
// valid 3-bit mode value, and ErrPacketTooLong if the encoded packet would
// exceed the maximum packet size. Encoding a packet produced by DecodePacket
// reproduces the original wire bytes exactly.
func (p *Packet) Encode() ([]byte, error) {
	if p.Version != 3 && p.Version != 4 {
		return nil, ErrInvalidProtocolVersion
	}
	if p.Mode == ModeReserved || p.Mode > ModePrivate {
		return nil, ErrInvalidMode
	}
	if msgSize+len(p.Trailer) > maxPacketSize {
		return nil, ErrPacketTooLong
	}

	buf := make([]byte, 0, msgSize+len(p.Trailer))
	liVnMode := uint8(p.Leap&0x03)<<6 | uint8(p.Version&0x07)<<3 | uint8(p.Mode)
	buf = append(buf, liVnMode, p.Stratum, byte(p.Poll), byte(p.Precision))
	buf = binary.BigEndian.AppendUint32(buf, p.RootDelay)
	buf = binary.BigEndian.AppendUint32(buf, p.RootDispersion)
	buf = binary.BigEndian.AppendUint32(buf, p.ReferenceID)
	buf = binary.BigEndian.AppendUint64(buf, p.ReferenceTime)
	buf = binary.BigEndian.AppendUint64(buf, p.OriginTime)
	buf = binary.BigEndian.AppendUint64(buf, p.ReceiveTime)
	buf = binary.BigEndian.AppendUint64(buf, p.TransmitTime)
	return append(buf, p.Trailer...), nil
}

// ReferenceTimestamp interprets the packet's ReferenceTime field as an
// absolute time.
func (p *Packet) ReferenceTimestamp() time.Time {
	return timestamp(p.ReferenceTime).TimeV4()
}

// ReceiveTimestamp interprets the packet's ReceiveTime field as an absolute
// time.
func (p *Packet) ReceiveTimestamp() time.Time {
	return timestamp(p.ReceiveTime).TimeV4()
}

// TransmitTimestamp interprets the packet's TransmitTime field as an
// absolute time.
func (p *Packet) TransmitTimestamp() time.Time {
	return timestamp(p.TransmitTime).TimeV4()
}

// VerifyOptions contains the caller-supplied evidence required to verify a
// decoded packet as the response to a previously issued query. Because the
// packet codec performs no network I/O, none of this information is known to
// the packet itself; it must be provided by the caller.
type VerifyOptions struct {
	// OriginTimestamp is the 64-bit association value the client placed in
	// the TransmitTime field of its request. The packet's OriginTime field
	// must match this value, protecting against spoofed or mismatched
	// responses.
	OriginTimestamp uint64

	// Timestamps contains the four protocol timestamps used to calculate
	// the clock offset and round-trip delay. ClientXmit and ClientRecv are
	// recorded by the caller when the request is sent and the response is
	// received; ServerRecv and ServerXmit are normally taken from the
	// packet's ReceiveTimestamp and TransmitTimestamp methods.
	Timestamps ProtocolTimestamps

	// Auth contains the symmetric key authentication parameters used to
	// verify the packet's trailing MAC. If Auth.Type is AuthNone, no
	// authentication is performed.
	Auth AuthOptions
}

// Verify checks the packet as a candidate response to a previously issued
// query and, if all checks pass, returns a Response built from the packet
// and the caller-supplied verification options. It returns an error and a
// nil Response if any check fails; in particular, a failed authentication
// check never yields a verified result.
//
// The following checks are performed, in order:
//   - The packet's mode must be ModeServer (ErrInvalidMode).
//   - The packet's OriginTime must match VerifyOptions.OriginTimestamp
//     (ErrServerResponseMismatch).
//   - The packet's TransmitTime must be nonzero (ErrInvalidTransmitTime),
//     and its ReceiveTime must not exceed its TransmitTime
//     (ErrServerTickedBackwards).
//   - If authentication is configured, the packet's trailing MAC must
//     verify against the supplied key and key ID (ErrAuthFailed or
//     ErrAuthNAK).
//
// The returned Response is not otherwise validated; call its Validate
// method to determine whether it is suitable for time synchronization.
func (p *Packet) Verify(opt *VerifyOptions) (*Response, error) {
	if opt == nil {
		opt = &VerifyOptions{}
	}

	// The packet must be a server-mode response.
	if p.Mode != ModeServer {
		return nil, ErrInvalidMode
	}

	// The packet's OriginTime must match the association value sent in
	// the original request's TransmitTime field.
	if p.OriginTime != opt.OriginTimestamp {
		return nil, ErrServerResponseMismatch
	}

	// Check for invalid timestamp fields.
	if p.TransmitTime == 0 {
		return nil, ErrInvalidTransmitTime
	}
	if p.ReceiveTime > p.TransmitTime {
		return nil, ErrServerTickedBackwards
	}

	// If symmetric key authentication was requested, verify the packet's
	// trailing MAC against its re-encoded wire bytes.
	if opt.Auth.Type != AuthNone {
		authKey, err := decodeAuthKey(opt.Auth)
		if err != nil {
			return nil, err
		}
		wire, err := p.Encode()
		if err != nil {
			return nil, err
		}
		qopt := &QueryOptions{Version: p.Version, Auth: opt.Auth}
		if err := verifyMAC(wire, qopt, authKey); err != nil {
			return nil, err
		}
	}

	// Timestamp aliases for readability.
	t1 := opt.Timestamps.ClientXmit
	t2 := opt.Timestamps.ServerRecv
	t3 := opt.Timestamps.ServerXmit
	t4 := opt.Timestamps.ClientRecv

	// Compose the response struct.
	r := &Response{
		ClockOffset:    offset(t1, t2, t3, t4),
		RTT:            rtt(t1, t2, t3, t4),
		Timestamps:     opt.Timestamps,
		Precision:      toInterval(p.Precision),
		Version:        p.Version,
		Stratum:        p.Stratum,
		Era:            inferEra(timestamp(p.TransmitTime)),
		Timescale:      TimescaleUTC,
		ReferenceID:    p.ReferenceID,
		ReferenceTime:  p.ReferenceTimestamp(),
		RootDelay:      timeShortV4(p.RootDelay).Duration(),
		RootDispersion: timeShortV4(p.RootDispersion).Duration(),
		Leap:           p.Leap,
		MinError:       minError(t1, t2, t3, t4),
		Poll:           toInterval(p.Poll),
		Time:           t3,
	}

	// Calculate root distance.
	r.RootDistance = rootDistance(r.RTT, r.RootDelay, r.RootDispersion)

	// If a kiss of death was received, interpret the reference ID as
	// a kiss code.
	if r.Stratum == 0 {
		r.KissCode = kissCode(r.ReferenceID)
	}

	// Responses with valid stratum values are considered synchronized.
	if r.Stratum > 1 && r.Stratum < maxStratum {
		r.Flags |= FlagSynchronized
	}

	return r, nil
}
