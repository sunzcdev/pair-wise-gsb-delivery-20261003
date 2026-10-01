package imapmemserver

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

func TestBaseSubject(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"Hello", "Hello"},
		{"Re: Hello", "Hello"},
		{"RE:  Fwd: Hello", "Hello"},
		{"Fw: [list] Re: Hello (fwd)", "Hello"},
		{"[fwd: Hello]", "Hello"},
		{"[list] Hello", "Hello"},
		{"[list][other] Hello", "Hello"},
		{"Re[2]: Hello", "Hello"},
		{"fwd:Hello", "Hello"},
		{"Hello (fwd)", "Hello"},
		{"really: Hello", "really: Hello"},
		{"[unclosed Hello", "[unclosed Hello"},
		{"Re:", ""},
		{"", ""},
	} {
		if got := baseSubject(test.in); got != test.want {
			t.Errorf("baseSubject(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

var sortTestTimes = []time.Time{
	time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC),
	time.Date(2024, 6, 1, 11, 0, 0, 0, time.UTC),
	time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
	time.Date(2024, 6, 1, 13, 0, 0, 0, time.UTC),
}

// sortTestMessages contains messages with RFC 2047 encoded subjects,
// duplicate sent dates, a missing Date header field, an invalid Date header
// field and a multi-address From header field.
var sortTestMessages = []string{
	// Subject: Café, sent 2024-01-01, from "Bob"
	"From: Bob <bob@example.com>\r\n" +
		"Subject: Re: =?UTF-8?Q?Caf=C3=A9?=\r\n" +
		"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n" +
		"\r\n" + strings.Repeat("a", 10),
	// Subject: Café, sent 2024-01-01 (duplicate), from "alice@example.com"
	"From: alice@example.com\r\n" +
		"Subject: =?UTF-8?Q?Caf=C3=A9?=\r\n" +
		"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n" +
		"\r\n" + strings.Repeat("b", 110),
	// Subject: Apple, no Date header field, multi-address From
	"From: Carol <carol@example.com>, extra@example.com\r\n" +
		"Subject: Fwd: Apple\r\n" +
		"\r\n" + strings.Repeat("c", 210),
	// Subject: Zebra, invalid Date header field, no From header field
	"Subject: Zebra\r\n" +
		"Date: not-a-date\r\n" +
		"\r\n" + strings.Repeat("d", 310),
}

func newSortTestMailbox(t *testing.T) *Mailbox {
	t.Helper()
	mbox := NewMailbox("INBOX", 1)
	for i, raw := range sortTestMessages {
		mbox.appendBytes([]byte(raw), &imap.AppendOptions{Time: sortTestTimes[i]})
	}
	return mbox
}

func TestSort(t *testing.T) {
	for _, test := range []struct {
		name     string
		criteria []imap.SortCriterion
		search   *imap.SearchCriteria
		want     []uint32
	}{
		{
			name:     "subject",
			criteria: []imap.SortCriterion{{Key: imap.SortKeySubject}},
			want:     []uint32{3, 1, 2, 4}, // "Apple" < "Café" (tie, stable) < "Zebra"
		},
		{
			name:     "reverse subject",
			criteria: []imap.SortCriterion{{Key: imap.SortKeySubject, Reverse: true}},
			want:     []uint32{4, 1, 2, 3}, // ties keep their mailbox order
		},
		{
			name:     "date",
			criteria: []imap.SortCriterion{{Key: imap.SortKeyDate}},
			// duplicate sent dates keep their mailbox order, missing and
			// invalid Date header fields fall back to the internal date
			want: []uint32{1, 2, 3, 4},
		},
		{
			name:     "reverse date",
			criteria: []imap.SortCriterion{{Key: imap.SortKeyDate, Reverse: true}},
			want:     []uint32{4, 3, 1, 2},
		},
		{
			name:     "from",
			criteria: []imap.SortCriterion{{Key: imap.SortKeyFrom}},
			// missing From sorts first, then address without display name,
			// then display names; only the first address is considered
			want: []uint32{4, 2, 1, 3},
		},
		{
			name:     "arrival",
			criteria: []imap.SortCriterion{{Key: imap.SortKeyArrival}},
			want:     []uint32{1, 2, 3, 4},
		},
		{
			name:     "reverse arrival",
			criteria: []imap.SortCriterion{{Key: imap.SortKeyArrival, Reverse: true}},
			want:     []uint32{4, 3, 2, 1},
		},
		{
			name:     "size",
			criteria: []imap.SortCriterion{{Key: imap.SortKeySize}},
			want:     []uint32{1, 2, 3, 4},
		},
		{
			name: "subject then date",
			criteria: []imap.SortCriterion{
				{Key: imap.SortKeySubject},
				{Key: imap.SortKeyDate, Reverse: true},
			},
			want: []uint32{3, 1, 2, 4},
		},
		{
			name:     "search criteria",
			criteria: []imap.SortCriterion{{Key: imap.SortKeySubject}},
			search: &imap.SearchCriteria{
				Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: "example.com"}},
			},
			want: []uint32{3, 1, 2}, // message 4 has no From header field
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mbox := newSortTestMailbox(t)
			view := mbox.NewView()
			defer view.Close()

			options := &imap.SortOptions{
				SearchCriteria: test.search,
				SortCriteria:   test.criteria,
			}

			seqData, err := view.Sort(imapserver.NumKindSeq, options)
			if err != nil {
				t.Fatalf("Sort() = %v", err)
			}
			if !reflect.DeepEqual(seqData.Nums, test.want) {
				t.Errorf("Sort(seq).Nums = %v, want %v", seqData.Nums, test.want)
			}

			uidData, err := view.Sort(imapserver.NumKindUID, options)
			if err != nil {
				t.Fatalf("Sort() = %v", err)
			}
			if !reflect.DeepEqual(uidData.Nums, test.want) {
				t.Errorf("Sort(uid).Nums = %v, want %v", uidData.Nums, test.want)
			}
		})
	}
}

