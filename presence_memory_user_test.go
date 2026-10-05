package centrifuge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func collectUserEvents(m *MemoryPresenceManager) chan UserPresenceEvent {
	events := make(chan UserPresenceEvent, 32)
	m.SetUserPresenceEmitter(func(e UserPresenceEvent) {
		events <- e
	})
	return events
}

func userInfo(clientID, userID string) *ClientInfo {
	return &ClientInfo{ClientID: clientID, UserID: userID}
}

// TestMemoryUserPresence_ThreePagesOneEnterOneLeave mirrors the headline use
// case: three connections of the same user produce a single enter, and the
// leave fires only after the third connection is gone.
func TestMemoryUserPresence_ThreePagesOneEnterOneLeave(t *testing.T) {
	n, _ := New(Config{LogHandler: func(LogEntry) {}})
	require.NoError(t, n.Run())
	defer func() { _ = n.Shutdown(context.Background()) }()

	m, err := NewMemoryPresenceManager(n, MemoryPresenceManagerConfig{})
	require.NoError(t, err)
	events := collectUserEvents(m)

	ch := "room"
	user := "user-1"

	ev, fired, err := m.AddUserPresence(ch, "c1", userInfo("c1", user))
	require.NoError(t, err)
	require.True(t, fired)
	require.Equal(t, UserPresenceEnter, ev.Type)

	_, fired, err = m.AddUserPresence(ch, "c2", userInfo("c2", user))
	require.NoError(t, err)
	require.False(t, fired)
	_, fired, err = m.AddUserPresence(ch, "c3", userInfo("c3", user))
	require.NoError(t, err)
	require.False(t, fired)

	// Removing two of the three connections must not produce a leave.
	_, fired, err = m.RemoveUserPresence(ch, "c1", user)
	require.NoError(t, err)
	require.False(t, fired)
	_, fired, err = m.RemoveUserPresence(ch, "c2", user)
	require.NoError(t, err)
	require.False(t, fired)

	// Last connection leaves -> immediate leave (no grace configured).
	ev, fired, err = m.RemoveUserPresence(ch, "c3", user)
	require.NoError(t, err)
	require.True(t, fired)
	require.Equal(t, UserPresenceLeave, ev.Type)
	require.Equal(t, user, ev.UserID)

	// No deferred events.
	select {
	case e := <-events:
		t.Fatalf("unexpected deferred event: %v", e)
	default:
	}
}

// TestMemoryUserPresence_DistinctUsersAreIndependent makes sure two different
// users each get their own enter/leave.
func TestMemoryUserPresence_DistinctUsersAreIndependent(t *testing.T) {
	n, _ := New(Config{LogHandler: func(LogEntry) {}})
	require.NoError(t, n.Run())
	defer func() { _ = n.Shutdown(context.Background()) }()

	m, err := NewMemoryPresenceManager(n, MemoryPresenceManagerConfig{})
	require.NoError(t, err)

	ch := "room"
	for _, user := range []string{"a", "b"} {
		_, fired, err := m.AddUserPresence(ch, "c-"+user, userInfo("c-"+user, user))
		require.NoError(t, err)
		require.True(t, fired)
	}
	ev, fired, err := m.RemoveUserPresence(ch, "c-a", "a")
	require.NoError(t, err)
	require.True(t, fired)
	require.Equal(t, "a", ev.UserID)
}

// TestMemoryUserPresence_GraceReconnectCancelsLeave verifies that a reconnect
// inside the grace window cancels the pending leave and never emits a duplicate
// enter, while leaving after the full grace does emit.
func TestMemoryUserPresence_GraceReconnectCancelsLeave(t *testing.T) {
	n, _ := New(Config{LogHandler: func(LogEntry) {}})
	require.NoError(t, n.Run())
	defer func() { _ = n.Shutdown(context.Background()) }()

	grace := 200 * time.Millisecond
	m, err := NewMemoryPresenceManager(n, MemoryPresenceManagerConfig{UserPresenceGracePeriod: grace})
	require.NoError(t, err)
	events := collectUserEvents(m)

	ch, user := "room", "user-1"
	_, fired, err := m.AddUserPresence(ch, "c1", userInfo("c1", user))
	require.NoError(t, err)
	require.True(t, fired)

	// Last connection goes away: leave is deferred.
	_, fired, err = m.RemoveUserPresence(ch, "c1", user)
	require.NoError(t, err)
	require.False(t, fired)

	// Reconnect before grace elapses (possibly a different client id after a
	// page reload): no leave, no duplicate enter.
	_, fired, err = m.AddUserPresence(ch, "c2", userInfo("c2", user))
	require.NoError(t, err)
	require.False(t, fired)

	select {
	case e := <-events:
		t.Fatalf("reconnect should have cancelled leave, got %v", e)
	case <-time.After(grace + 150*time.Millisecond):
	}

	// Now stay away longer than the grace: leave must fire via the emitter.
	_, fired, err = m.RemoveUserPresence(ch, "c2", user)
	require.NoError(t, err)
	require.False(t, fired)

	select {
	case e := <-events:
		require.Equal(t, UserPresenceLeave, e.Type)
		require.Equal(t, user, e.UserID)
	case <-time.After(grace + 500*time.Millisecond):
		t.Fatal("timed out waiting for deferred leave")
	}
}

// TestMemoryUserPresence_GraceExpiresWithoutReconnect covers the case where no
// reconnect arrives: the deferred leave is delivered through the emitter.
func TestMemoryUserPresence_GraceExpiresWithoutReconnect(t *testing.T) {
	n, _ := New(Config{LogHandler: func(LogEntry) {}})
	require.NoError(t, n.Run())
	defer func() { _ = n.Shutdown(context.Background()) }()

	grace := 80 * time.Millisecond
	m, err := NewMemoryPresenceManager(n, MemoryPresenceManagerConfig{UserPresenceGracePeriod: grace})
	require.NoError(t, err)
	events := collectUserEvents(m)

	ch, user := "room", "user-7"
	_, _, _ = m.AddUserPresence(ch, "c1", userInfo("c1", user))
	_, _, _ = m.RemoveUserPresence(ch, "c1", user)

	select {
	case e := <-events:
		require.Equal(t, UserPresenceLeave, e.Type)
		require.Equal(t, user, e.UserID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for deferred leave")
	}

	// A subsequent add after the leave is a fresh enter.
	_, fired, err := m.AddUserPresence(ch, "c2", userInfo("c2", user))
	require.NoError(t, err)
	require.True(t, fired)
}
