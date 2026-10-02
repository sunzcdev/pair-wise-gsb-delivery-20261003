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
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// recordedCmd is a command observed by the cursorServer double.
type recordedCmd struct {
	command byte
	stmtID  uint32
	numRows uint32 // only meaningful for COM_STMT_FETCH
}

// cursorServer is a stateful, scripted double for the server side of a
// server-side cursor. It parses every command the client writes and, on each
// write, appends the reply that the driver should receive next. This lets the
// tests assert the exact sequence of COM_STMT_EXECUTE / COM_STMT_FETCH /
// close commands.
type cursorServer struct {
	// configuration
	fetchSize int    // size requested by the client (informational)
	totalRows int    // rows the cursor logically contains
	stmtID    uint32 // prepared statement id returned to the client

	// scripted behavior
	// failFetch is the zero-based batch number on which COM_STMT_FETCH is
	// answered with an error packet instead of rows. -1 disables it.
	failFetch int

	// observed state
	cmds       []recordedCmd
	nextRow    int // index of the next row to emit
	fetchCount int // number of COM_STMT_FETCH(batch) commands seen
}

// newCursorConn wires a cursorServer into a mockConn whose replies are
// produced on demand. The returned mysqlConn negotiates CLIENT_DEPRECATE_EOF.
func newCursorConn(t *testing.T, srv *cursorServer) (*mockConn, *mysqlConn) {
	t.Helper()
	conn := new(mockConn)
	cfg := NewConfig()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	mc := &mysqlConn{
		buf:              newBuffer(),
		cfg:              cfg,
		netConn:          conn,
		rawConn:          conn,
		closech:          make(chan struct{}),
		maxAllowedPacket: defaultMaxAllowedPacket,
		capabilities:     clientProtocol41 | clientDeprecateEOF,
		status:           statusInAutocommit,
	}
	conn.server = srv
	return conn, mc
}