// TestSortExpunge checks that sequence numbers returned by SORT are converted
// to the view of a session which hasn't been notified of an expunge yet.
func TestSortExpunge(t *testing.T) {
	mbox := newSortTestMailbox(t)

	view1 := mbox.NewView()
	defer view1.Close()
	view2 := mbox.NewView()
	defer view2.Close()

	// Expunge the second message from another session: view1 doesn't know
	// about the expunge yet
	mbox.mutex.Lock()
	mbox.l[1].flags[canonicalFlag(imap.FlagDeleted)] = struct{}{}
	mbox.mutex.Unlock()
	uids := imap.UIDSetNum(2)
	if err := view2.Expunge(nil, &uids); err != nil {
		t.Fatalf("Expunge() = %v", err)
	}

	options := &imap.SortOptions{
		SortCriteria: []imap.SortCriterion{{Key: imap.SortKeyArrival}},
	}

	// From view1's point-of-view, the mailbox still has 4 messages and the
	// remaining ones have sequence numbers 1, 3 and 4
	seqData, err := view1.Sort(imapserver.NumKindSeq, options)
	if err != nil {
		t.Fatalf("Sort() = %v", err)
	}
	if want := []uint32{1, 3, 4}; !reflect.DeepEqual(seqData.Nums, want) {
		t.Errorf("Sort(seq).Nums = %v, want %v", seqData.Nums, want)
	}

	uidData, err := view1.Sort(imapserver.NumKindUID, options)
	if err != nil {
		t.Fatalf("Sort() = %v", err)
	}
	if want := []uint32{1, 3, 4}; !reflect.DeepEqual(uidData.Nums, want) {
		t.Errorf("Sort(uid).Nums = %v, want %v", uidData.Nums, want)
	}

	// Sequence numbers in search criteria are also interpreted from view1's
	// point-of-view
	filteredData, err := view1.Sort(imapserver.NumKindUID, &imap.SortOptions{
		SearchCriteria: &imap.SearchCriteria{SeqNum: []imap.SeqSet{imap.SeqSetNum(4)}},
		SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
	})
	if err != nil {
		t.Fatalf("Sort() = %v", err)
	}
	if want := []uint32{4}; !reflect.DeepEqual(filteredData.Nums, want) {
		t.Errorf("Sort(uid, seq 4).Nums = %v, want %v", filteredData.Nums, want)
	}
}

// TestSortConcurrentExpunge runs SORT commands while another session
// concurrently expunges messages.
func TestSortConcurrentExpunge(t *testing.T) {
	mbox := NewMailbox("INBOX", 1)
	const n = 64
	for i := 0; i < n; i++ {
		raw := fmt.Sprintf("Subject: message %d\r\n\r\nbody", i)
		mbox.appendBytes([]byte(raw), &imap.AppendOptions{Time: sortTestTimes[0].Add(time.Duration(i) * time.Minute)})
	}

	view := mbox.NewView()
	defer view.Close()

	done := make(chan error, 1)
	go func() {
		var err error
		defer func() { done <- err }()
		for i := 0; i < 100; i++ {
			for _, numKind := range []imapserver.NumKind{imapserver.NumKindSeq, imapserver.NumKindUID} {
				var data *imap.SortData
				data, err = view.Sort(numKind, &imap.SortOptions{
					SortCriteria: []imap.SortCriterion{{Key: imap.SortKeyArrival}},
				})
				if err != nil {
					return
				}
				seen := make(map[uint32]struct{})
				for _, num := range data.Nums {
					if num == 0 || num > n {
						err = fmt.Errorf("Sort() returned out-of-range number %d", num)
						return
					}
					if _, ok := seen[num]; ok {
						err = fmt.Errorf("Sort() returned duplicate number %d", num)
						return
					}
					seen[num] = struct{}{}
				}
			}
		}
	}()

	for uid := imap.UID(1); uid <= n; uid += 2 {
		mbox.mutex.Lock()
		for _, msg := range mbox.l {
			if msg.uid == uid {
				msg.flags[canonicalFlag(imap.FlagDeleted)] = struct{}{}
			}
		}
		mbox.mutex.Unlock()
		uids := imap.UIDSetNum(uid)
		if err := mbox.Expunge(nil, &uids); err != nil {
			t.Fatalf("Expunge() = %v", err)
		}
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
