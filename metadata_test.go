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
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
)

// frame is a single MySQL packet (without the 4-byte header).
type frame struct {
	seq  byte
	data []byte
}

// scriptedServer is a minimal MySQL 8 server that runs a prepared-statement
// script over an in-memory connection. It records every packet it sends and
// every command packet it receives, allowing tests to assert on the actual
// protocol data exchanged for repeated executions.
type scriptedServer struct {
	client *pipeConn
	server *pipeConn

	mu      sync.Mutex
	sent    []frame
	receive [][]byte

	// Frame-index boundaries marking where each command response starts.
	prepareEnd   int
	firstExecEnd int

	// capabilityMask, when set, is ANDed with the default advertised caps so
	// tests can clear individual bits (e.g. clientDeprecateEOF).
	capabilityMask capabilityFlag

	t *testing.T
}

func newScriptedServer(t *testing.T) *scriptedServer {
	t.Helper()
	client, server := netPipe()
	return &scriptedServer{t: t, client: client, server: server}
}

func (s *scriptedServer) write(seq byte, payload []byte) {
	s.mu.Lock()
	s.sent = append(s.sent, frame{seq: seq, data: append([]byte(nil), payload...)})
	s.mu.Unlock()

	pkt := make([]byte, 4+len(payload))
	pkt[0] = byte(len(payload))
	pkt[1] = byte(len(payload) >> 8)
	pkt[2] = byte(len(payload) >> 16)
	pkt[3] = seq
	copy(pkt[4:], payload)
	if _, err := s.server.Write(pkt); err != nil {
		s.t.Fatalf("server write: %v", err)
	}
}

func (s *scriptedServer) read() []byte {
	data, err := readFrame(s.server)
	if err != nil {
		s.t.Fatalf("server read: %v", err)
	}
	s.mu.Lock()
	s.receive = append(s.receive, append([]byte(nil), data...))
	s.mu.Unlock()
	return data
}

func readFrame(conn net.Conn) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	n := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	payload := make([]byte, n)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func lenEncInt(n uint64) []byte {
	switch {
	case n < 251:
		return []byte{byte(n)}
	case n < 1<<16:
		return []byte{0xfc, byte(n), byte(n >> 8)}
	case n < 1<<24:
		return []byte{0xfd, byte(n), byte(n >> 8), byte(n >> 16)}
	default:
		b := make([]byte, 9)
		b[0] = 0xfe
		binary.LittleEndian.PutUint64(b[1:], n)
		return b
	}
}

func lenEncString(v string) []byte {
	return append(lenEncInt(uint64(len(v))), v...)
}

// handshakeV10 builds a MySQL (clientMySQL set) handshake advertising the
// requested capability flags.
func handshakeV10(caps capabilityFlag) []byte {
	var b bytes.Buffer
	b.WriteByte(0x0a)                                                 // protocol version
	b.WriteString("8.0.29\x00")                                       // server version
	binary.Write(&b, binary.LittleEndian, uint32(1))                  // connection id
	b.WriteString("12345678")                                         // auth-plugin-data-part-1
	b.WriteByte(0x00)                                                 // filler
	binary.Write(&b, binary.LittleEndian, uint16(caps))               // capability flags lower
	b.WriteByte(defaultCollationID)                                   // character set
	binary.Write(&b, binary.LittleEndian, uint16(statusInAutocommit)) // status flags
	binary.Write(&b, binary.LittleEndian, uint16(caps>>16))           // capability flags upper
	b.WriteByte(21)                                                   // auth plugin data length
	b.Write(make([]byte, 10))                                         // reserved (10 bytes for MySQL)
	b.WriteString("123456789012\x00")                                 // auth-plugin-data-part-2
	b.WriteString("mysql_native_password\x00")                        // auth plugin name
	return b.Bytes()
}

func okPacket(status statusFlag) []byte {
	return append([]byte{iOK, 0x00, 0x00}, byte(status), byte(status>>8), 0x00, 0x00)
}

