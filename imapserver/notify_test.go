package imapserver_test

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

type notifyEventRecord struct {
	kind        string
	mailbox     string
	uid         imap.UID
	flags       []imap.Flag
	numMessages uint32
	overflow    bool
}

// notifyTestEnv is a single server with one or more client connections.
type notifyTestEnv struct {
	t       *testing.T
	server  *imapserver.Server
	ln      net.Listener
	closers []io.Closer
}

func newNotifyTestEnv(t *testing.T) *notifyTestEnv {
	t.Helper()

	memServer := imapmemserver.New()

	user := imapmemserver.NewUser("user", "pass")
	for _, name := range []string{"INBOX", "Other", "Third"} {
		if err := user.Create(name, nil); err != nil {
			t.Fatalf("Create(%q) = %v", name, err)
		}
	}
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
			imap.CapNotify:    {},
		},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil {
			t.Logf("Serve() = %v", err)
		}
	}()

	env := &notifyTestEnv{t: t, server: server, ln: ln}
	t.Cleanup(func() {
		server.Close()
	})
	return env
}

func (env *notifyTestEnv) connect(options *imapclient.Options) *imapclient.Client {
	env.t.Helper()

	conn, err := net.Dial("tcp", env.ln.Addr().String())
	if err != nil {
		env.t.Fatalf("net.Dial() = %v", err)
	}

	client := imapclient.New(conn, options)
	env.closers = append(env.closers, client)
	if err := client.Login("user", "pass").Wait(); err != nil {
		env.t.Fatalf("Login() = %v", err)
	}
	return client
}

func (env *notifyTestEnv) notifyClient(events chan<- notifyEventRecord) *imapclient.Client {
	env.t.Helper()

	options := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Status: func(data *imap.StatusData) {
				events <- notifyEventRecord{
					kind:        "STATUS",
					mailbox:     data.Mailbox,
					numMessages: *data.NumMessages,
				}
			},
			Fetch: func(msg *imapclient.FetchMessageData) {
				var (
					uid   imap.UID
					flags []imap.Flag
				)
				for {
					item := msg.Next()
					if item == nil {
						break
					}
					switch data := item.(type) {
					case imapclient.FetchItemDataUID:
						uid = data.UID
					case imapclient.FetchItemDataFlags:
						flags = data.Flags
					}
				}
				events <- notifyEventRecord{kind: "FLAGCHANGE", uid: uid, flags: flags}
			},
			Expunge: func(seqNum uint32) {
				events <- notifyEventRecord{kind: "EXPUNGE"}
			},
			NotificationOverflow: func() {
				events <- notifyEventRecord{kind: "OVERFLOW"}
			},
		},
	}
	return env.connect(options)
}

func waitNotifyEvent(t *testing.T, ch <-chan notifyEventRecord, what string) notifyEventRecord {
	t.Helper()
	select {
	case event := <-ch:
		return event
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		return notifyEventRecord{}
	}
}

func appendMessage(t *testing.T, client *imapclient.Client, mailbox string) imap.UID {
	t.Helper()
	raw := "Subject: test\r\n\r\nhello\r\n"
	cmd := client.Append(mailbox, int64(len(raw)), nil)
	cmd.Write([]byte(raw))
	cmd.Close()
	data, err := cmd.Wait()
	if err != nil {
		t.Fatalf("Append(%q) = %v", mailbox, err)
	}
	return data.UID
}

