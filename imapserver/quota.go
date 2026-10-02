package imapserver

import (
	"sort"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// QuotaData is the data returned by a GETQUOTA or SETQUOTA command.
type QuotaData struct {
	Root      string
	Resources map[imap.QuotaResourceType]QuotaResourceData
}

// QuotaResourceData contains the usage and limit for a single quota resource.
//
// The usage and limit are expressed in the resource unit defined by RFC 9208
// section 5: STORAGE is measured in kibibytes (1024 octets), MESSAGE in
// number of messages.
type QuotaResourceData struct {
	Usage int64
	Limit int64
}

// SessionQuota is an IMAP session which supports the QUOTA extension.
//
// See RFC 9208.
type SessionQuota interface {
	Session

	// QuotaResourceTypes returns the resource types tracked by the server.
	QuotaResourceTypes() []imap.QuotaResourceType

	// GetQuota returns the quota data for the provided root.
	GetQuota(root string) (*QuotaData, error)

	// GetQuotaRoot returns the quota roots covering the provided mailbox.
	GetQuotaRoot(mailbox string) ([]QuotaData, error)
}

// SessionQuotaSet is an IMAP session which is allowed to change quotas with
// the SETQUOTA command.
//
// Only administrative sessions are expected to implement this interface;
// regular user sessions should not, in which case the server doesn't
// advertise the QUOTASET capability and rejects SETQUOTA.
//
// See RFC 9208 section 4.3.
type SessionQuotaSet interface {
	SessionQuota

	// SetQuota changes the resource limits for the provided quota root.
	//
	// A resource limit of 0 means that the resource is not limited.
	SetQuota(root string, limits map[imap.QuotaResourceType]int64) error
}

func (c *Conn) handleGetQuota(dec *imapwire.Decoder) error {
	var root string
	if !dec.ExpectSP() || !dec.ExpectAString(&root) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session, ok := c.session.(SessionQuota)
	if !ok {
		return newClientBugError("QUOTA is not supported")
	}
	data, err := session.GetQuota(root)
	if err != nil {
		return err
	}
	if data == nil {
		data = &QuotaData{Root: root}
	}
	return c.writeQuota(data)
}

func (c *Conn) handleGetQuotaRoot(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session, ok := c.session.(SessionQuota)
	if !ok {
		return newClientBugError("QUOTA is not supported")
	}
	data, err := session.GetQuotaRoot(mailbox)
	if err != nil {
		return err
	}
	if err := c.writeQuotaRoot(mailbox, data); err != nil {
		return err
	}
	for i := range data {
		if err := c.writeQuota(&data[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) handleSetQuota(dec *imapwire.Decoder) error {
	var root string
	if !dec.ExpectSP() || !dec.ExpectAString(&root) || !dec.ExpectSP() {
		return dec.Err()
	}
	limits := make(map[imap.QuotaResourceType]int64)
	if err := dec.ExpectList(func() error {
		var (
			typ   string
			limit int64
		)
		if !dec.ExpectAtom(&typ) || !dec.ExpectSP() || !dec.ExpectNumber64(&limit) {
			return dec.Err()
		}
		limits[imap.QuotaResourceType(typ)] = limit
		return nil
	}); err != nil || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session, ok := c.session.(SessionQuotaSet)
	if !ok {
		return newClientBugError("SETQUOTA is not supported")
	}
	return session.SetQuota(root, limits)
}

func (c *Conn) writeQuotaRoot(mailbox string, data []QuotaData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("QUOTAROOT").SP().Mailbox(mailbox)
	for i := range data {
		enc.SP().String(data[i].Root)
	}
	return enc.CRLF()
}

func (c *Conn) writeQuota(data *QuotaData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("QUOTA").SP().String(data.Root).SP().Special('(')

	types := make([]imap.QuotaResourceType, 0, len(data.Resources))
	for typ := range data.Resources {
		types = append(types, typ)
	}
	sort.Slice(types, func(i, j int) bool {
		return types[i] < types[j]
	})

	for i, typ := range types {
		if i > 0 {
			enc.SP()
		}
		res := data.Resources[typ]
		enc.Atom(string(typ)).SP().Number64(res.Usage).SP().Number64(res.Limit)
	}
	return enc.Special(')').CRLF()
}
