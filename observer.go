package milter

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Direction identifies the direction of an observed protocol packet.
type Direction int

const (
	// DirectionInbound is a packet sent by the MTA to the milter.
	DirectionInbound Direction = iota
	// DirectionOutbound is a packet sent by the milter to the MTA.
	DirectionOutbound
)

func (d Direction) String() string {
	if d == DirectionInbound {
		return "inbound"
	}
	return "outbound"
}

// ObservedEvent describes a single milter protocol packet observed on a
// session.
type ObservedEvent struct {
	// Direction of the packet.
	Direction Direction
	// Code is the milter command or response code.
	Code byte
	// Length is the packet payload length in bytes, including the command
	// byte.
	Length uint32

	// Actions and Protocol are the negotiated capabilities. They are only
	// set on option negotiation (CodeOptNeg) events.
	Actions  OptAction
	Protocol OptProtocol

	// Final reports whether an outbound response is a terminal response
	// (accept, discard, reject or tempfail) that ends processing of the
	// current message. It is always false for inbound packets.
	Final bool

	// Data is the packet payload without the command byte. It is only
	// populated if the Observer opted into payload capture by returning
	// true from CapturePayload. It may contain message bodies, addresses
	// and macro values. The slice is a private copy made for the observer
	// and may be retained.
	Data []byte
}

// Observer receives milter protocol events.
//
// ObserveEvent is called synchronously from the goroutine serving the
// session, after the packet has been read from or written to the network.
// The server never holds any lock while calling the observer, so a slow
// observer only stalls its own session and never blocks the whole service.
// Implementations must be safe for concurrent use and should return
// quickly. Events are purely informational: the observer cannot alter the
// bytes read from or written to the network.
type Observer interface {
	// ObserveEvent is called for every protocol packet. Returning an error
	// (or panicking) is an observer failure and is handled according to
	// Server.ObserverFailurePolicy.
	ObserveEvent(ev ObservedEvent) error
	// CapturePayload reports whether packet payloads should be included in
	// ObservedEvent.Data. Returning false (the privacy-preserving default)
	// records only metadata: direction, command, length, negotiated
	// capabilities and response final state.
	CapturePayload() bool
}

// ObserverFailurePolicy determines how Observer failures are handled.
type ObserverFailurePolicy int

const (
	// ObserverFailureLog logs the observer failure and continues serving
	// the session. This is the default policy.
	ObserverFailureLog ObserverFailurePolicy = iota
	// ObserverFailureClose logs the observer failure and closes the
	// affected session. Other sessions are not affected.
	ObserverFailureClose
)

// Recorder is an Observer that keeps all observed events in memory, in
// their original order. It never writes anything to disk.
//
// By default only packet metadata is recorded. Pass capturePayload=true to
// NewRecorder to additionally keep full packet payloads, e.g. to replay the
// session later with Replay.
type Recorder struct {
	mu             sync.Mutex
	capturePayload bool
	events         []ObservedEvent
}

// NewRecorder creates an in-memory Recorder. If capturePayload is false,
// packet payloads (message bodies, addresses, macro values) are not stored.
func NewRecorder(capturePayload bool) *Recorder {
	return &Recorder{capturePayload: capturePayload}
}

// CapturePayload implements Observer.
func (r *Recorder) CapturePayload() bool {
	return r.capturePayload
}

// ObserveEvent implements Observer.
func (r *Recorder) ObserveEvent(ev ObservedEvent) error {
	if !r.capturePayload {
		ev.Data = nil
	}
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return nil
}

// Events returns a copy of all events recorded so far, in order.
func (r *Recorder) Events() []ObservedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ObservedEvent(nil), r.events...)
}

// ReplayDiff describes the first behavioral difference found while
// replaying a recorded session.
type ReplayDiff struct {
	// Index is the position of the diverging event in the recording.
	Index int
	// Reason describes the difference.
	Reason string
	// Expected is the outbound packet found in the recording, if any.
	Expected *Message
	// Actual is the outbound packet produced by the replay, if any.
	Actual *Message
}