// TestServerNotifyTwoSessions brings up two sessions on the same account,
// subscribes them to an overlapping set of mailboxes and then interleaves
// APPEND, STORE and EXPUNGE operations. It checks the event types, mailbox
// names and ordering received by the session which didn't perform the
// operation.
func TestServerNotifyTwoSessions(t *testing.T) {
	eventsA := make(chan notifyEventRecord, 64)
	eventsB := make(chan notifyEventRecord, 64)

	env := newNotifyTestEnv(t)
	clientA := env.notifyClient(eventsA)
	clientB := env.notifyClient(eventsB)
	operator := env.connect(nil)

	subscribe(t, clientA, "INBOX", "Other")
	subscribe(t, clientB, "Other", "Third")

	// A idles and receives pushed NOTIFY events. B performs the mutations from
	// another connection.
	idleA, err := clientA.Idle()
	if err != nil {
		t.Fatalf("A Idle() = %v", err)
	}

	uidB := appendMessage(t, clientB, "Other")

	event := waitNotifyEvent(t, eventsA, "A MessageNew on Other")
	if event.kind != "STATUS" || event.mailbox != "Other" || event.numMessages != 1 {
		t.Fatalf("A event = %+v, want STATUS Other MESSAGES=1", event)
	}

	// Flag change: B selects the mailbox and stores flags.
	if _, err := clientB.Select("Other", nil).Wait(); err != nil {
		t.Fatalf("B Select(Other) = %v", err)
	}
	if err := clientB.Store(imap.UIDSetNum(uidB), &imap.StoreFlags{
		Op:    imap.StoreFlagsSet,
		Flags: []imap.Flag{imap.FlagSeen, imap.FlagFlagged},
	}, nil).Close(); err != nil {
		t.Fatalf("B Store() = %v", err)
	}

	event = waitNotifyEvent(t, eventsA, "A FlagChange on Other")
	if event.kind != "FLAGCHANGE" || event.uid != uidB {
		t.Fatalf("A event = %+v, want FLAGCHANGE uid=%v", event, uidB)
	}
	if !hasFlag(event.flags, imap.FlagSeen) || !hasFlag(event.flags, imap.FlagFlagged) {
		t.Fatalf("A FlagChange flags = %v, want Seen and Flagged", event.flags)
	}

	// Mark \\Deleted and expunge. A gets a FlagChange then an expunge event
	// (reported as a STATUS MESSAGES count for the non-selected mailbox).
	if err := clientB.Store(imap.UIDSetNum(uidB), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		t.Fatalf("B Store(+Deleted) = %v", err)
	}

	event = waitNotifyEvent(t, eventsA, "A FlagChange for +Deleted")
	if event.kind != "FLAGCHANGE" || event.uid != uidB || !hasFlag(event.flags, imap.FlagDeleted) {
		t.Fatalf("A event = %+v, want FLAGCHANGE with Deleted uid=%v", event, uidB)
	}

	if err := clientB.Expunge().Close(); err != nil {
		t.Fatalf("B Expunge() = %v", err)
	}

	event = waitNotifyEvent(t, eventsA, "A MessageExpunge on Other")
	if event.kind != "STATUS" || event.mailbox != "Other" || event.numMessages != 0 {
		t.Fatalf("A event = %+v, want STATUS Other MESSAGES=0", event)
	}

	// Mailbox scoping: append to INBOX (not subscribed by B) and Third
	// (subscribed by B). A leaves IDLE first; B then idles and must only see
	// the Third event.
	if err := idleA.Close(); err != nil {
		t.Fatalf("A Idle().Close() = %v", err)
	}

	appendMessage(t, operator, "INBOX")
	appendMessage(t, operator, "Third")

	// Drain events B accumulated earlier for the "Other" mailbox (it was a
	// subscriber during the first half of the test while not idling).
drainB:
	for {
		select {
		case <-eventsB:
		default:
			break drainB
		}
	}

	idleB, err := clientB.Idle()
	if err != nil {
		t.Fatalf("B Idle() = %v", err)
	}

	event = waitNotifyEvent(t, eventsB, "B MessageNew on Third")
	if event.kind != "STATUS" || event.mailbox != "Third" || event.numMessages != 1 {
		t.Fatalf("B event = %+v, want STATUS Third MESSAGES=1", event)
	}

	select {
	case stray := <-eventsB:
		t.Fatalf("B received an unexpected event: %+v", stray)
	case <-time.After(200 * time.Millisecond):
	}

	if err := idleB.Close(); err != nil {
		t.Fatalf("B Idle().Close() = %v", err)
	}
}

func subscribe(t *testing.T, client *imapclient.Client, mailboxes ...string) {
	t.Helper()
	cmd, err := client.Notify(&imap.NotifyOptions{
		Items: []imap.NotifyItem{{
			Mailboxes: mailboxes,
			Events: []imap.NotifyEvent{
				imap.NotifyEventMessageNew,
				imap.NotifyEventMessageExpunge,
				imap.NotifyEventFlagChange,
			},
		}},
	})
	if err != nil {
		t.Fatalf("Notify() = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Notify().Wait() = %v", err)
	}
}

