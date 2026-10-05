package wsutil

import (
	"fmt"
	"strings"

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

// ErrUnnegotiatedRsvBits is returned when a received frame carries RSV bits
// that no negotiated extension has declared.
var ErrUnnegotiatedRsvBits = ws.ProtocolError(
	"non-zero rsv bits not negotiated by any extension",
)

// NegotiatedExtension describes a single WebSocket extension together with
// the RSV bits it is allowed to use once negotiated.
type NegotiatedExtension struct {
	// Name is the extension name as it appears in the
	// Sec-WebSocket-Extensions header, e.g. "permessage-deflate".
	Name string

	// RsvBits is the mask of RSV bits (in the form of ws.Header.Rsv, see
	// ws.Rsv() and ws.RsvBits()) that the extension may set on the frames
	// it produces or consumes. Frames carrying any other RSV bits are
	// rejected as protocol errors.
	RsvBits byte

	// FirstFragmentOnly is the subset of RsvBits that carries meaning only
	// on the first fragment of a (possibly fragmented) message. Such bits
	// are never set on continuation frames by the derived send
	// configuration and are rejected when received on one.
	FirstFragmentOnly byte

	// Negotiate is the extension's handshake negotiation logic. It is
	// called by Negotiator with each extension offer from the peer and
	// should return the option to be sent back as accepted, or a zero
	// option to decline the offer.
	//
	// It may be nil if the extension is not used with Negotiator.
	Negotiate func(httphead.Option) (httphead.Option, error)

	// Recv and Send contain the extension logic used to derive receive and
	// send path configuration. Either may be nil.
	Recv RecvExtension
	Send SendExtension
}

// NegotiatedExtensions is the result of a successful extension negotiation.
// It is the single source from which both receive and send path
// configuration should be derived.
type NegotiatedExtensions []NegotiatedExtension

// NegotiateExtensions validates the given set of negotiated extensions and
// returns it as the negotiation result. It returns an
// *ExtensionConflictError if two extensions declare the same RSV bit.
//
// It is intended to be called before the upgrade succeeds, so that
// conflicting extensions fail the handshake instead of silently corrupting
// the framing later on.
func NegotiateExtensions(exts ...NegotiatedExtension) (NegotiatedExtensions, error) {
	var bits byte
	for i, x := range exts {
		// First-fragment-only bits are meaningful only as a subset of the
		// declared bits.
		exts[i].FirstFragmentOnly &= x.RsvBits
		if c := x.RsvBits & bits; c != 0 {
			return nil, &ExtensionConflictError{
				Name: x.Name,
				Bits: c,
			}
		}
		bits |= x.RsvBits
	}
	return NegotiatedExtensions(exts), nil
}

// RecvExtensions derives the receive path configuration from the
// negotiation result. The returned extensions are meant to be assigned to
// the Reader.Extensions field.
//
// The first extension in the returned slice is a guard that rejects frames
// carrying RSV bits that no negotiated extension has declared. Bits declared
// as FirstFragmentOnly are additionally rejected on continuation frames.
func (xs NegotiatedExtensions) RecvExtensions() []RecvExtension {
	if len(xs) == 0 {
		return nil
	}
	recv := make([]RecvExtension, 0, len(xs)+1)
	recv = append(recv, RecvExtensionFunc(xs.checkBits))
	for _, x := range xs {
		if x.Recv != nil {
			recv = append(recv, x.Recv)
		}
	}
	return recv
}

// SendExtensions derives the send path configuration from the negotiation
// result. The returned extensions are meant to be passed to
// Writer.SetExtensions().
//
// Bits declared as FirstFragmentOnly are stripped from continuation frames,
// so that extensions which naively set their bits on every data frame do
// not propagate them to non-first fragments.
func (xs NegotiatedExtensions) SendExtensions() []SendExtension {
	if len(xs) == 0 {
		return nil
	}
	send := make([]SendExtension, 0, len(xs))
	for _, x := range xs {
		if x.Send != nil {
			send = append(send, sendExtension{x})
		}
	}
	return send
}

// checkBits implements the receive path guard against unnegotiated RSV bits.
func (xs NegotiatedExtensions) checkBits(h ws.Header) (ws.Header, error) {
	var allowed byte
	for _, x := range xs {
		allowed |= x.RsvBits
		if h.OpCode == ws.OpContinuation {
			allowed &^= x.FirstFragmentOnly
		}
	}
	if h.Rsv&^allowed != 0 {
		return h, ErrUnnegotiatedRsvBits
	}
	return h, nil
}

// sendExtension wraps a single negotiated extension's Send logic, stripping
// first-fragment-only bits from continuation frames.
type sendExtension struct {
	x NegotiatedExtension
}

// SetBits implements SendExtension.
func (s sendExtension) SetBits(h ws.Header) (ws.Header, error) {
	h, err := s.x.Send.SetBits(h)
	if err != nil {
		return h, err
	}
	if h.OpCode == ws.OpContinuation {
		h.Rsv &^= s.x.FirstFragmentOnly
	}
	return h, nil
}

// ExtensionConflictError is returned when two extensions from the same
// negotiation result declare the same RSV bit(s).
type ExtensionConflictError struct {
	// Name is the name of the extension whose RSV bits declaration
	// conflicts with some other extension negotiated earlier.
	Name string
	// Bits is the mask of the conflicting RSV bits.
	Bits byte
}

// Error implements error interface.
func (e *ExtensionConflictError) Error() string {
	return fmt.Sprintf(
		"extension %q declares rsv bits already claimed by another negotiated extension: %s",
		e.Name, rsvBitsString(e.Bits),
	)
}

func rsvBitsString(bits byte) string {
	names := make([]string, 0, 3)
	for _, b := range []struct {
		mask byte
		name string
	}{
		{ws.Rsv(true, false, false), "rsv1"},
		{ws.Rsv(false, true, false), "rsv2"},
		{ws.Rsv(false, false, true), "rsv3"},
	} {
		if bits&b.mask != 0 {
			names = append(names, b.name)
		}
	}
	return strings.Join(names, ", ")
}

// Negotiator negotiates a fixed set of extensions during the WebSocket
// handshake and collects the accepted ones into a NegotiatedExtensions
// result.
type Negotiator struct {
	exts     []NegotiatedExtension
	accepted NegotiatedExtensions
	bits     byte
}

// NewNegotiator returns a Negotiator that negotiates the given extensions.
func NewNegotiator(exts ...NegotiatedExtension) *Negotiator {
	return &Negotiator{exts: exts}
}

// Negotiate implements the signature of the ws.Upgrader.Negotiate callback.
// It offers opt to every registered extension and remembers the first one
// that accepts it. If the accepting extension declares RSV bits that
// conflict with an already accepted extension, an *ExtensionConflictError is
// returned, which makes ws.Upgrader reject the handshake before the upgrade
// succeeds.
func (n *Negotiator) Negotiate(opt httphead.Option) (httphead.Option, error) {
	for _, x := range n.exts {
		if x.Negotiate == nil {
			continue
		}
		accept, err := x.Negotiate(opt)
		if err != nil {
			return httphead.Option{}, err
		}
		if accept.Size() == 0 {
			continue
		}
		if bits := x.RsvBits & n.bits; bits != 0 {
			return httphead.Option{}, &ExtensionConflictError{
				Name: x.Name,
				Bits: bits,
			}
		}
		n.bits |= x.RsvBits
		n.accepted = append(n.accepted, x)
		return accept, nil
	}
	return httphead.Option{}, nil
}

// Result returns the negotiation result: the set of extensions accepted
// during the handshake. Both receive and send path configuration should be
// derived from it.
func (n *Negotiator) Result() NegotiatedExtensions {
	return n.accepted
}
