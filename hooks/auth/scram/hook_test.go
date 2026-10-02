// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2022 mochi-mqtt, mochi-co
// SPDX-FileContributor: mochi-co

package scram

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/packets"
	"github.com/stretchr/testify/require"
)

var logger = slog.New(slog.NewTextHandler(io.Discard, nil))

// testStore returns a credential store containing a single user enrolled with
// the given password and iteration count.
func testStore(t *testing.T, username, password string, iterations int) *MapStore {
	t.Helper()
	salt, err := GenerateSalt(16)
	require.NoError(t, err)

	store := NewMapStore()
	store.Add(username, DeriveCredentials([]byte(password), salt, iterations))
	return store
}

// newScramServer returns a server with the SCRAM hook installed.
func newScramServer(t *testing.T, opts *Options) *mqtt.Server {
	t.Helper()
	capabilities := mqtt.NewDefaultServerCapabilities()
	capabilities.MaximumMessageExpiryInterval = 0
	capabilities.ReceiveMaximum = 0

	s := mqtt.New(&mqtt.Options{
		Logger:       logger,
		Capabilities: capabilities,
	})
	require.NoError(t, s.AddHook(new(Hook), opts))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// localClient is a minimal MQTT v5 protocol client speaking raw packets over
// an in-memory pipe to the server.
type localClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	done chan error
}

// newLocalClient establishes a connection to the server and returns a client
// ready to send a CONNECT packet.
func newLocalClient(t *testing.T, s *mqtt.Server) *localClient {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	c := &localClient{
		t:    t,
		conn: clientConn,
		br:   bufio.NewReader(clientConn),
		done: make(chan error, 1),
	}
	go func() {
		c.done <- s.EstablishConnection("tcp", serverConn)
	}()
	t.Cleanup(func() { _ = c.conn.Close() })
	return c
}

// connect sends a CONNECT packet carrying the SCRAM client-first-message.
func (c *localClient) connect(clientID, username string, clientFirst []byte) {
	c.t.Helper()
	pk := packets.Packet{
		ProtocolVersion: 5,
		FixedHeader:     packets.FixedHeader{Type: packets.Connect},
		Connect: packets.ConnectParams{
			ProtocolName:     []byte("MQTT"),
			Clean:            true,
			Keepalive:        30,
			ClientIdentifier: clientID,
		},
	}
	if username != "" {
		pk.Connect.UsernameFlag = true
		pk.Connect.Username = []byte(username)
	}
	if clientFirst != nil {
		pk.Properties.AuthenticationMethod = Method
		pk.Properties.AuthenticationData = clientFirst
	}
	c.send(pk)
}

// sendAuth sends an AUTH packet continuing the enhanced authentication exchange.
func (c *localClient) sendAuth(data []byte) {
	c.t.Helper()
	c.send(packets.Packet{
		ProtocolVersion: 5,
		FixedHeader:     packets.FixedHeader{Type: packets.Auth},
		ReasonCode:      packets.CodeContinueAuthentication.Code,
		Properties: packets.Properties{
			AuthenticationMethod: Method,
			AuthenticationData:   data,
		},
	})
}

// send encodes and writes a packet to the server.
func (c *localClient) send(pk packets.Packet) {
	c.t.Helper()
	var buf bytes.Buffer
	var err error
	switch pk.FixedHeader.Type {
	case packets.Connect:
		err = pk.ConnectEncode(&buf)
	case packets.Auth:
		err = pk.AuthEncode(&buf)
	case packets.Publish:
		err = pk.PublishEncode(&buf)
	case packets.Disconnect:
		err = pk.DisconnectEncode(&buf)
	case packets.Pingreq:
		err = pk.PingreqEncode(&buf)
	default:
		c.t.Fatalf("unsupported outbound packet type %d", pk.FixedHeader.Type)
	}
	require.NoError(c.t, err)

	require.NoError(c.t, c.conn.SetWriteDeadline(time.Now().Add(5*time.Second)))
	_, err = c.conn.Write(buf.Bytes())
	require.NoError(c.t, err)
}

