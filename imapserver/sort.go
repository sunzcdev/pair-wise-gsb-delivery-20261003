package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// SessionSort is an IMAP session which supports the SORT extension.
//
// Backends which implement this interface may perform the sort on their
// side: they receive the parsed sort criteria and search criteria, and only
// need to return the matching messages in the requested order. They don't
// need to read the full message bodies into memory, only the fields required
// by the sort keys.
type SessionSort interface {
	Session

	// Sort returns the messages matching the search criteria, in the order
	// defined by the sort criteria.
	Sort(kind NumKind, sortCriteria []imap.SortCriterion, criteria *imap.SearchCriteria) (*imap.SortData, error)
}

func (c *Conn) handleSort(dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	var sortCriteria []imap.SortCriterion
	err := dec.ExpectList(func() error {
		var criterion imap.SortCriterion

		var atom string
		if !dec.ExpectAtom(&atom) {
			return dec.Err()
		}
		if strings.EqualFold(atom, "REVERSE") {
			if !dec.ExpectSP() || !dec.ExpectAtom(&atom) {
				return dec.Err()
			}
			criterion.Reverse = true
		}

		criterion.Key = imap.SortKey(strings.ToUpper(atom))
		switch criterion.Key {
		case imap.SortKeyArrival,
			imap.SortKeyCc,
			imap.SortKeyDate,
			imap.SortKeyFrom,
			imap.SortKeySize,
			imap.SortKeySubject,
			imap.SortKeyTo:
			// supported
		default:
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Code: imap.ResponseCodeClientBug,
				Text: fmt.Sprintf("unsupported SORT sort key %q", atom),
			}
		}

		sortCriteria = append(sortCriteria, criterion)
		return nil
	})
	if err != nil {
		return fmt.Errorf("in sort-criteria: %w", err)
	}

	if !dec.ExpectSP() {
		return dec.Err()
	}

	var charset string
	if !dec.ExpectAString(&charset) || !dec.ExpectSP() {
		return dec.Err()
	}
	switch strings.ToUpper(charset) {
	case "US-ASCII", "UTF-8":
		// nothing to do
	default:
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeBadCharset, // TODO: return list of supported charsets
			Text: "Only US-ASCII and UTF-8 are supported SORT charsets",
		}
	}

	var criteria imap.SearchCriteria
	if err := readSearchCriteriaArgs(&criteria, dec, ""); err != nil {
		return err
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}

	sess, ok := c.session.(SessionSort)
	if !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "SORT is not supported",
		}
	}

	data, err := sess.Sort(numKind, sortCriteria, &criteria)
	if err != nil {
		return err
	}

	return c.writeSort(numKind, data.All)
}

// readSearchCriteriaArgs reads a list of search keys until the end of the
// command line. If atom is non-empty, it is used as the first search key.
func readSearchCriteriaArgs(criteria *imap.SearchCriteria, dec *imapwire.Decoder, atom string) error {
	for {
		var err error
		if atom != "" {
			err = readSearchKeyWithAtom(criteria, dec, atom)
			atom = ""
		} else {
			err = readSearchKey(criteria, dec)
		}
		if err != nil {
			return fmt.Errorf("in search-key: %w", err)
		}

		if !dec.SP() {
			break
		}
	}

	return nil
}

func (c *Conn) writeSort(numKind NumKind, nums []uint32) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("SORT")
	for _, num := range nums {
		if numKind == NumKindUID {
			enc.SP().UID(imap.UID(num))
		} else {
			enc.SP().Number(num)
		}
	}
	return enc.CRLF()
}
