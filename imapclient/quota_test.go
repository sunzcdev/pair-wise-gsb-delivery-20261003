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
	quotaTestAdminUsername = "quota-admin"
	quotaTestAdminPassword = "quota-admin-password"
	quotaTestUserUsername  = "quota-user"
	quotaTestUserPassword  = "quota-user-password"

	quotaTestMessage = "Subject: quota test\r\n\r\nHello!\r\n"
)

// newQuotaTestServer starts an in-memory IMAP server with the QUOTA
// extension enabled. It returns a function to create new logged-in clients.
func newQuotaTestServer(t *testing.T) (newClient func(username, password string) *imapclient.Client) {
	t.Helper()

	memServer := imapmemserver.New()

	admin := imapmemserver.NewAdminUser(quotaTestAdminUsername, quotaTestAdminPassword)
	if err := admin.Create("INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if err := admin.Create("Archive", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	memServer.AddUser(admin)

	user := imapmemserver.NewUser(quotaTestUserUsername, quotaTestUserPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:             {},
			imap.CapIMAP4rev2:             {},
			imap.CapQuota:                 {},
			imap.CapQuotaSet:              {},
			imap.Cap("QUOTA=RES-STORAGE"): {},
			imap.Cap("QUOTA=RES-MESSAGE"): {},
		},
	})
	t.Cleanup(func() { server.Close() })

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil {
			t.Errorf("Serve() = %v", err)
		}
	}()

	return func(username, password string) *imapclient.Client {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("net.Dial() = %v", err)
		}
		client := imapclient.New(conn, nil)
		t.Cleanup(func() { client.Close() })
		if err := client.Login(username, password).Wait(); err != nil {
			t.Fatalf("Login().Wait() = %v", err)
		}
		return client
	}
}

func appendMessage(client *imapclient.Client, mailbox string) error {
	appendCmd := client.Append(mailbox, int64(len(quotaTestMessage)), nil)
	if _, err := appendCmd.Write([]byte(quotaTestMessage)); err != nil {
		return err
	}
	if err := appendCmd.Close(); err != nil {
		return err
	}
	_, err := appendCmd.Wait()
	return err
}

func isOverQuota(err error) bool {
	var imapErr *imap.Error
	return errors.As(err, &imapErr) && imapErr.Code == imap.ResponseCodeOverQuota
}

func getQuotaResource(t *testing.T, client *imapclient.Client, root string, resourceType imap.QuotaResourceType) imapclient.QuotaResourceData {
	t.Helper()
	data, err := client.GetQuota(root).Wait()
	if err != nil {
		t.Fatalf("GetQuota().Wait() = %v", err)
	}
	resource, ok := data.Resources[resourceType]
	if !ok {
		t.Fatalf("GetQuota() missing resource %v", resourceType)
	}
	return resource
}

func numMessages(t *testing.T, client *imapclient.Client, mailbox string) uint32 {
	t.Helper()
	data, err := client.Status(mailbox, &imap.StatusOptions{NumMessages: true}).Wait()
	if err != nil {
		t.Fatalf("Status().Wait() = %v", err)
	}
	if data.NumMessages == nil {
		t.Fatal("Status() missing NumMessages")
	}
	return *data.NumMessages
}

