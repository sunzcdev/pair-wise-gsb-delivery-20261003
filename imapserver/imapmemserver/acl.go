package imapmemserver

import "github.com/emersion/go-imap/v2"

// allRights is the full set of rights supported by the in-memory server.
var allRights = imap.RightSet("lrswipcdak")

// initACL marks the mailbox as owned by the provided user and grants the
// owner the full set of rights.
func (mbox *Mailbox) initACL(owner string) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	mbox.owner = owner
	mbox.acl = map[imap.RightsIdentifier]imap.RightSet{
		imap.RightsIdentifier(owner): copyRights(allRights),
	}
}

// Copy is an unexported helper to duplicate a right set.
func copyRights(rs imap.RightSet) imap.RightSet {
	newRights := make(imap.RightSet, len(rs))
	copy(newRights, rs)
	return newRights
}

// effectiveRightsLocked returns the rights the given user has on the mailbox.
//
// The caller must hold mbox.mutex.
//
// Rights are computed per RFC 4314 section 5.1: the "anyone" and user
// identifiers are combined, while the negative identifiers ("-anyone" and
// "-user") are subtracted. The mailbox owner always holds the full set of
// rights.
func (mbox *Mailbox) effectiveRightsLocked(username string) imap.RightSet {
	if username != "" && username == mbox.owner {
		// Rights that have been explicitly changed via SETACL are reported
		// as stored; the owner's ability to administer the mailbox is
		// handled separately in the ACL commands.
		if stored, ok := mbox.acl[imap.RightsIdentifier(username)]; ok {
			return copyRights(stored)
		}
		return copyRights(allRights)
	}

	rights := imap.RightSet{}
	rights = rights.Add(mbox.acl[imap.RightsIdentifierAnyone])
	if username != "" {
		rights = rights.Add(mbox.acl[imap.RightsIdentifier(username)])
	}

	rights = rights.Remove(mbox.acl[imap.RightsIdentifier("-"+imap.RightsIdentifierAnyone)])
	if username != "" {
		rights = rights.Remove(mbox.acl[imap.RightsIdentifier("-"+imap.RightsIdentifier(username))])
	}

	return rights
}

func (mbox *Mailbox) myRights(username string) (imap.RightSet, error) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	// Don't reveal the existence of mailboxes which are invisible to the
	// user (RFC 4314 section 5.2).
	if username != mbox.owner && !mbox.effectiveRightsLocked(username).Has(imap.RightLookup) {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}

	return mbox.effectiveRightsLocked(username), nil
}

func (mbox *Mailbox) getACLData(username string) (*imap.ACLData, error) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	rights := mbox.effectiveRightsLocked(username)
	if username != mbox.owner && !rights.Has(imap.RightLookup) {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}

	acl := make(map[imap.RightsIdentifier]imap.RightSet, len(mbox.acl))
	for ri, rs := range mbox.acl {
		acl[ri] = copyRights(rs)
	}
	return &imap.ACLData{
		Mailbox: mbox.name,
		Rights:  acl,
	}, nil
}

func (mbox *Mailbox) listRights(username string, ri imap.RightsIdentifier) (*imap.ListRightsData, error) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	rights := mbox.effectiveRightsLocked(username)
	if username != mbox.owner && !rights.Has(imap.RightLookup) {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}

	return &imap.ListRightsData{
		Mailbox:    mbox.name,
		Identifier: ri,
		Required:   imap.RightSet{},
		Optional:   []imap.RightSet{copyRights(allRights)},
	}, nil
}

func (mbox *Mailbox) setACL(username string, ri imap.RightsIdentifier, mod imap.RightModification, rs imap.RightSet) error {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	rights := mbox.effectiveRightsLocked(username)
	if username != mbox.owner && !rights.Has(imap.RightLookup) {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	if username != mbox.owner && !rights.Has(imap.RightAdminister) {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNoPerm,
			Text: "Permission denied",
		}
	}

	current := copyRights(mbox.acl[ri])
	switch mod {
	case imap.RightModificationReplace:
		current = copyRights(rs)
	case imap.RightModificationAdd:
		current = current.Add(rs)
	case imap.RightModificationRemove:
		current = current.Remove(rs)
	}

	if len(current) == 0 {
		delete(mbox.acl, ri)
	} else {
		mbox.acl[ri] = current
	}
	return nil
}

func (mbox *Mailbox) deleteACL(username string, ri imap.RightsIdentifier) error {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	rights := mbox.effectiveRightsLocked(username)
	if username != mbox.owner && !rights.Has(imap.RightLookup) {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	if username != mbox.owner && !rights.Has(imap.RightAdminister) {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNoPerm,
			Text: "Permission denied",
		}
	}

	if _, ok := mbox.acl[ri]; !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such ACL identifier",
		}
	}

	delete(mbox.acl, ri)
	return nil
}

// visibleLocked reports whether the mailbox is visible (has the "l" right) to
// the user.
//
// The caller must hold mbox.mutex.
func (mbox *Mailbox) visibleLocked(username string) bool {
	rights := mbox.effectiveRightsLocked(username)
	return rights.Has(imap.RightLookup)
}
