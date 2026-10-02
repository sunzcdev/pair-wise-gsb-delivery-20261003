package centrifuge

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/centrifugal/protocol"
	"github.com/stretchr/testify/require"
)

// manualClock is a test clock for deterministic lease expiry scenarios.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestSharedSessionOwnershipStoreSimultaneousTakeover(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	key := SessionKey{UserID: "u1", DeviceID: "d1"}

	// Nodes racing to take over the same session: every acquire must get its
	// own generation, and only the last one may remain valid.
	const numOwners = 8
	start := make(chan struct{})
	leases := make([]SessionLease, numOwners)
	var wg sync.WaitGroup
	for i := 0; i < numOwners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			lease, err := store.Acquire(context.Background(), key, SessionOwner{
				NodeID:   "n" + strconv.Itoa(i%2),
				ClientID: "c" + strconv.Itoa(i),
			}, time.Minute)
			require.NoError(t, err)
			leases[i] = lease
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[uint64]bool, numOwners)
	var maxGeneration uint64
	for _, lease := range leases {
		require.False(t, seen[lease.Generation], "generation %d issued twice", lease.Generation)
		seen[lease.Generation] = true
		maxGeneration = max(maxGeneration, lease.Generation)
	}
	require.Len(t, seen, numOwners)

	for _, lease := range leases {
		err := store.Validate(context.Background(), key, lease)
		if lease.Generation == maxGeneration {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrSessionOwnershipLost)
		}
	}
}

func TestSharedSessionOwnershipStoreLateRelease(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	key := SessionKey{UserID: "u1", DeviceID: "d1"}
	ownerA := SessionOwner{NodeID: "n1", ClientID: "c1"}
	ownerB := SessionOwner{NodeID: "n2", ClientID: "c2"}

	leaseA, err := store.Acquire(context.Background(), key, ownerA, time.Minute)
	require.NoError(t, err)
	leaseB, err := store.Acquire(context.Background(), key, ownerB, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uint64(1), leaseA.Generation)
	require.Equal(t, uint64(2), leaseB.Generation)

	// The old owner's disconnect arrives late, after the takeover: its release
	// must not disturb the new owner's lease.
	require.NoError(t, store.Release(context.Background(), key, leaseA, ownerA))
	require.NoError(t, store.Validate(context.Background(), key, leaseB))

	// A release with the current generation but a stale owner is also a no-op.
	require.NoError(t, store.Release(context.Background(), key, leaseB, ownerA))
	require.NoError(t, store.Validate(context.Background(), key, leaseB))

	// The current owner's own release works.
	require.NoError(t, store.Release(context.Background(), key, leaseB, ownerB))
	require.ErrorIs(t, store.Validate(context.Background(), key, leaseB), ErrSessionOwnershipLost)

	// Generations stay monotonic across the release: no generation reuse.
	leaseC, err := store.Acquire(context.Background(), key, ownerA, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uint64(3), leaseC.Generation)
}

func TestSharedSessionOwnershipStoreOwnerCrashExpiry(t *testing.T) {
	t.Parallel()
	clock := &manualClock{now: time.Now()}
	store := NewSharedSessionOwnershipStore()
	store.table.now = clock.Now
	key := SessionKey{UserID: "u1", DeviceID: "d1"}
	ownerA := SessionOwner{NodeID: "n1", ClientID: "c1"}
	ownerB := SessionOwner{NodeID: "n2", ClientID: "c2"}
	const ttl = 30 * time.Second

	leaseA, err := store.Acquire(context.Background(), key, ownerA, ttl)
	require.NoError(t, err)

	// Renew extends the lease: half a TTL after the renew the lease is valid.
	clock.Advance(ttl / 2)
	require.NoError(t, store.Renew(context.Background(), key, leaseA, ownerA, ttl))
	clock.Advance(ttl / 2)
	require.NoError(t, store.Validate(context.Background(), key, leaseA))

	// The owner crashes: no more renews, the lease expires.
	clock.Advance(ttl + time.Second)
	require.ErrorIs(t, store.Validate(context.Background(), key, leaseA), ErrSessionOwnershipLost)
	require.ErrorIs(t, store.Renew(context.Background(), key, leaseA, ownerA, ttl), ErrSessionOwnershipLost)

	// A reconnect after the crash still takes over - the stale lease never
	// blocks an acquire - and the generation keeps increasing.
	leaseB, err := store.Acquire(context.Background(), key, ownerB, ttl)
	require.NoError(t, err)
	require.Equal(t, leaseA.Generation+1, leaseB.Generation)
	require.NoError(t, store.Validate(context.Background(), key, leaseB))
}

