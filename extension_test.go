package ws

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/gobwas/httphead"
)

func TestCheckExtensionConflicts(t *testing.T) {
	ext := func(name string, rsv byte) NegotiatedExtension {
		return NegotiatedExtension{
			Option: httphead.Option{Name: []byte(name)},
			Rsv:    rsv,
		}
	}
	for _, test := range []struct {
		name  string
		exts  []NegotiatedExtension
		check func(*testing.T, error)
	}{
		{
			name: "disjoint bits",
			exts: []NegotiatedExtension{
				ext("alpha", RsvBit1),
				ext("beta", RsvBit2|RsvBit3),
			},
			check: func(t *testing.T, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			},
		},
		{
			name: "no bits",
			exts: []NegotiatedExtension{
				ext("alpha", 0),
				ext("beta", 0),
			},
			check: func(t *testing.T, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			},
		},
		{
			name: "conflict",
			exts: []NegotiatedExtension{
				ext("alpha", RsvBit1),
				ext("beta", RsvBit1),
			},
			check: func(t *testing.T, err error) {
				var conflict *ExtensionConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("want *ExtensionConflictError, got %T: %v", err, err)
				}
				if !errors.Is(err, ErrExtensionConflict) {
					t.Fatalf("error must wrap ErrExtensionConflict: %v", err)
				}
				if conflict.Name != "beta" || conflict.Claimed != "alpha" ||
					conflict.Bits != RsvBit1 {
					t.Fatalf("unexpected conflict: %+v", conflict)
				}
				msg := err.Error()
				for _, want := range []string{"alpha", "beta", "RSV1"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("error %q must mention %q", msg, want)
					}
				}
			},
		},
		{
			name: "invalid bit",
			exts: []NegotiatedExtension{
				ext("alpha", 0x08),
			},
			check: func(t *testing.T, err error) {
				if err == nil {
					t.Fatalf("want error for invalid rsv bits")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.check(t, CheckExtensionConflicts(test.exts))
		})
	}
}

func rawUpgradeRequest(extensions string) []byte {
	lines := []string{
		"GET /ws HTTP/1.1",
		"Host: example.org",
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Version: 13",
		"Sec-WebSocket-Key: " + string(mustMakeNonce()),
	}
	if extensions != "" {
		lines = append(lines, headerSecExtensions+": "+extensions)
	}
	lines = append(lines, "", "")
	return []byte(strings.Join(lines, "\r\n"))
}

func TestUpgraderExtensionConflict(t *testing.T) {
	// Two fake extensions both claim RSV1: server must reject the upgrade
	// before writing the successful response.
	u := Upgrader{
		NegotiateExtensions: func(offers []httphead.Option) ([]NegotiatedExtension, error) {
			return []NegotiatedExtension{
				{Option: httphead.Option{Name: []byte("alpha")}, Rsv: RsvBit1},
				{Option: httphead.Option{Name: []byte("beta")}, Rsv: RsvBit1},
			}, nil
		},
	}

	conn := bytes.NewBuffer(rawUpgradeRequest("alpha, beta"))
	_, err := u.Upgrade(conn)
	var conflict *ExtensionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want *ExtensionConflictError, got %T: %v", err, err)
	}
	if resp := conn.String(); !strings.Contains(resp, " 500 ") {
		t.Fatalf("expected 500 rejection response, got:\n%s", resp)
	}
}

func TestUpgraderNegotiateExtensionsSuccess(t *testing.T) {
	// Two fake extensions claiming disjoint RSV bits must be accepted and
	// reported with their bit declarations.
	u := Upgrader{
		NegotiateExtensions: func(offers []httphead.Option) ([]NegotiatedExtension, error) {
			if len(offers) != 2 || string(offers[0].Name) != "alpha" ||
				string(offers[1].Name) != "beta" {
				t.Fatalf("unexpected offers: %v", offers)
			}
			return []NegotiatedExtension{
				{Option: offers[0], Rsv: RsvBit1},
				{Option: offers[1], Rsv: RsvBit2},
			}, nil
		},
	}

	conn := bytes.NewBuffer(rawUpgradeRequest("alpha, beta"))
	hs, err := u.Upgrade(conn)
	if err != nil {
		t.Fatalf("unexpected upgrade error: %v", err)
	}
	if len(hs.NegotiatedExtensions) != 2 {
		t.Fatalf("want 2 negotiated extensions, got %d", len(hs.NegotiatedExtensions))
	}
	if string(hs.NegotiatedExtensions[0].Option.Name) != "alpha" ||
		hs.NegotiatedExtensions[0].Rsv != RsvBit1 {
		t.Fatalf("unexpected first extension: %+v", hs.NegotiatedExtensions[0])
	}
	if string(hs.NegotiatedExtensions[1].Option.Name) != "beta" ||
		hs.NegotiatedExtensions[1].Rsv != RsvBit2 {
		t.Fatalf("unexpected second extension: %+v", hs.NegotiatedExtensions[1])
	}
	if len(hs.Extensions) != 2 {
		t.Fatalf("legacy extensions list must be populated, got %d", len(hs.Extensions))
	}
	if resp := conn.String(); !strings.Contains(resp, " 101 ") ||
		!strings.Contains(resp, "Sec-WebSocket-Extensions: alpha,beta") {
		t.Fatalf("expected 101 response with extensions, got:\n%s", resp)
	}
}

