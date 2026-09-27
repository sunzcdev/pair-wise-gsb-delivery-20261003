// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
)

// maxHandshakeLineSize bounds the size of a single handshake line read from
// the dedicated handshake channel. It is generous enough to hold a handshake
// that includes an AutoMTLS certificate.
const maxHandshakeLineSize = 64 * 1024

// clientHandshake manages the host side of the dedicated handshake channel.
// The client creates a listener before starting the plugin, passes its
// address and a random one-time token to the plugin via environment
// variables, and accepts handshake connections until one presents the
// correct token. This keeps the handshake off the plugin's stdout, so
// plugins are free to write to stdout at any time during startup.
type clientHandshake struct {
	listener  net.Listener
	token     string
	socketDir string
	timeout   time.Duration
	lineCh    chan string
	done      chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	closing bool
}

// newClientHandshake creates the dedicated handshake channel for a single
// plugin launch. On Windows it listens on loopback TCP with an ephemeral
// port; elsewhere it listens on a unix socket inside a freshly created
// temporary directory. Neither relies on a fixed port.
func newClientHandshake(socketCfg UnixSocketConfig, timeout time.Duration) (*clientHandshake, error) {
	token, err := generateHandshakeToken()
	if err != nil {
		return nil, err
	}

	h := &clientHandshake{
		token:   token,
		timeout: timeout,
		lineCh:  make(chan string, 1),
		done:    make(chan struct{}),
		conns:   make(map[net.Conn]struct{}),
	}

	if runtime.GOOS == "windows" {
		h.listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		return h, nil
	}

	h.socketDir, err = os.MkdirTemp(socketCfg.TempDir, "plugin-handshake")
	if err != nil {
		return nil, err
	}

	socketPath := filepath.Join(h.socketDir, "handshake.sock")
	h.listener, err = net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(h.socketDir)
		return nil, err
	}

	// If a custom group is configured, the plugin process may need the
	// socket and its directory to be group-writable in order to connect.
	if socketCfg.Group != "" {
		if err := setGroupWritable(h.socketDir, socketCfg.Group, 0o770); err != nil {
			h.close()
			return nil, err
		}
		if err := setGroupWritable(socketPath, socketCfg.Group, 0o660); err != nil {
			h.close()
			return nil, err
		}
	}

	return h, nil
}

// env returns the environment variables that tell the plugin how to reach
// the handshake channel and which credential to present.
func (h *clientHandshake) env() []string {
	return []string{
		fmt.Sprintf("%s=%s", EnvHandshakeNetwork, h.listener.Addr().Network()),
		fmt.Sprintf("%s=%s", EnvHandshakeAddr, h.listener.Addr().String()),
		fmt.Sprintf("%s=%s", EnvHandshakeToken, h.token),
	}
}

// close shuts down the handshake channel and removes any resources it
// created, such as the unix socket directory. It is safe to call multiple
// times.
func (h *clientHandshake) close() {
	h.closeOnce.Do(func() {
		close(h.done)
		_ = h.listener.Close()

		h.mu.Lock()
		h.closing = true
		for conn := range h.conns {
			_ = conn.Close()
		}
		h.mu.Unlock()

		if h.socketDir != "" {
			_ = os.RemoveAll(h.socketDir)
		}
	})
}

// acceptLoop accepts connections on the handshake listener until it is
// closed. Any number of connections may arrive before the real plugin
// connects, so each connection is authenticated independently and the
// listener stays open until the handshake completes or the start fails.
func (h *clientHandshake) acceptLoop(logger hclog.Logger) {
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			return
		}
		go h.handleConn(logger, conn)
	}
}

// handleConn reads a single handshake line from the connection, verifies
// the token, and delivers the line if and only if the token matches the
// one generated for this launch. Connections with missing or stale
// credentials are rejected and closed.
func (h *clientHandshake) handleConn(logger hclog.Logger, conn net.Conn) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		_ = conn.Close()
		return
	}
	h.conns[conn] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.conns, conn)
		h.mu.Unlock()
		_ = conn.Close()
	}()

	_ = conn.SetDeadline(time.Now().Add(h.timeout))

	line, err := bufio.NewReader(io.LimitReader(conn, maxHandshakeLineSize)).ReadString('\n')
	if err != nil {
		logger.Warn("error reading from handshake connection", "error", err)
		return
	}

	token, handshakeLine, found := strings.Cut(strings.TrimSpace(line), "|")
	if !found || subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) != 1 {
		logger.Warn("rejected handshake connection with invalid token",
			"remote_addr", conn.RemoteAddr())
		return
	}

	select {
	case h.lineCh <- handshakeLine:
	case <-h.done:
	}
}

// generateHandshakeToken returns a random credential that authenticates the
// plugin instance started for a single launch.
func generateHandshakeToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("error generating handshake token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// looksLikeHandshake reports whether a line read from the plugin's stdout
// plausibly contains a legacy handshake. It is used to ignore unrelated
// stdout output while a dedicated handshake channel is available, without
// mistaking it for a legacy handshake.
func looksLikeHandshake(line string) bool {
	parts := strings.SplitN(strings.TrimSpace(line), "|", 5)
	if len(parts) < 4 {
		return false
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return false
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return false
	}
	switch parts[2] {
	case "tcp", "unix":
		return true
	}
	return false
}

// sendHandshakeLine delivers the plugin's handshake line to the client over
// the dedicated handshake channel, authenticated with the token the client
// generated for this launch. It returns (false, nil) when the client did
// not offer a handshake channel, in which case the caller should fall back
// to the legacy stdout handshake.
func sendHandshakeLine(line string) (bool, error) {
	network := os.Getenv(EnvHandshakeNetwork)
	addr := os.Getenv(EnvHandshakeAddr)
	token := os.Getenv(EnvHandshakeToken)
	if network == "" || addr == "" || token == "" {
		return false, nil
	}

	conn, err := net.DialTimeout(network, addr, time.Minute)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()

	if _, err := fmt.Fprintf(conn, "%s|%s\n", token, line); err != nil {
		return false, err
	}

	return true, nil
}
