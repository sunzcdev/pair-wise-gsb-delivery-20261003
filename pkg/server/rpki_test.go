// Copyright (C) 2015-2021 Nippon Telegraph and Telephone Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/internal/pkg/table"
	"github.com/osrg/gobgp/v4/pkg/config/oc"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/packet/rtr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_readRTRMessageRejectsOversizedLength(t *testing.T) {
	header := make([]byte, rtr.RTR_MIN_LEN)
	header[0] = 1 // Protocol Version
	header[1] = rtr.RTR_CACHE_RESET
	binary.BigEndian.PutUint32(header[4:8], 0xffffffff)

	if _, err := readRTRMessage(bytes.NewReader(header)); err == nil {
		t.Fatal("expected an error when the RTR Length is too large, got nil")
	}
}

func Test_readRTRMessageAcceptsValidLength(t *testing.T) {
	header := make([]byte, rtr.RTR_MIN_LEN)
	header[0] = 1 // Protocol Version
	header[1] = rtr.RTR_CACHE_RESET
	binary.BigEndian.PutUint32(header[4:8], rtr.RTR_MIN_LEN)

	data, err := readRTRMessage(bytes.NewReader(header))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) != rtr.RTR_MIN_LEN {
		t.Fatalf("expected %d bytes, got %d", rtr.RTR_MIN_LEN, len(data))
	}
}

// mockRTRServer is a local RTR stand-in: it accepts TCP connections from
// the ROA client and lets the test drive RTR PDUs explicitly, including
// pauses and disconnects in the middle of an update.
type mockRTRServer struct {
	ln     net.Listener
	connCh chan net.Conn
}

func newMockRTRServer(t *testing.T) *mockRTRServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	s := &mockRTRServer{
		ln:     ln,
		connCh: make(chan net.Conn, 4),
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.connCh <- conn
		}
	}()
	return s
}

func (s *mockRTRServer) addr() string {
	return s.ln.Addr().String()
}

func (s *mockRTRServer) accept(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-s.connCh:
		t.Cleanup(func() { conn.Close() })
		return conn
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the RTR client to connect")
		return nil
	}
}

func sendRTR(t *testing.T, conn net.Conn, msgs ...rtr.RTRMessage) {
	t.Helper()
	for _, msg := range msgs {
		data, err := msg.Serialize()
		require.NoError(t, err)
		_, err = conn.Write(data)
		require.NoError(t, err)
	}
}

func readResetQuery(t *testing.T, conn net.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	data, err := readRTRMessage(conn)
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	msg, err := rtr.ParseRTR(data)
	require.NoError(t, err)
	require.IsType(t, &rtr.RTRResetQuery{}, msg)
}

// roaTestHarness serializes access to the ROA table through a mutex, the
// same way BgpServer serializes ROA events and RIB walks with its lock.
type roaTestHarness struct {
	manager *roaManager
	table   *table.ROATable
	mu      sync.Mutex
}

func newROATestHarness(t *testing.T) *roaTestHarness {
	t.Helper()
	rt := table.NewROATable(logger)
	m := newROAManager(rt, logger)
	h := &roaTestHarness{manager: m, table: rt}
	go func() {
		for ev := range m.ReceiveROA() {
			h.mu.Lock()
			m.HandleROAEvent(ev)
			h.mu.Unlock()
		}
	}()
	return h
}

func (h *roaTestHarness) validationStatus(prefix string, originAS uint32) oc.RpkiValidationResultType {
	h.mu.Lock()
	defer h.mu.Unlock()
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(prefix))
	if err != nil {
		return oc.RPKI_VALIDATION_RESULT_TYPE_NONE
	}
	attrs := []bgp.PathAttributeInterface{bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{
		bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{64512, originAS}),
	})}
	path := table.NewPath(bgp.RF_IPv4_UC, &table.PeerInfo{LocalAS: 64512}, bgp.PathNLRI{NLRI: nlri}, false, attrs, time.Now(), false)
	return h.table.Validate(path).Status
}

