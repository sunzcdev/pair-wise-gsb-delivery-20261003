package wsutil

import (
	"github.com/gobwas/httphead"
	"github.com/gobwas/ws"
)

// RecvExtension is an interface for clearing fragment header RSV bits.
type RecvExtension interface {
	UnsetBits(ws.Header) (ws.Header, error)
}

// RecvExtensionFunc is an adapter to allow the use of ordinary functions as
// RecvExtension.
type RecvExtensionFunc func(ws.Header) (ws.Header, error)

// BitsRecv implements RecvExtension.
func (fn RecvExtensionFunc) UnsetBits(h ws.Header) (ws.Header, error) {
	return fn(h)
}

// SendExtension is an interface for setting fragment header RSV bits.
type SendExtension interface {
	SetBits(ws.Header) (ws.Header, error)
}

// SendExtensionFunc is an adapter to allow the use of ordinary functions as
// SendExtension.
type SendExtensionFunc func(ws.Header) (ws.Header, error)

// BitsSend implements SendExtension.
func (fn SendExtensionFunc) SetBits(h ws.Header) (ws.Header, error) {
	return fn(h)
}

// Flags describe how an extension may use the RSV bits it claimed during the
// handshake.
//
// By default every claimed RSV bit is treated as a per-fragment bit which can
// appear on any frame. If RSVFirstFragment is set for a bit, then it may be
// present only on the first fragment of a data message.
type Flags uint8

const (
	// RsvFirstFragment marks RSV bits that are meaningful only on the first
	// fragment of a data message. Such bits MUST NOT be set on continuation
	// fragments and control frames.
	// When set it applies to all RSV bits the extension claims.
	RsvFirstFragment Flags = 1 << iota
)

// Extension is a negotiated extension that participates in both receiving
// and sending frame headers.
//
// RecvExtension and SendExtension methods are invoked with frames carrying
// only the RSV bits the extension claimed: bits of other extensions are
// already stripped by the shared arbitration, so implementations do not need
// to know about their RSV masks at all.
type Extension interface {
	RecvExtension
	SendExtension
}

// NegotiatedExtension pairs a negotiated extension with the RSV bits it owns
// and the flags describing their usage.
//
// It is the per-connection result of the handshake: both the receiving and
// the sending paths must be configured from the same slice of it to guarantee
// that both sides share a single source of truth about RSV bit ownership.
type NegotiatedExtension struct {
	// Name is the extension name as negotiated in the
	// Sec-WebSocket-Extensions header. It is used in error messages.
	Name string

	// Rsv is the bitmask of RSV bits owned by the extension (see RsvBit*
	// constants).
	Rsv byte

	// Flags describes how the owned RSV bits may be used. Use RsvBit* values
	// RsvFirstFragment restricts all owned bits to the first fragment of a
	// data message.
	Flags Flags

	// Extension processes frame headers. It may be nil if the extension does
	// not need to change any header bits (e.g. it declared RSV bits only to
	// reserve them).
	Extension Extension
}

// NewExtensions creates the shared send/receive RSV bit arbitration from the
// negotiated extensions. It returns an error describing the first conflict
// (using extension names and bits) if two extensions claim the same RSV bit
// or invalid bits are declared.
//
// The returned *Extensions implements both RecvExtension and SendExtension
// and is safe to configure on a Reader and a Writer of the same connection.
func NewExtensions(exts ...NegotiatedExtension) (*Extensions, error) {
	e := &Extensions{
		extensions: append([]NegotiatedExtension(nil), exts...),
	}
	root := make([]ws.NegotiatedExtension, 0, len(exts))
	for _, ext := range exts {
		root = append(root, ws.NegotiatedExtension{
			Option: httphead.Option{Name: []byte(ext.Name)},
			Rsv:    ext.Rsv,
		})
	}
	if err := ws.CheckExtensionConflicts(root); err != nil {
		return nil, err
	}
	for _, ext := range exts {
		for _, bit := range rsvBits {
			if ext.Rsv&bit == 0 {
				continue
			}
			e.allowed |= bit
		}
	}
	return e, nil
}

var rsvBits = [...]byte{ws.RsvBit1, ws.RsvBit2, ws.RsvBit3}