// columnDefinition builds one binary-protocol column definition (41-style).
func columnDefinition(name string, typ fieldType, unsigned bool) []byte {
	var b bytes.Buffer
	b.Write(lenEncString("def"))                                     // catalog
	b.Write(lenEncString("db"))                                      // schema
	b.Write(lenEncString("t"))                                       // table
	b.Write(lenEncString("t"))                                       // org_table
	b.Write(lenEncString(name))                                      // name
	b.Write(lenEncString(name))                                      // org_name
	b.WriteByte(0x0c)                                                // length of fixed fields
	binary.Write(&b, binary.LittleEndian, uint16(binaryCollationID)) // charset
	var length uint32 = 11
	binary.Write(&b, binary.LittleEndian, length) // column length
	b.WriteByte(byte(typ))                        // column type
	var flags uint16
	if unsigned {
		flags |= uint16(flagUnsigned)
	}
	binary.Write(&b, binary.LittleEndian, flags) // flags
	b.WriteByte(0x00)                            // decimals
	b.Write(make([]byte, 2))                     // filler
	return b.Bytes()
}

// resultsetWithMetadata returns the packets for a single-row binary result
// set whose metadata is fully present. deprecateEOF controls both the EOF
// after the columns and the terminating packet (OK with 0xfe header).
func resultsetWithMetadata(deprecateEOF bool, columns ...[]byte) [][]byte {
	var pkts [][]byte
	pkts = append(pkts, append(lenEncInt(uint64(len(columns))), 0x01)) // count + metadata follows
	pkts = append(pkts, columns...)
	if !deprecateEOF {
		pkts = append(pkts, eofPacket(statusInAutocommit))
	}
	pkts = append(pkts, binaryRow())
	if deprecateEOF {
		pkts = append(pkts, okEOFPacket(statusInAutocommit))
	} else {
		pkts = append(pkts, eofPacket(statusInAutocommit))
	}
	return pkts
}

// resultsetMetadataOmitted returns the packets sent when the server reuses
// the client's cached definitions: the count packet carries a 0x00 metadata
// byte, followed by the EOF/OK metadata packet and the rows.
func resultsetMetadataOmitted(deprecateEOF bool, count int) [][]byte {
	var pkts [][]byte
	pkts = append(pkts, append(lenEncInt(uint64(count)), 0x00))
	if deprecateEOF {
		pkts = append(pkts, okEOFPacket(statusInAutocommit))
	} else {
		pkts = append(pkts, eofPacket(statusInAutocommit))
	}
	pkts = append(pkts, binaryRow())
	if deprecateEOF {
		pkts = append(pkts, okEOFPacket(statusInAutocommit))
	} else {
		pkts = append(pkts, eofPacket(statusInAutocommit))
	}
	return pkts
}

func eofPacket(status statusFlag) []byte {
	return []byte{iEOF, 0x00, 0x00, byte(status), byte(status >> 8)}
}

func okEOFPacket(status statusFlag) []byte {
	// OK packet with 0xfe header; affected rows 0, last insert id 0.
	return []byte{iEOF, 0x00, 0x00, byte(status), byte(status >> 8), 0x00, 0x00}
}

// binaryRow builds one binary result-set row containing an INT value 42 and a
// VARCHAR value "hello", matching the columns used by the tests.
func binaryRow() []byte {
	var b bytes.Buffer
	b.WriteByte(0x00) // packet header
	// NULL bitmap: (2+7+2)/8 = 1 byte, no NULL columns.
	b.WriteByte(0x00)
	binary.Write(&b, binary.LittleEndian, int32(42))
	b.Write(lenEncString("hello"))
	return b.Bytes()
}

func (s *scriptedServer) serveLogin() {
	s.write(0, handshakeV10(s.capabilities()))
	// Handshake response.
	s.read()
	// mysql_native_password with an empty password completes immediately.
	s.write(2, okPacket(statusInAutocommit))
}

func (s *scriptedServer) capabilities() capabilityFlag {
	caps := clientProtocol41 | clientSecureConn | clientPluginAuth |
		clientPluginAuthLenEncClientData | clientConnectAttrs |
		clientDeprecateEOF | clientLongFlag | clientTransactions |
		clientMySQL | clientMultiResults |
		clientOptionalResultsetMetadata | clientParameterCountAvailable
	if s.capabilityMask != 0 {
		caps &= s.capabilityMask
	}
	return caps
}

