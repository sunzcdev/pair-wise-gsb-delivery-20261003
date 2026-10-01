package imapclient_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// sortTestMessages is a fixed set of messages exercising the SORT edge
// cases: RFC 2047 encoded subjects, a missing/invalid Date header, repeated
// dates and multiple addresses in address headers.
//
// The shared test pair always seeds one message (UID 1), so these messages
// are appended with UIDs 2..6 and sequence numbers 2..6. The seed has no
// Date, From-name or Subject fields relevant to sorting.
var sortTestMessages = []string{
	strings.Join([]string{
		"Message-Id: <m1@example.org>",
		"Date: Wed, 02 Feb 2022 10:00:00 +0000",
		"From: Alice <alice@a.example.org>",
		"To: Zoe <zoe@z.example.org>",
		"Subject: Hello",
		"",
		"first",
		"",
	}, "\r\n"),
	strings.Join([]string{
		"Message-Id: <m2@example.org>",
		"Date: Wed, 02 Feb 2022 10:00:00 +0000",
		"From: Bob <bob@b.example.org>",
		"To: Yann <yann@y.example.org>",
		// RFC 2047 encoded-word; decodes to "Re: Hello"
		"Subject: =?UTF-8?B?UmU6IEhlbGxv?=",
		"",
		"second message body",
		"",
	}, "\r\n"),
	strings.Join([]string{
		"Message-Id: <m3@example.org>",
		"Date: not-a-date",
		"From: Carol <carol@c.example.org>",
		"To: Xander <xander@x.example.org>",
		// decodes to "world"
		"Subject: =?UTF-8?Q?world?=",
		"",
		"third",
		"",
	}, "\r\n"),
	strings.Join([]string{
		"Message-Id: <m4@example.org>",
		"Date: Mon, 01 Feb 2021 08:30:00 +0000",
		// Multiple addresses in From; the first mailbox is used.
		"From: =?UTF-8?Q?D=C3=A9b?= <deb@d.example.org>, Erin <erin@e.example.org>",
		"To: Walt <walt@w.example.org>, Victor <victor@v.example.org>",
		"Subject: Fwd: Hello",
		"",
		"fourth message",
		"",
	}, "\r\n"),
	strings.Join([]string{
		"Message-Id: <m5@example.org>",
		// No Date header at all.
		"From: aaron@aa.example.org",
		"Cc: Chris <chris@cc.example.org>",
		"Subject: Apple",
		"",
		"five",
		"",
	}, "\r\n"),
}

func appendSortMessages(t *testing.T, client *imapclient.Client) {
	t.Helper()
	for i, raw := range sortTestMessages {
		options := &imap.AppendOptions{
			Time: time.Date(2020, time.January, i+1, 0, 0, 0, 0, time.UTC),
		}
		cmd := client.Append("INBOX", int64(len(raw)), options)
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
}

func newSortClient(t *testing.T) *imapclient.Client {
	t.Helper()
	client, _ := newClientServerPair(t, imap.ConnStateAuthenticated)
	t.Cleanup(func() { client.Close() })
	appendSortMessages(t, client)
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select().Wait() = %v", err)
	}
	return client
}

