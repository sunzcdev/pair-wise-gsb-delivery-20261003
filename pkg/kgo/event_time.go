package kgo

import (
	"container/heap"
	"time"
)

// EventTimeReason describes why an EventTimeRecord was emitted.
type EventTimeReason int8

const (
	// EventTimeWatermark means the record's event time fell at or below
	// the current watermark: the normal, in-order path.
	EventTimeWatermark EventTimeReason = iota
	// EventTimeBufferFull means the record was emitted before the
	// watermark reached it because the buffered record count exceeded
	// the configured maximum. Emitting the earliest buffered records
	// keeps memory bounded when one partition stalls faster than the
	// idle timeout can compensate for.
	EventTimeBufferFull
	// EventTimeRevoked means the record was buffered for a partition
	// that was revoked. A revoked partition's watermark can never
	// advance again, so its remaining records are flushed in event
	// time order rather than being dropped.
	EventTimeRevoked
	// EventTimeDrained means the record was flushed by Drain, e.g. on
	// shutdown, without waiting for the watermark.
	EventTimeDrained
)

// EventTimeRecord is a record emitted from an EventTimeMerger, along
// with why it was emitted. The underlying Record keeps its original
// Topic, Partition, and Offset, so downstream offset tracking and
// commits are unaffected.
type EventTimeRecord struct {
	Record *Record
	Reason EventTimeReason
}

// EventTimeOutput is the result of any EventTimeMerger call that can
// emit records.
type EventTimeOutput struct {
	// Records are records ready for processing. Records emitted with
	// reason EventTimeWatermark are in non-decreasing event time order.
	// Records with other reasons were emitted early (buffer pressure,
	// revocation, drain) and may be ahead of the watermark.
	Records []EventTimeRecord
	// Late contains records whose event time was already behind the
	// watermark when they arrived. They are returned separately instead
	// of being merged into Records so that the caller can decide how to
	// handle them (side output, metrics, dropping). They are never
	// silently discarded.
	Late []*Record
	// Watermark is the merger's watermark after this call. It is the
	// zero time if no partition is currently contributing.
	Watermark time.Time
}

// EventTimeMergerOpt configures an EventTimeMerger.
type EventTimeMergerOpt interface{ apply(*eventTimeMergerCfg) }

type eventTimeMergerOptFn func(*eventTimeMergerCfg)

func (fn eventTimeMergerOptFn) apply(cfg *eventTimeMergerCfg) { fn(cfg) }

type eventTimeMergerCfg struct {
	lateness    time.Duration
	idleTimeout time.Duration
	maxBuffered int
	eventTime   func(*Record) time.Time
	now         func() time.Time
}

// EventTimeAllowedLateness sets how far behind the per-partition
// maximum observed event time a partition's watermark trails. A record
// is late once the global watermark passes its event time, so the
// lateness bound is the maximum out-of-orderness the merger tolerates
// before judging a record late. The default is 0, meaning any record
// older than the per-partition maximum can be late.
func EventTimeAllowedLateness(d time.Duration) EventTimeMergerOpt {
	return eventTimeMergerOptFn(func(cfg *eventTimeMergerCfg) { cfg.lateness = d })
}

// EventTimeIdleTimeout sets how long a partition may go without
// delivering a record before it is excluded from the watermark
// calculation. Without this, one silent partition holds the global
// watermark back forever. An idle partition rejoins the calculation as
// soon as it delivers another record; that record may be judged late
// if the watermark moved past it in the meantime. The default is 0,
// meaning partitions never go idle.
func EventTimeIdleTimeout(d time.Duration) EventTimeMergerOpt {
	return eventTimeMergerOptFn(func(cfg *eventTimeMergerCfg) { cfg.idleTimeout = d })
}

// EventTimeMaxBuffered caps how many records the merger buffers while
// waiting for the watermark. When adding a record would exceed the
// cap, the earliest buffered records are emitted immediately with
// reason EventTimeBufferFull, ahead of the watermark. The default is
// 0, meaning unbounded.
func EventTimeMaxBuffered(n int) EventTimeMergerOpt {
	return eventTimeMergerOptFn(func(cfg *eventTimeMergerCfg) { cfg.maxBuffered = n })
}

// EventTimeFunc overrides how the event time of a record is
// determined. The default uses Record.Timestamp, which is the
// producer-set CreateTime timestamp.
func EventTimeFunc(fn func(*Record) time.Time) EventTimeMergerOpt {
	return eventTimeMergerOptFn(func(cfg *eventTimeMergerCfg) { cfg.eventTime = fn })
}

