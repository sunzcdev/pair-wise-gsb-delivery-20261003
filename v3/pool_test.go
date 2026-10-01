package ldap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/stretchr/testify/require"
)

// poolTestServer is a minimal in-process LDAP server for pool tests. It
// handles simple binds, Who Am I? and searches, and records the bind
// identity that was in effect for every search it serves.
type poolTestServer struct {
	ln net.Listener

	mu          sync.Mutex
	searchLog   []string // bound identity per search, in serve order
	bindLog     []string // accepted bind usernames, in serve order
	rejectBinds map[string]bool
	connCount   int
	searchSeq   int

	entries    int
	entryDelay time.Duration
}

func newPoolTestServer(t *testing.T, entries int, entryDelay time.Duration) *poolTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &poolTestServer{
		ln:          ln,
		rejectBinds: make(map[string]bool),
		entries:     entries,
		entryDelay:  entryDelay,
	}
	go server.acceptLoop()
	t.Cleanup(func() { _ = server.ln.Close() })
	return server
}

func (s *poolTestServer) url() string {
	return "ldap://" + s.ln.Addr().String()
}

func (s *poolTestServer) searchIdentities() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.searchLog...)
}

func (s *poolTestServer) bindCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bindLog)
}

func (s *poolTestServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connCount
}

func (s *poolTestServer) setRejectBind(username string, reject bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectBinds[username] = reject
}

func (s *poolTestServer) rejectBind(username string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejectBinds[username]
}

func (s *poolTestServer) recordBind(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindLog = append(s.bindLog, username)
}

func (s *poolTestServer) recordSearch(identity string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchLog = append(s.searchLog, identity)
	s.searchSeq++
	return s.searchSeq
}

func (s *poolTestServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.connCount++
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *poolTestServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	boundDN := ""
	for {
		packet, err := ber.ReadPacket(reader)
		if err != nil {
			return
		}
		if len(packet.Children) < 2 {
			return
		}
		msgID, ok := packet.Children[0].Value.(int64)
		if !ok {
			return
		}
		op := packet.Children[1]
		switch op.Tag {
		case ApplicationBindRequest:
			if !s.handleBind(conn, msgID, op, &boundDN) {
				return
			}
		case ApplicationSearchRequest:
			if !s.handleSearch(conn, msgID, boundDN) {
				return
			}
		case ApplicationExtendedRequest:
			if !s.handleExtended(conn, msgID, op, boundDN) {
				return
			}
		case ApplicationUnbindRequest:
			return
		}
	}
}

func (s *poolTestServer) handleBind(conn net.Conn, msgID int64, op *ber.Packet, boundDN *string) bool {
	username, _ := op.Children[1].Value.(string)
	if s.rejectBind(username) {
		return writePoolPacket(conn, poolResultPacket(msgID, ApplicationBindResponse, LDAPResultInvalidCredentials))
	}
	*boundDN = username
	s.recordBind(username)
	return writePoolPacket(conn, poolResultPacket(msgID, ApplicationBindResponse, LDAPResultSuccess))
}

func (s *poolTestServer) handleSearch(conn net.Conn, msgID int64, boundDN string) bool {
	searchID := s.recordSearch(boundDN)
	for row := 0; row < s.entries; row++ {
		if s.entryDelay > 0 {
			time.Sleep(s.entryDelay)
		}
		entry := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ApplicationSearchResultEntry, nil, "Search Result Entry")
		entry.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString,
			fmt.Sprintf("cn=search%03d-row%03d,dc=example,dc=com", searchID, row), "Object Name"))
		attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Attributes")
		attr := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Attribute")
		attr.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "cn", "Attribute Type"))
		values := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "Attribute Values")
		values.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString,
			fmt.Sprintf("search%03d-row%03d", searchID, row), "Attribute Value"))
		attr.AppendChild(values)
		attrs.AppendChild(attr)
		entry.AppendChild(attrs)
		if !writePoolPacket(conn, poolEnvelope(msgID, entry)) {
			return false
		}
	}
	return writePoolPacket(conn, poolResultPacket(msgID, ApplicationSearchResultDone, LDAPResultSuccess))
}

