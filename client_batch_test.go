package centrifuge

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/centrifugal/protocol"
	"github.com/stretchr/testify/require"
)

// gatedHistoryBroker lets a test hold back the history read of chosen channels
// until it releases the gate, and fail the read with the gate's error. It gives
// a test control over the order in which the subscriptions of a batch finish
// their recovery.
type gatedHistoryBroker struct {
	*MemoryBroker
	mu    sync.Mutex
	gates map[string]*historyGate
}

type historyGate struct {
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
	err         error
}

func newHistoryGate() *historyGate {
	return &historyGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *gatedHistoryBroker) setGate(ch string, gate *historyGate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gates == nil {
		b.gates = make(map[string]*historyGate)
	}
	b.gates[ch] = gate
}

func (b *gatedHistoryBroker) History(ch string, opts HistoryOptions) ([]*Publication, StreamPosition, error) {
	b.mu.Lock()
	gate := b.gates[ch]
	b.mu.Unlock()
	if gate != nil {
		gate.enteredOnce.Do(func() { close(gate.entered) })
		<-gate.release
		if gate.err != nil {
			return nil, StreamPosition{}, gate.err
		}
	}
	return b.MemoryBroker.History(ch, opts)
}

// newBatchTestNode returns a node with a gatedHistoryBroker, and publishes
// numPubs publications with history to each of the channels, returning their
// epochs.
func newBatchTestNode(t *testing.T, channels []string, numPubs int) (*Node, *gatedHistoryBroker, map[string]string) {
	t.Helper()
	node, err := New(Config{
		LogLevel:   LogLevelTrace,
		LogHandler: func(entry LogEntry) {},
	})
	require.NoError(t, err)
	memBroker, err := NewMemoryBroker(node, MemoryBrokerConfig{})
	require.NoError(t, err)
	broker := &gatedHistoryBroker{MemoryBroker: memBroker}
	node.SetBroker(broker)
	require.NoError(t, node.Run())
	t.Cleanup(func() { _ = node.Shutdown(context.Background()) })

	epochs := make(map[string]string, len(channels))
	for _, ch := range channels {
		for i := 0; i < numPubs; i++ {
			res, err := node.Publish(ch, []byte(`{"n":1}`), WithHistory(100, time.Minute))
			require.NoError(t, err)
			epochs[ch] = res.StreamPosition.Epoch
		}
	}
	return node, broker, epochs
}

// batchFrameCollector collects everything the transport receives.
type batchFrameCollector struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *batchFrameCollector) collect(sink chan []byte) {
	for frame := range sink {
		c.mu.Lock()
		c.frames = append(c.frames, frame)
		c.mu.Unlock()
	}
}

func (c *batchFrameCollector) get() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.frames...)
}

// batchEvent is one push of the batch on the wire: a subscribe result or a
// publication of a channel.
type batchEvent struct {
	channel string
	kind    string // "subscribe" or "pub".
	offset  uint64
}

// batchEvents decodes the pushes of the collected frames in wire order.
func batchEvents(frames [][]byte) []batchEvent {
	var events []batchEvent
	for _, frame := range frames {
		decoder := newReplyDecoder(protocol.TypeJSON, frame)
		for {
			reply, err := decoder.Decode()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil || reply.Push == nil {
				continue
			}
			switch {
			case reply.Push.Subscribe != nil:
				events = append(events, batchEvent{channel: reply.Push.Channel, kind: "subscribe"})
			case reply.Push.Pub != nil:
				events = append(events, batchEvent{channel: reply.Push.Channel, kind: "pub", offset: reply.Push.Pub.Offset})
			}
		}
	}
	return events
}

func newBatchTestClient(t *testing.T, node *Node, collector *batchFrameCollector) *Client {
	t.Helper()
	transport := newTestTransport(func() {})
	sink := make(chan []byte, 1024)
	transport.sink = sink
	go collector.collect(sink)
	client := newTestClientCustomTransport(t, context.Background(), node, transport, "42")
	connectClientV2(t, client)
	return client
}

func recoverFromStartOpts(epoch string) SubscribeOptions {
	return SubscribeOptions{
		EnableRecovery: true,
		EmitPresence:   true,
		RecoverSince:   &StreamPosition{Offset: 0, Epoch: epoch},
	}
}

