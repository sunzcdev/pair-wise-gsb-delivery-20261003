//go:build integration

package centrifuge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRedisUserPresence_ParityWithMemory replays the exact interleaved
// reconnect/leave scenario exercised by
// TestMemoryUserPresence_GraceReconnectCancelsLeave against Redis and asserts
// the same observable contract: one enter, reconnect inside grace cancels the
// leave, final departure after grace emits exactly one leave.
func TestRedisUserPresence_ParityWithMemory(t *testing.T) {
	t.Parallel()
	for _, tt := range redisPresenceTests {
		t.Run(tt.Name, func(t *testing.T) {
			grace := 150 * time.Millisecond
			n, pm := newUserPresenceRedisPM(t, getUniquePrefix(), grace, tt.Port)
			defer func() { _ = n.Shutdown(context.Background()) }()
			defer stopRedisPresenceManager(pm)
			events := collectRedisUserEvents(pm)

			ch, user := "parity-room", "parity-user"

			ev, fired, err := pm.AddUserPresence(ch, "c1", &ClientInfo{ClientID: "c1", UserID: user})
			require.NoError(t, err)
			require.True(t, fired)
			require.Equal(t, UserPresenceEnter, ev.Type)

			// Two additional pages -> no extra enters.
			for _, cid := range []string{"c2", "c3"} {
				_, fired, err = pm.AddUserPresence(ch, cid, &ClientInfo{ClientID: cid, UserID: user})
				require.NoError(t, err)
				require.False(t, fired)
			}

			// All pages leave; the last removal only schedules the leave.
			for _, cid := range []string{"c1", "c2", "c3"} {
				_, fired, err = pm.RemoveUserPresence(ch, cid, user)
				require.NoError(t, err)
				require.False(t, fired)
			}

			// Reconnect inside grace with a brand new client id cancels it.
			time.Sleep(grace / 2)
			_, fired, err = pm.AddUserPresence(ch, "c4", &ClientInfo{ClientID: "c4", UserID: user})
			require.NoError(t, err)
			require.False(t, fired, "reconnect within grace must not re-enter")

			select {
			case e := <-events:
				t.Fatalf("reconnect should cancel pending leave, got %v", e)
			case <-time.After(grace + 200*time.Millisecond):
			}

			// Final departure after grace -> exactly one leave via the emitter.
			_, fired, err = pm.RemoveUserPresence(ch, "c4", user)
			require.NoError(t, err)
			require.False(t, fired)

			select {
			case e := <-events:
				require.Equal(t, UserPresenceLeave, e.Type)
				require.Equal(t, user, e.UserID)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for parity leave event")
			}

			select {
			case e := <-events:
				t.Fatalf("expected exactly one leave, got extra %v", e)
			case <-time.After(grace + 200*time.Millisecond):
			}
		})
	}
}
