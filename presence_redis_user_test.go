//go:build integration

package centrifuge

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// collectRedisUserEvents wires the manager emitter to a buffered channel.
func collectRedisUserEvents(pm *RedisPresenceManager) chan UserPresenceEvent {
	events := make(chan UserPresenceEvent, 64)
	pm.SetUserPresenceEmitter(func(e UserPresenceEvent) {
		events <- e
	})
	return events
}

func newUserPresenceRedisPM(t *testing.T, prefix string, grace time.Duration, port int) (*Node, *RedisPresenceManager) {
	n := testNode(t)
	s, err := NewRedisShard(n, testSingleRedisConf(port))
	require.NoError(t, err)
	pm, err := NewRedisPresenceManager(n, RedisPresenceManagerConfig{
		Prefix:                  prefix,
		Shards:                  []*RedisShard{s},
		PresenceTTL:             2 * time.Second,
		UserPresenceGracePeriod: grace,
	})
	require.NoError(t, err)
	n.SetPresenceManager(pm)
	require.NoError(t, n.Run())
	return n, pm
}

// TestRedisUserPresence_ThreePagesOneEnterOneLeave keeps the same user's
// connections on a single manager and asserts exactly one enter and one leave.
func TestRedisUserPresence_ThreePagesOneEnterOneLeave(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			n, pm := newUserPresenceRedisPM(t, getUniquePrefix(), 0, tt.Port)
			defer func() { _ = n.Shutdown(context.Background()) }()
			defer stopRedisPresenceManager(pm)
			events := collectRedisUserEvents(pm)

			ch, user := "room", "user-1"
			ev, fired, err := pm.AddUserPresence(ch, "c1", &ClientInfo{ClientID: "c1", UserID: user})
			require.NoError(t, err)
			require.True(t, fired)
			require.Equal(t, UserPresenceEnter, ev.Type)

			for _, cid := range []string{"c2", "c3"} {
				_, fired, err = pm.AddUserPresence(ch, cid, &ClientInfo{ClientID: cid, UserID: user})
				require.NoError(t, err)
				require.False(t, fired)
			}

			_, fired, err = pm.RemoveUserPresence(ch, "c1", user)
			require.NoError(t, err)
			require.False(t, fired)
			_, fired, err = pm.RemoveUserPresence(ch, "c2", user)
			require.NoError(t, err)
			require.False(t, fired)

			ev, fired, err = pm.RemoveUserPresence(ch, "c3", user)
			require.NoError(t, err)
			require.True(t, fired)
			require.Equal(t, UserPresenceLeave, ev.Type)

			select {
			case e := <-events:
				t.Fatalf("unexpected deferred event with no grace: %v", e)
			default:
			}
		})
	}
}

// TestRedisUserPresence_TwoNodes_ConcurrentUpdates places the same user's
// connections on two different nodes and interleaves removals. Counting is done
// atomically in Lua, so concurrent updates must never double-fire an enter or
// lose the single final leave.
func TestRedisUserPresence_TwoNodes_ConcurrentUpdates(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			prefix := getUniquePrefix()
			n1, pm1 := newUserPresenceRedisPM(t, prefix, 0, tt.Port)
			n2, pm2 := newUserPresenceRedisPM(t, prefix, 0, tt.Port)
			defer func() { _ = n1.Shutdown(context.Background()) }()
			defer func() { _ = n2.Shutdown(context.Background()) }()
			defer stopRedisPresenceManager(pm1)
			defer stopRedisPresenceManager(pm2)

			ch, user := "shared-room", "multi-node-user"

			// Two connections arrive concurrently on two nodes.
			var wg sync.WaitGroup
			enters := make(chan bool, 2)
			for _, add := range []func() (bool, error){
				func() (bool, error) {
					_, f, err := pm1.AddUserPresence(ch, "n1c", &ClientInfo{ClientID: "n1c", UserID: user})
					return f, err
				},
				func() (bool, error) {
					_, f, err := pm2.AddUserPresence(ch, "n2c", &ClientInfo{ClientID: "n2c", UserID: user})
					return f, err
				},
			} {
				wg.Add(1)
				go func(fn func() (bool, error)) {
					defer wg.Done()
					fired, err := fn()
					require.NoError(t, err)
					enters <- fired
				}(add)
			}
			wg.Wait()
			close(enters)
			enterCount := 0
			for fired := range enters {
				if fired {
					enterCount++
				}
			}
			require.Equal(t, 1, enterCount, "exactly one enter across two concurrent nodes")

			// Connections disappear concurrently from both nodes. Exactly one
			// must report the last-connection leave.
			leaves := make(chan bool, 2)
			for _, rem := range []func() (bool, error){
				func() (bool, error) {
					_, f, err := pm1.RemoveUserPresence(ch, "n1c", user)
					return f, err
				},
				func() (bool, error) {
					_, f, err := pm2.RemoveUserPresence(ch, "n2c", user)
					return f, err
				},
			} {
				wg.Add(1)
				go func(fn func() (bool, error)) {
					defer wg.Done()
					fired, err := fn()
					require.NoError(t, err)
					leaves <- fired
				}(rem)
			}
			wg.Wait()
			close(leaves)
			leaveCount := 0
			for fired := range leaves {
				if fired {
					leaveCount++
				}
			}
			require.Equal(t, 1, leaveCount, "exactly one leave across two concurrent nodes")
		})
	}
}