func TestMemorySessionOwnershipStore(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionOwnershipStore()
	key := SessionKey{UserID: "u1", DeviceID: "d1"}
	ownerA := SessionOwner{NodeID: "n1", ClientID: "c1"}
	ownerB := SessionOwner{NodeID: "n1", ClientID: "c2"}

	leaseA, err := store.Acquire(context.Background(), key, ownerA, time.Minute)
	require.NoError(t, err)
	require.NoError(t, store.Validate(context.Background(), key, leaseA))

	leaseB, err := store.Acquire(context.Background(), key, ownerB, time.Minute)
	require.NoError(t, err)
	require.Equal(t, leaseA.Generation+1, leaseB.Generation)
	require.ErrorIs(t, store.Validate(context.Background(), key, leaseA), ErrSessionOwnershipLost)
	require.ErrorIs(t, store.Renew(context.Background(), key, leaseA, ownerA, time.Minute), ErrSessionOwnershipLost)
	require.NoError(t, store.Renew(context.Background(), key, leaseB, ownerB, time.Minute))
}

// newSessionOwnershipTestNode creates a running node with session ownership
// enabled and permissive subscribe/publish handlers.
func newSessionOwnershipTestNode(t *testing.T, store SessionOwnershipStore, cfg SessionOwnershipConfig) *Node {
	t.Helper()
	cfg.Store = store
	node, err := New(Config{
		LogLevel:         LogLevelError,
		LogHandler:       func(LogEntry) {},
		SessionOwnership: cfg,
	})
	require.NoError(t, err)
	node.OnConnect(func(client *Client) {
		client.OnSubscribe(func(e SubscribeEvent, cb SubscribeCallback) {
			cb(SubscribeReply{}, nil)
		})
		client.OnPublish(func(e PublishEvent, cb PublishCallback) {
			cb(PublishReply{}, nil)
		})
	})
	require.NoError(t, node.Run())
	t.Cleanup(func() { _ = node.Shutdown(context.Background()) })
	return node
}

