// Copyright 2026 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"unicode/utf8"
)

// writeTestFrame writes a single masked client frame with a small payload to
// w.
func writeTestFrame(w io.Writer, final bool, frameType int, payload []byte) {
	if len(payload) > maxControlFramePayloadSize {
		panic("payload too large for test helper")
	}
	b0 := byte(frameType)
	if final {
		b0 |= finalBit
	}
	key := newMaskKey()
	if _, err := w.Write([]byte{b0, maskBit | byte(len(payload))}); err != nil {
		panic(err)
	}
	if _, err := w.Write(key[:]); err != nil {
		panic(err)
	}
	masked := make([]byte, len(payload))
	copy(masked, payload)
	maskBytes(key, 0, masked)
	if _, err := w.Write(masked); err != nil {
		panic(err)
	}
}

// readCloseCode reads a single unmasked server close frame from r and returns
// the close code.
func readCloseCode(t *testing.T, r io.Reader) int {
	t.Helper()
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		t.Fatalf("reading close frame header: %v", err)
	}
	if opcode := int(header[0] & 0xf); opcode != CloseMessage {
		t.Fatalf("expected close frame, got opcode %d", opcode)
	}
	payload := make([]byte, int(header[1]&0x7f))
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("reading close frame payload: %v", err)
	}
	if len(payload) < 2 {
		t.Fatalf("close frame payload too short: %d", len(payload))
	}
	return int(binary.BigEndian.Uint16(payload))
}

func TestStrictTextReadValidFragmented(t *testing.T) {
	var connBuf, serverW bytes.Buffer
	rc := newTestConn(&connBuf, &serverW, true)
	rc.EnableStrictTextRead(true)

	// "he" + U+4E2D + "llo", with the three bytes of U+4E2D split across two
	// frames and a ping control frame in between.
	writeTestFrame(&connBuf, false, TextMessage, []byte("he\xe4"))
	writeTestFrame(&connBuf, true, PingMessage, []byte("ping"))
	writeTestFrame(&connBuf, true, continuationFrame, []byte("\xb8\xadllo"))

	messageType, p, err := rc.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage returned %v", err)
	}
	if messageType != TextMessage {
		t.Fatalf("messageType = %d, want %d", messageType, TextMessage)
	}
	if want := "he\xe4\xb8\xadllo"; string(p) != want {
		t.Fatalf("message = %q, want %q", p, want)
	}

	// The ping should have been answered with a pong.
	header := make([]byte, 2)
	if _, err := io.ReadFull(&serverW, header); err != nil {
		t.Fatalf("reading pong frame header: %v", err)
	}
	if opcode := int(header[0] & 0xf); opcode != PongMessage {
		t.Fatalf("expected pong frame, got opcode %d", opcode)
	}
}

func TestStrictTextReadInvalid(t *testing.T) {
	invalidPayloads := []struct {
		name    string
		payload []byte
	}{
		{"overlong two byte", []byte{0xc0, 0x80}},
		{"overlong three byte", []byte{0xe0, 0x80, 0x80}},
		{"overlong four byte", []byte{0xf0, 0x80, 0x80, 0x80}},
		{"surrogate", []byte{0xed, 0xa0, 0x80}},
		{"out of range", []byte{0xf4, 0x90, 0x80, 0x80}},
		{"invalid byte", []byte{0xff}},
		{"truncated at fin", []byte{0xe4, 0xb8}},
		{"lone continuation", []byte{0x80}},
	}
	for _, tc := range invalidPayloads {
		t.Run(tc.name, func(t *testing.T) {
			var connBuf, serverW bytes.Buffer
			rc := newTestConn(&connBuf, &serverW, true)
			rc.EnableStrictTextRead(true)

			writeTestFrame(&connBuf, true, TextMessage, tc.payload)

			_, _, err := rc.ReadMessage()
			if !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("ReadMessage returned %v, want ErrInvalidUTF8", err)
			}
			if code := readCloseCode(t, &serverW); code != CloseInvalidFramePayloadData {
				t.Fatalf("close code = %d, want %d", code, CloseInvalidFramePayloadData)
			}
		})
	}
}

func TestStrictTextReadInvalidFragmented(t *testing.T) {
	var connBuf, serverW bytes.Buffer
	rc := newTestConn(&connBuf, &serverW, true)
	rc.EnableStrictTextRead(true)

	// Invalid encoding split across two frames.
	writeTestFrame(&connBuf, false, TextMessage, []byte{0xed})
	writeTestFrame(&connBuf, true, continuationFrame, []byte{0xa0, 0x80})

	_, _, err := rc.ReadMessage()
	if !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("ReadMessage returned %v, want ErrInvalidUTF8", err)
	}
	if code := readCloseCode(t, &serverW); code != CloseInvalidFramePayloadData {
		t.Fatalf("close code = %d, want %d", code, CloseInvalidFramePayloadData)
	}
}

