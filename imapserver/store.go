package imapserver

import (
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleStore(tag string, dec *imapwire.Decoder, numKind NumKind) error {
	var (
		numSet  imap.NumSet
		item    string
		options imap.StoreOptions
	)
	if !dec.ExpectSP() || !dec.ExpectNumSet(numKind.wire(), &numSet) || !dec.ExpectSP() {
		return dec.Err()
	}

	// STORE modifiers, e.g. UNCHANGEDSINCE (RFC 7162)
	if dec.Special('(') {
		for {
			var paramName string
			if !dec.ExpectAtom(&paramName) || !dec.ExpectSP() {
				return dec.Err()
			}
			switch strings.ToUpper(paramName) {
			case "UNCHANGEDSINCE":
				if !dec.ExpectModSeq(&options.UnchangedSince) {
					return dec.Err()
				}
			default:
				return newClientBugError("Unknown STORE modifier")
			}
			if dec.Special(')') {
				break
			}
			if !dec.ExpectSP() {
				return dec.Err()
			}
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
	}

	if !dec.ExpectAtom(&item) || !dec.ExpectSP() {
		return dec.Err()
	}
	var flags []imap.Flag
	isList, err := dec.List(func() error {
		flag, err := internal.ExpectFlag(dec)
		if err != nil {
			return err
		}
		flags = append(flags, flag)
		return nil
	})
	if err != nil {
		return err
	} else if !isList {
		for {
			flag, err := internal.ExpectFlag(dec)
			if err != nil {
				return err
			}
			flags = append(flags, flag)

			if !dec.SP() {
				break
			}
		}
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	item = strings.ToUpper(item)
	silent := strings.HasSuffix(item, ".SILENT")
	item = strings.TrimSuffix(item, ".SILENT")

	var op imap.StoreFlagsOp
	switch {
	case strings.HasPrefix(item, "+"):
		op = imap.StoreFlagsAdd
		item = strings.TrimPrefix(item, "+")
	case strings.HasPrefix(item, "-"):
		op = imap.StoreFlagsDel
		item = strings.TrimPrefix(item, "-")
	default:
		op = imap.StoreFlagsSet
	}

	if item != "FLAGS" {
		return newClientBugError("STORE can only change FLAGS")
	}

	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}

	w := &FetchWriter{conn: c}
	result, err := c.session.Store(w, numSet, &imap.StoreFlags{
		Op:     op,
		Silent: silent,
		Flags:  flags,
	}, &options)
	if err != nil {
		return err
	}

	if result != nil {
		// MODIFIED must be sent after this command's FETCH responses and
		// before the tagged completion response (RFC 7162 section 3.1.3).
		if result.Modified != nil {
			if err := c.writeModified(result.Modified); err != nil {
				return err
			}
		}
	}

	// Flush pending unilateral updates (e.g. the flag changes made by
	// another session) before reporting the result.
	if err := c.poll("STORE"); err != nil {
		return err
	}

	if result != nil {
		if result.ModSeq != 0 {
			return c.writeStoreOK(tag, result.ModSeq)
		}
	}

	return c.writeStatusResp(tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: "STORE completed",
	})
}

// writeModified writes an untagged MODIFIED response (RFC 7162 section 3.1.3).
//
// The number set is a UIDSet for a UID STORE command, a SeqSet otherwise.
func (c *Conn) writeModified(modified imap.NumSet) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Number(1).SP().Atom("MODIFIED").SP().NumSet(modified)
	return enc.CRLF()
}

func (c *Conn) writeStoreOK(tag string, modSeq uint64) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom(tag).SP().Atom("OK").SP()
	enc.Special('[').Atom("HIGHESTMODSEQ").SP().ModSeq(modSeq).Special(']').SP()
	enc.Text("Conditional STORE completed")
	return enc.CRLF()
}
