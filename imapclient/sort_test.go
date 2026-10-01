package imapclient_test

import (
	"bufio"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func appendSortTestMessage(t *testing.T, client *imapclient.Client, raw string, internalDate time.Time) {
	t.Helper()
	cmd := client.Append("INBOX", int64(len(raw)), &imap.AppendOptions{Time: internalDate})
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatalf("AppendCommand.Write() = %v", err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("AppendCommand.Close() = %v", err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("AppendCommand.Wait() = %v", err)
	}
}

func TestSort(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapSort) {
		t.Fatal("server doesn't advertise SORT")
	}

	// The INBOX already contains simpleRawMessage, which has no Subject
	// header field
	appendSortTestMessage(t, client, "Subject: Banana\r\n\r\nbanana", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	appendSortTestMessage(t, client, "Subject: Apple\r\n\r\napple", time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC))

	for _, test := range []struct {
		name     string
		criteria []imapclient.SortCriterion
		search   *imap.SearchCriteria
		want     []uint32
	}{
		{
			name:     "subject",
			criteria: []imapclient.SortCriterion{{Key: imapclient.SortKeySubject}},
			want:     []uint32{1, 3, 2}, // "" < "Apple" < "Banana"
		},
		{
			name:     "reverse subject",
			criteria: []imapclient.SortCriterion{{Key: imapclient.SortKeySubject, Reverse: true}},
			want:     []uint32{2, 3, 1},
		},
		{
			name:     "search criteria",
			criteria: []imapclient.SortCriterion{{Key: imapclient.SortKeySubject}},
			search: &imap.SearchCriteria{
				Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "Apple"}},
			},
			want: []uint32{3},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := &imapclient.SortOptions{
				SearchCriteria: test.search,
				SortCriteria:   test.criteria,
			}
			if options.SearchCriteria == nil {
				options.SearchCriteria = &imap.SearchCriteria{}
			}

			nums, err := client.Sort(options).Wait()
			if err != nil {
				t.Fatalf("Sort().Wait() = %v", err)
			}
			if !reflect.DeepEqual(nums, test.want) {
				t.Errorf("Sort().Wait() = %v, want %v", nums, test.want)
			}

			uids, err := client.UIDSort(options).Wait()
			if err != nil {
				t.Fatalf("UIDSort().Wait() = %v", err)
			}
			if !reflect.DeepEqual(uids, test.want) {
				t.Errorf("UIDSort().Wait() = %v, want %v", uids, test.want)
			}
		})
	}
}

func TestSortBadCharset(t *testing.T) {
	conn, server := newMemClientServerPair(t)
	defer server.Close()
	defer conn.Close()

	r := bufio.NewReader(conn)
	readLine := func() string {
		t.Helper()
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString() = %v", err)
		}
		return strings.TrimRight(l, "\r\n")
	}
	writeCmd := func(format string, args ...interface{}) {
		t.Helper()
		if _, err := fmt.Fprintf(conn, format+"\r\n", args...); err != nil {
			t.Fatalf("Fprintf() = %v", err)
		}
	}

	readLine() // greeting

	writeCmd("a LOGIN %q %q", testUsername, testPassword)
	if l := readLine(); !strings.HasPrefix(l, "a OK") {
		t.Fatalf("LOGIN = %q", l)
	}

	writeCmd("b SELECT INBOX")
	for {
		if l := readLine(); strings.HasPrefix(l, "b ") {
			if !strings.HasPrefix(l, "b OK") {
				t.Fatalf("SELECT = %q", l)
			}
			break
		}
	}

	writeCmd("c SORT (DATE) X-UNKNOWN ALL")
	if l := readLine(); !strings.HasPrefix(l, "c NO") || !strings.Contains(l, "BADCHARSET") {
		t.Fatalf("SORT with unknown charset = %q, want NO [BADCHARSET]", l)
	}

	writeCmd("d SORT (DATE) UTF-8 ALL")
	if l := readLine(); !strings.HasPrefix(l, "* SORT") {
		t.Fatalf("SORT = %q, want untagged SORT response", l)
	}
	if l := readLine(); !strings.HasPrefix(l, "d OK") {
		t.Fatalf("SORT = %q", l)
	}

	writeCmd("e UID SORT (REVERSE DATE) US-ASCII ALL")
	if l := readLine(); !strings.HasPrefix(l, "* SORT") {
		t.Fatalf("UID SORT = %q, want untagged SORT response", l)
	}
	if l := readLine(); !strings.HasPrefix(l, "e OK") {
		t.Fatalf("UID SORT = %q", l)
	}
}