// recv reads and decodes the next packet from the server.
func (c *localClient) recv() packets.Packet {
	c.t.Helper()
	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	b, err := c.br.ReadByte()
	require.NoError(c.t, err)

	fh := new(packets.FixedHeader)
	require.NoError(c.t, fh.Decode(b))

	remaining, _, err := packets.DecodeLength(c.br)
	require.NoError(c.t, err)
	fh.Remaining = remaining

	p := make([]byte, remaining)
	_, err = io.ReadFull(c.br, p)
	require.NoError(c.t, err)

	pk := packets.Packet{FixedHeader: *fh, ProtocolVersion: 5}
	switch fh.Type {
	case packets.Connack:
		require.NoError(c.t, pk.ConnackDecode(p))
	case packets.Auth:
		require.NoError(c.t, pk.AuthDecode(p))
	case packets.Disconnect:
		require.NoError(c.t, pk.DisconnectDecode(p))
	case packets.Pingresp:
	default:
		c.t.Fatalf("unexpected inbound packet type %d", fh.Type)
	}
	return pk
}

// expectServerFirst reads the AUTH packet containing the server-first-message.
func (c *localClient) expectServerFirst() []byte {
	c.t.Helper()
	pk := c.recv()
	require.Equal(c.t, byte(packets.Auth), pk.FixedHeader.Type)
	require.Equal(c.t, packets.CodeContinueAuthentication.Code, pk.ReasonCode)
	require.Equal(c.t, Method, pk.Properties.AuthenticationMethod)
	require.NotEmpty(c.t, pk.Properties.AuthenticationData)
	return pk.Properties.AuthenticationData
}

// expectConnackError reads a CONNACK packet and requires a failure reason code.
func (c *localClient) expectConnackError() packets.Packet {
	c.t.Helper()
	pk := c.recv()
	require.Equal(c.t, byte(packets.Connack), pk.FixedHeader.Type)
	require.NotEqual(c.t, packets.CodeSuccess.Code, pk.ReasonCode)
	return pk
}

func TestHookID(t *testing.T) {
	h := new(Hook)
	require.Equal(t, "scram-auth", h.ID())
}

func TestHookProvides(t *testing.T) {
	h := new(Hook)
	require.True(t, h.Provides(mqtt.OnConnectAuthenticate))
	require.True(t, h.Provides(mqtt.OnAuthPacket))
	require.False(t, h.Provides(mqtt.OnACLCheck))
	require.False(t, h.Provides(mqtt.OnPublish))
}

func TestHookInit(t *testing.T) {
	t.Run("bad config type", func(t *testing.T) {
		h := new(Hook)
		h.SetOpts(logger, nil)
		require.ErrorIs(t, h.Init(map[string]any{}), mqtt.ErrInvalidConfigType)
	})

	t.Run("missing store", func(t *testing.T) {
		h := new(Hook)
		h.SetOpts(logger, nil)
		require.ErrorIs(t, h.Init(nil), ErrNoCredentialStore)
	})

	t.Run("defaults", func(t *testing.T) {
		h := new(Hook)
		h.SetOpts(logger, nil)
		require.NoError(t, h.Init(&Options{Store: NewMapStore()}))
		require.Equal(t, DefaultMaxRounds, h.config.MaxRounds)
		require.Equal(t, DefaultMinIterations, h.config.MinIterations)
		require.Equal(t, DefaultMaxIterations, h.config.MaxIterations)
		require.Equal(t, DefaultNonceLength, h.config.NonceLength)
	})

	t.Run("invalid options", func(t *testing.T) {
		for _, opts := range []*Options{
			{Store: NewMapStore(), MaxRounds: 1},
			{Store: NewMapStore(), MinIterations: -1},
			{Store: NewMapStore(), MinIterations: 100, MaxIterations: 10},
			{Store: NewMapStore(), NonceLength: 2},
			{Store: NewMapStore(), NonceLength: 128},
		} {
			h := new(Hook)
			h.SetOpts(logger, nil)
			require.ErrorIs(t, h.Init(opts), ErrInvalidOptions, "options %+v", opts)
		}
	})
}

