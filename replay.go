package milter

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// ReplayError describes the first behavioral difference found while
// replaying a recorded session.
type ReplayError struct {
	// Index is the position of the recorded event where behavior diverged.
	Index int
	// Want describes the recorded (expected) behavior.
	Want string
	// Got describes the behavior observed during replay.
	Got string
}

func (e *ReplayError) Error() string {
	return fmt.Sprintf("milter: replay diverged at event %d: want %s, got %s", e.Index, e.Want, e.Got)
}

// Replay feeds the recorded inbound commands back into the session state
// machine, in their original order, using milters created by s.NewMilter.
// Outbound traffic produced during the replay (responses and modifier
// actions) is compared against the recorded outbound events.
//
// It returns nil when the replay reproduces the recording exactly, or a
// *ReplayError describing the first difference. The recording must be
// complete: metadata-only recordings cannot be replayed (see Recorder.Full).
//
// If s.Observer is set, the replayed session is observed as well, so the
// replayed event stream can be compared with the original recording.
func (r *Recorder) Replay(s *Server) error {
	if !r.Full {
		return fmt.Errorf("milter: replay requires a full recording (Recorder.Full)")
	}
	return ReplayEvents(r.Events(), s)
}

// ReplayEvents is the core replay helper used by Recorder.Replay. It replays
// a complete, ordered event list against the session state machine driven by
// s and reports the first behavioral difference.
func ReplayEvents(events []Event, s *Server) error {
	conn := &replayConn{}
	session := &milterSession{
		server:   s,
		actions:  s.Actions,
		protocol: s.Protocol,
		conn:     conn,
		backend:  s.NewMilter(),
	}

	var pending []Event // produced outbound events not yet matched
	drain := func() {
		pending = append(pending, conn.drain()...)
	}

	for i, ev := range events {
		if ev.Direction == DirectionOutbound {
			drain()
			if len(pending) == 0 {
				return &ReplayError{Index: i, Want: describeEvent(ev), Got: "no output"}
			}
			got := pending[0]
			pending = pending[1:]
			if !eventsMatch(ev, got) {
				return &ReplayError{Index: i, Want: describeEvent(ev), Got: describeEvent(got)}
			}
			continue
		}

		msg := &Message{Code: ev.Code, Data: append([]byte(nil), ev.Data...)}
		resp, err := session.Process(msg)
		if err != nil {
			if err == errCloseSession {
				if i == len(events)-1 {
					return nil
				}
				return &ReplayError{Index: i, Want: "session to continue", Got: "session closed"}
			}
			return &ReplayError{Index: i, Want: "command to succeed", Got: fmt.Sprintf("error: %v", err)}
		}
		if resp != nil {
			if err := session.WritePacket(resp.Response()); err != nil {
				return &ReplayError{Index: i, Want: "response to be written", Got: fmt.Sprintf("error: %v", err)}
			}
			if !resp.Continue() {
				// prepare backend for next message
				session.backend = s.NewMilter()
			}
		}
	}

	drain()
	if len(pending) != 0 {
		return &ReplayError{Index: len(events), Want: "no more output", Got: describeEvent(pending[0])}
	}
	return nil
}

func eventsMatch(want, got Event) bool {
	return want.Direction == got.Direction &&
		want.Code == got.Code &&
		want.Length == got.Length &&
		want.Final == got.Final &&
		bytes.Equal(want.Data, got.Data)
}

func describeEvent(e Event) string {
	data := e.Data
	if len(data) > 32 {
		data = append(data[:32], '.', '.', '.')
	}
	return fmt.Sprintf("%s %q (len=%d, final=%t, data=%q)", e.Direction, rune(e.Code), e.Length, e.Final, data)
}

// replayConn is a net.Conn that captures written packets for comparison.
type replayConn struct {
	buf bytes.Buffer
	off int
}

func (c *replayConn) Read(b []byte) (int, error)         { return 0, io.EOF }
func (c *replayConn) Write(b []byte) (int, error)        { return c.buf.Write(b) }
func (c *replayConn) Close() error                       { return nil }
func (c *replayConn) LocalAddr() net.Addr                { return nil }
func (c *replayConn) RemoteAddr() net.Addr               { return nil }
func (c *replayConn) SetDeadline(t time.Time) error      { return nil }
func (c *replayConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *replayConn) SetWriteDeadline(t time.Time) error { return nil }

// drain parses all complete packets written since the last call and returns
// them as outbound events.
func (c *replayConn) drain() []Event {
	var events []Event
	buf := c.buf.Bytes()
	for len(buf)-c.off >= 5 {
		length := binary.BigEndian.Uint32(buf[c.off:])
		if uint32(len(buf)-c.off) < 4+length {
			break
		}
		data := buf[c.off+4 : c.off+4+int(length)]
		msg := &Message{Code: data[0], Data: data[1:]}
		events = append(events, newOutboundEvent(msg))
		c.off += 4 + int(length)
	}
	return events
}
