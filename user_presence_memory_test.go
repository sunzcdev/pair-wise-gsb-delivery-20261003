package centrifuge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type userPresenceRecorder struct {
	mu     sync.Mutex
	joins  []UserPresenceJoinEvent
	leaves []UserPresenceLeaveEvent
}

func (r *userPresenceRecorder) join(ev UserPresenceJoinEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.joins = append(r.joins, ev)
}

func (r *userPresenceRecorder) leave(ev UserPresenceLeaveEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaves = append(r.leaves, ev)
}

func (r *userPresenceRecorder) snapshot() ([]UserPresenceJoinEvent, []UserPresenceLeaveEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	joins := make([]UserPresenceJoinEvent, len(r.joins))
	copy(joins, r.joins)
	leaves := make([]UserPresenceLeaveEvent, len(r.leaves))
	copy(leaves, r.leaves)
	return joins, leaves
}

func newUserPresenceMemoryNode(t testing.TB, c MemoryPresenceManagerConfig) (*Node, *MemoryPresenceManager, *userPresenceRecorder) {
	t.Helper()
	n, err := New(Config{})
	require.NoError(t, err)

	rec := &userPresenceRecorder{}
	n.OnUserPresenceJoin(rec.join)
	n.OnUserPresenceLeave(rec.leave)

	m, err := NewMemoryPresenceManager(n, c)
	require.NoError(t, err)
	n.SetPresenceManager(m)
	require.NoError(t, n.Run())

	t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
	return n, m, rec
}

func info(clientID, userID string) *ClientInfo {
	return &ClientInfo{ClientID: clientID, UserID: userID}
}

func waitFor(t testing.TB, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.True(t, cond(), msg)
}

func TestMemoryUserPresence_SingleJoinOnceMultipleConnections(t *testing.T) {
	_, m, rec := newUserPresenceMemoryNode(t, MemoryPresenceManagerConfig{
		EnableUserPresenceEvents: func(ch string) bool { return ch == "ch1" },
	})

	require.NoError(t, m.AddPresence("ch1", "c1", info("c1", "u1")))
	require.NoError(t, m.AddPresence("ch1", "c2", info("c2", "u1")))
	require.NoError(t, m.AddPresence("ch1", "c3", info("c3", "u1")))

	joins, leaves := rec.snapshot()
	require.Len(t, joins, 1)
	require.Equal(t, "u1", joins[0].UserID)
	require.Equal(t, "c1", joins[0].ClientID)
	require.Empty(t, leaves)

	// Removing two of three connections must not leave.
	require.NoError(t, m.RemovePresence("ch1", "c1", "u1"))
	require.NoError(t, m.RemovePresence("ch1", "c2", "u1"))
	joins, leaves = rec.snapshot()
	require.Len(t, joins, 1)
	require.Empty(t, leaves, "leave fires only after the last connection is gone")

	// Last connection leaves.
	require.NoError(t, m.RemovePresence("ch1", "c3", "u1"))
	waitFor(t, func() bool {
		_, l := rec.snapshot()
		return len(l) == 1
	}, time.Second, "expected one leave")
	_, leaves = rec.snapshot()
	require.Len(t, leaves, 1)
	require.Equal(t, "u1", leaves[0].UserID)
}

func TestMemoryUserPresence_DisabledChannelNoEvents(t *testing.T) {
	_, m, rec := newUserPresenceMemoryNode(t, MemoryPresenceManagerConfig{
		EnableUserPresenceEvents: func(ch string) bool { return false },
	})
	require.NoError(t, m.AddPresence("ch", "c1", info("c1", "u1")))
	require.NoError(t, m.RemovePresence("ch", "c1", "u1"))
	joins, leaves := rec.snapshot()
	require.Empty(t, joins)
	require.Empty(t, leaves)
}

func TestMemoryUserPresence_GracePeriodCancelsLeave(t *testing.T) {
	_, m, rec := newUserPresenceMemoryNode(t, MemoryPresenceManagerConfig{
		EnableUserPresenceEvents: func(ch string) bool { return true },
		UserLeaveGracePeriod:     200 * time.Millisecond,
	})

	require.NoError(t, m.AddPresence("ch", "c1", info("c1", "u1")))
	require.NoError(t, m.RemovePresence("ch", "c1", "u1"))

	// No leave while grace runs.
	time.Sleep(80 * time.Millisecond)
	_, leaves := rec.snapshot()
	require.Empty(t, leaves)

	// Reconnect with a new connection inside the grace window: leave cancelled.
	require.NoError(t, m.AddPresence("ch", "c2", info("c2", "u1")))
	time.Sleep(250 * time.Millisecond)
	joins, leaves := rec.snapshot()
	require.Len(t, joins, 1)
	require.Empty(t, leaves, "reconnect within grace must cancel leave")

	// Leaving again after reconnect fires exactly one leave after grace.
	require.NoError(t, m.RemovePresence("ch", "c2", "u1"))
	waitFor(t, func() bool {
		_, l := rec.snapshot()
		return len(l) == 1
	}, time.Second, "expected leave after grace")
	_, leaves = rec.snapshot()
	require.Len(t, leaves, 1)
}

