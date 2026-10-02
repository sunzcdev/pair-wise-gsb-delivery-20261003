package centrifuge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/centrifugal/protocol"
	"github.com/stretchr/testify/require"
)

func TestSessionOwnershipKey(t *testing.T) {
	t.Parallel()
	// Length-prefixing must keep distinct (user, device) pairs distinct.
	require.NotEqual(t, sessionOwnershipKey("a|b", "c"), sessionOwnershipKey("a", "b|c"))
	require.Equal(t, sessionOwnershipKey("u", "d"), sessionOwnershipKey("u", "d"))
}

func TestMemorySessionOwnershipStore(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionOwnershipStore()
	now := time.Now()
	store.now = func() time.Time { return now }

	ctx := context.Background()
	key := sessionOwnershipKey("u", "d")

	gen1, err := store.Acquire(ctx, key, "owner-a", time.Minute)
	require.NoError(t, err)
	require.Equal(t, uint64(1), gen1)

	ok, err := store.Validate(ctx, key, "owner-a", gen1)
	require.NoError(t, err)
	require.True(t, ok)

	// Takeover while the previous lease is still live: the new owner gets a
	// higher generation and the old one immediately loses eligibility.
	gen2, err := store.Acquire(ctx, key, "owner-b", time.Minute)
	require.NoError(t, err)
	require.Equal(t, uint64(2), gen2)

	ok, err = store.Validate(ctx, key, "owner-a", gen1)
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = store.Validate(ctx, key, "owner-b", gen2)
	require.NoError(t, err)
	require.True(t, ok)

	// The old owner can neither renew nor affect the new owner's record.
	require.ErrorIs(t, store.Renew(ctx, key, "owner-a", gen1, time.Minute), ErrSessionOwnershipLost)
	require.NoError(t, store.Renew(ctx, key, "owner-b", gen2, time.Minute))
	require.NoError(t, store.Release(ctx, key, "owner-a", gen1))
	ok, err = store.Validate(ctx, key, "owner-b", gen2)
	require.NoError(t, err)
	require.True(t, ok)

	// Releasing the current generation invalidates it...
	require.NoError(t, store.Release(ctx, key, "owner-b", gen2))
	ok, err = store.Validate(ctx, key, "owner-b", gen2)
	require.NoError(t, err)
	require.False(t, ok)

	// ...but generations stay monotonic across the release.
	gen3, err := store.Acquire(ctx, key, "owner-c", time.Minute)
	require.NoError(t, err)
	require.Equal(t, uint64(3), gen3)

	// A foreign owner can never validate, renew or release.
	ok, err = store.Validate(ctx, key, "owner-b", gen3)
	require.NoError(t, err)
	require.False(t, ok)
	require.ErrorIs(t, store.Renew(ctx, key, "owner-b", gen3, time.Minute), ErrSessionOwnershipLost)
	require.NoError(t, store.Release(ctx, key, "owner-b", gen3))
	ok, err = store.Validate(ctx, key, "owner-c", gen3)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestMemorySessionOwnershipStoreExpiry(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionOwnershipStore()
	now := time.Now()
	store.now = func() time.Time { return now }

	ctx := context.Background()
	key := sessionOwnershipKey("u", "d")

	gen1, err := store.Acquire(ctx, key, "owner-a", 10*time.Second)
	require.NoError(t, err)

	// The owner crashes: no renewal, no release. The lease expires.
	now = now.Add(11 * time.Second)

	ok, err := store.Validate(ctx, key, "owner-a", gen1)
	require.NoError(t, err)
	require.False(t, ok)
	require.ErrorIs(t, store.Renew(ctx, key, "owner-a", gen1, 10*time.Second), ErrSessionOwnershipLost)

	// A new owner takes over with a higher generation.
	gen2, err := store.Acquire(ctx, key, "owner-b", 10*time.Second)
	require.NoError(t, err)
	require.Greater(t, gen2, gen1)
	ok, err = store.Validate(ctx, key, "owner-b", gen2)
	require.NoError(t, err)
	require.True(t, ok)
}

// Two nodes acquire ownership of the same key at the same time: both
// acquisitions overlap inside the store, exactly one of them wins the final
// record, and only the winner's generation validates afterwards.
func TestSharedSessionOwnershipStoreConcurrentAcquire(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	ctx := context.Background()
	key := sessionOwnershipKey("u", "d")

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	store.SetAcquireHook(func(context.Context, string) error {
		arrived <- struct{}{}
		<-release
		return nil
	})

	owners := []string{"node-a-owner", "node-b-owner"}
	gens := make([]uint64, len(owners))
	errs := make([]error, len(owners))
	var wg sync.WaitGroup
	for i := range owners {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gens[i], errs[i] = store.Acquire(ctx, key, owners[i], time.Minute)
		}(i)
	}

	for i := 0; i < len(owners); i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for concurrent acquisitions")
		}
	}
	close(release)
	wg.Wait()

	for i := range owners {
		require.NoError(t, errs[i])
	}
	// Both took over each other: generations are exactly {1, 2}.
	require.ElementsMatch(t, []uint64{1, 2}, gens)

	// Only the owner of the higher generation validates and renews.
	loserIdx := 0
	if gens[1] == 1 {
		loserIdx = 1
	}
	winnerIdx := 1 - loserIdx
	ok, err := store.Validate(ctx, key, owners[winnerIdx], gens[winnerIdx])
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.Validate(ctx, key, owners[loserIdx], gens[loserIdx])
	require.NoError(t, err)
	require.False(t, ok)
	require.ErrorIs(t, store.Renew(ctx, key, owners[loserIdx], gens[loserIdx], time.Minute), ErrSessionOwnershipLost)
	require.NoError(t, store.Renew(ctx, key, owners[winnerIdx], gens[winnerIdx], time.Minute))
}

