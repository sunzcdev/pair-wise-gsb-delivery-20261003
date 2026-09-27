package kgo

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

var eventTimeBase = time.Unix(1_000_000, 0)

// etRec builds a record whose event time is sec seconds after a fixed
// base, keeping topic/partition/offset intact for verification.
func etRec(topic string, partition int32, sec, offset int64) *Record {
	return &Record{
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
		Timestamp: eventTimeBase.Add(time.Duration(sec) * time.Second),
	}
}

// recID renders a record as t/p@offset(sec) for compact comparisons.
func recID(r *Record) string {
	sec := int64(r.Timestamp.Sub(eventTimeBase) / time.Second)
	return fmt.Sprintf("%s/%d@%d(%ds)", r.Topic, r.Partition, r.Offset, sec)
}

func recIDs(rs []*Record) []string {
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, recID(r))
	}
	return ids
}

func checkOutput(t *testing.T, step string, out EventTimeOutput, wantRecords, wantLate []string, wantForced int) {
	t.Helper()
	gotRecords := recIDs(out.Records)
	if fmt.Sprint(gotRecords) != fmt.Sprint(wantRecords) {
		t.Errorf("%s: records got %v, want %v", step, gotRecords, wantRecords)
	}
	gotLate := recIDs(out.Late)
	if fmt.Sprint(gotLate) != fmt.Sprint(wantLate) {
		t.Errorf("%s: late got %v, want %v", step, gotLate, wantLate)
	}
	if out.Forced != wantForced {
		t.Errorf("%s: forced got %d, want %d", step, out.Forced, wantForced)
	}
}

// TestEventTimeViewWatermarkFlow walks an interleaved two-partition
// scenario with a fake clock, showing when the view waits, when it emits,
// and when it judges a record late.
func TestEventTimeViewWatermarkFlow(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	v := NewEventTimeView(
		EventTimeAllowedLateness(5*time.Second),
		EventTimeIdleTimeout(30*time.Second),
		EventTimeNow(clock.Now),
	)
	v.AddPartition("t", 0)
	v.AddPartition("t", 1)

	// Registered but silent partitions must not stall anything, and no
	// active partition exists yet, so there is nothing to wait on.
	checkOutput(t, "no records", v.Poll(), nil, nil, 0)

	// Interleave records: t/0 at 10s and 12s, t/1 at 11s. Both
	// watermarks are now 12-5=7 and 11-5=6, so the merged watermark is
	// 6s and every buffered record (10, 11, 12) must wait.
	v.Add(etRec("t", 0, 10, 0), etRec("t", 0, 12, 1), etRec("t", 1, 11, 0))
	checkOutput(t, "all above merged watermark", v.Poll(), nil, nil, 0)

	// t/0 jumps to 20s, raising its watermark to 15, but t/1 is still at
	// 6: a silent-but-active partition stalls output.
	v.Add(etRec("t", 0, 20, 2))
	checkOutput(t, "silent partition stalls", v.Poll(), nil, nil, 0)

	// 31s of wall clock pass and t/0 delivers a 22s record. t/1 has been
	// silent for longer than the idle timeout, so it stops constraining
	// the watermark, which jumps to t/0's 22-5=17. Everything at or
	// under 17s emits in event-time order across partitions.
	clock.Advance(31 * time.Second)
	v.Add(etRec("t", 0, 22, 3))
	checkOutput(t, "idle releases", v.Poll(),
		[]string{"t/0@0(10s)", "t/1@0(11s)", "t/0@1(12s)"}, nil, 0)

	// t/1 delivers again at 8s. It is not late (its own watermark is
	// 11-5=6, and 8 >= 6), it reactivates the partition, and its
	// watermark stays 6, so the record itself must wait.
	v.Add(etRec("t", 1, 8, 1))
	checkOutput(t, "reactivated partition waits", v.Poll(), nil, nil, 0)

	// t/1 jumps to 30s, raising its watermark to 25. The merged
	// watermark is min(17, 25) = 17, so the 8s record emits while t/0's
	// 20s and 22s records keep waiting.
	v.Add(etRec("t", 1, 30, 2))
	checkOutput(t, "merged watermark emits 8s only", v.Poll(),
		[]string{"t/1@1(8s)"}, nil, 0)

	// t/0's watermark is 22-5=17. A record at 14s is strictly older:
	// late. A record at exactly 17s is at the boundary: not late, and it
	// emits immediately because the merged watermark is 17.
	v.Add(etRec("t", 0, 14, 4), etRec("t", 0, 17, 5))
	checkOutput(t, "late vs boundary", v.Poll(),
		[]string{"t/0@5(17s)"}, []string{"t/0@4(14s)"}, 0)

	// Revoking t/1 flushes its buffered 30s record even though the
	// merged watermark (now only t/0's 17) is below it: no future record
	// can ever order it.
	v.RevokePartition("t", 1)
	checkOutput(t, "revoke flushes", v.Poll(), []string{"t/1@2(30s)"}, nil, 0)

	// Revoking the last partition leaves no active partition at all, so
	// the remaining buffered records drain rather than stalling forever.
	v.RevokePartition("t", 0)
	checkOutput(t, "no active partitions drains", v.Poll(),
		[]string{"t/0@2(20s)", "t/0@3(22s)"}, nil, 0)
}

// TestEventTimeViewMaxBuffered shows records being emitted ahead of the
// watermark when the buffer limit is exceeded.
func TestEventTimeViewMaxBuffered(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	v := NewEventTimeView(
		EventTimeAllowedLateness(0),
		EventTimeMaxBuffered(2),
		EventTimeNow(clock.Now),
	)

	// t/1 sits at event time 0, pinning the merged watermark at 0 while
	// t/0 buffers records far above it.
	v.Add(etRec("t", 1, 0, 0))
	v.Add(etRec("t", 0, 50, 0), etRec("t", 0, 60, 1), etRec("t", 0, 70, 2))

	// The 0s record emits normally; the buffer then holds 3 records, one
	// over the limit, so the smallest (50s) is forced out early.
	checkOutput(t, "forced emit", v.Poll(),
		[]string{"t/1@0(0s)", "t/0@0(50s)"}, nil, 1)
	if got := v.Buffered(); got != 2 {
		t.Errorf("buffered got %d, want 2", got)
	}
}

// TestEventTimeViewAddFetches ensures the Fetches convenience path
// preserves topic, partition, and offset.
func TestEventTimeViewAddFetches(t *testing.T) {
	clock := &fakeClock{now: eventTimeBase}
	v := NewEventTimeView(EventTimeNow(clock.Now))

	var fetches Fetches
	fetches = append(fetches, Fetch{Topics: []FetchTopic{
		{Topic: "a", Partitions: []FetchPartition{
			{Partition: 0, Records: []*Record{etRec("a", 0, 1, 0)}},
			{Partition: 1, Records: []*Record{etRec("a", 1, 2, 0)}},
		}},
	}})
	v.AddFetches(fetches)

	// The merged watermark is min(1, 2) = 1, so only the 1s record emits;
	// the 2s record waits until partition 0 advances its watermark.
	checkOutput(t, "fetches", v.Poll(),
		[]string{"a/0@0(1s)"}, nil, 0)
	v.Add(etRec("a", 0, 3, 1))
	checkOutput(t, "fetches advanced", v.Poll(),
		[]string{"a/1@0(2s)"}, nil, 0)
}