func TestStrictTextReadBinaryNotValidated(t *testing.T) {
	var connBuf, serverW bytes.Buffer
	rc := newTestConn(&connBuf, &serverW, true)
	rc.EnableStrictTextRead(true)

	payload := []byte{0xff, 0xfe, 0xc0, 0x80}
	writeTestFrame(&connBuf, true, BinaryMessage, payload)

	messageType, p, err := rc.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage returned %v", err)
	}
	if messageType != BinaryMessage {
		t.Fatalf("messageType = %d, want %d", messageType, BinaryMessage)
	}
	if !bytes.Equal(p, payload) {
		t.Fatalf("message = %v, want %v", p, payload)
	}
}

func TestStrictTextReadDisabled(t *testing.T) {
	var connBuf, serverW bytes.Buffer
	rc := newTestConn(&connBuf, &serverW, true)

	payload := []byte{0xff, 0xfe}
	writeTestFrame(&connBuf, true, TextMessage, payload)

	_, p, err := rc.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage returned %v", err)
	}
	if !bytes.Equal(p, payload) {
		t.Fatalf("message = %v, want %v", p, payload)
	}
}

func TestStrictTextReadCompressed(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
		valid   bool
	}{
		{"valid", []byte("hello \xe4\xb8\x96\xe7\x95\x8c"), true},
		{"invalid", []byte("hello \xed\xa0\x80"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var connBuf, serverW bytes.Buffer
			wc := newTestConn(nil, &connBuf, false)
			wc.newCompressionWriter = compressNoContextTakeover
			rc := newTestConn(&connBuf, &serverW, true)
			rc.newDecompressionReader = decompressNoContextTakeover
			rc.EnableStrictTextRead(true)

			if err := wc.WriteMessage(TextMessage, test.payload); err != nil {
				t.Fatalf("WriteMessage returned %v", err)
			}

			_, p, err := rc.ReadMessage()
			if test.valid {
				if err != nil {
					t.Fatalf("ReadMessage returned %v", err)
				}
				if !bytes.Equal(p, test.payload) {
					t.Fatalf("message = %q, want %q", p, test.payload)
				}
			} else {
				if !errors.Is(err, ErrInvalidUTF8) {
					t.Fatalf("ReadMessage returned %v, want ErrInvalidUTF8", err)
				}
				if code := readCloseCode(t, &serverW); code != CloseInvalidFramePayloadData {
					t.Fatalf("close code = %d, want %d", code, CloseInvalidFramePayloadData)
				}
			}
		})
	}
}

func TestUTF8Decode(t *testing.T) {
	check := func(input []byte) {
		t.Helper()
		state, x := utf8Accept, rune(0)
		for _, b := range input {
			state, x = utf8Decode(state, x, b)
		}
		got := state == utf8Accept
		if want := utf8.Valid(input); got != want {
			t.Errorf("utf8Decode(%x) valid = %v, want %v", input, got, want)
		}
	}

	// Targeted cases.
	for _, input := range [][]byte{
		[]byte("hello \xe4\xb8\x96\xe7\x95\x8c"),
		{0xc0, 0x80},             // overlong
		{0xe0, 0x80, 0x80},       // overlong
		{0xf0, 0x80, 0x80, 0x80}, // overlong
		{0xed, 0xa0, 0x80},       // surrogate
		{0xed, 0xbf, 0xbf},       // surrogate
		{0xf4, 0x90, 0x80, 0x80}, // > U+10FFFF
		{0xe4, 0xb8},             // truncated
		{0xf0, 0x9f, 0x98, 0x80}, // emoji
		{0xef, 0xbf, 0xbd},       // U+FFFD
	} {
		check(input)
	}

	// Exhaustive one and two byte sequences.
	for b0 := 0; b0 < 256; b0++ {
		check([]byte{byte(b0)})
		for b1 := 0; b1 < 256; b1++ {
			check([]byte{byte(b0), byte(b1)})
		}
	}

	// Pseudo-random longer sequences with a deterministic seed.
	seed := uint64(1)
	next := func() byte {
		seed = seed*6364136223846793005 + 1442695040888963407
		return byte(seed >> 33)
	}
	for i := 0; i < 100000; i++ {
		input := make([]byte, 3+int(next())%6)
		for j := range input {
			input[j] = next()
		}
		check(input)
	}
}
