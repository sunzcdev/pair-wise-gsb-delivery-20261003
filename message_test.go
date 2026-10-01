// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

func testMsgConf() *Config {
	conf := testConfNoKeepAlive()
	conf.EnableMessageChannel = true
	conf.MessageChannelQueueSize = 8
	conf.MaxMessageSize = 1024
	return conf
}

func testMessageChannelPair(t *testing.T, clientConf, serverConf *Config) (*MessageChannel, *MessageChannel) {
	clientConn, serverConn := testConnTLS(t)
	client, server := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	clientCh, err := client.MessageChannel()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	serverCh, err := server.MessageChannel()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clientCh.WaitReady(ctx); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := serverCh.WaitReady(ctx); err != nil {
		t.Fatalf("err: %v", err)
	}
	return clientCh, serverCh
}

func waitForCondition(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMessageChannel_Disabled(t *testing.T) {
	client, server := testClientServer(t)

	if _, err := client.MessageChannel(); err != ErrMessageChannelDisabled {
		t.Fatalf("bad: %v", err)
	}
	if _, err := server.MessageChannel(); err != ErrMessageChannelDisabled {
		t.Fatalf("bad: %v", err)
	}
}

func TestMessageChannel_Basic(t *testing.T) {
	clientCh, serverCh := testMessageChannelPair(t, testMsgConf(), testMsgConf())

	maxSize := int(clientCh.MaxMessageSize())
	if maxSize != 1024 {
		t.Fatalf("bad: %d", maxSize)
	}

	full := make([]byte, maxSize)
	for i := range full {
		full[i] = byte(i)
	}
	msgs := [][]byte{
		[]byte("hello"),
		{}, // empty message is delivered as an empty message
		full,
	}
	for _, msg := range msgs {
		if err := clientCh.SendMessage(msg); err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Each message must arrive whole, one per read, in order
	for i, want := range msgs {
		got, err := serverCh.ReceiveMessage()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("bad message %d: got %d bytes, want %d", i, len(got), len(want))
		}
	}

	// And in the other direction
	if err := serverCh.SendMessage([]byte("world")); err != nil {
		t.Fatalf("err: %v", err)
	}
	got, err := clientCh.ReceiveMessage()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(got) != "world" {
		t.Fatalf("bad: %q", got)
	}

	if clientCh.Dropped() != 0 || serverCh.Dropped() != 0 {
		t.Fatalf("bad: dropped %d %d", clientCh.Dropped(), serverCh.Dropped())
	}
}

func TestMessageChannel_NegotiatedMaxSize(t *testing.T) {
	clientConf := testMsgConf()
	clientConf.MaxMessageSize = 512
	serverConf := testMsgConf()
	serverConf.MaxMessageSize = 1024

	clientCh, serverCh := testMessageChannelPair(t, clientConf, serverConf)

	// Both sides must honor the smaller of the two limits
	if got := clientCh.MaxMessageSize(); got != 512 {
		t.Fatalf("bad: %d", got)
	}
	if got := serverCh.MaxMessageSize(); got != 512 {
		t.Fatalf("bad: %d", got)
	}

	if err := clientCh.SendMessage(make([]byte, 513)); err != ErrMessageTooLarge {
		t.Fatalf("bad: %v", err)
	}
	if err := serverCh.SendMessage(make([]byte, 513)); err != ErrMessageTooLarge {
		t.Fatalf("bad: %v", err)
	}

	// A message at the negotiated limit still goes through
	if err := clientCh.SendMessage(make([]byte, 512)); err != nil {
		t.Fatalf("err: %v", err)
	}
	got, err := serverCh.ReceiveMessage()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 512 {
		t.Fatalf("bad: %d", len(got))
	}
}

func TestMessageChannel_DropAndResume(t *testing.T) {
	clientConn, serverConn := testConnTLS(t)
	client, server := testClientServerConfig(t, clientConn, serverConn, testMsgConf(), testMsgConf())

	clientCh, err := client.MessageChannel()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	serverCh, err := server.MessageChannel()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clientCh.WaitReady(ctx); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := serverCh.WaitReady(ctx); err != nil {
		t.Fatalf("err: %v", err)
	}

	// The receiver (server) does not read for a while. Send more
	// messages than the queue can hold.
	const total = 20
	const queueSize = 8
	for i := 0; i < total; i++ {
		msg := make([]byte, 4)
		binary.BigEndian.PutUint32(msg, uint32(i))
		if err := clientCh.SendMessage(msg); err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// The oldest messages are dropped locally at the receiver
	waitForCondition(t, "drops", func() bool {
		return serverCh.Dropped() == total-queueSize
	})

	// While the receiver is still not reading messages, control frames
	// and normal streams must keep making progress.
	if _, err := client.Ping(); err != nil {
		t.Fatalf("err: %v", err)
	}

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer stream.Close()

	accepted, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer accepted.Close()

	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("err: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(accepted, buf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("bad: %q", buf)
	}

	// Resume reading. The queued messages must be the most recent ones,
	// each delivered whole with its boundary intact.
	for i := total - queueSize; i < total; i++ {
		msg, err := serverCh.ReceiveMessage()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(msg) != 4 {
			t.Fatalf("bad message length: %d", len(msg))
		}
		if got := binary.BigEndian.Uint32(msg); got != uint32(i) {
			t.Fatalf("bad: got %d, want %d", got, i)
		}
	}

	// No further drops happened while draining
	if got := serverCh.Dropped(); got != total-queueSize {
		t.Fatalf("bad: %d", got)
	}

	// The channel keeps working afterwards
	if err := clientCh.SendMessage([]byte("after")); err != nil {
		t.Fatalf("err: %v", err)
	}
	msg, err := serverCh.ReceiveMessage()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(msg) != "after" {
		t.Fatalf("bad: %q", msg)
	}
}

func TestMessageChannel_SessionClose(t *testing.T) {
	clientConn, serverConn := testConnTLS(t)
	client, server := testClientServerConfig(t, clientConn, serverConn, testMsgConf(), testMsgConf())

	serverCh, err := server.MessageChannel()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("err: %v", err)
	}

	waitForCondition(t, "server shutdown", server.IsClosed)
	if _, err := serverCh.ReceiveMessage(); err == nil {
		t.Fatalf("expected error")
	}
	if err := serverCh.SendMessage([]byte("x")); !errors.Is(err, ErrSessionShutdown) {
		t.Fatalf("bad: %v", err)
	}
}