func (s *poolTestServer) handleExtended(conn net.Conn, msgID int64, op *ber.Packet, boundDN string) bool {
	name := op.Children[0].Data.String()
	if name != ControlTypeWhoAmI {
		return writePoolPacket(conn, poolResultPacket(msgID, ApplicationExtendedResponse, LDAPResultProtocolError))
	}
	authzID := ""
	if boundDN != "" {
		authzID = "dn:" + boundDN
	}
	resp := poolResultOp(ApplicationExtendedResponse, LDAPResultSuccess)
	resp.AppendChild(ber.NewString(ber.ClassContext, ber.TypePrimitive, 11, authzID, "responseValue"))
	return writePoolPacket(conn, poolEnvelope(msgID, resp))
}

func poolEnvelope(msgID int64, op *ber.Packet) *ber.Packet {
	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
	packet.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, msgID, "MessageID"))
	packet.AppendChild(op)
	return packet
}

func poolResultOp(appTag int8, resultCode uint64) *ber.Packet {
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ber.Tag(appTag), nil, "Result")
	op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(resultCode), "resultCode"))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "diagnosticMessage"))
	return op
}

func poolResultPacket(msgID int64, appTag int8, resultCode uint64) *ber.Packet {
	return poolEnvelope(msgID, poolResultOp(appTag, resultCode))
}

func writePoolPacket(conn net.Conn, packet *ber.Packet) bool {
	_, err := conn.Write(packet.Bytes())
	return err == nil
}

func poolTestSearchRequest() *SearchRequest {
	return NewSearchRequest(
		"dc=example,dc=com", ScopeWholeSubtree, NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"cn"}, nil)
}

// Two identities alternate borrowing from the pool. The server records the
// identity actually seen for every search; borrowers that silently rebind as
// someone else before returning the connection must be detected, and the
// next search under that key must still run as the key identity.
func TestPoolAlternatingIdentities(t *testing.T) {
	server := newPoolTestServer(t, 1, 0)
	pool := NewPool(func() (*Conn, error) { return DialURL(server.url()) }, 4)
	defer func() { _ = pool.Close() }()

	keys := []PoolKey{
		{Username: "cn=alice,dc=example,dc=com", Password: "alicepw"},
		{Username: "cn=bob,dc=example,dc=com", Password: "bobpw"},
	}

	const rounds = 4
	var expected []string
	for round := 0; round < rounds; round++ {
		for i, key := range keys {
			conn, err := pool.Get(key)
			require.NoError(t, err)

			_, err = conn.Search(poolTestSearchRequest())
			require.NoError(t, err)
			expected = append(expected, key.Username)

			// Misbehave on alternating borrows: leave the connection bound
			// as the other identity before returning it.
			if (round+i)%2 == 1 {
				other := keys[(i+1)%len(keys)]
				require.NoError(t, conn.Bind(other.Username, other.Password))
			}
			require.NoError(t, pool.Put(key, conn))
		}
	}

	require.Equal(t, expected, server.searchIdentities(),
		"every search must run as the identity of the key it was borrowed under")

	// Both identities share one connection each; the pool re-binds instead of
	// dialing fresh connections.
	require.Equal(t, 2, server.connections())
	require.Greater(t, server.bindCount(), 2,
		"returned connections with changed bind state must be re-bound")
}

// A borrower starts an asynchronous search and returns the connection before
// the stream finishes. The next borrower of the same connection must not
// receive any of the previous borrower's responses.
func TestPoolAsyncSearchEarlyReturn(t *testing.T) {
	server := newPoolTestServer(t, 50, 2*time.Millisecond)
	pool := NewPool(func() (*Conn, error) { return DialURL(server.url()) }, 2)
	defer func() { _ = pool.Close() }()

	key := PoolKey{Username: "cn=async,dc=example,dc=com", Password: "pw"}

	conn, err := pool.Get(key)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	resp := conn.SearchAsync(ctx, poolTestSearchRequest(), 8)

	// Consume the beginning of the stream, then return the connection while
	// the search is still streaming.
	require.True(t, resp.Next())
	require.NotNil(t, resp.Entry())
	cancel()
	require.NoError(t, pool.Put(key, conn))

	// The abandoned stream must terminate rather than block forever.
	for resp.Next() {
	}

	// The next borrower reuses the same connection but must only see its own
	// search results.
	conn2, err := pool.Get(key)
	require.NoError(t, err)
	require.Same(t, conn, conn2)

	result, err := conn2.Search(poolTestSearchRequest())
	require.NoError(t, err)
	require.Len(t, result.Entries, 50)
	secondSearchPrefix := "cn=search002-"
	for _, entry := range result.Entries {
		require.Contains(t, entry.DN, secondSearchPrefix,
			"subsequent borrower received a response from the previous borrower's search")
	}
	require.NoError(t, pool.Put(key, conn2))
}