func (h *roaTestHarness) waitForStatus(t *testing.T, prefix string, originAS uint32, want oc.RpkiValidationResultType) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.validationStatus(prefix, originAS); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("validation status for %s originated by AS%d did not become %s", prefix, originAS, want)
}

func roaAnnounce(prefix string, prefixLen, maxLen uint8, as uint32) *rtr.RTRIPPrefix {
	return rtr.NewRTRIPPrefix(netip.MustParseAddr(prefix), prefixLen, maxLen, as, 1)
}

func roaWithdraw(prefix string, prefixLen, maxLen uint8, as uint32) *rtr.RTRIPPrefix {
	return rtr.NewRTRIPPrefix(netip.MustParseAddr(prefix), prefixLen, maxLen, as, 0)
}

// A single RTR update that withdraws an old ROA and announces a new one
// must take effect only as a whole, at EndOfData. While the update is
// still in flight (even across long pauses), validation results must be
// computed from the previously committed set.
func TestRTRUpdateAppliedAtomicallyAtEndOfData(t *testing.T) {
	server := newMockRTRServer(t)
	h := newROATestHarness(t)
	require.NoError(t, h.manager.AddServer(server.addr(), 0))

	conn := server.accept(t)
	readResetQuery(t, conn)

	// First complete update: 10.0.0.0/24 max /24 AS 65000.
	sendRTR(t, conn,
		rtr.NewRTRCacheResponse(1),
		roaAnnounce("10.0.0.0", 24, 24, 65000),
		rtr.NewRTREndOfData(1, 1),
	)
	h.waitForStatus(t, "10.0.0.0/24", 65000, oc.RPKI_VALIDATION_RESULT_TYPE_VALID)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_INVALID, h.validationStatus("10.0.0.0/24", 65001))

	// The cache replaces the ROA: withdraw AS 65000, announce AS 65001.
	sendRTR(t, conn, rtr.NewRTRCacheResponse(1))
	sendRTR(t, conn, roaWithdraw("10.0.0.0", 24, 24, 65000))
	// Pause in the middle of the update: the withdrawal must be staged,
	// not applied, so routes still see the complete previous set.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_VALID, h.validationStatus("10.0.0.0/24", 65000))
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_INVALID, h.validationStatus("10.0.0.0/24", 65001))

	// The announcement must be staged as well.
	sendRTR(t, conn, roaAnnounce("10.0.0.0", 24, 24, 65001))
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_VALID, h.validationStatus("10.0.0.0/24", 65000))
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_INVALID, h.validationStatus("10.0.0.0/24", 65001))

	// EndOfData commits the whole update at once and validation results
	// flip to the new generation.
	sendRTR(t, conn, rtr.NewRTREndOfData(1, 2))
	h.waitForStatus(t, "10.0.0.0/24", 65001, oc.RPKI_VALIDATION_RESULT_TYPE_VALID)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_INVALID, h.validationStatus("10.0.0.0/24", 65000))
}

// If the connection drops in the middle of an update, the staged changes
// must be discarded and the previously committed set must stay in effect.
// When the cache comes back with a new session, the old generation is
// replaced wholesale by the first complete update of the new session.
func TestRTRDisconnectMidUpdateKeepsCommittedSet(t *testing.T) {
	server := newMockRTRServer(t)
	h := newROATestHarness(t)
	require.NoError(t, h.manager.AddServer(server.addr(), 0))

	conn := server.accept(t)
	readResetQuery(t, conn)

	sendRTR(t, conn,
		rtr.NewRTRCacheResponse(1),
		roaAnnounce("10.0.0.0", 24, 24, 65000),
		rtr.NewRTREndOfData(1, 1),
	)
	h.waitForStatus(t, "10.0.0.0/24", 65000, oc.RPKI_VALIDATION_RESULT_TYPE_VALID)

	// Start an update that withdraws the ROA, then drop the connection
	// before EndOfData.
	sendRTR(t, conn,
		rtr.NewRTRCacheResponse(1),
		roaWithdraw("10.0.0.0", 24, 24, 65000),
	)
	require.NoError(t, conn.Close())

	// The staged withdrawal must be discarded; the committed ROA stays.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_VALID, h.validationStatus("10.0.0.0/24", 65000))

	// The client reconnects and the cache answers the reset query with a
	// full dump under a new session.
	conn = server.accept(t)
	readResetQuery(t, conn)
	sendRTR(t, conn,
		rtr.NewRTRCacheResponse(2),
		roaAnnounce("10.0.0.0", 24, 24, 65002),
		rtr.NewRTREndOfData(2, 3),
	)

	// The session change wipes the old generation and the validation
	// results move to the new one.
	h.waitForStatus(t, "10.0.0.0/24", 65002, oc.RPKI_VALIDATION_RESULT_TYPE_VALID)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_INVALID, h.validationStatus("10.0.0.0/24", 65000))
}