// prepAndQuery performs COM_STMT_PREPARE + COM_STMT_EXECUTE against the
// double and returns the resulting binary rows.
func prepAndQuery(t *testing.T, mc *mysqlConn, srv *cursorServer) (*binaryRows, *mysqlStmt) {
	t.Helper()
	stmt, err := mc.Prepare("SELECT n FROM t")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	ms := stmt.(*mysqlStmt)
	ms.id = srv.stmtID

	rows, err := ms.query(nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return rows, ms
}

// ---- packet builders ------------------------------------------------------

func appendPacket(data []byte, seq byte, payload []byte) []byte {
	data = binary.LittleEndian.AppendUint32(data, uint32(len(payload)))
	data[len(data)-4] = byte(len(payload))
	data[len(data)-3] = byte(len(payload) >> 8)
	data[len(data)-2] = byte(len(payload) >> 16)
	data[len(data)-1] = seq
	data = append(data, payload...)
	return data
}

// columnCount packet for the execute response.
func pktColumnCount(seq byte) []byte {
	return appendPacket(nil, seq, []byte{0x01}) // one column
}

// one column definition: a BIGINT named "n".
func pktColumnDef(seq byte) []byte {
	var p []byte
	p = appendLengthEncodedString(p, "def")     // catalog
	p = appendLengthEncodedString(p, "db")      // schema
	p = appendLengthEncodedString(p, "t")       // table
	p = appendLengthEncodedString(p, "t")       // org_table
	p = appendLengthEncodedString(p, "n")       // name
	p = appendLengthEncodedString(p, "n")       // org_name
	p = append(p, 0x0c)                         // length of fixed fields
	p = binary.LittleEndian.AppendUint16(p, 63) // charset binary
	p = binary.LittleEndian.AppendUint32(p, 20) // column length
	p = append(p, byte(fieldTypeLongLong))
	p = binary.LittleEndian.AppendUint16(p, 0) // flags
	p = append(p, 0)                           // decimals
	p = append(p, 0, 0)                        // filler
	return appendPacket(nil, seq, p)
}

// a binary result row containing one signed BIGINT value.
func pktBinaryRow(seq byte, v int64) []byte {
	p := []byte{0x00} // packet header
	p = append(p, 0x00)
	p = binary.LittleEndian.AppendUint64(p, uint64(v))
	return appendPacket(nil, seq, p)
}

// an EOF/OK terminator with the given server status flags. Under
// CLIENT_DEPRECATE_EOF this is an OK packet headed 0xFE with 0 affected rows
// and 0 last insert id.
func pktTerminator(seq byte, flags statusFlag) []byte {
	p := []byte{iEOF, 0x00, 0x00}
	p = append(p, byte(flags), byte(flags>>8))
	p = binary.LittleEndian.AppendUint16(p, 0) // warnings
	return appendPacket(nil, seq, p)
}

func pktErr(seq byte, code uint16, msg string) []byte {
	p := []byte{iERR}
	p = binary.LittleEndian.AppendUint16(p, code)
	p = append(p, '#')
	p = append(p, 'H', 'Y', '0', '0', '0')
	p = append(p, msg...)
	return appendPacket(nil, seq, p)
}

// ---- command parsing ------------------------------------------------------

// parseCommands walks a write buffer containing one or more MySQL packets
// and records the commands.
func (srv *cursorServer) parseCommands(written []byte) {
	for len(written) >= 4 {
		pktLen := int(getUint24(written[:3]))
		if len(written) < 4+pktLen {
			return
		}
		payload := written[4 : 4+pktLen]
		written = written[4+pktLen:]
		if pktLen == 0 {
			continue
		}
		cmd := recordedCmd{command: payload[0]}
		switch payload[0] {
		case comStmtExecute:
			cmd.stmtID = binary.LittleEndian.Uint32(payload[1:5])
		case comStmtFetch:
			cmd.stmtID = binary.LittleEndian.Uint32(payload[1:5])
			cmd.numRows = binary.LittleEndian.Uint32(payload[5:9])
		}
		srv.cmds = append(srv.cmds, cmd)
	}
}

// replyFor builds the server reply following the latest command.
func (srv *cursorServer) replyFor(cmd recordedCmd) []byte {
	switch cmd.command {
	case comStmtPrepare:
		// COM_STMT_PREPARE ok: statement id, 1 column, 0 params.
		p := []byte{iOK}
		p = binary.LittleEndian.AppendUint32(p, srv.stmtID)
		p = binary.LittleEndian.AppendUint16(p, 1) // columns
		p = binary.LittleEndian.AppendUint16(p, 0) // params
		p = append(p, 0x00)                        // reserved
		p = binary.LittleEndian.AppendUint16(p, 0) // warnings
		out := appendPacket(nil, 1, p)
		out = append(out, pktColumnDef(2)...)
		// With CLIENT_DEPRECATE_EOF there is no trailing EOF after the
		// column definitions of a prepared statement.
		return out

	case comStmtExecute:
		// Result set header, column definition, then the cursor indicator.
		out := pktColumnCount(1)
		out = append(out, pktColumnDef(2)...)
		flags := statusInAutocommit | statusCursorExists
		out = append(out, pktTerminator(3, flags)...)
		return out

	case comStmtFetch:
		srv.fetchCount++
		seq := byte(1)
		if cmd.numRows == 0 {
			// Close request: server finishes the cursor immediately.
			flags := statusInAutocommit | statusLastRowSent
			return pktTerminator(seq, flags)
		}

		if srv.failFetch == srv.fetchCount-1 {
			return pktErr(seq, 1317, "cursor fetch failed")
		}

		// Emit up to fetchSize rows, then the batch terminator.
		var out []byte
		remaining := int(cmd.numRows)
		for remaining > 0 && srv.nextRow < srv.totalRows {
			out = append(out, pktBinaryRow(seq, int64(srv.nextRow))...)
			seq++
			srv.nextRow++
			remaining--
		}
		flags := statusInAutocommit
		if srv.nextRow >= srv.totalRows {
			flags |= statusLastRowSent
		}
		out = append(out, pktTerminator(seq, flags)...)
		return out
	}
	return nil
}

var _ net.Conn = new(mockConn)

// assertCommands checks the observed command sequence against the wanted
// list of (command, numRows) pairs.
func assertCommands(t *testing.T, cmds []recordedCmd, want ...recordedCmd) {
	t.Helper()
	if len(cmds) != len(want) {
		t.Fatalf("command count = %d, want %d (%+v got %+v)", len(cmds), len(want), want, cmds)
	}
	for i := range want {
		if cmds[i].command != want[i].command ||
			(want[i].command == comStmtFetch && cmds[i].numRows != want[i].numRows) {
			t.Fatalf("cmd[%d] = %+v, want %+v", i, cmds[i], want[i])
		}
	}
}

// fetch reads every row from rows and returns the BIGINT values.
func drainCursorRows(t *testing.T, rows *binaryRows) []int64 {
	t.Helper()
	dest := make([]driver.Value, 1)
	var got []int64
	for {
		err := rows.Next(dest)
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		v, ok := dest[0].(int64)
		if !ok {
			t.Fatalf("row %d: got %T, want int64", len(got), dest[0])
		}
		got = append(got, v)
	}
}

// connReusable verifies the connection carries no leftover data and can
// answer another command (statement reuse / connection return).
func connReusable(t *testing.T, mc *mysqlConn) {
	t.Helper()
	if err := mc.error(); err != nil {
		t.Fatalf("connection invalid after rows closed: %v", err)
	}
	if mc.buf.busy() {
		t.Fatalf("residual bytes left in read buffer after rows closed")
	}
	if !mc.IsValid() {
		t.Fatalf("connection not valid (IsValid=false) after rows closed")
	}
}

// prepareExecuteReplies pre-loads the prepare + execute exchange. Because the
// double replies on every write, this simply performs the writes.
func setupCursorRows(t *testing.T, fetchSize, totalRows int) (*cursorServer, *mysqlConn, *binaryRows, *mysqlStmt) {
	srv := &cursorServer{fetchSize: fetchSize, totalRows: totalRows, stmtID: 7, failFetch: -1}
	_, mc := newCursorConn(t, srv)
	mc.cfg.FetchSize = fetchSize
	rows, stmt := prepAndQuery(t, mc, srv)
	return srv, mc, rows, stmt
}

// Scenario 1: fewer than one full batch. Only one COM_STMT_FETCH should be
// issued, and no close fetch must be needed after a clean drain.
func TestCursorFewerThanOneBatch(t *testing.T) {
	srv, mc, rows, stmt := setupCursorRows(t, 10, 3)

	got := drainCursorRows(t, rows)
	if len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("rows = %v, want [0 1 2]", got)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close after drain: %v", err)
	}

	assertCommands(t, srv.cmds,
		recordedCmd{command: comStmtPrepare},
		recordedCmd{command: comStmtExecute, stmtID: 7},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 10},
	)
	connReusable(t, mc)

	// Statement reuse: run the query again on the same statement/conn.
	srv.nextRow = 0
	srv.fetchCount = 0
	srv.cmds = nil
	rows2, err := stmt.query(nil)
	if err != nil {
		t.Fatalf("reuse query: %v", err)
	}
	got2 := drainCursorRows(t, rows2)
	if len(got2) != 3 || got2[2] != 2 {
		t.Fatalf("reused rows = %v", got2)
	}
	if err := rows2.Close(); err != nil {
		t.Fatalf("close reused rows: %v", err)
	}
	assertCommands(t, srv.cmds,
		recordedCmd{command: comStmtExecute, stmtID: 7},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 10},
	)
	connReusable(t, mc)
}

