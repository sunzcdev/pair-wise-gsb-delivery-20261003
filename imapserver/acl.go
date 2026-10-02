package imapserver

import (
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// flagIs reports whether the flag is the given system flag, compared
// case-insensitively (system flags are case-insensitive per RFC 9051).
func flagIs(flag, system imap.Flag) bool {
	return strings.EqualFold(string(flag), string(system))
}

// ACL-related errors.
//
// errACLMailboxNotFound deliberately carries no detail about the mailbox, so
// that users without the lookup right cannot distinguish between a
// non-existent and an existing but invisible mailbox (RFC 4314 section 5.2).
var errACLMailboxNotFound = &imap.Error{
	Type: imap.StatusResponseTypeNo,
	Code: imap.ResponseCodeNonExistent,
	Text: "No such mailbox",
}

var errACLNoPermission = &imap.Error{
	Type: imap.StatusResponseTypeNo,
	Code: imap.ResponseCodeNoPerm,
	Text: "Permission denied",
}

// aclSession returns the session if it implements the ACL extension.
func (c *Conn) aclSession() SessionACL {
	session, _ := c.session.(SessionACL)
	return session
}

// checkMailboxRights verifies that the authenticated user holds all the
// required rights on the mailbox.
//
// The lookup right is always required: without it, the mailbox is considered
// invisible and the same response is returned as for a non-existent mailbox.
func (c *Conn) checkMailboxRights(mailbox string, required ...imap.Right) error {
	session := c.aclSession()
	if session == nil {
		return nil
	}

	rights, err := session.MyRights(mailbox)
	if err != nil {
		return err
	}

	for _, right := range required {
		if !rights.Has(right) {
			if right == imap.RightLookup {
				return errACLMailboxNotFound
			}
			return errACLNoPermission
		}
	}

	return nil
}

func (c *Conn) handleGetACL(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session := c.aclSession()
	if session == nil {
		return newClientBugError("ACL is not supported")
	}
	data, err := session.GetACL(mailbox)
	if err != nil {
		return err
	}
	return c.writeACL(data)
}

func (c *Conn) handleMyRights(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session := c.aclSession()
	if session == nil {
		return newClientBugError("ACL is not supported")
	}
	rights, err := session.MyRights(mailbox)
	if err != nil {
		return err
	}
	return c.writeMyRights(mailbox, rights)
}

func (c *Conn) handleListRights(dec *imapwire.Decoder) error {
	var mailbox, identifier string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() || !dec.ExpectAString(&identifier) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session := c.aclSession()
	if session == nil {
		return newClientBugError("ACL is not supported")
	}
	data, err := session.ListRights(mailbox, imap.RightsIdentifier(identifier))
	if err != nil {
		return err
	}
	return c.writeListRights(data)
}

func (c *Conn) handleSetACL(dec *imapwire.Decoder) error {
	var (
		mailbox    string
		identifier string
		rightsStr  string
	)
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() || !dec.ExpectAString(&identifier) || !dec.ExpectSP() || !dec.ExpectAString(&rightsStr) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session := c.aclSession()
	if session == nil {
		return newClientBugError("ACL is not supported")
	}

	var mod imap.RightModification
	var rights imap.RightSet
	switch {
	case len(rightsStr) > 0 && rightsStr[0] == byte(imap.RightModificationAdd):
		mod = imap.RightModificationAdd
		rights = imap.RightSet(rightsStr[1:])
	case len(rightsStr) > 0 && rightsStr[0] == byte(imap.RightModificationRemove):
		mod = imap.RightModificationRemove
		rights = imap.RightSet(rightsStr[1:])
	default:
		mod = imap.RightModificationReplace
		rights = imap.RightSet(rightsStr)
	}

	return session.SetACL(mailbox, imap.RightsIdentifier(identifier), mod, rights)
}

func (c *Conn) handleDeleteACL(dec *imapwire.Decoder) error {
	var mailbox, identifier string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() || !dec.ExpectAString(&identifier) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session := c.aclSession()
	if session == nil {
		return newClientBugError("ACL is not supported")
	}
	return session.DeleteACL(mailbox, imap.RightsIdentifier(identifier))
}

func (c *Conn) writeACL(data *imap.ACLData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("ACL").SP().Mailbox(data.Mailbox)

	identifiers := make([]imap.RightsIdentifier, 0, len(data.Rights))
	for ri := range data.Rights {
		identifiers = append(identifiers, ri)
	}
	sort.Slice(identifiers, func(i, j int) bool {
		return identifiers[i] < identifiers[j]
	})

	for _, ri := range identifiers {
		enc.SP().String(string(ri)).SP().String(internal.FormatRights(imap.RightModificationReplace, data.Rights[ri]))
	}

	return enc.CRLF()
}

func (c *Conn) writeMyRights(mailbox string, rights imap.RightSet) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("MYRIGHTS").SP().Mailbox(mailbox).SP()
	enc.String(string(rights))
	return enc.CRLF()
}

func (c *Conn) writeListRights(data *imap.ListRightsData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("LISTRIGHTS").SP().Mailbox(data.Mailbox).SP()
	enc.String(string(data.Identifier)).SP().String(string(data.Required))
	for _, rights := range data.Optional {
		enc.SP().String(string(rights))
	}
	return enc.CRLF()
}