// TestScramSuccess verifies a full SCRAM-SHA-256 enhanced authentication
// exchange: the server verifies the client proof and the client verifies the
// server proof carried in the CONNACK authentication data.
func TestScramSuccess(t *testing.T) {
	s := newScramServer(t, &Options{Store: testStore(t, "mochi", "melon", 4096)})
	c := newLocalClient(t, s)

	conv, err := NewClientConversation("mochi", []byte("melon"))
	require.NoError(t, err)

	c.connect("scram-client", "mochi", conv.ClientFirst())

	serverFirst := c.expectServerFirst()
	clientFinal, err := conv.ClientFinal(serverFirst)
	require.NoError(t, err)

	c.sendAuth(clientFinal)

	connack := c.recv()
	require.Equal(t, byte(packets.Connack), connack.FixedHeader.Type)
	require.Equal(t, packets.CodeSuccess.Code, connack.ReasonCode)
	require.Equal(t, Method, connack.Properties.AuthenticationMethod)
	require.NoError(t, conv.VerifyServerFinal(connack.Properties.AuthenticationData))

	// The connection is established and usable.
	c.send(packets.Packet{ProtocolVersion: 5, FixedHeader: packets.FixedHeader{Type: packets.Pingreq}})
	require.Equal(t, byte(packets.Pingresp), c.recv().FixedHeader.Type)

	c.send(packets.Packet{ProtocolVersion: 5, FixedHeader: packets.FixedHeader{Type: packets.Disconnect}})
}

func TestScramWrongPassword(t *testing.T) {
	s := newScramServer(t, &Options{Store: testStore(t, "mochi", "melon", 4096)})
	c := newLocalClient(t, s)

	conv, err := NewClientConversation("mochi", []byte("wrong"))
	require.NoError(t, err)

	c.connect("scram-client", "mochi", conv.ClientFirst())
	serverFirst := c.expectServerFirst()

	clientFinal, err := conv.ClientFinal(serverFirst)
	require.NoError(t, err)
	c.sendAuth(clientFinal)

	require.Equal(t, packets.ErrBadUsernameOrPassword.Code, c.expectConnackError().ReasonCode)
}

// TestScramBadNonce verifies that a client-final-message carrying a nonce other
// than the one issued in the server-first-message is rejected.
func TestScramBadNonce(t *testing.T) {
	s := newScramServer(t, &Options{Store: testStore(t, "mochi", "melon", 4096)})
	c := newLocalClient(t, s)

	conv, err := NewClientConversation("mochi", []byte("melon"))
	require.NoError(t, err)

	c.connect("scram-client", "mochi", conv.ClientFirst())
	serverFirst := c.expectServerFirst()

	clientFinal, err := conv.ClientFinal(serverFirst)
	require.NoError(t, err)

	tampered := bytes.Replace(clientFinal, []byte(",r="), []byte(",r=forged"), 1)
	require.NotEqual(t, clientFinal, tampered)
	c.sendAuth(tampered)

	require.Equal(t, packets.ErrBadUsernameOrPassword.Code, c.expectConnackError().ReasonCode)
}

func TestScramUnknownUser(t *testing.T) {
	s := newScramServer(t, &Options{Store: testStore(t, "mochi", "melon", 4096)})
	c := newLocalClient(t, s)

	conv, err := NewClientConversation("nobody", []byte("melon"))
	require.NoError(t, err)

	c.connect("scram-client", "nobody", conv.ClientFirst())
	require.Equal(t, packets.ErrBadUsernameOrPassword.Code, c.expectConnackError().ReasonCode)
}