// A batch whose channels recover at their own pace: while one channel is still
// reading its history, a publication of an already-recovered channel must not
// overtake the success reply of the group. Once every channel recovered, the
// client gets every subscribe result first, and the queued publication after
// them.
func TestClientSubscribeBatchRecoveryOrdering(t *testing.T) {
	setIsInTest(t)

	chA := testChannelRecoveryOrderingPrefix + "_batch_a"
	chB := "batch_b"
	node, broker, epochs := newBatchTestNode(t, []string{chA, chB}, 3)

	// Hold back the history read of B, so that A recovers first.
	gateB := newHistoryGate()
	broker.setGate(chB, gateB)

	aSynced := make(chan struct{})
	var aSyncedOnce sync.Once
	setTestAtSyncPoint(t, func(channel string) {
		if channel == chA {
			aSyncedOnce.Do(func() { close(aSynced) })
		}
	})

	var collector batchFrameCollector
	client := newBatchTestClient(t, node, &collector)

	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch([]BatchSubscribeRequest{
			{Channel: chA, Options: recoverFromStartOpts(epochs[chA])},
			{Channel: chB, Options: recoverFromStartOpts(epochs[chB])},
		})
	}()

	// A passed its sync point (its recovery is done, its buffer queues the
	// publications which come now), and B is still reading its history.
	select {
	case <-aSynced:
	case <-time.After(5 * time.Second):
		require.Fail(t, "channel A did not reach its sync point")
	}
	select {
	case <-gateB.entered:
	case <-time.After(5 * time.Second):
		require.Fail(t, "channel B did not start reading its history")
	}

	// A new publication of the already-recovered channel.
	_, err := node.Publish(chA, []byte(`{"n":2}`), WithHistory(100, time.Minute))
	require.NoError(t, err)

	// It must not be written before the success reply of the whole group.
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, batchEvents(collector.get()), "nothing of the group may reach the client before the whole group replies")

	close(gateB.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "batch subscribe did not return")
	}

	// Wait for the queued publication of A – the last thing the batch writes.
	var events []batchEvent
	require.Eventually(t, func() bool {
		events = batchEvents(collector.get())
		for _, e := range events {
			if e.channel == chA && e.kind == "pub" && e.offset == 4 {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	// Every subscribe result (with its recovered publications) comes before the
	// publication which arrived while the group was subscribing.
	require.Equal(t, []batchEvent{
		{channel: chA, kind: "subscribe"},
		{channel: chA, kind: "pub", offset: 1},
		{channel: chA, kind: "pub", offset: 2},
		{channel: chA, kind: "pub", offset: 3},
		{channel: chB, kind: "subscribe"},
		{channel: chB, kind: "pub", offset: 1},
		{channel: chB, kind: "pub", offset: 2},
		{channel: chB, kind: "pub", offset: 3},
		{channel: chA, kind: "pub", offset: 4},
	}, events)

	require.True(t, client.IsSubscribed(chA))
	require.True(t, client.IsSubscribed(chB))
	for _, ch := range []string{chA, chB} {
		pres, err := node.Presence(ch)
		require.NoError(t, err)
		require.Contains(t, pres.Presence, client.ID())
	}
}

// A batch whose last channel fails: the hub registration, the presence entry
// and the recovery buffer of the already-recovered channel are rolled back,
// and the client gets nothing of the group.
func TestClientSubscribeBatchFailureRollsBack(t *testing.T) {
	setIsInTest(t)

	chA := testChannelRecoveryOrderingPrefix + "_batch_fail_a"
	chB := "batch_fail_b"
	node, broker, epochs := newBatchTestNode(t, []string{chA, chB}, 3)

	// B fails its history read once it gets to it.
	gateB := newHistoryGate()
	gateB.err = ErrorLimitExceeded
	broker.setGate(chB, gateB)

	aSynced := make(chan struct{})
	var aSyncedOnce sync.Once
	setTestAtSyncPoint(t, func(channel string) {
		if channel == chA {
			aSyncedOnce.Do(func() { close(aSynced) })
		}
	})

	var collector batchFrameCollector
	client := newBatchTestClient(t, node, &collector)

	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch([]BatchSubscribeRequest{
			{Channel: chA, Options: recoverFromStartOpts(epochs[chA])},
			{Channel: chB, Options: recoverFromStartOpts(epochs[chB])},
		})
	}()

	select {
	case <-aSynced:
	case <-time.After(5 * time.Second):
		require.Fail(t, "channel A did not reach its sync point")
	}
	select {
	case <-gateB.entered:
	case <-time.After(5 * time.Second):
		require.Fail(t, "channel B did not start reading its history")
	}

	// A publication of the already-recovered channel, queued in its buffer.
	_, err := node.Publish(chA, []byte(`{"n":2}`), WithHistory(100, time.Minute))
	require.NoError(t, err)

	close(gateB.release)
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrorLimitExceeded)
	case <-time.After(5 * time.Second):
		require.Fail(t, "batch subscribe did not return")
	}

	// The whole group is rolled back: no subscriptions, no hub registrations,
	// no presence entries, no recovery buffers, nothing on the wire.
	require.False(t, client.IsSubscribed(chA))
	require.False(t, client.IsSubscribed(chB))
	require.Empty(t, client.channels)
	require.Equal(t, 0, node.hub.NumSubscribers(chA))
	require.Equal(t, 0, node.hub.NumSubscribers(chB))
	for _, ch := range []string{chA, chB} {
		pres, err := node.Presence(ch)
		require.NoError(t, err)
		require.Empty(t, pres.Presence)
	}
	require.False(t, client.pubSubSync.Buffering(), "recovery buffer leaked")
	require.Zero(t, client.pubSubSync.Held(), "recovery buffer limit still held")
	require.Empty(t, batchEvents(collector.get()), "nothing of the failed group may reach the client")

	client.mu.RLock()
	status := client.status
	client.mu.RUnlock()
	require.NotEqual(t, statusClosed, status, "a failed batch must not close the client")

	// The client can subscribe again and gets publications.
	require.NoError(t, client.Subscribe(chA, WithRecovery(true)))
	_, err = node.Publish(chA, []byte(`{"n":3}`), WithHistory(100, time.Minute))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		for _, e := range batchEvents(collector.get()) {
			if e.channel == chA && e.kind == "pub" && e.offset == 5 {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
}

// Invalid batches fail before any work is done.
func TestClientSubscribeBatchValidation(t *testing.T) {
	node := defaultNodeNoHandlers()
	defer func() { _ = node.Shutdown(context.Background()) }()
	client := newTestClient(t, node, "42")
	connectClientV2(t, client)

	// An empty batch is a no-op.
	require.NoError(t, client.SubscribeBatch(nil))

	// An empty channel fails the batch.
	require.Error(t, client.SubscribeBatch([]BatchSubscribeRequest{{Channel: ""}}))

	// Duplicate channels fail the batch before anything is reserved.
	err := client.SubscribeBatch([]BatchSubscribeRequest{{Channel: "a"}, {Channel: "a"}})
	require.Error(t, err)
	require.Empty(t, client.channels)

	// A channel the client is already subscribed to fails the whole batch:
	// the other channel of the batch is not subscribed either.
	require.NoError(t, client.Subscribe("existing"))
	err = client.SubscribeBatch([]BatchSubscribeRequest{{Channel: "new"}, {Channel: "existing"}})
	require.ErrorIs(t, err, ErrorAlreadySubscribed)
	require.False(t, client.IsSubscribed("new"))
	require.True(t, client.IsSubscribed("existing"))

	// A closed client reports nothing, as Client.Subscribe does.
	closedClient := newTestClient(t, node, "43")
	connectClientV2(t, closedClient)
	closeClientAndWait(closedClient)
	require.NoError(t, closedClient.SubscribeBatch([]BatchSubscribeRequest{{Channel: "x"}}))
}

// An unsubscribe of one channel of an in-flight batch waits for the commit
// point of the group, and then unsubscribes exactly that channel.
func TestClientSubscribeBatchConcurrentUnsubscribe(t *testing.T) {
	setIsInTest(t)

	chA := testChannelRecoveryOrderingPrefix + "_batch_unsub_a"
	chB := "batch_unsub_b"
	node, broker, epochs := newBatchTestNode(t, []string{chA, chB}, 3)

	gateB := newHistoryGate()
	broker.setGate(chB, gateB)

	aSynced := make(chan struct{})
	var aSyncedOnce sync.Once
	setTestAtSyncPoint(t, func(channel string) {
		if channel == chA {
			aSyncedOnce.Do(func() { close(aSynced) })
		}
	})

	var collector batchFrameCollector
	client := newBatchTestClient(t, node, &collector)

	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch([]BatchSubscribeRequest{
			{Channel: chA, Options: recoverFromStartOpts(epochs[chA])},
			{Channel: chB, Options: recoverFromStartOpts(epochs[chB])},
		})
	}()

	select {
	case <-aSynced:
	case <-time.After(5 * time.Second):
		require.Fail(t, "channel A did not reach its sync point")
	}
	select {
	case <-gateB.entered:
	case <-time.After(5 * time.Second):
		require.Fail(t, "channel B did not start reading its history")
	}

	// The unsubscribe waits for the commit point of the group.
	unsubDone := make(chan struct{})
	go func() {
		client.Unsubscribe(chA)
		close(unsubDone)
	}()
	select {
	case <-unsubDone:
		require.Fail(t, "unsubscribe did not wait for the batch")
	case <-time.After(200 * time.Millisecond):
	}

	close(gateB.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "batch subscribe did not return")
	}
	select {
	case <-unsubDone:
	case <-time.After(5 * time.Second):
		require.Fail(t, "unsubscribe did not finish after the batch committed")
	}

	// The batch committed both channels, then the unsubscribe removed its own.
	require.False(t, client.IsSubscribed(chA))
	require.True(t, client.IsSubscribed(chB))
	require.Equal(t, 0, node.hub.NumSubscribers(chA))
	require.Equal(t, 1, node.hub.NumSubscribers(chB))
}

