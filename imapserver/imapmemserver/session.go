package imapmemserver

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

type (
	user    = User
	mailbox = MailboxView
)

// UserSession represents a session tied to a specific user.
//
// UserSession implements imapserver.Session. Typically, a UserSession pointer
// is embedded into a larger struct which overrides Login.
type UserSession struct {
	*user    // immutable
	*mailbox // may be nil
}

var _ imapserver.SessionIMAP4rev2 = (*UserSession)(nil)

// NewUserSession creates a new user session.
func NewUserSession(user *User) *UserSession {
	return &UserSession{user: user}
}

func (sess *UserSession) Close() error {
	if sess != nil && sess.mailbox != nil {
		sess.mailbox.Close()
	}
	return nil
}

func (sess *UserSession) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	mbox, err := sess.user.mailbox(name)
	if err != nil {
		return nil, err
	}
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	sess.mailbox = mbox.NewView()
	return mbox.selectDataLocked(), nil
}

func (sess *UserSession) Unselect() error {
	sess.mailbox.Close()
	sess.mailbox = nil
	return nil
}

func (sess *UserSession) Copy(numSet imap.NumSet, destName string) (*imap.CopyData, error) {
	dest, err := sess.user.mailbox(destName)
	if err != nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	} else if sess.mailbox != nil && dest == sess.mailbox.Mailbox {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Source and destination mailboxes are identical",
		}
	}

	// Compute the footprint first, without mutating anything.
	var delta quotaResourceDelta
	var msgs []*message
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		d := messageDelta(msg)
		delta.messages += d.messages
		delta.storage += d.storage
		msgs = append(msgs, msg)
	})

	if len(msgs) == 0 {
		return &imap.CopyData{UIDValidity: dest.uidValidity}, nil
	}

	// Reserve the destination quota atomically before writing anything.
	destRoot := dest.quotaRoot
	if destRoot == nil {
		destRoot = sess.user.defaultQuotaRoot()
		dest.quotaRoot = destRoot
	}
	if err := destRoot.reserve(delta); err != nil {
		return nil, err
	}

	// COPY is all-or-nothing: on a write failure, give the reservation back
	// so that a failed operation never consumes quota.
	success := false
	defer func() {
		if !success {
			destRoot.release(delta)
		}
	}()

	var sourceUIDs, destUIDs imap.UIDSet
	for _, msg := range msgs {
		appendData := dest.appendReserved(msg.buf, dest.copyMsgOptions(msg))
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
	}

	success = true
	return &imap.CopyData{
		UIDValidity: dest.uidValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	}, nil
}

func (sess *UserSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, destName string) error {
	dest, err := sess.user.mailbox(destName)
	if err != nil {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	} else if sess.mailbox != nil && dest == sess.mailbox.Mailbox {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Source and destination mailboxes are identical",
		}
	}

	srcRoot := sess.mailbox.quotaRoot
	if srcRoot == nil {
		srcRoot = sess.user.defaultQuotaRoot()
		sess.mailbox.quotaRoot = srcRoot
	}
	destRoot := dest.quotaRoot
	if destRoot == nil {
		destRoot = sess.user.defaultQuotaRoot()
		dest.quotaRoot = destRoot
	}

	var (
		delta    quotaResourceDelta
		msgs     []*message
		expunged = make(map[*message]struct{})
	)
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		d := messageDelta(msg)
		delta.messages += d.messages
		delta.storage += d.storage
		msgs = append(msgs, msg)
		expunged[msg] = struct{}{}
	})

	if len(msgs) == 0 {
		return w.WriteCopyData(&imap.CopyData{UIDValidity: dest.uidValidity})
	}

	// Within the same quota root, a MOVE is a relocation: the source share is
	// released as the destination share is consumed, so the messages are
	// never counted twice. Across roots the destination share is reserved up
	// front, atomically with respect to other sessions.
	if err := moveReserve(srcRoot, destRoot, delta); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			moveRollback(srcRoot, destRoot, delta)
		}
	}()

	// Lock both mailboxes for the relocation in a deterministic order so
	// concurrent moves in opposite directions cannot deadlock. Holding both
	// for the copy+expunge makes the move atomic with respect to other
	// commands.
	var sourceUIDs, destUIDs imap.UIDSet
	lockMailboxes(dest, sess.mailbox.Mailbox)
	defer dest.mutex.Unlock()
	defer sess.mailbox.mutex.Unlock()
	for _, msg := range msgs {
		appendData := dest.appendReservedLocked(msg.buf, dest.copyMsgOptions(msg))
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
	}
	seqNums := sess.mailbox.expungeLocked(expunged)

	// The destination copy and the source removal are now both done: the
	// move reached its success boundary, so the source share can be freed
	// (no-op for a same-root move).
	moveCommit(srcRoot, destRoot, delta)
	committed = true

	err = w.WriteCopyData(&imap.CopyData{
		UIDValidity: dest.uidValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	})
	if err != nil {
		return err
	}

	for _, seqNum := range seqNums {
		if err := w.WriteExpunge(sess.mailbox.tracker.EncodeSeqNum(seqNum)); err != nil {
			return err
		}
	}

	return nil
}

func (sess *UserSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if sess.mailbox == nil {
		return nil
	}
	return sess.mailbox.Poll(w, allowExpunge)
}

func (sess *UserSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if sess.mailbox == nil {
		return nil // TODO
	}
	return sess.mailbox.Idle(w, stop)
}

// QuotaResourceTypes implements imapserver.SessionQuota.
func (sess *UserSession) QuotaResourceTypes() []imap.QuotaResourceType {
	return sess.user.quotaResourceTypes()
}

// GetQuota implements imapserver.SessionQuota.
func (sess *UserSession) GetQuota(root string) (*imapserver.QuotaData, error) {
	return sess.user.getQuota(root)
}

// GetQuotaRoot implements imapserver.SessionQuota.
func (sess *UserSession) GetQuotaRoot(mailbox string) ([]imapserver.QuotaData, error) {
	return sess.user.getQuotaRoot(mailbox)
}

var _ imapserver.SessionQuota = (*UserSession)(nil)