// prepareOk answers COM_STMT_PREPARE for a 0-parameter, 2-column SELECT,
// sending the two column definitions that MySQL always returns at prepare
// time.
func (s *scriptedServer) prepareOk(stmtID uint32, deprecateEOF bool, columns ...[]byte) {
	s.read() // COM_STMT_PREPARE
	pkt := make([]byte, 12)
	pkt[0] = iOK
	binary.LittleEndian.PutUint32(pkt[1:], stmtID)
	binary.LittleEndian.PutUint16(pkt[5:], uint16(len(columns))) // column count
	binary.LittleEndian.PutUint16(pkt[7:], 0)                    // param count
	s.write(1, pkt)
	seq := byte(2)
	for _, col := range columns {
		s.write(seq, col)
		seq++
	}
	if !deprecateEOF {
		s.write(seq, eofPacket(statusInAutocommit))
	}
}

// prepareOkNoColumns answers COM_STMT_PREPARE with zero result columns.
func (s *scriptedServer) prepareOkNoColumns(stmtID uint32) {
	s.read() // COM_STMT_PREPARE
	pkt := make([]byte, 12)
	pkt[0] = iOK
	binary.LittleEndian.PutUint32(pkt[1:], stmtID)
	binary.LittleEndian.PutUint16(pkt[5:], 0) // column count
	binary.LittleEndian.PutUint16(pkt[7:], 0) // param count
	s.write(1, pkt)
}

func (s *scriptedServer) close() {
	s.server.Close()
	s.client.Close()
}

func dialScripted(s *scriptedServer) func(context.Context, string, string) (net.Conn, error) {
	return func(context.Context, string, string) (net.Conn, error) {
		return s.client, nil
	}
}

// executeRequest describes a parsed COM_STMT_EXECUTE packet.
type executeRequest struct {
	stmtID                uint32
	flags                 byte
	parameterCountPresent bool
	parameterCount        byte
}

func parseExecute(p []byte) executeRequest {
	return parseExecuteWithCaps(p, true)
}

func parseExecuteWithCaps(p []byte, parameterCountAvailable bool) executeRequest {
	// payload: COM_STMT_EXECUTE(1) stmt_id(4) flags(1) iter(4)
	r := executeRequest{}
	r.stmtID = binary.LittleEndian.Uint32(p[1:5])
	r.flags = p[5]
	if parameterCountAvailable {
		r.parameterCountPresent = true
		r.parameterCount = p[9]
	}
	return r
}

func testColumns() [][]byte {
	return [][]byte{
		columnDefinition("id", fieldTypeLong, false),
		columnDefinition("name", fieldTypeVarString, false),
	}
}

// connectScripted performs a full client login against the scripted server
// and returns the live driver connection.
func connectScripted(t *testing.T, s *scriptedServer) *mysqlConn {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.serveLogin()
	}()

	cfg := NewConfig()
	cfg.User = "root"
	cfg.AllowNativePasswords = true
	cfg.DialFunc = dialScripted(s)

	connector, err := NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbConn, err := connector.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-done

	mc, ok := dbConn.(*mysqlConn)
	if !ok {
		t.Fatalf("expected *mysqlConn, got %T", dbConn)
	}
	return mc
}

// countColumnDefinitions counts the ColumnDefinition41 packets in the frames
// the server sent: these are the payloads carrying a "def" catalog between
// the column-count packet and the row stream. The tests instead rely on the
// explicit metadata byte and frame layout, so this is used only for a simple
// sanity assertion.
func metadataFollowsByte(frames []frame) (followed int, omitted int) {
	for _, f := range frames {
		// Column-count packets are length-encoded integers with a trailing
		// metadata byte; for 1-2 columns they are exactly two bytes.
		if len(f.data) == 2 && f.data[0] <= 2 &&
			(f.data[1] == 0x00 || f.data[1] == 0x01) {
			if f.data[1] == 0x01 {
				followed++
			} else {
				omitted++
			}
		}
	}
	return followed, omitted
}