// A client which closes while its batch is subscribing: the whole group is
// rolled back at the commit point, and close() – which waits for the in-flight
// subscriptions – completes.
func TestClientSubscribeBatchClientCloses(t *testing.T) {
	setIsInTest(t)

	chA := testChannelRecoveryOrderingPrefix + "_batch_close_a"
	chB := "batch_close_b"
	node, broker, epochs := newBatchTestNode(t, []string{chA, chB}, 3)

	gateB := newHistoryGate()
	broker.setGate(chB, gateB)

	aSynced := make(chan struct{})
	var aSyncedOnce sync.Once
	setTestAtSyncPoint(t, func(channel string) {
		if channel == chA {
			aSyncedOnce.Do(func() { close(aSynced) })
		}
	})

	var collector batchFrameCollector
	client := newBatchTestClient(t, node, &collector)

	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch([]BatchSubscribeRequest{
			{Channel: chA, Options: recoverFromStartOpts(epochs[chA])},
			{Channel: chB, Options: recoverFromStartOpts(epochs[chB])},
		})
	}()

	select {
	case <-aSynced:
	case <-time.After(5 * time.Second):
		require.Fail(t, "channel A did not reach its sync point")
	}
	select {
	case <-gateB.entered:
	case <-time.After(5 * time.Second):
		require.Fail(t, "channel B did not start reading its history")
	}

	// Close the client while the batch is subscribing. close() marks the client
	// closed and then waits for the in-flight subscriptions of the batch.
	closeDone := make(chan struct{})
	go func() {
		_ = client.close(DisconnectForceNoReconnect)
		close(closeDone)
	}()
	require.Eventually(t, func() bool {
		client.mu.RLock()
		defer client.mu.RUnlock()
		return client.status == statusClosed
	}, 5*time.Second, time.Millisecond)

	close(gateB.release)
	select {
	case err := <-done:
		require.Error(t, err, "the batch of a closed client must not commit")
	case <-time.After(5 * time.Second):
		require.Fail(t, "batch subscribe did not return")
	}
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		require.Fail(t, "close did not finish after the batch rolled back")
	}

	// The whole group is rolled back.
	require.Empty(t, client.channels)
	require.Equal(t, 0, node.hub.NumSubscribers(chA))
	require.Equal(t, 0, node.hub.NumSubscribers(chB))
	require.False(t, client.pubSubSync.Buffering(), "recovery buffer leaked")
	require.Zero(t, client.pubSubSync.Held(), "recovery buffer limit still held")
}

