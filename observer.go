package milter

import (
	"encoding/binary"
	"fmt"
	"log"
	"sync"
)

// Direction describes the direction of an observed protocol packet.
type Direction byte

const (
	// DirectionInbound is a command packet sent by the MTA to the milter.
	DirectionInbound Direction = '>'
	// DirectionOutbound is a packet sent by the milter to the MTA, either a
	// response or a modifier action.
	DirectionOutbound Direction = '<'
)

func (d Direction) String() string {
	switch d {
	case DirectionInbound:
		return "inbound"
	case DirectionOutbound:
		return "outbound"
	default:
		return fmt.Sprintf("unknown(%d)", byte(d))
	}
}

// Event describes a single observed protocol packet.
//
// Data holds a private copy of the packet payload (without the command
// code): observers can inspect it but cannot alter the bytes read from or
// written to the wire.
type Event struct {
	Direction Direction
	Code      byte
	// Length is the packet length as transmitted on the wire (command code
	// byte plus payload).
	Length uint32
	Data   []byte

	// Negotiated capabilities, set for SMFIC_OPTNEG packets only.
	Version  uint32
	Actions  OptAction
	Protocol OptProtocol

	// Final reports whether an outbound packet is a terminal response
	// (accept, discard, reject or tempfail).
	Final bool
}

// Observer receives protocol events from milter sessions.
//
// Observers are called from the goroutine serving each session, without any
// server-wide lock held: a slow observer only slows down its own session,
// never the whole service. Observer implementations shared between sessions
// must do their own synchronization.
//
// Observer failures never affect session processing. Errors returned by
// Observe (and panics, which are recovered) are handed to
// Server.ObserverErrorHandler, or logged when it is nil.
type Observer interface {
	Observe(Event) error
}

// ObserverFunc adapts a function to the Observer interface.
type ObserverFunc func(Event) error

// Observe implements Observer.
func (f ObserverFunc) Observe(e Event) error { return f(e) }

// Recorder is an Observer that keeps events in memory for later inspection
// or replay. It never writes to files.
//
// By default only metadata is kept: direction, command code, packet length,
// negotiated capabilities and response final state. Payload data (message
// bodies, addresses, macro values) is dropped unless Full is set.
type Recorder struct {
	// Full selects complete in-memory recording, including payload data.
	// It is intended for tests and replay and must be opted into explicitly.
	Full bool

	mu     sync.Mutex
	events []Event
}

// Observe implements Observer.
func (r *Recorder) Observe(e Event) error {
	if r.Full {
		e.Data = append([]byte(nil), e.Data...)
	} else {
		e.Data = nil
	}
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return nil
}

// Events returns a copy of the recorded events, in their original order.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// observe reports an event to the server observer, if any. Observer failures
// are contained here and never reach the session state machine.
func (m *milterSession) observe(e Event) {
	obs := m.server.Observer
	if obs == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			m.handleObserverError(fmt.Errorf("milter: observer panic: %v", r))
		}
	}()
	if err := obs.Observe(e); err != nil {
		m.handleObserverError(err)
	}
}

func (m *milterSession) handleObserverError(err error) {
	if h := m.server.ObserverErrorHandler; h != nil {
		h(err)
		return
	}
	log.Printf("milter: observer error: %v", err)
}

func newInboundEvent(msg *Message) Event {
	e := Event{
		Direction: DirectionInbound,
		Code:      msg.Code,
		Length:    uint32(len(msg.Data)) + 1,
		Data:      append([]byte(nil), msg.Data...),
	}
	if Code(msg.Code) == CodeOptNeg {
		e.Version, e.Actions, e.Protocol = parseOptNeg(msg.Data)
	}
	return e
}

func newOutboundEvent(msg *Message) Event {
	e := Event{
		Direction: DirectionOutbound,
		Code:      msg.Code,
		Length:    uint32(len(msg.Data)) + 1,
		Data:      append([]byte(nil), msg.Data...),
		Final:     isFinalResponse(msg.Code),
	}
	if Code(msg.Code) == CodeOptNeg {
		e.Version, e.Actions, e.Protocol = parseOptNeg(msg.Data)
	}
	return e
}

func parseOptNeg(data []byte) (version uint32, actions OptAction, protocol OptProtocol) {
	if len(data) >= 12 {
		version = binary.BigEndian.Uint32(data)
		actions = OptAction(binary.BigEndian.Uint32(data[4:]))
		protocol = OptProtocol(binary.BigEndian.Uint32(data[8:]))
	}
	return version, actions, protocol
}

// isFinalResponse reports whether code is a terminal response code, matching
// the Continue semantics of Response implementations.
func isFinalResponse(code byte) bool {
	switch ActionCode(code) {
	case ActAccept, ActDiscard, ActReject, ActTempFail:
		return true
	}
	return false
}
