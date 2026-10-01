// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func shortTestConf() *Config {
	conf := testConfNoKeepAlive()
	conf.EnableShortMessages = true
	conf.MaxShortMessageSize = 64
	conf.ShortMessageBacklog = 4
	conf.StreamOpenTimeout = time.Second
	conf.ConnectionWriteTimeout = time.Second
	return conf
}

func testShortClientServer(t testing.TB) (*Session, *Session) {
	clientConn, serverConn := testConnTCP(t)
	return testClientServerConfig(t, clientConn, serverConn, shortTestConf(), shortTestConf())
}

func TestShortMessageBoundary(t *testing.T) {
	client, server := testShortClientServer(t)

	messages := [][]byte{
		[]byte(""),
		[]byte("a"),
		[]byte("bc"),
		[]byte("third message"),
	}
	for _, message := range messages {
		if _, err := client.SendShortMessage(message); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	for _, want := range messages {
		message, dropped, err := server.RecvShortMessage()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if dropped != 0 {
			t.Fatalf("unexpected dropped count: %d", dropped)
		}
		if string(message) != string(want) {
			t.Fatalf("message = %q, want %q", message, want)
		}
	}
}

func TestShortMessageReceiveDropsAndControlProgress(t *testing.T) {
	conf := shortTestConf()
	clientConn, serverConn := testConnTCP(t)
	client, server := testClientServerConfig(t, clientConn, serverConn, conf, conf.Clone())

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	stopSending := make(chan struct{})
	sendingDone := make(chan struct{})
	go func() {
		defer close(sendingDone)
		for i := 0; ; i++ {
			select {
			case <-stopSending:
				return
			default:
			}
			message := []byte(fmt.Sprintf("%032d", i))
			if _, err := client.SendShortMessage(message); err != nil {
				return
			}
		}
	}()

	pingErr := make(chan error, 1)
	go func() {
		_, err := client.Ping()
		pingErr <- err
	}()

	select {
	case err := <-pingErr:
		if err != nil {
			t.Fatalf("ping during short-message backlog: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("control frame did not progress while receiver stopped reading")
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadUint64(&server.shortRecvDrops) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if atomic.LoadUint64(&server.shortRecvDrops) == 0 {
		t.Fatalf("receiver did not observe local queue drops")
	}
	close(stopSending)
	<-sendingDone

	const marker = "marker"
	if _, err := client.SendShortMessage([]byte(marker)); err != nil {
		t.Fatalf("send marker: %v", err)
	}

	message, firstDrops, err := server.RecvShortMessage()
	if err != nil {
		t.Fatalf("first recv: %v", err)
	}
	if firstDrops == 0 {
		t.Fatalf("expected local receive drops")
	}
	if len(message) == 0 {
		t.Fatalf("message was split or truncated: %q", message)
	}

	for {
		message, dropped, err := server.RecvShortMessage()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if len(message) == 0 {
			t.Fatalf("message was split or truncated: %q", message)
		}
		firstDrops += dropped
		if string(message) == marker {
			break
		}
	}
	if firstDrops == 0 {
		t.Fatalf("RecvShortMessage did not report local drops")
	}

	if _, err := stream.Write([]byte("still reliable")); err != nil {
		t.Fatalf("stream write: %v", err)
	}
	accepted, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	buffer := make([]byte, 64)
	n, err := accepted.Read(buffer)
	if err != nil && err != io.EOF {
		t.Fatalf("stream read: %v", err)
	}
	if string(buffer[:n]) != "still reliable" {
		t.Fatalf("stream data = %q", buffer[:n])
	}
}

func TestShortMessageDisabledByDefault(t *testing.T) {
	clientConn, serverConn := testConnTCP(t)
	client, _ := testClientServerConfig(
		t, clientConn, serverConn, testConfNoKeepAlive(), testConfNoKeepAlive())

	_, err := client.SendShortMessage(nil)
	if err != ErrShortMessagesUnsupported {
		t.Fatalf("error = %v, want %v", err, ErrShortMessagesUnsupported)
	}
}

func TestShortMessagePeerUnsupported(t *testing.T) {
	clientConn, serverConn := testConnTCP(t)
	client, _ := testClientServerConfig(
		t, clientConn, serverConn, shortTestConf(), testConfNoKeepAlive())

	done := make(chan error, 1)
	go func() {
		_, err := client.SendShortMessage(nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != ErrShortMessagesUnsupported {
			t.Fatalf("error = %v, want %v", err, ErrShortMessagesUnsupported)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("peer capability negotiation did not fail")
	}
}

func TestShortMessageMaxSize(t *testing.T) {
	client, _ := testShortClientServer(t)

	_, err := client.SendShortMessage(make([]byte, 65))
	if err != ErrShortMessageTooLarge {
		t.Fatalf("error = %v, want %v", err, ErrShortMessageTooLarge)
	}
}

type blockedReadConn struct {
	io.ReadWriteCloser

	mu        sync.Mutex
	blocked   bool
	unblockCh chan struct{}
}

func (c *blockedReadConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	blocked := c.blocked
	unblockCh := c.unblockCh
	c.mu.Unlock()
	if blocked {
		<-unblockCh
	}
	return c.ReadWriteCloser.Read(b)
}

func (c *blockedReadConn) setBlocked(blocked bool) {
	c.mu.Lock()
	if c.blocked == blocked {
		c.mu.Unlock()
		return
	}
	c.blocked = blocked
	if blocked {
		c.unblockCh = make(chan struct{})
	} else {
		close(c.unblockCh)
	}
	c.mu.Unlock()
}

func TestShortMessageSendDrops(t *testing.T) {
	clientPipe, serverPipe := testConnPipe(t)
	serverConn := &blockedReadConn{ReadWriteCloser: serverPipe, unblockCh: make(chan struct{})}
	conf := shortTestConf()
	client, server := testClientServerConfig(t, clientPipe, serverConn, conf, conf.Clone())

	if _, err := client.SendShortMessage([]byte("ready")); err != nil {
		t.Fatalf("ready send: %v", err)
	}
	if _, _, err := server.RecvShortMessage(); err != nil {
		t.Fatalf("ready recv: %v", err)
	}

	serverConn.setBlocked(true)
	time.Sleep(50 * time.Millisecond)

	var dropped uint64
	deadline := time.Now().Add(2 * time.Second)
	for dropped == 0 && time.Now().Before(deadline) {
		dropped, _ = client.SendShortMessage([]byte("fill"))
	}
	if dropped == 0 {
		t.Fatalf("sender did not report local send-queue drops")
	}

	serverConn.setBlocked(false)
	if _, err := client.SendShortMessage([]byte("marker")); err != nil {
		t.Fatalf("marker send: %v", err)
	}
	for {
		message, _, err := server.RecvShortMessage()
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if string(message) == "marker" {
			return
		}
	}
}
