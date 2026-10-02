package imapclient_test

import (
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const (
	quotaUsername = "quota-user"
	quotaPassword = "quota-password"
)

// quotaTestEnv starts an in-memory IMAP service with a regular endpoint
// (QUOTA only) and an administrative endpoint (QUOTA + QUOTASET), both backed
// by the same users and the same quota state.
type quotaTestEnv struct {
	t         *testing.T
	mem       *imapmemserver.Server
	userAddr  string
	adminAddr string
}

func newQuotaTestEnv(t *testing.T) *quotaTestEnv {
	t.Helper()

	mem := imapmemserver.New()

	user := imapmemserver.NewUser(quotaUsername, quotaPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("Create INBOX: %v", err)
	}
	if err := user.Create("Other", nil); err != nil {
		t.Fatalf("Create Other: %v", err)
	}
	mem.AddUser(user)

	return &quotaTestEnv{
		t:         t,
		mem:       mem,
		userAddr:  listenQuota(t, mem, false),
		adminAddr: listenQuota(t, mem, true),
	}
}

func listenQuota(t *testing.T, mem *imapmemserver.Server, admin bool) string {
	t.Helper()
	caps := imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapIMAP4rev2: {},
		imap.CapQuota:     {},
	}
	newSession := func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return mem.NewSession(), nil, nil
	}
	if admin {
		caps[imap.CapQuotaSet] = struct{}{}
		newSession = func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewAdminSession(), nil, nil
		}
	}

	server := imapserver.New(&imapserver.Options{
		NewSession:   newSession,
		Caps:         caps,
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { server.Close() })
	return ln.Addr().String()
}