func testSessionOwnershipConfig(store SessionOwnershipStore) SessionOwnershipConfig {
	return SessionOwnershipConfig{
		Store:          store,
		AcquireTimeout: time.Second,
		LeaseTTL:       500 * time.Millisecond,
		RenewInterval:  50 * time.Millisecond,
	}
}

func newSessionOwnershipNode(t *testing.T, config SessionOwnershipConfig) *Node {
	t.Helper()
	n, err := New(Config{
		LogLevel:         LogLevelTrace,
		LogHandler:       func(entry LogEntry) {},
		SessionOwnership: config,
	})
	require.NoError(t, err)
	n.OnConnect(func(client *Client) {
		client.OnSubscribe(func(e SubscribeEvent, cb SubscribeCallback) {
			cb(SubscribeReply{}, nil)
		})
		client.OnPublish(func(e PublishEvent, cb PublishCallback) {
			cb(PublishReply{}, nil)
		})
	})
	require.NoError(t, n.Run())
	t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
	return n
}

func newSessionOwnershipClient(t *testing.T, node *Node, userID string, deviceID string) *Client {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	t.Cleanup(cancelFn)
	transport := newTestTransport(cancelFn)
	transport.setProtocolVersion(ProtocolVersion2)
	newCtx := SetCredentials(ctx, &Credentials{UserID: userID, DeviceID: deviceID})
	client, err := newClient(newCtx, node, transport)
	require.NoError(t, err)
	return client
}

func connectSessionOwnershipClient(client *Client) error {
	rwWrapper := testReplyWriterWrapper()
	if err := client.connectCmd(&protocol.ConnectRequest{}, &protocol.Command{}, time.Now(), rwWrapper.rw); err != nil {
		return err
	}
	client.triggerConnect()
	client.scheduleOnConnectTimers()
	return nil
}

func requireSubscribeDenied(t *testing.T, client *Client, ch string, expected error) {
	t.Helper()
	rwWrapper := testReplyWriterWrapper()
	err := client.handleSubscribe(&protocol.SubscribeRequest{Channel: ch}, &protocol.Command{Id: 1}, time.Now(), rwWrapper.rw)
	require.ErrorIs(t, err, expected)
}

func requirePublishDenied(t *testing.T, client *Client, ch string, expected error) {
	t.Helper()
	rwWrapper := testReplyWriterWrapper()
	err := client.handlePublish(&protocol.PublishRequest{Channel: ch, Data: []byte(`{}`)}, &protocol.Command{Id: 1}, time.Now(), rwWrapper.rw)
	require.ErrorIs(t, err, expected)
}