func assertColumnsAndRow(t *testing.T, rows *binaryRows) {
	t.Helper()

	names := rows.Columns()
	if got, want := len(names), 2; got != want {
		t.Fatalf("Columns() len = %d, want %d", got, want)
	}
	if names[0] != "id" || names[1] != "name" {
		t.Fatalf("Columns() = %v, want [id name]", names)
	}

	vdest := make([]driver.Value, 2)
	if err := rows.Next(vdest); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got, want := vdest[0], int64(42); got != want {
		t.Fatalf("column id = %v (%T), want int64(42)", got, got)
	}
	if got, want := vdest[1], []byte("hello"); !bytes.Equal(got.([]byte), want) {
		t.Fatalf("column name = %q, want %q", got, want)
	}

	if err := rows.Next(vdest); err != io.EOF {
		t.Fatalf("second Next = %v, want io.EOF", err)
	}
}

// TestOptionalResultsetMetadataReused verifies that a second execution of the
// same prepared statement receives no column definitions, while Rows still
// exposes the correct columns and decodes the row from the cached metadata.
func TestOptionalResultsetMetadataReused(t *testing.T) {
	s := newScriptedServer(t)
	defer s.close()
	mc := connectScripted(t, s)

	if mc.capabilities&clientOptionalResultsetMetadata == 0 {
		t.Fatal("CLIENT_OPTIONAL_RESULTSET_METADATA not negotiated")
	}
	if mc.capabilities&clientParameterCountAvailable == 0 {
		t.Fatal("CLIENT_PARAMETER_COUNT_AVAILABLE not negotiated")
	}

	const stmtID uint32 = 7
	columns := testColumns()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		s.prepareOk(stmtID, true, columns...)
		s.mu.Lock()
		s.prepareEnd = len(s.sent)
		s.mu.Unlock()

		// First execution: full metadata.
		first := s.read()
		if first[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", first[0])
		}
		for i, pkt := range resultsetWithMetadata(true, columns...) {
			s.write(byte(i+1), pkt)
		}
		s.mu.Lock()
		s.firstExecEnd = len(s.sent)
		s.mu.Unlock()

		// Second execution: metadata omitted.
		second := s.read()
		if second[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", second[0])
		}
		for i, pkt := range resultsetMetadataOmitted(true, 2) {
			s.write(byte(i+1), pkt)
		}
	}()

	stmt, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps := stmt.(*mysqlStmt)

	rows1, err := ps.query(nil)
	if err != nil {
		t.Fatalf("first query: %v", err)
	}
	assertColumnsAndRow(t, rows1)

	rows2, err := ps.query(nil)
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	assertColumnsAndRow(t, rows2)

	<-serverDone

	// Snapshot the exact frames sent for each execution using the boundaries
	// recorded by the server goroutine.
	s.mu.Lock()
	firstFrames := append([]frame(nil), s.sent[s.prepareEnd:s.firstExecEnd]...)
	secondFrames := append([]frame(nil), s.sent[s.firstExecEnd:]...)
	s.mu.Unlock()

	// Both COM_STMT_EXECUTE requests must ask the server to reuse metadata and
	// carry the parameter count.
	var executes [][]byte
	s.mu.Lock()
	for _, p := range s.receive {
		if len(p) > 0 && p[0] == comStmtExecute {
			executes = append(executes, p)
		}
	}
	s.mu.Unlock()
	if len(executes) != 2 {
		t.Fatalf("got %d COM_STMT_EXECUTE packets, want 2", len(executes))
	}
	for i, p := range executes {
		req := parseExecute(p)
		if req.flags&stmtExecuteFlagParameterCount == 0 {
			t.Errorf("execute #%d flags = %#x, metadata-reuse bit not set", i+1, req.flags)
		}
		if !req.parameterCountPresent || req.parameterCount != 0 {
			t.Errorf("execute #%d parameter count = present:%v value:%d, want 0",
				i+1, req.parameterCountPresent, req.parameterCount)
		}
	}

	// Assert on the actual protocol data: the first execution sent column
	// definitions, the second sent the metadata-omitted marker instead.
	followedFirst, omittedFirst := metadataFollowsByte(firstFrames)
	if followedFirst != 1 || omittedFirst != 0 {
		t.Fatalf("first execution metadata markers: %d followed, %d omitted; want 1/0",
			followedFirst, omittedFirst)
	}
	followedSecond, omittedSecond := metadataFollowsByte(secondFrames)
	if followedSecond != 0 || omittedSecond != 1 {
		t.Fatalf("second execution metadata markers: %d followed, %d omitted; want 0/1",
			followedSecond, omittedSecond)
	}

	if n := countColumnPackets(firstFrames); n != 2 {
		t.Fatalf("first execution column definitions = %d, want 2", n)
	}
	if n := countColumnPackets(secondFrames); n != 0 {
		t.Fatalf("second execution column definitions = %d, want 0", n)
	}
}

