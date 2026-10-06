package kfake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/kversion"
)

// Strict topic ID pinning (kgo.PinTopicIDs) binds a topic name to a fixed
// topic ID. When the topic is deleted and recreated under the same name, the
// replacement gets a new ID; a pinned client must keep talking to the old ID
// and fail rather than silently produce to, consume from, or flush queued
// records to the replacement. If the negotiated protocol can only address the
// topic by name, strict mode is refused outright.
//
// The tests drive a fake broker that first answers metadata with the old ID
// and then with the same name under a new ID, then inspect how producing,
// consuming, and already-queued records each end.

func pinOpts(topic string, id [16]byte) []kgo.Opt {
	return []kgo.Opt{
		kgo.PinTopicIDs(map[string][16]byte{topic: id}),
		kgo.MetadataMinAge(20 * time.Millisecond),
		kgo.RetryBackoffFn(func(int) time.Duration { return 5 * time.Millisecond }),
	}
}

// producePromiseErr produces one record and waits for its promise, returning
// the delivery error (nil on success).
func producePromiseErr(t *testing.T, ctx context.Context, cl *kgo.Client, topic, value string) error {
	t.Helper()
	done := make(chan error, 1)
	rec := kgo.StringRecord(value)
	rec.Topic = topic
	cl.Produce(ctx, rec, func(_ *kgo.Record, err error) { done <- err })
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("produce promise for %q never resolved", value)
		return nil
	}
}

