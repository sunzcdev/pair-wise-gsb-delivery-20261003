package imapclient_test

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestStore(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	seqSet := imap.SeqSetNum(1)
	storeFlags := imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagDeleted},
	}
	msgs, err := client.Store(seqSet, &storeFlags, nil).Collect()
	if err != nil {
		t.Fatalf("Store().Collect() = %v", err)
	} else if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %v, want %v", len(msgs), 1)
	}
	msg := msgs[0]
	if msg.SeqNum != 1 {
		t.Errorf("msg.SeqNum = %v, want %v", msg.SeqNum, 1)
	}

	found := false
	for _, f := range msg.Flags {
		if f == imap.FlagDeleted {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("msg.Flags is missing deleted flag: %v", msg.Flags)
	}
}

func TestStoreCondStore(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	// The newly appended message has modseq 0; fetch it.
	fetchMsgs, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		ModSeq: true,
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	} else if len(fetchMsgs) != 1 {
		t.Fatalf("len(fetchMsgs) = %v, want 1", len(fetchMsgs))
	}
	if fetchMsgs[0].ModSeq != 0 {
		t.Fatalf("initial ModSeq = %v, want 0", fetchMsgs[0].ModSeq)
	}

	// A real flag change bumps the message modseq.
	storeFlags := imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagFlagged},
	}
	msgs, err := client.Store(imap.SeqSetNum(1), &storeFlags, nil).Collect()
	if err != nil {
		t.Fatalf("Store().Collect() = %v", err)
	} else if len(msgs) != 1 || msgs[0].ModSeq != 2 {
		t.Fatalf("got %+v, want one message with modseq 2", msgs)
	}

	// CHANGEDSINCE 5 must not match anything.
	changed, err := client.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{
		ChangedSince: 5,
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch(CHANGEDSINCE 5) = %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("CHANGEDSINCE 5 matched %v messages, want 0", len(changed))
	}

	// CHANGEDSINCE 1 must match the changed message.
	changed, err = client.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{
		ChangedSince: 1,
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch(CHANGEDSINCE 1) = %v", err)
	}
	if len(changed) != 1 || changed[0].ModSeq != 2 {
		t.Fatalf("CHANGEDSINCE 1 got %+v, want modseq 2", changed)
	}
}

func TestSearchCondStore(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	data, err := client.UIDSearch(&imap.SearchCriteria{
		ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 0},
	}, &imap.SearchOptions{ReturnAll: true}).Wait()
	if err != nil {
		t.Fatalf("UIDSearch(MODSEQ 0) = %v", err)
	}
	uids := data.AllUIDs()
	if len(uids) != 1 || uids[0] != 1 {
		t.Fatalf("UIDSearch(MODSEQ 0) uids = %v, want [1]", uids)
	}
	if data.ModSeq == 0 {
		t.Fatalf("UIDSearch data.ModSeq = 0, want non-zero mailbox modseq")
	}
}

func TestCondStoreStatusAndSelect(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	status, err := client.Status("INBOX", &imap.StatusOptions{
		HighestModSeq: true,
	}).Wait()
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if status.HighestModSeq != 1 {
		t.Fatalf("HighestModSeq = %v, want 1 for a fresh mailbox", status.HighestModSeq)
	}

	data, err := client.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait()
	if err != nil {
		t.Fatalf("Select() = %v", err)
	}
	if data.HighestModSeq != 1 {
		t.Fatalf("SelectData.HighestModSeq = %v, want 1", data.HighestModSeq)
	}
}
