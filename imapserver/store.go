package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleStore(dec *imapwire.Decoder, numKind NumKind) error {
	var (
		numSet imap.NumSet
		item   string
	)
	if !dec.ExpectSP() || !dec.ExpectNumSet(numKind.wire(), &numSet) || !dec.ExpectSP() {
		return dec.Err()
	}

	var options imap.StoreOptions
	if dec.Special('(') {
		// Store modifier, e.g. "(UNCHANGEDSINCE <mod-seq>)"
		var name string
		if !dec.ExpectAtom(&name) {
			return dec.Err()
		}
		if strings.ToUpper(name) != "UNCHANGEDSINCE" {
			return newClientBugError(fmt.Sprintf("Unknown STORE modifier %q", name))
		}
		if !dec.ExpectSP() || !dec.ExpectModSeq(&options.UnchangedSince) || !dec.ExpectSpecial(')') || !dec.ExpectSP() {
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
	storeFlags := &imap.StoreFlags{
		Op:     op,
		Silent: silent,
		Flags:  flags,
	}

	if options.UnchangedSince != 0 {
		session, ok := c.session.(SessionCondStore)
		if !ok {
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Text: "CONDSTORE is not supported",
			}
		}
		modified, err := session.StoreCondStore(w, numSet, storeFlags, &options)
		if err != nil {
			return err
		}
		if !isNumSetEmpty(modified) {
			return c.writeModified(modified)
		}
		return nil
	}

	return c.session.Store(w, numSet, storeFlags, &options)
}

// writeModified writes an untagged OK response with a MODIFIED response
// code, listing the messages which failed a conditional STORE.
func (c *Conn) writeModified(numSet imap.NumSet) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("MODIFIED").SP().NumSet(numSet).Special(']')
	enc.SP().Text("Conditional STORE failed for some messages")
	return enc.CRLF()
}
