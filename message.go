// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
)

// MessageChannel is an optional session-scoped channel for short,
// independent messages, such as real-time location updates where stale
// data may be discarded. It shares the underlying connection with normal
// streams, but unlike streams it preserves message boundaries: each call
// to SendMessage is delivered as exactly one message to ReceiveMessage,
// never split and never merged.
//
// The transport underneath remains the reliable session connection. The
// only place messages are dropped is the local receive queue: if the
// application does not read fast enough and the bounded queue fills up,
// the oldest queued message is dropped to make room for newer ones, and
// the drop is counted in Dropped. The peer is never asked to retransmit.
//
// The channel must be enabled in the Config of both sides. Support and
// the maximum message size are negotiated when the session starts; use
// Ready or WaitReady to learn when negotiation has completed.
type MessageChannel struct {
	session *Session

	// recvMax is the maximum message size we accept, announced to the peer
	recvMax uint32

	// sendMax is the negotiated maximum size for outgoing messages.
	// It is only valid once the ready channel is closed.
	sendMax atomic.Uint32

	// recvCh is the bounded queue of received messages
	recvCh chan []byte

	// dropped counts incoming messages dropped locally because recvCh
	// was full when they arrived
	dropped atomic.Uint64

	// ready is closed once negotiation with the peer has completed
	ready     chan struct{}
	readyOnce sync.Once

	// ackSent tracks whether we have answered the peer's SYN
	ackSent int32
}

// newMessageChannel constructs the message channel for a session
func newMessageChannel(s *Session, config *Config) *MessageChannel {
	return &MessageChannel{
		session: s,
		recvMax: config.MaxMessageSize,
		recvCh:  make(chan []byte, config.MessageChannelQueueSize),
		ready:   make(chan struct{}),
	}
}

// MessageChannel returns the session's short-message channel, or
// ErrMessageChannelDisabled if it was not enabled in the Config.
func (s *Session) MessageChannel() (*MessageChannel, error) {
	if s.msgCh == nil {
		return nil, ErrMessageChannelDisabled
	}
	return s.msgCh, nil
}

// Ready returns a channel that is closed once capability negotiation
// with the peer has completed and messages may be sent.
func (m *MessageChannel) Ready() <-chan struct{} {
	return m.ready
}

// WaitReady blocks until negotiation has completed, the context is
// cancelled, or the session is closed.
func (m *MessageChannel) WaitReady(ctx context.Context) error {
	select {
	case <-m.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.session.shutdownCh:
		return m.session.shutdownErr
	}
}

// MaxMessageSize returns the negotiated maximum size of a single
// outgoing message. It is only valid once Ready is closed.
func (m *MessageChannel) MaxMessageSize() uint32 {
	return m.sendMax.Load()
}

// Dropped returns the number of incoming messages that were dropped
// locally because the receive queue was full when they arrived.
func (m *MessageChannel) Dropped() uint64 {
	return m.dropped.Load()
}

// SendMessage sends a single complete message. It blocks until
// negotiation has completed and returns ErrMessageTooLarge if the
// message exceeds the negotiated maximum message size.
func (m *MessageChannel) SendMessage(msg []byte) error {
	select {
	case <-m.ready:
	case <-m.session.shutdownCh:
		return ErrSessionShutdown
	}
	if uint32(len(msg)) > m.sendMax.Load() {
		return ErrMessageTooLarge
	}
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeMessage, 0, 0, uint32(len(msg)))
	return m.session.waitForSend(hdr, msg)
}

// ReceiveMessage returns the next complete message. A message is always
// delivered in full or not at all; it is never split across calls.
func (m *MessageChannel) ReceiveMessage() ([]byte, error) {
	select {
	case msg := <-m.recvCh:
		return msg, nil
	case <-m.session.shutdownCh:
		return nil, m.session.shutdownErr
	}
}

// sync announces support for the message channel to the peer. The
// length field carries the maximum message size we are willing to
// receive.
func (m *MessageChannel) sync() {
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeMessage, flagSYN, 0, m.recvMax)
	_ = m.session.waitForSend(hdr, nil)
}

// handleSync processes a SYN or ACK negotiation frame from the peer.
func (m *MessageChannel) handleSync(hdr header) error {
	peerMax := hdr.Length()
	if peerMax == 0 || peerMax > maxMessageSizeLimit {
		m.session.logger.Printf("[ERR] yamux: invalid max message size announced: %d", peerMax)
		return ErrInvalidMsgType
	}

	// Answer a SYN with an ACK exactly once
	if hdr.Flags()&flagSYN == flagSYN && atomic.CompareAndSwapInt32(&m.ackSent, 0, 1) {
		ack := header(make([]byte, headerSize))
		ack.encode(typeMessage, flagACK, 0, m.recvMax)
		if err := m.session.sendNoWait(ack); err != nil {
			return err
		}
	}

	m.sendMax.Store(min(peerMax, m.recvMax))
	m.readyOnce.Do(func() { close(m.ready) })
	return nil
}

// handleMessage reads one complete message frame and queues it for the
// application. The payload is always drained from the wire in full so
// that framing stays intact and subsequent control frames keep flowing,
// even when the message itself ends up dropped.
func (m *MessageChannel) handleMessage(hdr header, conn io.Reader) error {
	length := hdr.Length()
	if length > m.recvMax {
		m.session.logger.Printf("[ERR] yamux: message exceeds maximum size (max: %d, recv: %d)", m.recvMax, length)
		return ErrMessageTooLarge
	}

	var buf []byte
	if length > 0 {
		buf = make([]byte, length)
		if _, err := io.ReadFull(conn, buf); err != nil {
			m.session.logger.Printf("[ERR] yamux: failed to read message payload: %v", err)
			return err
		}
	}

	m.enqueue(buf)
	return nil
}

// enqueue adds a message to the receive queue. If the queue is full,
// the oldest queued message is dropped locally to make room for the
// newer one, and the drop is recorded. Only called from the recvLoop
// goroutine.
func (m *MessageChannel) enqueue(msg []byte) {
	select {
	case m.recvCh <- msg:
		return
	default:
	}

	select {
	case <-m.recvCh:
		m.dropped.Add(1)
	default:
	}
	m.recvCh <- msg
}

// handleMsgData is invoked for a typeMessage frame
func (s *Session) handleMsgData(hdr header) error {
	mc := s.msgCh
	if mc == nil {
		// The peer is using the extension although we never announced
		// support for it.
		s.logger.Printf("[ERR] yamux: message frame received but message channel is disabled")
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return ErrInvalidMsgType
	}

	if hdr.StreamID() != 0 {
		s.logger.Printf("[ERR] yamux: message frame with non-zero stream id: %d", hdr.StreamID())
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return ErrInvalidMsgType
	}

	flags := hdr.Flags()
	var err error
	switch {
	case flags&(flagSYN|flagACK) != 0:
		err = mc.handleSync(hdr)
	case flags != 0:
		// FIN and RST are not defined for the message channel
		s.logger.Printf("[ERR] yamux: unexpected flags on message frame: %d", flags)
		err = ErrUnexpectedFlag
	default:
		err = mc.handleMessage(hdr, s.bufRead)
	}
	if err != nil {
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
	}
	return err
}
