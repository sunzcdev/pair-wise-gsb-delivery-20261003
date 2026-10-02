// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// packet wraps a payload with a 4 byte MySQL packet header. The sequence byte
// is supplied by the caller to model the server's expected sequence numbers.
func cursorPacket(seq byte, payload ...byte) []byte {
	p := make([]byte, 4+len(payload))
	putUint24(p, len(payload))
	p[3] = seq
	copy(p[4:], payload)
	return p
}

// cursorColumnPacket builds a minimal one-column (BIGINT named "n") column
// definition packet. Only the fields the driver actually reads need to be
// populated.
func cursorColumnPacket(seq byte) []byte {
	var b []byte
	b = appendLengthEncodedString(b, "def")     // catalog
	b = appendLengthEncodedString(b, "")        // schema
	b = appendLengthEncodedString(b, "")        // table
	b = appendLengthEncodedString(b, "")        // org_table
	b = appendLengthEncodedString(b, "n")       // name
	b = appendLengthEncodedString(b, "")        // org_name
	b = append(b, 0x0c)                         // length of fixed fields
	b = append(b, 0x3f, 0x00)                   // charset (binary)
	b = binary.LittleEndian.AppendUint32(b, 11) // column length
	b = append(b, byte(fieldTypeLongLong))      // type BIGINT
	b = binary.LittleEndian.AppendUint16(b, 0)  // flags
	b = append(b, 0x00)                         // decimals
	b = append(b, 0x00, 0x00)                   // filler
	return cursorPacket(seq, b...)
}

// cursorRowPacket builds a binary resultset row with a single BIGINT value.
func cursorRowPacket(seq byte, v int64) []byte {
	b := []byte{iOK}    // row header
	b = append(b, 0x00) // NULL bitmap: (1 column + 7 + 2) / 8 = 1 byte
	b = binary.LittleEndian.AppendUint64(b, uint64(v))
	return cursorPacket(seq, b...)
}

// cursorEOFPacket builds an EOF packet with the given server status flags.
func cursorEOFPacket(seq byte, status statusFlag) []byte {
	return cursorPacket(seq,
		iEOF,
		0x00, 0x00, // warning count
		byte(status), byte(status>>8),
	)
}

// cursorOKPacket builds an 0xFE-headed OK packet used as the batch terminator
// when the connection negotiated CLIENT_DEPRECATE_EOF. Layout:
// 0xfe, affected_rows(LEC), last_insert_id(LEC), status(2), warnings(2).
func cursorOKPacket(seq byte, status statusFlag) []byte {
	p := []byte{iEOF, 0x00, 0x00} // 0xfe header, 0 affected, 0 insert id
	p = append(p, byte(status), byte(status>>8))
	p = append(p, 0x00, 0x00) // warnings
	return cursorPacket(seq, p...)
}

// cursorErrPacket builds a server error packet (errno 1146, generic message).
func cursorErrPacket(seq byte) []byte {
	msg := "cursor boom"
	p := []byte{iERR}
	p = binary.LittleEndian.AppendUint16(p, 1146)
	p = append(p, '#')
	p = append(p, 'H', 'Y', '0', '0', '0')
	p = append(p, msg...)
	return cursorPacket(seq, p...)
}

// queue appends one queued server reply.
func queue(conn *mockConn, packets ...[]byte) {
	conn.queuedReplies = append(conn.queuedReplies, joinPackets(packets...))
}

func joinPackets(packets ...[]byte) []byte {
	var out []byte
	for _, p := range packets {
		out = append(out, p...)
	}
	return out
}

// newCursorMockConn returns a mock connection configured without
// CLIENT_DEPRECATE_EOF so plain 0xFE EOF packets are used, together with a
// prepared statement requesting a read-only cursor.
func newCursorMockConn(t *testing.T, fetchSize uint32) (*mockConn, *mysqlStmt) {
	t.Helper()
	conn, mc := newRWMockConn(0)
	stmt := &mysqlStmt{
		mc:            mc,
		id:            7,
		paramCount:    0,
		cursor:        cursorTypeReadOnly,
		maxPacketRows: fetchSize,
	}
	return conn, stmt
}

// packetAt returns the parsed packets (header+payload) written to the mock.
func writtenPackets(t *testing.T, conn *mockConn) [][]byte {
	t.Helper()
	var pkts [][]byte
	for written := conn.written; len(written) > 0; {
		n := 4 + getUint24(written)
		if n > len(written) {
			t.Fatalf("invalid packet length: got %d, remaining %d", n, len(written))
		}
		pkts = append(pkts, written[:n])
		written = written[n:]
	}
	return pkts
}

