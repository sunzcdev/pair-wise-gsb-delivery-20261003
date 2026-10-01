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

// Sort implements the SORT extension (RFC 5256).
//
// Only message headers are parsed to compute sort keys: message bodies are
// never read.
func (mbox *MailboxView) Sort(numKind imapserver.NumKind, options *imap.SortOptions) (*imap.SortData, error) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	searchCriteria := options.SearchCriteria
	if searchCriteria == nil {
		searchCriteria = &imap.SearchCriteria{}
	}
	mbox.staticSearchCriteria(searchCriteria)

	type sortableMessage struct {
		msg     *message
		seqNum  uint32 // from the server point-of-view
		sortKey *messageSortKey
	}
	var matched []sortableMessage
	for i, msg := range mbox.l {
		seqNum := uint32(i) + 1
		if !msg.search(mbox.tracker.EncodeSeqNum(seqNum), searchCriteria) {
			continue
		}
		matched = append(matched, sortableMessage{
			msg:     msg,
			seqNum:  seqNum,
			sortKey: msg.sortKey(),
		})
	}

	// Messages with equal sort keys keep their mailbox order
	sort.SliceStable(matched, func(i, j int) bool {
		return compareSortKeys(matched[i].sortKey, matched[j].sortKey, options.SortCriteria) < 0
	})

	var data imap.SortData
	for _, m := range matched {
		switch numKind {
		case imapserver.NumKindSeq:
			seqNum := mbox.tracker.EncodeSeqNum(m.seqNum)
			if seqNum == 0 {
				continue
			}
			data.Nums = append(data.Nums, seqNum)
		case imapserver.NumKindUID:
			data.Nums = append(data.Nums, uint32(m.msg.uid))
		}
	}
	return &data, nil
}

// messageSortKey contains the pre-computed sort keys of a message.
type messageSortKey struct {
	arrival time.Time
	date    time.Time
	size    int64
	from    string
	to      string
	cc      string
	subject string
}

func (msg *message) sortKey() *messageSortKey {
	key := &messageSortKey{
		arrival: msg.t,
		size:    int64(len(msg.buf)),
	}

	br := bufio.NewReader(bytes.NewReader(msg.buf))
	header, err := textproto.ReadHeader(br)
	if err != nil {
		key.date = msg.t
		return key
	}
	mailHeader := mail.Header{gomessage.Header{header}}

	// If the Date header field is missing or invalid, fall back to the
	// internal date
	if t, err := mailHeader.Date(); err == nil && !t.IsZero() {
		key.date = t
	} else {
		key.date = msg.t
	}

	key.from = addressSortKey(mailHeader, "From")
	key.to = addressSortKey(mailHeader, "To")
	key.cc = addressSortKey(mailHeader, "Cc")

	if subject, err := mailHeader.Subject(); err == nil {
		key.subject = baseSubject(subject)
	}

	return key
}

// addressSortKey returns the sort key for an address header field: the
// display name of the first address, or its address if the display name is
// missing. If the header field is missing or cannot be parsed, an empty
// string is returned.
func addressSortKey(header mail.Header, field string) string {
	addrs, err := header.AddressList(field)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	if addrs[0].Name != "" {
		return addrs[0].Name
	}
	return addrs[0].Address
}

func compareSortKeys(a, b *messageSortKey, criteria []imap.SortCriterion) int {
	for _, criterion := range criteria {
		var cmp int
		switch criterion.Key {
		case imap.SortKeyArrival:
			cmp = compareTime(a.arrival, b.arrival)
		case imap.SortKeyDate:
			cmp = compareTime(a.date, b.date)
		case imap.SortKeySize:
			cmp = compareInt64(a.size, b.size)
		case imap.SortKeyFrom:
			cmp = compareString(a.from, b.from)
		case imap.SortKeyTo:
			cmp = compareString(a.to, b.to)
		case imap.SortKeyCc:
			cmp = compareString(a.cc, b.cc)
		case imap.SortKeySubject:
			cmp = compareString(a.subject, b.subject)
		}
		if criterion.Reverse {
			cmp = -cmp
		}
		if cmp != 0 {
			return cmp
		}
	}
	return 0
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

func compareString(a, b string) int {
	return strings.Compare(strings.ToLower(a), strings.ToLower(b))
}

// baseSubject returns the "base subject" of a message, as described by
// RFC 5256 section 2.1: RFC 2047 encoding is expected to be already decoded,
// and subject prefixes ("Re:", "Fw:", "[...]") and trailers ("(fwd)") are
// stripped.
func baseSubject(subject string) string {
	for {
		prev := subject

		// Remove subj-trailer
		for {
			subject = strings.TrimSpace(subject)
			if !hasSuffixFold(subject, "(fwd)") {
				break
			}
			subject = subject[:len(subject)-len("(fwd)")]
		}

		// Remove subj-leader and subj-blob
		for {
			afterBlobs := trimSubjectBlobs(subject)
			if rest, ok := trimSubjectRefwd(afterBlobs); ok {
				subject = rest
				continue
			}
			if afterBlobs != subject && strings.TrimSpace(afterBlobs) != "" {
				subject = afterBlobs
				continue
			}
			break
		}

		// Remove "[fwd:" ... "]"
		if trimmed := strings.TrimSpace(subject); hasPrefixFold(trimmed, "[fwd:") && strings.HasSuffix(trimmed, "]") {
			subject = trimmed[len("[fwd:") : len(trimmed)-1]
		}

		if subject == prev {
			break
		}
	}
	return strings.TrimSpace(subject)
}

// trimSubjectBlobs strips leading "[...]" blobs (and surrounding whitespace)
// from a subject.
func trimSubjectBlobs(s string) string {
	for {
		s = strings.TrimLeft(s, " \t")
		if !strings.HasPrefix(s, "[") {
			return s
		}
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return s
		}
		s = s[end+1:]
	}
}

// trimSubjectRefwd strips a leading "re:" / "fw:" / "fwd:" prefix
// (case-insensitive, optionally followed by a "[...]" blob) from a subject.
func trimSubjectRefwd(s string) (string, bool) {
	s = strings.TrimLeft(s, " \t")
	lower := strings.ToLower(s)

	var rest string
	switch {
	case strings.HasPrefix(lower, "re"):
		rest = s[len("re"):]
	case strings.HasPrefix(lower, "fw"):
		rest = s[len("fw"):]
		rest = strings.TrimLeft(rest, " \t")
		if len(rest) > 0 && (rest[0] == 'd' || rest[0] == 'D') {
			rest = rest[1:]
		}
	default:
		return "", false
	}

	rest = trimSubjectBlobs(rest)
	rest = strings.TrimLeft(rest, " \t")
	if !strings.HasPrefix(rest, ":") {
		return "", false
	}
	return rest[1:], true
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}
