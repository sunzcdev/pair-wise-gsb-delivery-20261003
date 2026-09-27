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
	"io"
	"reflect"
	"testing"
)

func framePacket(seq byte, payload []byte) []byte {
	data := make([]byte, 4, 4+len(payload))
	putUint24(data[:3], len(payload))
	data[3] = seq
	return append(data, payload...)
}

// buildSessionStateOkPacket builds an OK packet (0x00 or 0xFE header) in the
// CLIENT_SESSION_TRACK layout, carrying the given session state changes block.
func buildSessionStateOkPacket(header byte, status statusFlag, changes []byte) []byte {
	data := []byte{header, 0x00, 0x00} // header, affected rows, last insert id
	data = append(data, byte(status), byte(status>>8))
	data = append(data, 0x00, 0x00) // warning count
	data = append(data, 0x00)       // info: empty length-encoded string
	data = appendLengthEncodedInteger(data, uint64(len(changes)))
	return append(data, changes...)
}

func sessionStateBlock(typ SessionTrackType, payload []byte) []byte {
	data := []byte{byte(typ)}
	data = appendLengthEncodedInteger(data, uint64(len(payload)))
	return append(data, payload...)
}

func sysVarPayload(name, value string) []byte {
	return appendLengthEncodedString(appendLengthEncodedString(nil, name), value)
}

func testColumnDefPacket(name string) []byte {
	data := appendLengthEncodedString(nil, "def") // catalog
	data = appendLengthEncodedString(data, "")    // schema
	data = appendLengthEncodedString(data, "")    // table
	data = appendLengthEncodedString(data, "")    // org_table
	data = appendLengthEncodedString(data, name)  // name
	data = appendLengthEncodedString(data, "")    // org_name
	data = append(data, 0x0c)                     // filler
	data = append(data, 0x21, 0x00)               // charset
	data = append(data, 0x01, 0x00, 0x00, 0x00)   // length
	data = append(data, byte(fieldTypeLong))      // type
	data = append(data, 0x00, 0x00)               // flags
	data = append(data, 0x00)                     // decimals
	return data
}

func TestSessionTrackCapabilityNegotiation(t *testing.T) {
	_, mc := newRWMockConn(0)
	mc.initCapabilities(clientProtocol41|clientDeprecateEOF|clientSessionTrack, 0)
	if mc.capabilities&clientSessionTrack == 0 {
		t.Fatal("expected CLIENT_SESSION_TRACK to be requested when the server supports it")
	}

	_, mc = newRWMockConn(0)
	mc.initCapabilities(clientProtocol41|clientDeprecateEOF, 0)
	if mc.capabilities&clientSessionTrack != 0 {
		t.Fatal("expected CLIENT_SESSION_TRACK to be disabled when the server does not support it")
	}
}