func TestSort(t *testing.T) {
	client := newSortClient(t)

	sort := func(criteria []imap.SortCriterion, search *imap.SearchCriteria) []uint32 {
		nums, err := client.Sort(&imapclient.SortOptions{
			SearchCriteria: search,
			SortCriteria:   criteria,
		}).Wait()
		if err != nil {
			t.Fatalf("Sort().Wait() = %v", err)
		}
		return nums
	}

	// "Apple"(uid 6) first; the two "Hello" base subjects (uids 2 and 3) are
	// equal and keep mailbox order; "Fwd: Hello"(uid 5) follows; "world"
	// (uid 4) is last. Uses i;ascii-casemap on base subjects.
	t.Run("subject", func(t *testing.T) {
		got := sort([]imap.SortCriterion{{Key: imap.SortKeySubject}}, nil)
		want := []uint32{1, 6, 2, 3, 5, 4}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SUBJECT = %v, want %v", got, want)
		}
	})

	t.Run("subject_reverse", func(t *testing.T) {
		got := sort([]imap.SortCriterion{{Key: imap.SortKeySubject, Reverse: true}}, nil)
		want := []uint32{4, 2, 3, 5, 6, 1}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("REVERSE SUBJECT = %v, want %v", got, want)
		}
	})

	// Messages without a valid Date sort first in mailbox order: uid 4 has a
	// malformed Date and uid 1 (the seed) has no Date. Then the remaining are
	// ordered by date; uids 2 and 3 share the same date and keep mailbox
	// order.
	t.Run("date", func(t *testing.T) {
		got := sort([]imap.SortCriterion{{Key: imap.SortKeyDate}}, nil)
		want := []uint32{4, 1, 6, 5, 2, 3}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("DATE = %v, want %v", got, want)
		}
	})

	// ARRIVAL uses the internal date set at APPEND time: mailbox order.
	t.Run("arrival", func(t *testing.T) {
		got := sort([]imap.SortCriterion{{Key: imap.SortKeyArrival}}, nil)
		want := []uint32{2, 3, 4, 5, 6, 1}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ARRIVAL = %v, want %v", got, want)
		}
	})

	// FROM uses the local-part of the first From mailbox.
	t.Run("from", func(t *testing.T) {
		got := sort([]imap.SortCriterion{{Key: imap.SortKeyFrom}}, nil)
		want := []uint32{1, 6, 2, 3, 4, 5}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("FROM = %v, want %v", got, want)
		}
	})

	// TO uses the local-part of the first To mailbox; uid 6 has no To field
	// and sorts first.
	t.Run("to", func(t *testing.T) {
		got := sort([]imap.SortCriterion{{Key: imap.SortKeyTo}}, nil)
		want := []uint32{1, 6, 5, 4, 3, 2}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("TO = %v, want %v", got, want)
		}
	})

	// SIZE sorts by RFC822.SIZE, smallest first.
	t.Run("size", func(t *testing.T) {
		got := sort([]imap.SortCriterion{{Key: imap.SortKeySize}}, nil)
		want := []uint32{6, 1, 4, 2, 3, 5}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SIZE = %v, want %v", got, want)
		}
	})

	// Search criteria restrict the sorted set. The non-ASCII value exercises
	// CHARSET UTF-8 handling and RFC 2047 encoded-word decoding.
	t.Run("search_criteria", func(t *testing.T) {
		criteria := &imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{
				Key:   "From",
				Value: "Déb",
			}},
		}
		got := sort([]imap.SortCriterion{{Key: imap.SortKeySubject}}, criteria)
		want := []uint32{5}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SORT with search criteria = %v, want %v", got, want)
		}
	})

	// Multiple criteria: DATE then ARRIVAL.
	t.Run("multi_criteria", func(t *testing.T) {
		got := sort([]imap.SortCriterion{
			{Key: imap.SortKeyDate},
			{Key: imap.SortKeyArrival, Reverse: true},
		}, nil)
		want := []uint32{4, 1, 6, 5, 3, 2}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("DATE + REVERSE ARRIVAL = %v, want %v", got, want)
		}
	})
}

func TestUIDSort(t *testing.T) {
	client := newSortClient(t)

	uids, err := client.UIDSort(&imapclient.SortOptions{
		SortCriteria: []imap.SortCriterion{{Key: imap.SortKeyDate}},
	}).Wait()
	if err != nil {
		t.Fatalf("UIDSort().Wait() = %v", err)
	}

	want := []uint32{4, 1, 6, 5, 2, 3}
	if !reflect.DeepEqual(uids, want) {
		t.Errorf("UID SORT DATE = %v, want %v", uids, want)
	}
}