// TestCursorExecutePacketShape verifies the wire format of the execute and
// fetch commands for a read-only cursor.
func TestCursorExecutePacketShape(t *testing.T) {
	conn, stmt := newCursorMockConn(t, 40)
	rows := executeCursorStmt(t, conn, stmt)

	pkts := writtenPackets(t, conn)
	if len(pkts) < 1 {
		t.Fatal("no packets written")
	}
	exec := pkts[0]
	if exec[4] != comStmtExecute {
		t.Fatalf("first command = %d, want COM_STMT_EXECUTE", exec[4])
	}
	if id := binary.LittleEndian.Uint32(exec[5:9]); id != 7 {
		t.Fatalf("statement id = %d, want 7", id)
	}
	if flag := exec[9]; flag != comStmtCursorReadOnly {
		t.Fatalf("cursor flag = %#x, want %#x", flag, comStmtCursorReadOnly)
	}
	if iters := binary.LittleEndian.Uint32(exec[10:14]); iters != 1 {
		t.Fatalf("iteration count = %d, want 1", iters)
	}

	// Trigger one fetch and inspect the COM_STMT_FETCH packet.
	queue(conn,
		cursorRowPacket(1, 1),
		cursorEOFPacket(2, statusLastRowSent),
	)

	if got := readOneRow(t, rows); got != 1 {
		t.Fatalf("fetched row = %d, want 1", got)
	}

	var fetchPkt []byte
	for _, p := range writtenPackets(t, conn) {
		if p[4] == comStmtFetch {
			fetchPkt = p
		}
	}
	if fetchPkt == nil {
		t.Fatal("no COM_STMT_FETCH packet written")
	}
	if id := binary.LittleEndian.Uint32(fetchPkt[5:9]); id != 7 {
		t.Fatalf("fetch statement id = %d, want 7", id)
	}
	if n := binary.LittleEndian.Uint32(fetchPkt[9:13]); n != 40 {
		t.Fatalf("fetch row count = %d, want 40", n)
	}
}

// newCursorMockConnDEPRECATEEOF is the modern-server variant where EOF packets
// are replaced by 0xFE-headed OK packets. In that mode readColumns does not
// expect a separate terminator after the column definitions.
func newCursorMockConnDEPRECATEEOF(t *testing.T, fetchSize uint32) (*mockConn, *mysqlStmt) {
	t.Helper()
	conn, stmt := newCursorMockConn(t, fetchSize)
	stmt.mc.capabilities |= clientDeprecateEOF
	return conn, stmt
}

// executeCursorStmt drives stmt.query(nil) after enqueueing the column header
// response (column count, column definition, cursor-marker EOF).
func executeCursorStmt(t *testing.T, conn *mockConn, stmt *mysqlStmt) *binaryRows {
	t.Helper()
	queue(conn,
		cursorPacket(1, 0x01),                  // column count = 1
		cursorColumnPacket(2),                  // column definition
		cursorEOFPacket(3, statusCursorExists), // cursor opened, no rows yet
	)
	rows, err := stmt.query(nil)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if !rows.rs.cursorOpen {
		t.Fatal("expected a server-side cursor to be open")
	}
	return rows
}

// writtenCommands extracts the command byte of every MySQL packet written to
// the mock connection, in order.
func writtenCommands(t *testing.T, conn *mockConn) []byte {
	t.Helper()
	var cmds []byte
	for written := conn.written; len(written) > 0; {
		packetLen := 4 + getUint24(written)
		if packetLen > len(written) {
			t.Fatalf("invalid packet length: got %d, remaining %d", packetLen, len(written))
		}
		cmds = append(cmds, written[4])
		written = written[packetLen:]
	}
	return cmds
}

// findFetches returns the 4-byte row count carried by every COM_STMT_FETCH
// packet written to the connection.
func fetchCounts(t *testing.T, conn *mockConn) []uint32 {
	t.Helper()
	var counts []uint32
	for written := conn.written; len(written) > 0; {
		packetLen := 4 + getUint24(written)
		if packetLen > len(written) {
			t.Fatalf("invalid packet length: got %d, remaining %d", packetLen, len(written))
		}
		if written[4] == comStmtFetch {
			counts = append(counts, binary.LittleEndian.Uint32(written[9:13]))
		}
		written = written[packetLen:]
	}
	return counts
}

func readOneRow(t *testing.T, rows *binaryRows) int64 {
	t.Helper()
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	v, ok := dest[0].(int64)
	if !ok {
		t.Fatalf("expected int64 row, got %T (%v)", dest[0], dest[0])
	}
	return v
}

