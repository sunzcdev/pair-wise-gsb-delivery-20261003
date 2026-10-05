//go:build integration

package centrifuge

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testUserPresenceTTL     = 2 * time.Second
	testUserPresenceGrace   = 800 * time.Millisecond
	testUserPresenceSweep   = 200 * time.Millisecond
	testUserPresenceRefresh = 500 * time.Millisecond
)

func newRedisUserPresencePair(t *testing.T, port int, prefix string, grace time.Duration) (*Node, *Node, *RedisPresenceManager, *RedisPresenceManager, *userPresenceRecorder, *userPresenceRecorder) {
	t.Helper()

	newOne := func() (*Node, *RedisPresenceManager, *userPresenceRecorder) {
		n := testNode(t)
		rec := &userPresenceRecorder{}
		n.OnUserPresenceJoin(rec.join)
		n.OnUserPresenceLeave(rec.leave)

		s, err := NewRedisShard(n, testSingleRedisConf(port))
		require.NoError(t, err)
		pm, err := NewRedisPresenceManager(n, RedisPresenceManagerConfig{
			Prefix:                      prefix,
			Shards:                      []*RedisShard{s},
			PresenceTTL:                 testUserPresenceTTL,
			UserPresenceCleanupInterval: testUserPresenceSweep,
			UserLeaveGracePeriod:        grace,
			EnableUserPresenceEvents: func(ch string) bool {
				return true
			},
		})
		require.NoError(t, err)
		n.SetPresenceManager(pm)
		require.NoError(t, n.Run())
		return n, pm, rec
	}

	n1, pm1, rec1 := newOne()
	n2, pm2, rec2 := newOne()

	t.Cleanup(func() {
		_ = n1.Shutdown(context.Background())
		_ = n2.Shutdown(context.Background())
		stopRedisPresenceManager(pm1)
		stopRedisPresenceManager(pm2)
	})

	return n1, n2, pm1, pm2, rec1, rec2
}

// drainRecorders pulls events observed on either node into the combined set
// until cond() is true or timeout.
func waitForCluster(t *testing.T, rec1, rec2 *userPresenceRecorder, cond func(joins []UserPresenceJoinEvent, leaves []UserPresenceLeaveEvent) bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j1, l1 := rec1.snapshot()
		j2, l2 := rec2.snapshot()
		joins := append(append([]UserPresenceJoinEvent{}, j1...), j2...)
		leaves := append(append([]UserPresenceLeaveEvent{}, l1...), l2...)
		if cond(joins, leaves) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	j1, l1 := rec1.snapshot()
	j2, l2 := rec2.snapshot()
	leaves := append(append([]UserPresenceLeaveEvent{}, l1...), l2...)
	joins := append(append([]UserPresenceJoinEvent{}, j1...), j2...)
	require.True(t, cond(joins, leaves),
		"cluster condition unmet; joins=%d leaves=%d", len(joins), len(leaves))
}

func TestRedisUserPresence_MultiNodeContract(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			prefix := getUniquePrefix()
			_, _, pm1, pm2, rec1, rec2 := newRedisUserPresencePair(t, tt.Port, prefix, testUserPresenceGrace)

			ch := "room"
			// Same user's three connections split across two nodes.
			require.NoError(t, pm1.AddPresence(ch, "c1", info("c1", "alice")))
			require.NoError(t, pm2.AddPresence(ch, "c2", info("c2", "alice")))
			require.NoError(t, pm1.AddPresence(ch, "c3", info("c3", "alice")))

			waitForCluster(t, rec1, rec2, func(joins []UserPresenceJoinEvent, _ []UserPresenceLeaveEvent) bool {
				return len(joins) == 1
			}, 3*time.Second)

			// Remove connections one at a time on alternating nodes; no leave
			// until the last one.
			require.NoError(t, pm1.RemovePresence(ch, "c1", "alice"))
			time.Sleep(testUserPresenceGrace + 400*time.Millisecond)
			require.NoError(t, pm2.RemovePresence(ch, "c2", "alice"))
			time.Sleep(testUserPresenceGrace + 400*time.Millisecond)

			_, l1 := rec1.snapshot()
			_, l2 := rec2.snapshot()
			require.Empty(t, append(append([]UserPresenceLeaveEvent{}, l1...), l2...),
				"non-final removals must not leave even past the grace window")

			// Last connection (on node 1) drops abnormally: leave scheduled.
			require.NoError(t, pm1.RemovePresence(ch, "c3", "alice"))

			// Reconnect on the *other* node within grace: cancels cluster-wide.
			time.Sleep(testUserPresenceGrace / 3)
			require.NoError(t, pm2.AddPresence(ch, "c4", info("c4", "alice")))

			time.Sleep(testUserPresenceGrace + 800*time.Millisecond)
			j1, l1 := rec1.snapshot()
			j2, l2 := rec2.snapshot()
			totalJoins := len(j1) + len(j2)
			totalLeaves := len(l1) + len(l2)
			require.Equal(t, 1, totalJoins, "reconnect within grace must not re-join")
			require.Equal(t, 0, totalLeaves, "reconnect within grace must cancel leave cluster-wide")

			// Final disconnect on node 2: exactly one leave across the cluster.
			require.NoError(t, pm2.RemovePresence(ch, "c4", "alice"))
			waitForCluster(t, rec1, rec2, func(_ []UserPresenceJoinEvent, leaves []UserPresenceLeaveEvent) bool {
				return len(leaves) == 1
			}, 5*time.Second,
			)
			_, l1 = rec1.snapshot()
			_, l2 = rec2.snapshot()
			require.Equal(t, 1, len(l1)+len(l2))
		})
	}
}

