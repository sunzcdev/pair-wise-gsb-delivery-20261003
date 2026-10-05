package centrifuge

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/centrifugal/protocol"
	"github.com/stretchr/testify/require"
)

// gatedHistoryBroker lets a test control when each channel's History returns,
// and make it fail: the order in which the subscriptions of a batch finish
// their recovery is then up to the test.
type gatedHistoryBroker struct {
	*MemoryBroker
	mu       sync.Mutex
	gates    map[string]chan struct{}
	entered  map[string]chan struct{}
	failures map[string]error
}

func newGatedHistoryBroker(b *MemoryBroker) *gatedHistoryBroker {
	return &gatedHistoryBroker{
		MemoryBroker: b,
		gates:        make(map[string]chan struct{}),
		entered:      make(map[string]chan struct{}),
		failures:     make(map[string]error),
	}
}

// gate makes the next History call for ch block until release is called, and
// returns a channel which is closed when that call starts waiting.
func (b *gatedHistoryBroker) gate(ch string) (entered <-chan struct{}, release func()) {
	g := make(chan struct{})
	e := make(chan struct{})
	b.mu.Lock()
	b.gates[ch] = g
	b.entered[ch] = e
	b.mu.Unlock()
	return e, func() { close(g) }
}

// fail makes the next History call for ch return err.
func (b *gatedHistoryBroker) fail(ch string, err error) {
	b.mu.Lock()
	b.failures[ch] = err
	b.mu.Unlock()
}

func (b *gatedHistoryBroker) History(ch string, opts HistoryOptions) ([]*Publication, StreamPosition, error) {
	b.mu.Lock()
	gate := b.gates[ch]
	entered := b.entered[ch]
	delete(b.gates, ch)
	delete(b.entered, ch)
	b.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if gate != nil {
		<-gate
	}
	b.mu.Lock()
	failErr := b.failures[ch]
	delete(b.failures, ch)
	b.mu.Unlock()
	if failErr != nil {
		return nil, StreamPosition{}, failErr
	}
	return b.MemoryBroker.History(ch, opts)
}

// batchTestNode returns a node with a gated history broker and presence, a
// client connected to it, and the channel of the frames the client receives.
func batchTestNode(t *testing.T) (*Node, *gatedHistoryBroker, *Client, chan []byte) {
	t.Helper()
	node, err := New(Config{ClientQueueMaxSize: 1 << 20})
	require.NoError(t, err)
	memBroker, err := NewMemoryBroker(node, MemoryBrokerConfig{})
	require.NoError(t, err)
	broker := newGatedHistoryBroker(memBroker)
	node.SetBroker(broker)
	presence, err := NewMemoryPresenceManager(node, MemoryPresenceManagerConfig{})
	require.NoError(t, err)
	node.SetPresenceManager(presence)
	require.NoError(t, node.Run())
	t.Cleanup(func() { _ = node.Shutdown(context.Background()) })

	transport := newTestTransport(func() {})
	sink := make(chan []byte, 1024)
	transport.sink = sink
	client := newTestClientCustomTransport(t, context.Background(), node, transport, "42")
	connectClientV2(t, client)
	return node, broker, client, sink
}

// batchTestFramePushes returns the pushes of a frame.
func batchTestFramePushes(t *testing.T, frame []byte) []*protocol.Push {
	t.Helper()
	var pushes []*protocol.Push
	decoder := newReplyDecoder(protocol.TypeJSON, frame)
	for {
		reply, err := decoder.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if reply.Push != nil {
			pushes = append(pushes, reply.Push)
		}
	}
	return pushes
}

// batchTestFrames returns the pushes of the frames received so far, in order.
func batchTestFrames(t *testing.T, sink chan []byte) []*protocol.Push {
	t.Helper()
	var pushes []*protocol.Push
	for {
		select {
		case frame := <-sink:
			pushes = append(pushes, batchTestFramePushes(t, frame)...)
		default:
			return pushes
		}
	}
}

// requireNoBatchFrames fails if the client receives a push within d (the
// connect reply may still be in flight and is not a push).
func requireNoBatchFrames(t *testing.T, sink chan []byte, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case frame := <-sink:
			if pushes := batchTestFramePushes(t, frame); len(pushes) > 0 {
				t.Fatalf("push received before the batch committed: %q", frame)
			}
		case <-deadline:
			return
		}
	}
}