func TestHandleOkPacketSessionState(t *testing.T) {
	gtid := "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5"
	var changes []byte
	changes = append(changes, sessionStateBlock(SessionTrackSystemVariables, sysVarPayload("autocommit", "ON"))...)
	changes = append(changes, sessionStateBlock(SessionTrackSchema, appendLengthEncodedString(nil, "shop"))...)
	// Unknown block type; its payload deliberately looks like valid blocks to
	// verify it is skipped by length without misaligning the following block.
	changes = append(changes, sessionStateBlock(SessionTrackType(0x7f), []byte{0x01, 0x05, 'x', 0x00})...)
	changes = append(changes, sessionStateBlock(SessionTrackGTIDs, appendLengthEncodedString(nil, gtid))...)
	changes = append(changes, sessionStateBlock(SessionTrackStateChange, appendLengthEncodedString(nil, "1"))...)

	_, mc := newRWMockConn(0)
	mc.capabilities = clientProtocol41 | clientSessionTrack

	var got []SessionStateChange
	mc.cfg.sessionStateCallback = func(ch []SessionStateChange) {
		got = append(got, ch...)
	}

	pkt := buildSessionStateOkPacket(iOK, statusInAutocommit|statusSessionStateChanged, changes)
	if err := mc.resultUnchanged().handleOkPacket(pkt); err != nil {
		t.Fatal(err)
	}

	want := []SessionStateChange{
		{Type: SessionTrackSystemVariables, Name: "autocommit", Value: "ON"},
		{Type: SessionTrackSchema, Value: "shop"},
		{Type: SessionTrackGTIDs, Value: gtid},
		{Type: SessionTrackStateChange, Value: "1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected callback changes: got %+v, want %+v", got, want)
	}

	state := mc.SessionState()
	if state.Schema != "shop" {
		t.Fatalf("unexpected schema: got %q, want %q", state.Schema, "shop")
	}
	if state.SystemVars["autocommit"] != "ON" {
		t.Fatalf("unexpected system vars: got %v", state.SystemVars)
	}
	if !reflect.DeepEqual(state.GTIDs, []string{gtid}) {
		t.Fatalf("unexpected GTIDs: got %v", state.GTIDs)
	}

	// The returned state must be a copy.
	state.SystemVars["autocommit"] = "OFF"
	state.GTIDs[0] = "mutated"
	if mc.SessionState().SystemVars["autocommit"] != "ON" || mc.SessionState().GTIDs[0] != gtid {
		t.Fatal("SessionState() must return a copy")
	}
}

func TestHandleOkPacketSessionStateTruncated(t *testing.T) {
	status := statusInAutocommit | statusSessionStateChanged

	// OK packet prefix: header, affected rows, last insert id, status, warnings.
	prefix := []byte{iOK, 0x00, 0x00, byte(status), byte(status >> 8), 0x00, 0x00}

	tests := []struct {
		name string
		pkt  []byte
	}{
		{"MissingInfoAndState", prefix},
		{"TruncatedInfo", append(append([]byte{}, prefix...), 0x0a, 'x')},
		{"MissingStateInfo", append(append([]byte{}, prefix...), 0x00)},
		{"TruncatedStateInfo", append(append([]byte{}, prefix...), 0x00, 0x0a, 0x01)},
		{"BlockTypeWithoutPayload", append(append([]byte{}, prefix...), 0x00, 0x01, byte(SessionTrackSchema))},
		{"TruncatedBlockPayload", append(append([]byte{}, prefix...), 0x00, 0x02, byte(SessionTrackSchema), 0x05)},
		{"TruncatedSchemaValue", append(append([]byte{}, prefix...), 0x00, 0x04,
			byte(SessionTrackSchema), 0x03, 0x05, 'a', 'b')},
		{"SystemVarMissingValue", append(append([]byte{}, prefix...), 0x00, 0x0c,
			byte(SessionTrackSystemVariables), 0x0b, 0x0a, 'a', 'u', 't', 'o', 'c', 'o', 'm', 'm', 'i', 't')},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, mc := newRWMockConn(0)
			mc.capabilities = clientProtocol41 | clientSessionTrack
			if err := mc.resultUnchanged().handleOkPacket(tc.pkt); err != ErrMalformPkt {
				t.Fatalf("expected ErrMalformPkt, got %v", err)
			}
			if len(mc.pendingSessionState) != 0 {
				t.Fatal("no session state may be staged from a malformed packet")
			}
		})
	}
}

