package smtp

import (
	"errors"
	"net"
	"sync"
)

// ErrConnLimit is returned by ConnLimiter implementations when a connection
// cannot be admitted because a limit has been reached.
var ErrConnLimit = errors.New("smtp: connection limit exceeded")

// ConnLimiter limits the number of concurrent connections handled by
// a Server.
//
// Implementations must be safe for concurrent use.
type ConnLimiter interface {
	// LimitConn is called when a new connection is accepted, before the
	// greeting is sent. ip is the source IP address of the client, it may
	// be nil if it cannot be determined (e.g. for Unix sockets).
	//
	// If the connection is admitted, LimitConn returns a lease tracking
	// it. Otherwise an error is returned and the connection is rejected.
	LimitConn(ip net.IP) (ConnLimitLease, error)
}

// ConnLimitLease tracks a single admitted connection.
//
// A lease initially counts towards the unauthenticated (per-source-IP)
// group. After a successful AUTH command the connection is moved to the
// authenticated (per-user) group with SetUser.
//
// Implementations must be safe for concurrent use.
type ConnLimitLease interface {
	// SetUser moves the connection from the unauthenticated group to the
	// authenticated group for the given user. If the per-user limit is
	// reached, an error is returned and the lease remains in the
	// unauthenticated group: the move is atomic and never temporarily
	// exceeds either limit.
	SetUser(username string) error

	// UnsetUser moves the connection back to the unauthenticated group.
	// It is called when the connection state is reset, e.g. after
	// STARTTLS.
	UnsetUser()

	// Free releases the lease. It is called when the connection is
	// closed. Free is idempotent.
	Free()
}

// AuthIdentitySession is an optional add-on interface for Session.
//
// If implemented, the identity returned by AuthIdentity is used as the
// per-user key for connection limiting after a successful AUTH command.
// Sessions not implementing this interface are only accounted for in the
// unauthenticated per-source-IP group.
type AuthIdentitySession interface {
	Session

	// AuthIdentity returns the identity of the user authenticated via
	// the AUTH command. It is only called after a successful
	// authentication. An empty string disables per-user accounting for
	// the connection.
	AuthIdentity() string
}

// MemoryLimiter is a ConnLimiter implementation keeping counters in
// process memory.
//
// It only limits connections accepted by this process: the counters are
// not shared with other nodes and do not provide any cluster-wide
// guarantees. Deployments running multiple nodes need a ConnLimiter
// implementation backed by shared storage to enforce global limits.
//
// The zero value is valid and imposes no limits.
type MemoryLimiter struct {
	// Maximum number of unauthenticated connections per source IP
	// address. IPv4-mapped IPv6 addresses are treated as IPv4 addresses.
	// Zero or negative means no limit.
	MaxPerIP int
	// Maximum number of authenticated connections per user. Zero or
	// negative means no limit.
	MaxPerUser int

	locker sync.Mutex
	ips    map[string]int
	users  map[string]int
}

// LimitConn implements ConnLimiter.
func (l *MemoryLimiter) LimitConn(ip net.IP) (ConnLimitLease, error) {
	key := limiterIPKey(ip)

	l.locker.Lock()
	defer l.locker.Unlock()

	if l.ips == nil {
		l.ips = make(map[string]int)
		l.users = make(map[string]int)
	}

	if l.MaxPerIP > 0 && l.ips[key] >= l.MaxPerIP {
		return nil, ErrConnLimit
	}
	l.ips[key]++

	return &memoryLimitLease{limiter: l, ip: key}, nil
}

// limiterIPKey converts an IP address to its canonical map key. IPv4-mapped
// IPv6 addresses are folded into their IPv4 form so that both
// representations share the same quota.
func limiterIPKey(ip net.IP) string {
	if ip == nil {
		// Connections without a determinable source IP (e.g. Unix
		// sockets) share a single bucket.
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return string(v4)
	}
	return string(ip.To16())
}

type memoryLimitLease struct {
	limiter *MemoryLimiter
	ip      string
	user    string
	freed   bool
}

func (l *memoryLimitLease) SetUser(username string) error {
	l.limiter.locker.Lock()
	defer l.limiter.locker.Unlock()

	if l.freed {
		return errors.New("smtp: connection limit lease already released")
	}
	if l.user != "" {
		// Already in the authenticated group.
		return nil
	}
	if l.limiter.MaxPerUser > 0 && l.limiter.users[username] >= l.limiter.MaxPerUser {
		return ErrConnLimit
	}

	l.limiter.users[username]++
	l.limiter.decIP(l.ip)
	l.user = username
	return nil
}

func (l *memoryLimitLease) UnsetUser() {
	l.limiter.locker.Lock()
	defer l.limiter.locker.Unlock()

	if l.freed || l.user == "" {
		return
	}

	l.limiter.decUser(l.user)
	// The connection moves back to the unauthenticated group
	// unconditionally: it already exists, so the per-IP limit may be
	// temporarily exceeded here. New connections are still rejected
	// until the count drops below the limit again.
	l.limiter.ips[l.ip]++
	l.user = ""
}

func (l *memoryLimitLease) Free() {
	l.limiter.locker.Lock()
	defer l.limiter.locker.Unlock()

	if l.freed {
		return
	}
	l.freed = true

	if l.user != "" {
		l.limiter.decUser(l.user)
		l.user = ""
	} else {
		l.limiter.decIP(l.ip)
	}
}

func (l *MemoryLimiter) decIP(key string) {
	l.ips[key]--
	if l.ips[key] <= 0 {
		delete(l.ips, key)
	}
}

func (l *MemoryLimiter) decUser(key string) {
	l.users[key]--
	if l.users[key] <= 0 {
		delete(l.users, key)
	}
}

// connRemoteIP extracts the source IP address of a connection. It returns
// nil if the address cannot be determined or is not an IP address.
func connRemoteIP(c net.Conn) net.IP {
	addr := c.RemoteAddr()
	if addr == nil {
		return nil
	}
	if tcpAddr, ok := addr.(*net.TCPAddr); ok {
		return tcpAddr.IP
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	return net.ParseIP(host)
}
