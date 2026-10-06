package centrifuge

import (
	"net/http"

	"github.com/centrifugal/centrifuge/internal/sserecovery"

	"github.com/centrifugal/protocol"
	"github.com/segmentio/encoding/json"
)

// cursorFromRequest extracts a recovery cursor presented on reconnect. The
// Last-Event-ID header is the browser-native channel (EventSource populates it
// atomically from the last fully received id field); the query param and custom
// header cover non-browser clients and test harnesses.
func cursorFromRequest(r *http.Request) string {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return v
	}
	if v := r.Header.Get(sseRecoveryHeader); v != "" {
		return v
	}
	return r.URL.Query().Get(sseRecoveryQueryParam)
}

// applyRecoveryCursor rewrites the connect command's server-side subscription
// positions from a verified cursor. It runs before the command frame is
// dispatched, so every channel still flows through the existing OnConnecting
// permission checks: the cursor only supplies a resume position, never access.
//
// Positions are applied per channel. The signed payload binds the user and the
// exact channel set, so tampering with either invalidates the signature; the
// server independently decides which of those channels it actually subscribes.
func applyRecoveryCursor(requestData []byte, cur sserecovery.Cursor) []byte {
	cmds, _ := decodeJSONCommands(requestData)
	if len(cmds) == 0 || cmds[0].Connect == nil {
		return requestData
	}

	connect := cmds[0].Connect
	if len(connect.Subs) == 0 {
		return requestData
	}
	// Apply the cursor position per channel. Integrity (the exact signed set)
	// is guaranteed by the signature; authorization is enforced independently by
	// the server's OnConnecting subscriptions, which only materialize channels
	// the user may hold. A channel present in the cursor but absent from the
	// server's grant simply never gets a subscription, so the cursor cannot
	// authorize it.
	for ch, sub := range connect.Subs {
		pos, ok := cur.Position(ch)
		if !ok {
			continue
		}
		sub.Recover = true
		sub.Epoch = pos.Epoch
		sub.Offset = pos.Offset
	}

	out, err := encodeJSONCommands(cmds)
	if err != nil {
		return requestData
	}
	return out
}

func decodeJSONCommands(data []byte) ([]*protocol.Command, bool) {
	var cmds []*protocol.Command
	rest := data
	for len(rest) > 0 {
		var line []byte
		if idx := indexNewline(rest); idx >= 0 {
			line = rest[:idx]
			rest = rest[idx+1:]
		} else {
			line = rest
			rest = nil
		}
		if len(line) == 0 {
			continue
		}
		cmd := &protocol.Command{}
		if err := json.Unmarshal(line, cmd); err != nil {
			return nil, false
		}
		cmds = append(cmds, cmd)
	}
	return cmds, true
}

func encodeJSONCommands(cmds []*protocol.Command) ([]byte, error) {
	var out []byte
	for i, cmd := range cmds {
		b, err := json.Marshal(cmd)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, b...)
	}
	return out, nil
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// frameScan accumulates the per-channel delivery information visible in the
// encoded protocol frames written down the SSE stream. It maps the numeric
// channel ids used in compact pushes back to channel names, seeds positions
// from the connect reply's per-channel recovery verdicts, and stages the
// offsets carried by complete publication frames.
type frameScan struct {
	tracker  *sserecovery.Tracker
	idToName map[int64]string
	// staged holds publication offsets observed in the current write batch, in
	// frame order (frames without a tracked publication contribute nothing).
	// They are committed to the tracker only after the batch is fully written,
	// flushed and acknowledged.
	staged []stagedAdvance
	// offsetsByFrame[i] is the count of staged offsets contributed by frames
	// [0..i], used to render a per-event cumulative cursor.
	offsetsByFrame []int
}

type stagedAdvance struct {
	channel string
	epoch   string
	offset  uint64
}

func newFrameScan(tracker *sserecovery.Tracker) *frameScan {
	return &frameScan{tracker: tracker, idToName: make(map[int64]string)}
}

// scanFrame inspects one encoded JSON reply. It never fails the stream: cursor
// metadata is best-effort and the frame is already on its way to the client.
func (s *frameScan) scanFrame(frame []byte) {
	before := len(s.staged)
	var reply protocol.Reply
	if err := json.Unmarshal(frame, &reply); err != nil {
		s.offsetsByFrame = append(s.offsetsByFrame, before)
		return
	}

	// Connect reply carries one recovery verdict per server-side subscription.
	if reply.Connect != nil {
		for name, res := range reply.Connect.Subs {
			if !res.GetRecoverable() {
				continue
			}
			s.tracker.Seed(name, res.GetEpoch(), res.GetOffset(), res.GetRecovered())
			// Server-side subscriptions deliver recovered publications inline in
			// the connect reply (not as separate pushes). Stage their offsets so
			// a fully delivered connect frame advances their positions too.
			for _, pub := range res.Publications {
				if pub.GetOffset() != 0 {
					s.staged = append(s.staged, stagedAdvance{
						channel: name,
						epoch:   pub.GetEpoch(),
						offset:  pub.GetOffset(),
					})
				}
			}
			if res.GetId() != 0 {
				s.idToName[res.GetId()] = name
			}
		}
		s.offsetsByFrame = append(s.offsetsByFrame, len(s.staged))
		return
	}

	push := reply.Push
	if push == nil || push.Pub == nil {
		s.offsetsByFrame = append(s.offsetsByFrame, before)
		return
	}
	name := push.Channel
	if name == "" && push.Id != 0 {
		name = s.idToName[push.Id]
	}
	if name == "" {
		s.offsetsByFrame = append(s.offsetsByFrame, before)
		return
	}
	s.staged = append(s.staged, stagedAdvance{
		channel: name,
		epoch:   push.Pub.GetEpoch(),
		offset:  push.Pub.GetOffset(),
	})
	s.offsetsByFrame = append(s.offsetsByFrame, len(s.staged))
}

// commitAdvances applies staged offsets after a fully delivered batch.
func (s *frameScan) commitAdvances() {
	for _, a := range s.staged {
		s.tracker.Advance(a.channel, a.epoch, a.offset)
	}
	s.resetBatch()
}

// discardAdvances drops staged offsets when a batch did not fully deliver.
func (s *frameScan) discardAdvances() {
	s.resetBatch()
}

func (s *frameScan) resetBatch() {
	s.staged = s.staged[:0]
	s.offsetsByFrame = s.offsetsByFrame[:0]
}

// channelsUpTo returns the staged positions attributable to frames [0..frameIdx]
// inclusive, for rendering that frame's cumulative cursor.
func (s *frameScan) channelsUpTo(frameIdx int) []sserecovery.Channel {
	if frameIdx < 0 || frameIdx >= len(s.offsetsByFrame) {
		return nil
	}
	n := s.offsetsByFrame[frameIdx]
	out := make([]sserecovery.Channel, 0, n)
	for i := 0; i < n; i++ {
		a := s.staged[i]
		out = append(out, sserecovery.Channel{Channel: a.channel, Epoch: a.epoch, Offset: a.offset})
	}
	return out
}
