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

var (
	_ imapserver.SessionIMAP4rev2 = (*UserSession)(nil)
	_ imapserver.SessionACL       = (*UserSession)(nil)
)

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

	var sourceUIDs, destUIDs imap.UIDSet
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		appendData := dest.copyMsg(msg)
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

	sess.mailbox.mutex.Lock()
	defer sess.mailbox.mutex.Unlock()

	var sourceUIDs, destUIDs imap.UIDSet
	expunged := make(map[*message]struct{})
	sess.mailbox.forEachLocked(numSet, func(seqNum uint32, msg *message) {
		appendData := dest.copyMsg(msg)
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
		expunged[msg] = struct{}{}
	})
	seqNums := sess.mailbox.expungeLocked(expunged)

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

func (sess *UserSession) resolveMailbox(name string) (*Mailbox, error) {
	return sess.user.mailbox(name)
}

// MyRights implements imapserver.SessionACL.
func (sess *UserSession) MyRights(mailbox string) (imap.RightSet, error) {
	mbox, err := sess.resolveMailbox(mailbox)
	if err != nil {
		return nil, err
	}
	return mbox.myRights(sess.user.username)
}

// SetACL implements imapserver.SessionACL.
func (sess *UserSession) SetACL(mailbox string, ri imap.RightsIdentifier, rm imap.RightModification, rs imap.RightSet) error {
	mbox, err := sess.resolveMailbox(mailbox)
	if err != nil {
		return err
	}
	return mbox.setACL(sess.user.username, ri, rm, rs)
}

// DeleteACL implements imapserver.SessionACL.
func (sess *UserSession) DeleteACL(mailbox string, ri imap.RightsIdentifier) error {
	mbox, err := sess.resolveMailbox(mailbox)
	if err != nil {
		return err
	}
	return mbox.deleteACL(sess.user.username, ri)
}

// GetACL implements imapserver.SessionACL.
func (sess *UserSession) GetACL(mailbox string) (*imap.ACLData, error) {
	mbox, err := sess.resolveMailbox(mailbox)
	if err != nil {
		return nil, err
	}
	return mbox.getACLData(sess.user.username)
}

// ListRights implements imapserver.SessionACL.
func (sess *UserSession) ListRights(mailbox string, ri imap.RightsIdentifier) (*imap.ListRightsData, error) {
	mbox, err := sess.resolveMailbox(mailbox)
	if err != nil {
		return nil, err
	}
	return mbox.listRights(sess.user.username, ri)
}