// Returning a connection concurrently with Close must not hand stale
// connections or responses to later users: the returned connection is closed
// and subsequent Get calls fail.
func TestPoolCloseConcurrentWithPut(t *testing.T) {
	server := newPoolTestServer(t, 20, time.Millisecond)
	pool := NewPool(func() (*Conn, error) { return DialURL(server.url()) }, 2)

	key := PoolKey{Username: "cn=racy,dc=example,dc=com", Password: "pw"}

	conn, err := pool.Get(key)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := conn.SearchAsync(ctx, poolTestSearchRequest(), 4)

	putDone := make(chan error, 1)
	go func() { putDone <- pool.Put(key, conn) }()
	require.NoError(t, pool.Close())
	putErr := <-putDone
	require.True(t, putErr == nil || errors.Is(putErr, ErrPoolClosed))

	_, err = pool.Get(key)
	require.ErrorIs(t, err, ErrPoolClosed)

	// Whichever ordering won, the returned connection was closed, so its
	// in-flight responses cannot be delivered to any later user.
	require.True(t, conn.IsClosing())
	for resp.Next() {
	}
}

// When the pool cannot re-establish the key identity on a returned
// connection, the connection is closed and discarded - a still-open TCP
// connection is never treated as clean.
func TestPoolRebindFailureDiscardsConnection(t *testing.T) {
	server := newPoolTestServer(t, 1, 0)
	pool := NewPool(func() (*Conn, error) { return DialURL(server.url()) }, 2)
	defer func() { _ = pool.Close() }()

	key := PoolKey{Username: "cn=carol,dc=example,dc=com", Password: "carolpw"}

	conn, err := pool.Get(key)
	require.NoError(t, err)
	require.Equal(t, 1, server.connections())

	// The borrower leaves the connection bound as someone else, and the
	// server now refuses to re-establish the pool identity.
	require.NoError(t, conn.Bind("cn=mallory,dc=example,dc=com", "mallorypw"))
	server.setRejectBind(key.Username, true)

	err = pool.Put(key, conn)
	require.Error(t, err)
	require.True(t, conn.IsClosing(), "connection with unrestorable identity must be closed")
	require.Equal(t, 0, pool.IdleCount(key), "connection with unrestorable identity must not be pooled")

	// The next borrower gets a freshly dialed connection, not the discarded
	// (still TCP-alive at the time) one.
	server.setRejectBind(key.Username, false)
	conn2, err := pool.Get(key)
	require.NoError(t, err)
	require.NotSame(t, conn, conn2)
	require.Equal(t, 2, server.connections())
	require.NoError(t, pool.Put(key, conn2))
}

// The pool key carries the TLS requirement: a plaintext connection must not
// satisfy a key that requires TLS, and a connection returned under a key
// with a different TLS requirement is discarded.
func TestPoolTLSRequirement(t *testing.T) {
	server := newPoolTestServer(t, 1, 0)
	pool := NewPool(func() (*Conn, error) { return DialURL(server.url()) }, 2)
	defer func() { _ = pool.Close() }()

	// A plaintext dial cannot satisfy a TLS-requiring key.
	_, err := pool.Get(PoolKey{TLS: true})
	require.Error(t, err)

	// Returning a plaintext connection under a TLS key discards it.
	plainKey := PoolKey{Username: "cn=dave,dc=example,dc=com", Password: "pw"}
	conn, err := pool.Get(plainKey)
	require.NoError(t, err)
	err = pool.Put(PoolKey{Username: plainKey.Username, Password: plainKey.Password, TLS: true}, conn)
	require.Error(t, err)
	require.True(t, conn.IsClosing())
	require.Equal(t, 0, pool.IdleCount(plainKey))
}
