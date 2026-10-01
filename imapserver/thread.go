package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleThread(tag string, dec *imapwire.Decoder, numKind NumKind) error {
	var algorithm string
	if !dec.ExpectSP() || !dec.ExpectAtom(&algorithm) || !dec.ExpectSP() {
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
			Text: "Only US-ASCII and UTF-8 are supported THREAD charsets",
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

	session, ok := c.session.(SessionThread)
	if !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "THREAD is not supported",
		}
	}

	if !strings.EqualFold(algorithm, string(imap.ThreadReferences)) {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Unsupported THREAD algorithm",
		}
	}

	threads, err := session.Thread(numKind, imap.ThreadReferences, &criteria)
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
	needSP := false
	for _, num := range data.Chain {
		if needSP {
			enc.SP()
		}
		enc.Number(num)
		needSP = true
	}
	for i := range data.SubThreads {
		if needSP {
			enc.SP()
		}
		writeThreadData(enc, &data.SubThreads[i])
		needSP = true
	}
	enc.Special(')')
}
