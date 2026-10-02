package imapserver

import (
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
	if !dec.ExpectSP() || !dec.ExpectNumSet(numKind.wire(), &numSet) || !dec.ExpectSP() || !dec.ExpectAtom(&item) || !dec.ExpectSP() {
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

	// RFC 4314 section 5.2: the rights required for STORE depend on the
	// flags being changed. Any flag other than \Seen and \Deleted requires
	// "w"; \Seen alone requires "s", \Deleted alone requires "d". A STORE
	// mixing the two requires both "s" and "d".
	changeSeen, changeDeleted, changeOther := false, false, false
	for _, flag := range flags {
		switch {
		case flagIs(flag, imap.FlagSeen):
			changeSeen = true
		case flagIs(flag, imap.FlagDeleted):
			changeDeleted = true
		default:
			changeOther = true
		}
	}
	required := []imap.Right{imap.RightLookup}
	if changeOther {
		required = append(required, imap.RightWrite)
	} else {
		if changeSeen {
			required = append(required, imap.RightSeen)
		}
		if changeDeleted {
			required = append(required, imap.RightDelete)
		}
	}
	if err := c.checkMailboxRights(c.selectedMailbox, required...); err != nil {
		return err
	}

	w := &FetchWriter{conn: c}
	options := imap.StoreOptions{}
	return c.session.Store(w, numSet, &imap.StoreFlags{
		Op:     op,
		Silent: silent,
		Flags:  flags,
	}, &options)
}