// TestScramIterationBounds verifies that credentials with an iteration cost
// outside the configured bounds are rejected.
func TestScramIterationBounds(t *testing.T) {
	for name, opts := range map[string]struct {
		iterations int
		options    *Options
	}{
		"below minimum": {100, &Options{MinIterations: 4096}},
		"above maximum": {100000, &Options{MaxIterations: 65536}},
	} {
		t.Run(name, func(t *testing.T) {
			options := opts.options
			options.Store = testStore(t, "mochi", "melon", opts.iterations)
			s := newScramServer(t, options)
			c := newLocalClient(t, s)

			conv, err := NewClientConversation("mochi", []byte("melon"))
			require.NoError(t, err)

			c.connect("scram-client", "mochi", conv.ClientFirst())
			require.Equal(t, packets.ErrBadUsernameOrPassword.Code, c.expectConnackError().ReasonCode)
		})
	}
}

// TestScramMaxRounds verifies that the exchange is aborted once the
// configured maximum number of authentication rounds is exceeded.
func TestScramMaxRounds(t *testing.T) {
	s := newScramServer(t, &Options{
		Store:     testStore(t, "mochi", "melon", 4096),
		MaxRounds: 3, // connect + two auth packets
	})
	c := newLocalClient(t, s)

	conv, err := NewClientConversation("mochi", []byte("melon"))
	require.NoError(t, err)

	c.connect("scram-client", "mochi", conv.ClientFirst())
	c.expectServerFirst()

	// Restart the exchange twice, consuming the remaining rounds.
	for i := 0; i < 2; i++ {
		restart, err := NewClientConversation("mochi", []byte("melon"))
		require.NoError(t, err)
		c.sendAuth(restart.ClientFirst())
		c.expectServerFirst()
	}

	// No rounds remain; the server rejects the connection.
	require.Equal(t, packets.ErrBadUsernameOrPassword.Code, c.expectConnackError().ReasonCode)
}

// TestScramDuplicateAuth verifies that an AUTH packet received after the
// authentication exchange has completed is a protocol violation.
func TestScramDuplicateAuth(t *testing.T) {
	s := newScramServer(t, &Options{Store: testStore(t, "mochi", "melon", 4096)})
	c := newLocalClient(t, s)

	conv, err := NewClientConversation("mochi", []byte("melon"))
	require.NoError(t, err)

	c.connect("scram-client", "mochi", conv.ClientFirst())
	serverFirst := c.expectServerFirst()

	clientFinal, err := conv.ClientFinal(serverFirst)
	require.NoError(t, err)
	c.sendAuth(clientFinal)

	connack := c.recv()
	require.Equal(t, packets.CodeSuccess.Code, connack.ReasonCode)

	// A duplicate AUTH packet after the exchange is a protocol violation.
	c.sendAuth(clientFinal)
	disconnect := c.recv()
	require.Equal(t, byte(packets.Disconnect), disconnect.FixedHeader.Type)
	require.Equal(t, packets.ErrProtocolViolation.Code, disconnect.ReasonCode)
}

// TestScramPublishMidAuth verifies that a PUBLISH packet sent before the
// authentication exchange has completed causes the connection to be rejected.
func TestScramPublishMidAuth(t *testing.T) {
	s := newScramServer(t, &Options{Store: testStore(t, "mochi", "melon", 4096)})
	c := newLocalClient(t, s)

	conv, err := NewClientConversation("mochi", []byte("melon"))
	require.NoError(t, err)

	c.connect("scram-client", "mochi", conv.ClientFirst())
	c.expectServerFirst()

	c.send(packets.Packet{
		ProtocolVersion: 5,
		FixedHeader:     packets.FixedHeader{Type: packets.Publish},
		TopicName:       "a/b",
		Payload:         []byte("premature"),
	})

	require.Equal(t, packets.ErrBadUsernameOrPassword.Code, c.expectConnackError().ReasonCode)
}

