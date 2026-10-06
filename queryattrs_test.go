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
	"strings"
	"testing"
)

// okReply returns a wire packet containing an OK packet for the given
// packet sequence number.
func okReply(seq byte) []byte {
	return []byte{
		0x07, 0x00, 0x00, seq, // packet header: 7-byte payload
		iOK,        // OK packet header
		0x00,       // affected rows
		0x00,       // last insert id
		0x02, 0x00, // SERVER_STATUS_AUTOCOMMIT
		0x00, 0x00, // warnings
	}
}

// splitPackets splits concatenated wire packets into their payloads.
func splitPackets(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var payloads [][]byte
	for len(data) > 0 {
		pktLen := 4 + int(getUint24(data))
		if pktLen > len(data) {
			t.Fatalf("invalid packet length: got %d, remaining bytes %d", pktLen, len(data))
		}
		payloads = append(payloads, data[4:pktLen])
		data = data[pktLen:]
	}
	return payloads
}

func readLenEncStr(t *testing.T, data []byte, pos *int) string {
	t.Helper()
	n, isNull, size := readLengthEncodedInteger(data[*pos:])
	if isNull {
		t.Fatalf("unexpected NULL length-encoded string at %d", *pos)
	}
	*pos += size
	s := string(data[*pos : *pos+int(n)])
	*pos += int(n)
	return s
}

