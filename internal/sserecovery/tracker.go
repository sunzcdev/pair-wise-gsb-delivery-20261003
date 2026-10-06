package sserecovery

import (
	"sort"
	"sync"
)

// Tracker records, per channel, the greatest stream position fully delivered
// to an SSE connection. Positions only advance when the handler has both
// written and flushed a complete event and received its write acknowledgement:
// an event cut off mid-body never moves the position, so a reconnect resumes
// from the last fully delivered offset and redelivers the partial one.
//
// Methods are safe for concurrent use; the SSE handler is the sole writer and
// the reconnect logic only reads a snapshot to mint a token.
type Tracker struct {
	mu sync.Mutex
	// channels maps channel name to its tracked entry. Numeric channel ids used
	// on the wire are translated to names before the tracker is touched.
	channels map[string]trackedChannel
}

type trackedChannel struct {
	epoch string
	// offset is the last delivered offset. known=false means a recoverable
	// subscription exists but nothing has been delivered yet (fresh subscribe,
	// or recovery failed because history expired).
	offset uint64
	known  bool
}

// NewTracker creates an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{channels: make(map[string]trackedChannel)}
}

// Seed records a subscription immediately after its subscribe/connect result
// has been delivered. recovered is the result's recovered flag and offset its
// reported stream offset:
//
//   - recovered=true:  every publication up to offset is already accounted for,
//     so offset is the delivered position even if no separate pub frames follow.
//   - recovered=false: history could not bridge the gap; the client must restart
//     that channel from offset on reconnect, which is communicated by leaving
//     the entry unknown until a complete publication lands.
func (t *Tracker) Seed(channel, epoch string, offset uint64, recovered bool) {
	if channel == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := t.channels[channel]
	ch.epoch = epoch
	if recovered {
		ch.offset = offset
		ch.known = true
	} else {
		ch.offset = 0
		ch.known = false
	}
	t.channels[channel] = ch
}

// Advance marks offset (with epoch) delivered after a complete, acknowledged
// write. Positions are monotonic within an epoch.
func (t *Tracker) Advance(channel, epoch string, offset uint64) {
	if channel == "" || offset == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := t.channels[channel]
	if ch.known && ch.epoch != "" && epoch != "" && ch.epoch != epoch {
		// A new epoch invalidates ordering; take the new position.
		ch.known = false
	}
	if !ch.known || offset >= ch.offset {
		ch.offset = offset
		ch.known = true
		if epoch != "" {
			ch.epoch = epoch
		}
	}
	t.channels[channel] = ch
}

// Snapshot returns a stable copy of the tracked positions, in channel order.
// Channels with no delivered position are intentionally omitted: reconnecting
// with no offset for such a channel is exactly what makes the broker report
// partial rather than full recovery.
func (t *Tracker) Snapshot() []Channel {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Channel, 0, len(t.channels))
	for name, ch := range t.channels {
		if !ch.known {
			continue
		}
		out = append(out, Channel{Channel: name, Epoch: ch.epoch, Offset: ch.offset})
	}
	sortChannels(out)
	return out
}

// SnapshotWith returns the sorted positions that would result if extra offsets
// (channel/epoch/offset triples) were applied, without mutating the tracker.
// It renders the cursor for a batch that has been written but whose delivery
// is not yet confirmed.
func (t *Tracker) SnapshotWith(extra []Channel) []Channel {
	t.mu.Lock()
	defer t.mu.Unlock()
	projected := make(map[string]trackedChannel, len(t.channels)+len(extra))
	for name, ch := range t.channels {
		projected[name] = ch
	}
	for _, e := range extra {
		if e.Channel == "" || e.Offset == 0 {
			continue
		}
		ch := projected[e.Channel]
		if ch.epoch != "" && e.Epoch != "" && ch.epoch != e.Epoch {
			ch.known = false
		}
		if !ch.known || e.Offset >= ch.offset {
			ch.offset = e.Offset
			ch.known = true
			if e.Epoch != "" {
				ch.epoch = e.Epoch
			}
		}
		projected[e.Channel] = ch
	}
	out := make([]Channel, 0, len(projected))
	for name, ch := range projected {
		if !ch.known {
			continue
		}
		out = append(out, Channel{Channel: name, Epoch: ch.epoch, Offset: ch.offset})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out
}

// KnownChannels returns every seeded channel name, sorted, including channels
// without a delivered position.
func (t *Tracker) KnownChannels() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.channels))
	for name := range t.channels {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortChannels(c []Channel) {
	sort.Slice(c, func(i, j int) bool { return c[i].Channel < c[j].Channel })
}
