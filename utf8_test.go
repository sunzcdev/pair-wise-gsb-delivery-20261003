// Copyright 2013 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// clientFrame returns a masked frame as sent by a client to a server.
func clientFrame(opcode int, fin bool, payload []byte) []byte {
	b0 := byte(opcode)
	if fin {
		b0 |= finalBit
	}
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	b := []byte{b0, maskBit | byte(len(payload))}
	b = append(b, key[:]...)
	for i, c := range payload {
		b = append(b, c^key[i%4])
	}
	return b
}

// checkClose1007 verifies that the server's output starts with a close frame
// carrying code CloseInvalidFramePayloadData.
func checkClose1007(t *testing.T, out *bytes.Buffer) {
	t.Helper()
	p := out.Bytes()
	if len(p) < 4 {
		t.Fatalf("no close frame written, got %v", p)
	}
	if p[0] != CloseMessage|finalBit {
		t.Fatalf("first frame is not a close frame: %08b", p[0])
	}
	if code := int(binary.BigEndian.Uint16(p[2:])); code != CloseInvalidFramePayloadData {
		t.Fatalf("close code = %d, want %d", code, CloseInvalidFramePayloadData)
	}
}

func newStrictServer(in *bytes.Buffer) (*Conn, *bytes.Buffer) {
	out := &bytes.Buffer{}
	c := newTestConn(in, out, true)
	c.strictTextValidation = true
	return c, out
}

func TestStrictTextValidationFragmented(t *testing.T) {
	// A multi-byte rune split across frames with a control frame in between
	// is validated as a single text message.
	text := []byte("hello 世界!")
	var in bytes.Buffer
	in.Write(clientFrame(TextMessage, false, text[:7]))      // "hello " + first byte of 世
	in.Write(clientFrame(PingMessage, true, []byte("ping"))) // interleaved control frame
	in.Write(clientFrame(continuationFrame, false, text[7:8]))
	in.Write(clientFrame(continuationFrame, true, text[8:])) // rest of 世, 界 and "!"

	c, _ := newStrictServer(&in)
	mt, p, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if mt != TextMessage {
		t.Fatalf("message type = %d, want %d", mt, TextMessage)
	}
	if !bytes.Equal(p, text) {
		t.Fatalf("message = %q, want %q", p, text)
	}
}

func TestStrictTextValidationInvalid(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"overlong 2-byte", []byte{0xc0, 0xaf}},             // overlong encoding of '/'
		{"overlong 3-byte", []byte{0xe0, 0x80, 0xaf}},       // overlong encoding of '/'
		{"overlong 4-byte", []byte{0xf0, 0x80, 0x80, 0xaf}}, // overlong encoding of '/'
		{"surrogate", []byte{0xed, 0xa0, 0x80}},             // U+D800
		{"surrogate max", []byte{0xed, 0xbf, 0xbf}},         // U+DFFF
		{"out of range", []byte{0xf4, 0x90, 0x80, 0x80}},    // U+110000
		{"lone continuation", []byte{0x80}},                 //
		{"truncated at FIN", []byte("abc\xe4\xb8")},         // 世 missing a byte at FIN
		{"truncated rune at FIN", []byte{0xe4}},             // first byte of 世 only
		{"split invalid", []byte("ok\xe4\x28")},             // bad continuation byte
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in bytes.Buffer
			in.Write(clientFrame(TextMessage, true, tt.data))
			c, out := newStrictServer(&in)
			_, _, err := c.ReadMessage()
			if !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("ReadMessage error = %v, want %v", err, ErrInvalidUTF8)
			}
			checkClose1007(t, out)
			// The error is permanent.
			if _, _, err := c.NextReader(); !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("NextReader error = %v, want %v", err, ErrInvalidUTF8)
			}
		})
	}
}

func TestStrictTextValidationInvalidAcrossFrames(t *testing.T) {
	// An invalid sequence assembled from multiple frames is rejected.
	var in bytes.Buffer
	in.Write(clientFrame(TextMessage, false, []byte{0xed}))
	in.Write(clientFrame(continuationFrame, true, []byte{0xa0, 0x80})) // surrogate halves
	c, out := newStrictServer(&in)
	if _, _, err := c.ReadMessage(); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("ReadMessage error = %v, want %v", err, ErrInvalidUTF8)
	}
	checkClose1007(t, out)
}

func TestStrictTextValidationBinaryIgnored(t *testing.T) {
	// Binary messages are not validated.
	data := []byte{0xc0, 0xaf, 0xed, 0xa0, 0x80}
	var in bytes.Buffer
	in.Write(clientFrame(BinaryMessage, true, data))
	c, out := newStrictServer(&in)
	mt, p, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if mt != BinaryMessage || !bytes.Equal(p, data) {
		t.Fatalf("message = %d, %v; want %d, %v", mt, p, BinaryMessage, data)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected output: %v", out.Bytes())
	}
}

func TestStrictTextValidationDisabled(t *testing.T) {
	// Without the strict mode, invalid UTF-8 passes through.
	data := []byte{0xc0, 0xaf}
	var in bytes.Buffer
	in.Write(clientFrame(TextMessage, true, data))
	c := newTestConn(&in, &bytes.Buffer{}, true)
	_, p, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if !bytes.Equal(p, data) {
		t.Fatalf("message = %v, want %v", p, data)
	}
}

func TestStrictTextValidationCompressed(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("hello 世界!"),
		[]byte{0xed, 0xa0, 0x80},     // surrogate
		[]byte("truncated \xe4\xb8"), // incomplete rune at FIN
	} {
		// Client writes a compressed message.
		var in bytes.Buffer
		wc := newTestConn(nil, &in, false)
		wc.newCompressionWriter = compressNoContextTakeover
		wc.enableWriteCompression = true
		if err := wc.WriteMessage(TextMessage, data); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}

		out := &bytes.Buffer{}
		c := newTestConn(&in, out, true)
		c.newDecompressionReader = decompressNoContextTakeover
		c.strictTextValidation = true

		_, p, err := c.ReadMessage()
		if errors.Is(err, ErrInvalidUTF8) {
			checkClose1007(t, out)
			continue
		}
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if !bytes.Equal(p, data) {
			t.Fatalf("message = %q, want %q", p, data)
		}
	}
}
