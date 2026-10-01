package imapclient_test

import (
	"crypto/tls"
	"net"
	"reflect"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// gatedSortSession wraps an IMAP session and blocks SORT until a gate is
// opened, letting the test trigger an EXPUNGE on another connection while a
// SORT is in flight.
type gatedSortSession struct {
	imapserver.Session
	gate <-chan struct{}
}

func (s *gatedSortSession) Sort(kind imapserver.NumKind, sortCriteria []imap.SortCriterion, criteria *imap.SearchCriteria) (*imap.SortData, error) {
	<-s.gate
	return s.Session.(imapserver.SessionSort).Sort(kind, sortCriteria, criteria)
}

func (s *gatedSortSession) Namespace() (*imap.NamespaceData, error) {
	return s.Session.(imapserver.SessionNamespace).Namespace()
}

func (s *gatedSortSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	return s.Session.(imapserver.SessionMove).Move(w, numSet, dest)
}

func newSortExpungePair(t *testing.T, gate <-chan struct{}) (a, b *imapclient.Client, cleanup func()) {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(testUsername, testPassword)
	user.Create("INBOX", nil)
	memServer.AddUser(user)

	cert, err := tls.X509KeyPair([]byte(rsaCertPEM), []byte(rsaKeyPEM))
	if err != nil {
		t.Fatalf("tls.X509KeyPair() = %v", err)
	}

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &gatedSortSession{
				Session: memServer.NewSession(),
				gate:    gate,
			}, nil, nil
		},
		TLSConfig:    &tls.Config{Certificates: []tls.Certificate{cert}},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
			imap.CapSort:      {},
		},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil {
			t.Errorf("Serve() = %v", err)
		}
	}()

	connect := func() *imapclient.Client {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("net.Dial() = %v", err)
		}
		c := imapclient.New(conn, nil)
		if err := c.Login(testUsername, testPassword).Wait(); err != nil {
			t.Fatalf("Login().Wait() = %v", err)
		}
		return c
	}

	a = connect()
	b = connect()

	cleanup = func() {
		a.Close()
		b.Close()
	}
	return a, b, cleanup
}