func requirePublishOK(t *testing.T, client *Client, ch string) {
	t.Helper()
	rwWrapper := testReplyWriterWrapper()
	err := client.handlePublish(&protocol.PublishRequest{Channel: ch, Data: []byte(`{}`)}, &protocol.Command{Id: 1}, time.Now(), rwWrapper.rw)
	require.NoError(t, err)
	require.Len(t, rwWrapper.replies, 1)
	require.Nil(t, rwWrapper.replies[0].Error)
}

// A mobile client switches networks: its old connection is still hanging on
// node 1 while the new connection lands on node 2. The new connection takes
// the session over with a higher generation; the old connection can no longer
// subscribe or publish and is disconnected once its renewal reaches the store.
func TestSessionOwnershipTakeoverAcrossNodes(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	config := testSessionOwnershipConfig(store)
	node1 := newSessionOwnershipNode(t, config)
	node2 := newSessionOwnershipNode(t, config)

	client1 := newSessionOwnershipClient(t, node1, "u", "d")
	require.NoError(t, connectSessionOwnershipClient(client1))
	disconnected1 := make(chan Disconnect, 1)
	client1.OnDisconnect(func(e DisconnectEvent) { disconnected1 <- e.Disconnect })

	// The old connection works before the takeover.
	subscribeClientV2(t, client1, "test")
	requirePublishOK(t, client1, "test")

	// The new connection on the other node takes the session over.
	client2 := newSessionOwnershipClient(t, node2, "u", "d")
	require.NoError(t, connectSessionOwnershipClient(client2))
	require.Greater(t, client2.ownership.generation, client1.ownership.generation)

	// The old connection immediately loses its operation eligibility, even
	// though its socket is still open.
	requireSubscribeDenied(t, client1, "test2", ErrorSessionTakenOver)
	requirePublishDenied(t, client1, "test", ErrorSessionTakenOver)

	// The new connection can subscribe and publish.
	subscribeClientV2(t, client2, "test")
	requirePublishOK(t, client2, "test")

	// The old connection is disconnected once its next renewal reaches the
	// store and finds the newer generation.
	select {
	case d := <-disconnected1:
		require.Equal(t, DisconnectSessionTakenOver.Code, d.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the old connection to be disconnected")
	}
}

// Two connections of the same user and device connect to two nodes at the same
// time. Both acquisitions overlap inside the shared store; exactly one
// connection wins the final record and stays eligible, the loser is
// disconnected with DisconnectSessionTakenOver.
func TestSessionOwnershipSimultaneousTakeoverAcrossNodes(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	config := testSessionOwnershipConfig(store)
	node1 := newSessionOwnershipNode(t, config)
	node2 := newSessionOwnershipNode(t, config)

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	store.SetAcquireHook(func(context.Context, string) error {
		arrived <- struct{}{}
		<-release
		return nil
	})
	defer store.SetAcquireHook(nil)

	client1 := newSessionOwnershipClient(t, node1, "u", "d")
	client2 := newSessionOwnershipClient(t, node2, "u", "d")

	errCh := make(chan error, 2)
	go func() { errCh <- connectSessionOwnershipClient(client1) }()
	go func() { errCh <- connectSessionOwnershipClient(client2) }()

	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for concurrent acquisitions")
		}
	}
	close(release)
	require.NoError(t, <-errCh)
	require.NoError(t, <-errCh)

	// Both acquisitions succeeded, the generations are exactly {1, 2} and
	// only the higher one is still eligible.
	gen1 := client1.ownership.generation
	gen2 := client2.ownership.generation
	require.ElementsMatch(t, []uint64{1, 2}, []uint64{gen1, gen2})

	key := sessionOwnershipKey("u", "d")
	ok1, err := store.Validate(context.Background(), key, client1.ID(), gen1)
	require.NoError(t, err)
	ok2, err := store.Validate(context.Background(), key, client2.ID(), gen2)
	require.NoError(t, err)
	require.NotEqual(t, ok1, ok2, "exactly one connection may stay eligible")
	if gen1 > gen2 {
		require.True(t, ok1)
	} else {
		require.True(t, ok2)
	}

	// The loser is disconnected with DisconnectSessionTakenOver.
	disconnected := make(chan Disconnect, 2)
	client1.OnDisconnect(func(e DisconnectEvent) { disconnected <- e.Disconnect })
	client2.OnDisconnect(func(e DisconnectEvent) { disconnected <- e.Disconnect })
	select {
	case d := <-disconnected:
		require.Equal(t, DisconnectSessionTakenOver.Code, d.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the losing connection to be disconnected")
	}
	// The winner stays connected.
	select {
	case d := <-disconnected:
		t.Fatalf("both connections were disconnected, second: %s", d)
	case <-time.After(300 * time.Millisecond):
	}
}