// Messages arriving late from a previous connection must not roll the
// table back to an older generation.
func TestRTRLateMessagesFromOldConnectionIgnored(t *testing.T) {
	rt := table.NewROATable(logger)
	m := newROAManager(rt, logger)
	h := &roaTestHarness{manager: m, table: rt}

	host := "127.0.0.1:3323"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := &roaClient{
		host:        host,
		eventCh:     m.eventCh,
		pendingROAs: make([]pendingROA, 0),
		ctx:         ctx,
		cancelfnc:   cancel,
	}
	m.clientMap[host] = client

	// net.Pipe connections are only used as connection identities; all
	// events are injected synchronously.
	oldConn, _ := net.Pipe()
	newConn, _ := net.Pipe()
	t.Cleanup(func() { oldConn.Close(); newConn.Close() })

	inject := func(conn net.Conn, msgs ...rtr.RTRMessage) {
		t.Helper()
		for _, msg := range msgs {
			data, err := msg.Serialize()
			require.NoError(t, err)
			h.mu.Lock()
			m.HandleROAEvent(&roaEvent{
				EventType: roaRTR,
				Src:       host,
				Data:      data,
				conn:      conn,
				timestamp: time.Now(),
			})
			h.mu.Unlock()
		}
	}

	// First generation on the old connection.
	client.conn = oldConn
	inject(oldConn,
		rtr.NewRTRCacheResponse(1),
		roaAnnounce("10.0.0.0", 24, 24, 65000),
		rtr.NewRTREndOfData(1, 1),
	)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_VALID, h.validationStatus("10.0.0.0/24", 65000))

	// Reconnect: the new connection brings a new session and generation.
	client.conn = newConn
	inject(newConn,
		rtr.NewRTRCacheResponse(2),
		roaAnnounce("10.0.0.0", 24, 24, 65002),
		rtr.NewRTREndOfData(2, 2),
	)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_VALID, h.validationStatus("10.0.0.0/24", 65002))
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_INVALID, h.validationStatus("10.0.0.0/24", 65000))

	// Late messages on the old connection: a CacheResponse must not
	// reopen staging, a withdrawal must not touch the table, and an
	// EndOfData must not roll back session or serial.
	inject(oldConn,
		rtr.NewRTRCacheResponse(1),
		roaWithdraw("10.0.0.0", 24, 24, 65002),
		rtr.NewRTREndOfData(1, 9),
	)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_VALID, h.validationStatus("10.0.0.0/24", 65002))
	assert.Equal(t, uint16(2), client.sessionID)
	assert.Equal(t, uint32(2), client.serialNumber)
	assert.True(t, client.endOfData)
	assert.True(t, client.conn == newConn)

	// A late disconnect from the old connection must not tear down the
	// current one either.
	h.mu.Lock()
	m.HandleROAEvent(&roaEvent{
		EventType: roaDisconnected,
		Src:       host,
		conn:      oldConn,
		timestamp: time.Now(),
	})
	h.mu.Unlock()
	assert.True(t, client.conn == newConn)
	assert.True(t, client.endOfData)
	assert.Equal(t, oc.RPKI_VALIDATION_RESULT_TYPE_VALID, h.validationStatus("10.0.0.0/24", 65002))
}
