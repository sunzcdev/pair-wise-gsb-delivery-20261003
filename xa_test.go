// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// xaFakeServer is a minimal MySQL protocol stand-in that understands the
// XA statements. It keeps prepared branches server-side across
// connections, like a real server, and can drop responses to simulate
// lost packets.
type xaFakeServer struct {
	ln net.Listener

	mu         sync.Mutex
	prepared   map[string]XID
	committed  []XID
	rolledBack []XID
	queries    []string

	// dropOnPrepare / dropOnCommit apply the statement server-side but
	// close the connection without sending the response.
	dropOnPrepare bool
	dropOnCommit  bool
}

func newXAFakeServer(t *testing.T) *xaFakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &xaFakeServer{ln: ln, prepared: make(map[string]XID)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func xaKey(xid XID) string {
	return fmt.Sprintf("%d/%s/%s", xid.FormatID, xid.GlobalTransactionID, xid.BranchQualifier)
}

func (s *xaFakeServer) connect(t *testing.T) *mysqlConn {
	t.Helper()
	cfg := NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = s.ln.Addr().String()
	cfg.User = "xa"
	cfg.DBName = "test"
	connector, err := NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := connector.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mc, ok := conn.(*mysqlConn)
	if !ok {
		t.Fatalf("expected *mysqlConn, got %T", conn)
	}
	t.Cleanup(func() { mc.Close() })
	return mc
}

func (s *xaFakeServer) logQuery(q string) {
	s.mu.Lock()
	s.queries = append(s.queries, q)
	s.mu.Unlock()
}

func (s *xaFakeServer) loggedQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

func (s *xaFakeServer) serve(c net.Conn) {
	defer c.Close()
	// Initial handshake, then accept any authentication.
	if err := writeSrvPacket(c, 0, xaHandshakePayload()); err != nil {
		return
	}
	if _, err := readSrvPacket(c); err != nil {
		return
	}
	if err := writeSrvPacket(c, 2, xaOKPacket()); err != nil {
		return
	}
	for {
		p, err := readSrvPacket(c)
		if err != nil || len(p) == 0 {
			return
		}
		switch p[0] {
		case comQuit:
			return
		case comQuery:
			if !s.handleQuery(c, string(p[1:])) {
				return
			}
		default:
			writeSrvPacket(c, 1, xaErrPacket(1047, "08S01", "unsupported command"))
		}
	}
}

// handleQuery handles one COM_QUERY. It returns false if the connection
// must be closed without a response.
func (s *xaFakeServer) handleQuery(c net.Conn, q string) bool {
	s.logQuery(q)
	upper := strings.ToUpper(q)
	switch {
	case strings.HasPrefix(upper, "XA START "):
		if _, err := parseXIDLiterals(q[len("XA START "):]); err != nil {
			writeSrvPacket(c, 1, xaErrPacket(1064, "42000", "malformed xid"))
			return true
		}
		writeSrvPacket(c, 1, xaOKPacket())
	case strings.HasPrefix(upper, "XA END "):
		writeSrvPacket(c, 1, xaOKPacket())
	case strings.HasPrefix(upper, "XA PREPARE "):
		xid, err := parseXIDLiterals(q[len("XA PREPARE "):])
		if err != nil {
			writeSrvPacket(c, 1, xaErrPacket(1064, "42000", "malformed xid"))
			return true
		}
		s.mu.Lock()
		s.prepared[xaKey(xid)] = xid
		drop := s.dropOnPrepare
		s.mu.Unlock()
		if drop {
			return false
		}
		writeSrvPacket(c, 1, xaOKPacket())
	case strings.HasPrefix(upper, "XA COMMIT "):
		xid, err := parseXIDLiterals(q[len("XA COMMIT "):])
		if err != nil {
			writeSrvPacket(c, 1, xaErrPacket(1064, "42000", "malformed xid"))
			return true
		}
		s.mu.Lock()
		key := xaKey(xid)
		if _, ok := s.prepared[key]; !ok {
			s.mu.Unlock()
			writeSrvPacket(c, 1, xaErrPacket(1397, "XAE04", "XAER_NOTA: Unknown XID"))
			return true
		}
		delete(s.prepared, key)
		s.committed = append(s.committed, xid)
		drop := s.dropOnCommit
		s.mu.Unlock()
		if drop {
			return false
		}
		writeSrvPacket(c, 1, xaOKPacket())
	case strings.HasPrefix(upper, "XA ROLLBACK "):
		xid, err := parseXIDLiterals(q[len("XA ROLLBACK "):])
		if err != nil {
			writeSrvPacket(c, 1, xaErrPacket(1064, "42000", "malformed xid"))
			return true
		}
		s.mu.Lock()
		key := xaKey(xid)
		delete(s.prepared, key)
		s.rolledBack = append(s.rolledBack, xid)
		s.mu.Unlock()
		writeSrvPacket(c, 1, xaOKPacket())
	case upper == "XA RECOVER":
		s.writeRecoverResult(c)
	default:
		writeSrvPacket(c, 1, xaErrPacket(1064, "42000", "unsupported statement"))
	}
	return true
}

func (s *xaFakeServer) writeRecoverResult(c net.Conn) {
	s.mu.Lock()
	xids := make([]XID, 0, len(s.prepared))
	for _, xid := range s.prepared {
		xids = append(xids, xid)
	}
	s.mu.Unlock()

	seq := byte(1)
	write := func(p []byte) {
		if err := writeSrvPacket(c, seq, p); err == nil {
			seq++
		}
	}
	write([]byte{4}) // column count
	write(xaColumnDef("formatID", byte(fieldTypeLongLong)))
	write(xaColumnDef("gtrid_length", byte(fieldTypeLongLong)))
	write(xaColumnDef("bqual_length", byte(fieldTypeLongLong)))
	write(xaColumnDef("data", byte(fieldTypeBLOB)))
	write(xaEOFPacket())
	for _, xid := range xids {
		var row []byte
		row = appendLenEncStr(row, strconv.FormatInt(int64(xid.FormatID), 10))
		row = appendLenEncStr(row, strconv.Itoa(len(xid.GlobalTransactionID)))
		row = appendLenEncStr(row, strconv.Itoa(len(xid.BranchQualifier)))
		data := make([]byte, 0, len(xid.GlobalTransactionID)+len(xid.BranchQualifier))
		data = append(data, xid.GlobalTransactionID...)
		data = append(data, xid.BranchQualifier...)
		row = appendLenEncBytes(row, data)
		write(row)
	}
	write(xaEOFPacket())
}

// readSrvPacket reads one protocol packet from the client.
func readSrvPacket(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return body, err
}

// writeSrvPacket writes one protocol packet to the client.
func writeSrvPacket(w io.Writer, seq byte, body []byte) error {
	hdr := []byte{byte(len(body)), byte(len(body) >> 8), byte(len(body) >> 16), seq}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func xaHandshakePayload() []byte {
	caps := uint32(clientMySQL) | uint32(clientLongFlag) | uint32(clientProtocol41) |
		uint32(clientSecureConn) | uint32(clientTransactions) |
		uint32(clientPluginAuth) | uint32(clientMultiResults)
	var b []byte
	b = append(b, 10) // protocol version
	b = append(b, "8.0.0-xafake"...)
	b = append(b, 0)
	b = append(b, 1, 0, 0, 0)             // connection id
	b = append(b, 1, 2, 3, 4, 5, 6, 7, 8) // auth data, part 1
	b = append(b, 0)                      // filler
	b = append(b, byte(caps), byte(caps>>8))
	b = append(b, 45)         // charset: utf8mb4_general_ci
	b = append(b, 0x02, 0x00) // status: autocommit
	b = append(b, byte(caps>>16), byte(caps>>24))
	b = append(b, 21)                                               // auth plugin data length
	b = append(b, make([]byte, 10)...)                              // reserved
	b = append(b, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 0) // auth data, part 2
	b = append(b, defaultAuthPlugin...)
	b = append(b, 0)
	return b
}

func xaOKPacket() []byte {
	// header, affected rows, insert id, status (autocommit), warnings
	return []byte{iOK, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
}

func xaEOFPacket() []byte {
	// header, warnings, status (autocommit)
	return []byte{iEOF, 0x00, 0x00, 0x02, 0x00}
}

func xaErrPacket(num uint16, state, msg string) []byte {
	p := []byte{iERR, byte(num), byte(num >> 8), '#'}
	p = append(p, state...)
	return append(p, msg...)
}

func xaColumnDef(name string, fieldType byte) []byte {
	var b []byte
	b = appendLenEncStr(b, "def") // catalog
	b = appendLenEncStr(b, "")    // schema
	b = appendLenEncStr(b, "")    // table
	b = appendLenEncStr(b, "")    // original table
	b = appendLenEncStr(b, name)  // name
	b = appendLenEncStr(b, "")    // original name
	b = append(b, 0x0c)           // length of fixed fields
	b = append(b, 0x3f, 0x00)     // charset: binary
	b = append(b, 0, 0, 0, 0)     // column length
	b = append(b, fieldType)
	b = append(b, 0, 0) // flags
	b = append(b, 0)    // decimals
	b = append(b, 0, 0) // reserved
	return b
}

func appendLenEncStr(b []byte, s string) []byte {
	return appendLenEncBytes(b, []byte(s))
}

func appendLenEncBytes(b, v []byte) []byte {
	if len(v) >= 251 {
		panic("test helper only supports short length encoded strings")
	}
	b = append(b, byte(len(v)))
	return append(b, v...)
}

var errXIDParse = errors.New("malformed xid literals")

// parseXIDLiterals parses the SQL literal form 'gtrid','bqual',formatID,
// reversing the encoding produced by XID.appendSQL with backslash
// escaping.
func parseXIDLiterals(s string) (XID, error) {
	var xid XID
	i := 0

	skipSpace := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	parseString := func() ([]byte, error) {
		skipSpace()
		if i >= len(s) || s[i] != '\'' {
			return nil, errXIDParse
		}
		i++
		var v []byte
		for i < len(s) {
			switch s[i] {
			case '\\':
				if i+1 >= len(s) {
					return nil, errXIDParse
				}
				i++
				switch s[i] {
				case '0':
					v = append(v, 0)
				case 'n':
					v = append(v, '\n')
				case 'r':
					v = append(v, '\r')
				case 'Z':
					v = append(v, 0x1a)
				default:
					v = append(v, s[i])
				}
				i++
			case '\'':
				if i+1 < len(s) && s[i+1] == '\'' {
					v = append(v, '\'')
					i += 2
				} else {
					i++
					return v, nil
				}
			default:
				v = append(v, s[i])
				i++
			}
		}
		return nil, errXIDParse
	}
	parseComma := func() error {
		skipSpace()
		if i >= len(s) || s[i] != ',' {
			return errXIDParse
		}
		i++
		return nil
	}

	gtrid, err := parseString()
	if err != nil {
		return xid, err
	}
	if err := parseComma(); err != nil {
		return xid, err
	}
	bqual, err := parseString()
	if err != nil {
		return xid, err
	}
	if err := parseComma(); err != nil {
		return xid, err
	}
	skipSpace()
	end := i
	for end < len(s) && s[end] != ' ' && s[end] != '\t' {
		end++
	}
	formatID, err := strconv.ParseInt(s[i:end], 10, 32)
	if err != nil {
		return xid, errXIDParse
	}

	xid.FormatID = int32(formatID)
	xid.GlobalTransactionID = gtrid
	xid.BranchQualifier = bqual
	return xid, nil
}

func TestXIDValidate(t *testing.T) {
	valid := XID{FormatID: 1, GlobalTransactionID: []byte("g"), BranchQualifier: []byte("b")}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid XID, got %v", err)
	}
	if err := (XID{FormatID: 1}).Validate(); err == nil {
		t.Fatal("expected error for empty gtrid")
	}
	tooLongG := XID{FormatID: 1, GlobalTransactionID: make([]byte, XAMaxGTrIDLength+1)}
	if err := tooLongG.Validate(); err == nil {
		t.Fatal("expected error for oversized gtrid")
	}
	tooLongB := XID{FormatID: 1, GlobalTransactionID: []byte("g"), BranchQualifier: make([]byte, XAMaxBQualLength+1)}
	if err := tooLongB.Validate(); err == nil {
		t.Fatal("expected error for oversized bqual")
	}
	maxOK := XID{FormatID: 1, GlobalTransactionID: make([]byte, XAMaxGTrIDLength), BranchQualifier: make([]byte, XAMaxBQualLength)}
	if err := maxOK.Validate(); err != nil {
		t.Fatalf("expected valid XID at size limits, got %v", err)
	}
}

func TestXIDString(t *testing.T) {
	xid := XID{
		FormatID:            42,
		GlobalTransactionID: []byte{'g', '\'', '\\', 0x00, 0xff},
		BranchQualifier:     []byte("b'q"),
	}
	want := "'g\\'\\\\\\0" + "\xff" + "','b\\'q',42"
	if got := xid.String(); got != want {
		t.Fatalf("unexpected SQL form:\n got: %q\nwant: %q", got, want)
	}

	// Round-trip through the literal parser used by the fake server must
	// preserve all components byte-for-byte.
	parsed, err := parseXIDLiterals(xid.String())
	if err != nil {
		t.Fatal(err)
	}
	if !xid.Equal(parsed) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", parsed, xid)
	}

	// NO_BACKSLASH_ESCAPES mode must double quotes instead.
	noEsc := string(xid.appendSQL(nil, true))
	wantNoEsc := "'g''\\\x00" + "\xff" + "','b''q',42"
	if noEsc != wantNoEsc {
		t.Fatalf("unexpected NO_BACKSLASH_ESCAPES form:\n got: %q\nwant: %q", noEsc, wantNoEsc)
	}
}

func TestXALifecycle(t *testing.T) {
	srv := newXAFakeServer(t)
	mc := srv.connect(t)

	xid := XID{FormatID: 1, GlobalTransactionID: []byte("gtrid-1"), BranchQualifier: []byte("branch-1")}

	branch, err := mc.XAStart(xid)
	if err != nil {
		t.Fatal(err)
	}
	if branch.State() != XAStateActive {
		t.Fatalf("expected active, got %s", branch.State())
	}
	if err := branch.End(); err != nil {
		t.Fatal(err)
	}
	if branch.State() != XAStateIdle {
		t.Fatalf("expected idle, got %s", branch.State())
	}
	if err := branch.Prepare(); err != nil {
		t.Fatal(err)
	}
	if branch.State() != XAStatePrepared {
		t.Fatalf("expected prepared, got %s", branch.State())
	}
	if err := branch.Commit(mc); err != nil {
		t.Fatal(err)
	}
	if branch.State() != XAStateCommitted {
		t.Fatalf("expected committed, got %s", branch.State())
	}

	want := []string{
		"XA START 'gtrid-1','branch-1',1",
		"XA END 'gtrid-1','branch-1',1",
		"XA PREPARE 'gtrid-1','branch-1',1",
		"XA COMMIT 'gtrid-1','branch-1',1",
	}
	got := srv.loggedQueries()
	if len(got) != len(want) {
		t.Fatalf("unexpected queries: %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("query %d:\n got: %q\nwant: %q", i, got[i], want[i])
		}
	}
}

func TestXAIllegalTransitions(t *testing.T) {
	srv := newXAFakeServer(t)
	mc := srv.connect(t)

	xid := XID{FormatID: 1, GlobalTransactionID: []byte("g"), BranchQualifier: []byte("b")}
	branch, err := mc.XAStart(xid)
	if err != nil {
		t.Fatal(err)
	}

	// Prepare requires the branch to be ended first.
	if err := branch.Prepare(); !errors.Is(err, ErrXAIllegalState) {
		t.Fatalf("expected ErrXAIllegalState, got %v", err)
	}
	// Commit requires a prepared branch.
	if err := branch.Commit(mc); !errors.Is(err, ErrXAIllegalState) {
		t.Fatalf("expected ErrXAIllegalState, got %v", err)
	}
	if err := branch.End(); err != nil {
		t.Fatal(err)
	}
	// Ending twice is illegal.
	if err := branch.End(); !errors.Is(err, ErrXAIllegalState) {
		t.Fatalf("expected ErrXAIllegalState, got %v", err)
	}
	if err := branch.Prepare(); err != nil {
		t.Fatal(err)
	}
	// After prepare the branch is detached; End/Prepare are illegal.
	if err := branch.End(); !errors.Is(err, ErrXAIllegalState) {
		t.Fatalf("expected ErrXAIllegalState, got %v", err)
	}
	if err := branch.Prepare(); !errors.Is(err, ErrXAIllegalState) {
		t.Fatalf("expected ErrXAIllegalState, got %v", err)
	}
	if err := branch.Rollback(mc); err != nil {
		t.Fatal(err)
	}
	// A resolved branch cannot be resolved again.
	if err := branch.Commit(mc); !errors.Is(err, ErrXAIllegalState) {
		t.Fatalf("expected ErrXAIllegalState, got %v", err)
	}

	// Illegal transitions must be rejected locally, without sending
	// anything to the server.
	want := []string{
		"XA START 'g','b',1",
		"XA END 'g','b',1",
		"XA PREPARE 'g','b',1",
		"XA ROLLBACK 'g','b',1",
	}
	got := srv.loggedQueries()
	if len(got) != len(want) {
		t.Fatalf("unexpected queries: %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("query %d:\n got: %q\nwant: %q", i, got[i], want[i])
		}
	}
}

// TestXARecoverResolveNewConnection walks the recovery flow: a branch is
// prepared, its connection is lost, and a new connection discovers the
// prepared branch via XA RECOVER and resolves it.
func TestXARecoverResolveNewConnection(t *testing.T) {
	srv := newXAFakeServer(t)

	xid := XID{
		FormatID:            7,
		GlobalTransactionID: []byte{'g', 0x00, '\'', '\\', 0xff},
		BranchQualifier:     []byte("b'q"),
	}

	mc1 := srv.connect(t)
	branch, err := mc1.XAStart(xid)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.End(); err != nil {
		t.Fatal(err)
	}
	if err := branch.Prepare(); err != nil {
		t.Fatal(err)
	}

	// The connection goes away. A prepared branch must not be rolled back
	// like a local transaction on connection cleanup.
	if err := mc1.Close(); err != nil {
		t.Fatal(err)
	}
	for _, q := range srv.loggedQueries() {
		if strings.Contains(strings.ToUpper(q), "ROLLBACK") {
			t.Fatalf("prepared branch must not be rolled back on connection close, saw %q", q)
		}
	}

	// A different connection discovers the prepared branch.
	mc2 := srv.connect(t)
	xids, err := mc2.XARecover()
	if err != nil {
		t.Fatal(err)
	}
	if len(xids) != 1 {
		t.Fatalf("expected 1 prepared branch, got %d", len(xids))
	}
	if !xids[0].Equal(xid) {
		t.Fatalf("recovered XID mismatch:\n got: %+v\nwant: %+v", xids[0], xid)
	}

	// The caller makes the final decision on the new connection.
	if err := mc2.XACommit(xids[0]); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	committed := len(srv.committed) == 1 && srv.committed[0].Equal(xid)
	srv.mu.Unlock()
	if !committed {
		t.Fatal("server did not record the commit")
	}

	xids, err = mc2.XARecover()
	if err != nil {
		t.Fatal(err)
	}
	if len(xids) != 0 {
		t.Fatalf("expected no prepared branches after commit, got %d", len(xids))
	}
}

// TestXAPrepareIndeterminate verifies that a lost PREPARE response keeps
// the outcome indeterminate instead of guessing.
func TestXAPrepareIndeterminate(t *testing.T) {
	srv := newXAFakeServer(t)
	srv.dropOnPrepare = true
	mc := srv.connect(t)

	xid := XID{FormatID: 1, GlobalTransactionID: []byte("g"), BranchQualifier: []byte("b")}
	branch, err := mc.XAStart(xid)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.End(); err != nil {
		t.Fatal(err)
	}
	err = branch.Prepare()
	if !errors.Is(err, ErrXAIndeterminate) {
		t.Fatalf("expected ErrXAIndeterminate, got %v", err)
	}
	if branch.State() != XAStateUnknown {
		t.Fatalf("expected unknown state, got %s", branch.State())
	}

	// The branch may in fact be prepared on the server; it can be found
	// from another connection and resolved there.
	mc2 := srv.connect(t)
	xids, err := mc2.XARecover()
	if err != nil {
		t.Fatal(err)
	}
	if len(xids) != 1 || !xids[0].Equal(xid) {
		t.Fatalf("unexpected recovered xids: %+v", xids)
	}
	if err := mc2.XARollback(xids[0]); err != nil {
		t.Fatal(err)
	}
}

// TestXACommitIndeterminate verifies that a lost COMMIT response is
// reported as indeterminate. The driver must not declare success, and
// must not infer it later from the XID being absent from XA RECOVER.
func TestXACommitIndeterminate(t *testing.T) {
	srv := newXAFakeServer(t)
	mc := srv.connect(t)

	xid := XID{FormatID: 1, GlobalTransactionID: []byte("g"), BranchQualifier: []byte("b")}
	branch, err := mc.XAStart(xid)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.End(); err != nil {
		t.Fatal(err)
	}
	if err := branch.Prepare(); err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	srv.dropOnCommit = true
	srv.mu.Unlock()

	err = branch.Commit(mc)
	if !errors.Is(err, ErrXAIndeterminate) {
		t.Fatalf("expected ErrXAIndeterminate, got %v", err)
	}
	if branch.State() != XAStateUnknown {
		t.Fatalf("expected unknown state, got %s", branch.State())
	}

	// The commit was in fact applied server-side, so the XID is gone from
	// XA RECOVER. Absence must not be treated as proof of success: the
	// branch stays in the unknown state and the error stands.
	mc2 := srv.connect(t)
	xids, err := mc2.XARecover()
	if err != nil {
		t.Fatal(err)
	}
	if len(xids) != 0 {
		t.Fatalf("expected no prepared branches, got %+v", xids)
	}
	if branch.State() != XAStateUnknown {
		t.Fatalf("branch state must remain unknown, got %s", branch.State())
	}
}

// TestXARollbackFromIdle verifies that an ended but not yet prepared
// branch can be rolled back on its attached connection.
func TestXARollbackFromIdle(t *testing.T) {
	srv := newXAFakeServer(t)
	mc := srv.connect(t)

	xid := XID{FormatID: 1, GlobalTransactionID: []byte("g"), BranchQualifier: []byte("b")}
	branch, err := mc.XAStart(xid)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.End(); err != nil {
		t.Fatal(err)
	}
	if err := branch.Rollback(nil); err != nil {
		t.Fatal(err)
	}
	if branch.State() != XAStateRolledBack {
		t.Fatalf("expected rolled back, got %s", branch.State())
	}
	srv.mu.Lock()
	n := len(srv.rolledBack)
	srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 rolled back branch, got %d", n)
	}
}
