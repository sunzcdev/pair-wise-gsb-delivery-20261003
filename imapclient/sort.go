package imapclient

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// SortKey is a criterion used to sort messages.
//
// This is an alias for imap.SortKey.
type SortKey = imap.SortKey

const (
	SortKeyArrival = imap.SortKeyArrival
	SortKeyCc      = imap.SortKeyCc
	SortKeyDate    = imap.SortKeyDate
	SortKeyFrom    = imap.SortKeyFrom
	SortKeySize    = imap.SortKeySize
	SortKeySubject = imap.SortKeySubject
	SortKeyTo      = imap.SortKeyTo
)

// SortCriterion is a sort key, optionally reversed.
//
// This is an alias for imap.SortCriterion.
type SortCriterion = imap.SortCriterion

// SortOptions contains options for the SORT command.
//
// This is an alias for imap.SortOptions.
type SortOptions = imap.SortOptions

func (c *Client) sort(numKind imapwire.NumKind, options *SortOptions) *SortCommand {
	cmd := &SortCommand{}
	enc := c.beginCommand(uidCmdName("SORT", numKind), cmd)
	enc.SP().List(len(options.SortCriteria), func(i int) {
		criterion := options.SortCriteria[i]
		if criterion.Reverse {
			enc.Atom("REVERSE").SP()
		}
		enc.Atom(string(criterion.Key))
	})
	enc.SP().Atom("UTF-8").SP()
	writeSearchKey(enc.Encoder, options.SearchCriteria)
	enc.end()
	return cmd
}

func (c *Client) handleSort() error {
	cmd := findPendingCmdByType[*SortCommand](c)
	for c.dec.SP() {
		var num uint32
		if !c.dec.ExpectNumber(&num) {
			return c.dec.Err()
		}
		if cmd != nil {
			cmd.nums = append(cmd.nums, num)
		}
	}
	return nil
}

// Sort sends a SORT command.
//
// This command requires support for the SORT extension.
func (c *Client) Sort(options *SortOptions) *SortCommand {
	return c.sort(imapwire.NumKindSeq, options)
}

// UIDSort sends a UID SORT command.
//
// See Sort.
func (c *Client) UIDSort(options *SortOptions) *SortCommand {
	return c.sort(imapwire.NumKindUID, options)
}

// SortCommand is a SORT command.
type SortCommand struct {
	commandBase
	nums []uint32
}

func (cmd *SortCommand) Wait() ([]uint32, error) {
	err := cmd.wait()
	return cmd.nums, err
}