func TestQuotaPermissions(t *testing.T) {
	newClient := newQuotaTestServer(t)

	// A regular user is not allowed to set quotas
	userClient := newClient(quotaTestUserUsername, quotaTestUserPassword)
	err := userClient.SetQuota("", map[imap.QuotaResourceType]int64{
		imap.QuotaResourceMessage: 10,
	}).Wait()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("SetQuota().Wait() = %v, want NOPERM error", err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo || imapErr.Code != imap.ResponseCodeNoPerm {
		t.Errorf("SetQuota().Wait() = %v, want NO [NOPERM]", imapErr)
	}

	// An administrator is allowed to set quotas
	adminClient := newClient(quotaTestAdminUsername, quotaTestAdminPassword)
	if err := adminClient.SetQuota("", map[imap.QuotaResourceType]int64{
		imap.QuotaResourceMessage: 10,
	}).Wait(); err != nil {
		t.Errorf("SetQuota().Wait() = %v", err)
	}
}

func TestQuota(t *testing.T) {
	newClient := newQuotaTestServer(t)
	client := newClient(quotaTestAdminUsername, quotaTestAdminPassword)

	if err := client.SetQuota("", map[imap.QuotaResourceType]int64{
		imap.QuotaResourceStorage: 1 << 20,
		imap.QuotaResourceMessage: 3,
	}).Wait(); err != nil {
		t.Fatalf("SetQuota().Wait() = %v", err)
	}

	// The quota root covers multiple mailboxes
	quotaRootData, err := client.GetQuotaRoot("INBOX").Wait()
	if err != nil {
		t.Fatalf("GetQuotaRoot().Wait() = %v", err)
	}
	if len(quotaRootData) != 1 || quotaRootData[0].Root != "" {
		t.Fatalf("GetQuotaRoot() = %v, want a single empty root", quotaRootData)
	}
	if resource := quotaRootData[0].Resources[imap.QuotaResourceMessage]; resource.Usage != 0 || resource.Limit != 3 {
		t.Errorf("MESSAGE resource = %+v, want usage 0 limit 3", resource)
	}

	// Fill the quota from two different mailboxes covered by the same root
	for i := 0; i < 2; i++ {
		if err := appendMessage(client, "INBOX"); err != nil {
			t.Fatalf("Append() = %v", err)
		}
	}
	if err := appendMessage(client, "Archive"); err != nil {
		t.Fatalf("Append() = %v", err)
	}
	if resource := getQuotaResource(t, client, "", imap.QuotaResourceMessage); resource.Usage != 3 || resource.Limit != 3 {
		t.Errorf("MESSAGE resource = %+v, want usage 3 limit 3", resource)
	}

	// The quota is full: the next APPEND fails and doesn't consume quota
	if err := appendMessage(client, "INBOX"); !isOverQuota(err) {
		t.Errorf("Append() = %v, want OVERQUOTA error", err)
	}
	if resource := getQuotaResource(t, client, "", imap.QuotaResourceMessage); resource.Usage != 3 {
		t.Errorf("MESSAGE usage after failed APPEND = %v, want 3", resource.Usage)
	}

	// Moving messages inside the same quota root doesn't count them twice
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select().Wait() = %v", err)
	}
	if _, err := client.Move(imap.UIDSetNum(1, 2), "Archive").Wait(); err != nil {
		t.Fatalf("Move().Wait() = %v", err)
	}
	if resource := getQuotaResource(t, client, "", imap.QuotaResourceMessage); resource.Usage != 3 {
		t.Errorf("MESSAGE usage after MOVE = %v, want 3", resource.Usage)
	}
	if n := numMessages(t, client, "INBOX"); n != 0 {
		t.Errorf("INBOX message count = %v, want 0", n)
	}
	if n := numMessages(t, client, "Archive"); n != 3 {
		t.Errorf("Archive message count = %v, want 3", n)
	}

	// Expunging releases quota
	if _, err := client.Select("Archive", nil).Wait(); err != nil {
		t.Fatalf("Select().Wait() = %v", err)
	}
	storeCmd := client.Store(imap.UIDSetNum(2, 3), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  []imap.Flag{imap.FlagDeleted},
		Silent: true,
	}, nil)
	if err := storeCmd.Close(); err != nil {
		t.Fatalf("Store().Close() = %v", err)
	}
	if err := client.Expunge().Close(); err != nil {
		t.Fatalf("Expunge().Close() = %v", err)
	}
	if resource := getQuotaResource(t, client, "", imap.QuotaResourceMessage); resource.Usage != 1 {
		t.Errorf("MESSAGE usage after EXPUNGE = %v, want 1", resource.Usage)
	}

	// The STORAGE resource is enforced in bytes
	storage := getQuotaResource(t, client, "", imap.QuotaResourceStorage)
	if want := int64(len(quotaTestMessage)); storage.Usage != want {
		t.Errorf("STORAGE usage = %v, want %v", storage.Usage, want)
	}
	if err := client.SetQuota("", map[imap.QuotaResourceType]int64{
		imap.QuotaResourceStorage: storage.Usage,
	}).Wait(); err != nil {
		t.Fatalf("SetQuota().Wait() = %v", err)
	}
	if err := appendMessage(client, "INBOX"); !isOverQuota(err) {
		t.Errorf("Append() = %v, want OVERQUOTA error", err)
	}
}

// TestQuotaConcurrentAppend has two sessions appending concurrently to
// different mailboxes covered by the same quota root. The total number of
// successful APPEND commands must not exceed the quota.
func TestQuotaConcurrentAppend(t *testing.T) {
	newClient := newQuotaTestServer(t)

	adminClient := newClient(quotaTestAdminUsername, quotaTestAdminPassword)
	if err := adminClient.SetQuota("", map[imap.QuotaResourceType]int64{
		imap.QuotaResourceStorage: 1 << 20,
		imap.QuotaResourceMessage: 10,
	}).Wait(); err != nil {
		t.Fatalf("SetQuota().Wait() = %v", err)
	}

	const perSession = 8

	var wg sync.WaitGroup
	var mutex sync.Mutex
	var succeeded, overQuota int
	appendLoop := func(mailbox string) {
		defer wg.Done()
		client := newClient(quotaTestAdminUsername, quotaTestAdminPassword)
		for i := 0; i < perSession; i++ {
			err := appendMessage(client, mailbox)
			mutex.Lock()
			switch {
			case err == nil:
				succeeded++
			case isOverQuota(err):
				overQuota++
			default:
				t.Errorf("Append() = %v", err)
			}
			mutex.Unlock()
		}
	}

	wg.Add(2)
	go appendLoop("INBOX")
	go appendLoop("Archive")
	wg.Wait()

	if succeeded != 10 {
		t.Errorf("successful APPEND commands = %v, want 10", succeeded)
	}
	if overQuota != 2*perSession-10 {
		t.Errorf("OVERQUOTA errors = %v, want %v", overQuota, 2*perSession-10)
	}

	// The actual number of messages matches the quota usage
	if n := numMessages(t, adminClient, "INBOX") + numMessages(t, adminClient, "Archive"); n != 10 {
		t.Errorf("total message count = %v, want 10", n)
	}
	if resource := getQuotaResource(t, adminClient, "", imap.QuotaResourceMessage); resource.Usage != 10 || resource.Limit != 10 {
		t.Errorf("MESSAGE resource = %+v, want usage 10 limit 10", resource)
	}
	storage := getQuotaResource(t, adminClient, "", imap.QuotaResourceStorage)
	if want := int64(10 * len(quotaTestMessage)); storage.Usage != want {
		t.Errorf("STORAGE usage = %v, want %v", storage.Usage, want)
	}
}