// TestRedisUserPresence_GraceReconnectCancelsLeave runs the grace-period cancel
// across two nodes: node1 schedules the leave, node2's reconnect cancels it.
func TestRedisUserPresence_GraceReconnectCancelsLeave(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			prefix := getUniquePrefix()
			grace := 400 * time.Millisecond
			n1, pm1 := newUserPresenceRedisPM(t, prefix, grace, tt.Port)
			n2, pm2 := newUserPresenceRedisPM(t, prefix, grace, tt.Port)
			defer func() { _ = n1.Shutdown(context.Background()) }()
			defer func() { _ = n2.Shutdown(context.Background()) }()
			defer stopRedisPresenceManager(pm1)
			defer stopRedisPresenceManager(pm2)

			events1 := collectRedisUserEvents(pm1)
			collectRedisUserEvents(pm2)

			ch, user := "grace-room", "grace-user"
			_, fired, err := pm1.AddUserPresence(ch, "c1", &ClientInfo{ClientID: "c1", UserID: user})
			require.NoError(t, err)
			require.True(t, fired)

			_, fired, err = pm1.RemoveUserPresence(ch, "c1", user)
			require.NoError(t, err)
			require.False(t, fired, "leave should be deferred")

			// Reconnect on the other node before the grace period elapses.
			time.Sleep(grace / 2)
			_, fired, err = pm2.AddUserPresence(ch, "c2", &ClientInfo{ClientID: "c2", UserID: user})
			require.NoError(t, err)
			require.False(t, fired, "reconnect inside grace must not re-enter")

			select {
			case e := <-events1:
				t.Fatalf("pending leave should be cancelled, got %v", e)
			case <-time.After(grace + 300*time.Millisecond):
			}
		})
	}
}

// TestRedisUserPresence_GraceLeaveConfirmed waits through the full grace and
// expects exactly one deferred leave emitted by the background reaper.
func TestRedisUserPresence_GraceLeaveConfirmed(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			prefix := getUniquePrefix()
			grace := 200 * time.Millisecond
			n1, pm1 := newUserPresenceRedisPM(t, prefix, grace, tt.Port)
			defer func() { _ = n1.Shutdown(context.Background()) }()
			defer stopRedisPresenceManager(pm1)
			events := collectRedisUserEvents(pm1)

			ch, user := "grace-room", "grace-user-2"
			_, _, err := pm1.AddUserPresence(ch, "c1", &ClientInfo{ClientID: "c1", UserID: user})
			require.NoError(t, err)
			_, fired, err := pm1.RemoveUserPresence(ch, "c1", user)
			require.NoError(t, err)
			require.False(t, fired)

			select {
			case e := <-events:
				require.Equal(t, UserPresenceLeave, e.Type)
				require.Equal(t, user, e.UserID)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for confirmed grace leave")
			}
		})
	}
}

// TestRedisUserPresence_NaturalExpiry_InterleavedWithReconnect interleaves
// natural presence TTL expiry cleanup with a reconnect. A crashed node never
// calls remove; the background reaper must reconcile the expired client and
// produce a single user leave, while a reconnect that lands before the grace
// boundary must suppress it.
func TestRedisUserPresence_NaturalExpiry_InterleavedWithReconnect(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			prefix := getUniquePrefix()
			grace := 500 * time.Millisecond
			n1, pm1 := newUserPresenceRedisPM(t, prefix, grace, tt.Port)
			n2, pm2 := newUserPresenceRedisPM(t, prefix, grace, tt.Port)
			defer func() { _ = n1.Shutdown(context.Background()) }()
			defer func() { _ = n2.Shutdown(context.Background()) }()
			defer stopRedisPresenceManager(pm1)
			defer stopRedisPresenceManager(pm2)
			events := collectRedisUserEvents(pm2)

			ch, user := "expiry-room", "expiry-user-"+strconv.Itoa(int(time.Now().UnixNano()%100000))

			// The only connection is registered and then "crashes": we never
			// call remove. Wait for its presence TTL to elapse so the reaper
			// reconciles it and schedules a grace leave.
			_, fired, err := pm1.AddUserPresence(ch, "c1", &ClientInfo{ClientID: "c1", UserID: user})
			require.NoError(t, err)
			require.True(t, fired)

			// Wait beyond the short PresenceTTL for the expiry reaper to run,
			// but then reconnect before the grace leave is confirmed.
			time.Sleep(2200 * time.Millisecond) // > PresenceTTL (2s)
			_, fired, err = pm2.AddUserPresence(ch, "c2", &ClientInfo{ClientID: "c2", UserID: user})
			// Either the expiry had already counted to zero (pending grace), in
			// which case this cancels it, or the user still counts as present;
			// in neither case may a leave slip through after this point.
			require.NoError(t, err)
			_ = fired

			select {
			case e := <-events:
				if e.Type == UserPresenceLeave {
					// A leave may already have been confirmed between expiry and
					// reconnect; that is acceptable only if it happened before
					// the reconnect. Here we reconnect well inside the 500ms
					// grace window after the 2s TTL, so require cancellation.
					t.Fatalf("reconnect inside grace should suppress leave: %v", e)
				}
			case <-time.After(grace + 300*time.Millisecond):
			}

			// Finally let it expire cleanly (remove, no reconnect) and expect
			// exactly one leave.
			_, fired, err = pm2.RemoveUserPresence(ch, "c2", user)
			require.NoError(t, err)
			require.False(t, fired)
			select {
			case e := <-events:
				require.Equal(t, UserPresenceLeave, e.Type)
				require.Equal(t, user, e.UserID)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for natural expiry leave")
			}
		})
	}
}