// A batch whose channel A finished its recovery while channel B is still
// reading its history: a publication to A must not reach the client before the
// whole batch succeeded. Once it has, the client gets the subscribe pushes of
// both channels with their recovered publications first, and the publication
// which came meanwhile after them.
func TestClientSubscribeBatchSuccessOrdersPublicationsAfterBatch(t *testing.T) {
	node, broker, client, sink := batchTestNode(t)

	spA, err := node.Publish("batch_a", []byte(`{"a":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)
	spB, err := node.Publish("batch_b", []byte(`{"b":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)

	entered, release := broker.gate("batch_b")
	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch(map[string]SubscribeOptions{
			"batch_a": {EnableRecovery: true, EmitPresence: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spA.Epoch}},
			"batch_b": {EnableRecovery: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spB.Epoch}},
		})
	}()
	// B is reading its history now; A (sorted first) finished its recovery.
	<-entered

	// A new publication for A: queued in A's recovery buffer, counted against
	// the shared budget, but not written before the batch commits.
	_, err = node.Publish("batch_a", []byte(`{"a":2}`), WithHistory(10, time.Minute))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return client.pubSubSync.Held() > 0 },
		5*time.Second, time.Millisecond)
	requireNoBatchFrames(t, sink, 100*time.Millisecond)

	release()
	require.NoError(t, <-done)

	require.True(t, client.IsSubscribed("batch_a"))
	require.True(t, client.IsSubscribed("batch_b"))
	presenceResult, err := node.Presence("batch_a")
	require.NoError(t, err)
	require.Contains(t, presenceResult.Presence, client.ID())

	// The pushes go through the client's write queue, so they land in the sink
	// asynchronously.
	var pushes []*protocol.Push
	require.Eventually(t, func() bool {
		pushes = append(pushes, batchTestFrames(t, sink)...)
		return len(pushes) >= 5
	}, 5*time.Second, time.Millisecond)
	require.Len(t, pushes, 5)
	require.NotNil(t, pushes[0].Subscribe)
	require.Equal(t, "batch_a", pushes[0].Channel)
	require.NotNil(t, pushes[1].Pub)
	require.Equal(t, "batch_a", pushes[1].Channel)
	require.Equal(t, uint64(1), pushes[1].Pub.Offset)
	require.NotNil(t, pushes[2].Subscribe)
	require.Equal(t, "batch_b", pushes[2].Channel)
	require.NotNil(t, pushes[3].Pub)
	require.Equal(t, "batch_b", pushes[3].Channel)
	require.Equal(t, uint64(1), pushes[3].Pub.Offset)
	// The publication which came while B was recovering follows the whole batch.
	require.NotNil(t, pushes[4].Pub)
	require.Equal(t, "batch_a", pushes[4].Channel)
	require.Equal(t, uint64(2), pushes[4].Pub.Offset)
	require.Equal(t, `{"a":2}`, string(pushes[4].Pub.Data))
}

// A batch whose channel B fails after channel A was subscribed: everything the
// batch did for A — the hub registration, the presence entry and the recovery
// buffer — is rolled back, and the client got nothing.
func TestClientSubscribeBatchFailureRollsBack(t *testing.T) {
	node, broker, client, sink := batchTestNode(t)

	spA, err := node.Publish("batch_a", []byte(`{"a":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)
	spB, err := node.Publish("batch_b", []byte(`{"b":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)

	entered, release := broker.gate("batch_b")
	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch(map[string]SubscribeOptions{
			"batch_a": {EnableRecovery: true, EmitPresence: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spA.Epoch}},
			"batch_b": {EnableRecovery: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spB.Epoch}},
		})
	}()
	// B is reading its history now; A is already registered in the hub.
	<-entered
	require.Equal(t, 1, node.hub.NumSubscribers("batch_a"))

	broker.fail("batch_b", errors.New("history failure"))
	release()
	require.Error(t, <-done)

	// Everything rolled back: no subscriptions, no hub registrations, no
	// presence, no recovery buffers.
	require.Empty(t, client.Channels())
	require.Equal(t, 0, node.hub.NumSubscribers("batch_a"))
	require.Equal(t, 0, node.hub.NumSubscribers("batch_b"))
	presenceResult, err := node.Presence("batch_a")
	require.NoError(t, err)
	require.Empty(t, presenceResult.Presence)
	require.False(t, client.pubSubSync.Buffering())
	require.Zero(t, client.pubSubSync.Held())
	require.Empty(t, batchTestFrames(t, sink))

	// Publications to A after the rollback are not delivered.
	_, err = node.Publish("batch_a", []byte(`{"a":2}`), WithHistory(10, time.Minute))
	require.NoError(t, err)
	requireNoBatchFrames(t, sink, 100*time.Millisecond)
}

