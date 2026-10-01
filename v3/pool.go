package ldap

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrPoolClosed is returned by Pool.Get and Pool.Put after Pool.Close has
// been called.
var ErrPoolClosed = errors.New("ldap: connection pool is closed")

// PoolKey identifies a class of pooled connections. Connections are only
// shared between borrowers that use equal keys, so the key carries both the
// authentication identity the connection must be bound as and whether the
// connection must be TLS-protected.
type PoolKey struct {
	// Username is the bind DN the connection must be authenticated as.
	// An empty username (with an empty password) means anonymous.
	Username string
	// Password is the credential used to (re)establish Username.
	Password string
	// TLS requires the connection to be TLS-protected (ldaps or StartTLS).
	TLS bool
}

// Pool is an optional connection pool for backends that issue repeated
// searches. It hands out *Conn values keyed by PoolKey and validates every
// returned connection before making it available again: a borrower that
// changed the bind state is detected via the Who Am I? extended operation
// and the pool re-binds the connection to the key identity. If the identity
// cannot be re-established the connection is closed and discarded - a merely
// alive TCP connection is never treated as a clean one.
//
// The zero value is not usable; create a pool with NewPool. A Pool is safe
// for concurrent use.
type Pool struct {
	dial    func() (*Conn, error)
	maxIdle int

	mu     sync.Mutex
	idle   map[PoolKey][]*Conn
	closed bool
}

// NewPool returns a Pool that creates connections with dial. dial must
// return a started connection (e.g. the result of DialURL); the pool binds
// it to the requested PoolKey identity. Up to maxIdlePerKey connections per
// key are kept for reuse; additional returned connections are closed.
func NewPool(dial func() (*Conn, error), maxIdlePerKey int) *Pool {
	if maxIdlePerKey < 0 {
		maxIdlePerKey = 0
	}
	return &Pool{
		dial:    dial,
		maxIdle: maxIdlePerKey,
		idle:    make(map[PoolKey][]*Conn),
	}
}

// Get returns a connection authenticated as the identity described by key,
// either reused from the pool or freshly dialed and bound. It returns
// ErrPoolClosed after Close.
func (p *Pool) Get(key PoolKey) (*Conn, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrPoolClosed
		}
		conns := p.idle[key]
		if len(conns) == 0 {
			p.mu.Unlock()
			break
		}
		conn := conns[len(conns)-1]
		p.idle[key] = conns[:len(conns)-1]
		p.mu.Unlock()

		if p.reusable(key, conn) {
			return conn, nil
		}
		_ = conn.Close()
	}

	conn, err := p.dial()
	if err != nil {
		return nil, err
	}
	if !poolTLSMatches(key, conn) {
		_ = conn.Close()
		return nil, fmt.Errorf("ldap: pooled connection does not meet TLS requirement of %t", key.TLS)
	}
	if key.Username != "" || key.Password != "" {
		if err := bindPoolIdentity(key, conn); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("ldap: pool could not bind new connection: %w", err)
		}
	}
	return conn, nil
}

// Put returns conn to the pool under key. Before the connection is made
// available again the pool verifies that it is still bound as the key
// identity; if the borrower changed the bind state the pool re-binds with
// the key credentials. Connections that fail verification or re-binding are
// closed and discarded, as are connections returned after Close.
func (p *Pool) Put(key PoolKey, conn *Conn) error {
	if conn == nil {
		return nil
	}
	if !p.reusable(key, conn) {
		_ = conn.Close()
		return fmt.Errorf("ldap: pooled connection for %q is not reusable, discarding", key.Username)
	}
	if err := ensurePoolIdentity(key, conn); err != nil {
		_ = conn.Close()
		return err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = conn.Close()
		return ErrPoolClosed
	}
	if len(p.idle[key]) >= p.maxIdle {
		p.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	p.idle[key] = append(p.idle[key], conn)
	p.mu.Unlock()
	return nil
}

// Close shuts the pool down and closes every idle connection. Connections
// still borrowed are closed when they are returned via Put. Get and Put
// return ErrPoolClosed after Close.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	idle := p.idle
	p.idle = make(map[PoolKey][]*Conn)
	p.mu.Unlock()

	var firstErr error
	for _, conns := range idle {
		for _, conn := range conns {
			if err := conn.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// IdleCount reports how many connections are currently idle for key.
func (p *Pool) IdleCount(key PoolKey) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle[key])
}

// reusable reports whether conn is healthy enough to be (re)used for key:
// not closing and matching the TLS requirement of the key. GetLastError is
// deliberately not consulted: it also records benign events such as late
// responses for abandoned asynchronous searches, and a failed read loop
// marks the connection as closing anyway. Cleanliness of a returned
// connection is established by the Who Am I? round-trip in
// ensurePoolIdentity.
func (p *Pool) reusable(key PoolKey, conn *Conn) bool {
	if conn.IsClosing() {
		return false
	}
	return poolTLSMatches(key, conn)
}

func poolTLSMatches(key PoolKey, conn *Conn) bool {
	_, isTLS := conn.TLSConnectionState()
	return isTLS == key.TLS
}

// ensurePoolIdentity verifies conn is bound as the key identity and restores
// that identity if a borrower changed the bind state. Any failure closes the
// caller's trust in the connection: the connection must be discarded.
func ensurePoolIdentity(key PoolKey, conn *Conn) error {
	whoami, err := conn.WhoAmI(nil)
	if err != nil {
		return fmt.Errorf("ldap: pool could not verify connection identity: %w", err)
	}
	if poolAuthzIDMatches(whoami.AuthzID, key.Username) {
		return nil
	}
	if err := bindPoolIdentity(key, conn); err != nil {
		return fmt.Errorf("ldap: pool could not restore connection identity %q: %w", key.Username, err)
	}
	return nil
}

func bindPoolIdentity(key PoolKey, conn *Conn) error {
	if key.Username == "" && key.Password == "" {
		// An anonymous bind resets any previous authenticated state.
		_, err := conn.SimpleBind(&SimpleBindRequest{AllowEmptyPassword: true})
		return err
	}
	return conn.Bind(key.Username, key.Password)
}

// poolAuthzIDMatches compares a Who Am I? authzId with the pool identity.
// The "dn:" form is compared case-insensitively; an empty username expects
// the anonymous (empty) authzId.
func poolAuthzIDMatches(authzID, username string) bool {
	if username == "" {
		return authzID == ""
	}
	if dn, ok := strings.CutPrefix(authzID, "dn:"); ok {
		return strings.EqualFold(dn, username)
	}
	if user, ok := strings.CutPrefix(authzID, "u:"); ok {
		return user == username
	}
	return false
}
