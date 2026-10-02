package imapmemserver

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// AdminSession is an IMAP session with quota-administration privileges.
//
// It wraps a regular UserSession and additionally implements
// imapserver.SessionQuotaSet, which is what makes the server advertise the
// QUOTASET capability and accept SETQUOTA. A plain UserSession deliberately
// does not implement that interface, so ordinary users can neither see the
// capability nor change quotas.
type AdminSession struct {
	*UserSession
}

var _ imapserver.SessionQuotaSet = (*AdminSession)(nil)

// SetQuota implements imapserver.SessionQuotaSet.
func (sess *AdminSession) SetQuota(root string, limits map[imap.QuotaResourceType]int64) error {
	return sess.user.setQuota(root, limits)
}