func (env *quotaTestEnv) dialLogin(t *testing.T, admin bool) *imapclient.Client {
	t.Helper()

	addr := env.userAddr
	if admin {
		addr = env.adminAddr
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	client := imapclient.New(conn, nil)
	if err := client.Login(quotaUsername, quotaPassword).Wait(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	return client
}

// assertOverQuota checks that err is an IMAP NO/OVERQUOTA error.
func assertOverQuota(t *testing.T, err error) {
	t.Helper()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("got %v, want *imap.Error", err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("error type = %q, want NO", imapErr.Type)
	}
	if imapErr.Code != imap.ResponseCodeOverQuota {
		t.Errorf("error code = %q, want OVERQUOTA", imapErr.Code)
	}
}

func appendMessage(t *testing.T, client *imapclient.Client, mailbox, body string) error {
	t.Helper()
	cmd := client.Append(mailbox, int64(len(body)), nil)
	if _, err := cmd.Write([]byte(body)); err != nil {
		t.Fatalf("Append.Write: %v", err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("Append.Close: %v", err)
	}
	_, err := cmd.Wait()
	return err
}

// TestQuotaConcurrentSharedRoot is the core concurrency test: two sessions
// append concurrently to two different mailboxes that share one quota root.
//
// With a MESSAGE limit of 1, exactly one append may succeed even though both
// sessions race the check. The error code, the actual message counts and the
// GETQUOTA usage must all agree.
func TestQuotaConcurrentSharedRoot(t *testing.T) {
	env := newQuotaTestEnv(t)

	// Configure the quota through the administrative endpoint.
	admin := env.dialLogin(t, true)
	defer admin.Close()
	if err := admin.SetQuota(imapmemserver.DefaultQuotaRoot, map[imap.QuotaResourceType]int64{
		imap.QuotaResourceMessage: 1,
	}).Wait(); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}

	clientA := env.dialLogin(t, false)
	defer clientA.Close()
	clientB := env.dialLogin(t, false)
	defer clientB.Close()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var errA, errB error
	go func() {
		defer wg.Done()
		<-start
		errA = appendMessage(t, clientA, "INBOX", "message A")
	}()
	go func() {
		defer wg.Done()
		<-start
		errB = appendMessage(t, clientB, "Other", "message B")
	}()
	close(start)
	wg.Wait()

	// Exactly one of the two appends must succeed.
	switch {
	case errA == nil && errB != nil:
		assertOverQuota(t, errB)
	case errB == nil && errA != nil:
		assertOverQuota(t, errA)
	default:
		t.Fatalf("expected exactly one success, got errA=%v errB=%v", errA, errB)
	}

	// The two mailboxes must together hold exactly one message.
	nA := mailboxMessages(t, clientA, "INBOX")
	nB := mailboxMessages(t, clientB, "Other")
	if nA+nB != 1 {
		t.Errorf("total messages = %d (%d+%d), want 1", nA+nB, nA, nB)
	}

	// GETQUOTA must report the same reality: 1/1 on the shared root.
	data, err := clientA.GetQuota(imapmemserver.DefaultQuotaRoot).Wait()
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	msgRes := data.Resources[imap.QuotaResourceMessage]
	if msgRes.Usage != 1 || msgRes.Limit != 1 {
		t.Errorf("MESSAGE usage/limit = %d/%d, want 1/1", msgRes.Usage, msgRes.Limit)
	}

	// Both mailboxes resolve to the same quota root.
	rootsA, err := clientA.GetQuotaRoot("INBOX").Wait()
	if err != nil {
		t.Fatalf("GetQuotaRoot INBOX: %v", err)
	}
	rootsB, err := clientB.GetQuotaRoot("Other").Wait()
	if err != nil {
		t.Fatalf("GetQuotaRoot Other: %v", err)
	}
	if len(rootsA) != 1 || len(rootsB) != 1 ||
		rootsA[0].Root != imapmemserver.DefaultQuotaRoot ||
		rootsB[0].Root != imapmemserver.DefaultQuotaRoot {
		t.Fatalf("both mailboxes must map to the default root, got %+v %+v", rootsA, rootsB)
	}
	if rootsA[0].Resources[imap.QuotaResourceMessage] != msgRes ||
		rootsB[0].Resources[imap.QuotaResourceMessage] != msgRes {
		t.Errorf("GETQUOTAROOT resource data inconsistent with GETQUOTA")
	}
}

func mailboxMessages(t *testing.T, client *imapclient.Client, mailbox string) uint32 {
	t.Helper()
	data, err := client.Status(mailbox, &imap.StatusOptions{NumMessages: true}).Wait()
	if err != nil {
		t.Fatalf("Status %s: %v", mailbox, err)
	}
	if data.NumMessages == nil {
		t.Fatalf("Status %s returned no message count", mailbox)
	}
	return *data.NumMessages
}

// TestQuotaPermissionSeparation verifies that only the administrative
// endpoint advertises QUOTASET and accepts SETQUOTA, while the regular
// endpoint advertises QUOTA but neither advertises QUOTASET nor allows quota
// changes.
func TestQuotaPermissionSeparation(t *testing.T) {
	env := newQuotaTestEnv(t)

	user := env.dialLogin(t, false)
	defer user.Close()
	admin := env.dialLogin(t, true)
	defer admin.Close()

	if !user.Caps().Has(imap.CapQuota) {
		t.Errorf("regular session should advertise QUOTA")
	}
	if user.Caps().Has(imap.CapQuotaSet) {
		t.Errorf("regular session must not advertise QUOTASET")
	}
	if !admin.Caps().Has(imap.CapQuotaSet) {
		t.Errorf("admin session should advertise QUOTASET")
	}

	err := user.SetQuota(imapmemserver.DefaultQuotaRoot, map[imap.QuotaResourceType]int64{
		imap.QuotaResourceMessage: 5,
	}).Wait()
	if err == nil {
		t.Fatalf("regular user SETQUOTA should be rejected")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeBad {
		t.Fatalf("regular SETQUOTA err = %v, want BAD", err)
	}

	// The resource capabilities must be advertised alongside QUOTA.
	if !user.Caps().Has(imap.Cap("QUOTA=RES-STORAGE")) ||
		!user.Caps().Has(imap.Cap("QUOTA=RES-MESSAGE")) {
		t.Errorf("missing QUOTA=RES-* capabilities in %v", user.Caps())
	}
}

// TestQuotaStorageUnitsAndExpunge checks the STORAGE resource is reported in
// kibibytes (ceil(size/1024)), that a too-big append fails with OVERQUOTA
// without consuming anything, and that EXPUNGE releases quota.
func TestQuotaStorageUnitsAndExpunge(t *testing.T) {
	env := newQuotaTestEnv(t)

	admin := env.dialLogin(t, true)
	defer admin.Close()
	// 1 KiB of storage, plenty of messages.
	if err := admin.SetQuota(imapmemserver.DefaultQuotaRoot, map[imap.QuotaResourceType]int64{
		imap.QuotaResourceStorage: 1,
		imap.QuotaResourceMessage: 100,
	}).Wait(); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}

	client := env.dialLogin(t, false)
	defer client.Close()

	// A 1-byte message consumes exactly one STORAGE unit (1024 octets) due
	// to ceiling.
	if err := appendMessage(t, client, "INBOX", "x"); err != nil {
		t.Fatalf("append 1-byte: %v", err)
	}
	data, err := client.GetQuota(imapmemserver.DefaultQuotaRoot).Wait()
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	storage := data.Resources[imap.QuotaResourceStorage]
	if storage.Usage != 1 || storage.Limit != 1 {
		t.Fatalf("STORAGE usage/limit = %d/%d, want 1/1", storage.Usage, storage.Limit)
	}

	// Root is full on storage: another append must fail and not be counted.
	err = appendMessage(t, client, "Other", "y")
	assertOverQuota(t, err)

	if got := mailboxMessages(t, client, "Other"); got != 0 {
		t.Errorf("failed append left %d messages in Other, want 0", got)
	}
	data, _ = client.GetQuota(imapmemserver.DefaultQuotaRoot).Wait()
	if got := data.Resources[imap.QuotaResourceStorage].Usage; got != 1 {
		t.Errorf("failed append changed STORAGE usage to %d, want 1", got)
	}

	// Expunge the message after marking it deleted: usage must go back to 0.
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select: %v", err)
	}
	storeCmd := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagDeleted},
	}, nil)
	if err := storeCmd.Close(); err != nil {
		t.Fatalf("Store.Close: %v", err)
	}
	if err := client.Expunge().Close(); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	data, _ = client.GetQuota(imapmemserver.DefaultQuotaRoot).Wait()
	if got := data.Resources[imap.QuotaResourceStorage].Usage; got != 0 {
		t.Errorf("STORAGE usage after expunge = %d, want 0", got)
	}
	if got := data.Resources[imap.QuotaResourceMessage].Usage; got != 0 {
		t.Errorf("MESSAGE usage after expunge = %d, want 0", got)
	}

	// The released quota can be used again.
	if err := appendMessage(t, client, "Other", "z"); err != nil {
		t.Errorf("append after expunge: %v", err)
	}
}