// TestOptionalResultsetMetadataReusedClassicEOF repeats the reuse scenario
// with CLIENT_DEPRECATE_EOF disabled, exercising the legacy EOF metadata
// packet path.
func TestOptionalResultsetMetadataReusedClassicEOF(t *testing.T) {
	s := newScriptedServer(t)
	defer s.close()
	s.capabilityMask = ^clientDeprecateEOF
	mc := connectScripted(t, s)

	if mc.capabilities&clientDeprecateEOF != 0 {
		t.Fatal("CLIENT_DEPRECATE_EOF must not be negotiated")
	}

	const stmtID uint32 = 7
	columns := testColumns()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		s.prepareOk(stmtID, false, columns...)

		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		for i, pkt := range resultsetWithMetadata(false, columns...) {
			s.write(byte(i+1), pkt)
		}

		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		for i, pkt := range resultsetMetadataOmitted(false, 2) {
			s.write(byte(i+1), pkt)
		}
	}()

	stmt, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps := stmt.(*mysqlStmt)

	rows1, err := ps.query(nil)
	if err != nil {
		t.Fatalf("first query: %v", err)
	}
	assertColumnsAndRow(t, rows1)

	rows2, err := ps.query(nil)
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	assertColumnsAndRow(t, rows2)

	<-serverDone
}

// countColumnPackets counts ColumnDefinition41 frames ("def" catalog) in the
// provided frames.
func countColumnPackets(frames []frame) int {
	n := 0
	for _, f := range frames {
		if bytes.HasPrefix(f.data, []byte{0x03, 'd', 'e', 'f'}) {
			n++
		}
	}
	return n
}

// TestOmittedMetadataWithoutCacheErrors verifies that a clear error is
// returned when the server omits column definitions on the first execution
// while no definitions are cached locally.
func TestOmittedMetadataWithoutCacheErrors(t *testing.T) {
	s := newScriptedServer(t)
	defer s.close()
	mc := connectScripted(t, s)

	const stmtID uint32 = 9
	columns := testColumns()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		// Prepare returns columns, but simulate a connection whose cache was
		// discarded by closing them via a server that re-prepares: here the
		// driver still caches them, so force an empty cache below instead.
		s.prepareOk(stmtID, true, columns...)
		req := s.read()
		if req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		for i, pkt := range resultsetMetadataOmitted(true, 2) {
			s.write(byte(i+1), pkt)
		}
	}()

	stmt, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps := stmt.(*mysqlStmt)
	// Simulate no usable local definitions (e.g. prepare carried no columns
	// or the cache was never populated).
	ps.columns = nil

	if _, err := ps.query(nil); !errors.Is(err, ErrNoCachedMetadata) {
		t.Fatalf("query with omitted metadata and empty cache: err = %v, want ErrNoCachedMetadata", err)
	}
	<-serverDone
}