func TestHTTPUpgraderNegotiateExtensions(t *testing.T) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(rawUpgradeRequest("alpha, beta"))))
	if err != nil {
		t.Fatalf("read request: %v", err)
	}

	u := HTTPUpgrader{
		NegotiateExtensions: func(offers []httphead.Option) ([]NegotiatedExtension, error) {
			return []NegotiatedExtension{
				{Option: offers[0], Rsv: RsvBit1},
				{Option: offers[1], Rsv: RsvBit2},
			}, nil
		},
	}

	_, _, hs, err := u.Upgrade(req, newRecorder())
	if err != nil {
		t.Fatalf("unexpected upgrade error: %v", err)
	}
	if len(hs.NegotiatedExtensions) != 2 {
		t.Fatalf("want 2 negotiated extensions, got %d", len(hs.NegotiatedExtensions))
	}
}

// serveHTTPUpgrade reads a single upgrade request from client and replies
// with a 101 response accepting the given extension header value.
func serveHTTPUpgrade(t *testing.T, client net.Conn, extensions string) {
	t.Helper()
	go func() {
		defer client.Close()
		req, err := http.ReadRequest(bufio.NewReader(client))
		if err != nil {
			return
		}
		nonce := req.Header.Get(headerSecKey)
		var b bytes.Buffer
		b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		b.WriteString("Upgrade: websocket\r\n")
		b.WriteString("Connection: Upgrade\r\n")
		b.WriteString(headerSecAccept + ": " +
			string(makeAccept(strToBytes(nonce))) + "\r\n")
		if extensions != "" {
			b.WriteString(headerSecExtensions + ": " + extensions + "\r\n")
		}
		b.WriteString("\r\n")
		_, _ = client.Write(b.Bytes())
	}()
}

func dialWithExtensions(t *testing.T, d Dialer, accepted string) (net.Conn, Handshake) {
	t.Helper()
	server, client := net.Pipe()
	serveHTTPUpgrade(t, server, accepted)
	d.NetDial = func(_ context.Context, _, _ string) (net.Conn, error) {
		return client, nil
	}
	conn, _, hs, err := d.Dial(context.Background(), "ws://example.org")
	if err != nil {
		client.Close()
		t.Fatalf("unexpected dial error: %v", err)
	}
	return conn, hs
}

func TestDialerNegotiateExtensions(t *testing.T) {
	dialer := func(claim func(httphead.Option) byte) Dialer {
		return Dialer{
			Extensions: []httphead.Option{
				{Name: []byte("alpha")},
				{Name: []byte("beta")},
			},
			NegotiateExtensions: func(opt httphead.Option) (NegotiatedExtension, error) {
				return NegotiatedExtension{Option: opt, Rsv: claim(opt)}, nil
			},
		}
	}

	t.Run("legal combination", func(t *testing.T) {
		d := dialer(func(opt httphead.Option) byte {
			switch string(opt.Name) {
			case "alpha":
				return RsvBit1
			case "beta":
				return RsvBit2
			default:
				return 0
			}
		})
		conn, hs := dialWithExtensions(t, d, "alpha,beta")
		defer conn.Close()
		if len(hs.NegotiatedExtensions) != 2 {
			t.Fatalf("want 2 negotiated extensions, got %d", len(hs.NegotiatedExtensions))
		}
		if hs.NegotiatedExtensions[0].Rsv != RsvBit1 ||
			hs.NegotiatedExtensions[1].Rsv != RsvBit2 {
			t.Fatalf("unexpected rsv declarations: %+v", hs.NegotiatedExtensions)
		}
	})

	t.Run("conflict before success", func(t *testing.T) {
		// The server accepts both fake extensions, but the client maps both
		// to RSV1, which must be detected before the upgrade is reported as
		// successful.
		d := dialer(func(httphead.Option) byte { return RsvBit1 })
		server, client := net.Pipe()
		serveHTTPUpgrade(t, server, "alpha,beta")
		d.NetDial = func(_ context.Context, _, _ string) (net.Conn, error) {
			return client, nil
		}
		_, _, _, err := d.Dial(context.Background(), "ws://example.org")
		client.Close()
		var conflict *ExtensionConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("want *ExtensionConflictError, got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), "RSV1") {
			t.Fatalf("conflict error must mention the bit: %v", err)
		}
	})
}
