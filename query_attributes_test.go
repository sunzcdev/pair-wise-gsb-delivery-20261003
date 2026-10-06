package mysql

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

func testQueryAttributes() []QueryAttribute {
	return []QueryAttribute{
		{Name: "", Value: ""},
		{Name: "trace_id", Value: 42},
		{Name: "operation", Value: nil},
		{Name: "trace_id", Value: "other"},
	}
}

func testEncodedQueryAttributes(includeTypes bool) []byte {
	var expected []byte
	expected = append(expected, 0x04) // NULL bitmap: only the NULL attribute is set.
	expected = append(expected, 0x01) // new_params_bound_flag
	if includeTypes {
		expected = append(expected,
			byte(fieldTypeString), 0x00,
			byte(fieldTypeLongLong), 0x00,
			byte(fieldTypeNULL), 0x00,
			byte(fieldTypeString), 0x00,
		)
	}
	expected = appendLengthEncodedString(expected, "")
	expected = appendLengthEncodedString(expected, "trace_id")
	expected = appendLengthEncodedString(expected, "operation")
	expected = appendLengthEncodedString(expected, "trace_id")
	expected = appendLengthEncodedInteger(expected, 0)
	expected = append(expected, 42, 0, 0, 0, 0, 0, 0, 0)
	expected = appendLengthEncodedString(expected, "other")
	return expected
}

func TestQueryAttributesAreSeparateFromNamedValues(t *testing.T) {
	conn, mc := newRWMockConn(0)
	mc.capabilities |= clientQueryAttributes

	stmt := &mysqlStmt{mc: mc, id: 1, paramCount: 1}
	attrs := testQueryAttributes()
	values, names, err := stmt.mc.prepareQueryAttributes(WithQueryAttributes(context.Background(), attrs...))
	if err != nil {
		t.Fatal(err)
	}
	if err := stmt.writeExecutePacket([]driver.Value{int64(7)}, values, names); err != nil {
		t.Fatal(err)
	}

	packet, rest := nextWrittenPacket(t, conn.written)
	if len(rest) != 0 {
		t.Fatalf("unexpected extra packets: %d bytes", len(rest))
	}

	expected := []byte{
		comStmtExecute,
		1, 0, 0, 0,
		0x00,
		1, 0, 0, 0,
	}
	expected = append(expected, 0x05, 0x01) // Five total parameters in one parameter set.
	expected = append(expected, 0x08)       // Ordinary parameter and the NULL query attribute are set.
	expected = append(expected, 0x01)
	expected = append(expected,
		byte(fieldTypeLongLong), 0x00,
		byte(fieldTypeString), 0x00,
		byte(fieldTypeLongLong), 0x00,
		byte(fieldTypeNULL), 0x00,
		byte(fieldTypeString), 0x00,
	)
	expected = appendLengthEncodedString(expected, "")
	expected = appendLengthEncodedString(expected, "trace_id")
	expected = appendLengthEncodedString(expected, "operation")
	expected = appendLengthEncodedString(expected, "trace_id")
	expected = append(expected, 7, 0, 0, 0, 0, 0, 0, 0)
	expected = appendLengthEncodedInteger(expected, 0)
	expected = append(expected, 42, 0, 0, 0, 0, 0, 0, 0)
	expected = appendLengthEncodedString(expected, "other")

	if !bytes.Equal(packet, expected) {
		t.Fatalf("prepared packet = % x, want % x", packet, expected)
	}
}

