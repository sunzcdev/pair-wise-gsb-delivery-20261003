// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

// mfaStub is a minimal MySQL server protocol stub speaking over an in-memory
// connection. It drives the client through a scripted authentication
// handshake.
type mfaStub struct {
	conn net.Conn
	seq  uint8 // sequence number of the next packet in either direction
}

func testScramble(seed byte) []byte {
	scramble := make([]byte, 20)
	for i := range scramble {
		scramble[i] = seed + byte(i)
	}
	return scramble
}

func (s *mfaStub) writePacket(payload []byte) error {
	pkt := make([]byte, 4+len(payload))
	pkt[0] = byte(len(payload))
	pkt[1] = byte(len(payload) >> 8)
	pkt[2] = byte(len(payload) >> 16)
	pkt[3] = s.seq
	s.seq++
	copy(pkt[4:], payload)
	_, err := s.conn.Write(pkt)
	return err
}

func (s *mfaStub) readPacket() ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(s.conn, header); err != nil {
		return nil, err
	}
	if header[3] != s.seq {
		return nil, fmt.Errorf("unexpected client sequence number: got %d, want %d", header[3], s.seq)
	}
	s.seq++
	payload := make([]byte, int(header[0])|int(header[1])<<8|int(header[2])<<16)
	if _, err := io.ReadFull(s.conn, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// expectCommand resets the packet sequence tracking for the command phase
// that follows a successful authentication.
func (s *mfaStub) expectCommand() {
	s.seq = 0
}

const mfaStubCapabilities = clientMySQL | clientLongFlag | clientProtocol41 |
	clientSecureConn | clientTransactions | clientPluginAuth

func (s *mfaStub) sendHandshake(plugin string, scramble []byte) error {
	caps := uint32(mfaStubCapabilities)
	payload := []byte{10} // protocol version
	payload = append(payload, "8.0.40-mfa-stub"...)
	payload = append(payload, 0)
	payload = binary.LittleEndian.AppendUint32(payload, 1) // connection id
	payload = append(payload, scramble[:8]...)
	payload = append(payload, 0)
	payload = binary.LittleEndian.AppendUint16(payload, uint16(caps))
	payload = append(payload, 45)                          // charset
	payload = binary.LittleEndian.AppendUint16(payload, 2) // status flags
	payload = binary.LittleEndian.AppendUint16(payload, uint16(caps>>16))
	payload = append(payload, 21)                  // auth plugin data length
	payload = append(payload, make([]byte, 10)...) // reserved
	payload = append(payload, scramble[8:20]...)
	payload = append(payload, 0)
	payload = append(payload, plugin...)
	payload = append(payload, 0)
	return s.writePacket(payload)
}

func (s *mfaStub) sendAuthNextFactor(plugin string, scramble []byte) error {
	payload := []byte{iAuthNextFactor}
	payload = append(payload, plugin...)
	payload = append(payload, 0)
	payload = append(payload, scramble...)
	payload = append(payload, 0)
	return s.writePacket(payload)
}

func (s *mfaStub) sendAuthSwitchRequest(plugin string, data []byte) error {
	payload := []byte{iEOF}
	payload = append(payload, plugin...)
	payload = append(payload, 0)
	payload = append(payload, data...)
	return s.writePacket(payload)
}

func (s *mfaStub) sendOK() error {
	return s.writePacket([]byte{iOK, 0, 0, 2, 0, 0, 0})
}

func (s *mfaStub) sendErr(errno uint16, msg string) error {
	payload := []byte{iERR, byte(errno), byte(errno >> 8), '#', '2', '8', '0', '0', '0'}
	payload = append(payload, msg...)
	return s.writePacket(payload)
}

// parseHandshakeResponseAuth extracts the user name and the auth response
// from the client's handshake response packet.
func parseHandshakeResponseAuth(payload []byte) (user string, authResp []byte, err error) {
	pos := 4 + 4 + 1 + 23 // capabilities, max packet size, collation, filler
	end := bytes.IndexByte(payload[pos:], 0)
	if end < 0 {
		return "", nil, errors.New("malformed handshake response: user")
	}
	user = string(payload[pos : pos+end])
	pos += end + 1
	if pos >= len(payload) {
		return "", nil, errors.New("malformed handshake response: auth response")
	}
	// small enough for a 1-byte length encoded integer
	authRespLen := int(payload[pos])
	pos++
	if pos+authRespLen > len(payload) {
		return "", nil, errors.New("malformed handshake response: auth response length")
	}
	return user, payload[pos : pos+authRespLen], nil
}

// startMFAStubServer wires cfg to a fresh in-memory connection and runs the
// server-side script in a goroutine. It returns a channel reporting the
// script's result.
func startMFAStubServer(cfg *Config, serve func(*mfaStub) error) <-chan error {
	clientConn, serverConn := net.Pipe()
	cfg.Net = "mfa-stub"
	cfg.Addr = "mfa-stub"
	cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}
	errCh := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		errCh <- serve(&mfaStub{conn: serverConn})
	}()
	return errCh
}

