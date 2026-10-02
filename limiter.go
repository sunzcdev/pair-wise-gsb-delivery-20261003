package smtp

import (
	"errors"
	"net"
	"sync"
)

// Limiter tracks and restricts the number of concurrent server connections.
//
// Two independent quotas are enforced:
//
//   - Connections that have not completed authentication are attributed to
//     their source IP address.
//   - Once authentication succeeds, a connection is moved atomically from the
//     source IP bucket to the authenticated user's bucket.
//
// A Limiter may be implemented against an in-process map or a shared data
// store (e.g. Redis) so that quota accounting can be reused by other services.
// All methods must be safe for concurrent use.
//
// Reservation lifecycle:
//
//   - Begin reserves a slot when a connection is accepted.
//   - Authenticate atomically releases the source IP slot and reserves a user
//     slot; if the user quota is exhausted, no slot changes hands.
//   - Deauthenticate moves an authenticated connection back to its source IP
//     bucket, for instance when STARTTLS resets the session state.
//   - End releases whatever slot the connection currently holds when it is
//     closed.
//
// The quota errors returned by an implementation are mapped by the server to
// the SMTP response appropriate for the command being processed.
type Limiter interface {
	// Begin attributes a new connection to its source IP and reserves a slot
	// in that bucket. It returns ErrSourceLimitExceeded (or another error) if
	// the connection must be rejected. A slot is reserved only when nil is
	// returned.
	Begin(remote net.IP) error

	// Authenticate atomically moves a connection from the source IP bucket to
	// the authenticated user's bucket. If reserving a slot for username fails,
	// the connection keeps its source IP slot and the authentication attempt
	// is rejected.
	Authenticate(remote net.IP, username string) error

	// Deauthenticate moves an authenticated connection back into the source IP
	// bucket. If the source IP quota is full, the connection keeps its user
	// slot and the returned error must be handled by closing the connection.
	Deauthenticate(remote net.IP, username string) error

	// End releases the slot currently held by a connection: the user slot when
	// username is non-empty, otherwise the source IP slot.
	End(remote net.IP, username string)
}

// Quota errors. Limiter implementations may return these sentinel errors or
// errors wrapping them; the server maps them to SMTP responses.
var (
	// ErrSourceLimitExceeded is returned when the source IP connection quota
	// is exhausted.
	ErrSourceLimitExceeded = errors.New("smtp: too many connections from this address")

	// ErrUserLimitExceeded is returned when the authenticated user connection
	// quota is exhausted.
	ErrUserLimitExceeded = errors.New("smtp: too many authenticated sessions for this user")
)

// CanonicalIP normalizes an IP address used as a source IP bucket key.
//
// An IPv4-mapped IPv6 address (for example ::ffff:203.0.113.5) denotes the
// same host as the embedded IPv4 address, so both forms collapse to the same
// 4-byte representation. A nil IP is returned as-is, which limiters may treat
// as "no source address" (e.g. for Unix domain sockets).
func CanonicalIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

// MemoryLimiter is a Limiter backed by in-process maps.
//
// It only coordinates connections handled by this Server process: counts are
// not shared with other processes or nodes and do not provide a cluster-wide
// guarantee. For that, implement Limiter on top of a shared data store.
type MemoryLimiter struct {
	mu sync.Mutex

	// Maximum number of concurrent unauthenticated connections per source
	// IP. A value <= 0 disables the limit.
	maxConnsPerSource int
	// Maximum number of concurrent authenticated connections per user. A
	// value <= 0 disables the limit.
	maxConnsPerUser int

	sources map[string]int
	users   map[string]int
}

// NewMemoryLimiter creates a MemoryLimiter allowing at most
// maxConnsPerSource concurrent unauthenticated connections from a single
// source IP and at most maxConnsPerUser concurrent authenticated connections
// for a single user. A non-positive value disables the corresponding limit.
func NewMemoryLimiter(maxConnsPerSource, maxConnsPerUser int) *MemoryLimiter {
	return &MemoryLimiter{
		maxConnsPerSource: maxConnsPerSource,
		maxConnsPerUser:   maxConnsPerUser,
		sources:           make(map[string]int),
		users:             make(map[string]int),
	}
}

// Begin implements Limiter.
func (l *MemoryLimiter) Begin(remote net.IP) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := sourceKey(remote)
	if key == "" {
		return nil
	}

	if l.maxConnsPerSource > 0 && l.sources[key] >= l.maxConnsPerSource {
		return ErrSourceLimitExceeded
	}
	l.sources[key]++
	return nil
}

// Authenticate implements Limiter. The source bucket release and user bucket
// reservation happen under a single lock, so a full user bucket can never be
// temporarily overbooked.
func (l *MemoryLimiter) Authenticate(remote net.IP, username string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if username == "" {
		return nil
	}
	if l.maxConnsPerUser > 0 && l.users[username] >= l.maxConnsPerUser {
		return ErrUserLimitExceeded
	}

	if key := sourceKey(remote); key != "" {
		l.sources[key]--
		if l.sources[key] <= 0 {
			delete(l.sources, key)
		}
	}
	l.users[username]++
	return nil
}

// Deauthenticate implements Limiter.
func (l *MemoryLimiter) Deauthenticate(remote net.IP, username string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := sourceKey(remote)
	if key != "" && l.maxConnsPerSource > 0 && l.sources[key] >= l.maxConnsPerSource {
		return ErrSourceLimitExceeded
	}

	l.users[username]--
	if l.users[username] <= 0 {
		delete(l.users, username)
	}
	if key != "" {
		l.sources[key]++
	}
	return nil
}

// End implements Limiter.
func (l *MemoryLimiter) End(remote net.IP, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if username != "" {
		l.users[username]--
		if l.users[username] <= 0 {
			delete(l.users, username)
		}
		return
	}

	if key := sourceKey(remote); key != "" {
		l.sources[key]--
		if l.sources[key] <= 0 {
			delete(l.sources, key)
		}
	}
}

// SourceConns returns the number of currently tracked connections attributed
// to the given source IP. It is mainly intended for tests and metrics.
func (l *MemoryLimiter) SourceConns(remote net.IP) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sources[sourceKey(remote)]
}

// UserConns returns the number of currently tracked authenticated connections
// attributed to username. It is mainly intended for tests and metrics.
func (l *MemoryLimiter) UserConns(username string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.users[username]
}

func sourceKey(remote net.IP) string {
	ip := CanonicalIP(remote)
	if ip == nil {
		return ""
	}
	return ip.String()
}
