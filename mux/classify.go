// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package mux

import "encoding/binary"

// Protocol identifies a protocol demultiplexed from the shared UDP socket.
type Protocol int

const (
	// ProtocolUnknown is assigned to datagrams that fail RFC 7983
	// classification or the per-protocol header checks.
	ProtocolUnknown Protocol = iota
	// ProtocolSTUN identifies STUN messages (RFC 5389).
	ProtocolSTUN
	// ProtocolDTLS identifies DTLS records (RFC 9147).
	ProtocolDTLS
	// ProtocolTURN identifies TURN ChannelData messages (RFC 8656).
	ProtocolTURN
	// ProtocolRTP identifies RTP packets (RFC 3550).
	ProtocolRTP
	// ProtocolRTCP identifies RTCP packets (RFC 3550).
	ProtocolRTCP
)

// String returns the human-readable name of the protocol.
func (p Protocol) String() string {
	switch p {
	case ProtocolSTUN:
		return "STUN"
	case ProtocolDTLS:
		return "DTLS"
	case ProtocolTURN:
		return "TURN ChannelData"
	case ProtocolRTP:
		return "RTP"
	case ProtocolRTCP:
		return "RTCP"
	case ProtocolUnknown:
		return "Unknown"
	}

	return "Unknown"
}

// RFC 7983 first byte ranges and minimal header sizes.
const (
	stunMaxFirstByte = 3
	dtlsMinFirstByte = 20
	dtlsMaxFirstByte = 63
	turnMinFirstByte = 64
	turnMaxFirstByte = 79
	rtpMinFirstByte  = 128
	rtpMaxFirstByte  = 191

	stunHeaderSize        = 20
	dtlsRecordHeaderSize  = 13
	channelDataHeaderSize = 4
	rtpHeaderSize         = 12
	rtcpHeaderSize        = 4

	stunMagicCookie = 0x2112A442
)

// classify routes b to a protocol using the RFC 7983 first byte ranges and
// validates the minimal header and length invariants of the matched
// protocol. Datagrams that fail validation are ProtocolUnknown.
func classify(b []byte) Protocol {
	if len(b) == 0 {
		return ProtocolUnknown
	}

	switch first := b[0]; {
	case first <= stunMaxFirstByte:
		return classifySTUN(b)
	case first >= dtlsMinFirstByte && first <= dtlsMaxFirstByte:
		return classifyDTLS(b)
	case first >= turnMinFirstByte && first <= turnMaxFirstByte:
		return classifyTURN(b)
	case first >= rtpMinFirstByte && first <= rtpMaxFirstByte:
		return classifyRTPRTCP(b)
	default:
		return ProtocolUnknown
	}
}

func classifySTUN(b []byte) Protocol {
	if len(b) < stunHeaderSize {
		return ProtocolUnknown
	}
	if binary.BigEndian.Uint32(b[4:8]) != stunMagicCookie {
		return ProtocolUnknown
	}
	// The message length counts 32-bit padded attributes after the header.
	msgLen := int(binary.BigEndian.Uint16(b[2:4]))
	if msgLen%4 != 0 || msgLen+stunHeaderSize != len(b) {
		return ProtocolUnknown
	}

	return ProtocolSTUN
}

func classifyDTLS(b []byte) Protocol {
	if len(b) < dtlsRecordHeaderSize {
		return ProtocolUnknown
	}
	// Assigned content types; RFC 7983 reserves the wider 20-63 range.
	switch b[0] {
	case 20, 21, 22, 23, 24:
	default:
		return ProtocolUnknown
	}
	// DTLS versions: 1.0 = {254, 255}, 1.2 = {254, 253}, 1.3 = {254, 252}.
	if b[1] != 254 || (b[2] != 255 && b[2] != 253 && b[2] != 252) {
		return ProtocolUnknown
	}
	// The record length must cover the rest of the datagram exactly.
	if recLen := int(binary.BigEndian.Uint16(b[11:13])); recLen+dtlsRecordHeaderSize != len(b) {
		return ProtocolUnknown
	}

	return ProtocolDTLS
}

func classifyTURN(b []byte) Protocol {
	if len(b) < channelDataHeaderSize {
		return ProtocolUnknown
	}
	if ch := binary.BigEndian.Uint16(b[0:2]); ch < 0x4000 || ch > 0x7FFF {
		return ProtocolUnknown
	}
	// Over UDP the length field counts exactly the application data,
	// without the padding used by TCP framing.
	if int(binary.BigEndian.Uint16(b[2:4]))+channelDataHeaderSize != len(b) {
		return ProtocolUnknown
	}

	return ProtocolTURN
}

func classifyRTPRTCP(b []byte) Protocol {
	if len(b) < 2 { //nolint:mnd
		return ProtocolUnknown
	}
	// RFC 5761: RTCP is demultiplexed from RTP by the packet type range
	// 192-223 (64-95 with the marker bit masked out).
	if pt := b[1] & 0x7F; pt >= 64 && pt <= 95 { //nolint:mnd
		return classifyRTCP(b)
	}

	return classifyRTP(b)
}

func classifyRTP(b []byte) Protocol {
	if len(b) < rtpHeaderSize {
		return ProtocolUnknown
	}
	// The CSRC count must fit into the datagram.
	if cc := int(b[0] & 0x0F); rtpHeaderSize+4*cc > len(b) {
		return ProtocolUnknown
	}

	return ProtocolRTP
}

func classifyRTCP(b []byte) Protocol {
	if len(b) < rtcpHeaderSize {
		return ProtocolUnknown
	}
	// The length field counts 32-bit words minus one and must cover the
	// whole datagram, compound packets included.
	if (int(binary.BigEndian.Uint16(b[2:4]))+1)*4 != len(b) {
		return ProtocolUnknown
	}

	return ProtocolRTCP
}