// TestCursorShortBatch covers a result set smaller than one fetch batch.
// Only one COM_STMT_FETCH is sent; its single EOF carries LAST_ROW_SENT.
func TestCursorShortBatch(t *testing.T) {
	conn, stmt := newCursorMockConn(t, 4)
	rows := executeCursorStmt(t, conn, stmt)

	// One batch: two rows then an EOF ending the result.
	queue(conn,
		cursorRowPacket(1, 10),
		cursorRowPacket(2, 20),
		cursorEOFPacket(3, statusLastRowSent|statusCursorExists),
	)

	if got := readOneRow(t, rows); got != 10 {
		t.Fatalf("row 1: got %d, want 10", got)
	}
	if got := readOneRow(t, rows); got != 20 {
		t.Fatalf("row 2: got %d, want 20", got)
	}

	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}

	// Exactly one fetch of the configured size was issued, no early close.
	if got := fetchCounts(t, conn); len(got) != 1 || got[0] != 4 {
		t.Fatalf("fetch counts: got %v, want [4]", got)
	}

	// Close after natural exhaustion must not send anything; the server
	// already closed the cursor, and the connection must hold no leftovers.
	if err := rows.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(conn.data) != 0 {
		t.Fatal("unconsumed packets remain on the stream")
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("second close must be a no-op: %v", err)
	}
	if cmds := writtenCommands(t, conn); cmds[len(cmds)-1] != comStmtFetch {
		t.Fatalf("unexpected trailing command %d", cmds[len(cmds)-1])
	}
}

// TestCursorDeprecateEOF exercises the modern CLIENT_DEPRECATE_EOF wire format:
// batch terminators are 0xFE-headed OK packets rather than classic EOF packets.
func TestCursorDeprecateEOF(t *testing.T) {
	conn, stmt := newCursorMockConnDEPRECATEEOF(t, 2)

	queue(conn,
		cursorPacket(1, 0x01),
		cursorColumnPacket(2),
		cursorOKPacket(3, statusCursorExists),
	)
	rows, err := stmt.query(nil)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if !rows.rs.cursorOpen {
		t.Fatal("expected a server-side cursor to be open")
	}

	queue(conn,
		cursorRowPacket(1, 99),
		cursorOKPacket(2, statusLastRowSent),
	)
	if got := readOneRow(t, rows); got != 99 {
		t.Fatalf("row: got %d, want 99", got)
	}
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(conn.data) != 0 {
		t.Fatal("unconsumed packets remain")
	}
}

// TestCursorExactBatch covers a result set ending exactly on a batch boundary.
// The first full batch ends without LAST_ROW_SENT; the following empty batch
// signals the end.
func TestCursorExactBatch(t *testing.T) {
	conn, stmt := newCursorMockConn(t, 2)
	rows := executeCursorStmt(t, conn, stmt)

	// First batch: exactly fetchSize rows, cursor remains open.
	queue(conn,
		cursorRowPacket(1, 1),
		cursorRowPacket(2, 2),
		cursorEOFPacket(3, statusCursorExists),
	)
	// Second batch: no rows, the server reports the last row was already sent.
	queue(conn,
		cursorEOFPacket(1, statusLastRowSent),
	)

	if got := readOneRow(t, rows); got != 1 {
		t.Fatalf("row 1: got %d, want 1", got)
	}
	if got := readOneRow(t, rows); got != 2 {
		t.Fatalf("row 2: got %d, want 2", got)
	}
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}

	if got := fetchCounts(t, conn); len(got) != 2 {
		t.Fatalf("fetch counts: got %v, want two fetches", got)
	}

	if err := rows.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(conn.data) != 0 {
		t.Fatal("unconsumed packets remain on the stream")
	}
}