type credentialsCall struct {
	factor int
	plugin string
}

// recordingProvider returns per-(factor, plugin) passwords and records every
// call the driver makes.
func recordingProvider(passwords map[credentialsCall]string, calls *[]credentialsCall, mu *sync.Mutex) CredentialsProviderFunc {
	return func(_ context.Context, factor int, plugin string) (string, error) {
		mu.Lock()
		*calls = append(*calls, credentialsCall{factor, plugin})
		mu.Unlock()
		passwd, ok := passwords[credentialsCall{factor, plugin}]
		if !ok {
			return "", fmt.Errorf("unexpected credentials request: factor %d, plugin %q", factor, plugin)
		}
		return passwd, nil
	}
}

// TestMultiFactorAuthSuccess walks a full multi-factor handshake with
// different plugins per factor and a mid-flow plugin switch, then verifies
// the connection is usable through database/sql only after the whole flow
// completed.
func TestMultiFactorAuthSuccess(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []credentialsCall
	)
	passwords := map[credentialsCall]string{
		{1, "mysql_native_password"}: "factor1-secret",
		{2, "mysql_native_password"}: "factor2-secret",
		{2, "mysql_clear_password"}:  "factor2-clear",
		{3, "mysql_native_password"}: "factor3-secret",
	}

	cfg := NewConfig()
	cfg.User = "mfa"
	// The DSN password must not be used: the provider is consulted for every
	// factor, including the first one.
	cfg.Passwd = "dsn-password-must-not-be-used"
	cfg.AllowCleartextPasswords = true
	cfg.Logger = &NopLogger{}
	if err := cfg.Apply(AuthCredentialsProvider(recordingProvider(passwords, &calls, &mu))); err != nil {
		t.Fatal(err)
	}

	scramble1 := testScramble(10)
	scramble2 := testScramble(40)
	scramble3 := testScramble(70)

	errCh := startMFAStubServer(cfg, func(s *mfaStub) error {
		// factor 1: mysql_native_password
		if err := s.sendHandshake("mysql_native_password", scramble1); err != nil {
			return err
		}
		payload, err := s.readPacket()
		if err != nil {
			return err
		}
		user, authResp, err := parseHandshakeResponseAuth(payload)
		if err != nil {
			return err
		}
		if user != "mfa" {
			return fmt.Errorf("unexpected user %q", user)
		}
		if want := scramblePassword(scramble1, "factor1-secret"); !bytes.Equal(authResp, want) {
			return fmt.Errorf("factor 1: unexpected auth response %x, want %x", authResp, want)
		}

		// factor 2: the server continues with the next factor
		if err := s.sendAuthNextFactor("mysql_native_password", scramble2); err != nil {
			return err
		}
		authResp, err = s.readPacket()
		if err != nil {
			return err
		}
		// The first factor's password must not be reused for this factor.
		if want := scramblePassword(scramble2, "factor2-secret"); !bytes.Equal(authResp, want) {
			return fmt.Errorf("factor 2: unexpected auth response %x, want %x", authResp, want)
		}

		// factor 2, continued: the server switches the plugin mid-flow
		if err := s.sendAuthSwitchRequest("mysql_clear_password", nil); err != nil {
			return err
		}
		authResp, err = s.readPacket()
		if err != nil {
			return err
		}
		if want := []byte("factor2-clear\x00"); !bytes.Equal(authResp, want) {
			return fmt.Errorf("factor 2 (switched plugin): unexpected auth response %q, want %q", authResp, want)
		}

		// factor 3
		if err := s.sendAuthNextFactor("mysql_native_password", scramble3); err != nil {
			return err
		}
		authResp, err = s.readPacket()
		if err != nil {
			return err
		}
		if want := scramblePassword(scramble3, "factor3-secret"); !bytes.Equal(authResp, want) {
			return fmt.Errorf("factor 3: unexpected auth response %x, want %x", authResp, want)
		}

		// The whole authentication flow succeeded.
		if err := s.sendOK(); err != nil {
			return err
		}

		// The connection is now handed to database/sql, which pings it.
		s.expectCommand()
		cmd, err := s.readPacket()
		if err != nil {
			return err
		}
		if len(cmd) != 1 || cmd[0] != comPing {
			return fmt.Errorf("expected COM_PING, got %x", cmd)
		}
		return s.sendOK()
	})

	db := sql.OpenDB(newConnector(cfg))
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("PingContext failed: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stub server: %v", err)
	}

	wantCalls := []credentialsCall{
		{1, "mysql_native_password"},
		{2, "mysql_native_password"},
		{2, "mysql_clear_password"},
		{3, "mysql_native_password"},
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprintf("%v", calls) != fmt.Sprintf("%v", wantCalls) {
		t.Fatalf("unexpected credentials calls: got %v, want %v", calls, wantCalls)
	}
}

