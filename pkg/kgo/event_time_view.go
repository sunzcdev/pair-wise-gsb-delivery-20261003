// This file implements an optional event-time read view over consumed
// records. Downstream consumers that want to process multiple partitions
// roughly in event-time order can use EventTimeView instead of maintaining
// their own per-partition buffers and heaps.

package kgo

import (
	"container/heap"
	"sort"
	"time"
)

// EventTimeViewOpt is an option to configure an EventTimeView.
type EventTimeViewOpt interface {
	apply(*eventTimeViewCfg)
}

type eventTimeViewOpt struct{ fn func(*eventTimeViewCfg) }

func (opt eventTimeViewOpt) apply(cfg *eventTimeViewCfg) { opt.fn(cfg) }

type eventTimeViewCfg struct {
	allowedLateness time.Duration
	idleTimeout     time.Duration
	maxBuffered     int
	now             func() time.Time
	extract         func(*Record) time.Time
}

// EventTimeAllowedLateness sets how far behind a partition's maximum seen
// event time a record may be before it is considered late. A record whose
// event time is strictly before (partition max seen - allowed lateness) is
// diverted to EventTimeOutput.Late rather than merged into the normal
// output. The default is 0, meaning any record older than the partition's
// maximum seen event time is late.
func EventTimeAllowedLateness(d time.Duration) EventTimeViewOpt {
	return eventTimeViewOpt{func(cfg *eventTimeViewCfg) { cfg.allowedLateness = d }}
}

// EventTimeIdleTimeout sets how long a partition may go without delivering
// a record (in wall-clock time, per the EventTimeNow clock) before it is
// considered idle. Idle partitions are excluded from the merged watermark
// so that a silent partition does not stall output forever. An idle
// partition becomes active again when it delivers a record, resuming from
// its previous watermark. The default of 0 disables idle detection,
// meaning a partition that goes silent after delivering at least one
// record stalls the view indefinitely.
func EventTimeIdleTimeout(d time.Duration) EventTimeViewOpt {
	return eventTimeViewOpt{func(cfg *eventTimeViewCfg) { cfg.idleTimeout = d }}
}

// EventTimeMaxBuffered bounds how many records the view buffers while
// waiting for the watermark to advance. If a Poll would leave more than n
// records buffered, the records with the smallest event times are emitted
// early (ahead of the watermark) until the buffer is at the limit, and
// EventTimeOutput.Forced reports how many records were emitted this way.
// The default of 0 means unlimited buffering.
func EventTimeMaxBuffered(n int) EventTimeViewOpt {
	return eventTimeViewOpt{func(cfg *eventTimeViewCfg) { cfg.maxBuffered = n }}
}

// EventTimeNow overrides the wall clock used for idle-partition detection.
// This exists mainly for tests; the default is time.Now.
func EventTimeNow(fn func() time.Time) EventTimeViewOpt {
	return eventTimeViewOpt{func(cfg *eventTimeViewCfg) { cfg.now = fn }}
}

// EventTimeExtractTime overrides how the event time of a record is
// extracted. The default uses Record.Timestamp.
func EventTimeExtractTime(fn func(*Record) time.Time) EventTimeViewOpt {
	return eventTimeViewOpt{func(cfg *eventTimeViewCfg) { cfg.extract = fn }}
}

// EventTimeOutput is the result of one EventTimeView.Poll.
type EventTimeOutput struct {
	// Records are records whose event time is at or under the merged
	// watermark, ordered by event time (ties broken by topic, partition,
	// offset), followed by any records flushed from revoked partitions
	// and any records force-emitted by the max-buffered limit.
	//
	// This is NOT a global ordering guarantee: Kafka itself provides no
	// ordering across partitions, and this view only emits records once
	// the watermark shows no active partition can still deliver something
	// older (within the allowed lateness). Records emitted in different
	// polls are not ordered with respect to each other.
	Records []*Record

	// Late are records that arrived with an event time older than their
	// partition's watermark (max seen event time - allowed lateness).
	// They are returned separately and are never merged into Records.
	Late []*Record

	// Forced is how many records in Records were emitted ahead of the
	// watermark because of the EventTimeMaxBuffered limit.
	Forced int
}

type eventTimeTP struct {
	topic     string
	partition int32
}