// TestPinnedTopicProduceRejectedAfterRecreate: records produced after the
// client learns of the recreation fail instead of landing on the new
// incarnation.
func TestPinnedTopicProduceRejectedAfterRecreate(t *testing.T) {
	t.Parallel()
	const topic = "t-pin-produce"
	c := newCluster(t, NumBrokers(1), SeedTopics(1, topic))
	oldID := c.TopicInfo(topic).TopicID

	cl := newPlainClient(t, c, pinOpts(topic, oldID)...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := producePromiseErr(t, ctx, cl, topic, "before"); err != nil {
		t.Fatalf("initial produce to the pinned topic failed: %v", err)
	}

	recreateTopic(t, c, topic)
	newID := c.TopicInfo(topic).TopicID
	if newID == oldID {
		t.Fatal("recreation did not assign a new topic ID")
	}
	produceNStrings(t, newPlainClient(t, c), topic, 2)

	cl.ForceMetadataRefresh()

	err := producePromiseErr(t, ctx, cl, topic, "after")
	if !errors.Is(err, kgo.ErrTopicIDMismatch) {
		t.Fatalf("post-recreate produce returned %v, want ErrTopicIDMismatch", err)
	}
}

// TestPinnedTopicQueuedRecordsRejected exercises the "queued but not yet
// sent" requirement: the pinned client's first metadata response names the
// topic with an ID other than the pin, so a record waiting client side for
// partitions must fail rather than be sent to the replacement.
func TestPinnedTopicQueuedRecordsRejected(t *testing.T) {
	t.Parallel()
	const topic = "t-pin-queued"
	c := newCluster(t, NumBrokers(1), SeedTopics(1, topic))
	oldID := c.TopicInfo(topic).TopicID

	recreateTopic(t, c, topic)
	newID := c.TopicInfo(topic).TopicID
	if newID == oldID {
		t.Fatal("recreation did not assign a new topic ID")
	}
	produceNStrings(t, newPlainClient(t, c), topic, 1)

	cl := newPlainClient(t, c, pinOpts(topic, oldID)...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err := producePromiseErr(t, ctx, cl, topic, "queued")
	if !errors.Is(err, kgo.ErrTopicIDMismatch) {
		t.Fatalf("queued record resolved with %v, want ErrTopicIDMismatch", err)
	}
}

// TestPinnedTopicBufferedRecordsRejected: records that are already buffered
// (partition chosen) but not yet sent when the client learns of the
// recreation are failed rather than drained to the new incarnation.
func TestPinnedTopicBufferedRecordsRejected(t *testing.T) {
	t.Parallel()
	const topic = "t-pin-buffered"
	c := newCluster(t, NumBrokers(1), SeedTopics(1, topic))
	oldID := c.TopicInfo(topic).TopicID

	// Hold the first produce request so records stay buffered/unsent while
	// the topic is recreated underneath the client.
	held, release := hold(c, kmsg.Produce, 0)
	t.Cleanup(release)

	cl := newPlainClient(t, c,
		append(pinOpts(topic, oldID), kgo.ManualFlushing())...,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := kgo.StringRecord("first")
	first.Topic = topic
	firstDone := make(chan error, 1)
	cl.Produce(ctx, first, func(_ *kgo.Record, err error) { firstDone <- err })
	go func() { _ = cl.Flush(ctx) }()

	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("first produce request was never held")
	}

	// Recreate while the first record's request is held unsent.
	recreateTopic(t, c, topic)
	if c.TopicInfo(topic).TopicID == oldID {
		t.Fatal("recreation did not assign a new topic ID")
	}
	cl.ForceMetadataRefresh()

	// Release the held old-ID request. It is addressed to the old ID, so
	// the broker answers UNKNOWN_TOPIC_ID; strict pinning must abandon it
	// rather than retry it by name into the new incarnation.
	release()
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("held record addressed to the deleted topic was reported successful")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("held produce promise never resolved after release")
	}

	// A record buffered after the client learned of the recreation must
	// fail with the identity error and never be sent.
	err := producePromiseErr(t, ctx, cl, topic, "buffered")
	if !errors.Is(err, kgo.ErrTopicIDMismatch) {
		t.Fatalf("buffered post-recreate produce returned %v, want ErrTopicIDMismatch", err)
	}
}

// TestPinnedTopicConsumeRejectedAfterRecreate: a consumer that read the old
// topic must surface the identity error after recreation and never hand back
// records produced to the new incarnation.
func TestPinnedTopicConsumeRejectedAfterRecreate(t *testing.T) {
	t.Parallel()
	const topic = "t-pin-consume"
	c := newCluster(t, NumBrokers(1), SeedTopics(1, topic))
	oldID := c.TopicInfo(topic).TopicID

	prod := newPlainClient(t, c)
	produceNStrings(t, prod, topic, 2)

	opts := append(pinOpts(topic, oldID),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(50*time.Millisecond),
	)
	cl := newPlainClient(t, c, opts...)

	first := consumeN(t, cl, 2, 10*time.Second)
	if len(first) != 2 {
		t.Fatalf("consumed %d records before recreation, want 2", len(first))
	}

	recreateTopic(t, c, topic)
	if c.TopicInfo(topic).TopicID == oldID {
		t.Fatal("recreation did not assign a new topic ID")
	}
	produceNStrings(t, newPlainClient(t, c), topic, 2)
	cl.ForceMetadataRefresh()

	deadline := time.Now().Add(15 * time.Second)
	sawMismatch := false
	for time.Now().Before(deadline) && !sawMismatch {
		pollCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		fs := cl.PollFetches(pollCtx)
		cancel()
		fs.EachRecord(func(r *kgo.Record) {
			t.Fatalf("consumed a record from the recreated topic %s/%d@%d", r.Topic, r.Partition, r.Offset)
		})
		for _, e := range fs.Errors() {
			if errors.Is(e.Err, kgo.ErrTopicIDMismatch) {
				sawMismatch = true
			}
		}
	}
	if !sawMismatch {
		t.Fatal("never surfaced ErrTopicIDMismatch while consuming the recreated topic")
	}
}

// TestPinnedTopicRejectedWhenProtocolIsByName: if the negotiated protocol can
// only request metadata/produce/fetch by name (versions below topic IDs),
// strict mode must be refused rather than silently downgraded.
func TestPinnedTopicRejectedWhenProtocolIsByName(t *testing.T) {
	t.Parallel()
	const topic = "t-pin-oldproto"
	c := newCluster(t, NumBrokers(1), SeedTopics(1, topic))
	id := c.TopicInfo(topic).TopicID

	vs := kversion.Stable()
	vs.SetMaxKeyVersion(int16(kmsg.Metadata), 9) // IDs arrive in v10
	vs.SetMaxKeyVersion(int16(kmsg.Produce), 12) // IDs on the wire in v13
	vs.SetMaxKeyVersion(int16(kmsg.Fetch), 12)   // IDs on the wire in v13

	cl := newPlainClient(t, c, append(pinOpts(topic, id), kgo.MaxVersions(vs))...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err := producePromiseErr(t, ctx, cl, topic, "x")
	if !errors.Is(err, kgo.ErrTopicIDPinningUnsupported) {
		t.Fatalf("produce on a name-only protocol returned %v, want ErrTopicIDPinningUnsupported", err)
	}
}

// TestPinnedTopicGroupConsumeHealthy: on a modern, ID-capable cluster a group
// consumer with a pin works normally, including ID-addressed offset
// commits/fetches; strict pinning must not false-trigger on the happy path.
func TestPinnedTopicGroupConsumeHealthy(t *testing.T) {
	t.Parallel()
	const (
		topic = "t-pin-group"
		group = "g-pin-group"
	)
	c := newCluster(t, NumBrokers(1), SeedTopics(1, topic))
	id := c.TopicInfo(topic).TopicID

	produceNStrings(t, newPlainClient(t, c), topic, 3)

	opts := append(pinOpts(topic, id),
		kgo.ConsumeTopics(topic),
		kgo.ConsumerGroup(group),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(50*time.Millisecond),
	)
	cl := newPlainClient(t, c, opts...)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var got int
	deadline := time.Now().Add(15 * time.Second)
	for got < 3 && time.Now().Before(deadline) {
		fs := cl.PollFetches(ctx)
		for _, e := range fs.Errors() {
			t.Fatalf("unexpected fetch error on healthy pinned group: %v", e.Err)
		}
		got += len(fs.Records())
	}
	if got != 3 {
		t.Fatalf("healthy pinned group consumed %d records, want 3", got)
	}

	// A manual commit by pinned ID must succeed.
	if err := cl.CommitUncommittedOffsets(ctx); err != nil {
		t.Fatalf("commit by pinned topic id failed: %v", err)
	}
}

// TestPinnedTopicConsumeRejectedWhenProtocolIsByName mirrors the produce
// name-only test for a consumer: a capped fetch/metadata protocol cannot
// identify the pinned topic, so poll surfaces ErrTopicIDPinningUnsupported
// rather than reading by name.
func TestPinnedTopicConsumeRejectedWhenProtocolIsByName(t *testing.T) {
	t.Parallel()
	const topic = "t-pin-oldproto-consume"
	c := newCluster(t, NumBrokers(1), SeedTopics(1, topic))
	id := c.TopicInfo(topic).TopicID

	vs := kversion.Stable()
	vs.SetMaxKeyVersion(int16(kmsg.Metadata), 9)
	vs.SetMaxKeyVersion(int16(kmsg.Produce), 12)
	vs.SetMaxKeyVersion(int16(kmsg.Fetch), 12)

	opts := append(pinOpts(topic, id),
		kgo.MaxVersions(vs),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(50*time.Millisecond),
	)
	cl := newPlainClient(t, c, opts...)

	deadline := time.Now().Add(15 * time.Second)
	sawUnsupported := false
	for time.Now().Before(deadline) && !sawUnsupported {
		pollCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		fs := cl.PollFetches(pollCtx)
		cancel()
		for _, e := range fs.Errors() {
			if errors.Is(e.Err, kgo.ErrTopicIDPinningUnsupported) {
				sawUnsupported = true
			}
		}
	}
	if !sawUnsupported {
		t.Fatal("strict pinning did not surface ErrTopicIDPinningUnsupported on a name-only fetch protocol")
	}
}

// TestPinnedTopicValidation rejects configurations that cannot provide the
// identity guarantee up front (no network involved).
func TestPinnedTopicValidation(t *testing.T) {
	t.Parallel()
	id := [16]byte{1, 2, 3}

	if err := kgo.ValidateOpts(
		kgo.SeedBrokers("127.0.0.1:1"),
		kgo.PinTopicIDs(map[string][16]byte{"t": id}),
		kgo.AllowAutoTopicCreation(),
	); err == nil {
		t.Fatal("expected an error when pinning with auto topic creation, got nil")
	}

	if err := kgo.ValidateOpts(
		kgo.SeedBrokers("127.0.0.1:1"),
		kgo.PinTopicIDs(map[string][16]byte{"t": id}),
		kgo.ConsumeRegex(),
		kgo.ConsumeTopics(".*"),
	); err == nil {
		t.Fatal("expected an error when pinning with regex consuming, got nil")
	}

	if err := kgo.ValidateOpts(
		kgo.SeedBrokers("127.0.0.1:1"),
		kgo.PinTopicIDs(map[string][16]byte{"t": {}}),
	); err == nil {
		t.Fatal("expected an error when pinning the all-zero ID, got nil")
	}

	if err := kgo.ValidateOpts(
		kgo.SeedBrokers("127.0.0.1:1"),
		kgo.PinTopicIDs(map[string][16]byte{"t": id}),
	); err != nil {
		t.Fatalf("valid pin config rejected: %v", err)
	}
}
