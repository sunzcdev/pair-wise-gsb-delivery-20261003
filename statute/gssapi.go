package statute

import (
	"encoding/binary"
	"fmt"
	"io"
)

// GSS-API authentication subnegotiation version (RFC 1961)
const GSSAuthVersion = byte(0x01)

// GSS-API message types (RFC 1961 section 3-5)
const (
	// GSSMsgAuthentication carries a GSS-API context-level token
	GSSMsgAuthentication = byte(0x01)
	// GSSMsgProtectionLevel carries a wrapped protection-level octet
	GSSMsgProtectionLevel = byte(0x02)
	// GSSMsgEncapsulated carries GSS_Wrap protected user data
	GSSMsgEncapsulated = byte(0x03)
	// GSSMsgAbort signals that the peer aborts the authentication
	GSSMsgAbort = byte(0xff)
)

// GSS-API protection levels (RFC 1961 section 4)
const (
	// GSSProtectionNone no per-message protection
	GSSProtectionNone = byte(0x00)
	// GSSProtectionIntegrity per-message integrity only
	GSSProtectionIntegrity = byte(0x01)
	// GSSProtectionConfidentiality per-message integrity and confidentiality
	GSSProtectionConfidentiality = byte(0x02)
)

// GSSMaxTokenSize is the largest token carryable by the 2-octet TLEN field
const GSSMaxTokenSize = 0xffff

// ErrGSSMessageTooLarge is returned when a token does not fit the TLEN field
var ErrGSSMessageTooLarge = fmt.Errorf("gss-api token exceeds %d bytes", GSSMaxTokenSize)

// GSSMessage is the GSS-API subnegotiation message (RFC 1961 section 3)
//
//	+-----+------+------+............+
//	| VER | MTYP | TLEN |   TOKEN    |
//	+-----+------+------+............+
//	|  1  |  1   |  2   | up to 2^16-1|
//	+-----+------+------+............+
type GSSMessage struct {
	Ver   byte
	MTyp  byte
	Token []byte
}

// NewGSSMessage new GSS-API subnegotiation message
func NewGSSMessage(mtyp byte, token []byte) GSSMessage {
	return GSSMessage{
		Ver:   GSSAuthVersion,
		MTyp:  mtyp,
		Token: token,
	}
}

// ParseGSSMessage parse GSS-API subnegotiation message.
func ParseGSSMessage(r io.Reader) (msg GSSMessage, err error) {
	hdr := make([]byte, 4)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return
	}
	msg.Ver, msg.MTyp = hdr[0], hdr[1]
	tlen := int(binary.BigEndian.Uint16(hdr[2:4]))
	msg.Token = make([]byte, tlen)
	_, err = io.ReadFull(r, msg.Token)
	return
}

// Bytes GSS-API subnegotiation message to bytes
func (sf GSSMessage) Bytes() []byte {
	b := make([]byte, 0, 4+len(sf.Token))
	b = append(b, sf.Ver, sf.MTyp, 0, 0)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(sf.Token)))
	b = append(b, sf.Token...)
	return b
}