// TestMetadataCacheBoundToStatement verifies that definitions cached for one
// prepared statement are not reused for a different statement id (the case
// when the server closes the old statement and recycles its id after a
// re-prepare).
func TestMetadataCacheBoundToStatement(t *testing.T) {
	s := newScriptedServer(t)
	defer s.close()
	mc := connectScripted(t, s)

	columns := testColumns()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

		// Statement id 1: prepare, one execution with full metadata.
		s.prepareOk(1, true, columns...)
		req := s.read()
		if binary.LittleEndian.Uint32(req[1:5]) != 1 {
			s.t.Fatalf("expected stmt id 1, got %d", binary.LittleEndian.Uint32(req[1:5]))
		}
		for i, pkt := range resultsetWithMetadata(true, columns...) {
			s.write(byte(i+1), pkt)
		}

		// A fresh statement reusing id 1 (re-prepared after the server closed
		// the previous one). The prepare response reports zero columns (the
		// server does not know the result shape yet), so the new statement's
		// cache is empty. The execution then omits metadata: the driver must
		// fail rather than reuse the old id-1 definitions.
		s.prepareOkNoColumns(1)
		req = s.read()
		if binary.LittleEndian.Uint32(req[1:5]) != 1 {
			s.t.Fatalf("expected recycled stmt id 1, got %d", binary.LittleEndian.Uint32(req[1:5]))
		}
		for i, pkt := range resultsetMetadataOmitted(true, 2) {
			s.write(byte(i+1), pkt)
		}
	}()

	stmt1, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps1 := stmt1.(*mysqlStmt)
	rows, err := ps1.query(nil)
	if err != nil {
		t.Fatalf("first statement query: %v", err)
	}
	assertColumnsAndRow(t, rows)

	// Re-prepare: a new mysqlStmt is created, so the cache of ps1 cannot leak.
	stmt2, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps2 := stmt2.(*mysqlStmt)
	if ps2 == ps1 {
		t.Fatal("re-prepare returned the same statement instance")
	}
	if len(ps2.columns) != 0 {
		t.Fatalf("new statement must not inherit the old cache, got %d columns", len(ps2.columns))
	}

	// The recycled-id statement's server omits metadata with no local cache.
	if _, err := ps2.query(nil); !errors.Is(err, ErrNoCachedMetadata) {
		t.Fatalf("recycled statement omitted metadata: err = %v, want ErrNoCachedMetadata", err)
	}
	<-serverDone
}

// TestColumnCountChangeRefusesStaleCache verifies that a result shape change
// (column count differs from the cached definitions) is never decoded with
// the stale cache; the server must send definitions, and a missing-metadata
// reply with a changed count yields an explicit error.
func TestColumnCountChangeRefusesStaleCache(t *testing.T) {
	s := newScriptedServer(t)
	defer s.close()
	mc := connectScripted(t, s)

	const stmtID uint32 = 11
	columns := testColumns()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		s.prepareOk(stmtID, true, columns...)

		// First execution: two columns, full metadata.
		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		for i, pkt := range resultsetWithMetadata(true, columns...) {
			s.write(byte(i+1), pkt)
		}

		// Second execution: shape changed to three columns but metadata is
		// omitted. The driver must refuse the stale two-column cache.
		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		for i, pkt := range resultsetMetadataOmitted(true, 3) {
			s.write(byte(i+1), pkt)
		}
	}()

	stmt, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps := stmt.(*mysqlStmt)
	rows, err := ps.query(nil)
	if err != nil {
		t.Fatalf("first query: %v", err)
	}
	assertColumnsAndRow(t, rows)

	if _, err := ps.query(nil); !errors.Is(err, ErrNoCachedMetadata) {
		t.Fatalf("changed column count with omitted metadata: err = %v, want ErrNoCachedMetadata", err)
	}
	<-serverDone
}