// EventTimeNowFunc overrides the clock used to detect idle partitions.
// This exists mainly so tests can drive idleness with a fake clock.
// The default is time.Now.
func EventTimeNowFunc(fn func() time.Time) EventTimeMergerOpt {
	return eventTimeMergerOptFn(func(cfg *eventTimeMergerCfg) { cfg.now = fn })
}

// eventTimePart tracks the watermark state of one partition.
type eventTimePart struct {
	maxSeen    time.Time // max event time observed on this partition
	seen       bool      // whether any record has been observed
	lastRecord time.Time // wall clock time of the last observed record, for idleness
}

// watermark returns this partition's watermark if it contributes to
// the global minimum, and whether it contributes at all.
func (p *eventTimePart) watermark(now time.Time, lateness, idleTimeout time.Duration) (time.Time, bool) {
	if !p.seen {
		// A partition that has never produced cannot contribute a
		// meaningful watermark; treating it as active would block
		// all output forever.
		return time.Time{}, false
	}
	if idleTimeout > 0 && now.Sub(p.lastRecord) >= idleTimeout {
		return time.Time{}, false
	}
	return p.maxSeen.Add(-lateness), true
}

// eventTimeHeapItem is one buffered record. The sequence number breaks
// event time ties in arrival order so that output is stable.
type eventTimeHeapItem struct {
	rec  *Record
	ts   time.Time
	seq  uint64
	part eventTimeTopicPartition
}

type eventTimeTopicPartition struct {
	topic     string
	partition int32
}

type eventTimeHeap []eventTimeHeapItem

func (h eventTimeHeap) Len() int { return len(h) }

func (h eventTimeHeap) Less(i, j int) bool {
	if !h[i].ts.Equal(h[j].ts) {
		return h[i].ts.Before(h[j].ts)
	}
	return h[i].seq < h[j].seq
}

func (h eventTimeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *eventTimeHeap) Push(x any) { *h = append(*h, x.(eventTimeHeapItem)) }

