package imapmemserver

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/mail"
)

// threadTestMessage is a message in the fixed threading test set.
type threadTestMessage struct {
	id   string   // Message-ID
	refs []string // References
	irt  []string // In-Reply-To
	day  int      // sent date: 2026-01-<day>
}

// threadTestMessages is a fixed set of messages exercising missing
// ancestors, duplicate Message-IDs, self-references, reference cycles and
// In-Reply-To-only relationships.
var threadTestMessages = []threadTestMessage{
	{id: "root@example.com", day: 1},
	{id: "r1@example.com", refs: []string{"root@example.com"}, day: 2},
	{id: "r2@example.com", refs: []string{"root@example.com", "r1@example.com"}, day: 3},
	{id: "r3@example.com", refs: []string{"root@example.com"}, day: 4},
	{id: "deep@example.com", refs: []string{"root@example.com", "r1@example.com", "r2@example.com"}, day: 5},
	{id: "irt@example.com", irt: []string{"r3@example.com"}, day: 6},
	// Missing ancestor: missing@example.com is not in the mailbox
	{id: "orphan1@example.com", refs: []string{"missing@example.com"}, day: 7},
	{id: "orphan2@example.com", refs: []string{"missing@example.com"}, day: 8},
	// Reference cycle
	{id: "cycA@example.com", refs: []string{"cycB@example.com"}, day: 9},
	{id: "cycB@example.com", refs: []string{"cycA@example.com"}, day: 10},
	// Self-reference
	{id: "self@example.com", refs: []string{"self@example.com"}, day: 11},
	// Duplicate Message-ID
	{id: "dup@example.com", day: 12},
	{id: "dup@example.com", day: 13},
}

// idThread is a thread tree keyed by Message-ID, used to compare results
// independently from sequence numbers and UIDs.
type idThread struct {
	chain []string
	subs  []idThread
}

// threadTestExpected is the expected REFERENCES thread tree for
// threadTestMessages.
var threadTestExpected = []idThread{
	{chain: []string{"root@example.com"}, subs: []idThread{
		{chain: []string{"r1@example.com", "r2@example.com", "deep@example.com"}},
		{chain: []string{"r3@example.com", "irt@example.com"}},
	}},
	{chain: []string{"orphan1@example.com"}},
	{chain: []string{"orphan2@example.com"}},
	{chain: []string{"cycB@example.com", "cycA@example.com"}},
	{chain: []string{"self@example.com"}},
	{chain: []string{"dup@example.com"}},
}

func (m *threadTestMessage) raw() string {
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Message-ID: <%s>\r\n", m.id)
	if len(m.refs) > 0 {
		b.WriteString("References:")
		for _, ref := range m.refs {
			fmt.Fprintf(&b, " <%s>", ref)
		}
		b.WriteString("\r\n")
	}
	if len(m.irt) > 0 {
		b.WriteString("In-Reply-To:")
		for _, irt := range m.irt {
			fmt.Fprintf(&b, " <%s>", irt)
		}
		b.WriteString("\r\n")
	}
	date := time.Date(2026, 1, m.day, 12, 0, 0, 0, time.UTC)
	fmt.Fprintf(&b, "Date: %s\r\n", date.Format(time.RFC1123Z))
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\nbody\r\n")
	return b.String()
}

// newThreadTestMailboxView creates a mailbox containing threadTestMessages
// inserted in the given order.
func newThreadTestMailboxView(t *testing.T, order []int) *MailboxView {
	t.Helper()

	user := NewUser("username", "password")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	mbox, err := user.mailbox("INBOX")
	if err != nil {
		t.Fatalf("mailbox() = %v", err)
	}
	for _, i := range order {
		m := threadTestMessages[i]
		date := time.Date(2026, 1, m.day, 12, 0, 0, 0, time.UTC)
		mbox.appendLiteral(strings.NewReader(m.raw()), &imap.AppendOptions{Time: date})
	}
	return mbox.NewView()
}

func orderedIndexes() []int {
	order := make([]int, len(threadTestMessages))
	for i := range order {
		order[i] = i
	}
	return order
}

func shuffledIndexes() []int {
	// Fixed permutation: out-of-order import of the same conversation
	return []int{9, 4, 12, 0, 7, 2, 11, 5, 8, 1, 10, 3, 6}
}

// messageIDs returns a map from UID to Message-ID for all messages in the
// mailbox.
func messageIDs(view *MailboxView) map[imap.UID]string {
	ids := make(map[imap.UID]string)
	for _, msg := range view.Mailbox.l {
		header := mail.Header{msg.reader().Header}
		id, _ := header.MessageID()
		ids[msg.uid] = id
	}
	return ids
}

