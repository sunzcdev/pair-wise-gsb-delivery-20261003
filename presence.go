package centrifuge

// UserPresenceType describes a user-level presence transition.
//
// Unlike connection-level join/leave events (emitted per client connection),
// user presence is coalesced across all connections of the same user inside a
// channel: a user "enters" a channel when their first connection subscribes,
// and "leaves" only after their last connection is gone (plus an optional
// grace period that absorbs quick reconnects).
type UserPresenceType int

const (
	// UserPresenceEnter means a user appeared in a channel: the connection
	// count for that (channel, user) pair transitioned from zero to one.
	UserPresenceEnter UserPresenceType = iota
	// UserPresenceLeave means a user disappeared from a channel: the
	// connection count transitioned to zero and stayed there through the
	// configured grace period (or immediately when no grace period is set).
	UserPresenceLeave
)

// UserPresenceEvent is a user-level presence transition. It only describes
// online/offline status; Centrifuge does not promise any message delivery for
// the period a user was disconnected.
type UserPresenceEvent struct {
	// Channel the event belongs to.
	Channel string
	// Type is the kind of transition: enter or leave.
	Type UserPresenceType
	// UserID is the identifier of the user whose presence changed.
	UserID string
	// Info is the ClientInfo of the connection that triggered the enter event.
	// For leave events it carries the user identifier only; other fields may be
	// empty (the originating connection is usually already gone).
	Info *ClientInfo
}

// UserPresenceHandler is a server-side callback invoked once per user-level
// presence transition. In a multi-node deployment the handler is invoked on
// every running node, so user-facing effects (for example a push to other
// backends) must be idempotent or deduplicated by the application.
type UserPresenceHandler func(UserPresenceEvent)

// PresenceStats represents a short presence information for channel.
type PresenceStats struct {
	// NumClients is a number of client connections in channel.
	NumClients int
	// NumUsers is a number of unique users in channel.
	NumUsers int
}

// PresenceManager is responsible for channel presence management.
type PresenceManager interface {
	// Presence returns actual presence information for channel.
	Presence(ch string) (map[string]*ClientInfo, error)
	// PresenceStats returns short stats of current presence data
	// suitable for scenarios when caller does not need full client
	// info returned by presence method.
	PresenceStats(ch string) (PresenceStats, error)
	// AddPresence sets or updates presence information in channel
	// for connection with specified identifier. PresenceManager should
	// have a property to expire client information that was not updated
	// (touched) after some configured time interval.
	AddPresence(ch string, clientID string, info *ClientInfo) error
	// RemovePresence removes presence information for connection
	// with specified client and user identifiers.
	RemovePresence(ch string, clientID string, userID string) error
}

// UserPresenceManager is an optional PresenceManager extension that maintains
// per-user connection counts and reports user-level presence transitions.
//
// The contract requires counting to be atomic with respect to concurrent
// updates coming from different nodes. Implementations must not rely on a
// read-then-decrement sequence performed by the caller: the decision whether a
// transition happened (0 -> 1 on add, 1 -> 0 on remove or natural expiry) and
// the counter mutation must be one atomic operation.
//
// When the last connection of a user disappears and a grace period greater
// than zero is configured, the leave transition must be deferred. A reconnect
// (AddPresence) that lands during the grace period must cancel the pending
// leave atomically. Explicit unsubscribes, abnormal connection drops and
// natural presence TTL expiry all decrement the same counter and share this
// semantics.
type UserPresenceManager interface {
	PresenceManager

	// AddUserPresence registers a connection for the (channel, user) pair and
	// atomically reports the resulting user-level transition. It returns
	// UserPresenceEnter when the user previously had no live connections and no
	// pending leave, and no event when the user was already present or a pending
	// leave was cancelled by this reconnect.
	AddUserPresence(ch string, clientID string, info *ClientInfo) (UserPresenceEvent, bool, error)
	// RemoveUserPresence unregisters a connection for the (channel, user) pair
	// and atomically reports the resulting user-level transition. It returns
	// UserPresenceLeave only when this removed the last live connection. When a
	// grace period is configured the returned boolean is false and the leave is
	// scheduled instead; the implementation is responsible for emitting it via
	// the provided emitter once the grace period elapses without a reconnect.
	RemoveUserPresence(ch string, clientID string, userID string) (UserPresenceEvent, bool, error)
	// SetUserPresenceEmitter registers the sink for transitions that are not
	// returned synchronously — i.e. deferred grace-period leaves and leaves
	// produced by natural TTL expiry. The Node wires this to its cluster-wide
	// user presence fan-out.
	SetUserPresenceEmitter(emit func(UserPresenceEvent))
}