// TestOptionalResultsetMetadataNotAdvertised verifies that when the server
// does not advertise the capability, the driver does not set the reuse flag,
// always parses full metadata and never expects the metadata byte.
func TestOptionalResultsetMetadataNotAdvertised(t *testing.T) {
	s := newScriptedServer(t)
	defer s.close()

	// Login without the optional-metadata capability.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.write(0, handshakeV10(s.capabilities()&^clientOptionalResultsetMetadata&^clientParameterCountAvailable))
		s.read()
		s.write(2, okPacket(statusInAutocommit))
	}()
	cfg := NewConfig()
	cfg.User = "root"
	cfg.DialFunc = dialScripted(s)
	connector, err := NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbConn, err := connector.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-done
	mc := dbConn.(*mysqlConn)

	if mc.capabilities&clientOptionalResultsetMetadata != 0 {
		t.Fatal("optional-metadata capability must not be negotiated")
	}

	const stmtID uint32 = 5
	columns := testColumns()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		s.prepareOk(stmtID, true, columns...)

		// Old servers send a plain one-byte column-count packet (no trailing
		// metadata byte) and always include definitions.
		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		pkts := resultsetWithMetadata(true, columns...)
		// Strip the metadata-follows byte to emulate the legacy packet.
		pkts[0] = lenEncInt(2)
		for i, pkt := range pkts {
			s.write(byte(i+1), pkt)
		}
	}()

	stmt, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps := stmt.(*mysqlStmt)
	rows, err := ps.query(nil)
	if err != nil {
		t.Fatalf("query against legacy server: %v", err)
	}
	assertColumnsAndRow(t, rows)

	// The execute request must not carry the reuse flag or parameter count.
	s.mu.Lock()
	var exec []byte
	for _, p := range s.receive {
		if len(p) > 0 && p[0] == comStmtExecute {
			exec = p
		}
	}
	s.mu.Unlock()
	req := parseExecuteWithCaps(exec, false)
	if req.flags != 0 {
		t.Fatalf("execute flags = %#x, want 0", req.flags)
	}
	if req.parameterCountPresent {
		t.Fatal("parameter count must not be sent to a legacy server")
	}
	<-serverDone
}

// binaryRowTwoInts builds a binary row with two 8-byte big-endian values used
// in the type-change test.
func binaryRowTwoInts() []byte {
	var b bytes.Buffer
	b.WriteByte(0x00)
	b.WriteByte(0x00) // NULL bitmap for two columns
	binary.Write(&b, binary.LittleEndian, int64(42))
	binary.Write(&b, binary.LittleEndian, int64(7))
	return b.Bytes()
}

// TestColumnTypeChangeRefreshesCache verifies that when the result columns
// keep the same count but change their type (and the server therefore sends
// full definitions again), the cached definitions are replaced and the new
// layout is used to decode rows; a subsequent omitted-metadata execution uses
// the refreshed definitions.
func TestColumnTypeChangeRefreshesCache(t *testing.T) {
	s := newScriptedServer(t)
	defer s.close()
	mc := connectScripted(t, s)

	const stmtID uint32 = 13

	intCols := [][]byte{
		columnDefinition("id", fieldTypeLong, false),
		columnDefinition("name", fieldTypeVarString, false),
	}
	bigintCols := [][]byte{
		columnDefinition("a", fieldTypeLongLong, false),
		columnDefinition("b", fieldTypeLongLong, false),
	}

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		s.prepareOk(stmtID, true, intCols...)

		// Execution 1: INT + VARCHAR, full metadata.
		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		for i, pkt := range resultsetWithMetadata(true, intCols...) {
			s.write(byte(i+1), pkt)
		}

		// Execution 2: same count, different types/names, server resends
		// definitions (metadata-follows = 1) and signals METADATA_CHANGED.
		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		pkts := resultsetWithMetadata(true, bigintCols...)
		// Replace the data row with two bigints and set METADATA_CHANGED in
		// both the trailing OK packet.
		pkts[3] = binaryRowTwoInts()
		pkts[4] = okEOFPacket(statusInAutocommit | statusMetadataChanged)
		for i, pkt := range pkts {
			s.write(byte(i+1), pkt)
		}

		// Execution 3: metadata omitted again; the refreshed cache must be
		// used.
		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		omit := resultsetMetadataOmitted(true, 2)
		omit[2] = binaryRowTwoInts()
		for i, pkt := range omit {
			s.write(byte(i+1), pkt)
		}
	}()

	stmt, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps := stmt.(*mysqlStmt)

	rows1, err := ps.query(nil)
	if err != nil {
		t.Fatalf("query 1: %v", err)
	}
	dest := make([]driver.Value, 2)
	if err := rows1.Next(dest); err != nil && err != io.EOF {
		t.Fatalf("rows1.Next: %v", err)
	}
	if _, ok := dest[0].(int64); !ok {
		t.Fatalf("first layout id type = %T, want int64", dest[0])
	}
	if !bytes.Equal(dest[1].([]byte), []byte("hello")) {
		t.Fatalf("first layout name = %q, want hello", dest[1])
	}
	if err := rows1.Close(); err != nil {
		t.Fatalf("rows1.Close: %v", err)
	}

	rows2, err := ps.query(nil)
	if err != nil {
		t.Fatalf("query 2: %v", err)
	}
	names := rows2.Columns()
	if names[0] != "a" || names[1] != "b" {
		t.Fatalf("refreshed columns = %v, want [a b]", names)
	}
	dest2 := make([]driver.Value, 2)
	if err := rows2.Next(dest2); err != nil && err != io.EOF {
		t.Fatalf("rows2.Next: %v", err)
	}
	if got := dest2[0]; got != int64(42) {
		t.Fatalf("refreshed layout a = %v (%T), want int64(42)", got, got)
	}
	if got := dest2[1]; got != int64(7) {
		t.Fatalf("refreshed layout b = %v (%T), want int64(7)", got, got)
	}
	if err := rows2.Close(); err != nil {
		t.Fatalf("rows2.Close: %v", err)
	}

	rows3, err := ps.query(nil)
	if err != nil {
		t.Fatalf("query 3 with omitted metadata: %v", err)
	}
	if got := rows3.Columns(); got[0] != "a" || got[1] != "b" {
		t.Fatalf("omitted-metadata columns = %v, want [a b]", got)
	}
	dest3 := make([]driver.Value, 2)
	if err := rows3.Next(dest3); err != nil && err != io.EOF {
		t.Fatalf("rows3.Next: %v", err)
	}
	if dest3[0] != int64(42) || dest3[1] != int64(7) {
		t.Fatalf("omitted-metadata row = %v, %v; want 42, 7", dest3[0], dest3[1])
	}
	if err := rows3.Close(); err != nil {
		t.Fatalf("rows3.Close: %v", err)
	}

	<-serverDone
}