// The old connection's disconnect arrives after the takeover completed. Its
// late, generation-guarded release must not remove the new owner's record.
func TestSessionOwnershipLateDisconnect(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	config := testSessionOwnershipConfig(store)
	node1 := newSessionOwnershipNode(t, config)
	node2 := newSessionOwnershipNode(t, config)

	client1 := newSessionOwnershipClient(t, node1, "u", "d")
	require.NoError(t, connectSessionOwnershipClient(client1))

	// The new connection takes the session over on the other node.
	client2 := newSessionOwnershipClient(t, node2, "u", "d")
	require.NoError(t, connectSessionOwnershipClient(client2))

	// Only now does the old connection's close run: its release carries the
	// stale generation and must be a no-op in the store.
	client1.Disconnect(DisconnectConnectionClosed)

	ok, err := store.Validate(context.Background(), client2.ownership.key, client2.ID(), client2.ownership.generation)
	require.NoError(t, err)
	require.True(t, ok, "late release of the old generation must not remove the new owner's record")

	// The new connection still works.
	subscribeClientV2(t, client2, "test")
	requirePublishOK(t, client2, "test")
}

// The ownership store is slow or down: the acquisition does not complete
// within the configured timeout and the connect fails with a dedicated
// disconnect instead of hanging or succeeding without a lease.
func TestSessionOwnershipAcquireTimeout(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	store.SetAcquireHook(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})
	config := testSessionOwnershipConfig(store)
	config.AcquireTimeout = 100 * time.Millisecond
	node := newSessionOwnershipNode(t, config)

	client := newSessionOwnershipClient(t, node, "u", "d")
	rwWrapper := testReplyWriterWrapper()
	started := time.Now()
	err := client.connectCmd(&protocol.ConnectRequest{}, &protocol.Command{}, time.Now(), rwWrapper.rw)
	require.Equal(t, DisconnectSessionOwnershipUnavailable, err)
	require.Less(t, time.Since(started), 5*time.Second)
}

// The ownership store starts failing renewals: the connection is disconnected
// with a clear error instead of keeping an unconfirmed lease.
func TestSessionOwnershipRenewStoreError(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	config := testSessionOwnershipConfig(store)
	node := newSessionOwnershipNode(t, config)

	client := newSessionOwnershipClient(t, node, "u", "d")
	require.NoError(t, connectSessionOwnershipClient(client))
	disconnected := make(chan Disconnect, 1)
	client.OnDisconnect(func(e DisconnectEvent) { disconnected <- e.Disconnect })

	store.SetRenewError(errors.New("ownership store down"))

	select {
	case d := <-disconnected:
		require.Equal(t, DisconnectServerError.Code, d.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the connection to be disconnected")
	}
}

// The owning node crashes: it stops renewing and never releases. Once the
// lease TTL passes, the crashed owner's generation is no longer valid and a
// new connection takes the session over with a higher generation.
func TestSessionOwnershipOwnerCrash(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	var mu sync.Mutex
	now := time.Now()
	store.SetClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})

	ctx := context.Background()
	key := sessionOwnershipKey("u", "d")

	gen1, err := store.Acquire(ctx, key, "crashed-owner", 10*time.Second)
	require.NoError(t, err)
	require.Equal(t, uint64(1), gen1)

	// The owner crashes: no renewal, no release. The lease expires.
	mu.Lock()
	now = now.Add(11 * time.Second)
	mu.Unlock()

	ok, err := store.Validate(ctx, key, "crashed-owner", gen1)
	require.NoError(t, err)
	require.False(t, ok)
	require.ErrorIs(t, store.Renew(ctx, key, "crashed-owner", gen1, 10*time.Second), ErrSessionOwnershipLost)

	// A new connection takes the session over with a higher generation.
	gen2, err := store.Acquire(ctx, key, "new-owner", 10*time.Second)
	require.NoError(t, err)
	require.Greater(t, gen2, gen1)
	ok, err = store.Validate(ctx, key, "new-owner", gen2)
	require.NoError(t, err)
	require.True(t, ok)
}