func TestQueryAttributesTextProtocol(t *testing.T) {
	conn, mc := newRWMockConn(0)
	mc.capabilities = clientQueryAttributes
	conn.queuedReplies = [][]byte{okReply(1), okReply(1)}

	ctx := WithQueryAttributes(context.Background(),
		QueryAttribute{Name: "trace_id", Value: "abc-123"},
		QueryAttribute{Name: "operation", Value: ""},     // empty string
		QueryAttribute{Name: "retries", Value: int64(7)}, // integer
		QueryAttribute{Name: "note", Value: nil},         // NULL
		QueryAttribute{Name: "trace_id", Value: "dup"},   // duplicate name
	)
	if _, err := mc.ExecContext(ctx, "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}

	// Second statement on the same connection without attributes must not
	// see the attributes of the first one.
	if _, err := mc.ExecContext(context.Background(), "SELECT 2", nil); err != nil {
		t.Fatal(err)
	}

	payloads := splitPackets(t, conn.written)
	if len(payloads) != 2 {
		t.Fatalf("expected 2 packets, got %d", len(payloads))
	}

	// First packet: COM_QUERY with 5 attributes.
	pkt := payloads[0]
	if pkt[0] != comQuery {
		t.Fatalf("expected COM_QUERY, got %#x", pkt[0])
	}
	pos := 1
	if count, _, n := readLengthEncodedInteger(pkt[pos:]); count != 5 {
		t.Fatalf("parameter_count: got %d, want 5", count)
	} else {
		pos += n
	}
	if count, _, n := readLengthEncodedInteger(pkt[pos:]); count != 1 {
		t.Fatalf("parameter_set_count: got %d, want 1", count)
	} else {
		pos += n
	}

	// NULL bitmap: 1 byte for 5 attributes, only the NULL attribute (index 3).
	if pkt[pos] != 0x08 {
		t.Fatalf("NULL bitmap: got %#08b, want 0x08", pkt[pos])
	}
	pos++
	if pkt[pos] != 0x01 {
		t.Fatalf("new-params-bound flag: got %#x, want 0x01", pkt[pos])
	}
	pos++

	// Types: string, string, longlong, NULL, string.
	wantTypes := []byte{
		byte(fieldTypeString), 0x00,
		byte(fieldTypeString), 0x00,
		byte(fieldTypeLongLong), 0x00,
		byte(fieldTypeNULL), 0x00,
		byte(fieldTypeString), 0x00,
	}
	if !bytes.Equal(pkt[pos:pos+10], wantTypes) {
		t.Fatalf("parameter types: got %v, want %v", pkt[pos:pos+10], wantTypes)
	}
	pos += 10

	// Names, including the duplicated "trace_id".
	for _, want := range []string{"trace_id", "operation", "retries", "note", "trace_id"} {
		if got := readLenEncStr(t, pkt, &pos); got != want {
			t.Fatalf("attribute name: got %q, want %q", got, want)
		}
	}

	// Values.
	if got := readLenEncStr(t, pkt, &pos); got != "abc-123" {
		t.Fatalf("trace_id value: got %q, want %q", got, "abc-123")
	}
	if got := readLenEncStr(t, pkt, &pos); got != "" {
		t.Fatalf("operation value: got %q, want empty string", got)
	}
	if got := int64(binary.LittleEndian.Uint64(pkt[pos:])); got != 7 {
		t.Fatalf("retries value: got %d, want 7", got)
	}
	pos += 8
	// The NULL attribute has no value bytes.
	if got := readLenEncStr(t, pkt, &pos); got != "dup" {
		t.Fatalf("duplicate trace_id value: got %q, want %q", got, "dup")
	}

	if query := string(pkt[pos:]); query != "SELECT 1" {
		t.Fatalf("query: got %q, want %q", query, "SELECT 1")
	}

	// Second packet: COM_QUERY without attributes. parameter_count must be
	// zero, proving attributes do not linger on the connection.
	pkt = payloads[1]
	want := append([]byte{comQuery, 0x00, 0x01}, "SELECT 2"...)
	if !bytes.Equal(pkt, want) {
		t.Fatalf("second statement packet: got %v, want %v", pkt, want)
	}
}

func TestQueryAttributesStmtExecute(t *testing.T) {
	conn, mc := newRWMockConn(0)
	mc.capabilities = clientQueryAttributes
	stmt := &mysqlStmt{mc: mc, id: 1, paramCount: 1}
	conn.queuedReplies = [][]byte{okReply(1), okReply(1)}

	ctx := WithQueryAttributes(context.Background(),
		QueryAttribute{Name: "trace_id", Value: "t-1"},
	)
	if _, err := stmt.ExecContext(ctx, []driver.NamedValue{{Value: "hello"}}); err != nil {
		t.Fatal(err)
	}

	// Second execution on the same statement without attributes.
	if _, err := stmt.ExecContext(context.Background(), []driver.NamedValue{{Value: "world"}}); err != nil {
		t.Fatal(err)
	}

	payloads := splitPackets(t, conn.written)
	if len(payloads) != 2 {
		t.Fatalf("expected 2 packets, got %d", len(payloads))
	}

	// First packet: COM_STMT_EXECUTE with 1 parameter + 1 attribute.
	pkt := payloads[0]
	if pkt[0] != comStmtExecute {
		t.Fatalf("expected COM_STMT_EXECUTE, got %#x", pkt[0])
	}
	if id := binary.LittleEndian.Uint32(pkt[1:5]); id != 1 {
		t.Fatalf("statement id: got %d, want 1", id)
	}
	pos := 10 // command + statement id + flags + iteration count
	if pkt[pos] != 0x00 {
		t.Fatalf("NULL bitmap: got %#08b, want 0x00", pkt[pos])
	}
	pos++
	if pkt[pos] != 0x01 {
		t.Fatalf("new-params-bound flag: got %#x, want 0x01", pkt[pos])
	}
	pos++
	wantTypes := []byte{byte(fieldTypeString), 0x00, byte(fieldTypeString), 0x00}
	if !bytes.Equal(pkt[pos:pos+4], wantTypes) {
		t.Fatalf("parameter types: got %v, want %v", pkt[pos:pos+4], wantTypes)
	}
	pos += 4
	// Names: empty for the statement parameter, then the attribute name.
	if got := readLenEncStr(t, pkt, &pos); got != "" {
		t.Fatalf("statement parameter name: got %q, want empty", got)
	}
	if got := readLenEncStr(t, pkt, &pos); got != "trace_id" {
		t.Fatalf("attribute name: got %q, want %q", got, "trace_id")
	}
	// Values: statement parameter first, then the attribute.
	if got := readLenEncStr(t, pkt, &pos); got != "hello" {
		t.Fatalf("parameter value: got %q, want %q", got, "hello")
	}
	if got := readLenEncStr(t, pkt, &pos); got != "t-1" {
		t.Fatalf("attribute value: got %q, want %q", got, "t-1")
	}
	if pos != len(pkt) {
		t.Fatalf("trailing bytes in execute packet: %v", pkt[pos:])
	}

	// Second packet: only the statement parameter; the attribute is gone.
	pkt = payloads[1]
	pos = 10
	pos++ // NULL bitmap
	pos++ // new-params-bound flag
	wantTypes = []byte{byte(fieldTypeString), 0x00}
	if !bytes.Equal(pkt[pos:pos+2], wantTypes) {
		t.Fatalf("parameter types: got %v, want %v", pkt[pos:pos+2], wantTypes)
	}
	pos += 2
	if got := readLenEncStr(t, pkt, &pos); got != "" {
		t.Fatalf("statement parameter name: got %q, want empty", got)
	}
	if got := readLenEncStr(t, pkt, &pos); got != "world" {
		t.Fatalf("parameter value: got %q, want %q", got, "world")
	}
	if pos != len(pkt) {
		t.Fatalf("attribute leaked into second execution: trailing bytes %v", pkt[pos:])
	}
}

func TestQueryAttributesUnsupportedServer(t *testing.T) {
	conn, mc := newRWMockConn(0) // no CLIENT_QUERY_ATTRIBUTES capability

	ctx := WithQueryAttributes(context.Background(),
		QueryAttribute{Name: "trace_id", Value: "abc-123"},
	)

	// The default policy is an explicit error.
	if _, err := mc.ExecContext(ctx, "SELECT 1", nil); !errors.Is(err, ErrQueryAttributesUnsupported) {
		t.Fatalf("expected ErrQueryAttributesUnsupported, got %v", err)
	}
	if _, err := mc.QueryContext(ctx, "SELECT 1", nil); !errors.Is(err, ErrQueryAttributesUnsupported) {
		t.Fatalf("expected ErrQueryAttributesUnsupported, got %v", err)
	}
	stmt := &mysqlStmt{mc: mc, id: 1, paramCount: 0}
	if _, err := stmt.ExecContext(ctx, nil); !errors.Is(err, ErrQueryAttributesUnsupported) {
		t.Fatalf("expected ErrQueryAttributesUnsupported, got %v", err)
	}
	if _, err := stmt.QueryContext(ctx, nil); !errors.Is(err, ErrQueryAttributesUnsupported) {
		t.Fatalf("expected ErrQueryAttributesUnsupported, got %v", err)
	}
	if conn.writes != 0 {
		t.Fatal("statement was sent despite unsupported query attributes")
	}

	// With queryAttributes=ignore the attributes are dropped explicitly.
	mc.cfg.queryAttributesIgnore = true
	conn.queuedReplies = [][]byte{okReply(1)}
	if _, err := mc.ExecContext(ctx, "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}
	payloads := splitPackets(t, conn.written)
	if len(payloads) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(payloads))
	}
	// Without the capability the packet is a plain COM_QUERY: no
	// parameter_count, no attributes.
	want := append([]byte{comQuery}, "SELECT 1"...)
	if !bytes.Equal(payloads[0], want) {
		t.Fatalf("packet: got %v, want %v", payloads[0], want)
	}
}

