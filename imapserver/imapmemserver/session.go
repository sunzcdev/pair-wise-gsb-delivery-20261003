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

	notify *notifyState
}

var (
	_ imapserver.SessionIMAP4rev2 = (*UserSession)(nil)
	_ imapserver.SessionNotify    = (*UserSession)(nil)
)

// NewUserSession creates a new user session.
func NewUserSession(user *User) *UserSession {
	return &UserSession{user: user, notify: newNotifyState(user)}
}

func (sess *UserSession) Close() error {
	if sess != nil && sess.mailbox != nil {
		sess.mailbox.Close()
	}
	if sess != nil && sess.notify != nil {
		sess.notify.close()
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
	sess.mailbox = mbox.NewView(sess.notify)
	return mbox.selectDataLocked(), nil
}

func (sess *UserSession) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	mbox, err := sess.user.mailbox(mailbox)
	if err != nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	}

	var source *MailboxView
	if sess.mailbox != nil && sess.mailbox.Mailbox == mbox {
		source = sess.mailbox
	}
	return mbox.appendLiteral(r, options, source)
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

	var sourceUIDs, destUIDs imap.UIDSet
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		appendData := dest.copyMsg(msg, sess.mailbox)
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
	})

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

	// Gather the messages to move while holding the source lock, then perform
	// the destination append and source expunge without nesting the two
	// mailbox locks.
	var moved []*message
	sess.mailbox.mutex.Lock()
	sess.mailbox.forEachLocked(numSet, func(seqNum uint32, msg *message) {
		moved = append(moved, msg)
	})
	sess.mailbox.mutex.Unlock()

	var sourceUIDs, destUIDs imap.UIDSet
	for _, msg := range moved {
		appendData := dest.copyMsg(msg, sess.mailbox)
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
	}

	expunged := make(map[*message]struct{}, len(moved))
	for _, msg := range moved {
		expunged[msg] = struct{}{}
	}

	sess.mailbox.mutex.Lock()
	seqNums := sess.mailbox.expungeLocked(expunged, sess.mailbox)
	sess.mailbox.mutex.Unlock()

	sess.user.notifyChange(sess.mailbox.Mailbox, sess.mailbox, imap.NotifyEventMessageExpunge, 0, nil)

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
	if sess.mailbox != nil {
		if err := sess.mailbox.Poll(w, allowExpunge); err != nil {
			return err
		}
	}
	return nil
}

func (sess *UserSession) NotifyPoll(nw *imapserver.NotifyWriter, w *imapserver.UpdateWriter, allowExpunge bool) error {
	if sess.mailbox != nil {
		if err := sess.mailbox.Poll(w, allowExpunge); err != nil {
			return err
		}
	}
	return sess.notify.drain(nw)
}

func (sess *UserSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if sess.mailbox == nil {
		return nil // TODO
	}
	return sess.mailbox.Idle(w, stop)
}

func (sess *UserSession) NotifyIdle(nw *imapserver.NotifyWriter, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	wake := make(chan struct{}, 64)
	sess.notify.setIdleWake(wake)
	defer sess.notify.setIdleWake(nil)

	// Flush events which arrived before entering IDLE.
	if err := sess.notify.drain(nw); err != nil {
		return err
	}

	if sess.mailbox == nil {
		for {
			select {
			case <-wake:
				if err := sess.notify.drain(nw); err != nil {
					return err
				}
			case <-stop:
				return nil
			}
		}
	}

	// The mailbox tracker manages its own wakeup; run its Idle loop in a
	// goroutine and multiplex it with the NOTIFY wakeup channel.
	done := make(chan error, 1)
	go func() {
		done <- sess.mailbox.Idle(w, stop)
	}()

	for {
		select {
		case <-wake:
			if err := sess.notify.drain(nw); err != nil {
				return err
			}
		case <-stop:
			return <-done
		case err := <-done:
			return err
		}
	}
}
