package centrifuge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// userPresenceHarness abstracts the memory and Redis managers so the exact same
// behavioral contract runs against both implementations. Grace period and
// expiry timings are deliberately expressed as real durations; implementations
// expose short intervals for tests through their configs.
type userPresenceHarness struct {
	name  string
	mgr   PresenceManager
	rec   *userPresenceRecorder
	grace time.Duration
	// settle waits long enough for the manager's leave timer / expiry
	// reconciler and cross-node propagation to finish.
	settle time.Duration
	// refreshInterval (0 = no background refresh in the test).
	refreshInterval time.Duration
	closeFn         func()
}

// runUserPresenceContract executes the common behavioral contract. Each
// implementation supplies a harness factory so memory and Redis stay in lock
// step.
func runUserPresenceContract(t *testing.T, makeHarness func(t *testing.T) userPresenceHarness) {
	t.Helper()

	t.Run("one join per user regardless of connection count", func(t *testing.T) {
		h := makeHarness(t)
		defer h.closeFn()

		require.NoError(t, h.mgr.AddPresence("room", "c1", info("c1", "alice")))
		require.NoError(t, h.mgr.AddPresence("room", "c2", info("c2", "alice")))
		require.NoError(t, h.mgr.AddPresence("room", "c3", info("c3", "alice")))

		joins, leaves := h.rec.snapshot()
		require.Len(t, joins, 1)
		require.Equal(t, "alice", joins[0].UserID)
		require.Empty(t, leaves)

		require.NoError(t, h.mgr.RemovePresence("room", "c1", "alice"))
		require.NoError(t, h.mgr.RemovePresence("room", "c2", "alice"))
		time.Sleep(h.settle)
		_, leaves = h.rec.snapshot()
		require.Empty(t, leaves, "non-final removals never leave")

		require.NoError(t, h.mgr.RemovePresence("room", "c3", "alice"))
		waitFor(t, func() bool {
			_, l := h.rec.snapshot()
			return len(l) == 1
		}, 5*time.Second, "leave after last connection")
		_, leaves = h.rec.snapshot()
		require.Len(t, leaves, 1)
		require.Equal(t, "alice", leaves[0].UserID)
	})

	t.Run("reconnect within grace cancels leave", func(t *testing.T) {
		h := makeHarness(t)
		defer h.closeFn()

		require.NoError(t, h.mgr.AddPresence("room", "c1", info("c1", "alice")))
		require.NoError(t, h.mgr.RemovePresence("room", "c1", "alice"))

		// Re-enter well within the grace period.
		time.Sleep(h.grace / 3)
		require.NoError(t, h.mgr.AddPresence("room", "c2", info("c2", "alice")))

		// Wait past the original grace deadline.
		time.Sleep(h.grace + h.settle)
		joins, leaves := h.rec.snapshot()
		require.Len(t, joins, 1)
		require.Empty(t, leaves, "no leave and no extra join after grace reconnect")

		// Final disconnect then leaves exactly once.
		require.NoError(t, h.mgr.RemovePresence("room", "c2", "alice"))
		waitFor(t, func() bool {
			_, l := h.rec.snapshot()
			return len(l) == 1
		}, 5*time.Second, "leave after final disconnect")
	})

	t.Run("independent users do not interfere", func(t *testing.T) {
		h := makeHarness(t)
		defer h.closeFn()

		require.NoError(t, h.mgr.AddPresence("room", "a1", info("a1", "alice")))
		require.NoError(t, h.mgr.AddPresence("room", "b1", info("b1", "bob")))
		require.NoError(t, h.mgr.AddPresence("room", "a2", info("a2", "alice")))

		require.NoError(t, h.mgr.RemovePresence("room", "a1", "alice"))
		require.NoError(t, h.mgr.RemovePresence("room", "a2", "alice"))

		waitFor(t, func() bool {
			_, l := h.rec.snapshot()
			return len(l) == 1
		}, 5*time.Second, "alice leaves")
		_, leaves := h.rec.snapshot()
		require.Equal(t, "alice", leaves[0].UserID)

		// Bob is still present.
		p, err := h.mgr.Presence("room")
		require.NoError(t, err)
		require.Contains(t, p, "b1")
	})

	t.Run("natural expiry follows the same counting semantics", func(t *testing.T) {
		h := makeHarness(t)
		defer h.closeFn()

		require.NoError(t, h.mgr.AddPresence("room", "c1", info("c1", "alice")))
		// No Remove: let presence expire naturally.
		waitFor(t, func() bool {
			_, l := h.rec.snapshot()
			return len(l) == 1
		}, 8*time.Second, "leave via natural expiry")

		p, err := h.mgr.Presence("room")
		require.NoError(t, err)
		require.NotContains(t, p, "c1")

		// Re-enter after expiry: fresh join.
		require.NoError(t, h.mgr.AddPresence("room", "c2", info("c2", "alice")))
		joins, _ := h.rec.snapshot()
		require.Len(t, joins, 2)
	})

	t.Run("refresh prevents natural expiry", func(t *testing.T) {
		h := makeHarness(t)
		defer h.closeFn()

		require.NoError(t, h.mgr.AddPresence("room", "c1", info("c1", "alice")))

		stop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(h.refreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					_ = h.mgr.AddPresence("room", "c1", info("c1", "alice"))
				}
			}
		}()
		time.Sleep(4 * h.settle)
		close(stop)

		_, leaves := h.rec.snapshot()
		require.Empty(t, leaves, "refreshed connection must not expire")
	})
}
