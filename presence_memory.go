package centrifuge

import (
	"context"
	"sync"
	"time"
)

// MemoryPresenceManager is builtin default PresenceManager which allows running
// Centrifuge-based server without any external storage. All data managed inside process
// memory.
//
// With this PresenceManager you can only run single Centrifuge node. If you need to scale
// you should consider using another PresenceManager implementation instead – for example
// RedisPresenceManager.
//
// Running single node can be sufficient for many use cases especially when you
// need maximum performance and not too many online clients. Consider configuring
// your load balancer to have one backup Centrifuge node for HA in this case.
type MemoryPresenceManager struct {
	node        *Node
	config      MemoryPresenceManagerConfig
	presenceHub *presenceHub
}

var _ PresenceManager = (*MemoryPresenceManager)(nil)

// MemoryPresenceManagerConfig is a MemoryPresenceManager config.
type MemoryPresenceManagerConfig struct {
	// EnableUserPresenceEvents reports whether user-level presence events must be
	// maintained for a channel. When it returns true, the PresenceManager counts
	// connections per user and invokes the Node's OnUserPresenceJoin /
	// OnUserPresenceLeave handlers on the zero-to-one and one-to-zero connection
	// count transitions. Per-connection Join/Leave behavior (EmitJoinLeave) is
	// independent and stays available regardless of this option.
	// By default user presence events are disabled.
	EnableUserPresenceEvents func(channel string) bool

	// UserLeaveGracePeriod is an interval to wait after a user's last connection
	// in a channel disappears before emitting the user leave event. If the user
	// reconnects within this interval, the pending leave event is cancelled and no
	// leave is emitted. Zero means the leave is emitted immediately once the last
	// connection is gone. Only applies to channels for which
	// EnableUserPresenceEvents returns true.
	UserLeaveGracePeriod time.Duration

	// PresenceTTL defines how long a presence entry survives without being
	// refreshed. Unlike RedisPresenceManager, entries in memory never expire by
	// default (zero value) because there is no separate node process that could
	// own stale entries: disconnect and unsubscribe remove them synchronously. Set
	// a positive TTL to mirror Redis presence expiration semantics — entries that
	// stop being refreshed (e.g. after a hard process crash of an embedded
	// scenario) are then cleaned up and produce the same user leave events.
	PresenceTTL time.Duration

	// PresenceCleanupInterval is how often expired presence entries are collected
	// when PresenceTTL is set. Zero value means PresenceTTL / 3. The minimum
	// effective interval is 10ms.
	PresenceCleanupInterval time.Duration
}

// NewMemoryPresenceManager initializes MemoryPresenceManager.
func NewMemoryPresenceManager(n *Node, c MemoryPresenceManagerConfig) (*MemoryPresenceManager, error) {
	return &MemoryPresenceManager{
		node:        n,
		config:      c,
		presenceHub: newPresenceHubWithConfig(n, c),
	}, nil
}

// AddPresence - see PresenceManager interface description.
func (m *MemoryPresenceManager) AddPresence(ch string, uid string, info *ClientInfo) error {
	return m.presenceHub.add(ch, uid, info)
}

// RemovePresence - see PresenceManager interface description.
func (m *MemoryPresenceManager) RemovePresence(ch string, clientID string, _ string) error {
	return m.presenceHub.remove(ch, clientID)
}

// Presence - see PresenceManager interface description.
func (m *MemoryPresenceManager) Presence(ch string) (map[string]*ClientInfo, error) {
	return m.presenceHub.get(ch)
}

// PresenceStats - see PresenceManager interface description.
func (m *MemoryPresenceManager) PresenceStats(ch string) (PresenceStats, error) {
	return m.presenceHub.getStats(ch)
}

// Close is noop for now.
func (m *MemoryPresenceManager) Close(_ context.Context) error {
	m.presenceHub.stop()
	return nil
}

type presenceHub struct {
	mu sync.Mutex

	node   *Node
	config MemoryPresenceManagerConfig

	// presence maps channel -> clientID -> entry.
	presence map[string]map[string]*memoryPresenceEntry
	// users maps channel -> userID -> per-user state. Entries only exist for
	// channels where user presence events are enabled.
	users map[string]map[string]*memoryUserPresence

	now func() time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
}

