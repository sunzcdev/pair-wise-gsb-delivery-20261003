package ldap

import (
	"net"
	"sync"
	"testing"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/stretchr/testify/require"
)

// dirSyncTestEntry is an entry served by dirSyncTestServer.
type dirSyncTestEntry struct {
	dn         string
	attributes map[string][]string
}

// dirSyncTestPage is one page of a dirSyncTestServer: the entries returned
// for a given request cookie and the cookie sent back with the page. An
// empty nextCookie marks the final page.
type dirSyncTestPage struct {
	entries    []dirSyncTestEntry
	nextCookie []byte
}

// dirSyncTestServer is a minimal in-process LDAP server that only answers
// search requests carrying a DirSync control. Requests with an unknown
// cookie are rejected with unwillingToPerform.
type dirSyncTestServer struct {
	listener net.Listener
	pages    map[string]*dirSyncTestPage

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newDirSyncTestServer(t *testing.T, pages map[string]*dirSyncTestPage) *dirSyncTestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &dirSyncTestServer{listener: listener, pages: pages}
	server.wg.Add(1)
	go server.accept()
	t.Cleanup(server.close)
	return server
}

func (s *dirSyncTestServer) addr() string {
	return s.listener.Addr().String()
}

func (s *dirSyncTestServer) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.listener.Close()
	s.wg.Wait()
}

func (s *dirSyncTestServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serve(conn)
	}
}

func (s *dirSyncTestServer) serve(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	for {
		packet, err := ber.ReadPacket(conn)
		if err != nil {
			return
		}
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		if len(packet.Children) < 2 || packet.Children[1].Tag != ApplicationSearchRequest {
			continue
		}
		messageID, ok := packet.Children[0].Value.(int64)
		if !ok {
			return
		}
		s.handleSearch(conn, messageID, packet)
	}
}

func (s *dirSyncTestServer) handleSearch(conn net.Conn, messageID int64, packet *ber.Packet) {
	cookie := ""
	if len(packet.Children) > 2 {
		for _, controlPacket := range packet.Children[2].Children {
			control, err := DecodeControl(controlPacket)
			if err != nil {
				continue
			}
			if dirSync, ok := control.(*ControlDirSync); ok {
				cookie = string(dirSync.Cookie)
			}
		}
	}

	page, ok := s.pages[cookie]
	if !ok {
		writePacket(conn, dirSyncDonePacket(messageID, LDAPResultUnwillingToPerform, nil))
		return
	}
	for _, entry := range page.entries {
		writePacket(conn, dirSyncEntryPacket(messageID, entry))
	}
	writePacket(conn, dirSyncDonePacket(messageID, LDAPResultSuccess, page.nextCookie))
}

func writePacket(conn net.Conn, packet *ber.Packet) {
	if _, err := conn.Write(packet.Bytes()); err != nil {
		return
	}
}

func dirSyncEntryPacket(messageID int64, entry dirSyncTestEntry) *ber.Packet {
	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
	packet.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, messageID, "Message ID"))

	resultEntry := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ApplicationSearchResultEntry, nil, "Search Result Entry")
	resultEntry.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, entry.dn, "Object Name"))
	attributes := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Attributes")
	for name, values := range entry.attributes {
		attribute := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Attribute")
		attribute.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, name, "Attribute Name"))
		attributeValues := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "Attribute Values")
		for _, value := range values {
			attributeValues.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, value, "Attribute Value"))
		}
		attribute.AppendChild(attributeValues)
		attributes.AppendChild(attribute)
	}
	resultEntry.AppendChild(attributes)
	packet.AppendChild(resultEntry)
	return packet
}

func dirSyncDonePacket(messageID int64, resultCode int64, cookie []byte) *ber.Packet {
	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
	packet.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, messageID, "Message ID"))

	done := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ApplicationSearchResultDone, nil, "Search Result Done")
	done.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, resultCode, "resultCode"))
	done.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	diagnostic := ""
	if resultCode != LDAPResultSuccess {
		diagnostic = "dirsync cookie rejected"
	}
	done.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, diagnostic, "diagnosticMessage"))
	packet.AppendChild(done)

	if resultCode == LDAPResultSuccess {
		controls := ber.Encode(ber.ClassContext, ber.TypeConstructed, 0, nil, "Controls")
		control := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Control")
		control.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, ControlTypeDirSync, "Control Type (DirSync)"))

		value := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "DirSync Control Value")
		value.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, int64(0), "Flags"))
		value.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, int64(0), "MaxAttrCount"))
		value.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, string(cookie), "Cookie"))
		control.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, string(value.Bytes()), "Control Value (DirSync)"))

		controls.AppendChild(control)
		packet.AppendChild(controls)
	}
	return packet
}

func testDirSyncPages() map[string]*dirSyncTestPage {
	entry := func(cn string) dirSyncTestEntry {
		return dirSyncTestEntry{
			dn:         "cn=" + cn + ",dc=example,dc=com",
			attributes: map[string][]string{"cn": {cn}},
		}
	}
	return map[string]*dirSyncTestPage{
		"": {
			entries:    []dirSyncTestEntry{entry("alice")},
			nextCookie: []byte("cookie-1"),
		},
		"cookie-1": {
			entries:    []dirSyncTestEntry{entry("bob")},
			nextCookie: []byte("cookie-2"),
		},
		"cookie-2": {
			entries:    []dirSyncTestEntry{entry("carol")},
			nextCookie: nil,
		},
	}
}

