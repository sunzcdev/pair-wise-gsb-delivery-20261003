package stun

import "encoding/binary"

// Protocol identifies a datagram protocol accepted by Mux.
type Protocol uint8

const (
	// ProtocolUnknown is returned for datagrams that do not match a supported
	// protocol or fail its minimum header checks.
	ProtocolUnknown Protocol = iota
	// ProtocolSTUN is a RFC 5389 STUN message.
	ProtocolSTUN
	// ProtocolDTLS is a DTLS record.
	ProtocolDTLS
	// ProtocolRTP is an RTP packet.
	ProtocolRTP
	// ProtocolRTCP is an RTCP packet, including multiplexed RTCP.
	ProtocolRTCP
	// ProtocolTURN is a TURN ChannelData message.
	ProtocolTURN
)

// Classify applies the demultiplexing rules from RFC 7983 and validates the
// minimum fixed headers and declared lengths. It does not authenticate or
// fully decode a protocol message.
func Classify(p []byte) (Protocol, bool) {
	switch {
	case isSTUNDatagram(p):
		return ProtocolSTUN, true
	case isTURNDatagram(p):
		return ProtocolTURN, true
	case isDTLSDatagram(p):
		return ProtocolDTLS, true
	case isRTPDatagram(p):
		return ProtocolRTP, true
	case isRTCPDatagram(p):
		return ProtocolRTCP, true
	default:
		return ProtocolUnknown, false
	}
}

func isSTUNDatagram(p []byte) bool {
	const headerSize = 20

	if len(p) < headerSize || p[0]&0xc0 != 0 {
		return false
	}

	messageLength := int(binary.BigEndian.Uint16(p[2:4]))
	if messageLength%4 != 0 || len(p) != headerSize+messageLength {
		return false
	}

	return binary.BigEndian.Uint32(p[4:8]) == magicCookie
}

func isTURNDatagram(p []byte) bool {
	const (
		headerSize     = 4
		maxPaddingSize = 3
	)

	if len(p) < headerSize {
		return false
	}

	channelNumber := binary.BigEndian.Uint16(p[0:2])
	if channelNumber < 0x4000 || channelNumber > 0x7fff {
		return false
	}

	valueLength := int(binary.BigEndian.Uint16(p[2:4]))
	const maxChannelDataLength = 16384

	if valueLength > maxChannelDataLength {
		return false
	}

	paddingLength := len(p) - headerSize - valueLength

	return paddingLength >= 0 && paddingLength <= maxPaddingSize
}

func isDTLSDatagram(p []byte) bool {
	const (
		headerSize    = 13
		contentType20 = 20
		contentType63 = 63
		maxRecordSize = 18432
	)

	if len(p) < headerSize {
		return false
	}

	for offset := 0; offset < len(p); {
		if len(p)-offset < headerSize {
			return false
		}

		contentType := int(p[offset])
		if contentType < contentType20 || contentType > contentType63 {
			return false
		}

		if p[offset+1] != 0xfe || (p[offset+2] != 0xff && p[offset+2] != 0xfd) {
			return false
		}

		recordLength := int(binary.BigEndian.Uint16(p[offset+11 : offset+13]))
		if recordLength > maxRecordSize {
			return false
		}

		offset += headerSize + recordLength
		if offset > len(p) {
			return false
		}
	}

	return true
}

func isRTPDatagram(p []byte) bool {
	const (
		fixedHeaderSize = 12
		csrcSize        = 4
		extensionSize   = 4
	)

	if len(p) < fixedHeaderSize || p[0]>>6 != 2 {
		return false
	}

	payloadType := int(p[1] & 0x7f)
	if payloadType >= 64 && payloadType <= 95 {
		return false
	}

	offset := fixedHeaderSize + int(p[0]&0x0f)*csrcSize
	if len(p) < offset {
		return false
	}

	if p[0]&0x10 != 0 {
		if len(p) < offset+extensionSize {
			return false
		}

		extensionLength := int(binary.BigEndian.Uint16(p[offset+2:offset+4])) * csrcSize
		offset += extensionSize + extensionLength
		if len(p) < offset {
			return false
		}
	}

	if p[0]&0x20 != 0 {
		paddingLength := int(p[len(p)-1])

		return paddingLength > 0 && paddingLength <= len(p)-offset
	}

	return true
}

func isRTCPDatagram(p []byte) bool {
	const (
		minPacketSize = 8
		versionBits   = 2
	)

	if len(p) < minPacketSize || p[0]>>6 != versionBits {
		return false
	}

	payloadType := int(p[1] & 0x7f)
	if payloadType < 64 || payloadType > 95 {
		return false
	}

	offset := 0
	for offset < len(p) {
		if len(p)-offset < minPacketSize {
			return false
		}

		lengthWords := int(binary.BigEndian.Uint16(p[offset+2 : offset+4]))
		packetLength := (lengthWords + 1) * 4
		if packetLength < minPacketSize || offset+packetLength > len(p) {
			return false
		}

		if p[offset]&0x20 != 0 && offset+packetLength != len(p) {
			return false
		}

		if offset+packetLength == len(p) && p[offset]&0x20 != 0 {
			packet := p[offset : offset+packetLength]
			paddingLength := int(packet[len(packet)-1])
			if paddingLength == 0 || paddingLength > packetLength {
				return false
			}
		}

		offset += packetLength
	}

	return offset == len(p)
}
