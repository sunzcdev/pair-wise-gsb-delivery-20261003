// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPreference_WireBackwardCompatible verifies that Priority is an additive
// msgpack field: an encoded request without the field decodes with priority 0
// (the behaviour of an old binary), and a priority-bearing request survives a
// round trip.
func TestPreference_WireBackwardCompatible(t *testing.T) {
	req := &RequestVoteRequest{
		Term:         7,
		Priority:     42,
		LastLogIndex: 3,
		LastLogTerm:  2,
	}
	buf, err := encodeMsgPack(req)
	require.NoError(t, err)

	var decoded RequestVoteRequest
	require.NoError(t, decodeMsgPack(buf.Bytes(), &decoded))
	require.Equal(t, 42, decoded.Priority)
	require.Equal(t, uint64(7), decoded.Term)

	// Pre-vote carries the same additive field.
	pre := &RequestPreVoteRequest{Term: 8, Priority: 99}
	preBuf, err := encodeMsgPack(pre)
	require.NoError(t, err)
	var preDecoded RequestPreVoteRequest
	require.NoError(t, decodeMsgPack(preBuf.Bytes(), &preDecoded))
	require.Equal(t, 99, preDecoded.Priority)

	// Simulate an old binary: a request struct with the field absent (encoded
	// as a map without it) decodes to zero priority.
	oldWire := map[string]interface{}{
		"Term":         uint64(5),
		"LastLogIndex": uint64(1),
		"LastLogTerm":  uint64(1),
		"RPCHeader":    map[string]interface{}{"ProtocolVersion": int(3)},
	}
	oldBuf, err := encodeMsgPack(oldWire)
	require.NoError(t, err)
	var asIfOld RequestVoteRequest
	require.NoError(t, decodeMsgPack(oldBuf.Bytes(), &asIfOld))
	require.Equal(t, 0, asIfOld.Priority, "old nodes must read missing priority as zero")
}