// TestMultiFactorAuthCachingSha2FastAuth covers the real MySQL MFA flow
// where a caching_sha2_password fast-auth success is followed by a request
// for the next factor instead of a final OK packet.
func TestMultiFactorAuthCachingSha2FastAuth(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []credentialsCall
	)
	passwords := map[credentialsCall]string{
		{1, "caching_sha2_password"}: "factor1-secret",
		{2, "mysql_native_password"}: "factor2-secret",
	}

	cfg := NewConfig()
	cfg.User = "mfa"
	cfg.Logger = &NopLogger{}
	if err := cfg.Apply(AuthCredentialsProvider(recordingProvider(passwords, &calls, &mu))); err != nil {
		t.Fatal(err)
	}

	scramble1 := testScramble(10)
	scramble2 := testScramble(40)

	errCh := startMFAStubServer(cfg, func(s *mfaStub) error {
		if err := s.sendHandshake("caching_sha2_password", scramble1); err != nil {
			return err
		}
		payload, err := s.readPacket()
		if err != nil {
			return err
		}
		_, authResp, err := parseHandshakeResponseAuth(payload)
		if err != nil {
			return err
		}
		if want := scrambleSHA256Password(scramble1, "factor1-secret"); !bytes.Equal(authResp, want) {
			return fmt.Errorf("factor 1: unexpected auth response %x, want %x", authResp, want)
		}

		// Fast auth succeeded, but another factor is required.
		if err := s.writePacket([]byte{iAuthMoreData, cachingSha2PasswordFastAuthSuccess}); err != nil {
			return err
		}
		if err := s.sendAuthNextFactor("mysql_native_password", scramble2); err != nil {
			return err
		}
		authResp, err = s.readPacket()
		if err != nil {
			return err
		}
		if want := scramblePassword(scramble2, "factor2-secret"); !bytes.Equal(authResp, want) {
			return fmt.Errorf("factor 2: unexpected auth response %x, want %x", authResp, want)
		}
		if err := s.sendOK(); err != nil {
			return err
		}

		s.expectCommand()
		cmd, err := s.readPacket()
		if err != nil {
			return err
		}
		if len(cmd) != 1 || cmd[0] != comPing {
			return fmt.Errorf("expected COM_PING, got %x", cmd)
		}
		return s.sendOK()
	})

	db := sql.OpenDB(newConnector(cfg))
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("PingContext failed: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stub server: %v", err)
	}
}

// TestMultiFactorAuthSecondFactorFails verifies that a server-side rejection
// of the second factor aborts the connection attempt and that database/sql
// never sees a usable connection.
func TestMultiFactorAuthSecondFactorFails(t *testing.T) {
	passwords := map[credentialsCall]string{
		{1, "mysql_native_password"}: "factor1-secret",
		{2, "mysql_native_password"}: "factor2-wrong",
	}
	var (
		mu    sync.Mutex
		calls []credentialsCall
	)

	cfg := NewConfig()
	cfg.User = "mfa"
	cfg.Logger = &NopLogger{}
	if err := cfg.Apply(AuthCredentialsProvider(recordingProvider(passwords, &calls, &mu))); err != nil {
		t.Fatal(err)
	}

	scramble1 := testScramble(10)
	scramble2 := testScramble(40)

	errCh := startMFAStubServer(cfg, func(s *mfaStub) error {
		if err := s.sendHandshake("mysql_native_password", scramble1); err != nil {
			return err
		}
		if _, err := s.readPacket(); err != nil {
			return err
		}
		if err := s.sendAuthNextFactor("mysql_native_password", scramble2); err != nil {
			return err
		}
		if _, err := s.readPacket(); err != nil {
			return err
		}
		// Reject the second factor.
		return s.sendErr(1045, "Access denied for user 'mfa'@'%' (using password: YES)")
	})

	db := sql.OpenDB(newConnector(cfg))
	defer db.Close()
	err := db.PingContext(context.Background())
	var merr *MySQLError
	if !errors.As(err, &merr) || merr.Number != 1045 {
		t.Fatalf("expected MySQL error 1045, got %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stub server: %v", err)
	}
}

