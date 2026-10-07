// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"github.com/hashicorp/go-msgpack/v2/codec"
)

var (
	// errFallback marks an outcome where an application requested a full
	// sync.
	errFallback = errors.New("incremental sync fallback requested")

	// errIncrementalUnsupported marks that the remote side does not
	// implement IncrementalDelegate and the caller must use the regular
	// full-state push/pull.
	errIncrementalUnsupported = errors.New("remote side does not support incremental sync")
)

// incrementalUnsupportedResp is sent by a server that received an
// incremental offer but whose delegate does not implement
// IncrementalDelegate. The initiator recognizes it and retries with a full
// sync. The sentinel text is part of the wire behavior between supported
// versions.
const incrementalUnsupportedResp = "memberlist: incremental sync unsupported"

// incSyncSession drives a single incremental push/pull negotiation over an
// already established, labeled (and possibly encrypted/compressed) stream.
//
// memberlist never interprets the application payload; it only enforces a
// strict alternating frame schedule, in-order change delivery and the round
// and byte budgets:
//
//	initiator -> responder : incOffer{summary A}
//	responder -> initiator : incOfferAck{summary B}
//	initiator -> responder : incRound{request A, changes answering B}
//	responder -> initiator : incRound{request B, changes answering A}
//	... frames alternate until an incRound carries empty requests on both
//	sides (seen one frame apart); then:
//	responder -> initiator : incDone
//	initiator -> responder : incDone
//	responder -> initiator : incDone
//
// A side computes its request solely from the peer's immutable summary and
// keeps resending it until applying the peer's answers makes its delegate
// report no missing changes. Changes inside a frame form an ordered batch and
// are handed to the application in slice order, which is how application
// defined ordering between changes is honored.
type incSyncSession struct {
	m        *Memberlist
	conn     net.Conn
	label    string
	delegate IncrementalDelegate
	join     bool

	bytesExchanged int
	rounds         int

	// The offer frame is read by handleConn through a buffered reader that
	// may already contain subsequent frames. Those are reused for the first
	// recvFrame; afterwards frames are read fresh from the connection.
	firstReader io.Reader
	firstDec    *codec.Decoder
}

// newIncSyncSession returns nil when the local delegate does not implement
// IncrementalDelegate.
func (m *Memberlist) newIncSyncSession(conn net.Conn, label string, join bool) *incSyncSession {
	inc := asIncrementalDelegate(m.config.Delegate)
	if inc == nil {
		return nil
	}
	return &incSyncSession{m: m, conn: conn, label: label, delegate: inc, join: join}
}

// incrementalPushPull opens a fresh TCP connection and attempts an
// incremental negotiation. It returns errIncrementalUnsupported when the
// remote side cannot participate, in which case the caller retries with a
// normal full-state push/pull on a new connection.
func (m *Memberlist) incrementalPushPull(a Address, join bool) error {
	if asIncrementalDelegate(m.config.Delegate) == nil {
		return errIncrementalUnsupported
	}

	if a.Name == "" && m.config.RequireNodeNames {
		return errNodeNamesAreRequired
	}

	conn, err := m.transport.DialAddressTimeout(a, m.config.TCPTimeout)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	if err := conn.SetDeadline(time.Now().Add(m.config.TCPTimeout)); err != nil {
		m.logger.Printf("Err: Could not set the deadline: %s", err)
	}

	m.logger.Printf("[DEBUG] memberlist: Initiating incremental push/pull sync with: %s %s",
		a.Name, conn.RemoteAddr())
	metrics.IncrCounterWithLabels([]string{"memberlist", "tcp", "connect"}, 1, m.metricLabels)

	session := m.newIncSyncSession(conn, m.config.Label, join)
	return session.runInitiator()
}

// frameBytes returns the payload size represented by a frame.
func frameBytes(f *incSyncFrame) int {
	n := len(f.Summary) + len(f.Request)
	for _, c := range f.Changes {
		n += len(c)
	}
	return n
}

// charge adds wire and payload sizes to the cumulative byte budget.
func (s *incSyncSession) charge(wireSize int, frame *incSyncFrame) error {
	s.bytesExchanged += wireSize
	if payload := frameBytes(frame); payload > maxIncrementalMsgBytes {
		return fmt.Errorf("incremental sync message too large (%d > %d)",
			payload, maxIncrementalMsgBytes)
	}
	if s.bytesExchanged > maxIncrementalSyncBytes {
		return fmt.Errorf("incremental sync byte limit exceeded (%d > %d)",
			s.bytesExchanged, maxIncrementalSyncBytes)
	}
	return nil
}