type eventTimePartition struct {
	maxSeen  time.Time // max event time delivered by this partition
	seen     bool      // whether any record has been delivered
	idle     bool      // whether the partition is currently excluded from the watermark
	lastRecv time.Time // wall-clock time of the last record, for idle detection
}

// watermark returns the event time below which this partition has promised
// (within allowed lateness) to not deliver more records.
func (p *eventTimePartition) watermark(lateness time.Duration) time.Time {
	return p.maxSeen.Add(-lateness)
}

type eventTimeItem struct {
	rec *Record
	et  time.Time
}

// eventTimeHeap orders by event time, then by topic/partition/offset so
// that output is deterministic even for records with equal event times.
type eventTimeHeap []eventTimeItem

func (h eventTimeHeap) Len() int { return len(h) }

func (h eventTimeHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if !a.et.Equal(b.et) {
		return a.et.Before(b.et)
	}
	if a.rec.Topic != b.rec.Topic {
		return a.rec.Topic < b.rec.Topic
	}
	if a.rec.Partition != b.rec.Partition {
		return a.rec.Partition < b.rec.Partition
	}
	return a.rec.Offset < b.rec.Offset
}

func (h eventTimeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *eventTimeHeap) Push(x any) { *h = append(*h, x.(eventTimeItem)) }

func (h *eventTimeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = eventTimeItem{}
	*h = old[:n-1]
	return item
}

// EventTimeView is an optional read view that merges records from multiple
// partitions by event time using per-partition watermarks.
//
// Each partition's watermark is its maximum seen event time minus the
// allowed lateness. The merged watermark is the minimum watermark over all
// active partitions, and buffered records at or under it are safe to emit:
// no active partition can still deliver an older record without being
// late. Records that arrive older than their own partition's watermark
// are diverted to EventTimeOutput.Late.
//
// Partition lifecycle:
//
//   - Add: AddPartition registers a partition, and Add auto-registers
//     partitions seen in records. A partition that has not yet delivered
//     any record does not constrain the merged watermark, because Kafka
//     provides no event-time signal for an empty partition and waiting on
//     it could stall output forever.
//   - Idle: a partition that delivered at least one record and then goes
//     silent for longer than the idle timeout stops constraining the
//     merged watermark. It rejoins, resuming its previous watermark, when
//     it delivers another record.
//   - Revoke: RevokePartition drops all state for the partition. Records
//     it still had buffered can never be ordered by future progress, so
//     they are flushed on the next Poll.
//
// The view does not promise global ordering: it only promises that within
// one Poll, emitted records are ordered by event time and that no emitted
// record was still waitable-on under the configured lateness and idle
// rules. Records are never mutated; Topic, Partition, and Offset are
// preserved as consumed.
//
// An EventTimeView is not safe for concurrent use.
type EventTimeView struct {
	cfg   eventTimeViewCfg
	parts map[eventTimeTP]*eventTimePartition
	buf   eventTimeHeap
	flush []*Record // buffered records of revoked partitions, emitted next Poll
	late  []*Record // late records accumulated since the last Poll
}

// NewEventTimeView returns an EventTimeView configured by opts.
func NewEventTimeView(opts ...EventTimeViewOpt) *EventTimeView {
	cfg := eventTimeViewCfg{
		now:     time.Now,
		extract: func(r *Record) time.Time { return r.Timestamp },
	}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	return &EventTimeView{
		cfg:   cfg,
		parts: make(map[eventTimeTP]*eventTimePartition),
	}
}

// AddPartition registers a partition with the view. This is optional: Add
// auto-registers partitions it sees. Registering up front documents intent
// and ensures RevokePartition is meaningful even before the first record.
// A registered partition that has not delivered a record does not stall
// the merged watermark.
func (v *EventTimeView) AddPartition(topic string, partition int32) {
	v.part(eventTimeTP{topic, partition})
}

// RevokePartition drops all state for a partition. Any records the
// partition still had buffered are flushed on the next Poll regardless of
// the watermark, since no future record can advance the partition's
// watermark anymore.
func (v *EventTimeView) RevokePartition(topic string, partition int32) {
	tp := eventTimeTP{topic, partition}
	delete(v.parts, tp)
	kept := v.buf[:0]
	for _, item := range v.buf {
		if item.rec.Topic == topic && item.rec.Partition == partition {
			v.flush = append(v.flush, item.rec)
		} else {
			kept = append(kept, item)
		}
	}
	// Zero the dropped tail before re-heapifying so stale pointers do not
	// linger in the backing array.
	for i := len(kept); i < len(v.buf); i++ {
		v.buf[i] = eventTimeItem{}
	}
	v.buf = kept
	heap.Init(&v.buf)
}

