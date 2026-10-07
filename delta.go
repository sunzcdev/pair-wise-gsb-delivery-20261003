// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/hashicorp/go-msgpack/v2/codec"
)

const (
	// defaultDeltaSyncMaxRounds bounds how many request/response rounds
	// are allowed in each direction of an incremental sync.
	defaultDeltaSyncMaxRounds = 16
)

// deltaStep identifies the role of an individual frame in an incremental
// push/pull conversation.
type deltaStep uint8

const (
	// deltaDigest opens an incremental sync and carries the sender's
	// membership state together with its application digest.
	deltaDigest deltaStep = iota

	// deltaFetch carries a request produced by MissingDeltaChanges.
	deltaFetch

	// deltaChanges answers a deltaFetch with ordered changes.
	deltaChanges

	// deltaSnapshot answers a deltaFetch with a full LocalState snapshot.
	deltaSnapshot
)

// deltaFrameHeader precedes the opaque application payload of every frame
// exchanged over an incremental sync connection.
type deltaFrameHeader struct {
	Step       deltaStep
	Nodes      int
	Join       bool
	WantFull   bool
	PayloadLen int
}

// deltaErrorFrame carries the error message used to terminate a failed
// incremental sync.
type deltaErrorFrame struct {
	Error string
}

// errDeltaUnsupported indicates that the remote peer does not understand
// incremental sync. The caller must retry with a classic full push/pull.
var errDeltaUnsupported = errors.New("peer does not support incremental push/pull")

// deltaBudget tracks the total number of application payload bytes
// exchanged in both directions during a single incremental sync.
type deltaBudget struct {
	remaining int
}

func newDeltaBudget(limit int) *deltaBudget {
	if limit <= 0 {
		limit = maxPushStateBytes
	}
	return &deltaBudget{remaining: limit}
}

// charge accounts for payload bytes in either direction and returns an
// error once the total budget is exhausted.
func (b *deltaBudget) charge(n int) error {
	if n > b.remaining {
		return fmt.Errorf("incremental sync byte limit exceeded (%d more than %d)", n-b.remaining, b.remaining)
	}
	b.remaining -= n
	return nil
}

// deltaStream multiplexes the self-delimiting frames of an incremental sync
// over a single TCP connection. All frames share one buffered reader so
// that bytes read while decoding one frame remain available for the next.
type deltaStream struct {
	conn        net.Conn
	reader      *bufio.Reader
	streamLabel string
	m           *Memberlist
}

func newDeltaStream(m *Memberlist, conn net.Conn, streamLabel string) *deltaStream {
	return &deltaStream{
		conn:        conn,
		reader:      bufio.NewReader(conn),
		streamLabel: streamLabel,
		m:           m,
	}
}

func (m *Memberlist) deltaLimits() (maxRounds int) {
	maxRounds = m.config.DeltaSyncMaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultDeltaSyncMaxRounds
	}
	return maxRounds
}

// writeFrame sends a membership node list and an opaque application payload
// as a single self-delimiting frame.
func (s *deltaStream) writeFrame(step deltaStep, nodes []pushNodeState, join, wantFull bool, payload []byte) error {
	if err := s.conn.SetDeadline(deadlineTCP(s.m)); err != nil {
		s.m.logger.Printf("Err: Could not set the deadline: %s", err)
	}

	bufConn := bytes.NewBuffer(nil)
	if _, err := bufConn.Write([]byte{byte(deltaSyncMsg)}); err != nil {
		return err
	}

	hd := codec.MsgpackHandle{}
	enc := codec.NewEncoder(bufConn, &hd)
	header := deltaFrameHeader{
		Step:       step,
		Nodes:      len(nodes),
		Join:       join,
		WantFull:   wantFull,
		PayloadLen: len(payload),
	}
	if err := enc.Encode(&header); err != nil {
		return err
	}
	for i := range nodes {
		if err := enc.Encode(&nodes[i]); err != nil {
			return err
		}
	}
	if len(payload) > 0 {
		if _, err := bufConn.Write(payload); err != nil {
			return err
		}
	}
	return s.m.rawSendMsgStream(s.conn, bufConn.Bytes(), s.streamLabel)
}

