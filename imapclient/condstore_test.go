package imapclient_test

import (
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// TestCondStore checks CONDSTORE with two clients cross-modifying flags in
// the same mailbox.
func TestCondStore(t *testing.T) {
	memServer := imapmemserver.New()

	user := imapmemserver.NewUser(testUsername, testPassword)
	user.Create("INBOX", nil)
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		},
	})
	defer server.Close()

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil {
			t.Errorf("Serve() = %v", err)
		}
	}()

	fetchUpdates := make(chan *imapclient.FetchMessageBuffer, 32)
	newClient := func(options *imapclient.Options) *imapclient.Client {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("net.Dial() = %v", err)
		}
		client := imapclient.New(conn, options)
		if err := client.Login(testUsername, testPassword).Wait(); err != nil {
			t.Fatalf("Login().Wait() = %v", err)
		}
		return client
	}

	client1 := newClient(&imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Fetch: func(data *imapclient.FetchMessageData) {
				buf, err := data.Collect()
				if err != nil {
					t.Errorf("FetchMessageData.Collect() = %v", err)
					return
				}
				fetchUpdates <- buf
			},
		},
	})
	defer client1.Close()
	client2 := newClient(nil)
	defer client2.Close()

	for i := 0; i < 3; i++ {
		appendCmd := client1.Append("INBOX", int64(len(simpleRawMessage)), nil)
		appendCmd.Write([]byte(simpleRawMessage))
		appendCmd.Close()
		if _, err := appendCmd.Wait(); err != nil {
			t.Fatalf("AppendCommand.Wait() = %v", err)
		}
	}

	selectData1, err := client1.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait()
	if err != nil {
		t.Fatalf("Select().Wait() = %v", err)
	}
	if selectData1.HighestModSeq != 4 {
		t.Fatalf("SelectData.HighestModSeq = %v, want 4", selectData1.HighestModSeq)
	}
	selectData2, err := client2.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait()
	if err != nil {
		t.Fatalf("Select().Wait() = %v", err)
	}
	if selectData2.HighestModSeq != selectData1.HighestModSeq {
		t.Fatalf("clients see different HIGHESTMODSEQ: %v != %v", selectData2.HighestModSeq, selectData1.HighestModSeq)
	}

	// client2 performs a conditional STORE
	storeFlags := imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagFlagged},
	}
	storeOptions := imap.StoreOptions{UnchangedSince: selectData2.HighestModSeq}
	msgs, err := client2.Store(imap.SeqSetNum(2), &storeFlags, &storeOptions).Collect()
	if err != nil {
		t.Fatalf("Store().Collect() = %v", err)
	} else if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %v, want 1", len(msgs))
	}
	if msgs[0].ModSeq != 5 {
		t.Errorf("msgs[0].ModSeq = %v, want 5", msgs[0].ModSeq)
	}

	// client1 is asynchronously notified of the change
	if err := client1.Noop().Wait(); err != nil {
		t.Fatalf("Noop().Wait() = %v", err)
	}
	select {
	case buf := <-fetchUpdates:
		if buf.SeqNum != 2 {
			t.Errorf("buf.SeqNum = %v, want 2", buf.SeqNum)
		}
		if buf.ModSeq != 5 {
			t.Errorf("buf.ModSeq = %v, want 5", buf.ModSeq)
		}
		found := false
		for _, flag := range buf.Flags {
			if flag == imap.FlagFlagged {
				found = true
			}
		}
		if !found {
			t.Errorf("buf.Flags is missing flagged flag: %v", buf.Flags)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for unilateral FETCH data")
	}

	// client1 fetches the messages changed since it last synced
	msgs, err = client1.Fetch(imap.SeqSetNum(1, 2, 3), &imap.FetchOptions{
		Flags:        true,
		ChangedSince: selectData1.HighestModSeq,
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	} else if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %v, want 1", len(msgs))
	}
	if msgs[0].SeqNum != 2 || msgs[0].ModSeq != 5 {
		t.Errorf("msgs[0] = (seq %v, modseq %v), want (2, 5)", msgs[0].SeqNum, msgs[0].ModSeq)
	}

	// client1 conditionally stores with a stale mod-sequence: the update
	// partially applies (message 1 succeeds, message 2 was changed by
	// client2 in the meantime)
	silentStoreFlags := imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagAnswered},
	}
	staleStoreOptions := imap.StoreOptions{UnchangedSince: 4}
	if _, err := client1.Store(imap.UIDSetNum(1, 2), &silentStoreFlags, &staleStoreOptions).Collect(); err != nil {
		t.Fatalf("Store().Collect() = %v", err)
	}

	// The successful part of the conditional STORE is visible to client2
	msgs, err = client2.Fetch(imap.UIDSetNum(1, 2), &imap.FetchOptions{
		Flags:        true,
		ChangedSince: 5,
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	} else if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %v, want 1", len(msgs))
	}
	if msgs[0].UID != 1 || msgs[0].ModSeq != 6 {
		t.Errorf("msgs[0] = (uid %v, modseq %v), want (1, 6)", msgs[0].UID, msgs[0].ModSeq)
	}

	// Re-selecting the mailbox reports the up-to-date HIGHESTMODSEQ
	if err := client1.Unselect().Wait(); err != nil {
		t.Fatalf("Unselect().Wait() = %v", err)
	}
	selectData1, err = client1.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait()
	if err != nil {
		t.Fatalf("Select().Wait() = %v", err)
	}
	if selectData1.HighestModSeq != 6 {
		t.Errorf("SelectData.HighestModSeq = %v, want 6", selectData1.HighestModSeq)
	}

	// STATUS also exposes HIGHESTMODSEQ
	statusData, err := client1.Status("INBOX", &imap.StatusOptions{HighestModSeq: true}).Wait()
	if err != nil {
		t.Fatalf("Status().Wait() = %v", err)
	}
	if statusData.HighestModSeq != 6 {
		t.Errorf("StatusData.HighestModSeq = %v, want 6", statusData.HighestModSeq)
	}
}
