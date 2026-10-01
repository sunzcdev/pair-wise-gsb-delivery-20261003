package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleSort(dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	var sortCriteria []imap.SortCriterion
	err := dec.ExpectList(func() error {
		var criterion imap.SortCriterion

		var key string
		if !dec.ExpectAtom(&key) {
			return dec.Err()
		}
		if strings.EqualFold(key, "REVERSE") {
			criterion.Reverse = true
			if !dec.ExpectSP() || !dec.ExpectAtom(&key) {
				return dec.Err()
			}
		}

		switch sortKey := imap.SortKey(strings.ToUpper(key)); sortKey {
		case imap.SortKeyArrival, imap.SortKeyCc, imap.SortKeyDate,
			imap.SortKeyFrom, imap.SortKeySize, imap.SortKeySubject,
			imap.SortKeyTo:
			criterion.Key = sortKey
		default:
			return newClientBugError(fmt.Sprintf("unknown sort key %q", key))
		}

		sortCriteria = append(sortCriteria, criterion)
		return nil
	})
	if err != nil {
		return fmt.Errorf("in sort-criteria: %w", err)
	}

	var charset string
	if !dec.ExpectSP() || !dec.ExpectAString(&charset) || !dec.ExpectSP() {
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
	for {
		if err := readSearchKey(&criteria, dec); err != nil {
			return fmt.Errorf("in search-key: %w", err)
		}
		if !dec.SP() {
			break
		}
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}

	session, ok := c.session.(SessionSort)
	if !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "SORT is not supported",
		}
	}

	options := imap.SortOptions{
		SearchCriteria: &criteria,
		SortCriteria:   sortCriteria,
	}
	data, err := session.Sort(numKind, &options)
	if err != nil {
		return err
	}

	return c.writeSort(data)
}

func (c *Conn) writeSort(data *imap.SortData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("SORT")
	for _, num := range data.Nums {
		enc.SP().Number(num)
	}
	return enc.CRLF()
}
