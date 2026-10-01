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

// testThreadMessage describes a message in the fixed threading test set.
type testThreadMessage struct {
	id        string // Message-ID local part, empty means no Message-ID
	refs      []string
	inReplyTo string
	subject   string
	day       int // sent date is 2026-01-<day> 12:00 UTC
}

// threadTestMessages is a fixed set of messages exercising tricky threading
// cases:
//
//   - a -> b -> c: a regular conversation
//   - d -> e: d references missing ancestors missing1 and missing2, e uses
//     In-Reply-To instead of References
//   - x <-> y: circular references
//   - s: self-reference
//   - dup: two messages sharing the same Message-ID
//   - u: same subject as a, but no reference to it (must not be grouped)
var threadTestMessages = []testThreadMessage{
	{id: "a", subject: "hello", day: 1},
	{id: "b", refs: []string{"a"}, subject: "skipme", day: 2},
	{id: "c", refs: []string{"a", "b"}, subject: "c-sub", day: 3},
	{id: "d", refs: []string{"missing1", "missing2"}, subject: "d-sub", day: 4},
	{id: "e", inReplyTo: "d", subject: "e-sub", day: 5},
	{id: "x", refs: []string{"y"}, subject: "x-sub", day: 6},
	{id: "y", refs: []string{"x"}, subject: "y-sub", day: 7},
	{id: "s", refs: []string{"s"}, subject: "s-sub", day: 8},
	{id: "dup", subject: "dup-sub", day: 9},
	{id: "dup", subject: "dup-sub-2", day: 10},
	{id: "u", subject: "hello", day: 11},
}

// threadTestExpected is the expected thread tree for threadTestMessages,
// expressed with Message-IDs.
var threadTestExpected = []*idNode{
	{id: "a", kids: []*idNode{
		{id: "b", kids: []*idNode{
			{id: "c"},
		}},
	}},
	{id: "d", kids: []*idNode{
		{id: "e"},
	}},
	{id: "y", kids: []*idNode{
		{id: "x"},
	}},
	{id: "s"},
	{id: "dup"},
	{id: "u"},
}

// threadTestFilteredExpected is the expected thread tree when the message
// with subject "skipme" (b) is filtered out by the search criteria: c is
// spliced into b's position under a.
var threadTestFilteredExpected = []*idNode{
	{id: "a", kids: []*idNode{
		{id: "c"},
	}},
	{id: "d", kids: []*idNode{
		{id: "e"},
	}},
	{id: "y", kids: []*idNode{
		{id: "x"},
	}},
	{id: "s"},
	{id: "dup"},
	{id: "u"},
}

func (m *testThreadMessage) raw() []byte {
	var sb strings.Builder
	date := time.Date(2026, 1, m.day, 12, 0, 0, 0, time.UTC)
	fmt.Fprintf(&sb, "Date: %s\r\n", date.Format(time.RFC1123Z))
	fmt.Fprintf(&sb, "Subject: %s\r\n", m.subject)
	if m.id != "" {
		fmt.Fprintf(&sb, "Message-ID: <%s@test>\r\n", m.id)
	}
	if len(m.refs) > 0 {
		sb.WriteString("References:")
		for _, ref := range m.refs {
			fmt.Fprintf(&sb, " <%s@test>", ref)
		}
		sb.WriteString("\r\n")
	}
	if m.inReplyTo != "" {
		fmt.Fprintf(&sb, "In-Reply-To: <%s@test>\r\n", m.inReplyTo)
	}
	sb.WriteString("\r\nbody\r\n")
	return []byte(sb.String())
}

// idNode is a thread tree expressed with Message-IDs, so that trees can be
// compared regardless of the order messages were appended in.
type idNode struct {
	id   string
	kids []*idNode
}

func appendThreadTestMessages(msgs []testThreadMessage) (*MailboxView, map[uint32]string, map[uint32]string) {
	mbox := NewMailbox("INBOX", 1)
	seqToID := make(map[uint32]string)
	uidToID := make(map[uint32]string)
	for i := range msgs {
		m := &msgs[i]
		date := time.Date(2026, 1, m.day, 12, 0, 0, 0, time.UTC)
		appendData := mbox.appendBytes(m.raw(), &imap.AppendOptions{Time: date})
		seqToID[uint32(i)+1] = m.id
		uidToID[uint32(appendData.UID)] = m.id
	}
	return mbox.NewView(), seqToID, uidToID
}