// TestMultiFactorAuthMissingCredentials verifies that without a credentials
// provider the first-factor password is not reused for additional factors:
// the authentication fails with ErrNoCredentials instead.
func TestMultiFactorAuthMissingCredentials(t *testing.T) {
	cfg := NewConfig()
	cfg.User = "mfa"
	cfg.Passwd = "first-factor-only"
	cfg.Logger = &NopLogger{}

	scramble1 := testScramble(10)
	scramble2 := testScramble(40)

	errCh := startMFAStubServer(cfg, func(s *mfaStub) error {
		if err := s.sendHandshake("mysql_native_password", scramble1); err != nil {
			return err
		}
		payload, err := s.readPacket()
		if err != nil {
			return err
		}
		_, authResp, err := parseHandshakeResponseAuth(payload)
		if err != nil {
			return err
		}
		if want := scramblePassword(scramble1, "first-factor-only"); !bytes.Equal(authResp, want) {
			return fmt.Errorf("factor 1: unexpected auth response %x, want %x", authResp, want)
		}
		if err := s.sendAuthNextFactor("mysql_native_password", scramble2); err != nil {
			return err
		}
		// The client must not answer the second factor with the first
		// factor's password; it is expected to give up and close.
		pkt, err := s.readPacket()
		if err == nil {
			return fmt.Errorf("client unexpectedly answered the second factor with %x", pkt)
		}
		return nil
	})

	_, err := newConnector(cfg).Connect(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("expected ErrNoCredentials, got %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stub server: %v", err)
	}
}

// TestMultiFactorAuthContextCancel verifies that cancelling the connection
// context in the middle of a multi-factor handshake aborts the connection
// attempt.
func TestMultiFactorAuthContextCancel(t *testing.T) {
	passwords := map[credentialsCall]string{
		{1, "mysql_native_password"}: "factor1-secret",
	}
	var (
		mu    sync.Mutex
		calls []credentialsCall
	)

	cfg := NewConfig()
	cfg.User = "mfa"
	cfg.Logger = &NopLogger{}
	if err := cfg.Apply(AuthCredentialsProvider(recordingProvider(passwords, &calls, &mu))); err != nil {
		t.Fatal(err)
	}

	scramble1 := testScramble(10)
	received := make(chan struct{})

	errCh := startMFAStubServer(cfg, func(s *mfaStub) error {
		if err := s.sendHandshake("mysql_native_password", scramble1); err != nil {
			return err
		}
		if _, err := s.readPacket(); err != nil {
			return err
		}
		// Hold off the next-factor request until the test cancels the
		// context; the client is expected to go away.
		close(received)
		pkt, err := s.readPacket()
		if err == nil {
			return fmt.Errorf("client unexpectedly sent %x after cancellation", pkt)
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-received
		cancel()
	}()

	_, err := newConnector(cfg).Connect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stub server: %v", err)
	}
}

// TestFormatDSNDoesNotLeakFactorCredentials verifies that credentials
// returned by the provider never end up in the DSN representation.
func TestFormatDSNDoesNotLeakFactorCredentials(t *testing.T) {
	cfg := NewConfig()
	cfg.User = "mfa"
	cfg.Passwd = "first-factor"
	err := cfg.Apply(AuthCredentialsProvider(CredentialsProviderFunc(
		func(_ context.Context, factor int, _ string) (string, error) {
			return fmt.Sprintf("factor-%d-secret", factor), nil
		},
	)))
	if err != nil {
		t.Fatal(err)
	}

	dsn := cfg.FormatDSN()
	if strings.Contains(dsn, "factor-2-secret") || strings.Contains(dsn, "factor-3-secret") {
		t.Fatalf("DSN leaks additional factor credentials: %q", dsn)
	}
	if !strings.Contains(dsn, "first-factor") {
		t.Fatalf("DSN lost the first factor password: %q", dsn)
	}
}
