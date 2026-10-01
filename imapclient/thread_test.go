package imapclient_test

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const threadReplyRawMessage1 = `MIME-Version: 1.0
Message-Id: <reply1@example.com>
References: <191101702316132@example.com>

This is a reply!`

const threadReplyRawMessage2 = `MIME-Version: 1.0
Message-Id: <reply2@example.com>
References: <191101702316132@example.com> <reply1@example.com>

This is a reply to a reply!`

func appendThreadMessage(t *testing.T, client *imapclient.Client, raw string) {
	t.Helper()
	appendCmd := client.Append("INBOX", int64(len(raw)), nil)
	if _, err := appendCmd.Write([]byte(raw)); err != nil {
		t.Fatalf("AppendCommand.Write() = %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("AppendCommand.Close() = %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("AppendCommand.Wait() = %v", err)
	}
}

func TestThread(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapThreadReferences) {
		t.Skip("server doesn't support THREAD=REFERENCES")
	}

	appendThreadMessage(t, client, threadReplyRawMessage1)
	appendThreadMessage(t, client, threadReplyRawMessage2)

	options := imapclient.ThreadOptions{
		Algorithm:      imap.ThreadReferences,
		SearchCriteria: &imap.SearchCriteria{},
	}
	threads, err := client.Thread(&options).Wait()
	if err != nil {
		t.Fatalf("Thread().Wait() = %v", err)
	}

	want := []imapclient.ThreadData{{Chain: []uint32{1, 2, 3}}}
	if !reflect.DeepEqual(threads, want) {
		t.Errorf("Thread().Wait() = %v, want %v", threads, want)
	}
}

func TestUIDThread(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapThreadReferences) {
		t.Skip("server doesn't support THREAD=REFERENCES")
	}

	appendThreadMessage(t, client, threadReplyRawMessage1)
	appendThreadMessage(t, client, threadReplyRawMessage2)

	options := imapclient.ThreadOptions{
		Algorithm:      imap.ThreadReferences,
		SearchCriteria: &imap.SearchCriteria{},
	}
	threads, err := client.UIDThread(&options).Wait()
	if err != nil {
		t.Fatalf("UIDThread().Wait() = %v", err)
	}

	want := []imapclient.ThreadData{{Chain: []uint32{1, 2, 3}}}
	if !reflect.DeepEqual(threads, want) {
		t.Errorf("UIDThread().Wait() = %v, want %v", threads, want)
	}
}