// sendFrame encodes and writes one negotiation frame.
func (s *incSyncSession) sendFrame(frame *incSyncFrame) error {
	bufConn := bytes.NewBuffer(nil)
	bufConn.WriteByte(byte(incSyncMsg))

	hd := codec.MsgpackHandle{}
	enc := codec.NewEncoder(bufConn, &hd)
	if err := enc.Encode(frame); err != nil {
		return err
	}

	payload := bufConn.Bytes()
	if err := s.charge(len(payload), frame); err != nil {
		// A budget breach is a protocol failure, not an application request
		// for a full sync: do not send incFallback, which would make the
		// peer retry the full-state exchange. Closing the connection lets
		// the peer fail this round as well.
		_ = s.conn.Close()
		return err
	}
	return s.m.rawSendMsgStream(s.conn, payload, s.label)
}

// recvFrame reads the next negotiation frame.
func (s *incSyncSession) recvFrame() (*incSyncFrame, error) {
	var (
		dec     *codec.Decoder
		msgType messageType
	)
	if s.firstReader != nil && s.firstDec != nil {
		// Reuse the reader/decoder that already buffered bytes following
		// the offer frame (possible only without per-frame encryption or
		// compression). The message type was validated by handleConn.
		msgType = incSyncMsg
		dec = s.firstDec
		s.firstDec = nil
		s.firstReader = nil
	} else {
		s.firstDec = nil
		s.firstReader = nil
		var err error
		msgType, _, dec, err = s.m.readStream(s.conn, s.label)
		if err != nil {
			return nil, err
		}
	}

	if msgType == errMsg {
		var resp errResp
		if decErr := dec.Decode(&resp); decErr != nil {
			return nil, decErr
		}
		if resp.Error == incrementalUnsupportedResp {
			return nil, errIncrementalUnsupported
		}
		return nil, fmt.Errorf("remote error: %v", resp.Error)
	}

	if msgType != incSyncMsg {
		return nil, fmt.Errorf("received invalid msgType (%d), expected incSyncMsg (%d)",
			msgType, incSyncMsg)
	}

	var frame incSyncFrame
	if err := dec.Decode(&frame); err != nil {
		return nil, err
	}
	// Charge the payload size; framing overhead is bounded by the same
	// constant for received data.
	if err := s.charge(frameBytes(&frame), &frame); err != nil {
		_ = s.conn.Close()
		return nil, err
	}
	return &frame, nil
}

// expect reads a frame and verifies its op, translating incFallback.
func (s *incSyncSession) expect(op incSyncOp) (*incSyncFrame, error) {
	frame, err := s.recvFrame()
	if err != nil {
		return nil, err
	}
	if frame.Op == incFallback {
		return nil, errFallback
	}
	if frame.Op != op {
		return nil, fmt.Errorf("unexpected incremental op %d, expected %d", frame.Op, op)
	}
	return frame, nil
}

// handleServerOffer consumes an offer frame already decoded by the caller
// (via readStream) and runs the responder state machine.
func (s *incSyncSession) handleServerOffer(bufConn io.Reader, dec *codec.Decoder) error {
	var frame incSyncFrame
	if err := dec.Decode(&frame); err != nil {
		return err
	}
	if frame.Op == incFallback {
		return errFallback
	}
	if frame.Op != incOffer {
		return fmt.Errorf("unexpected incremental op %d, expected offer", frame.Op)
	}
	if err := s.charge(frameBytes(&frame), &frame); err != nil {
		return err
	}
	// Keep the offer's reader around only when it is a live connection
	// buffer that may already hold subsequent frames. Encrypted or
	// compressed offers arrive as single-frame byte buffers instead.
	if bufConn != nil {
		s.firstReader = bufConn
		s.firstDec = dec
	}
	return s.runResponderFromOffer(&frame)
}

// applyChanges hands the ordered batch to the application in slice order.
func (s *incSyncSession) applyChanges(changes [][]byte) error {
	for _, change := range changes {
		if err := s.delegate.ApplyChange(change, s.join); err != nil {
			return err
		}
	}
	return nil
}

// delegateError maps a delegate error onto the fallback protocol. Any error
// aborts the round without invoking the completion callback.
func (s *incSyncSession) delegateError(err error) error {
	if errors.Is(err, ErrIncrementalFallback) {
		_ = s.sendFrame(&incSyncFrame{Op: incFallback})
		return errFallback
	}
	return err
}

