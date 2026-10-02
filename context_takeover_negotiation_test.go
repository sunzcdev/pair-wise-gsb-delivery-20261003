// Copyright 2026 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type negotiationExpectation struct {
	clientTakeover bool
	serverTakeover bool

	// client side view
	clientWriteTO bool // client -> server retained
	clientReadTO  bool // server -> client retained

	wantClientErr bool
}

// RFC 7692 negotiation matrix. The client offers takeover by omitting both
// no_context_takeover parameters. The server accepts only what its
// configuration allows and echoes each parameter back independently.
func TestContextTakeoverNegotiationMatrix(t *testing.T) {
	cases := []negotiationExpectation{
		{false, false, false, false, false},
		// Client offers takeover, server declines only its own outgoing
		// direction: client -> server retains, server -> client resets.
		{true, false, true, false, false},  // clientWriteTO=true, clientReadTO=false
		{false, true, false, false, false}, // server cannot grant what the client did not offer
		{true, true, true, true, false},
	}

	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			serverConnCh := make(chan *Conn, 1)
			u := cstUpgrader
			u.EnableContextTakeover = tc.serverTakeover
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := u.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				serverConnCh <- c
				_, _, _ = c.ReadMessage()
				c.Close()
			}))
			defer srv.Close()

			d := cstDialer
			d.EnableCompression = true
			d.EnableContextTakeover = tc.clientTakeover
			d.HandshakeTimeout = 10 * time.Second
			c, _, err := d.DialContext(context.Background(), makeWsProto(srv.URL), nil)
			if tc.wantClientErr {
				if err == nil {
					c.Close()
					t.Fatal("expected handshake error")
				}
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()

			if (c.writeContextTakeover != nil) != tc.clientWriteTO {
				t.Errorf("client write takeover = %v, want %v", c.writeContextTakeover != nil, tc.clientWriteTO)
			}
			if (c.readContextTakeover != nil) != tc.clientReadTO {
				t.Errorf("client read takeover = %v, want %v", c.readContextTakeover != nil, tc.clientReadTO)
			}
			if c.newCompressionWriter == nil || c.newDecompressionReader == nil {
				t.Fatal("compression not negotiated at all")
			}

			var serverConn *Conn
			select {
			case serverConn = <-serverConnCh:
			case <-time.After(2 * time.Second):
				t.Fatal("server did not complete handshake")
			}

			// Server directions are the mirror image of the client's.
			serverWriteTO := tc.clientReadTO
			serverReadTO := tc.clientWriteTO
			if (serverConn.writeContextTakeover != nil) != serverWriteTO {
				t.Errorf("server write takeover = %v, want %v", serverConn.writeContextTakeover != nil, serverWriteTO)
			}
			if (serverConn.readContextTakeover != nil) != serverReadTO {
				t.Errorf("server read takeover = %v, want %v", serverConn.readContextTakeover != nil, serverReadTO)
			}
		})
	}
}

// Default configuration must keep the original wire behavior: the client
// offers both no_context_takeover parameters and the server echoes both.
func TestContextTakeoverDefaultUnchanged(t *testing.T) {
	seen := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Sec-WebSocket-Extensions")
		u := cstUpgrader // EnableContextTakeover defaults to false.
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		c.Close()
	}))
	defer srv.Close()

	const want = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"

	// Raw handshake: inspect the 101 response header directly.
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	if _, err := fmt.Fprintf(nc,
		"GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n"+
			"Sec-WebSocket-Extensions: %s\r\n\r\n", u.Host, key, want); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(nc), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Sec-WebSocket-Extensions"); got != want {
		t.Errorf("response extensions = %q, want %q", got, want)
	}
	if got := <-seen; got != want {
		t.Errorf("request extensions = %q, want %q", got, want)
	}

	// Package dialer with defaults: no takeover on either side.
	d := cstDialer
	d.EnableCompression = true
	d.HandshakeTimeout = 10 * time.Second
	c, _, err := d.DialContext(context.Background(), makeWsProto(srv.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if got := <-seen; got != want {
		t.Errorf("dialer request extensions = %q, want %q", got, want)
	}
	if c.writeContextTakeover != nil || c.readContextTakeover != nil {
		t.Fatal("takeover enabled by default")
	}
}

// A server that does not support takeover must keep its own outgoing context
// stateless (server_no_context_takeover) even when the client offers context
// takeover. The client -> server direction may still retain a dictionary
// because the server is prepared to decompress it. The client must honor the
// server's response instead of assuming takeover in both directions.
func TestContextTakeoverDowngradedByServer(t *testing.T) {
	serverConnCh := make(chan *Conn, 1)
	u := cstUpgrader
	u.EnableContextTakeover = false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConnCh <- c
		if c.writeContextTakeover != nil {
			t.Error("server retained outgoing context despite configuration")
		}
		c.Close()
	}))
	defer srv.Close()

	d := cstDialer
	d.EnableCompression = true
	d.EnableContextTakeover = true
	c, _, err := d.DialContext(context.Background(), makeWsProto(srv.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if c.readContextTakeover != nil {
		t.Fatal("client retained outgoing context after server demanded server_no_context_takeover")
	}
	if c.writeContextTakeover == nil {
		t.Fatal("client dropped outgoing takeover that the server accepted")
	}
	serverConn := <-serverConnCh
	if serverConn.writeContextTakeover != nil {
		t.Fatal("server retained outgoing context despite configuration")
	}
	if serverConn.readContextTakeover == nil {
		t.Fatal("server dropped incoming takeover that the client offered")
	}
}