type memoryPresenceEntry struct {
	info     *ClientInfo
	expireAt int64
}

type memoryUserPresence struct {
	// clients is the set of client IDs currently counted for this user.
	clients map[string]struct{}
	// pendingLeave is non-nil while a leave grace period is running.
	pendingLeave *time.Timer
}

func newPresenceHub() *presenceHub {
	return newPresenceHubWithConfig(nil, MemoryPresenceManagerConfig{})
}

func newPresenceHubWithConfig(n *Node, c MemoryPresenceManagerConfig) *presenceHub {
	h := &presenceHub{
		node:     n,
		config:   c,
		presence: make(map[string]map[string]*memoryPresenceEntry),
		users:    make(map[string]map[string]*memoryUserPresence),
		now:      time.Now,
		stopCh:   make(chan struct{}),
	}
	if n != nil {
		h.now = n.nowTimeGetter
	}
	if c.PresenceTTL > 0 {
		go h.runExpireLoop()
	}
	return h
}

func (h *presenceHub) userEventsEnabled(ch string) bool {
	return h.config.EnableUserPresenceEvents != nil && h.config.EnableUserPresenceEvents(ch)
}

func (h *presenceHub) add(ch string, clientID string, info *ClientInfo) error {
	var joinEvent *UserPresenceJoinEvent

	h.mu.Lock()

	entries, ok := h.presence[ch]
	if !ok {
		entries = make(map[string]*memoryPresenceEntry)
		h.presence[ch] = entries
	}

	entry := &memoryPresenceEntry{info: info}
	if h.config.PresenceTTL > 0 {
		entry.expireAt = h.now().Add(h.config.PresenceTTL).UnixNano()
	}
	entries[clientID] = entry

	userID := ""
	if info != nil {
		userID = info.UserID
	}
	if h.userEventsEnabled(ch) && userID != "" {
		userStates := h.users[ch]
		if userStates == nil {
			userStates = make(map[string]*memoryUserPresence)
			h.users[ch] = userStates
		}
		state, exists := userStates[userID]
		if !exists {
			state = &memoryUserPresence{clients: make(map[string]struct{})}
			userStates[userID] = state
		}
		if state.pendingLeave != nil {
			// Reconnect inside the grace period: cancel the pending leave and
			// do not re-join.
			state.pendingLeave.Stop()
			state.pendingLeave = nil
		}
		_, counted := state.clients[clientID]
		if !counted {
			state.clients[clientID] = struct{}{}
			if !exists {
				ev := UserPresenceJoinEvent{Channel: ch, UserID: userID, ClientID: clientID, Info: info}
				joinEvent = &ev
			}
		}
	}

	h.mu.Unlock()

	if joinEvent != nil && h.node != nil {
		h.node.notifyUserPresenceJoin(*joinEvent)
	}
	return nil
}

func (h *presenceHub) remove(ch string, clientID string) error {
	var leaveEvent *UserPresenceLeaveEvent

	h.mu.Lock()

	entries, ok := h.presence[ch]
	if !ok {
		h.mu.Unlock()
		return nil
	}
	entry, ok := entries[clientID]
	if !ok {
		h.mu.Unlock()
		return nil
	}

	delete(entries, clientID)
	if len(entries) == 0 {
		delete(h.presence, ch)
	}

	userID := ""
	if entry.info != nil {
		userID = entry.info.UserID
	}
	if h.userEventsEnabled(ch) && userID != "" {
		if state, ok := h.users[ch][userID]; ok {
			if _, counted := state.clients[clientID]; counted {
				delete(state.clients, clientID)
				if len(state.clients) == 0 && state.pendingLeave == nil {
					leaveEvent = h.scheduleUserLeaveLocked(ch, userID, state)
				}
			}
		}
	}

	h.mu.Unlock()

	if leaveEvent != nil && h.node != nil {
		h.node.notifyUserPresenceLeave(*leaveEvent)
	}
	return nil
}