func TestTextQueryAttributesDoNotLeakToNextQuery(t *testing.T) {
	conn, mc := newRWMockConn(0)
	mc.capabilities |= clientQueryAttributes

	values, names, err := mc.prepareQueryAttributes(
		WithQueryAttributes(context.Background(), testQueryAttributes()...),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := mc.writeQueryPacket("SELECT 1", values, names); err != nil {
		t.Fatal(err)
	}
	if err := mc.writeQueryPacket("SELECT 2", nil, nil); err != nil {
		t.Fatal(err)
	}

	first, remaining := nextWrittenPacket(t, conn.written)
	second, rest := nextWrittenPacket(t, remaining)
	if len(rest) != 0 {
		t.Fatalf("unexpected extra packets: %d bytes", len(rest))
	}

	expectedFirst := []byte{comQuery, 0x01, 0x04}
	expectedFirst = append(expectedFirst, testEncodedQueryAttributes(true)...)
	expectedFirst = append(expectedFirst, "SELECT 1"...)
	if !bytes.Equal(first, expectedFirst) {
		t.Fatalf("first query packet = % x, want % x", first, expectedFirst)
	}

	expectedSecond := []byte{comQuery, 0x01, 0x00}
	expectedSecond = append(expectedSecond, "SELECT 2"...)
	if !bytes.Equal(second, expectedSecond) {
		t.Fatalf("second query retained attributes: got % x, want % x", second, expectedSecond)
	}
}

func TestQueryAttributesAreScopedToContext(t *testing.T) {
	okPacket := []byte{
		0x07, 0x00, 0x00, 0x01,
		iOK,
		0x00, 0x00,
		0x02, 0x00,
		0x00, 0x00,
	}
	conn, mc := newRWMockConn(0)
	mc.capabilities |= clientQueryAttributes
	conn.queuedReplies = [][]byte{okPacket, okPacket}

	ctx := WithQueryAttributes(context.Background(), testQueryAttributes()...)
	if _, err := mc.ExecContext(ctx, "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.ExecContext(context.Background(), "SELECT 2", nil); err != nil {
		t.Fatal(err)
	}

	first, remaining := nextWrittenPacket(t, conn.written)
	second, rest := nextWrittenPacket(t, remaining)
	if len(rest) != 0 {
		t.Fatalf("unexpected extra packets: %d bytes", len(rest))
	}
	if !bytes.HasSuffix(first, []byte("SELECT 1")) || !bytes.Contains(first, []byte("trace_id")) {
		t.Fatalf("first request did not carry context attributes: % x", first)
	}
	expectedSecond := append([]byte{comQuery, 0x01, 0x00}, "SELECT 2"...)
	if !bytes.Equal(second, expectedSecond) {
		t.Fatalf("next request inherited attributes: got % x, want % x", second, expectedSecond)
	}
}

func TestQueryAttributesRemainSeparateFromNamedValues(t *testing.T) {
	okPacket := []byte{
		0x07, 0x00, 0x00, 0x01,
		iOK,
		0x00, 0x00,
		0x02, 0x00,
		0x00, 0x00,
	}
	conn, mc := newRWMockConn(0)
	mc.capabilities |= clientQueryAttributes
	mc.cfg.InterpolateParams = true
	conn.queuedReplies = [][]byte{okPacket}

	ctx := WithQueryAttributes(context.Background(), testQueryAttributes()...)
	if _, err := mc.ExecContext(ctx, "SELECT ?", []driver.NamedValue{{
		Ordinal: 1,
		Value:   int64(7),
	}}); err != nil {
		t.Fatal(err)
	}

	packet, rest := nextWrittenPacket(t, conn.written)
	if len(rest) != 0 {
		t.Fatalf("unexpected extra packets: %d bytes", len(rest))
	}
	expected := []byte{comQuery, 0x01, 0x04}
	expected = append(expected, testEncodedQueryAttributes(true)...)
	expected = append(expected, "SELECT 7"...)
	if !bytes.Equal(packet, expected) {
		t.Fatalf("text query packet = % x, want % x", packet, expected)
	}
}

func TestPreparedQueryAttributesDoNotLeakToNextExecution(t *testing.T) {
	conn, mc := newRWMockConn(0)
	mc.capabilities |= clientQueryAttributes
	stmt := &mysqlStmt{mc: mc, id: 1, paramCount: 0}

	values, names, err := mc.prepareQueryAttributes(
		WithQueryAttributes(context.Background(), testQueryAttributes()...),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := stmt.writeExecutePacket(nil, values, names); err != nil {
		t.Fatal(err)
	}
	if err := stmt.writeExecutePacket(nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	first, remaining := nextWrittenPacket(t, conn.written)
	second, rest := nextWrittenPacket(t, remaining)
	if len(rest) != 0 {
		t.Fatalf("unexpected extra packets: %d bytes", len(rest))
	}

	expectedFirst := []byte{
		comStmtExecute,
		1, 0, 0, 0,
		0x00,
		1, 0, 0, 0,
	}
	expectedFirst = append(expectedFirst, 0x04, 0x01) // Four query attributes in one parameter set.
	expectedFirst = append(expectedFirst, testEncodedQueryAttributes(true)...)
	if !bytes.Equal(first, expectedFirst) {
		t.Fatalf("first execute packet = % x, want % x", first, expectedFirst)
	}

	expectedSecond := []byte{
		comStmtExecute,
		1, 0, 0, 0,
		0x00,
		1, 0, 0, 0,
		0x00, // Zero parameters; the parameter section ends here.
	}
	if !bytes.Equal(second, expectedSecond) {
		t.Fatalf("second execute retained attributes: got % x, want % x", second, expectedSecond)
	}
}

func TestQueryAttributesUnsupportedPolicy(t *testing.T) {
	mc := &mysqlConn{capabilities: 0}
	attrs := testQueryAttributes()

	_, _, err := mc.prepareQueryAttributes(WithQueryAttributes(context.Background(), attrs...))
	if !errors.Is(err, ErrQueryAttributesNotSupported) {
		t.Fatalf("strict policy error = %v, want %v", err, ErrQueryAttributesNotSupported)
	}

	values, names, err := mc.prepareQueryAttributes(
		WithQueryAttributesIfSupported(context.Background(), attrs...),
	)
	if err != nil || values != nil || names != nil {
		t.Fatalf("fallback policy = values:%v names:%v err:%v", values, names, err)
	}
}

func TestQueryAttributesHandshakeCapability(t *testing.T) {
	mc := &mysqlConn{cfg: NewConfig()}
	mc.initCapabilities(clientQueryAttributes, 0)
	if mc.capabilities&clientQueryAttributes == 0 {
		t.Fatalf("negotiated capabilities = %b, want query attributes bit", mc.capabilities)
	}

	mc.initCapabilities(0, 0)
	if mc.capabilities&clientQueryAttributes != 0 {
		t.Fatalf("negotiated capabilities = %b, query attributes must not be claimed", mc.capabilities)
	}
}

func nextWrittenPacket(t *testing.T, written []byte) ([]byte, []byte) {
	t.Helper()
	if len(written) < 4 {
		t.Fatalf("written data too short: % x", written)
	}
	packetLen := 4 + int(written[0]) + int(written[1])<<8 + int(written[2])<<16
	if packetLen > len(written) {
		t.Fatalf("packet length %d exceeds written data %d", packetLen, len(written))
	}
	return written[4:packetLen], written[packetLen:]
}