// runInitiator drives the client side of the negotiation.
func (s *incSyncSession) runInitiator() error {
	localSummary := s.delegate.IncrementalSummary(s.join)
	if err := s.sendFrame(&incSyncFrame{Op: incOffer, Summary: localSummary}); err != nil {
		return err
	}

	ack, err := s.expect(incOfferAck)
	if err != nil {
		return err
	}
	peerSummary := ack.Summary

	myRequest, err := s.delegate.MissingChanges(peerSummary, s.join)
	if err != nil {
		return s.delegateError(err)
	}

	// peerRequest is what the responder last asked us for; empty until the
	// first round frame reveals it.
	var peerRequest []byte

	for {
		s.rounds++
		if s.rounds > maxIncrementalRounds {
			return fmt.Errorf("incremental sync round limit exceeded (%d)", maxIncrementalRounds)
		}

		// Produce the ordered answer to the peer's request first.
		answer, err := s.delegate.ProduceChanges(peerRequest, s.join)
		if err != nil {
			return s.delegateError(err)
		}

		if err := s.sendFrame(&incSyncFrame{
			Op:      incRound,
			Request: myRequest,
			Changes: answer,
		}); err != nil {
			return err
		}

		reply, err := s.recvFrame()
		if err != nil {
			return err
		}

		switch reply.Op {
		case incFallback:
			return errFallback
		case incRound, incDone:
		default:
			return fmt.Errorf("unexpected incremental op %d, expected round or done", reply.Op)
		}

		if err := s.applyChanges(reply.Changes); err != nil {
			return s.delegateError(err)
		}

		peerRequest = reply.Request

		// Recompute our needs after applying the batch against the same
		// immutable peer summary.
		next, err := s.delegate.MissingChanges(peerSummary, s.join)
		if err != nil {
			return s.delegateError(err)
		}
		myRequest = next

		if reply.Op == incDone {
			// The responder observed both requests empty. Agree, then wait
			// for its final acknowledgment (sent after the responder's own
			// completion callback) before notifying our application, so the
			// initiator returning means both sides completed.
			if len(myRequest) != 0 {
				return errors.New("peer finished incremental sync while changes were still requested")
			}
			if err := s.sendFrame(&incSyncFrame{Op: incDone}); err != nil {
				return err
			}
			if _, err := s.expect(incDone); err != nil {
				return err
			}
			s.delegate.IncrementalSyncComplete(s.join)
			return nil
		}
	}
}

// runResponderFromOffer drives the server side given the already decoded
// offer frame.
func (s *incSyncSession) runResponderFromOffer(offer *incSyncFrame) error {
	peerSummary := offer.Summary
	localSummary := s.delegate.IncrementalSummary(s.join)
	if err := s.sendFrame(&incSyncFrame{Op: incOfferAck, Summary: localSummary}); err != nil {
		return err
	}

	myRequest, err := s.delegate.MissingChanges(peerSummary, s.join)
	if err != nil {
		return s.delegateError(err)
	}

	for {
		s.rounds++
		if s.rounds > maxIncrementalRounds {
			return fmt.Errorf("incremental sync round limit exceeded (%d)", maxIncrementalRounds)
		}

		in, err := s.expect(incRound)
		if err != nil {
			return err
		}

		if err := s.applyChanges(in.Changes); err != nil {
			return s.delegateError(err)
		}

		answer, err := s.delegate.ProduceChanges(in.Request, s.join)
		if err != nil {
			return s.delegateError(err)
		}

		next, err := s.delegate.MissingChanges(peerSummary, s.join)
		if err != nil {
			return s.delegateError(err)
		}
		myRequest = next

		if len(myRequest) == 0 && len(in.Request) == 0 {
			// Converged. Close with a three-message handshake so that when
			// the initiator returns, both applications have been notified:
			//
			//	responder -> initiator : incDone (converged)
			//	initiator -> responder : incDone (ack)
			//	responder -> initiator : incDone (final, after our callback)
			if err := s.sendFrame(&incSyncFrame{Op: incDone}); err != nil {
				return err
			}
			if _, err := s.expect(incDone); err != nil {
				return err
			}
			s.delegate.IncrementalSyncComplete(s.join)
			if err := s.sendFrame(&incSyncFrame{Op: incDone}); err != nil {
				return err
			}
			return nil
		}

		if err := s.sendFrame(&incSyncFrame{
			Op:      incRound,
			Request: myRequest,
			Changes: answer,
		}); err != nil {
			return err
		}
	}
}