// scheduleUserLeaveLocked starts the leave grace period for a user whose
// connection count reached zero, or returns an event to dispatch immediately
// when no grace period is configured. Must be called with the hub write lock
// held. When a timer is started, dispatch happens later from the timer
// goroutine, after it re-checks the state under the lock.
func (h *presenceHub) scheduleUserLeaveLocked(ch, userID string, state *memoryUserPresence) *UserPresenceLeaveEvent {
	grace := h.config.UserLeaveGracePeriod
	if grace <= 0 {
		h.deleteUserLocked(ch, userID)
		return &UserPresenceLeaveEvent{Channel: ch, UserID: userID}
	}
	state.pendingLeave = time.AfterFunc(grace, func() {
		var event *UserPresenceLeaveEvent
		h.mu.Lock()
		if userStates := h.users[ch]; userStates != nil {
			if st, ok := userStates[userID]; ok && st.pendingLeave != nil {
				h.deleteUserLocked(ch, userID)
				event = &UserPresenceLeaveEvent{Channel: ch, UserID: userID}
			}
		}
		h.mu.Unlock()
		if event != nil && h.node != nil {
			h.node.notifyUserPresenceLeave(*event)
		}
	})
	return nil
}

// deleteUserLocked removes a user's state for a channel. Must be called with
// the hub write lock held.
func (h *presenceHub) deleteUserLocked(ch, userID string) {
	if userStates := h.users[ch]; userStates != nil {
		if state, ok := userStates[userID]; ok {
			state.pendingLeave = nil
			delete(userStates, userID)
			if len(userStates) == 0 {
				delete(h.users, ch)
			}
		}
	}
}

// runExpireLoop periodically drops presence entries whose TTL elapsed. It is
// only started when PresenceTTL is positive.
func (h *presenceHub) runExpireLoop() {
	interval := h.config.PresenceTTL / 3
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if h.config.PresenceCleanupInterval > 0 {
		interval = h.config.PresenceCleanupInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.expireEntries()
		}
	}
}

// expireEntries removes all presence entries past their TTL and drives the same
// per-user counting logic as an explicit remove, so natural expiry produces
// exactly the same user leave events as a disconnect or unsubscribe.
func (h *presenceHub) expireEntries() {
	var leaveEvents []UserPresenceLeaveEvent

	h.mu.Lock()
	now := h.now().UnixNano()
	for ch, entries := range h.presence {
		for clientID, entry := range entries {
			if entry.expireAt == 0 || entry.expireAt > now {
				continue
			}
			delete(entries, clientID)
			if len(entries) == 0 {
				delete(h.presence, ch)
			}

			userID := ""
			if entry.info != nil {
				userID = entry.info.UserID
			}
			if !h.userEventsEnabled(ch) || userID == "" {
				continue
			}
			state, ok := h.users[ch][userID]
			if !ok {
				continue
			}
			if _, counted := state.clients[clientID]; !counted {
				continue
			}
			delete(state.clients, clientID)
			if len(state.clients) == 0 && state.pendingLeave == nil {
				if ev := h.scheduleUserLeaveLocked(ch, userID, state); ev != nil {
					leaveEvents = append(leaveEvents, *ev)
				}
			}
		}
	}
	h.mu.Unlock()

	if h.node != nil {
		for _, ev := range leaveEvents {
			h.node.notifyUserPresenceLeave(ev)
		}
	}
}

func (h *presenceHub) stop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
	h.mu.Lock()
	for _, userStates := range h.users {
		for _, state := range userStates {
			if state.pendingLeave != nil {
				state.pendingLeave.Stop()
				state.pendingLeave = nil
			}
		}
	}
	h.mu.Unlock()
}

func (h *presenceHub) get(ch string) (map[string]*ClientInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	entries, ok := h.presence[ch]
	if !ok {
		// return empty map
		return nil, nil
	}

	data := make(map[string]*ClientInfo, len(entries))
	for k, v := range entries {
		data[k] = v.info
	}
	return data, nil
}

func (h *presenceHub) getStats(ch string) (PresenceStats, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	entries, ok := h.presence[ch]
	if !ok {
		// return empty map
		return PresenceStats{}, nil
	}

	numClients := len(entries)
	numUsers := 0
	uniqueUsers := map[string]struct{}{}

	for _, entry := range entries {
		if entry.info == nil {
			continue
		}
		userID := entry.info.UserID
		if _, ok := uniqueUsers[userID]; !ok {
			uniqueUsers[userID] = struct{}{}
			numUsers++
		}
	}

	return PresenceStats{
		NumClients: numClients,
		NumUsers:   numUsers,
	}, nil
}