// TestScramDisconnectMidAuth verifies the server cleans up gracefully when
// the client disconnects in the middle of the authentication exchange.
func TestScramDisconnectMidAuth(t *testing.T) {
	s := newScramServer(t, &Options{Store: testStore(t, "mochi", "melon", 4096)})
	c := newLocalClient(t, s)

	conv, err := NewClientConversation("mochi", []byte("melon"))
	require.NoError(t, err)

	c.connect("scram-client", "mochi", conv.ClientFirst())
	c.expectServerFirst()

	require.NoError(t, c.conn.Close())

	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not release the connection after mid-auth disconnect")
	}

	require.Equal(t, int64(0), s.Info.ClientsConnected)
}

// TestScramNonScramClientRejected verifies that clients not requesting the
// SCRAM-SHA-256 method are rejected when only the SCRAM hook is installed.
func TestScramNonScramClientRejected(t *testing.T) {
	s := newScramServer(t, &Options{Store: testStore(t, "mochi", "melon", 4096)})
	c := newLocalClient(t, s)

	pk := packets.Packet{
		ProtocolVersion: 5,
		FixedHeader:     packets.FixedHeader{Type: packets.Connect},
		Connect: packets.ConnectParams{
			ProtocolName:     []byte("MQTT"),
			Clean:            true,
			Keepalive:        30,
			ClientIdentifier: "legacy-client",
			UsernameFlag:     true,
			Username:         []byte("mochi"),
			PasswordFlag:     true,
			Password:         []byte("melon"),
		},
	}
	c.send(pk)
	require.Equal(t, packets.ErrBadUsernameOrPassword.Code, c.expectConnackError().ReasonCode)
}

// TestLegacyPasswordHookUnaffected verifies the existing username/password
// ledger hook continues to authenticate clients when the SCRAM hook is also
// installed, and that SCRAM clients are unaffected by its presence.
func TestLegacyPasswordHookUnaffected(t *testing.T) {
	capabilities := mqtt.NewDefaultServerCapabilities()
	capabilities.MaximumMessageExpiryInterval = 0
	capabilities.ReceiveMaximum = 0

	s := mqtt.New(&mqtt.Options{
		Logger:       logger,
		Capabilities: capabilities,
	})
	t.Cleanup(func() { _ = s.Close() })

	require.NoError(t, s.AddHook(new(Hook), &Options{Store: testStore(t, "mochi", "melon", 4096)}))
	require.NoError(t, s.AddHook(new(auth.Hook), &auth.Options{
		Ledger: &auth.Ledger{
			Auth: auth.AuthRules{
				{Username: "mochi", Password: "melon", Allow: true},
			},
			ACL: auth.ACLRules{
				{Filters: auth.Filters{"#": auth.ReadWrite}},
			},
		},
	}))

	t.Run("legacy username password client", func(t *testing.T) {
		c := newLocalClient(t, s)
		c.send(packets.Packet{
			ProtocolVersion: 5,
			FixedHeader:     packets.FixedHeader{Type: packets.Connect},
			Connect: packets.ConnectParams{
				ProtocolName:     []byte("MQTT"),
				Clean:            true,
				Keepalive:        30,
				ClientIdentifier: "legacy-client",
				UsernameFlag:     true,
				Username:         []byte("mochi"),
				PasswordFlag:     true,
				Password:         []byte("melon"),
			},
		})

		connack := c.recv()
		require.Equal(t, byte(packets.Connack), connack.FixedHeader.Type)
		require.Equal(t, packets.CodeSuccess.Code, connack.ReasonCode)
	})

	t.Run("scram client", func(t *testing.T) {
		c := newLocalClient(t, s)
		conv, err := NewClientConversation("mochi", []byte("melon"))
		require.NoError(t, err)

		c.connect("scram-client", "mochi", conv.ClientFirst())
		serverFirst := c.expectServerFirst()

		clientFinal, err := conv.ClientFinal(serverFirst)
		require.NoError(t, err)
		c.sendAuth(clientFinal)

		connack := c.recv()
		require.Equal(t, packets.CodeSuccess.Code, connack.ReasonCode)
		require.NoError(t, conv.VerifyServerFinal(connack.Properties.AuthenticationData))
	})
}
