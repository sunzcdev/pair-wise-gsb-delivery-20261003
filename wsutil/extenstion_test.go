package wsutil

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"testing"

	"github.com/gobwas/httphead"
	"github.com/gobwas/ws"
)

var (
	testRsv1 = ws.Rsv(true, false, false)
	testRsv2 = ws.Rsv(false, true, false)
	testRsv3 = ws.Rsv(false, false, true)
)

// fakeExtension is a tiny test extension that sets (on send) and clears (on
// receive) its declared RSV bits on every data frame.
type fakeExtension struct {
	bits byte
}

func (x fakeExtension) SetBits(h ws.Header) (ws.Header, error) {
	if h.OpCode.IsData() {
		h.Rsv |= x.bits
	}
	return h, nil
}

func (x fakeExtension) UnsetBits(h ws.Header) (ws.Header, error) {
	h.Rsv &^= x.bits
	return h, nil
}

// alphaExtension declares rsv1, meaningful only on the first fragment of a
// message.
func alphaExtension() NegotiatedExtension {
	return NegotiatedExtension{
		Name:              "alpha",
		RsvBits:           testRsv1,
		FirstFragmentOnly: testRsv1,
		Negotiate:         acceptOwnName("alpha"),
		Recv:              fakeExtension{testRsv1},
		Send:              fakeExtension{testRsv1},
	}
}

// betaExtension declares rsv2, meaningful on every data frame.
func betaExtension() NegotiatedExtension {
	return NegotiatedExtension{
		Name:      "beta",
		RsvBits:   testRsv2,
		Negotiate: acceptOwnName("beta"),
		Recv:      fakeExtension{testRsv2},
		Send:      fakeExtension{testRsv2},
	}
}

func acceptOwnName(name string) func(httphead.Option) (httphead.Option, error) {
	return func(opt httphead.Option) (httphead.Option, error) {
		if bytes.Equal(opt.Name, []byte(name)) {
			return httphead.Option{Name: []byte(name)}, nil
		}
		return httphead.Option{}, nil
	}
}

func TestNegotiateExtensionsConflict(t *testing.T) {
	conflicting := betaExtension()
	conflicting.RsvBits = testRsv1 // Claim the bit already used by alpha.

	_, err := NegotiateExtensions(alphaExtension(), conflicting)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	ce, ok := err.(*ExtensionConflictError)
	if !ok {
		t.Fatalf("expected *ExtensionConflictError, got %T: %v", err, err)
	}
	if ce.Name != "beta" {
		t.Errorf("conflicting extension name = %q; want %q", ce.Name, "beta")
	}
	if ce.Bits != testRsv1 {
		t.Errorf("conflicting bits = %v; want rsv1", ce.Bits)
	}
}

func TestNegotiatorUpgradeConflict(t *testing.T) {
	conflicting := betaExtension()
	conflicting.RsvBits = testRsv1 // Claim the bit already used by alpha.

	negotiator := NewNegotiator(alphaExtension(), conflicting)
	u := ws.Upgrader{
		Negotiate: negotiator.Negotiate,
	}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	done := make(chan error, 1)
	go func() {
		_, err := u.Upgrade(server)
		done <- err
	}()
	// Drain the client side so that the server can write its (error)
	// response into the unbuffered pipe.
	go io.Copy(ioutil.Discard, client)

	_, err := fmt.Fprintf(client, ""+
		"GET / HTTP/1.1\r\n"+
		"Host: example.com\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Extensions: alpha, beta\r\n"+
		"\r\n",
	)
	if err != nil {
		t.Fatalf("write handshake request: %v", err)
	}

	err = <-done
	ce, ok := err.(*ExtensionConflictError)
	if !ok {
		t.Fatalf("expected *ExtensionConflictError, got %T: %v", err, err)
	}
	if ce.Name != "beta" {
		t.Errorf("conflicting extension name = %q; want %q", ce.Name, "beta")
	}
	if ce.Bits != testRsv1 {
		t.Errorf("conflicting bits = %v; want rsv1", ce.Bits)
	}
}