// TestExecReusesCachedMetadata verifies that Exec (which discards the rows)
// also caches and reuses result-set metadata across repeated executions.
func TestExecReusesCachedMetadata(t *testing.T) {
	s := newScriptedServer(t)
	defer s.close()
	mc := connectScripted(t, s)

	const stmtID uint32 = 21
	columns := testColumns()

	s.mu.Lock()
	s.prepareEnd = 0
	s.mu.Unlock()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		s.prepareOk(stmtID, true, columns...)
		s.mu.Lock()
		s.prepareEnd = len(s.sent)
		s.mu.Unlock()

		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		for i, pkt := range resultsetWithMetadata(true, columns...) {
			s.write(byte(i+1), pkt)
		}
		s.mu.Lock()
		s.firstExecEnd = len(s.sent)
		s.mu.Unlock()

		if req := s.read(); req[0] != comStmtExecute {
			s.t.Fatalf("expected COM_STMT_EXECUTE, got %d", req[0])
		}
		for i, pkt := range resultsetMetadataOmitted(true, 2) {
			s.write(byte(i+1), pkt)
		}
	}()

	stmt, err := mc.Prepare("SELECT id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	ps := stmt.(*mysqlStmt)

	if _, err := ps.Exec(nil); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	if len(ps.columns) != 2 {
		t.Fatalf("after first exec cached columns = %d, want 2", len(ps.columns))
	}

	if _, err := ps.Exec(nil); err != nil {
		t.Fatalf("second exec with omitted metadata: %v", err)
	}

	<-serverDone

	s.mu.Lock()
	first := append([]frame(nil), s.sent[s.prepareEnd:s.firstExecEnd]...)
	second := append([]frame(nil), s.sent[s.firstExecEnd:]...)
	s.mu.Unlock()

	if n := countColumnPackets(first); n != 2 {
		t.Fatalf("first exec column definitions = %d, want 2", n)
	}
	if n := countColumnPackets(second); n != 0 {
		t.Fatalf("second exec column definitions = %d, want 0", n)
	}
}