// Add ingests records into the view. Records older than their partition's
// watermark are diverted to the late set; all others are buffered until
// the merged watermark passes their event time.
func (v *EventTimeView) Add(recs ...*Record) {
	now := v.cfg.now()
	for _, r := range recs {
		p := v.part(eventTimeTP{r.Topic, r.Partition})
		et := v.cfg.extract(r)
		if p.seen && et.Before(p.watermark(v.cfg.allowedLateness)) {
			v.late = append(v.late, r)
			continue
		}
		if !p.seen || et.After(p.maxSeen) {
			p.maxSeen = et
		}
		p.seen = true
		p.idle = false
		p.lastRecv = now
		heap.Push(&v.buf, eventTimeItem{rec: r, et: et})
	}
}

// AddFetches ingests all records in fetches, ignoring fetch errors. This
// is a convenience wrapper around Add for use with PollFetches results.
func (v *EventTimeView) AddFetches(fs Fetches) {
	fs.EachRecord(func(r *Record) { v.Add(r) })
}

// Buffered returns how many records are currently buffered waiting for the
// watermark to advance.
func (v *EventTimeView) Buffered() int { return len(v.buf) }

// Poll drains everything currently emittable: records at or under the
// merged watermark (in event-time order), records flushed from revoked
// partitions, records forced out by the max-buffered limit, and any late
// records accumulated since the last Poll.
func (v *EventTimeView) Poll() EventTimeOutput {
	v.refreshIdle()
	watermark, any := v.mergedWatermark()

	var out EventTimeOutput
	for len(v.buf) > 0 && (!any || !v.buf[0].et.After(watermark)) {
		out.Records = append(out.Records, heap.Pop(&v.buf).(eventTimeItem).rec)
	}

	if len(v.flush) > 0 {
		// Revoked-partition leftovers have no watermark to order them
		// against; sort them so their relative order is at least
		// deterministic.
		sort.Slice(v.flush, func(i, j int) bool {
			a, b := v.flush[i], v.flush[j]
			ae, be := v.cfg.extract(a), v.cfg.extract(b)
			if !ae.Equal(be) {
				return ae.Before(be)
			}
			if a.Topic != b.Topic {
				return a.Topic < b.Topic
			}
			if a.Partition != b.Partition {
				return a.Partition < b.Partition
			}
			return a.Offset < b.Offset
		})
		out.Records = append(out.Records, v.flush...)
		v.flush = nil
	}

	if v.cfg.maxBuffered > 0 {
		for len(v.buf) > v.cfg.maxBuffered {
			out.Records = append(out.Records, heap.Pop(&v.buf).(eventTimeItem).rec)
			out.Forced++
		}
	}

	out.Late = v.late
	v.late = nil
	return out
}

func (v *EventTimeView) part(tp eventTimeTP) *eventTimePartition {
	p, ok := v.parts[tp]
	if !ok {
		p = &eventTimePartition{}
		v.parts[tp] = p
	}
	return p
}

// refreshIdle marks partitions idle if they have not delivered a record
// within the idle timeout. Idle detection is lazy (only on Poll) so that
// callers fully control when wall-clock time is consulted.
func (v *EventTimeView) refreshIdle() {
	if v.cfg.idleTimeout <= 0 {
		return
	}
	now := v.cfg.now()
	for _, p := range v.parts {
		if p.seen && !p.idle && now.Sub(p.lastRecv) > v.cfg.idleTimeout {
			p.idle = true
		}
	}
}

// mergedWatermark returns the minimum watermark over all active
// partitions. The second return value reports whether any active
// partition exists; if none does, nothing can make further event-time
// progress, so callers should treat the watermark as unbounded and drain
// everything rather than stall forever.
func (v *EventTimeView) mergedWatermark() (time.Time, bool) {
	var min time.Time
	any := false
	for _, p := range v.parts {
		if !p.seen || p.idle {
			continue
		}
		w := p.watermark(v.cfg.allowedLateness)
		if !any || w.Before(min) {
			min = w
			any = true
		}
	}
	return min, any
}