// writeError terminates the conversation with an application visible error.
func (s *deltaStream) writeError(err error) error {
	bufConn := bytes.NewBuffer(nil)
	if _, err := bufConn.Write([]byte{byte(deltaSyncMsg)}); err != nil {
		return err
	}
	hd := codec.MsgpackHandle{}
	if err := codec.NewEncoder(bufConn, &hd).Encode(&deltaErrorFrame{err.Error()}); err != nil {
		return err
	}
	return s.m.rawSendMsgStream(s.conn, bufConn.Bytes(), s.streamLabel)
}

// readFrame decodes the next frame from the shared buffered reader.
func (s *deltaStream) readFrame() (deltaFrameHeader, []pushNodeState, []byte, error) {
	if err := s.conn.SetDeadline(deadlineTCP(s.m)); err != nil {
		s.m.logger.Printf("Err: Could not set the deadline: %s", err)
	}

	msgType, payloadReader, dec, err := s.m.readStream(s.reader, s.streamLabel)
	if err != nil {
		return deltaFrameHeader{}, nil, nil, err
	}
	if msgType != deltaSyncMsg {
		return deltaFrameHeader{}, nil, nil, fmt.Errorf("received invalid msgType (%d), expected deltaSyncMsg", msgType)
	}

	// An error frame is just the deltaErrorFrame struct and no header.
	var maybeError deltaErrorFrame
	if err := dec.Decode(&maybeError); err == nil && maybeError.Error != "" {
		return deltaFrameHeader{}, nil, nil, fmt.Errorf("remote delta sync error: %s", maybeError.Error)
	}

	var header deltaFrameHeader
	hd := codec.MsgpackHandle{}
	dec = codec.NewDecoder(payloadReader, &hd)
	if err := dec.Decode(&header); err != nil {
		return deltaFrameHeader{}, nil, nil, err
	}
	return s.readFrameBody(payloadReader, header)
}

// readFirstFrame parses an already read message type (for the inbound side,
// whose first frame is read by the shared stream dispatcher).
func (s *deltaStream) readFirstFrame(payloadReader io.Reader, dec *codec.Decoder) (deltaFrameHeader, []pushNodeState, []byte, error) {
	var maybeError deltaErrorFrame
	if err := dec.Decode(&maybeError); err == nil && maybeError.Error != "" {
		return deltaFrameHeader{}, nil, nil, fmt.Errorf("remote delta sync error: %s", maybeError.Error)
	}

	var header deltaFrameHeader
	hd := codec.MsgpackHandle{}
	dec = codec.NewDecoder(payloadReader, &hd)
	if err := dec.Decode(&header); err != nil {
		return deltaFrameHeader{}, nil, nil, err
	}
	return s.readFrameBody(payloadReader, header)
}

func (s *deltaStream) readFrameBody(payloadReader io.Reader, header deltaFrameHeader) (deltaFrameHeader, []pushNodeState, []byte, error) {
	if header.Nodes < 0 || header.Nodes > maxPushStateNodes {
		return deltaFrameHeader{}, nil, nil, fmt.Errorf("number of nodes in delta frame (%d) exceeds limit", header.Nodes)
	}
	if header.PayloadLen < 0 || header.PayloadLen > maxPushStateBytes {
		return deltaFrameHeader{}, nil, nil, fmt.Errorf("delta payload length (%d) exceeds limit", header.PayloadLen)
	}

	nodes := make([]pushNodeState, header.Nodes)
	for i := 0; i < header.Nodes; i++ {
		if err := dec := (*codec.Decoder)(nil); dec != nil {
		}
	}

	return header, nil, nil, nil
}
