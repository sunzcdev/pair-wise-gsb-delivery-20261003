package wsutil

import (
	"errors"
	"strings"
	"testing"

	"github.com/gobwas/ws"
)

// fakeExtension is a tiny extension that simply owns and toggles a fixed RSV
// bit on data frames. It does not implement compression.
type fakeExtension struct {
	bit       byte
	set       bool
	allFrames bool
}

func (e *fakeExtension) UnsetBits(h ws.Header) (ws.Header, error) {
	h.Rsv &^= e.bit
	return h, nil
}

func (e *fakeExtension) SetBits(h ws.Header) (ws.Header, error) {
	if e.set && (e.allFrames || (h.OpCode.IsData() && h.OpCode != ws.OpContinuation)) {
		h.Rsv |= e.bit
	}
	return h, nil
}

func newFakeArbiter(t *testing.T, firstFragment bool) (*Extensions, *fakeExtension, *fakeExtension) {
	t.Helper()
	alpha := &fakeExtension{bit: ws.RsvBit1, set: true}
	beta := &fakeExtension{bit: ws.RsvBit2, set: true}

	exts := []NegotiatedExtension{
		{Name: "alpha", Rsv: ws.RsvBit1, Extension: alpha},
		{Name: "beta", Rsv: ws.RsvBit2, Extension: beta},
	}
	if firstFragment {
		// Both bits are per-message bits: they are valid on the first
		// fragment only and must not propagate to continuation frames.
		exts[0].Flags = RsvFirstFragment
		exts[1].Flags = RsvFirstFragment
	}
	arbitration, err := NewExtensions(exts...)
	if err != nil {
		t.Fatalf("NewExtensions() error: %v", err)
	}
	return arbitration, alpha, beta
}

func TestExtensionsNewConflict(t *testing.T) {
	// Mutual exclusion: the same bit can not be claimed by two extensions.
	_, err := NewExtensions(
		NegotiatedExtension{Name: "alpha", Rsv: ws.RsvBit1},
		NegotiatedExtension{Name: "beta", Rsv: ws.RsvBit1},
	)
	if err == nil {
		t.Fatalf("expected conflict error")
	}
	if !errors.Is(err, ws.ErrExtensionConflict) {
		t.Fatalf("error must wrap ws.ErrExtensionConflict: %v", err)
	}
	for _, want := range []string{"alpha", "beta", "RSV1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
	}
}

func TestExtensionsLegalCombination(t *testing.T) {
	// Disjoint bits on the first fragment are accepted on both paths and
	// stripped on receive.
	arbitration, _, _ := newFakeArbiter(t, false)

	h := ws.Header{OpCode: ws.OpText, Fin: true}
	sent, err := arbitration.SetBits(h)
	if err != nil {
		t.Fatalf("SetBits() error: %v", err)
	}
	if r1, r2, r3 := ws.RsvBits(sent.Rsv); !r1 || !r2 || r3 {
		t.Fatalf("unexpected sent rsv: %v %v %v", r1, r2, r3)
	}

	recv, err := arbitration.UnsetBits(sent)
	if err != nil {
		t.Fatalf("UnsetBits() error: %v", err)
	}
	if recv.Rsv != 0 {
		t.Fatalf("all negotiated bits must be cleared, got %#x", recv.Rsv)
	}
}

func TestExtensionsPeerUnauthorizedBit(t *testing.T) {
	// Only RSV1 and RSV2 are negotiated: an RSV3 set by the peer is a
	// protocol error even though an extension is active.
	arbitration, _, _ := newFakeArbiter(t, false)

	h := ws.Header{
		OpCode: ws.OpText,
		Fin:    true,
		Rsv:    ws.Rsv(true, true, true),
	}
	_, err := arbitration.UnsetBits(h)
	if err == nil {
		t.Fatalf("expected protocol error for unauthorized RSV3")
	}
	var proto ws.ProtocolError
	if !errors.As(err, &proto) {
		t.Fatalf("error must be ws.ProtocolError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "RSV3") {
		t.Fatalf("error must name the offending bit: %v", err)
	}

	// A fully unnegotiated bit is rejected as well.
	noExt, err := NewExtensions()
	if err != nil {
		t.Fatalf("NewExtensions() error: %v", err)
	}
	h.Rsv = ws.RsvBit1
	if _, err := noExt.UnsetBits(h); err == nil {
		t.Fatalf("expected protocol error without any extension")
	}
}

func TestExtensionsFirstFragmentOnly(t *testing.T) {
	// Per-message bits must not be present on continuation fragments.
	arbitration, _, _ := newFakeArbiter(t, true)

	first := ws.Header{OpCode: ws.OpText, Fin: false, Rsv: ws.RsvBit1 | ws.RsvBit2}
	if _, err := arbitration.UnsetBits(first); err != nil {
		t.Fatalf("bits on first fragment must be accepted: %v", err)
	}

	cont := ws.Header{OpCode: ws.OpContinuation, Fin: true, Rsv: ws.RsvBit1}
	_, err := arbitration.UnsetBits(cont)
	if err == nil {
		t.Fatalf("per-message bit on continuation must be a protocol error")
	}
	if !strings.Contains(err.Error(), "first fragment") {
		t.Fatalf("error must explain first fragment restriction: %v", err)
	}

	// Same guarantee on the send path: a misbehaving extension trying to
	// propagate the bit to a continuation must fail instead of emitting an
	// illegal frame.
	arbitration.extensions[0].Extension = &fakeExtension{
		bit: ws.RsvBit1, set: true, allFrames: true,
	}
	_, err = arbitration.SetBits(ws.Header{OpCode: ws.OpContinuation, Fin: true})
	if err == nil {
		t.Fatalf("send path must reject first fragment bit on continuation")
	}

	// Control frames never carry first-fragment-only bits.
	if _, err := arbitration.UnsetBits(ws.Header{
		OpCode: ws.OpPing,
		Fin:    true,
		Rsv:    ws.RsvBit2,
	}); err == nil {
		t.Fatalf("per-message bit on control frame must be a protocol error")
	}
}

func TestExtensionsSendIsolation(t *testing.T) {
	// An extension may not set an RSV bit it did not negotiate.
	arbitration, _, _ := newFakeArbiter(t, false)
	bad := RecvSendExtensionFunc(
		func(h ws.Header) (ws.Header, error) {
			h.Rsv |= ws.RsvBit3
			return h, nil
		},
		func(h ws.Header) (ws.Header, error) {
			h.Rsv |= ws.RsvBit3
			return h, nil
		},
	)
	arbitration.extensions[0].Extension = bad

	if _, err := arbitration.SetBits(ws.Header{OpCode: ws.OpText, Fin: true}); err == nil {
		t.Fatalf("expected error when extension sets unnegotiated bit")
	}
}

// recvSendExtensionFunc pairs RecvExtensionFunc and SendExtensionFunc into an
// Extension.
type recvSendPair struct {
	recv RecvExtensionFunc
	send SendExtensionFunc
}

func (p recvSendPair) UnsetBits(h ws.Header) (ws.Header, error) {
	return p.recv(h)
}

func (p recvSendPair) SetBits(h ws.Header) (ws.Header, error) {
	return p.send(h)
}

func RecvSendExtensionFunc(
	recv func(ws.Header) (ws.Header, error),
	send func(ws.Header) (ws.Header, error),
) Extension {
	return recvSendPair{recv: recv, send: send}
}
