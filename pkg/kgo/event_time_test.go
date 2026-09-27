package kgo

import (
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

var eventTimeBase = time.UnixMilli(1700000000000)

// etmRec builds a record whose event time is sec seconds after a fixed
// base, keeping topic, partition, and offset intact for assertions.
func etmRec(topic string, partition int32, offset int64, sec int) *Record {
	return &Record{
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
		Timestamp: eventTimeBase.Add(time.Duration(sec) * time.Second),
	}
}

func etmCheckEmitted(t *testing.T, out EventTimeOutput, reason EventTimeReason, want ...*Record) {
	t.Helper()
	if len(out.Records) != len(want) {
		t.Fatalf("got %d emitted records, want %d", len(out.Records), len(want))
	}
	for i, er := range out.Records {
		if er.Record != want[i] {
			t.Errorf("emitted[%d] = %s/%d@%d, want %s/%d@%d",
				i, er.Record.Topic, er.Record.Partition, er.Record.Offset,
				want[i].Topic, want[i].Partition, want[i].Offset)
		}
		if er.Reason != reason {
			t.Errorf("emitted[%d] reason = %v, want %v", i, er.Reason, reason)
		}
	}
}

func etmCheckNone(t *testing.T, out EventTimeOutput) {
	t.Helper()
	if len(out.Records) != 0 || len(out.Late) != 0 {
		t.Fatalf("expected no output, got %d records and %d late", len(out.Records), len(out.Late))
	}
}

// Interleaved records across two partitions: a record waits until every
// contributing partition's watermark passes its event time, then emits
// in non-decreasing event time order.
func TestEventTimeMergerWaitsAndEmitsInOrder(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	m := NewEventTimeMerger(
		EventTimeAllowedLateness(2*time.Second),
		EventTimeNowFunc(clock.Now),
	)

	p0ts10 := etmRec("t", 0, 0, 10)
	p1ts1 := etmRec("t", 1, 0, 1)
	p1ts12 := etmRec("t", 1, 1, 12)
	p0ts20 := etmRec("t", 0, 1, 20)
	p1ts30 := etmRec("t", 1, 2, 30)

	// Only p0 has produced; its own watermark is 8, so ts=10 waits.
	etmCheckNone(t, m.AddRecord(p0ts10))

	// p1 joins far behind; the global watermark drops to min(8, -1).
	etmCheckNone(t, m.AddRecord(p1ts1))

	// p1 advances to 12, so its watermark is 10; the global watermark
	// is min(8, 10) = 8, releasing only ts=1.
	etmCheckEmitted(t, m.AddRecord(p1ts12), EventTimeWatermark, p1ts1)

	// p0 advances to 20, so its watermark is 18; the global watermark
	// is min(18, 10) = 10, releasing ts=10 but not ts=12.
	etmCheckEmitted(t, m.AddRecord(p0ts20), EventTimeWatermark, p0ts10)

	// p1 advances to 30, so its watermark is 28; the global watermark
	// is min(18, 28) = 18, releasing ts=12. ts=20 still waits.
	etmCheckEmitted(t, m.AddRecord(p1ts30), EventTimeWatermark, p1ts12)

	// Drain releases whatever is left, ahead of the watermark.
	etmCheckEmitted(t, m.Drain(), EventTimeDrained, p0ts20, p1ts30)
}

// A record whose event time is behind the current watermark is
// returned as late, never merged into the ordered stream.
func TestEventTimeMergerLateRecord(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	m := NewEventTimeMerger(EventTimeNowFunc(clock.Now)) // zero lateness

	p0ts10 := etmRec("t", 0, 0, 10)
	p0ts5 := etmRec("t", 0, 1, 5)

	// Zero lateness: the first record sets the watermark to its own
	// event time and is emitted immediately.
	etmCheckEmitted(t, m.AddRecord(p0ts10), EventTimeWatermark, p0ts10)

	// ts=5 is behind the watermark (10): late, returned separately.
	out := m.AddRecord(p0ts5)
	if len(out.Records) != 0 {
		t.Fatalf("late record leaked into ordered stream")
	}
	if len(out.Late) != 1 || out.Late[0] != p0ts5 {
		t.Fatalf("got %d late records, want the ts=5 record", len(out.Late))
	}
	if out.Late[0].Offset != 1 || out.Late[0].Partition != 0 || out.Late[0].Topic != "t" {
		t.Fatalf("late record lost its identity: %+v", out.Late[0])
	}
}

// A partition that stops producing is excluded from the watermark once
// the idle timeout passes, which can release records it was holding
// back. When it resumes, its stale records are judged late.
func TestEventTimeMergerIdlePartition(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	m := NewEventTimeMerger(
		EventTimeAllowedLateness(5*time.Second),
		EventTimeIdleTimeout(10*time.Second),
		EventTimeNowFunc(clock.Now),
	)

	p1ts4 := etmRec("t", 1, 0, 4)
	p0ts10 := etmRec("t", 0, 0, 10)
	p1ts2 := etmRec("t", 1, 1, 2)

	etmCheckNone(t, m.AddRecord(p1ts4)) // p1 watermark: -1
	clock.Advance(time.Second)
	// Global watermark is min(5, -1) = -1; both records wait.
	etmCheckNone(t, m.AddRecord(p0ts10))

	// 9.5s later p1 has been silent for 10.5s (idle), while p0 is
	// still active (9.5s < 10s). The watermark becomes p0's alone: 5.
	// That releases p1's buffered ts=4 but not p0's ts=10.
	clock.Advance(9*time.Second + 500*time.Millisecond)
	etmCheckEmitted(t, m.Tick(), EventTimeWatermark, p1ts4)

	// p1 resumes with a record the watermark (5) has already passed:
	// it is late.
	out := m.AddRecord(p1ts2)
	if len(out.Late) != 1 || out.Late[0] != p1ts2 {
		t.Fatalf("resumed partition's stale record was not judged late")
	}
}

// Revoking a partition flushes its buffered records in event time
// order and removes it from the watermark calculation.
func TestEventTimeMergerRevoke(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	m := NewEventTimeMerger(EventTimeNowFunc(clock.Now)) // zero lateness

	p0ts5 := etmRec("t", 0, 0, 5)
	p1ts3 := etmRec("t", 1, 0, 3)
	p1ts8 := etmRec("t", 1, 1, 8)
	p1ts6 := etmRec("t", 1, 2, 6)
	p0ts7 := etmRec("t", 0, 1, 7)

	etmCheckEmitted(t, m.AddRecord(p0ts5), EventTimeWatermark, p0ts5)
	etmCheckEmitted(t, m.AddRecord(p1ts3), EventTimeWatermark, p1ts3)
	etmCheckNone(t, m.AddRecord(p1ts8)) // watermark is min(5, 8) = 5
	etmCheckNone(t, m.AddRecord(p1ts6)) // 6 > 5, waits behind the watermark

	// Revoking p1 flushes its buffered records in event time order
	// (6 before 8) and leaves p0's watermark (5) as the global one.
	etmCheckEmitted(t, m.Revoke("t", 1), EventTimeRevoked, p1ts6, p1ts8)
	if got := m.Watermark(); !got.Equal(eventTimeBase.Add(5 * time.Second)) {
		t.Fatalf("watermark after revoke = %v, want base+5s", got)
	}

	// p0 keeps working normally afterwards.
	etmCheckEmitted(t, m.AddRecord(p0ts7), EventTimeWatermark, p0ts7)
}

// When the buffer cap is exceeded, the earliest buffered records are
// emitted ahead of the watermark to bound memory.
func TestEventTimeMergerBufferFull(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	m := NewEventTimeMerger(
		EventTimeAllowedLateness(time.Hour), // watermark never catches up here
		EventTimeMaxBuffered(2),
		EventTimeNowFunc(clock.Now),
	)

	p0ts10 := etmRec("t", 0, 0, 10)
	p0ts20 := etmRec("t", 0, 1, 20)
	p0ts30 := etmRec("t", 0, 2, 30)

	etmCheckNone(t, m.AddRecord(p0ts10))
	etmCheckNone(t, m.AddRecord(p0ts20))
	// The third record overflows the cap of 2: the earliest buffered
	// record is forced out.
	etmCheckEmitted(t, m.AddRecord(p0ts30), EventTimeBufferFull, p0ts10)
	if got := m.Buffered(); got != 2 {
		t.Fatalf("buffered = %d, want 2", got)
	}
}

// Add consumes whole fetches, and emitted records keep their original
// topic, partition, and offset.
func TestEventTimeMergerAddFetches(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	m := NewEventTimeMerger(EventTimeNowFunc(clock.Now)) // zero lateness

	p0 := []*Record{etmRec("a", 0, 10, 1), etmRec("a", 0, 11, 2)}
	p1 := []*Record{etmRec("a", 1, 20, 1), etmRec("a", 1, 21, 4)}
	fetches := Fetches{{
		Topics: []FetchTopic{{
			Topic: "a",
			Partitions: []FetchPartition{
				{Partition: 0, Records: p0},
				{Partition: 1, Records: p1},
			},
		}},
	}}

	// Watermark is min(2, 4) = 2: everything but p1's ts=4 is emitted,
	// ties broken by arrival order.
	out := m.Add(fetches)
	etmCheckEmitted(t, out, EventTimeWatermark, p0[0], p1[0], p0[1])
	for i, want := range []struct {
		topic     string
		partition int32
		offset    int64
	}{{"a", 0, 10}, {"a", 1, 20}, {"a", 0, 11}} {
		r := out.Records[i].Record
		if r.Topic != want.topic || r.Partition != want.partition || r.Offset != want.offset {
			t.Errorf("emitted[%d] identity = %s/%d@%d, want %s/%d@%d",
				i, r.Topic, r.Partition, r.Offset, want.topic, want.partition, want.offset)
		}
	}
}