// The recovery buffers of the channels of a batch share the per-client limit
// (ClientQueueMaxSize), as the subscriptions made at connect time do.
func TestClientSubscribeBatchQueueLimitShared(t *testing.T) {
	setIsInTest(t)

	prefix := testChannelRecoveryOrderingPrefix + "_batch_shared_limit"
	chA := prefix + "_a"
	chB := prefix + "_b"

	node, err := New(Config{
		LogLevel:           LogLevelTrace,
		LogHandler:         func(entry LogEntry) {},
		ClientQueueMaxSize: 2000,
	})
	require.NoError(t, err)
	require.NoError(t, node.Run())
	defer func() { _ = node.Shutdown(context.Background()) }()

	// About 1300 bytes a publication, one queued for each channel at its sync
	// point: one fits into the limit of the buffers, two don't.
	data := []byte(`{"data":"` + strings.Repeat("x", 1200) + `"}`)
	setTestAtSyncPoint(t, func(channel string) {
		if !strings.HasPrefix(channel, prefix) {
			return
		}
		if _, err := node.Publish(channel, data, WithHistory(1000, time.Minute)); err != nil {
			t.Error(err)
		}
	})

	positions := make(map[string]*StreamPosition, 2)
	for _, ch := range []string{chA, chB} {
		res, err := node.Publish(ch, data, WithHistory(1000, time.Minute))
		require.NoError(t, err)
		sp := res.StreamPosition
		positions[ch] = &sp
	}

	transport := newTestTransport(func() {})
	client := newTestClientCustomTransport(t, context.Background(), node, transport, "42")
	connectClientV2(t, client)

	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch([]BatchSubscribeRequest{
			{Channel: chA, Options: SubscribeOptions{EnableRecovery: true, RecoverSince: positions[chA]}},
			{Channel: chB, Options: SubscribeOptions{EnableRecovery: true, RecoverSince: positions[chB]}},
		})
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "batch subscribe did not return")
	}

	// The queued publications of the two channels together exceed the limit of
	// the buffers, so the client is told to resubscribe (a server-side
	// subscription disconnects with insufficient state).
	select {
	case <-transport.closeCh:
		require.Equal(t, DisconnectInsufficientState.Code, transport.disconnect.Code)
	case <-time.After(5 * time.Second):
		require.Fail(t, "client not disconnected after the overflow")
	}
}