func newDirSyncTestSession(t *testing.T, server *dirSyncTestServer, state *DirSyncState) (*Conn, *DirSyncSession) {
	t.Helper()
	conn, err := DialURL("ldap://" + server.addr())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	searchRequest := NewSearchRequest(
		"dc=example,dc=com",
		ScopeWholeSubtree, NeverDerefAliases, 0, 0, false,
		"(objectClass=*)",
		[]string{"cn"},
		nil,
	)
	session, err := NewDirSyncSession(conn, server.addr(), searchRequest, DirSyncObjectSecurity, 1000, state)
	require.NoError(t, err)
	return conn, session
}

func requireBatchCNs(t *testing.T, batch *DirSyncBatch, cns ...string) {
	t.Helper()
	require.NotNil(t, batch)
	require.Len(t, batch.Entries, len(cns))
	for i, cn := range cns {
		require.Equal(t, cn, batch.Entries[i].GetAttributeValue("cn"))
	}
}

func TestDirSyncSessionDeliversConfirmedBatches(t *testing.T) {
	server := newDirSyncTestServer(t, testDirSyncPages())
	_, session := newDirSyncTestSession(t, server, nil)

	batch, err := session.Next()
	require.NoError(t, err)
	requireBatchCNs(t, batch, "alice")

	// The next batch must not be delivered before the current one is confirmed.
	_, err = session.Next()
	require.ErrorIs(t, err, ErrDirSyncUnconfirmedBatch)

	// The persistable state must not expose the unconfirmed cookie.
	require.Empty(t, session.State().Cookie)

	batch.Confirm()
	require.Equal(t, "cookie-1", string(session.State().Cookie))

	batch, err = session.Next()
	require.NoError(t, err)
	requireBatchCNs(t, batch, "bob")
	batch.Confirm()
	require.Equal(t, "cookie-2", string(session.State().Cookie))

	batch, err = session.Next()
	require.NoError(t, err)
	requireBatchCNs(t, batch, "carol")
	require.False(t, session.Done())
	batch.Confirm()
	require.True(t, session.Done())

	batch, err = session.Next()
	require.NoError(t, err)
	require.Nil(t, batch)
}

func TestDirSyncSessionRestartRepeatsUnconfirmedBatch(t *testing.T) {
	server := newDirSyncTestServer(t, testDirSyncPages())
	conn, session := newDirSyncTestSession(t, server, nil)

	batch, err := session.Next()
	require.NoError(t, err)
	requireBatchCNs(t, batch, "alice")
	batch.Confirm()

	// Receive the second batch but do not confirm it, then "restart".
	batch, err = session.Next()
	require.NoError(t, err)
	requireBatchCNs(t, batch, "bob")
	savedState := session.State()
	conn.Close()

	// A new session resumed from the persisted state must repeat the
	// unconfirmed batch instead of skipping to the next page.
	_, resumed := newDirSyncTestSession(t, server, &savedState)

	batch, err = resumed.Next()
	require.NoError(t, err)
	requireBatchCNs(t, batch, "bob")
	batch.Confirm()
	require.Equal(t, "cookie-2", string(resumed.State().Cookie))

	batch, err = resumed.Next()
	require.NoError(t, err)
	requireBatchCNs(t, batch, "carol")
	batch.Confirm()
	require.True(t, resumed.Done())
}

func TestDirSyncSessionStateBinding(t *testing.T) {
	server := newDirSyncTestServer(t, testDirSyncPages())
	conn, err := DialURL("ldap://" + server.addr())
	require.NoError(t, err)
	defer conn.Close()

	newRequest := func() *SearchRequest {
		return NewSearchRequest(
			"dc=example,dc=com",
			ScopeWholeSubtree, NeverDerefAliases, 0, 0, false,
			"(objectClass=*)",
			[]string{"cn"},
			nil,
		)
	}

	validState := &DirSyncState{
		Server: server.addr(),
		BaseDN: "dc=example,dc=com",
		Filter: "(objectClass=*)",
		Cookie: []byte("cookie-1"),
	}

	_, err = NewDirSyncSession(conn, server.addr(), newRequest(), DirSyncObjectSecurity, 1000, validState)
	require.NoError(t, err)

	otherServer := *validState
	otherServer.Server = "other.example.com:389"
	_, err = NewDirSyncSession(conn, server.addr(), newRequest(), DirSyncObjectSecurity, 1000, &otherServer)
	require.ErrorContains(t, err, "bound to server")

	otherBase := *validState
	otherBase.BaseDN = "ou=other,dc=example,dc=com"
	_, err = NewDirSyncSession(conn, server.addr(), newRequest(), DirSyncObjectSecurity, 1000, &otherBase)
	require.ErrorContains(t, err, "bound to base DN")

	otherFilter := *validState
	otherFilter.Filter = "(objectClass=person)"
	_, err = NewDirSyncSession(conn, server.addr(), newRequest(), DirSyncObjectSecurity, 1000, &otherFilter)
	require.ErrorContains(t, err, "bound to filter")
}

func TestDirSyncSessionRejectedCookieRequiresBaseline(t *testing.T) {
	server := newDirSyncTestServer(t, testDirSyncPages())

	staleState := &DirSyncState{
		Server: server.addr(),
		BaseDN: "dc=example,dc=com",
		Filter: "(objectClass=*)",
		Cookie: []byte("cookie-the-server-forgot"),
	}
	_, session := newDirSyncTestSession(t, server, staleState)

	// The rejected cookie must surface as "baseline required", not as an
	// empty "no changes" result.
	batch, err := session.Next()
	require.ErrorIs(t, err, ErrDirSyncBaselineRequired)
	require.Nil(t, batch)
	require.False(t, session.Done())

	// After a reset the session establishes a new baseline from scratch.
	require.NoError(t, session.Reset())
	batch, err = session.Next()
	require.NoError(t, err)
	requireBatchCNs(t, batch, "alice")
	batch.Confirm()
	require.Equal(t, "cookie-1", string(session.State().Cookie))
}