// A batch which contains a channel the client is already subscribed to is
// rejected before it did any work; so is a single-channel subscribe racing an
// in-flight batch on one of its channels.
func TestClientSubscribeBatchDuplicateChannel(t *testing.T) {
	node, broker, client, _ := batchTestNode(t)

	require.NoError(t, client.Subscribe("batch_a"))
	err := client.SubscribeBatch(map[string]SubscribeOptions{
		"batch_a": {},
		"batch_b": {},
	})
	require.ErrorIs(t, err, ErrorAlreadySubscribed)
	require.False(t, client.IsSubscribed("batch_b"))
	require.Equal(t, 0, node.hub.NumSubscribers("batch_b"))

	// A single-channel subscribe racing an in-flight batch loses to the
	// batch's reservation, on every channel of the batch.
	spC, err := node.Publish("batch_c", []byte(`{"c":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)
	entered, release := broker.gate("batch_c")
	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch(map[string]SubscribeOptions{
			"batch_c": {EnableRecovery: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spC.Epoch}},
			"batch_d": {},
		})
	}()
	<-entered
	require.ErrorIs(t, client.Subscribe("batch_c"), ErrorAlreadySubscribed)
	require.ErrorIs(t, client.Subscribe("batch_d"), ErrorAlreadySubscribed)
	release()
	require.NoError(t, <-done)
	require.True(t, client.IsSubscribed("batch_c"))
	require.True(t, client.IsSubscribed("batch_d"))
}

// An unsubscribe racing a batch waits for the batch's commit point: it runs
// once the batch committed, and unsubscribes exactly its channel.
func TestClientSubscribeBatchConcurrentUnsubscribe(t *testing.T) {
	node, broker, client, _ := batchTestNode(t)

	spA, err := node.Publish("batch_a", []byte(`{"a":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)
	spB, err := node.Publish("batch_b", []byte(`{"b":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)

	entered, release := broker.gate("batch_b")
	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch(map[string]SubscribeOptions{
			"batch_a": {EnableRecovery: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spA.Epoch}},
			"batch_b": {EnableRecovery: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spB.Epoch}},
		})
	}()
	<-entered

	unsubscribed := make(chan struct{})
	go func() {
		client.Unsubscribe("batch_a")
		close(unsubscribed)
	}()
	// The unsubscribe waits for the in-flight batch.
	select {
	case <-unsubscribed:
		t.Fatal("unsubscribe did not wait for the batch")
	case <-time.After(100 * time.Millisecond):
	}

	release()
	require.NoError(t, <-done)
	<-unsubscribed
	require.False(t, client.IsSubscribed("batch_a"))
	require.True(t, client.IsSubscribed("batch_b"))
	require.Equal(t, 0, node.hub.NumSubscribers("batch_a"))
	require.Equal(t, 1, node.hub.NumSubscribers("batch_b"))
}

// A client closed while its batch subscribes: the batch rolls back at its
// commit point, nothing is subscribed and no recovery buffer leaks.
func TestClientSubscribeBatchClientClosed(t *testing.T) {
	node, broker, client, _ := batchTestNode(t)

	spA, err := node.Publish("batch_a", []byte(`{"a":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)
	spB, err := node.Publish("batch_b", []byte(`{"b":1}`), WithHistory(10, time.Minute))
	require.NoError(t, err)

	entered, release := broker.gate("batch_b")
	done := make(chan error, 1)
	go func() {
		done <- client.SubscribeBatch(map[string]SubscribeOptions{
			"batch_a": {EnableRecovery: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spA.Epoch}},
			"batch_b": {EnableRecovery: true, RecoverSince: &StreamPosition{Offset: 0, Epoch: spB.Epoch}},
		})
	}()
	<-entered

	closeClientAndWait(client)
	release()
	require.Error(t, <-done)

	require.Eventually(t, func() bool {
		return len(client.Channels()) == 0 &&
			node.hub.NumSubscribers("batch_a") == 0 &&
			node.hub.NumSubscribers("batch_b") == 0 &&
			!client.pubSubSync.Buffering() &&
			client.pubSubSync.Held() == 0
	}, 5*time.Second, time.Millisecond)
}
