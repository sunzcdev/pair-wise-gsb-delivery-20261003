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
	node            *Node
	config          MemoryPresenceManagerConfig
	presenceHub     *presenceHub
	userPresenceHub *userPresenceHub
}

var _ PresenceManager = (*MemoryPresenceManager)(nil)

// MemoryPresenceManagerConfig is a MemoryPresenceManager config.
type MemoryPresenceManagerConfig struct {
	// UserPresenceGracePeriod is the delay between the moment the last
	// connection of a user disappears from a channel and the user-level leave
	// event. A reconnect landing in this window cancels the pending leave, so
	// quick page reloads and network blips do not flap user presence.
	// Zero means emit the user leave immediately.
	UserPresenceGracePeriod time.Duration
}

// NewMemoryPresenceManager initializes MemoryPresenceManager.
func NewMemoryPresenceManager(n *Node, c MemoryPresenceManagerConfig) (*MemoryPresenceManager, error) {
	return &MemoryPresenceManager{
		node:            n,
		config:          c,
		presenceHub:     newPresenceHub(),
		userPresenceHub: newUserPresenceHub(c.UserPresenceGracePeriod),
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

var _ UserPresenceManager = (*MemoryPresenceManager)(nil)

// AddUserPresence - see UserPresenceManager interface description.
func (m *MemoryPresenceManager) AddUserPresence(ch string, clientID string, info *ClientInfo) (UserPresenceEvent, bool, error) {
	return m.userPresenceHub.add(ch, clientID, info)
}

// RemoveUserPresence - see UserPresenceManager interface description.
func (m *MemoryPresenceManager) RemoveUserPresence(ch string, clientID string, userID string) (UserPresenceEvent, bool, error) {
	return m.userPresenceHub.remove(ch, clientID, userID)
}

// SetUserPresenceEmitter - see UserPresenceManager interface description.
func (m *MemoryPresenceManager) SetUserPresenceEmitter(emit func(UserPresenceEvent)) {
	m.userPresenceHub.emit = emit
}

// Close stops pending grace-period timers.
func (m *MemoryPresenceManager) Close(_ context.Context) error {
	m.userPresenceHub.close()
	return nil
}

type presenceHub struct {
	sync.RWMutex
	presence map[string]map[string]*ClientInfo
}

func newPresenceHub() *presenceHub {
	return &presenceHub{
		presence: make(map[string]map[string]*ClientInfo),
	}
}

func (h *presenceHub) add(ch string, uid string, info *ClientInfo) error {
	h.Lock()
	defer h.Unlock()

	_, ok := h.presence[ch]
	if !ok {
		h.presence[ch] = make(map[string]*ClientInfo)
	}
	h.presence[ch][uid] = info
	return nil
}

func (h *presenceHub) remove(ch string, uid string) error {
	h.Lock()
	defer h.Unlock()

	if _, ok := h.presence[ch]; !ok {
		return nil
	}
	if _, ok := h.presence[ch][uid]; !ok {
		return nil
	}

	delete(h.presence[ch], uid)

	// clean up map if needed
	if len(h.presence[ch]) == 0 {
		delete(h.presence, ch)
	}

	return nil
}

func (h *presenceHub) get(ch string) (map[string]*ClientInfo, error) {
	h.RLock()
	defer h.RUnlock()

	presence, ok := h.presence[ch]
	if !ok {
		// return empty map
		return nil, nil
	}

	data := make(map[string]*ClientInfo, len(presence))
	for k, v := range presence {
		data[k] = v
	}
	return data, nil
}

func (h *presenceHub) getStats(ch string) (PresenceStats, error) {
	h.RLock()
	defer h.RUnlock()

	presence, ok := h.presence[ch]
	if !ok {
		// return empty map
		return PresenceStats{}, nil
	}

	numClients := len(presence)
	numUsers := 0
	uniqueUsers := map[string]struct{}{}

	for _, info := range presence {
		userID := info.UserID
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

// userPresenceHub keeps per-user connection counts for channels and schedules
// deferred leave events. All state is guarded by a single mutex, and the grace
// timer re-checks the count under the same mutex before firing, so explicit
// unsubscribes, abnormal drops and expiry share one counting semantics.
type userPresenceHub struct {
	mu          sync.Mutex
	gracePeriod time.Duration
	emit        func(UserPresenceEvent)
	// users maps channel -> userID -> user presence state.
	users map[string]map[string]*userPresenceState
}

type userPresenceState struct {
	// clients is the set of live client connections of this user in channel.
	clients map[string]struct{}
	// info is the ClientInfo recorded on the enter transition.
	info *ClientInfo
	// leaveTimer is set while a leave is pending during the grace period.
	leaveTimer *time.Timer
}

func newUserPresenceHub(gracePeriod time.Duration) *userPresenceHub {
	return &userPresenceHub{
		gracePeriod: gracePeriod,
		users:       make(map[string]map[string]*userPresenceState),
	}
}

func (h *userPresenceHub) add(ch string, clientID string, info *ClientInfo) (UserPresenceEvent, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	userID := info.UserID
	channelUsers, ok := h.users[ch]
	if !ok {
		channelUsers = make(map[string]*userPresenceState)
		h.users[ch] = channelUsers
	}

	state, existed := channelUsers[userID]
	if !existed {
		state = &userPresenceState{
			clients: make(map[string]struct{}),
			info:    info,
		}
		channelUsers[userID] = state
	} else {
		state.info = info
	}

	_, alreadyThere := state.clients[clientID]
	if state.leaveTimer != nil {
		// A reconnect inside the grace window cancels the pending leave. It
		// must not produce a duplicate enter event.
		state.leaveTimer.Stop()
		state.leaveTimer = nil
	}
	state.clients[clientID] = struct{}{}

	if !existed && !alreadyThere {
		// Genuine 0 -> 1 transition.
		return UserPresenceEvent{
			Channel: ch,
			Type:    UserPresenceEnter,
			UserID:  userID,
			Info:    info,
		}, true, nil
	}
	return UserPresenceEvent{}, false, nil
}

func (h *userPresenceHub) remove(ch string, clientID string, userID string) (UserPresenceEvent, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	channelUsers, ok := h.users[ch]
	if !ok {
		return UserPresenceEvent{}, false, nil
	}
	state, ok := channelUsers[userID]
	if !ok {
		return UserPresenceEvent{}, false, nil
	}
	if _, ok := state.clients[clientID]; !ok {
		return UserPresenceEvent{}, false, nil
	}

	delete(state.clients, clientID)
	if len(state.clients) > 0 {
		return UserPresenceEvent{}, false, nil
	}

	leaveEvent := UserPresenceEvent{
		Channel: ch,
		Type:    UserPresenceLeave,
		UserID:  userID,
		Info:    state.info,
	}

	if h.gracePeriod <= 0 {
		h.deleteStateLocked(ch, userID)
		return leaveEvent, true, nil
	}

	h.scheduleLeaveLocked(ch, userID, state)
	return UserPresenceEvent{}, false, nil
}

func (h *userPresenceHub) scheduleLeaveLocked(ch, userID string, state *userPresenceState) {
	state.leaveTimer = time.AfterFunc(h.gracePeriod, func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		channelUsers, ok := h.users[ch]
		if !ok {
			return
		}
		currentState, ok := channelUsers[userID]
		if !ok || currentState.leaveTimer == nil || len(currentState.clients) > 0 {
			// A reconnect cancelled this leave.
			return
		}

		event := UserPresenceEvent{
			Channel: ch,
			Type:    UserPresenceLeave,
			UserID:  userID,
			Info:    currentState.info,
		}
		h.deleteStateLocked(ch, userID)
		if h.emit != nil {
			h.emit(event)
		}
	})
}

func (h *userPresenceHub) deleteStateLocked(ch, userID string) {
	channelUsers, ok := h.users[ch]
	if !ok {
		return
	}
	delete(channelUsers, userID)
	if len(channelUsers) == 0 {
		delete(h.users, ch)
	}
}

func (h *userPresenceHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, channelUsers := range h.users {
		for _, state := range channelUsers {
			if state.leaveTimer != nil {
				state.leaveTimer.Stop()
				state.leaveTimer = nil
			}
		}
	}
	h.users = make(map[string]map[string]*userPresenceState)
}