func threadToIDTree(t *testing.T, view *MailboxView, numKind imapserver.NumKind, criteria *imap.SearchCriteria, idOf map[uint32]string) []*idNode {
	t.Helper()
	threads, err := view.Thread(numKind, imap.ThreadReferences, criteria)
	if err != nil {
		t.Fatalf("Thread() = %v", err)
	}
	var out []*idNode
	for i := range threads {
		out = append(out, convertThreadData(&threads[i], idOf))
	}
	return out
}

func convertThreadData(data *imap.ThreadData, idOf map[uint32]string) *idNode {
	var root, cur *idNode
	for _, num := range data.Chain {
		n := &idNode{id: idOf[num]}
		if cur == nil {
			root = n
		} else {
			cur.kids = append(cur.kids, n)
		}
		cur = n
	}
	for i := range data.SubThreads {
		cur.kids = append(cur.kids, convertThreadData(&data.SubThreads[i], idOf))
	}
	return root
}

func TestThreadReferences(t *testing.T) {
	view, seqToID, uidToID := appendThreadTestMessages(threadTestMessages)
	defer view.Close()

	criteria := &imap.SearchCriteria{}

	got := threadToIDTree(t, view, imapserver.NumKindSeq, criteria, seqToID)
	if !reflect.DeepEqual(got, threadTestExpected) {
		t.Errorf("Thread(seq) = %+v, want %+v", got, threadTestExpected)
	}

	got = threadToIDTree(t, view, imapserver.NumKindUID, criteria, uidToID)
	if !reflect.DeepEqual(got, threadTestExpected) {
		t.Errorf("Thread(uid) = %+v, want %+v", got, threadTestExpected)
	}
}

func TestThreadReferencesShuffled(t *testing.T) {
	permutations := [][]int{
		{10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0}, // reversed
		{3, 7, 0, 10, 5, 1, 8, 2, 9, 4, 6},
	}
	for _, perm := range permutations {
		msgs := make([]testThreadMessage, len(threadTestMessages))
		for i, j := range perm {
			msgs[i] = threadTestMessages[j]
		}
		view, seqToID, uidToID := appendThreadTestMessages(msgs)

		criteria := &imap.SearchCriteria{}
		got := threadToIDTree(t, view, imapserver.NumKindSeq, criteria, seqToID)
		if !reflect.DeepEqual(got, threadTestExpected) {
			t.Errorf("Thread(seq) with permutation %v = %+v, want %+v", perm, got, threadTestExpected)
		}
		got = threadToIDTree(t, view, imapserver.NumKindUID, criteria, uidToID)
		if !reflect.DeepEqual(got, threadTestExpected) {
			t.Errorf("Thread(uid) with permutation %v = %+v, want %+v", perm, got, threadTestExpected)
		}

		view.Close()
	}
}

func TestThreadReferencesSearchFilter(t *testing.T) {
	view, seqToID, _ := appendThreadTestMessages(threadTestMessages)
	defer view.Close()

	// Filter out the message with subject "skipme" (b): its child c must be
	// spliced into its position under a
	criteria := &imap.SearchCriteria{
		Not: []imap.SearchCriteria{{
			Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "skipme"}},
		}},
	}
	got := threadToIDTree(t, view, imapserver.NumKindSeq, criteria, seqToID)
	if !reflect.DeepEqual(got, threadTestFilteredExpected) {
		t.Errorf("Thread() with filter = %+v, want %+v", got, threadTestFilteredExpected)
	}
}

func TestThreadReferencesUnsupportedAlgorithm(t *testing.T) {
	view, _, _ := appendThreadTestMessages(threadTestMessages)
	defer view.Close()

	_, err := view.Thread(imapserver.NumKindSeq, imap.ThreadOrderedSubject, &imap.SearchCriteria{})
	imapErr, ok := err.(*imap.Error)
	if !ok {
		t.Fatalf("Thread(ORDEREDSUBJECT) error = %v, want *imap.Error", err)
	}
	if imapErr.Code != imap.ResponseCodeCannot {
		t.Errorf("Thread(ORDEREDSUBJECT) error code = %q, want CANNOT", imapErr.Code)
	}
}
