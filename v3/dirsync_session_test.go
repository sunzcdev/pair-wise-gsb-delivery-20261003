package ldap

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// dirSyncServer is a minimal in-process LDAP server that serves DirSync
// pages. Cookies are the index of the next page to serve; the server only
// accepts cookies it has issued itself, so expiring them simulates a server
// that lost its replication state.
type dirSyncServer struct {
	listener net.Listener
	pages    [][]*Entry

	mu           sync.Mutex
	validCookies map[string]bool
}

func newDirSyncServer(t *testing.T, pages [][]*Entry) *dirSyncServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start dirsync test server: %v", err)
	}
	s := &dirSyncServer{
		listener:     listener,
		pages:        pages,
		validCookies: map[string]bool{"": true},
	}
	go s.serve()
	t.Cleanup(func() { _ = s.listener.Close() })
	return s
}

func (s *dirSyncServer) addr() string { return s.listener.Addr().String() }

// expireCookies invalidates all issued cookies, simulating a server restart
// that lost its DirSync state.
func (s *dirSyncServer) expireCookies() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validCookies = map[string]bool{"": true}
}

func (s *dirSyncServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *dirSyncServer) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		packet, err := ber.ReadPacket(conn)
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
		switch packet.Children[1].Tag {
		case ApplicationBindRequest:
			s.writeDone(conn, msgID, ApplicationBindResponse, LDAPResultSuccess, "", nil)
		case ApplicationSearchRequest:
			s.handleSearch(conn, msgID, packet)
		case ApplicationUnbindRequest:
			return
		}
	}
}

func (s *dirSyncServer) handleSearch(conn net.Conn, msgID int64, packet *ber.Packet) {
	var cookie []byte
	if len(packet.Children) > 2 {
		for _, controlPacket := range packet.Children[2].Children {
			control, err := DecodeControl(controlPacket)
			if err != nil {
				continue
			}
			if dirSync, ok := control.(*ControlDirSync); ok {
				cookie = dirSync.Cookie
			}
		}
	}

	key := string(cookie)
	s.mu.Lock()
	_, valid := s.validCookies[key]
	s.mu.Unlock()
	if !valid {
		s.writeDone(conn, msgID, ApplicationSearchResultDone, LDAPResultUnwillingToPerform, "dirsync cookie rejected", nil)
		return
	}

	page := 0
	if key != "" {
		n, err := strconv.Atoi(key)
		if err != nil {
			s.writeDone(conn, msgID, ApplicationSearchResultDone, LDAPResultUnwillingToPerform, "dirsync cookie rejected", nil)
			return
		}
		page = n
	}

	var entries []*Entry
	if page < len(s.pages) {
		entries = s.pages[page]
	}
	nextCookie := strconv.Itoa(page + 1)
	s.mu.Lock()
	s.validCookies[nextCookie] = true
	s.mu.Unlock()

	for _, entry := range entries {
		writeSearchEntry(conn, msgID, entry)
	}
	s.writeDone(conn, msgID, ApplicationSearchResultDone, LDAPResultSuccess, "", []byte(nextCookie))
}

func writeSearchEntry(conn net.Conn, msgID int64, entry *Entry) {
	response := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Message")
	response.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, msgID, "Message ID"))
	entryPacket := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ApplicationSearchResultEntry, nil, "Search Result Entry")
	entryPacket.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, entry.DN, "Object Name"))
	attributes := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Attributes")
	for _, attr := range entry.Attributes {
		attribute := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Attribute")
		attribute.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, attr.Name, "Attribute Type"))
		values := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "Attribute Values")
		for _, value := range attr.Values {
			values.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, value, "Attribute Value"))
		}
		attribute.AppendChild(values)
		attributes.AppendChild(attribute)
	}
	entryPacket.AppendChild(attributes)
	response.AppendChild(entryPacket)
	_, _ = conn.Write(response.Bytes())
}

