// Copyright 2026 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bytes"
	"compress/flate"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func takeoverTestServer(t *testing.T, enable bool, handle func(*Conn)) *cstServer {
	t.Helper()
	u := cstUpgrader
	u.EnableContextTakeover = enable
	var s cstServer
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		s.wg.Add(1)
		defer s.wg.Done()
		defer c.Close()
		handle(c)
	}))
	s.URL = makeWsProto(s.Server.URL)
	t.Cleanup(s.Close)
	return &s
}

func dialTakeover(t *testing.T, url string, takeover bool) *Conn {
	t.Helper()
	d := cstDialer
	d.EnableCompression = true
	d.EnableContextTakeover = takeover
	d.HandshakeTimeout = 10 * time.Second
	c, _, err := d.DialContext(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// Two peers that negotiated context takeover exchange a stream of similar
// messages, an empty message, fragmented data and interleaved pings. Every
// message must round-trip exactly.
func TestContextTakeoverEndToEnd(t *testing.T) {
	similar := similarMessages()

	s := takeoverTestServer(t, true, func(c *Conn) {
		for {
			mt, r, err := c.NextReader()
			if err != nil {
				return
			}
			w, err := c.NextWriter(mt)
			if err != nil {
				return
			}
			if _, err := io.Copy(w, r); err != nil {
				return
			}
			if err := w.Close(); err != nil {
				return
			}
		}
	})

	c := dialTakeover(t, s.URL, true)

	if c.newCompressionWriter == nil || c.newDecompressionReader == nil {
		t.Fatal("compression not negotiated")
	}
	if c.writeContextTakeover == nil || c.readContextTakeover == nil {
		t.Fatal("context takeover not negotiated")
	}

	send := func(messageType int, data []byte, fragment int) {
		t.Helper()
		w, err := c.NextWriter(messageType)
		if err != nil {
			t.Fatalf("NextWriter: %v", err)
		}
		for len(data) > 0 {
			n := fragment
			if n <= 0 || n > len(data) {
				n = len(data)
			}
			if _, err := w.Write(data[:n]); err != nil {
				t.Fatalf("Write: %v", err)
			}
			data = data[n:]
		}
		if err := w.Close(); err != nil {
			t.Fatalf("writer Close: %v", err)
		}
	}

	expect := func(want []byte) {
		t.Helper()
		mt, got, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if mt != TextMessage || !bytes.Equal(got, want) {
			t.Fatalf("message mismatch: got %q, want %q", got, want)
		}
	}

	for _, m := range similar {
		send(TextMessage, m, 0)
		expect(m)
	}

	// Empty message keeps the dictionary intact.
	send(TextMessage, nil, 0)
	expect(nil)

	// Fragmented message interleaved with a ping while fragments are in
	// flight.
	frag := append(append([]byte(nil), similar[0]...), []byte("-fragmented-tail")...)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = c.WriteControl(PingMessage, []byte("heartbeat"), time.Now().Add(time.Second))
	}()
	send(TextMessage, frag, 37)
	expect(frag)

	// A ping followed by another data message.
	if err := c.WriteControl(PingMessage, []byte("x"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("ping: %v", err)
	}
	send(TextMessage, similar[1], 0)
	expect(similar[1])

	if c.writeContextTakeover == nil || c.readContextTakeover == nil {
		t.Fatal("context takeover state lost during the connection")
	}
}

// rawPeer is an independent permessage-deflate implementation that speaks
// raw frames over a net.Conn without using the package's compression helpers.
type rawPeer struct {
	conn io.ReadWriteCloser
	fw   *flate.Writer
	out  bytes.Buffer
	hist []byte
	wire *captureWire
}

func newRawPeer(conn io.ReadWriteCloser) *rawPeer {
	p := &rawPeer{conn: conn}
	p.fw, _ = flate.NewWriter(&p.out, 1)
	p.wire = &captureWire{ReadWriteCloser: conn}
	p.conn = p.wire
	return p
}

// captureWire remembers the compressed payload lengths flowing in each
// direction.
type captureWire struct {
	io.ReadWriteCloser
	sentLen []int
	readLen []int
}

func (c *captureWire) Write(p []byte) (int, error) {
	if len(p) >= 2 && p[0]&rsv1Bit != 0 {
		n := int(p[1] & 0x7f)
		switch {
		case n == 126:
			n = int(p[2])<<8 | int(p[3])
		case n == 127:
			n = 0
			for _, x := range p[2:10] {
				n = n<<8 | int(x)
			}
		}
		c.sentLen = append(c.sentLen, n)
	}
	return c.ReadWriteCloser.Write(p)
}

func (p *rawPeer) sendCompressed(messageType int, data []byte) error {
	p.out.Reset()
	if _, err := p.fw.Write(data); err != nil {
		return err
	}
	if err := p.fw.Flush(); err != nil {
		return err
	}
	raw := append([]byte(nil), p.out.Bytes()...)
	return writeRawFrame(p.conn, messageType, raw[:len(raw)-4], true, true)
}

func (p *rawPeer) recvCompressed(messageType int) ([]byte, error) {
	op, payload, err := readRawFrame(p.conn)
	if err != nil {
		return nil, err
	}
	if op&0xf != byte(messageType) || op&rsv1Bit == 0 {
		return nil, errInvalidCompression
	}
	p.wire.readLen = append(p.wire.readLen, len(payload))
	tail := []byte{0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}
	fr := flate.NewReaderDict(io.MultiReader(bytes.NewReader(payload), bytes.NewReader(tail)), p.hist)
	data, err := io.ReadAll(fr)
	if err != nil {
		return nil, err
	}
	p.hist = append(p.hist, data...)
	if len(p.hist) > maxDeflateWindow {
		p.hist = p.hist[len(p.hist)-maxDeflateWindow:]
	}
	return data, nil
}

func writeRawFrame(w io.Writer, messageType int, payload []byte, masked bool, compressed bool) error {
	b0 := byte(messageType) | finalBit
	if compressed {
		b0 |= rsv1Bit
	}
	hdr := make([]byte, 0, 14)
	switch {
	case len(payload) <= 125:
		hdr = append(hdr, b0, byte(len(payload)))
	case len(payload) > 65535:
		hdr = append(hdr, b0, 127)
		for i := 7; i >= 0; i-- {
			hdr = append(hdr, byte(len(payload)>>(8*i)))
		}
	default:
		hdr = append(hdr, b0, 126, byte(len(payload)>>8), byte(len(payload)))
	}
	if masked {
		hdr[1] |= maskBit
		key := newMaskKey()
		hdr = append(hdr, key[:]...)
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readRawFrame(r io.Reader) (byte, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = int64(b[0])<<8 | int64(b[1])
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = 0
		for _, x := range b {
			n = n<<8 | int64(x)
		}
	}
	payload := make([]byte, n)
	if hdr[1]&maskBit != 0 {
		var key [4]byte
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return 0, nil, err
		}
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
		maskBytes(key, 0, payload)
		return hdr[0], payload, nil
	}
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// A fully independent peer must interoperate with the package in both
// directions when context takeover is negotiated.
func TestContextTakeoverIndependentPeer(t *testing.T) {
	msgs := append(similarMessages(),
		nil, // empty message
		[]byte("independent peer after an empty message"),
	)

	s := takeoverTestServer(t, true, func(c *Conn) {
		for i, want := range msgs {
			mt, data, err := c.ReadMessage()
			if err != nil {
				t.Errorf("server read %d: %v", i, err)
				return
			}
			if mt != TextMessage || !bytes.Equal(data, want) {
				t.Errorf("server message %d got %q want %q", i, data, want)
				return
			}
			if err := c.WriteMessage(TextMessage, want); err != nil {
				t.Errorf("server write %d: %v", i, err)
				return
			}
		}
	})

	c := dialTakeover(t, s.URL, true)
	peer := newRawPeer(c.NetConn())

	for i, want := range msgs {
		if err := peer.sendCompressed(TextMessage, want); err != nil {
			t.Fatalf("peer send %d: %v", i, err)
		}
		got, err := peer.recvCompressed(TextMessage)
		if err != nil {
			t.Fatalf("peer recv %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("peer message %d got %q want %q", i, got, want)
		}
	}

	// The similar (non-empty, 128+ byte) messages must compress far better
	// with a retained dictionary than with a fresh one, in both directions.
	// Message 0 establishes the dictionary; message 1 references it.
	if len(msgs) < 2 {
		t.Fatal("need at least two messages")
	}
	stateless := len(compressStateless(similarMessages()[:1])[0])
	check := func(name string, lens []int) {
		if lens[1] >= lens[0] || lens[1] >= stateless*9/10 {
			t.Fatalf("%s: no context-takeover gain: first=%d later=%d stateless=%d",
				name, lens[0], lens[1], stateless)
		}
	}
	check("client->server", peer.wire.sentLen)
	check("server->client", peer.wire.readLen)
	t.Logf("wire lengths client->server: %v, server->client: %v, stateless baseline: %d",
		peer.wire.sentLen, peer.wire.readLen, stateless)
}

// Asymmetric negotiation: client offers takeover but a server without
// takeover enabled responds with only server_no_context_takeover. The
// client -> server stream keeps a dictionary while the server -> client
// stream resets every message. Both directions must decode correctly.
func TestContextTakeoverAsymmetricDirections(t *testing.T) {
	msgs := similarMessages()

	s := takeoverTestServer(t, false, func(c *Conn) {
		// Server reads with a retained dictionary (client offered it),
		// writes without context (server configuration).
		if c.readContextTakeover == nil {
			t.Error("server did not retain incoming context")
		}
		if c.writeContextTakeover != nil {
			t.Error("server retained outgoing context without configuration")
		}
		for _, want := range msgs {
			mt, data, err := c.ReadMessage()
			if err != nil {
				t.Errorf("server read: %v", err)
				return
			}
			if mt != TextMessage || !bytes.Equal(data, want) {
				t.Errorf("server got %q want %q", data, want)
				return
			}
			if err := c.WriteMessage(TextMessage, want); err != nil {
				t.Errorf("server write: %v", err)
				return
			}
		}
	})

	c := dialTakeover(t, s.URL, true)
	if c.writeContextTakeover == nil {
		t.Fatal("client did not retain outgoing context")
	}
	if c.readContextTakeover != nil {
		t.Fatal("client retained incoming context despite server_no_context_takeover")
	}

	for _, want := range msgs {
		if err := c.WriteMessage(TextMessage, want); err != nil {
			t.Fatalf("client write: %v", err)
		}
		mt, got, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
		if mt != TextMessage || !bytes.Equal(got, want) {
			t.Fatalf("client got %q want %q", got, want)
		}
	}
}

// A connection that has built up a retained dictionary must not affect a
// later connection to the same server. Each handshake creates a brand new
// connection state; the second connection must decode as if the first never
// existed, and both sides must report independent takeover contexts.
func TestContextTakeoverNoStateLeakAcrossConnections(t *testing.T) {
	u := cstUpgrader
	u.EnableContextTakeover = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer c.Close()
		if c.writeContextTakeover == nil || c.readContextTakeover == nil {
			t.Error("server connection missing fresh takeover state")
		}
		// A fresh connection must start with an empty receive dictionary.
		if len(c.readContextTakeover.hist) != 0 {
			t.Error("server connection inherited decompression history")
		}
		for range similarMessages() {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	useConnection := func() {
		d := cstDialer
		d.EnableCompression = true
		d.EnableContextTakeover = true
		d.HandshakeTimeout = 10 * time.Second
		c, _, err := d.DialContext(context.Background(), makeWsProto(srv.URL), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if c.writeContextTakeover == nil || c.readContextTakeover == nil {
			t.Fatal("client connection missing fresh takeover state")
		}
		if len(c.readContextTakeover.hist) != 0 {
			t.Fatal("client connection inherited decompression history")
		}
		// Populate dictionaries with several context-dependent messages.
		for i, m := range similarMessages() {
			if err := c.WriteMessage(TextMessage, m); err != nil {
				t.Fatalf("write %d: %v", i, err)
			}
			mt, got, err := c.ReadMessage()
			if err != nil || mt != TextMessage || !bytes.Equal(got, m) {
				t.Fatalf("read %d: %v", i, err)
			}
		}
	}

	useConnection()
	useConnection()
}