// TestServerNotifyReplacement verifies that a new NOTIFY SET replaces the
// previous subscription rather than adding to it.
func TestServerNotifyReplacement(t *testing.T) {
	events := make(chan notifyEventRecord, 32)
	env := newNotifyTestEnv(t)
	client := env.notifyClient(events)
	other := env.connect(nil)

	subscribe(t, client, "INBOX")
	subscribe(t, client, "Third")

	idle, err := client.Idle()
	if err != nil {
		t.Fatalf("Idle() = %v", err)
	}

	appendMessage(t, other, "INBOX")
	appendMessage(t, other, "Third")

	event := waitNotifyEvent(t, events, "MessageNew on Third only")
	if event.kind != "STATUS" || event.mailbox != "Third" {
		t.Fatalf("event = %+v, want STATUS Third", event)
	}

	select {
	case stray := <-events:
		t.Fatalf("received event for replaced subscription: %+v", stray)
	case <-time.After(200 * time.Millisecond):
	}

	if err := idle.Close(); err != nil {
		t.Fatalf("Idle().Close() = %v", err)
	}
}

// TestServerNotifyUnsupported verifies that event/specifier combinations the
// server doesn't implement are rejected with a protocol error.
func TestServerNotifyUnsupported(t *testing.T) {
	env := newNotifyTestEnv(t)
	client := env.connect(nil)

	tests := []struct {
		name    string
		options *imap.NotifyOptions
	}{
		{
			name: "mailbox event on SELECTED",
			options: &imap.NotifyOptions{
				Items: []imap.NotifyItem{{
					MailboxSpec: imap.NotifyMailboxSpecSelected,
					Events:      []imap.NotifyEvent{imap.NotifyEventMailboxName},
				}},
			},
		},
		{
			name: "INBOXES specifier",
			options: &imap.NotifyOptions{
				Items: []imap.NotifyItem{{
					MailboxSpec: imap.NotifyMailboxSpecInboxes,
					Events:      []imap.NotifyEvent{imap.NotifyEventMessageNew},
				}},
			},
		},
		{
			name: "SUBSCRIBED specifier",
			options: &imap.NotifyOptions{
				Items: []imap.NotifyItem{{
					MailboxSpec: imap.NotifyMailboxSpecSubscribed,
					Events:      []imap.NotifyEvent{imap.NotifyEventMessageNew},
				}},
			},
		},
		{
			name: "annotation event on PERSONAL",
			options: &imap.NotifyOptions{
				Items: []imap.NotifyItem{{
					MailboxSpec: imap.NotifyMailboxSpecPersonal,
					Events:      []imap.NotifyEvent{imap.NotifyEventAnnotationChange},
				}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := client.Notify(tc.options)
			if err != nil {
				t.Fatalf("Notify() send error = %v", err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatalf("Notify() = nil, want a BAD protocol error")
			}
		})
	}
}

func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}

// TestServerNotifyOverflow verifies that a client which doesn't drain events
// gets NOTIFICATIONOVERFLOW instead of the server buffering events forever.
func TestServerNotifyOverflow(t *testing.T) {
	events := make(chan notifyEventRecord, 4096)
	env := newNotifyTestEnv(t)
	client := env.notifyClient(events)
	operator := env.connect(nil)

	subscribe(t, client, "INBOX")

	// Flood the mailbox with APPENDs while the subscriber isn't idling. The
	// MESSAGES count events coalesce, so generate distinct FlagChange events
	// instead by storing flags on distinct messages. First append many
	// messages, then flag each one.
	const count = 400
	for i := 0; i < count; i++ {
		appendMessage(t, operator, "INBOX")
	}

	// Select and flag every message from the operator connection to produce
	// per-UID FlagChange notifications which don't coalesce across UIDs.
	if _, err := operator.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select(INBOX) = %v", err)
	}
	uids := imap.UIDSetNum()
	for i := uint32(1); i <= count; i++ {
		uids.AddNum(imap.UID(i))
	}
	if err := operator.Store(uids, &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, nil).Close(); err != nil {
		t.Fatalf("Store() = %v", err)
	}

	idle, err := client.Idle()
	if err != nil {
		t.Fatalf("Idle() = %v", err)
	}

	// Eventually the subscriber must observe an overflow indication rather
	// than waiting for every event.
	deadline := time.After(5 * time.Second)
	sawOverflow := false
	for !sawOverflow {
		select {
		case event := <-events:
			if event.kind == "OVERFLOW" {
				sawOverflow = true
			}
		case <-deadline:
			t.Fatal("never received NOTIFICATIONOVERFLOW for a flooded subscriber")
		}
	}

	if err := idle.Close(); err != nil {
		t.Fatalf("Idle().Close() = %v", err)
	}
}
