package imapmemserver

import (
	"bufio"
	"bytes"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// Sort returns the messages matching the search criteria, in the order
// defined by the sort criteria.
//
// It implements imapserver.SessionSort.
func (mbox *MailboxView) Sort(numKind imapserver.NumKind, sortCriteria []imap.SortCriterion, criteria *imap.SearchCriteria) (*imap.SortData, error) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	mbox.staticSearchCriteria(criteria)

	type entry struct {
		index  int
		seqNum uint32 // sequence number from the client point-of-view
		msg    *message
		fields sortFields
	}

	var entries []entry
	for i, msg := range mbox.l {
		seqNum := mbox.tracker.EncodeSeqNum(uint32(i) + 1)

		if !msg.search(seqNum, criteria) {
			continue
		}

		entries = append(entries, entry{
			index:  i,
			seqNum: seqNum,
			msg:    msg,
		})
	}

	// Lazily parse only the headers required by the sort keys: the message
	// bodies are never read.
	needHeader := false
	for _, criterion := range sortCriteria {
		switch criterion.Key {
		case imap.SortKeyCc, imap.SortKeyDate, imap.SortKeyFrom, imap.SortKeySubject, imap.SortKeyTo:
			needHeader = true
		}
	}

	for i := range entries {
		entries[i].fields = entries[i].msg.sortFields(needHeader)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		for _, criterion := range sortCriteria {
			cmp := compareSortField(entries[i].fields, entries[j].fields, criterion.Key)
			if cmp == 0 {
				continue
			}
			if criterion.Reverse {
				cmp = -cmp
			}
			return cmp < 0
		}
		// Stable tie-break: messages with equal sort values stay in mailbox
		// order, i.e. ascending sequence number.
		return entries[i].index < entries[j].index
	})

	data := imap.SortData{}
	for _, entry := range entries {
		switch numKind {
		case imapserver.NumKindSeq:
			if entry.seqNum == 0 {
				continue
			}
			data.All = append(data.All, entry.seqNum)
		case imapserver.NumKindUID:
			data.All = append(data.All, uint32(entry.msg.uid))
		}
	}

	return &data, nil
}

// sortFields contains the values used to compare messages during a SORT.
//
// hasDate is false when the Date header is missing or cannot be parsed.
// String fields are empty when the corresponding header is missing.
type sortFields struct {
	arrival     time.Time
	size        int64
	hasDate     bool
	date        time.Time
	subject     string
	fromMailbox string
	toMailbox   string
	ccMailbox   string
}

func (msg *message) sortFields(needHeader bool) sortFields {
	fields := sortFields{
		arrival: msg.t,
		size:    int64(len(msg.buf)),
	}
	if !needHeader {
		return fields
	}

	br := bufio.NewReader(bytes.NewReader(msg.buf))
	header, err := textproto.ReadHeader(br)
	if err != nil {
		return fields
	}

	h := mail.Header{Header: gomessage.Header{Header: header}}

	if t, err := h.Date(); err == nil {
		fields.hasDate = true
		fields.date = t
	}
	if subject, err := h.Subject(); err == nil {
		fields.subject = baseSubject(subject)
	}
	fields.fromMailbox = firstAddressMailbox(&h, "From")
	fields.toMailbox = firstAddressMailbox(&h, "To")
	fields.ccMailbox = firstAddressMailbox(&h, "Cc")

	return fields
}

func compareSortField(a, b sortFields, key imap.SortKey) int {
	switch key {
	case imap.SortKeyArrival:
		return compareTime(a.arrival, b.arrival)
	case imap.SortKeyDate:
		// Messages without a valid Date header are always considered older.
		if !a.hasDate || !b.hasDate {
			return compareBool(!a.hasDate, !b.hasDate)
		}
		return compareTime(a.date, b.date)
	case imap.SortKeySize:
		return compareInt64(a.size, b.size)
	case imap.SortKeySubject:
		return compareSortString(a.subject, b.subject)
	case imap.SortKeyFrom:
		return compareSortString(a.fromMailbox, b.fromMailbox)
	case imap.SortKeyTo:
		return compareSortString(a.toMailbox, b.toMailbox)
	case imap.SortKeyCc:
		return compareSortString(a.ccMailbox, b.ccMailbox)
	default:
		panic("imapmemserver: unsupported SORT sort key " + string(key))
	}
}

// firstAddressMailbox returns the local-part of the first usable mailbox in
// the given address header field, as required by RFC 5256 section 2.1.
//
// Multi-valued address headers: only the first address is used. An empty
// string is returned when the field is missing or malformed.
func firstAddressMailbox(h *mail.Header, key string) string {
	addrs, err := h.AddressList(key)
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if addr == nil || addr.Address == "" {
			continue
		}
		if idx := strings.IndexByte(addr.Address, '@'); idx >= 0 {
			return addr.Address[:idx]
		}
		return addr.Address
	}
	return ""
}

// baseSubject strips the leading "Re:"/"Fwd:" prefixes defined by RFC 5256
// section 2.1, and removes leading and trailing whitespace.
func baseSubject(subject string) string {
	s := strings.TrimSpace(subject)

	for {
		s = strings.TrimLeft(s, " \t")

		rest, ok := stripSubjectPrefix(s)
		if !ok {
			return s
		}
		s = strings.TrimLeft(rest, " \t")
	}
}

// stripSubjectPrefix removes a single RFC 5256 subject reply/forward
// prefix: "Re:", "Fwd:", "Re[...]:", or "Fwd[...]:".
func stripSubjectPrefix(s string) (string, bool) {
	var prefixLen int
	switch {
	case strings.HasPrefix(strings.ToLower(s), "re"):
		prefixLen = 2
	case strings.HasPrefix(strings.ToLower(s), "fwd"):
		prefixLen = 3
	default:
		return s, false
	}

	rest := s[prefixLen:]

	// Optional bracketed text between the prefix word and the colon, e.g.
	// "Re[2]:".
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return s, false
		}
		rest = rest[end+1:]
	}

	if !strings.HasPrefix(rest, ":") {
		return s, false
	}
	return rest[1:], true
}

func compareTime(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	default:
		return 0
	}
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareBool(a, b bool) int {
	if a == b {
		return 0
	}
	if a {
		return -1
	}
	return 1
}

// compareSortString compares two strings using the i;ascii-casemap collation
// defined by RFC 5256: US-ASCII letters are case-folded, the result is then
// compared as byte sequences. Empty values sort first.
func compareSortString(a, b string) int {
	return strings.Compare(casemap(a), casemap(b))
}

func casemap(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}