func toIDThreads(threads []imap.ThreadData, ids map[imap.UID]string) []idThread {
	out := make([]idThread, 0, len(threads))
	for _, t := range threads {
		out = append(out, toIDThread(t, ids))
	}
	return out
}

func toIDThread(t imap.ThreadData, ids map[imap.UID]string) idThread {
	var out idThread
	for _, num := range t.Chain {
		out.chain = append(out.chain, ids[imap.UID(num)])
	}
	for _, sub := range t.SubThreads {
		out.subs = append(out.subs, toIDThread(sub, ids))
	}
	return out
}

func TestThreadReferences(t *testing.T) {
	view := newThreadTestMailboxView(t, orderedIndexes())
	defer view.Close()

	threads, err := view.Thread(imapserver.NumKindUID, imap.ThreadReferences, &imap.SearchCriteria{})
	if err != nil {
		t.Fatalf("Thread() = %v", err)
	}

	got := toIDThreads(threads, messageIDs(view))
	if !reflect.DeepEqual(got, threadTestExpected) {
		t.Errorf("Thread() = %+v, want %+v", got, threadTestExpected)
	}
}

func TestThreadReferencesShuffled(t *testing.T) {
	orderedView := newThreadTestMailboxView(t, orderedIndexes())
	defer orderedView.Close()
	shuffledView := newThreadTestMailboxView(t, shuffledIndexes())
	defer shuffledView.Close()

	criteria := &imap.SearchCriteria{}
	orderedThreads, err := orderedView.Thread(imapserver.NumKindUID, imap.ThreadReferences, criteria)
	if err != nil {
		t.Fatalf("Thread() = %v", err)
	}
	shuffledThreads, err := shuffledView.Thread(imapserver.NumKindUID, imap.ThreadReferences, criteria)
	if err != nil {
		t.Fatalf("Thread() = %v", err)
	}

	ordered := toIDThreads(orderedThreads, messageIDs(orderedView))
	shuffled := toIDThreads(shuffledThreads, messageIDs(shuffledView))
	if !reflect.DeepEqual(ordered, shuffled) {
		t.Errorf("shuffled insertion changed thread tree:\nordered  = %+v\nshuffled = %+v", ordered, shuffled)
	}
}

func TestThreadReferencesSeqNums(t *testing.T) {
	view := newThreadTestMailboxView(t, orderedIndexes())
	defer view.Close()

	threads, err := view.Thread(imapserver.NumKindSeq, imap.ThreadReferences, &imap.SearchCriteria{})
	if err != nil {
		t.Fatalf("Thread() = %v", err)
	}

	// Messages were inserted in fixture order, so sequence numbers match
	// positions in threadTestMessages
	want := []imap.ThreadData{
		{Chain: []uint32{1}, SubThreads: []imap.ThreadData{
			{Chain: []uint32{2, 3, 5}},
			{Chain: []uint32{4, 6}},
		}},
		{Chain: []uint32{7}},
		{Chain: []uint32{8}},
		{Chain: []uint32{10, 9}},
		{Chain: []uint32{11}},
		{Chain: []uint32{12}},
	}
	if !reflect.DeepEqual(threads, want) {
		t.Errorf("Thread() = %+v, want %+v", threads, want)
	}
}

func TestThreadReferencesSearchCriteria(t *testing.T) {
	view := newThreadTestMailboxView(t, orderedIndexes())
	defer view.Close()

	// Filter out r1@example.com: its descendants must be re-attached to
	// the nearest surviving ancestor
	criteria := &imap.SearchCriteria{
		Not: []imap.SearchCriteria{{
			Header: []imap.SearchCriteriaHeaderField{{
				Key:   "Message-ID",
				Value: "r1@example.com",
			}},
		}},
	}
	threads, err := view.Thread(imapserver.NumKindSeq, imap.ThreadReferences, criteria)
	if err != nil {
		t.Fatalf("Thread() = %v", err)
	}

	want := []imap.ThreadData{
		{Chain: []uint32{1}, SubThreads: []imap.ThreadData{
			{Chain: []uint32{3, 5}},
			{Chain: []uint32{4, 6}},
		}},
		{Chain: []uint32{7}},
		{Chain: []uint32{8}},
		{Chain: []uint32{10, 9}},
		{Chain: []uint32{11}},
		{Chain: []uint32{12}},
	}
	if !reflect.DeepEqual(threads, want) {
		t.Errorf("Thread() = %+v, want %+v", threads, want)
	}
}

func TestThreadReferencesUnsupportedAlgorithm(t *testing.T) {
	view := newThreadTestMailboxView(t, orderedIndexes())
	defer view.Close()

	_, err := view.Thread(imapserver.NumKindUID, imap.ThreadOrderedSubject, &imap.SearchCriteria{})
	if err == nil {
		t.Fatal("Thread(ORDEREDSUBJECT) succeeded, want an error")
	}
}