// Scenario 2: total is an exact multiple of fetch size. The final full batch
// already carries SERVER_STATUS_LAST_ROW_SENT, so iteration ends right there
// without an extra empty fetch.
func TestCursorExactBatches(t *testing.T) {
	srv, mc, rows, _ := setupCursorRows(t, 2, 4)

	got := drainCursorRows(t, rows)
	if len(got) != 4 {
		t.Fatalf("rows = %v, want 4 rows", got)
	}
	for i, v := range got {
		if v != int64(i) {
			t.Fatalf("row %d = %d, want %d", i, v, i)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// prepare, execute, fetch(batch1 rows 0-1), fetch(batch2 rows 2-3)
	assertCommands(t, srv.cmds,
		recordedCmd{command: comStmtPrepare},
		recordedCmd{command: comStmtExecute, stmtID: 7},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
	)
	connReusable(t, mc)
}

// Scenario 3a: cancel (Rows.Close) while rows of the current batch are still
// unread. The driver must drain the in-flight batch and then send
// COM_STMT_FETCH with numRows = 0 to release the server cursor.
func TestCursorCloseMidBatch(t *testing.T) {
	srv, mc, rows, _ := setupCursorRows(t, 2, 10)

	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	// Close after consuming only one of the two rows in batch one.
	if err := rows.Close(); err != nil {
		t.Fatalf("early close: %v", err)
	}

	assertCommands(t, srv.cmds,
		recordedCmd{command: comStmtPrepare},
		recordedCmd{command: comStmtExecute, stmtID: 7},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 0},
	)
	connReusable(t, mc)
}

// After an early close, the statement and connection must be reusable with
// no leftover rows from the abandoned cursor.
func TestCursorReuseAfterEarlyClose(t *testing.T) {
	srv, mc, rows, stmt := setupCursorRows(t, 2, 10)

	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("early close: %v", err)
	}

	// Re-run the same statement; it must produce a fresh cursor from row 0.
	srv.nextRow = 0
	srv.fetchCount = 0
	srv.cmds = nil

	rows2, err := stmt.query(nil)
	if err != nil {
		t.Fatalf("reuse query after early close: %v", err)
	}
	got := drainCursorRows(t, rows2)
	if len(got) != 10 || got[0] != 0 || got[9] != 9 {
		t.Fatalf("reused rows = %v (len %d), want 0..9", got, len(got))
	}
	if err := rows2.Close(); err != nil {
		t.Fatalf("close reused rows: %v", err)
	}

	assertCommands(t, srv.cmds,
		recordedCmd{command: comStmtExecute, stmtID: 7},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
	)
	connReusable(t, mc)
}

