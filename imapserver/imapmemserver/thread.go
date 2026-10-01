package imapmemserver

import (
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/mail"
)

// threadContainer is a node in the REFERENCES threading graph. A container
// without an associated message (msg == nil) is a dummy: it stands for a
// referenced message which is not part of the searched set.
type threadContainer struct {
	id       string
	msg      *message // nil for dummy containers
	seqNum   uint32
	date     time.Time
	parent   *threadContainer
	children []*threadContainer
}

// Thread implements the REFERENCES threading algorithm (RFC 5256) over the
// messages matching the search criteria.
//
// Relationships are derived exclusively from the Message-ID, References and
// In-Reply-To header fields. Messages are processed in a canonical order
// (sent date, then Message-ID) so that the resulting tree does not depend on
// the order in which messages were added to the mailbox.
func (mbox *MailboxView) Thread(numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
	if algorithm != imap.ThreadReferences {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Unsupported THREAD algorithm",
		}
	}

	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	mbox.staticSearchCriteria(criteria)

	type threadMessage struct {
		msg    *message
		seqNum uint32
		id     string
		refs   []string
		date   time.Time
	}

	var msgs []*threadMessage
	for i, msg := range mbox.l {
		seqNum := mbox.tracker.EncodeSeqNum(uint32(i) + 1)
		if !msg.search(seqNum, criteria) {
			continue
		}
		if numKind == imapserver.NumKindSeq && seqNum == 0 {
			continue
		}

		header := mail.Header{msg.reader().Header}
		id, _ := header.MessageID()
		refs, _ := header.MsgIDList("References")
		// The last ID in In-Reply-To identifies the direct parent when
		// there is no References header
		if irt, _ := header.MsgIDList("In-Reply-To"); len(irt) > 0 {
			last := irt[len(irt)-1]
			found := false
			for _, ref := range refs {
				if ref == last {
					found = true
					break
				}
			}
			if !found {
				refs = append(refs, last)
			}
		}
		date, err := header.Date()
		if err != nil {
			date = msg.t
		}

		msgs = append(msgs, &threadMessage{
			msg:    msg,
			seqNum: seqNum,
			id:     id,
			refs:   refs,
			date:   date,
		})
	}

	// Sort messages by sent date (then Message-ID) before threading, so
	// that the result is independent from the mailbox insertion order
	sort.SliceStable(msgs, func(i, j int) bool {
		if !msgs[i].date.Equal(msgs[j].date) {
			return msgs[i].date.Before(msgs[j].date)
		}
		return msgs[i].id < msgs[j].id
	})

	idTable := make(map[string]*threadContainer)
	var all []*threadContainer
	newContainer := func(id string) *threadContainer {
		c := &threadContainer{id: id}
		all = append(all, c)
		return c
	}
	containerForID := func(id string) *threadContainer {
		if c, ok := idTable[id]; ok {
			return c
		}
		c := newContainer(id)
		idTable[id] = c
		return c
	}

	for _, tm := range msgs {
		var c *threadContainer
		if tm.id != "" {
			if existing, ok := idTable[tm.id]; ok {
				if existing.msg != nil {
					// Duplicate Message-ID: keep the first message
					continue
				}
				c = existing
			} else {
				c = containerForID(tm.id)
			}
		} else {
			// Messages without a Message-ID get their own container
			c = newContainer("")
		}
		c.msg = tm.msg
		c.seqNum = tm.seqNum
		c.date = tm.date

		var prev *threadContainer
		for _, ref := range tm.refs {
			if ref == "" {
				continue
			}
			refC := containerForID(ref)
			if prev != nil {
				linkThreadContainers(prev, refC)
			}
			prev = refC
		}
		if prev != nil {
			linkThreadContainers(prev, c)
		}
	}

	var roots []*threadContainer
	for _, c := range all {
		if c.parent == nil {
			roots = append(roots, c)
		}
	}

	roots = pruneThreadContainers(roots)
	sortThreadContainers(roots)

	num := func(c *threadContainer) uint32 {
		if numKind == imapserver.NumKindUID {
			return uint32(c.msg.uid)
		}
		return c.seqNum
	}

	threads := make([]imap.ThreadData, 0, len(roots))
	for _, c := range roots {
		threads = append(threads, flattenThreadContainer(c, num))
	}
	return threads, nil
}

// linkThreadContainers sets child as a child of parent. The link is skipped
// if the child already has a parent or if the link would create a loop
// (e.g. self-references or reference cycles).
func linkThreadContainers(parent, child *threadContainer) {
	if parent == child || child.parent != nil {
		return
	}
	for c := parent; c != nil; c = c.parent {
		if c == child {
			return
		}
	}
	child.parent = parent
	parent.children = append(parent.children, child)
}

// pruneThreadContainers removes dummy containers, promoting their children
// to the current level.
func pruneThreadContainers(containers []*threadContainer) []*threadContainer {
	var out []*threadContainer
	for _, c := range containers {
		c.children = pruneThreadContainers(c.children)
		if c.msg == nil {
			out = append(out, c.children...)
		} else {
			out = append(out, c)
		}
	}
	return out
}

// sortThreadContainers sorts siblings by sent date, recursively.
func sortThreadContainers(containers []*threadContainer) {
	sort.SliceStable(containers, func(i, j int) bool {
		if !containers[i].date.Equal(containers[j].date) {
			return containers[i].date.Before(containers[j].date)
		}
		return containers[i].id < containers[j].id
	})
	for _, c := range containers {
		sortThreadContainers(c.children)
	}
}

// flattenThreadContainer converts a container tree into the wire format:
// chains of single children are collapsed into ThreadData.Chain, while
// multiple children become ThreadData.SubThreads.
func flattenThreadContainer(c *threadContainer, num func(*threadContainer) uint32) imap.ThreadData {
	var data imap.ThreadData
	cur := c
	for {
		data.Chain = append(data.Chain, num(cur))
		if len(cur.children) != 1 {
			break
		}
		cur = cur.children[0]
	}
	for _, child := range cur.children {
		data.SubThreads = append(data.SubThreads, flattenThreadContainer(child, num))
	}
	return data
}
