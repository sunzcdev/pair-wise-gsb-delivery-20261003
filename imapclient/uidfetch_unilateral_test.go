package imapclient_test

import (
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestClient_UnsolicitedUIDFetch(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	got := make(chan *imapclient.FetchMessageData, 1)
	existsCh := make(chan uint32, 1)

	go func() {
		// Greeting.
		serverConn.Write([]byte("* OK ready\r\n"))
		// Unsolicited non-selected mailbox FlagChange event.
		serverConn.Write([]byte("* UID FETCH (UID 42 FLAGS (\\Seen))\r\n"))
		time.Sleep(100 * time.Millisecond)
		// A second unsolicited event proves the read loop is still alive.
		serverConn.Write([]byte("* 7 EXISTS\r\n"))
	}()

	client := imapclient.New(clientConn, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Fetch: func(msg *imapclient.FetchMessageData) {
				got <- msg
			},
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					existsCh <- *data.NumMessages
				}
			},
		},
	})
	defer client.Close()

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	select {
	case msg := <-got:
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
		if uid != 42 {
			t.Fatalf("uid = %v, want 42", uid)
		}
		if len(flags) != 1 || flags[0] != imap.FlagSeen {
			t.Fatalf("flags = %v, want [\\Seen]", flags)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for unilateral UID FETCH")
	}

	select {
	case n := <-existsCh:
		if n != 7 {
			t.Fatalf("exists = %v, want 7", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read loop dead: no EXISTS after UID FETCH")
	}
}