func TestMemoryUserPresence_DifferentUsersIndependent(t *testing.T) {
	_, m, rec := newUserPresenceMemoryNode(t, MemoryPresenceManagerConfig{
		EnableUserPresenceEvents: func(ch string) bool { return true },
	})

	require.NoError(t, m.AddPresence("ch", "c1", info("c1", "u1")))
	require.NoError(t, m.AddPresence("ch", "c2", info("c2", "u2")))
	require.NoError(t, m.RemovePresence("ch", "c1", "u1"))

	joins, leaves := rec.snapshot()
	require.Len(t, joins, 2)
	require.Len(t, leaves, 1)
	require.Equal(t, "u1", leaves[0].UserID)
}

func TestMemoryUserPresence_NaturalExpiryFiresLeave(t *testing.T) {
	_, m, rec := newUserPresenceMemoryNode(t, MemoryPresenceManagerConfig{
		EnableUserPresenceEvents: func(ch string) bool { return true },
		PresenceTTL:              120 * time.Millisecond,
		PresenceCleanupInterval:  30 * time.Millisecond,
	})

	require.NoError(t, m.AddPresence("ch", "c1", info("c1", "u1")))
	// Never refresh; simulate abnormal loss by waiting out the TTL (no Remove).
	waitFor(t, func() bool {
		_, l := rec.snapshot()
		return len(l) == 1
	}, 2*time.Second, "expected leave after natural expiry")

	p, err := m.Presence("ch")
	require.NoError(t, err)
	require.Empty(t, p)

	// A later add is treated as a fresh join, not a duplicate/no-op.
	require.NoError(t, m.AddPresence("ch", "c2", info("c2", "u1")))
	joins, _ := rec.snapshot()
	require.Len(t, joins, 2)
}

func TestMemoryUserPresence_RefreshPreventsExpiry(t *testing.T) {
	_, m, rec := newUserPresenceMemoryNode(t, MemoryPresenceManagerConfig{
		EnableUserPresenceEvents: func(ch string) bool { return true },
		PresenceTTL:              120 * time.Millisecond,
		PresenceCleanupInterval:  30 * time.Millisecond,
	})

	require.NoError(t, m.AddPresence("ch", "c1", info("c1", "u1")))
	// Keep refreshing.
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = m.AddPresence("ch", "c1", info("c1", "u1"))
			}
		}
	}()
	time.Sleep(400 * time.Millisecond)
	close(stop)
	_, leaves := rec.snapshot()
	require.Empty(t, leaves, "refreshed presence must not expire")
}

func TestMemoryUserPresence_PresenceStatsStillWorks(t *testing.T) {
	_, m, _ := newUserPresenceMemoryNode(t, MemoryPresenceManagerConfig{
		EnableUserPresenceEvents: func(ch string) bool { return true },
	})
	require.NoError(t, m.AddPresence("ch", "c1", info("c1", "u1")))
	require.NoError(t, m.AddPresence("ch", "c2", info("c2", "u1")))
	require.NoError(t, m.AddPresence("ch", "c3", info("c3", "u2")))
	stats, err := m.PresenceStats("ch")
	require.NoError(t, err)
	require.Equal(t, 3, stats.NumClients)
	require.Equal(t, 2, stats.NumUsers)
}

func TestMemoryUserPresence_Contract(t *testing.T) {
	runUserPresenceContract(t, func(t *testing.T) userPresenceHarness {
		const ttl = 300 * time.Millisecond
		n, m, rec := newUserPresenceMemoryNode(t, MemoryPresenceManagerConfig{
			EnableUserPresenceEvents: func(ch string) bool { return true },
			UserLeaveGracePeriod:     150 * time.Millisecond,
			PresenceTTL:              ttl,
			PresenceCleanupInterval:  60 * time.Millisecond,
		})
		_ = n
		return userPresenceHarness{
			name:            "memory",
			mgr:             m,
			rec:             rec,
			grace:           150 * time.Millisecond,
			settle:          300 * time.Millisecond,
			refreshInterval: 80 * time.Millisecond,
			closeFn:         func() {},
		}
	})
}