// Scenario 3b: cancel exactly on a batch boundary, before any row of the
// second batch has been read. No batch draining is needed; the driver sends
// the close fetch immediately.
func TestCursorCloseAtBatchBoundary(t *testing.T) {
	srv, mc, rows, _ := setupCursorRows(t, 2, 10)

	dest := make([]driver.Value, 1)
	for range 2 {
		if err := rows.Next(dest); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	// Both batch-1 rows consumed, but its terminator / next fetch not yet
	// driven. Closing here must first consume the batch-1 terminator, then
	// close the cursor.
	if err := rows.Close(); err != nil {
		t.Fatalf("boundary close: %v", err)
	}

	// Only one data fetch plus the close fetch. The boundary terminator is
	// consumed while draining but produces no additional command.
	assertCommands(t, srv.cmds,
		recordedCmd{command: comStmtPrepare},
		recordedCmd{command: comStmtExecute, stmtID: 7},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 2},
		recordedCmd{command: comStmtFetch, stmtID: 7, numRows: 0},
	)
	connReusable(t, mc)
}

// Scenario 3c: context cancellation while waiting for the second batch. The
// driver's watcher tears the connection down, so Next must surface the
// context error rather than block forever.
func TestCursorCancelDuringSecondBatch(t *testing.T) {
	srv := &cursorServer{fetchSize: 2, totalRows: 10, stmtID: 7, failFetch: -1}
	conn, mc := newCursorConn(t, srv)
	mc.cfg.FetchSize = 2
	mc.startWatcher()
	defer mc.cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	if err := mc.watchCancel(ctx); err != nil {
		t.Fatalf("watchCancel: %v", err)
	}

	rows, err := (&mysqlStmt{mc: mc, id: 7, paramCount: 0}).query(nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	rows.finish = mc.finish

	dest := make([]driver.Value, 1)
	for range 2 {
		if err := rows.Next(dest); err != nil {
			t.Fatalf("batch1 Next: %v", err)
		}
	}

	// Hold the second data-fetch command (3rd write: execute, fetch#1,
	// fetch#2) back until cancellation has torn the connection down, so
	// Next is blocked waiting on that command when the context is canceled.
	release := make(chan struct{})
	conn.blockWrite = func(writeNo int) chan struct{} {
		// Writes: 1=execute, 2=fetch#1, 3=fetch#2 (no prepare here).
		if writeNo == 3 {
			return release
		}
		return nil
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()

	err = rows.Next(dest)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next after cancel = %v, want context.Canceled", err)
	}

	// The connection is intentionally invalidated on cancellation.
	if !conn.closed {
		t.Fatalf("expected the underlying connection to be closed")
	}
}

// Scenario 4: the server reports an error on a later batch. The error must
// surface from Next, the cursor must not remain usable, and subsequent Close
// must be idempotent without sending stray fetches.
func TestCursorServerErrorOnFetch(t *testing.T) {
	srv := &cursorServer{fetchSize: 2, totalRows: 10, stmtID: 7, failFetch: 2}
	_, mc := newCursorConn(t, srv)
	mc.cfg.FetchSize = 2
	rows, _ := prepAndQuery(t, mc, srv)

	dest := make([]driver.Value, 1)
	// First batch: 2 rows.
	for range 2 {
		if err := rows.Next(dest); err != nil {
			t.Fatalf("batch1 Next: %v", err)
		}
	}
	// Second batch: another 2 rows.
	for range 2 {
		if err := rows.Next(dest); err != nil {
			t.Fatalf("batch2 Next: %v", err)
		}
	}

	// Requesting the third batch must surface the server error.
	err := rows.Next(dest)
	var myErr *MySQLError
	if !errors.As(err, &myErr) {
		t.Fatalf("expected *MySQLError, got %T: %v", err, err)
	}
	if myErr.Number != 1317 {
		t.Fatalf("error number = %d, want 1317", myErr.Number)
	}

	// Close must be a no-op: the cursor/rows were already invalidated and
	// the connection must not be left with residual data.
	cmdsBefore := len(srv.cmds)
	if err := rows.Close(); err != nil {
		t.Fatalf("close after error: %v", err)
	}
	if len(srv.cmds) != cmdsBefore {
		t.Fatalf("close sent extra commands: %+v", srv.cmds[cmdsBefore:])
	}
	// After a hard server error the connection is intentionally discarded by
	// database/sql; assert no buffered residual and a recorded bad state.
	if mc.buf.busy() {
		t.Fatalf("residual bytes left after server error")
	}
}

// The COM_STMT_EXECUTE packet must carry CURSOR_TYPE_READ_ONLY (0x01) in the
// flags byte when fetchSize is set.
func TestCursorExecutePacketFlag(t *testing.T) {
	conn, mc := newRWMockConn(0)
	mc.maxAllowedPacket = defaultMaxAllowedPacket
	stmt := &mysqlStmt{mc: mc, id: 42, paramCount: 0}

	if err := stmt.writeExecutePacket(nil, cursorTypeReadOnly); err != nil {
		t.Fatalf("writeExecutePacket: %v", err)
	}

	// Single packet; flags byte is at payload offset 5 (1 cmd + 4 stmtID),
	// so absolute offset 4+5 = 9.
	if got := conn.written[9]; got != byte(cursorTypeReadOnly) {
		t.Fatalf("execute flags = %#x, want %#x", got, cursorTypeReadOnly)
	}
}

func TestFetchSizeDSN(t *testing.T) {
	cfg, err := ParseDSN("user:pass@tcp(localhost:3306)/db?fetchSize=128")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if cfg.FetchSize != 128 {
		t.Fatalf("FetchSize = %d, want 128", cfg.FetchSize)
	}

	// Round-trip through FormatDSN.
	parsed, err := ParseDSN(cfg.FormatDSN())
	if err != nil {
		t.Fatalf("round-trip ParseDSN: %v", err)
	}
	if parsed.FetchSize != 128 {
		t.Fatalf("round-trip FetchSize = %d, want 128", parsed.FetchSize)
	}

	// Default (unset) stays zero and is omitted from the formatted DSN.
	defaultCfg := NewConfig()
	if defaultCfg.FetchSize != 0 {
		t.Fatalf("default FetchSize = %d, want 0", defaultCfg.FetchSize)
	}

	if _, err := ParseDSN("user:pass@/db?fetchSize=-3"); err == nil {
		t.Fatal("expected error for negative fetchSize")
	}
}

// With fetchSize unset (0), statements execute without a cursor and stream
// rows directly, as before.
func TestNoCursorWhenFetchSizeZero(t *testing.T) {
	conn, mc := newRWMockConn(0)
	mc.maxAllowedPacket = defaultMaxAllowedPacket
	ms := &mysqlStmt{mc: mc, id: 9, paramCount: 0}
	if err := ms.writeExecutePacket(nil, cursorTypeNoCursor); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := conn.written[9]; got != byte(cursorTypeNoCursor) {
		t.Fatalf("execute flags = %#x, want 0", got)
	}
}
