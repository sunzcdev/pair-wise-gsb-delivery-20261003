package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleThread(dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	var algorithm string
	if !dec.ExpectAtom(&algorithm) || !dec.ExpectSP() {
		return dec.Err()
	}

	var charset string
	if !dec.ExpectAString(&charset) {
		return dec.Err()
	}
	switch strings.ToUpper(charset) {
	case "US-ASCII", "UTF-8":
		// nothing to do
	default:
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeBadCharset,
			Text: "Only US-ASCII and UTF-8 are supported THREAD charsets",
		}
	}

	var criteria imap.SearchCriteria
	for {
		if !dec.ExpectSP() {
			return dec.Err()
		}
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

	session, ok := c.session.(SessionThread)
	if !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "THREAD is not supported",
		}
	}

	threads, err := session.Thread(numKind, imap.ThreadAlgorithm(strings.ToUpper(algorithm)), &criteria)
	if err != nil {
		return err
	}

	return c.writeThread(threads)
}

func (c *Conn) writeThread(threads []imap.ThreadData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("THREAD")
	for i := range threads {
		enc.SP()
		writeThreadData(enc.Encoder, &threads[i])
	}
	return enc.CRLF()
}

func writeThreadData(enc *imapwire.Encoder, data *imap.ThreadData) {
	enc.Special('(')
	for i, num := range data.Chain {
		if i > 0 {
			enc.SP()
		}
		enc.Number(num)
	}
	for i := range data.SubThreads {
		if len(data.Chain) > 0 || i > 0 {
			enc.SP()
		}
		writeThreadData(enc, &data.SubThreads[i])
	}
	enc.Special(')')
}
