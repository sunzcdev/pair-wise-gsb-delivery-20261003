package centrifuge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type userPresenceSink struct {
	mu     sync.Mutex
	events []UserPresenceEvent
	ch     chan UserPresenceEvent
}

func newUserPresenceSink() *userPresenceSink {
	return &userPresenceSink{ch: make(chan UserPresenceEvent, 32)}
}

func (s *userPresenceSink) handle(e UserPresenceEvent) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
	s.ch <- e
}

func (s *userPresenceSink) snapshot() []UserPresenceEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UserPresenceEvent, len(s.events))
	copy(out, s.events)
	return out
}

func nodeWithUserPresence(t *testing.T, grace time.Duration) (*Node, *MemoryPresenceManager, *userPresenceSink) {
	n, err := New(Config{LogHandler: func(LogEntry) {}})
	require.NoError(t, err)
	pm, err := NewMemoryPresenceManager(n, MemoryPresenceManagerConfig{UserPresenceGracePeriod: grace})
	require.NoError(t, err)
	n.SetPresenceManager(pm)
	sink := newUserPresenceSink()
	n.OnUserPresence(sink.handle)
	require.NoError(t, n.Run())
	return n, pm, sink
}

// TestNode_UserPresence_EndToEnd verifies the whole path: node add/remove ->
// manager counting -> memory broker fan-out -> registered Node handler.
func TestNode_UserPresence_EndToEnd(t *testing.T) {
	n, _, sink := nodeWithUserPresence(t, 0)
	defer func() { _ = n.Shutdown(context.Background()) }()

	ch, user := "chan1", "user-42"
	info := &ClientInfo{ClientID: "c1", UserID: user}
	n.addUserPresence(ch, "c1", info)
	n.addUserPresence(ch, "c2", &ClientInfo{ClientID: "c2", UserID: user})
	n.removeUserPresence(ch, "c1", user)
	n.removeUserPresence(ch, "c2", user)

	var enters, leaves int
	deadline := time.After(time.Second)
	for events := 0; events < 2; {
		select {
		case e := <-sink.ch:
			if e.Type == UserPresenceEnter {
				enters++
				require.Equal(t, ch, e.Channel)
				require.Equal(t, user, e.UserID)
				require.NotNil(t, e.Info)
			} else {
				leaves++
			}
			events++
		case <-deadline:
			t.Fatalf("expected 2 events, got enters=%d leaves=%d", enters, leaves)
		}
	}
	require.Equal(t, 1, enters)
	require.Equal(t, 1, leaves)
}

// TestNode_UserPresence_GraceCancel asserts reconnect inside grace suppresses
// the leave at the full Node fan-out level.
func TestNode_UserPresence_GraceCancel(t *testing.T) {
	grace := 150 * time.Millisecond
	n, _, sink := nodeWithUserPresence(t, grace)
	defer func() {
		_ = n.Shutdown(context.Background())
	}()

	ch, user := "chan2", "user-99"
	n.addUserPresence(ch, "c1", &ClientInfo{ClientID: "c1", UserID: user})
	<-sink.ch

	n.removeUserPresence(ch, "c1", user)
	time.Sleep(grace / 2)
	n.addUserPresence(ch, "c2", &ClientInfo{ClientID: "c2", UserID: user})

	// No leave should arrive after grace + margin.
	select {
	case e := <-sink.ch:
		t.Fatalf("unexpected event after grace reconnect: %v", e)
	case <-time.After(grace + 200*time.Millisecond):
	}

	// Final disconnect after grace emits a leave.
	n.removeUserPresence(ch, "c2", user)
	select {
	case e := <-sink.ch:
		require.Equal(t, UserPresenceLeave, e.Type)
		require.Equal(t, user, e.UserID)
	case <-time.After(time.Second):
		t.Fatal("expected deferred leave")
	}
}

// TestNode_UserPresence_DisabledWithoutUserPresenceManager ensures a plain
// PresenceManager that does not implement UserPresenceManager is a no-op.
func TestNode_UserPresence_DisabledWithoutUserPresenceManager(t *testing.T) {
	n, err := New(Config{LogHandler: func(LogEntry) {}})
	require.NoError(t, err)
	n.SetPresenceManager(NewTestPresenceManager())
	called := false
	n.OnUserPresence(func(UserPresenceEvent) { called = true })
	require.NoError(t, n.Run())
	defer func() { _ = n.Shutdown(context.Background()) }()

	n.addUserPresence("ch", "c1", &ClientInfo{ClientID: "c1", UserID: "u"})
	n.removeUserPresence("ch", "c1", "u")
	require.False(t, called)
	_ = context.Background()
}