func (s *dirSyncServer) writeDone(conn net.Conn, msgID int64, appTag ber.Tag, resultCode uint64, diagnostic string, cookie []byte) {
	response := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Message")
	response.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, msgID, "Message ID"))
	done := ber.Encode(ber.ClassApplication, ber.TypeConstructed, appTag, nil, "Result")
	done.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, resultCode, "Result Code"))
	done.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "Matched DN"))
	done.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, diagnostic, "Diagnostic Message"))
	response.AppendChild(done)
	if cookie != nil {
		controls := ber.Encode(ber.ClassContext, ber.TypeConstructed, 0, nil, "Controls")
		controls.AppendChild((&ControlDirSync{
			Criticality:  true,
			Flags:        DirSyncObjectSecurity,
			MaxAttrCount: 1000,
			Cookie:       cookie,
		}).Encode())
		response.AppendChild(controls)
	}
	_, _ = conn.Write(response.Bytes())
}

func dirSyncTestRequest() *SearchRequest {
	return NewSearchRequest(
		"dc=example,dc=com",
		ScopeWholeSubtree, NeverDerefAliases, 0, 0, false,
		"(objectClass=*)",
		[]string{"cn"},
		nil,
	)
}

func dirSyncTestConn(t *testing.T, server *dirSyncServer) *Conn {
	t.Helper()
	conn, err := DialURL("ldap://" + server.addr())
	if err != nil {
		t.Fatalf("failed to connect to dirsync test server: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func dirSyncTestPages() [][]*Entry {
	return [][]*Entry{
		{NewEntry("cn=a,dc=example,dc=com", map[string][]string{"cn": {"a"}})},
		{NewEntry("cn=b,dc=example,dc=com", map[string][]string{"cn": {"b"}})},
		{NewEntry("cn=c,dc=example,dc=com", map[string][]string{"cn": {"c"}})},
	}
}

func batchDNs(batch *DirSyncBatch) []string {
	dns := make([]string, 0, len(batch.Entries))
	for _, entry := range batch.Entries {
		dns = append(dns, entry.DN)
	}
	return dns
}

func TestDirSyncSessionRestartBeforeCommit(t *testing.T) {
	server := newDirSyncServer(t, dirSyncTestPages())

	conn := dirSyncTestConn(t, server)
	session, err := NewDirSyncSession(conn, server.addr(), dirSyncTestRequest(), DirSyncObjectSecurity, 1000, nil)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Page 0: delivered and confirmed.
	batch, err := session.Next()
	if err != nil {
		t.Fatalf("next page 0: %v", err)
	}
	if got := batchDNs(batch); len(got) != 1 || got[0] != "cn=a,dc=example,dc=com" {
		t.Fatalf("unexpected page 0 entries: %v", got)
	}
	if cookie := session.State().Cookie; len(cookie) != 0 {
		t.Fatalf("state must not contain the new cookie before commit, got %q", cookie)
	}
	session.Commit()
	if cookie := string(session.State().Cookie); cookie != "1" {
		t.Fatalf("expected cookie %q after commit, got %q", "1", cookie)
	}

	// Page 1: delivered and confirmed, state persisted.
	batch, err = session.Next()
	if err != nil {
		t.Fatalf("next page 1: %v", err)
	}
	if got := batchDNs(batch); len(got) != 1 || got[0] != "cn=b,dc=example,dc=com" {
		t.Fatalf("unexpected page 1 entries: %v", got)
	}
	session.Commit()
	persisted, err := MarshalDirSyncState(session.State())
	if err != nil {
		t.Fatalf("failed to marshal state: %v", err)
	}

	// Page 2: delivered but NOT confirmed. A follow-up Next in the same
	// session is rejected, then the process "restarts": only the persisted
	// state survives.
	batch, err = session.Next()
	if err != nil {
		t.Fatalf("next page 2: %v", err)
	}
	if got := batchDNs(batch); len(got) != 1 || got[0] != "cn=c,dc=example,dc=com" {
		t.Fatalf("unexpected page 2 entries: %v", got)
	}
	if _, err := session.Next(); !errors.Is(err, ErrDirSyncUncommittedBatch) {
		t.Fatalf("expected ErrDirSyncUncommittedBatch, got %v", err)
	}
	conn.Close()

	restored, err := UnmarshalDirSyncState(persisted)
	if err != nil {
		t.Fatalf("failed to unmarshal state: %v", err)
	}
	conn2 := dirSyncTestConn(t, server)
	session2, err := NewDirSyncSession(conn2, server.addr(), dirSyncTestRequest(), DirSyncObjectSecurity, 1000, restored)
	if err != nil {
		t.Fatalf("failed to resume session: %v", err)
	}

	// The unconfirmed page must be delivered again, not skipped.
	batch, err = session2.Next()
	if err != nil {
		t.Fatalf("next after restart: %v", err)
	}
	if got := batchDNs(batch); len(got) != 1 || got[0] != "cn=c,dc=example,dc=com" {
		t.Fatalf("expected page 2 to be repeated after restart, got %v", got)
	}
	session2.Commit()

	// All pages consumed: server reports no further entries.
	batch, err = session2.Next()
	if err != nil {
		t.Fatalf("next final: %v", err)
	}
	if !batch.Done || len(batch.Entries) != 0 {
		t.Fatalf("expected empty final batch, got %v", batchDNs(batch))
	}
	session2.Commit()
}

func TestDirSyncSessionStateMismatch(t *testing.T) {
	server := newDirSyncServer(t, dirSyncTestPages())
	conn := dirSyncTestConn(t, server)

	session, err := NewDirSyncSession(conn, server.addr(), dirSyncTestRequest(), DirSyncObjectSecurity, 1000, nil)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	if _, err := session.Next(); err != nil {
		t.Fatalf("next: %v", err)
	}
	session.Commit()
	base := session.State()

	cases := map[string]func(*DirSyncState){
		"server":     func(s *DirSyncState) { s.Server = "other.example.com:389" },
		"base DN":    func(s *DirSyncState) { s.BaseDN = "dc=other,dc=com" },
		"filter":     func(s *DirSyncState) { s.Filter = "(objectClass=person)" },
		"scope":      func(s *DirSyncState) { s.Scope = ScopeBaseObject },
		"attributes": func(s *DirSyncState) { s.Attributes = []string{"sn"} },
		"flags":      func(s *DirSyncState) { s.Flags = DirSyncIncrementalValues },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			state := *base
			state.Attributes = append([]string(nil), base.Attributes...)
			state.Cookie = append([]byte(nil), base.Cookie...)
			mutate(&state)
			if _, err := NewDirSyncSession(conn, server.addr(), dirSyncTestRequest(), DirSyncObjectSecurity, 1000, &state); !errors.Is(err, ErrDirSyncStateMismatch) {
				t.Fatalf("expected ErrDirSyncStateMismatch, got %v", err)
			}
		})
	}

	// A matching state is accepted.
	if _, err := NewDirSyncSession(conn, server.addr(), dirSyncTestRequest(), DirSyncObjectSecurity, 1000, base); err != nil {
		t.Fatalf("expected matching state to be accepted, got %v", err)
	}
}