func TestSessionStatePublishedOnlyWhenCommandCompletes(t *testing.T) {
	_, mc := newRWMockConn(0)
	mc.capabilities = clientProtocol41 | clientSessionTrack

	var calls int
	mc.cfg.sessionStateCallback = func([]SessionStateChange) { calls++ }

	first := buildSessionStateOkPacket(iOK,
		statusInTrans|statusMoreResultsExists|statusSessionStateChanged,
		sessionStateBlock(SessionTrackSchema, appendLengthEncodedString(nil, "first")))
	if err := mc.resultUnchanged().handleOkPacket(first); err != nil {
		t.Fatal(err)
	}
	if state := mc.SessionState(); state.Schema != "" {
		t.Fatalf("state published before command completion: %+v", state)
	}
	if calls != 0 {
		t.Fatal("callback invoked before command completion")
	}

	second := buildSessionStateOkPacket(iOK,
		statusInAutocommit|statusSessionStateChanged,
		sessionStateBlock(SessionTrackSchema, appendLengthEncodedString(nil, "second")))
	if err := mc.resultUnchanged().handleOkPacket(second); err != nil {
		t.Fatal(err)
	}
	if state := mc.SessionState(); state.Schema != "second" {
		t.Fatalf("unexpected schema after completion: %+v", state)
	}
	if calls != 1 {
		t.Fatalf("callback invoked %d times, want 1", calls)
	}
}

// sessionStateQueryResponse returns the raw server response for a single
// column, two row result set terminated by an OK packet (0xFE header) that
// carries session state changes.
func sessionStateQueryResponse(changes []byte) []byte {
	var data []byte
	data = append(data, framePacket(1, []byte{0x01})...)
	data = append(data, framePacket(2, testColumnDefPacket("a"))...)
	data = append(data, framePacket(3, []byte{0x01, '1'})...)
	data = append(data, framePacket(4, []byte{0x01, '2'})...)
	data = append(data, framePacket(5, buildSessionStateOkPacket(iEOF,
		statusInAutocommit|statusSessionStateChanged, changes))...)
	return data
}

func TestSessionStateNotPublishedBeforeRowsConsumed(t *testing.T) {
	var changes []byte
	changes = append(changes, sessionStateBlock(SessionTrackSchema, appendLengthEncodedString(nil, "newdb"))...)
	changes = append(changes, sessionStateBlock(SessionTrackSystemVariables, sysVarPayload("autocommit", "OFF"))...)
	changes = append(changes, sessionStateBlock(SessionTrackGTIDs, appendLengthEncodedString(nil, "uuid:1-3"))...)

	newConn := func() (*mysqlConn, *textRows, []driver.Value) {
		conn, mc := newRWMockConn(0)
		mc.capabilities = clientProtocol41 | clientDeprecateEOF | clientSessionTrack
		conn.queuedReplies = [][]byte{sessionStateQueryResponse(changes)}
		rows, err := mc.query("SELECT 1", nil)
		if err != nil {
			t.Fatal(err)
		}
		return mc, rows, make([]driver.Value, 1)
	}

	assertNotPublished := func(mc *mysqlConn) {
		t.Helper()
		if state := mc.SessionState(); state.Schema != "" || len(state.SystemVars) != 0 || len(state.GTIDs) != 0 {
			t.Fatalf("session state published before the result set was fully read: %+v", state)
		}
	}
	assertPublished := func(mc *mysqlConn) {
		t.Helper()
		state := mc.SessionState()
		if state.Schema != "newdb" || state.SystemVars["autocommit"] != "OFF" ||
			!reflect.DeepEqual(state.GTIDs, []string{"uuid:1-3"}) {
			t.Fatalf("unexpected session state: %+v", state)
		}
	}

	t.Run("ReadToEOF", func(t *testing.T) {
		mc, rows, dest := newConn()
		if err := rows.Next(dest); err != nil {
			t.Fatal(err)
		}
		assertNotPublished(mc)
		if err := rows.Next(dest); err != nil {
			t.Fatal(err)
		}
		assertNotPublished(mc)
		if err := rows.Next(dest); err != io.EOF {
			t.Fatalf("expected io.EOF, got %v", err)
		}
		assertPublished(mc)
	})

	t.Run("CloseDiscardsRemainingRows", func(t *testing.T) {
		mc, rows, dest := newConn()
		if err := rows.Next(dest); err != nil {
			t.Fatal(err)
		}
		assertNotPublished(mc)
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		assertPublished(mc)
	})
}