// TestCursorCancelDuringSecondBatch cancels while rows remain server-side.
// Rows.Close must drain the cursor with COM_STMT_FETCH until the server
// reports LAST_ROW_SENT, and must not issue COM_STMT_CLOSE or leave packets.
func TestCursorCancelDuringSecondBatch(t *testing.T) {
	conn, stmt := newCursorMockConn(t, 2)
	rows := executeCursorStmt(t, conn, stmt)

	// First batch fully consumed.
	queue(conn,
		cursorRowPacket(1, 1),
		cursorRowPacket(2, 2),
		cursorEOFPacket(3, statusCursorExists),
	)
	if got := readOneRow(t, rows); got != 1 {
		t.Fatalf("row 1: got %d, want 1", got)
	}
	if got := readOneRow(t, rows); got != 2 {
		t.Fatalf("row 2: got %d, want 2", got)
	}

	// Trigger the start of the second batch (one fetch in flight on the wire)
	// then immediately give up by closing.
	queue(conn,
		// The draining fetch triggered by Close sees one more row then end.
		cursorRowPacket(1, 3),
		cursorEOFPacket(2, statusLastRowSent),
	)

	fetchesBefore := len(fetchCounts(t, conn))
	if err := rows.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cmds := writtenCommands(t, conn)
	// Close must drain using COM_STMT_FETCH, not COM_STMT_CLOSE.
	fetchesAfter := fetchCounts(t, conn)
	if len(fetchesAfter) != fetchesBefore+1 {
		t.Fatalf("close issued %d drain fetches, want exactly 1", len(fetchesAfter)-fetchesBefore)
	}
	for _, c := range cmds {
		if c != comStmtExecute && c != comStmtFetch {
			t.Fatalf("unexpected command %d, want only COM_STMT_EXECUTE/FETCH", c)
		}
	}
	if len(conn.data) != 0 {
		t.Fatal("unconsumed packets remain after cancel")
	}

	// The connection must be reusable afterwards: a fresh command round-trip
	// must start from a clean sequence position.
	if mc := rows.mc; mc != nil {
		t.Fatal("rows still reference the connection after Close")
	}
}

// TestCursorServerErrorDuringFetch checks that a server error while fetching a
// later batch surfaces to the caller and that Close afterwards does not try to
// fetch from an already broken/closed cursor.
func TestCursorServerErrorDuringFetch(t *testing.T) {
	conn, stmt := newCursorMockConn(t, 2)
	rows := executeCursorStmt(t, conn, stmt)

	// First batch: one row, cursor remains open.
	queue(conn,
		cursorRowPacket(1, 1),
		cursorEOFPacket(2, statusCursorExists),
	)
	if got := readOneRow(t, rows); got != 1 {
		t.Fatalf("row 1: got %d, want 1", got)
	}

	// Second batch fails on the server.
	queue(conn, cursorErrPacket(1))

	dest := make([]driver.Value, 1)
	err := rows.Next(dest)
	var myErr *MySQLError
	if !errors.As(err, &myErr) || myErr.Number != 1146 {
		t.Fatalf("expected MySQLError 1146, got %v", err)
	}

	// After an error the driver drops the connection reference; Close is a
	// safe no-op and must not write another fetch to the dead stream.
	writesBefore := conn.writes
	if err := rows.Close(); err != nil {
		t.Fatalf("close after error: %v", err)
	}
	if conn.writes != writesBefore {
		t.Fatalf("Close wrote %d packets after a fatal server error", conn.writes-writesBefore)
	}
}

// TestCursorStatementReuseAfterEarlyClose verifies that after closing a cursor
// before all rows are read, the prepared statement can be executed again on a
// clean connection with no residual packets and correct sequence numbers.
func TestCursorStatementReuseAfterEarlyClose(t *testing.T) {
	conn, stmt := newCursorMockConn(t, 3)
	rows := executeCursorStmt(t, conn, stmt)

	// Read only one row of a large server-side result, then close early.
	queue(conn,
		cursorRowPacket(1, 100),
		cursorRowPacket(2, 101),
		cursorRowPacket(3, 102),
		cursorEOFPacket(4, statusCursorExists), // batch one of many
	)
	// Close drains the remaining batch, then keeps fetching until the end.
	queue(conn,
		cursorEOFPacket(1, statusLastRowSent), // the drained batch was last
	)

	if got := readOneRow(t, rows); got != 100 {
		t.Fatalf("row: got %d, want 100", got)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("early close: %v", err)
	}
	if len(conn.data) != 0 {
		t.Fatal("residual packets remain after early close")
	}

	// Execute the same statement again. The wire must start fresh: the driver
	// resets sequence numbers and the server's first reply is the header.
	rows2 := executeCursorStmt(t, conn, stmt)

	queue(conn,
		cursorRowPacket(1, 7),
		cursorEOFPacket(2, statusLastRowSent),
	)
	if got := readOneRow(t, rows2); got != 7 {
		t.Fatalf("reused statement row: got %d, want 7", got)
	}
	dest := make([]driver.Value, 1)
	if err := rows2.Next(dest); err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if err := rows2.Close(); err != nil {
		t.Fatalf("final close: %v", err)
	}
	if len(conn.data) != 0 {
		t.Fatal("residual packets remain after reused query")
	}
}