// TestSortConcurrentExpunge issues a SORT and a UID SORT on one connection
// while another connection expunges a message concurrently, then verifies
// that the sequence numbers and UIDs stay consistent: the expunged message is
// omitted and the two result lists describe the same messages.
func TestSortConcurrentExpunge(t *testing.T) {
	gate := make(chan struct{})
	var openGate sync.Once

	clientA, clientB, cleanup := newSortExpungePair(t, gate)
	defer cleanup()

	// Append 5 messages with distinct subjects.
	raws := []string{
		sortConcurrentRaw("one"),
		sortConcurrentRaw("two"),
		sortConcurrentRaw("three"),
		sortConcurrentRaw("four"),
		sortConcurrentRaw("five"),
	}
	for _, raw := range raws {
		cmd := clientA.Append("INBOX", int64(len(raw)), nil)
		if _, err := cmd.Write([]byte(raw)); err != nil {
			t.Fatalf("Write() = %v", err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("Append().Wait() = %v", err)
		}
	}

	if _, err := clientA.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("A Select() = %v", err)
	}
	if _, err := clientB.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("B Select() = %v", err)
	}

	// Mark the second message (seq 2, UID 2) as deleted on connection B.
	storeCmd := clientB.Store(imap.SeqSetNum(2), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagDeleted},
	}, nil)
	if err := storeCmd.Close(); err != nil {
		t.Fatalf("Store() = %v", err)
	}

	// Start SORT on A; it blocks inside the backend until the gate opens.
	sortDone := make(chan struct{})
	var seqNums []uint32
	go func() {
		defer close(sortDone)
		var err error
		seqNums, err = clientA.Sort(&imapclient.SortOptions{
			SortCriteria: []imap.SortCriterion{{Key: imap.SortKeySubject}},
		}).Wait()
		if err != nil {
			t.Errorf("A Sort().Wait() = %v", err)
		}
	}()

	// B expunges UID 2 while A's SORT is blocked. Opening the gate lets the
	// SORT proceed afterwards, but the mailbox is already mutated.
	if _, err := clientB.Expunge().Collect(); err != nil {
		t.Fatalf("B Expunge() = %v", err)
	}
	openGate.Do(func() { close(gate) })

	<-sortDone

	// SORT reflects A's mailbox view before A processes the EXPUNGE: the
	// expunged message is skipped, and remaining sequence numbers are not
	// shifted. Subject order: five(5), four(4), one(1), three(3).
	wantSeqNums := []uint32{5, 4, 1, 3}
	if !reflect.DeepEqual(seqNums, wantSeqNums) {
		t.Fatalf("SORT seqNums = %v, want %v", seqNums, wantSeqNums)
	}
	t.Logf("SORT seqNums = %v", seqNums)

	// UID SORT uses stable UIDs: UID 2 is simply absent.
	uids, err := clientA.UIDSort(&imapclient.SortOptions{
		SortCriteria: []imap.SortCriterion{{Key: imap.SortKeySubject}},
	}).Wait()
	if err != nil {
		t.Fatalf("A UIDSort().Wait() = %v", err)
	}
	wantUIDs := []uint32{5, 4, 1, 3}
	if !reflect.DeepEqual(uids, wantUIDs) {
		t.Fatalf("UID SORT uids = %v, want %v", uids, wantUIDs)
	}

	// After A observes the EXPUNGE (via NOOP), its view has 4 messages. The
	// surviving messages are now at sequence numbers 1..3 and 4; verify a
	// follow-up SORT and UID SORT still describe the same mailbox view and
	// agree, position by position.
	if err := clientA.Noop().Wait(); err != nil {
		t.Fatalf("Noop().Wait() = %v", err)
	}

	afterSeq, err := clientA.Sort(&imapclient.SortOptions{
		SortCriteria: []imap.SortCriterion{{Key: imap.SortKeySubject}},
	}).Wait()
	if err != nil {
		t.Fatalf("post-expunge Sort() = %v", err)
	}
	if want := []uint32{4, 3, 1, 2}; !reflect.DeepEqual(afterSeq, want) {
		t.Fatalf("post-expunge SORT seqNums = %v, want %v", afterSeq, want)
	}

	afterUID, err := clientA.UIDSort(&imapclient.SortOptions{
		SortCriteria: []imap.SortCriterion{{Key: imap.SortKeySubject}},
	}).Wait()
	if err != nil {
		t.Fatalf("post-expunge UIDSort() = %v", err)
	}
	if want := []uint32{5, 4, 1, 3}; !reflect.DeepEqual(afterUID, want) {
		t.Fatalf("post-expunge UID SORT uids = %v, want %v", afterUID, want)
	}

	// The two lists name the same messages in the same order. Fetch each
	// sequence number individually (a multi-message FETCH returns messages
	// in mailbox order, not in set order) and map it to its UID.
	var mappedUIDs []uint32
	for _, seqNum := range afterSeq {
		msgs, err := clientA.Fetch(imap.SeqSetNum(seqNum), &imap.FetchOptions{UID: true}).Collect()
		if err != nil {
			t.Fatalf("Fetch(%v).Collect() = %v", seqNum, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("Fetch(%v) returned %v messages, want 1", seqNum, len(msgs))
		}
		mappedUIDs = append(mappedUIDs, uint32(msgs[0].UID))
	}
	if !reflect.DeepEqual(mappedUIDs, afterUID) {
		t.Fatalf("post-expunge seqnum->UID = %v, want %v", mappedUIDs, afterUID)
	}
}

func sortConcurrentRaw(subject string) string {
	return "Date: Wed, 02 Feb 2022 10:00:00 +0000\r\n" +
		"From: sender@example.org\r\n" +
		"Subject: " + subject + "\r\n" +
		"\r\nbody\r\n"
}
