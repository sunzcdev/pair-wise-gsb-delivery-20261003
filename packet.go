// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// This file implements an offline codec for NTPv3 and NTPv4 wire packets.
// It performs no network I/O; it is intended for packet capture inspection
// and offline testing.

package ntp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"time"
)

var (
	// ErrPacketTruncated is returned when a packet is shorter than the
	// fixed 48-byte NTP header.
	ErrPacketTruncated = errors.New("NTP packet is shorter than the 48-byte header")

	// ErrPacketTooLong is returned when a packet exceeds MaxPacketSize.
	ErrPacketTooLong = errors.New("NTP packet exceeds the maximum packet size")

	// ErrPacketMode is returned when a packet contains a mode that is not
	// a valid NTPv3/NTPv4 time synchronization association mode.
	ErrPacketMode = errors.New("invalid mode in NTP packet")

	// ErrPacketVersion is returned when a packet contains an NTP protocol
	// version other than 3 or 4.
	ErrPacketVersion = errors.New("unsupported version in NTP packet")
)

const (
	// PacketHeaderSize is the size in bytes of the fixed NTPv3/NTPv4
	// packet header.
	PacketHeaderSize = msgSize

	// MaxPacketSize is the maximum accepted size in bytes of an
	// NTPv3/NTPv4 packet, including the fixed header and any trailing
	// data such as extension fields or a MAC.
	MaxPacketSize = 1500
)

// NTPv3/NTPv4 association modes. See RFC 5905 section 3.
const (
	ModeSymmetricActive  = 1
	ModeSymmetricPassive = 2
	ModeClient           = 3
	ModeServer           = 4
	ModeBroadcast        = 5
)

// A Packet is a decoded NTPv3 or NTPv4 wire packet. It contains the fixed
// 48-byte header fields exactly as they appear on the wire, plus any
// trailing bytes (extension fields, MAC, or unrecognized data) preserved
// uninterpreted in the Trailer field.
//
// A Packet is a raw, unverified representation of wire data. Decoding a
// packet does not authenticate it or promote it to a trusted Response; use
// the Verify method for that.
type Packet struct {
	// Leap is the 2-bit leap second indicator.
	Leap LeapIndicator

	// Version is the NTP protocol version. Must be 3 or 4.
	Version int

	// Mode is the NTP association mode (e.g., ModeClient or ModeServer).
	Mode int

	// Stratum is the stratum level of the packet's source.
	Stratum uint8

	// Poll is the maximum interval between successive messages, expressed
	// as a power of two in seconds.
	Poll int8

	// Precision is the precision of the source's clock, expressed as a
	// power of two in seconds.
	Precision int8

	// RootDelay is the estimated round-trip delay to the primary source,
	// in Q16.16 fixed-point seconds.
	RootDelay uint32

	// RootDispersion is the estimated maximum error relative to the
	// primary source, in Q16.16 fixed-point seconds.
	RootDispersion uint32

	// ReferenceID identifies the source's reference clock. When Stratum
	// is 0, this field contains a 4-character ASCII "kiss code".
	ReferenceID uint32

	// ReferenceTime is the time the source's clock was last set or
	// corrected, as a Q32.32 fixed-point NTP timestamp.
	ReferenceTime uint64

	// OriginTime is the time the request departed the client, as copied
	// from the request's TransmitTime, in Q32.32 fixed-point format.
	OriginTime uint64

	// ReceiveTime is the time the request arrived at the server, in
	// Q32.32 fixed-point format.
	ReceiveTime uint64

	// TransmitTime is the time the packet departed its source, in Q32.32
	// fixed-point format.
	TransmitTime uint64

	// Trailer contains any bytes following the fixed 48-byte header,
	// preserved verbatim. This may include extension fields, a symmetric
	// key MAC, or unrecognized data. The codec makes no attempt to
	// interpret these bytes.
	Trailer []byte
}

// DecodePacket parses an NTPv3 or NTPv4 wire packet from a byte slice,
// such as one obtained from a packet capture. The fixed header fields are
// decoded, and any bytes beyond the 48-byte header are preserved verbatim
// in the packet's Trailer field. The returned packet does not share memory
// with the input slice.
//
// DecodePacket returns ErrPacketTruncated if the data is shorter than the
// 48-byte header, ErrPacketTooLong if it exceeds MaxPacketSize,
// ErrPacketVersion if the packet's version is not 3 or 4, and
// ErrPacketMode if the packet's mode is not a valid time synchronization
// association mode.
func DecodePacket(data []byte) (*Packet, error) {
	if len(data) < PacketHeaderSize {
		return nil, ErrPacketTruncated
	}
	if len(data) > MaxPacketSize {
		return nil, ErrPacketTooLong
	}

	m := &messageV4{}
	r := bytes.NewReader(data[:PacketHeaderSize])
	binary.Read(r, binary.BigEndian, &m.LiVnMode)
	binary.Read(r, binary.BigEndian, &m.Stratum)
	binary.Read(r, binary.BigEndian, &m.Poll)
	binary.Read(r, binary.BigEndian, &m.Precision)
	binary.Read(r, binary.BigEndian, &m.RootDelay)
	binary.Read(r, binary.BigEndian, &m.RootDispersion)
	binary.Read(r, binary.BigEndian, &m.ReferenceID)
	binary.Read(r, binary.BigEndian, &m.ReferenceTime)
	binary.Read(r, binary.BigEndian, &m.OriginTime)
	binary.Read(r, binary.BigEndian, &m.ReceiveTime)
	binary.Read(r, binary.BigEndian, &m.TransmitTime)

	p := &Packet{
		Leap:           m.getLeap(),
		Version:        m.getVersion(),
		Mode:           int(m.getMode()),
		Stratum:        m.Stratum,
		Poll:           m.Poll,
		Precision:      m.Precision,
		RootDelay:      uint32(m.RootDelay),
		RootDispersion: uint32(m.RootDispersion),
		ReferenceID:    m.ReferenceID,
		ReferenceTime:  uint64(m.ReferenceTime),
		OriginTime:     uint64(m.OriginTime),
		ReceiveTime:    uint64(m.ReceiveTime),
		TransmitTime:   uint64(m.TransmitTime),
	}
	if err := p.validate(); err != nil {
		return nil, err
	}

	if len(data) > PacketHeaderSize {
		p.Trailer = append([]byte(nil), data[PacketHeaderSize:]...)
	}

	return p, nil
}