func (d *ReplayDiff) String() string {
	return fmt.Sprintf("milter: replay diverged at event %d: %s (expected %v, got %v)",
		d.Index, d.Reason, d.Expected, d.Actual)
}

// Replay feeds the inbound packets of a recorded session back into the
// milter session state machine, in their original order, and verifies that
// the produced outbound packets match the recording. The session is driven
// by s, which must be configured like the server that produced the
// recording; s.NewMilter is used to create backends exactly as in Serve.
// s.Observer is not notified during replay.
//
// Replay requires a recording with captured payloads (see NewRecorder). It
// returns nil if the replay reproduces the recording exactly, otherwise a
// ReplayDiff describing the first behavioral difference.
func Replay(events []ObservedEvent, s *Server) *ReplayDiff {
	conn := new(replayConn)
	session := &milterSession{
		server:   s,
		actions:  s.Actions,
		protocol: s.Protocol,
		conn:     conn,
		backend:  s.NewMilter(),
	}

	for i := 0; i < len(events); {
		ev := events[i]
		if ev.Direction != DirectionInbound {
			return &ReplayDiff{
				Index:    i,
				Reason:   "outbound packet without preceding command",
				Expected: &Message{Code: ev.Code, Data: ev.Data},
			}
		}
		if ev.Data == nil {
			return &ReplayDiff{
				Index:  i,
				Reason: "packet payload not captured, replay requires a full recording",
			}
		}

		resp, err := session.Process(&Message{Code: ev.Code, Data: ev.Data})
		outputs := conn.drain()
		if err != nil {
			if err == errCloseSession {
				if i == len(events)-1 {
					return nil
				}
				return &ReplayDiff{Index: i, Reason: "session closed before the end of the recording"}
			}
			return &ReplayDiff{Index: i, Reason: fmt.Sprintf("command failed: %v", err)}
		}
		if resp != nil {
			outputs = append(outputs, resp.Response())
		}

		j := i + 1
		k := 0
		for j < len(events) && events[j].Direction == DirectionOutbound {
			recorded := &events[j]
			if k >= len(outputs) {
				return &ReplayDiff{
					Index:    j,
					Reason:   "recorded output packet was not reproduced",
					Expected: &Message{Code: recorded.Code, Data: recorded.Data},
				}
			}
			if outputs[k].Code != recorded.Code || !bytes.Equal(outputs[k].Data, recorded.Data) {
				return &ReplayDiff{
					Index:    j,
					Reason:   "output packet differs from recording",
					Expected: &Message{Code: recorded.Code, Data: recorded.Data},
					Actual:   outputs[k],
				}
			}
			j++
			k++
		}
		if k < len(outputs) {
			return &ReplayDiff{
				Index:  i,
				Reason: "replay produced an output packet that is not in the recording",
				Actual: outputs[k],
			}
		}

		if resp != nil && !resp.Continue() {
			session.backend = s.NewMilter()
		}
		i = j
	}
	return nil
}

// replayConn is a net.Conn that buffers writes so that packets written by a
// replayed session can be inspected. Reads always return io.EOF.
type replayConn struct {
	buf bytes.Buffer
}

func (c *replayConn) Read(b []byte) (int, error)  { return 0, io.EOF }
func (c *replayConn) Write(b []byte) (int, error) { return c.buf.Write(b) }
func (c *replayConn) Close() error                { return nil }

func (c *replayConn) LocalAddr() net.Addr                { return nil }
func (c *replayConn) RemoteAddr() net.Addr               { return nil }
func (c *replayConn) SetDeadline(t time.Time) error      { return nil }
func (c *replayConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *replayConn) SetWriteDeadline(t time.Time) error { return nil }

// drain removes all buffered writes and parses them into packets.
func (c *replayConn) drain() []*Message {
	var msgs []*Message
	for c.buf.Len() > 0 {
		var length uint32
		if err := binary.Read(&c.buf, binary.BigEndian, &length); err != nil {
			return msgs
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(&c.buf, data); err != nil {
			return msgs
		}
		msgs = append(msgs, &Message{Code: data[0], Data: data[1:]})
	}
	return msgs
}
