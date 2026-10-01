package imapmemserver

import (
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
)

// TestThreadCommand exercises the THREAD and UID THREAD commands end-to-end:
// it serves an imapmemserver backend over a real connection and checks the
// trees parsed by imapclient.
func TestThreadCommand(t *testing.T) {
	memServer := New()
	user := NewUser("user", "pass")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	memServer.AddUser(user)

	mbox, err := user.mailbox("INBOX")
	if err != nil {
		t.Fatalf("mailbox() = %v", err)
	}
	seqToID := make(map[uint32]string)
	uidToID := make(map[uint32]string)
	for i := range threadTestMessages {
		m := &threadTestMessages[i]
		date := time.Date(2026, 1, m.day, 12, 0, 0, 0, time.UTC)
		appendData := mbox.appendBytes(m.raw(), &imap.AppendOptions{Time: date})
		seqToID[uint32(i)+1] = m.id
		uidToID[uint32(appendData.UID)] = m.id
	}

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}
	go server.Serve(ln)
	defer server.Close()

	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("DialInsecure() = %v", err)
	}
	defer c.Close()

	if err := c.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}

	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatalf("Capability() = %v", err)
	}
	if !caps.Has(imap.CapThreadReferences) {
		t.Errorf("THREAD=REFERENCES not advertised in capabilities: %v", caps)
	}

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}

	options := &imapclient.ThreadOptions{
		Algorithm:      imap.ThreadReferences,
		SearchCriteria: &imap.SearchCriteria{},
	}

	threads, err := c.Thread(options).Wait()
	if err != nil {
		t.Fatalf("Thread() = %v", err)
	}
	if got := convertThreads(threads, seqToID); !reflect.DeepEqual(got, threadTestExpected) {
		t.Errorf("THREAD = %+v, want %+v", got, threadTestExpected)
	}

	uidThreads, err := c.UIDThread(options).Wait()
	if err != nil {
		t.Fatalf("UIDThread() = %v", err)
	}
	if got := convertThreads(uidThreads, uidToID); !reflect.DeepEqual(got, threadTestExpected) {
		t.Errorf("UID THREAD = %+v, want %+v", got, threadTestExpected)
	}
}

func convertThreads(threads []imapclient.ThreadData, idOf map[uint32]string) []*idNode {
	var out []*idNode
	for i := range threads {
		out = append(out, convertThreadData(&threads[i], idOf))
	}
	return out
}
