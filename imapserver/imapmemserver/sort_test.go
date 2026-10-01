package imapmemserver

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// newSortTestMailbox builds a mailbox with the given raw messages.
func newSortTestMailbox(t *testing.T, raws []string) (*User, *Mailbox) {
	t.Helper()
	user := NewUser("sort-user", "password")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	mbox, err := user.mailbox("INBOX")
	if err != nil {
		t.Fatalf("mailbox() = %v", err)
	}
	for _, raw := range raws {
		mbox.appendBytes([]byte(raw), &imap.AppendOptions{})
	}
	return user, mbox
}

func sortRawMessage(subject string) string {
	return "Date: Wed, 02 Feb 2022 10:00:00 +0000\r\n" +
		"From: sender@example.org\r\n" +
		"Subject: " + subject + "\r\n" +
		"\r\nbody\r\n"
}

func TestBaseSubject(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello", "Hello"},
		{"Re: Hello", "Hello"},
		{"re: Hello", "Hello"},
		{"RE: Hello", "Hello"},
		{"Re[2]: Hello", "Hello"},
		{"Fwd: Hello", "Hello"},
		{"Fwd[42]: Hello", "Hello"},
		{"  Re:   Hello  ", "Hello"},
		{"Re: Fwd: Hello", "Hello"},
		{"Reply: Hello", "Reply: Hello"}, // not a recognized prefix
		{"Reference: Hello", "Reference: Hello"},
		{"Re[broken: Hello", "Re[broken: Hello"},
	}
	for _, tc := range tests {
		if got := baseSubject(tc.input); got != tc.want {
			t.Errorf("baseSubject(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMailboxViewSort(t *testing.T) {
	raws := []string{
		sortRawMessage("zebra"),
		sortRawMessage("apple"),
		sortRawMessage("mango"),
	}
	_, mbox := newSortTestMailbox(t, raws)
	view := mbox.NewView()
	defer view.Close()

	data, err := view.Sort(imapserver.NumKindUID,
		[]imap.SortCriterion{{Key: imap.SortKeySubject}},
		&imap.SearchCriteria{})
	if err != nil {
		t.Fatalf("Sort() = %v", err)
	}

	wantUIDs := []uint32{2, 3, 1}
	if got := data.All; !reflect.DeepEqual(got, wantUIDs) {
		t.Errorf("UID SORT SUBJECT = %v, want %v", got, wantUIDs)
	}

	seqData, err := view.Sort(imapserver.NumKindSeq,
		[]imap.SortCriterion{{Key: imap.SortKeySubject}},
		&imap.SearchCriteria{})
	if err != nil {
		t.Fatalf("Sort(seq) = %v", err)
	}
	wantSeqNums := []uint32{2, 3, 1}
	if got := seqData.All; !reflect.DeepEqual(got, wantSeqNums) {
		t.Errorf("SORT SUBJECT = %v, want %v", got, wantSeqNums)
	}
}

// TestMailboxViewSortConcurrentExpunge verifies that sequence numbers and
// UIDs returned by SORT and UID SORT stay consistent when an EXPUNGE happens
// on another session (another connection) while sorting.
//
// The sorting session holds a snapshot of the mailbox: the mailbox content is
// read under the mailbox lock, but sequence numbers are translated through
// the session tracker so that they describe the sorting client's view.
func TestMailboxViewSortConcurrentExpunge(t *testing.T) {
	raws := []string{
		sortRawMessage("one"),
		sortRawMessage("two"),
		sortRawMessage("three"),
		sortRawMessage("four"),
		sortRawMessage("five"),
	}
	_, mbox := newSortTestMailbox(t, raws)

	sortingView := mbox.NewView()
	defer sortingView.Close()
	expungingView := mbox.NewView()
	defer expungingView.Close()

	// Mark the second message (current server sequence number 2, UID 2) as
	// deleted, then expunge it through the other session.
	mbox.mutex.Lock()
	mbox.l[1].flags[canonicalFlag(imap.FlagDeleted)] = struct{}{}
	mbox.mutex.Unlock()

	if err := mbox.Expunge(&imapserver.ExpungeWriter{}, nil); err != nil {
		t.Fatalf("Expunge() = %v", err)
	}

	// The sorting session has not yet received the EXPUNGE response. Its
	// tracker still describes a 5-message mailbox, but the mailbox only has
	// 4 left. Encoding the remaining server-side sequence numbers through the
	// sorting view must skip the removed one without shifting unrelated
	// messages.
	data, err := sortingView.Sort(imapserver.NumKindUID,
		[]imap.SortCriterion{{Key: imap.SortKeySubject}},
		&imap.SearchCriteria{})
	if err != nil {
		t.Fatalf("UID Sort() = %v", err)
	}

	// UIDs are stable: the expunged UID 2 must not appear.
	wantUIDs := []uint32{5, 4, 1, 3}
	if got := data.All; !reflect.DeepEqual(got, wantUIDs) {
		t.Errorf("UID SORT with concurrent expunge = %v, want %v", got, wantUIDs)
	}

	seqData, err := sortingView.Sort(imapserver.NumKindSeq,
		[]imap.SortCriterion{{Key: imap.SortKeySubject}},
		&imap.SearchCriteria{})
	if err != nil {
		t.Fatalf("seq Sort() = %v", err)
	}

	// The sorting client still sees 5 messages: UID 2 is the expunged one
	// and must be omitted, while UID 1 -> seq 1, UID 3 -> seq 3,
	// UID 4 -> seq 4, UID 5 -> seq 5.
	// Subject order: five(5), four(4), one(1), three(3).
	wantSeqNums := []uint32{5, 4, 1, 3}
	if got := seqData.All; !reflect.DeepEqual(got, wantSeqNums) {
		t.Errorf("SORT with concurrent expunge = %v, want %v", got, wantSeqNums)
	}
}