func TestQueryAttributesCapabilityNegotiation(t *testing.T) {
	// Requested when the server offers it.
	mc := &mysqlConn{cfg: NewConfig()}
	mc.initCapabilities(clientProtocol41|clientQueryAttributes, 0)
	if mc.capabilities&clientQueryAttributes == 0 {
		t.Fatal("CLIENT_QUERY_ATTRIBUTES not negotiated although the server supports it")
	}

	// Not requested when the server does not offer it.
	mc = &mysqlConn{cfg: NewConfig()}
	mc.initCapabilities(clientProtocol41, 0)
	if mc.capabilities&clientQueryAttributes != 0 {
		t.Fatal("CLIENT_QUERY_ATTRIBUTES negotiated although the server does not support it")
	}
}

func TestQueryAttributesContext(t *testing.T) {
	// No attributes: the context is returned unchanged.
	ctx := context.Background()
	if got := WithQueryAttributes(ctx); got != ctx {
		t.Fatal("WithQueryAttributes without attributes changed the context")
	}
	if attrs := queryAttributesFromContext(ctx); attrs != nil {
		t.Fatalf("unexpected attributes: %v", attrs)
	}

	// The attributes are copied; mutating the caller's slice must not
	// affect the stored ones.
	attrs := []QueryAttribute{{Name: "trace_id", Value: "abc"}}
	ctx = WithQueryAttributes(ctx, attrs...)
	attrs[0] = QueryAttribute{Name: "mutated", Value: "bad"}
	got := queryAttributesFromContext(ctx)
	if len(got) != 1 || got[0].Name != "trace_id" || got[0].Value != "abc" {
		t.Fatalf("stored attributes were mutated: %v", got)
	}
}

func TestQueryAttributesInvalidValue(t *testing.T) {
	_, mc := newRWMockConn(0)
	mc.capabilities = clientQueryAttributes

	ctx := WithQueryAttributes(context.Background(),
		QueryAttribute{Name: "bad", Value: struct{}{}},
	)
	if _, err := mc.ExecContext(ctx, "SELECT 1", nil); err == nil ||
		!strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("expected conversion error naming the attribute, got %v", err)
	}
}

func TestQueryAttributesDSN(t *testing.T) {
	// Default: statements with attributes fail on unsupported servers.
	cfg := NewConfig()
	if cfg.queryAttributesIgnore {
		t.Fatal("queryAttributesIgnore must default to false")
	}

	cfg, err := ParseDSN("user:pass@tcp(localhost:3306)/db?queryAttributes=ignore")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.queryAttributesIgnore {
		t.Fatal("queryAttributes=ignore was not applied")
	}
	if dsn := cfg.FormatDSN(); !strings.Contains(dsn, "queryAttributes=ignore") {
		t.Fatalf("FormatDSN lost queryAttributes=ignore: %q", dsn)
	}

	cfg, err = ParseDSN("user:pass@tcp(localhost:3306)/db?queryAttributes=error")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.queryAttributesIgnore {
		t.Fatal("queryAttributes=error must keep the explicit error policy")
	}

	if _, err = ParseDSN("user:pass@tcp(localhost:3306)/db?queryAttributes=bogus"); err == nil {
		t.Fatal("expected an error for an invalid queryAttributes value")
	}

	// Functional option equivalent.
	cfg = NewConfig()
	if err := cfg.Apply(IgnoreUnsupportedQueryAttributes(true)); err != nil {
		t.Fatal(err)
	}
	if !cfg.queryAttributesIgnore {
		t.Fatal("IgnoreUnsupportedQueryAttributes(true) was not applied")
	}
}
