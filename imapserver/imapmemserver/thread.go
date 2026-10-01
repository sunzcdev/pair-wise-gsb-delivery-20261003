package imapmemserver

import (
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/mail"
)

// threadContainer is a node in a message thread tree.
//
// A container may have no message attached: this happens when a referenced
// message is missing from the mailbox or has been filtered out by the search
// criteria. Empty containers are pruned before the tree is returned.
type threadContainer struct {
	id       string // Message-ID of the attached message, if any
	msg      *message
	seqNum   uint32
	sentDate time.Time
	parent   *threadContainer
	children []*threadContainer
}

// Thread implements the REFERENCES threading algorithm (RFC 5256).
//
// Messages are organized by their Message-ID, References and In-Reply-To
// header fields. Messages are processed ordered by sent date, so the
// resulting tree does not depend on the order in which messages were added
// to the mailbox.
func (mbox *MailboxView) Thread(numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
	if algorithm != imap.ThreadReferences {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: "Unsupported thread algorithm",
		}
	}

	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	mbox.staticSearchCriteria(criteria)

	// Collect messages matching the search criteria, ordered by sent date
	// so that the resulting tree is independent of the mailbox order
	type threadMessage struct {
		msg      *message
		seqNum   uint32
		id       string
		refs     []string
		sentDate time.Time
	}
	var msgs []threadMessage
	for i, msg := range mbox.l {
		seqNum := uint32(i) + 1
		if !msg.search(mbox.tracker.EncodeSeqNum(seqNum), criteria) {
			continue
		}

		header := mail.Header{msg.reader().Header}
		msgID, _ := header.MessageID()
		refs, _ := header.MsgIDList("References")
		inReplyTo, _ := header.MsgIDList("In-Reply-To")
		if n := len(inReplyTo); n > 0 {
			last := inReplyTo[n-1]
			if !stringListContains(refs, last) {
				refs = append(refs, last)
			}
		}
		sentDate, err := header.Date()
		if err != nil {
			sentDate = msg.t
		}

		msgs = append(msgs, threadMessage{
			msg:      msg,
			seqNum:   seqNum,
			id:       msgID,
			refs:     refs,
			sentDate: sentDate,
		})
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		if !msgs[i].sentDate.Equal(msgs[j].sentDate) {
			return msgs[i].sentDate.Before(msgs[j].sentDate)
		}
		if msgs[i].id != msgs[j].id {
			return msgs[i].id < msgs[j].id
		}
		return msgs[i].seqNum < msgs[j].seqNum
	})

	containers := make(map[string]*threadContainer)
	var all []*threadContainer
	getContainer := func(id string) *threadContainer {
		if c, ok := containers[id]; ok {
			return c
		}
		c := &threadContainer{id: id}
		containers[id] = c
		all = append(all, c)
		return c
	}

	for _, m := range msgs {
		var container *threadContainer
		if m.id != "" {
			container = getContainer(m.id)
			if container.msg != nil {
				// Duplicate Message-ID: keep the first message
				continue
			}
			container.msg = m.msg
			container.seqNum = m.seqNum
			container.sentDate = m.sentDate
		} else {
			// Messages without a Message-ID cannot be referenced: each
			// gets its own unregistered container
			container = &threadContainer{
				msg:      m.msg,
				seqNum:   m.seqNum,
				sentDate: m.sentDate,
			}
			all = append(all, container)
		}

		// Link the containers of the referenced messages together, then
		// link this message's container under the last reference
		var parent *threadContainer
		for _, refID := range m.refs {
			if refID == "" {
				continue
			}
			c := getContainer(refID)
			linkThreadContainers(parent, c)
			parent = c
		}
		linkThreadContainers(parent, container)
	}

	var roots []*threadContainer
	for _, c := range all {
		if c.parent == nil {
			roots = append(roots, c)
		}
	}

	roots = pruneThreadContainers(roots, nil)

	// Sort siblings by sent date
	var sortContainers func(l []*threadContainer)
	sortContainers = func(l []*threadContainer) {
		sort.SliceStable(l, func(i, j int) bool {
			if !l[i].sentDate.Equal(l[j].sentDate) {
				return l[i].sentDate.Before(l[j].sentDate)
			}
			if l[i].id != l[j].id {
				return l[i].id < l[j].id
			}
			return l[i].seqNum < l[j].seqNum
		})
		for _, c := range l {
			sortContainers(c.children)
		}
	}
	sortContainers(roots)

	numOf := func(c *threadContainer) uint32 {
		switch numKind {
		case imapserver.NumKindSeq:
			return mbox.tracker.EncodeSeqNum(c.seqNum)
		case imapserver.NumKindUID:
			return uint32(c.msg.uid)
		default:
			panic("imapmemserver: unknown NumKind")
		}
	}

	threads := make([]imap.ThreadData, 0, len(roots))
	for _, c := range roots {
		threads = append(threads, buildThreadData(c, numOf))
	}
	return threads, nil
}

// linkThreadContainers makes child a child of parent, unless the link would
// be a self-reference or would create a reference loop, or unless child
// already has a parent.
func linkThreadContainers(parent, child *threadContainer) {
	if parent == nil || child == nil || parent == child || child.parent != nil {
		return
	}
	// Don't create a loop: refuse the link if child is an ancestor of parent
	for c := parent; c != nil; c = c.parent {
		if c == child {
			return
		}
	}
	child.parent = parent
	parent.children = append(parent.children, child)
}

// pruneThreadContainers removes containers which have no message attached,
// splicing their children into their position.
func pruneThreadContainers(siblings []*threadContainer, parent *threadContainer) []*threadContainer {
	var out []*threadContainer
	for _, c := range siblings {
		c.children = pruneThreadContainers(c.children, c)
		if c.msg == nil {
			for _, child := range c.children {
				child.parent = parent
			}
			out = append(out, c.children...)
		} else {
			c.parent = parent
			out = append(out, c)
		}
	}
	return out
}

// buildThreadData converts a thread container tree into ThreadData. Runs of
// single children are collapsed into a chain.
func buildThreadData(c *threadContainer, numOf func(*threadContainer) uint32) imap.ThreadData {
	var data imap.ThreadData
	cur := c
	for {
		data.Chain = append(data.Chain, numOf(cur))
		if len(cur.children) != 1 {
			break
		}
		cur = cur.children[0]
	}
	for _, child := range cur.children {
		data.SubThreads = append(data.SubThreads, buildThreadData(child, numOf))
	}
	return data
}

func stringListContains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
