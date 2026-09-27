// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	hclog "github.com/hashicorp/go-hclog"
)

const (
	// envHandshakeAddr is the environment variable used to tell the plugin
	// the address of the host's dedicated handshake channel. When set, the
	// plugin performs the protocol handshake over this channel instead of
	// writing the negotiation line to stdout.
	envHandshakeAddr = "PLUGIN_HANDSHAKE_ADDR"

	// envHandshakeToken is the environment variable used to tell the plugin
	// the per-launch authentication token it must present on the handshake
	// channel before the host will accept its protocol negotiation line.
	envHandshakeToken = "PLUGIN_HANDSHAKE_TOKEN"
)

// handshakeServer implements the host side of the dedicated handshake
// channel. The host creates one per plugin launch, generates a random
// authentication token, and passes the channel address and token to the
// plugin via environment variables. The plugin connects, authenticates with
// the token, and then exchanges its protocol, address, and certificate
// information, keeping the handshake completely off stdout.
//
// The channel uses a Unix socket on Unix systems and an ephemeral loopback
// TCP port on Windows, so it never depends on a fixed port.
type handshakeServer struct {
	listener net.Listener
	token    string
	logger   hclog.Logger
}

// newHandshakeServer creates a handshake channel listener and its random
// per-launch token. On Unix the socket is created inside socketDir, which
// must already exist. On Windows an ephemeral TCP port on 127.0.0.1 is used
// and socketDir is ignored.
func newHandshakeServer(socketDir string, logger hclog.Logger) (*handshakeServer, error) {
	token, err := generateHandshakeToken()
	if err != nil {
		return nil, err
	}

	var listener net.Listener
	if runtime.GOOS == "windows" {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	} else {
		listener, err = handshakeListenerUnix(socketDir)
	}
	if err != nil {
		return nil, err
	}

	return &handshakeServer{
		listener: listener,
		token:    token,
		logger:   logger,
	}, nil
}

// generateHandshakeToken returns a random per-launch credential used to
// authenticate the plugin on the handshake channel.
func generateHandshakeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate handshake token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func handshakeListenerUnix(socketDir string) (net.Listener, error) {
	tf, err := os.CreateTemp(socketDir, "plugin-handshake")
	if err != nil {
		return nil, err
	}
	path := tf.Name()

	// Close the file and remove it because it has to not exist for
	// the domain socket.
	if err := tf.Close(); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}

	// Wrap the listener in rmListener so that the Unix domain socket file
	// is removed on close.
	return newDeleteFileListener(l, path), nil
}

// addr returns the address the plugin should dial to perform the handshake.
func (s *handshakeServer) addr() string {
	return s.listener.Addr().String()
}

// close shuts down the handshake channel, unblocking any pending accept
// and removing the socket file if there is one.
func (s *handshakeServer) close() error {
	return s.listener.Close()
}

// accept accepts connections on the handshake channel until the listener is
// closed. The protocol line from the first correctly authenticated
// connection is sent on lineCh; lines from any further connections are
// discarded. Connections that fail authentication are closed and do not
// prevent the real plugin from completing its handshake.
func (s *handshakeServer) accept(lineCh chan<- string, connTimeout time.Duration) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn, lineCh, connTimeout)
	}
}

func (s *handshakeServer) handleConn(conn net.Conn, lineCh chan<- string, connTimeout time.Duration) {
	defer func() { _ = conn.Close() }()

	if connTimeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(connTimeout))
	}

	reader := bufio.NewReader(conn)

	token, err := reader.ReadString('\n')
	if err != nil {
		s.logger.Debug("error reading handshake token", "error", err)
		return
	}

	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(s.token)) != 1 {
		s.logger.Debug("rejected handshake attempt with invalid token")
		return
	}

	line, err := reader.ReadString('\n')
	if err != nil {
		s.logger.Debug("error reading handshake protocol line", "error", err)
		return
	}

	// Only the first authenticated handshake wins. If a handshake was
	// already delivered (e.g. via the legacy stdout path), discard this one.
	select {
	case lineCh <- strings.TrimSpace(line):
	default:
	}
}

// sendHandshake connects to the host's handshake channel, authenticates with
// the given token, and sends the protocol negotiation line containing the
// plugin's protocol, address, and certificate information.
func sendHandshake(addr, token, protocolLine string) error {
	network := "unix"
	if runtime.GOOS == "windows" {
		network = "tcp"
	}

	conn, err := net.DialTimeout(network, addr, 30*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect to handshake channel: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := fmt.Fprintf(conn, "%s\n%s\n", token, protocolLine); err != nil {
		return fmt.Errorf("failed to send handshake: %w", err)
	}

	return nil
}
