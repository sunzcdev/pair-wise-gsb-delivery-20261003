package imapserver

import (
	"sort"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// QuotaData is the data returned by a QUOTA response.
type QuotaData struct {
	Root      string
	Resources map[imap.QuotaResourceType]*QuotaResourceData
}

// QuotaResourceData contains the usage and limit for a quota resource.
type QuotaResourceData struct {
	Usage int64
	Limit int64
}

// QuotaRootData is the data returned by a QUOTAROOT response.
type QuotaRootData struct {
	Mailbox string
	Roots   []string
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

	enc := newResponseEncoder(c)
	defer enc.end()
	return writeQuota(enc.Encoder, data)
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

	enc := newResponseEncoder(c)
	defer enc.end()
	if err := writeQuotaRoot(enc.Encoder, data); err != nil {
		return err
	}
	for _, root := range data.Roots {
		quotaData, err := session.GetQuota(root)
		if err != nil {
			return err
		}
		if err := writeQuota(enc.Encoder, quotaData); err != nil {
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

	resources := make(map[imap.QuotaResourceType]int64)
	err := dec.ExpectList(func() error {
		var (
			name  string
			limit int64
		)
		if !dec.ExpectAtom(&name) || !dec.ExpectSP() || !dec.ExpectNumber64(&limit) {
			return dec.Err()
		}
		resources[imap.QuotaResourceType(name)] = limit
		return nil
	})
	if err != nil {
		return err
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionQuotaSet)
	if !ok {
		return newClientBugError("SETQUOTA is not supported")
	}

	return session.SetQuota(root, resources)
}

func writeQuota(enc *imapwire.Encoder, data *QuotaData) error {
	resourceTypes := make([]string, 0, len(data.Resources))
	for resourceType := range data.Resources {
		resourceTypes = append(resourceTypes, string(resourceType))
	}
	sort.Strings(resourceTypes)

	enc.Atom("*").SP().Atom("QUOTA").SP().String(data.Root).SP().Special('(')
	for i, resourceType := range resourceTypes {
		resource := data.Resources[imap.QuotaResourceType(resourceType)]
		if i > 0 {
			enc.SP()
		}
		enc.Atom(resourceType).SP().Number64(resource.Usage).SP().Number64(resource.Limit)
	}
	enc.Special(')')
	return enc.CRLF()
}

func writeQuotaRoot(enc *imapwire.Encoder, data *QuotaRootData) error {
	enc.Atom("*").SP().Atom("QUOTAROOT").SP().Mailbox(data.Mailbox)
	for _, root := range data.Roots {
		enc.SP().String(root)
	}
	return enc.CRLF()
}