func TestNegotiatedExtensionsSend(t *testing.T) {
	neg, err := NegotiateExtensions(alphaExtension(), betaExtension())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var buf bytes.Buffer
	w := NewWriterSize(&buf, ws.StateServerSide, ws.OpText, 8)
	w.SetExtensions(neg.SendExtensions()...)

	if _, err := w.Write([]byte("hello, ")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.FlushFragment(); err != nil {
		t.Fatalf("flush fragment: %v", err)
	}
	if _, err := w.Write([]byte("world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var headers []ws.Header
	for buf.Len() > 0 {
		h, err := ws.ReadHeader(&buf)
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		headers = append(headers, h)
		buf.Next(int(h.Length))
	}
	if len(headers) != 2 {
		t.Fatalf("got %d frames; want 2", len(headers))
	}

	first, second := headers[0], headers[1]
	if first.OpCode != ws.OpText {
		t.Errorf("first frame opcode = %v; want OpText", first.OpCode)
	}
	if act, want := first.Rsv, testRsv1|testRsv2; act != want {
		t.Errorf("first frame rsv = %v; want rsv1|rsv2", act)
	}
	if second.OpCode != ws.OpContinuation {
		t.Errorf("second frame opcode = %v; want OpContinuation", second.OpCode)
	}
	if second.Rsv&testRsv1 != 0 {
		t.Errorf("continuation frame carries first-fragment-only rsv1 bit")
	}
	if act, want := second.Rsv, testRsv2; act != want {
		t.Errorf("continuation frame rsv = %v; want rsv2 only", act)
	}
}

func TestNegotiatedExtensionsRecv(t *testing.T) {
	neg, err := NegotiateExtensions(alphaExtension(), betaExtension())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var buf bytes.Buffer
	first := ws.NewFrame(ws.OpText, false, []byte("hello, "))
	first.Header.Rsv = testRsv1 | testRsv2
	last := ws.NewFrame(ws.OpContinuation, true, []byte("world"))
	last.Header.Rsv = testRsv2
	for _, f := range []ws.Frame{first, last} {
		if err := ws.WriteFrame(&buf, f); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}

	r := NewClientSideReader(&buf)
	r.State |= ws.StateExtended
	r.Extensions = neg.RecvExtensions()

	h, err := r.NextFrame()
	if err != nil {
		t.Fatalf("next frame: %v", err)
	}
	if h.Rsv != 0 {
		t.Errorf("header rsv = %v after extensions; want 0", h.Rsv)
	}
	p, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if act, want := string(p), "hello, world"; act != want {
		t.Errorf("message = %q; want %q", act, want)
	}
}

func TestNegotiatedExtensionsUnnegotiatedBits(t *testing.T) {
	neg, err := NegotiateExtensions(alphaExtension(), betaExtension())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	unnegotiated := ws.NewFrame(ws.OpText, true, []byte("nope"))
	unnegotiated.Header.Rsv = testRsv3

	first := ws.NewFrame(ws.OpText, false, []byte("hel"))
	first.Header.Rsv = testRsv1
	wrongContinuation := ws.NewFrame(ws.OpContinuation, true, []byte("lo"))
	wrongContinuation.Header.Rsv = testRsv1 // First-fragment-only bit.

	for _, test := range []struct {
		name   string
		frames []ws.Frame
	}{
		{"unnegotiated bit", []ws.Frame{unnegotiated}},
		{"first-fragment-only bit on continuation", []ws.Frame{first, wrongContinuation}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			for _, f := range test.frames {
				if err := ws.WriteFrame(&buf, f); err != nil {
					t.Fatalf("write frame: %v", err)
				}
			}

			r := NewClientSideReader(&buf)
			r.State |= ws.StateExtended
			r.Extensions = neg.RecvExtensions()

			var err error
			for range test.frames {
				if _, err = r.NextFrame(); err != nil {
					break
				}
				if err = r.Discard(); err != nil {
					break
				}
			}
			if err != ErrUnnegotiatedRsvBits {
				t.Errorf("error = %v; want %v", err, ErrUnnegotiatedRsvBits)
			}
		})
	}
}