// Encode serializes the packet into its wire representation: the fixed
// 48-byte header followed by the raw Trailer bytes. Encoding a packet and
// decoding the result preserves all wire fields.
func (p *Packet) Encode() ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	m := p.message()
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, m.LiVnMode)
	binary.Write(buf, binary.BigEndian, m.Stratum)
	binary.Write(buf, binary.BigEndian, m.Poll)
	binary.Write(buf, binary.BigEndian, m.Precision)
	binary.Write(buf, binary.BigEndian, m.RootDelay)
	binary.Write(buf, binary.BigEndian, m.RootDispersion)
	binary.Write(buf, binary.BigEndian, m.ReferenceID)
	binary.Write(buf, binary.BigEndian, m.ReferenceTime)
	binary.Write(buf, binary.BigEndian, m.OriginTime)
	binary.Write(buf, binary.BigEndian, m.ReceiveTime)
	binary.Write(buf, binary.BigEndian, m.TransmitTime)
	buf.Write(p.Trailer)

	return buf.Bytes(), nil
}

// validate checks that the packet's version, mode, and trailer length are
// acceptable for an NTPv3/NTPv4 wire packet.
func (p *Packet) validate() error {
	if p.Version != 3 && p.Version != 4 {
		return ErrPacketVersion
	}
	if p.Mode < ModeSymmetricActive || p.Mode > ModeBroadcast {
		return ErrPacketMode
	}
	if len(p.Trailer) > MaxPacketSize-PacketHeaderSize {
		return ErrPacketTooLong
	}
	return nil
}

// message converts the packet into its internal message representation.
func (p *Packet) message() *messageV4 {
	m := &messageV4{
		Stratum:        p.Stratum,
		Poll:           p.Poll,
		Precision:      p.Precision,
		RootDelay:      timeShortV4(p.RootDelay),
		RootDispersion: timeShortV4(p.RootDispersion),
		ReferenceID:    p.ReferenceID,
		ReferenceTime:  timestamp(p.ReferenceTime),
		OriginTime:     timestamp(p.OriginTime),
		ReceiveTime:    timestamp(p.ReceiveTime),
		TransmitTime:   timestamp(p.TransmitTime),
	}
	m.setLeap(p.Leap)
	m.setVersion(p.Version)
	m.setMode(modeV4(p.Mode))
	return m
}

// VerifyOptions contains the caller-supplied values needed to verify an
// NTPv3/NTPv4 response packet against the query that elicited it.
type VerifyOptions struct {
	// OriginTime is the association value the client placed in the
	// request's TransmitTime field. The response packet's OriginTime must
	// match this value, protecting against spoofed responses.
	OriginTime uint64

	// ClientXmit is the local time at which the request was transmitted.
	ClientXmit time.Time

	// ClientRecv is the local time at which the response was received.
	ClientRecv time.Time

	// Auth contains the symmetric key authentication parameters used to
	// verify the packet's MAC. The default (AuthNone) skips MAC
	// verification.
	Auth AuthOptions
}

// Verify checks the packet as a response to a previously issued query and,
// only if all checks pass, promotes it to a Response. The caller must
// supply the original request's association value, the local transmit and
// receive timestamps, and (optionally) symmetric key authentication
// parameters. If authentication is requested and fails, no Response is
// returned.
func (p *Packet) Verify(opt VerifyOptions) (*Response, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if p.Mode != ModeServer {
		return nil, ErrInvalidMode
	}
	if p.TransmitTime == 0 {
		return nil, ErrInvalidTransmitTime
	}
	if p.ReceiveTime > p.TransmitTime {
		return nil, ErrServerTickedBackwards
	}
	if p.OriginTime != opt.OriginTime {
		return nil, ErrServerResponseMismatch
	}

	// If symmetric authentication was requested, verify the MAC over the
	// packet's wire encoding. On failure, no verified result is returned.
	if opt.Auth.Type != AuthNone {
		key, err := decodeAuthKey(opt.Auth)
		if err != nil {
			return nil, err
		}
		raw, err := p.Encode()
		if err != nil {
			return nil, err
		}
		if err := verifyMAC(raw, p.Version, opt.Auth, key); err != nil {
			return nil, err
		}
	}

	return newV4Response(p.message(), opt.ClientXmit, opt.ClientRecv), nil
}