func newSessionOwnershipTestClient(t *testing.T, node *Node, userID, deviceID string) *Client {
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

func subscribeSessionClient(t *testing.T, client *Client, channel string) error {
	t.Helper()
	rwWrapper := testReplyWriterWrapper()
	return client.handleSubscribe(&protocol.SubscribeRequest{
		Channel: channel,
	}, &protocol.Command{Id: 2}, time.Now(), rwWrapper.rw)
}

func publishSessionClient(t *testing.T, client *Client, channel string) error {
	t.Helper()
	rwWrapper := testReplyWriterWrapper()
	return client.handlePublish(&protocol.PublishRequest{
		Channel: channel,
		Data:    []byte(`{}`),
	}, &protocol.Command{Id: 3}, time.Now(), rwWrapper.rw)
}

func waitTransportDisconnect(t *testing.T, transport *testTransport) Disconnect {
	t.Helper()
	select {
	case <-transport.closeCh:
	case <-time.After(10 * time.Second):
		t.Fatal("no disconnect in timeout")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.disconnect
}

func TestClientSessionTakeoverSameNode(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionOwnershipStore()
	node := newSessionOwnershipTestNode(t, store, SessionOwnershipConfig{})

	clientA := newSessionOwnershipTestClient(t, node, "42", "dev1")
	connectClientV2(t, clientA)
	require.NoError(t, subscribeSessionClient(t, clientA, "test"))
	require.NoError(t, publishSessionClient(t, clientA, "test"))

	// A new connection with the same user ID + device ID takes the session over.
	clientB := newSessionOwnershipTestClient(t, node, "42", "dev1")
	connectClientV2(t, clientB)
	require.Equal(t, uint64(2), clientB.sessionOwnership.lease.Generation)

	// The old connection's protected operations are fenced by generation.
	require.Equal(t, ErrorSessionRevoked, subscribeSessionClient(t, clientA, "test2"))
	require.Equal(t, ErrorSessionRevoked, publishSessionClient(t, clientA, "test"))

	// The old socket is closed (the timing of the close itself is not the
	// guarantee - the fencing above is).
	d := waitTransportDisconnect(t, clientA.transport.(*testTransport))
	require.Equal(t, DisconnectSessionTakenOver.Code, d.Code)

	// The new connection keeps working.
	require.NoError(t, subscribeSessionClient(t, clientB, "test"))
	require.NoError(t, publishSessionClient(t, clientB, "test"))
}

func TestClientSessionTakeoverAcrossNodes(t *testing.T) {
	t.Parallel()
	store := NewSharedSessionOwnershipStore()
	cfg := SessionOwnershipConfig{
		LeaseTTL:      300 * time.Millisecond,
		RenewInterval: 50 * time.Millisecond,
	}
	node1 := newSessionOwnershipTestNode(t, store, cfg)
	node2 := newSessionOwnershipTestNode(t, store, cfg)

	clientA := newSessionOwnershipTestClient(t, node1, "42", "dev1")
	connectClientV2(t, clientA)
	require.NoError(t, subscribeSessionClient(t, clientA, "test"))

	// Takeover from another node: no broadcast or kick message is involved,
	// the shared store is the only proof.
	clientB := newSessionOwnershipTestClient(t, node2, "42", "dev1")
	connectClientV2(t, clientB)

	key := SessionKey{UserID: "42", DeviceID: "dev1"}
	require.NoError(t, store.Validate(context.Background(), key, clientB.sessionOwnership.lease))
	require.ErrorIs(t, store.Validate(context.Background(), key, clientA.sessionOwnership.lease), ErrSessionOwnershipLost)

	// Node1 never notified clientA, yet its protected operations are fenced.
	require.Equal(t, ErrorSessionRevoked, subscribeSessionClient(t, clientA, "test2"))
	require.Equal(t, ErrorSessionRevoked, publishSessionClient(t, clientA, "test"))

	// Without any kick from node2, clientA's own renew loop learns of the
	// takeover and disconnects it.
	d := waitTransportDisconnect(t, clientA.transport.(*testTransport))
	require.Equal(t, DisconnectSessionTakenOver.Code, d.Code)

	require.NoError(t, publishSessionClient(t, clientB, "test"))
}

func TestClientSessionLateDisconnectDoesNotDisturbNewOwner(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionOwnershipStore()
	node := newSessionOwnershipTestNode(t, store, SessionOwnershipConfig{})

	clientA := newSessionOwnershipTestClient(t, node, "42", "dev1")
	connectClientV2(t, clientA)
	clientB := newSessionOwnershipTestClient(t, node, "42", "dev1")
	connectClientV2(t, clientB)

	// The old connection's close arrives late - after the takeover.
	require.NoError(t, clientA.close(DisconnectConnectionClosed))

	// The new owner's lease survives the late release.
	key := SessionKey{UserID: "42", DeviceID: "dev1"}
	require.NoError(t, store.Validate(context.Background(), key, clientB.sessionOwnership.lease))
	require.NoError(t, subscribeSessionClient(t, clientB, "test"))
	require.NoError(t, publishSessionClient(t, clientB, "test"))
}

// blockingAcquireStore hangs every Acquire until the context is done, to
// exercise the acquire timeout path deterministically.
type blockingAcquireStore struct {
	SessionOwnershipStore
}

func (s *blockingAcquireStore) Acquire(ctx context.Context, _ SessionKey, _ SessionOwner, _ time.Duration) (SessionLease, error) {
	<-ctx.Done()
	return SessionLease{}, ctx.Err()
}

func TestClientSessionOwnershipAcquireTimeout(t *testing.T) {
	t.Parallel()
	store := &blockingAcquireStore{SessionOwnershipStore: NewMemorySessionOwnershipStore()}
	node := newSessionOwnershipTestNode(t, store, SessionOwnershipConfig{
		AcquireTimeout: 50 * time.Millisecond,
	})

	client := newSessionOwnershipTestClient(t, node, "42", "dev1")
	rwWrapper := testReplyWriterWrapper()
	err := client.connectCmd(&protocol.ConnectRequest{}, &protocol.Command{}, time.Now(), rwWrapper.rw)
	require.Equal(t, ErrorInternal, err)
	require.False(t, client.authenticated)
	require.Nil(t, client.sessionOwnership)

	// The failed acquire left no lease behind: a later acquire starts at
	// generation 1.
	lease, err := store.SessionOwnershipStore.Acquire(context.Background(), SessionKey{UserID: "42", DeviceID: "dev1"}, SessionOwner{}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uint64(1), lease.Generation)
}

// failingRenewStore fails every Renew with a store error.
type failingRenewStore struct {
	SessionOwnershipStore
}

func (s *failingRenewStore) Renew(context.Context, SessionKey, SessionLease, SessionOwner, time.Duration) error {
	return errors.New("store unavailable")
}

func TestClientSessionOwnershipRenewFailureDisconnects(t *testing.T) {
	t.Parallel()
	store := &failingRenewStore{SessionOwnershipStore: NewMemorySessionOwnershipStore()}
	node := newSessionOwnershipTestNode(t, store, SessionOwnershipConfig{
		RenewInterval: 10 * time.Millisecond,
	})

	client := newSessionOwnershipTestClient(t, node, "42", "dev1")
	connectClientV2(t, client)

	// A renew failure is fail-closed: the connection is revoked and
	// disconnected so it can reconnect and acquire a fresh lease.
	d := waitTransportDisconnect(t, client.transport.(*testTransport))
	require.Equal(t, DisconnectForceReconnect.Code, d.Code)
	require.True(t, client.sessionOwnership.revoked.Load())
	require.Equal(t, ErrorSessionRevoked, publishSessionClient(t, client, "test"))
}

func TestClientSessionOwnershipDisabledWithoutDeviceID(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionOwnershipStore()
	node := newSessionOwnershipTestNode(t, store, SessionOwnershipConfig{})

	client := newSessionOwnershipTestClient(t, node, "42", "")
	connectClientV2(t, client)
	require.Nil(t, client.sessionOwnership)
	require.NoError(t, subscribeSessionClient(t, client, "test"))
	require.NoError(t, publishSessionClient(t, client, "test"))
}