// TestQuotaMoveSameRoot verifies that moving messages inside one quota root
// does not double-count them and does not fail merely because the root is
// full. COPY, by contrast, doubles the footprint and must be rejected.
func TestQuotaMoveSameRoot(t *testing.T) {
	env := newQuotaTestEnv(t)

	admin := env.dialLogin(t, true)
	defer admin.Close()
	if err := admin.SetQuota(imapmemserver.DefaultQuotaRoot, map[imap.QuotaResourceType]int64{
		imap.QuotaResourceMessage: 1,
	}).Wait(); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}

	client := env.dialLogin(t, false)
	defer client.Close()

	if err := appendMessage(t, client, "INBOX", "only one allowed"); err != nil {
		t.Fatalf("append: %v", err)
	}

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select: %v", err)
	}

	// MOVE inside the same, already-full root must succeed: usage stays 1.
	if _, err := client.Move(imap.SeqSetNum(1), "Other").Wait(); err != nil {
		t.Fatalf("Move within full root: %v", err)
	}
	data, _ := client.GetQuota(imapmemserver.DefaultQuotaRoot).Wait()
	if got := data.Resources[imap.QuotaResourceMessage].Usage; got != 1 {
		t.Errorf("MESSAGE usage after move = %d, want 1", got)
	}
	if got := mailboxMessages(t, client, "INBOX"); got != 0 {
		t.Errorf("INBOX messages after move = %d, want 0", got)
	}
	if got := mailboxMessages(t, client, "Other"); got != 1 {
		t.Errorf("Other messages after move = %d, want 1", got)
	}

	// Move it back, then COPY must fail: a copy is a second message.
	if _, err := client.Select("Other", nil).Wait(); err != nil {
		t.Fatalf("Select Other: %v", err)
	}
	if _, err := client.Move(imap.SeqSetNum(1), "INBOX").Wait(); err != nil {
		t.Fatalf("Move back: %v", err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select INBOX: %v", err)
	}
	_, err := client.Copy(imap.SeqSetNum(1), "Other").Wait()
	assertOverQuota(t, err)
	if got := mailboxMessages(t, client, "Other"); got != 0 {
		t.Errorf("failed COPY left %d messages in Other, want 0", got)
	}
	data, _ = client.GetQuota(imapmemserver.DefaultQuotaRoot).Wait()
	if got := data.Resources[imap.QuotaResourceMessage].Usage; got != 1 {
		t.Errorf("failed COPY changed usage to %d, want 1", got)
	}
}
