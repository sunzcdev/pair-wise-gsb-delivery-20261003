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

	var msgs []*message
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		msgs = append(msgs, msg)
	})

	var size int64
	for _, msg := range msgs {
		size += int64(len(msg.buf))
	}

	// Reserve quota in the destination's quota root before copying. If the
	// reservation fails, the whole COPY fails and no quota is consumed.
	if root := dest.quotaRoot; root != nil && len(msgs) > 0 {
		if err := root.consume(size, int64(len(msgs))); err != nil {
			return nil, err
		}
	}

	var sourceUIDs, destUIDs imap.UIDSet
	for _, msg := range msgs {
		appendData := dest.copyMsg(msg)
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
	}

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

	var msgs []*message
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		msgs = append(msgs, msg)
	})

	var size int64
	for _, msg := range msgs {
		size += int64(len(msg.buf))
	}

	// When the source and destination mailboxes share the same quota root,
	// the move doesn't change the root's usage: don't count the messages
	// twice, not even transiently. Otherwise, reserve quota in the
	// destination's quota root before moving.
	srcRoot := sess.mailbox.quotaRoot
	destRoot := dest.quotaRoot
	sameRoot := srcRoot == destRoot
	if !sameRoot && destRoot != nil && len(msgs) > 0 {
		if err := destRoot.consume(size, int64(len(msgs))); err != nil {
			return err
		}
	}

	var sourceUIDs, destUIDs imap.UIDSet
	expunged := make(map[*message]struct{})
	for _, msg := range msgs {
		appendData := dest.copyMsg(msg)
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
		expunged[msg] = struct{}{}
	}

	sess.mailbox.mutex.Lock()
	seqNums := sess.mailbox.expungeLocked(expunged)
	sess.mailbox.mutex.Unlock()

	if !sameRoot && srcRoot != nil && len(msgs) > 0 {
		srcRoot.release(size, int64(len(msgs)))
	}

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