// Extensions is the shared arbitration over RSV bits of all negotiated
// extensions of a single connection. It implements RecvExtension and
// SendExtension.
//
// Its methods are not goroutine safe.
type Extensions struct {
	extensions []NegotiatedExtension

	// allowed is the union of all claimed RSV bits.
	allowed byte
}

// Allowed returns the mask of all RSV bits claimed by the negotiated
// extensions.
func (e *Extensions) Allowed() byte {
	return e.allowed
}

// UnsetBits implements RecvExtension.
//
// It fails with a protocol error if the peer sets an RSV bit that no
// extension negotiated, or sets a first-fragment-only bit on a continuation
// fragment or a control frame. Then each extension processes only its own
// bits, which must be cleared afterwards.
func (e *Extensions) UnsetBits(h ws.Header) (ws.Header, error) {
	if h.Rsv&^e.allowed != 0 {
		return h, ws.ProtocolError(unexpectedRsvBitsError(h.Rsv, e, ""))
	}
	if bits := firstFragmentBits(h, e.extensions); bits != 0 {
		return h, ws.ProtocolError(rsvBitsName(bits) +
			" bit is allowed on the first fragment only")
	}
	for _, ext := range e.extensions {
		mine := h.Rsv & ext.Rsv
		if ext.Extension == nil {
			h.Rsv &^= ext.Rsv
			continue
		}
		rest := h.Rsv &^ ext.Rsv
		h.Rsv = mine
		var err error
		h, err = ext.Extension.UnsetBits(h)
		if err != nil {
			return h, err
		}
		// The extension may only clear or leave its own bits; it must not
		// introduce bits it does not own. Bits of other extensions are
		// restored untouched.
		if h.Rsv&^ext.Rsv != 0 {
			return h, ws.ProtocolError(unexpectedRsvBitsError(h.Rsv, e, ext.Name))
		}
		h.Rsv |= rest
	}
	return h, nil
}

// SetBits implements SendExtension.
//
// It makes sure extensions can not set RSV bits they did not negotiate nor
// propagate first-fragment-only bits onto continuation fragments.
func (e *Extensions) SetBits(h ws.Header) (ws.Header, error) {
	for _, ext := range e.extensions {
		if ext.Extension == nil {
			continue
		}
		rest := h.Rsv &^ ext.Rsv
		h.Rsv &= ext.Rsv
		var err error
		h, err = ext.Extension.SetBits(h)
		if err != nil {
			return h, err
		}
		if h.Rsv&^ext.Rsv != 0 {
			return h, ws.ProtocolError("extension " + ext.Name +
				" set rsv bits it did not negotiate: " +
				rsvBitsName(h.Rsv&^ext.Rsv))
		}
		h.Rsv |= rest
	}
	if bits := firstFragmentBits(h, e.extensions); bits != 0 {
		return h, ws.ProtocolError("first fragment only rsv bits " +
			rsvBitsName(bits) + " propagated to continuation or control frame")
	}
	return h, nil
}

// firstFragmentBits returns the bits set in h which are declared as valid on
// the first fragment of a data message only.
func firstFragmentBits(h ws.Header, exts []NegotiatedExtension) byte {
	if h.OpCode.IsData() && h.OpCode != ws.OpContinuation {
		return 0
	}
	var bits byte
	for _, ext := range exts {
		if ext.Flags&RsvFirstFragment == 0 {
			continue
		}
		bits |= h.Rsv & ext.Rsv
	}
	return bits
}

func unexpectedRsvBitsError(rsv byte, e *Extensions, by string) string {
	bits := rsv &^ e.allowed
	var msg string
	if by != "" {
		msg = "extension " + by + " leaves unnegotiated rsv bits "
	} else {
		msg = "unexpected rsv bits from peer: "
	}
	return msg + rsvBitsName(bits)
}

func rsvBitsName(mask byte) string {
	const sep = "|"
	var b [len("RSV1")*3 + 2]byte
	buf := b[:0]
	for _, bit := range []struct {
		mask byte
		name string
	}{
		{ws.RsvBit1, "RSV1"},
		{ws.RsvBit2, "RSV2"},
		{ws.RsvBit3, "RSV3"},
	} {
		if mask&bit.mask == 0 {
			continue
		}
		if len(buf) > 0 {
			buf = append(buf, sep...)
		}
		buf = append(buf, bit.name...)
	}
	return string(buf)
}
