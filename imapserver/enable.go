package imapserver

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleEnable(dec *imapwire.Decoder) error {
	var requested []imap.Cap
	for dec.SP() {
		cap, err := internal.ExpectCap(dec)
		if err != nil {
			return err
		}
		requested = append(requested, cap)
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	var enabled []imap.Cap
	for _, req := range requested {
		if req == imap.CapCondStore {
			if _, ok := c.session.(SessionCondStore); !ok {
				continue
			}
		}
		switch req {
		case imap.CapIMAP4rev2, imap.CapUTF8Accept, imap.CapCondStore:
			enabled = append(enabled, req)
		}
	}

	c.mutex.Lock()
	for _, e := range enabled {
		c.enabled[e] = struct{}{}
	}
	if c.state == imap.ConnStateSelected {
		for _, e := range enabled {
			if e == imap.CapCondStore {
				c.condStore = true
			}
		}
	}
	c.mutex.Unlock()

	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("ENABLED")
	for _, c := range enabled {
		enc.SP().Atom(string(c))
	}
	return enc.CRLF()
}