func TestDirSyncSessionCookieRejected(t *testing.T) {
	server := newDirSyncServer(t, dirSyncTestPages())
	conn := dirSyncTestConn(t, server)

	session, err := NewDirSyncSession(conn, server.addr(), dirSyncTestRequest(), DirSyncObjectSecurity, 1000, nil)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	if _, err := session.Next(); err != nil {
		t.Fatalf("next: %v", err)
	}
	session.Commit()
	state := session.State()

	// Server loses its replication state and rejects the persisted cookie.
	server.expireCookies()

	conn2 := dirSyncTestConn(t, server)
	resumed, err := NewDirSyncSession(conn2, server.addr(), dirSyncTestRequest(), DirSyncObjectSecurity, 1000, state)
	if err != nil {
		t.Fatalf("failed to resume session: %v", err)
	}
	if _, err := resumed.Next(); !errors.Is(err, ErrDirSyncCookieRejected) {
		t.Fatalf("expected ErrDirSyncCookieRejected, got %v", err)
	}

	// Re-establishing a baseline (session without state) works again.
	fresh, err := NewDirSyncSession(conn2, server.addr(), dirSyncTestRequest(), DirSyncObjectSecurity, 1000, nil)
	if err != nil {
		t.Fatalf("failed to create baseline session: %v", err)
	}
	batch, err := fresh.Next()
	if err != nil {
		t.Fatalf("baseline next: %v", err)
	}
	if got := batchDNs(batch); len(got) != 1 || got[0] != "cn=a,dc=example,dc=com" {
		t.Fatalf("expected baseline to restart from page 0, got %v", got)
	}
}