func (h *eventTimeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// EventTimeMerger merges records from multiple partitions into a
// single event time ordered stream using watermarks.
//
// Kafka itself guarantees ordering only within a single partition.
// This merger provides a best-effort view across partitions: a record
// is emitted once every contributing partition's watermark has passed
// the record's event time. It does NOT provide global ordering:
// records arriving later than the configured allowed lateness are
// returned separately as late, and records forced out by the buffer
// cap, revocation, or draining can be emitted ahead of the watermark.
//
// The merger is not safe for concurrent use; callers are expected to
// drive it from a single poll loop.
type EventTimeMerger struct {
	cfg   eventTimeMergerCfg
	parts map[eventTimeTopicPartition]*eventTimePart
	buf   eventTimeHeap
	seq   uint64
}

// NewEventTimeMerger returns an EventTimeMerger configured with the
// given options.
func NewEventTimeMerger(opts ...EventTimeMergerOpt) *EventTimeMerger {
	cfg := eventTimeMergerCfg{
		eventTime: func(r *Record) time.Time { return r.Timestamp },
		now:       time.Now,
	}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	return &EventTimeMerger{
		cfg:   cfg,
		parts: make(map[eventTimeTopicPartition]*eventTimePart),
	}
}

// Watermark returns the current global watermark: the minimum
// watermark over all partitions that have produced a record and are
// not idle. It returns the zero time if no partition contributes.
func (m *EventTimeMerger) Watermark() time.Time {
	return m.watermark(m.cfg.now())
}

func (m *EventTimeMerger) watermark(now time.Time) time.Time {
	var wm time.Time
	for _, p := range m.parts {
		pwm, ok := p.watermark(now, m.cfg.lateness, m.cfg.idleTimeout)
		if !ok {
			continue
		}
		if wm.IsZero() || pwm.Before(wm) {
			wm = pwm
		}
	}
	return wm
}

// Buffered returns how many records are currently buffered waiting for
// the watermark.
func (m *EventTimeMerger) Buffered() int { return len(m.buf) }

// Add buffers all records in fetches and returns whatever becomes
// emittable. It is meant to be called with the result of PollFetches /
// PollRecords. Partitions appearing in fetches are tracked
// automatically; partitions that disappear must be removed with Revoke
// (or will eventually be idled out if an idle timeout is configured).
func (m *EventTimeMerger) Add(fetches Fetches) EventTimeOutput {
	var out EventTimeOutput
	fetches.EachRecord(func(r *Record) {
		m.addRecord(r, &out)
	})
	m.emit(m.cfg.now(), &out)
	return out
}

// AddRecord buffers a single record and returns whatever becomes
// emittable. It is equivalent to Add for callers that already iterate
// records themselves.
func (m *EventTimeMerger) AddRecord(r *Record) EventTimeOutput {
	var out EventTimeOutput
	m.addRecord(r, &out)
	m.emit(m.cfg.now(), &out)
	return out
}

func (m *EventTimeMerger) addRecord(r *Record, out *EventTimeOutput) {
	now := m.cfg.now()
	ts := m.cfg.eventTime(r)
	tp := eventTimeTopicPartition{r.Topic, r.Partition}

	p := m.parts[tp]

	// Judge lateness against the watermark before this record can
	// advance it; otherwise a record could never be late on its own
	// partition. A partition's first observed record is exempt: a
	// newly assigned partition is usually far behind the existing
	// watermark, and judging its entire backlog late would make the
	// late stream useless.
	if p != nil && p.seen {
		if wm := m.watermark(now); !wm.IsZero() && ts.Before(wm) {
			out.Late = append(out.Late, r)
			return
		}
	}

	if p == nil {
		p = &eventTimePart{}
		m.parts[tp] = p
	}
	if !p.seen || ts.After(p.maxSeen) {
		p.maxSeen = ts
	}
	p.seen = true
	p.lastRecord = now

	heap.Push(&m.buf, eventTimeHeapItem{rec: r, ts: ts, seq: m.seq, part: tp})
	m.seq++

	// If the buffer overflows, shed the earliest records first: they
	// are the closest to being emittable anyway, and shedding the
	// newest would reorder the stream far more.
	for m.cfg.maxBuffered > 0 && len(m.buf) > m.cfg.maxBuffered {
		item := heap.Pop(&m.buf).(eventTimeHeapItem)
		out.Records = append(out.Records, EventTimeRecord{item.rec, EventTimeBufferFull})
	}
}

// emit pops every buffered record at or below the watermark.
func (m *EventTimeMerger) emit(now time.Time, out *EventTimeOutput) {
	wm := m.watermark(now)
	for len(m.buf) > 0 && !wm.IsZero() && !m.buf[0].ts.After(wm) {
		item := heap.Pop(&m.buf).(eventTimeHeapItem)
		out.Records = append(out.Records, EventTimeRecord{item.rec, EventTimeWatermark})
	}
	out.Watermark = wm
}

// Tick re-evaluates idleness and the watermark without adding records.
// Call this periodically when fetches are slow: a partition that went
// idle since the last record may allow buffered records to be emitted.
func (m *EventTimeMerger) Tick() EventTimeOutput {
	var out EventTimeOutput
	m.emit(m.cfg.now(), &out)
	return out
}

// Revoke removes partitions from the merger, e.g. from an
// OnPartitionsRevoked callback. Any records still buffered for these
// partitions are flushed in event time order with reason
// EventTimeRevoked, since their partition's watermark can never
// advance again. Removing a partition may also raise the watermark and
// release records of the remaining partitions.
func (m *EventTimeMerger) Revoke(topic string, partitions ...int32) EventTimeOutput {
	var out EventTimeOutput
	if len(partitions) == 0 {
		out.Watermark = m.Watermark()
		return out
	}
	revoked := make(map[eventTimeTopicPartition]bool, len(partitions))
	for _, partition := range partitions {
		tp := eventTimeTopicPartition{topic, partition}
		revoked[tp] = true
		delete(m.parts, tp)
	}
	kept := m.buf[:0]
	var flush eventTimeHeap
	for _, item := range m.buf {
		if revoked[item.part] {
			flush = append(flush, item)
		} else {
			kept = append(kept, item)
		}
	}
	m.buf = kept
	heap.Init(&m.buf)
	heap.Init(&flush)
	for len(flush) > 0 {
		item := heap.Pop(&flush).(eventTimeHeapItem)
		out.Records = append(out.Records, EventTimeRecord{item.rec, EventTimeRevoked})
	}
	m.emit(m.cfg.now(), &out)
	return out
}

// Drain flushes every buffered record in event time order with reason
// EventTimeDrained and resets the merger. It is meant for shutdown,
// when waiting for the watermark is no longer possible.
func (m *EventTimeMerger) Drain() EventTimeOutput {
	var out EventTimeOutput
	for len(m.buf) > 0 {
		item := heap.Pop(&m.buf).(eventTimeHeapItem)
		out.Records = append(out.Records, EventTimeRecord{item.rec, EventTimeDrained})
	}
	m.parts = make(map[eventTimeTopicPartition]*eventTimePart)
	out.Watermark = time.Time{}
	return out
}