func TestRedisUserPresence_NaturalExpiryInterleavedWithReconnect(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			prefix := getUniquePrefix()
			_, _, pm1, pm2, rec1, rec2 := newRedisUserPresencePair(t, tt.Port, prefix, testUserPresenceGrace)

			ch := "room2"
			require.NoError(t, pm1.AddPresence(ch, "c1", info("c1", "alice")))
			require.NoError(t, pm2.AddPresence(ch, "c2", info("c2", "alice")))

			waitForCluster(t, rec1, rec2, func(joins []UserPresenceJoinEvent, _ []UserPresenceLeaveEvent) bool {
				return len(joins) == 1
			}, 3*time.Second)

			// Explicitly remove c1; c2 stops refreshing (simulated crash) and
			// expires naturally. The sweeper must produce exactly one leave.
			require.NoError(t, pm1.RemovePresence(ch, "c1", "alice"))
			waitForCluster(t, rec1, rec2, func(_ []UserPresenceJoinEvent, leaves []UserPresenceLeaveEvent) bool {
				return len(leaves) == 1
			}, testUserPresenceTTL+5*time.Second,
			)

			// Re-enter long after expiry: fresh join on another node.
			require.NoError(t, pm2.AddPresence(ch, "c3", info("c3", "alice")))
			waitForCluster(t, rec1, rec2, func(joins []UserPresenceJoinEvent, _ []UserPresenceLeaveEvent) bool {
				return len(joins) == 2
			}, 3*time.Second)
		})
	}
}

func TestRedisUserPresence_ConcurrentRemovesSingleLeave(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			prefix := getUniquePrefix()
			// No grace period: two nodes remove their last connections at the
			// same time; atomic counters guarantee a single leave.
			_, _, pm1, pm2, rec1, rec2 := newRedisUserPresencePair(t, tt.Port, prefix, 0)

			ch := "room3"
			require.NoError(t, pm1.AddPresence(ch, "c1", info("c1", "alice")))
			require.NoError(t, pm2.AddPresence(ch, "c2", info("c2", "alice")))
			waitForCluster(t, rec1, rec2, func(joins []UserPresenceJoinEvent, _ []UserPresenceLeaveEvent) bool {
				return len(joins) == 1
			}, 3*time.Second)

			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _ = pm1.RemovePresence(ch, "c1", "alice") }()
			go func() { defer wg.Done(); _ = pm2.RemovePresence(ch, "c2", "alice") }()
			wg.Wait()

			waitForCluster(t, rec1, rec2, func(_ []UserPresenceJoinEvent, leaves []UserPresenceLeaveEvent) bool {
				return len(leaves) == 1
			}, 3*time.Second,
			)
			_, l1 := rec1.snapshot()
			_, l2 := rec2.snapshot()
			require.Equal(t, 1, len(l1)+len(l2), fmt.Sprintf("l1=%d l2=%d", len(l1), len(l2)))
		})
	}
}
