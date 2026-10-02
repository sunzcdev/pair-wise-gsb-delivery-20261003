package imapmemserver

import (
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

var errOverQuota = &imap.Error{
	Type: imap.StatusResponseTypeNo,
	Code: imap.ResponseCodeOverQuota,
	Text: "Quota exceeded",
}

// quotaRoot tracks the resource usage and limits of a quota root.
//
// A quota root may cover multiple mailboxes: all mailboxes pointing to the
// same quotaRoot share its usage counters and limits.
type quotaRoot struct {
	name string // immutable

	mutex         sync.Mutex
	limits        map[imap.QuotaResourceType]int64
	usageStorage  int64 // STORAGE resource usage, in bytes
	usageMessages int64 // MESSAGE resource usage, in number of messages
}

func newQuotaRoot(name string) *quotaRoot {
	return &quotaRoot{
		name:   name,
		limits: make(map[imap.QuotaResourceType]int64),
	}
}

// consume atomically checks the limits and reserves quota for new messages.
//
// The check and the reservation are performed while holding the lock, so
// concurrent operations on mailboxes covered by this quota root cannot
// over-commit. On success, the reservation must later be either committed
// (the messages have been stored) or rolled back with release.
func (q *quotaRoot) consume(storage, messages int64) error {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	if limit, ok := q.limits[imap.QuotaResourceStorage]; ok && q.usageStorage+storage > limit {
		return errOverQuota
	}
	if limit, ok := q.limits[imap.QuotaResourceMessage]; ok && q.usageMessages+messages > limit {
		return errOverQuota
	}

	q.usageStorage += storage
	q.usageMessages += messages
	return nil
}

// release frees previously consumed quota.
func (q *quotaRoot) release(storage, messages int64) {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	q.usageStorage -= storage
	q.usageMessages -= messages
	if q.usageStorage < 0 {
		q.usageStorage = 0
	}
	if q.usageMessages < 0 {
		q.usageMessages = 0
	}
}

func (q *quotaRoot) setLimits(limits map[imap.QuotaResourceType]int64) {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	for resourceType, limit := range limits {
		q.limits[resourceType] = limit
	}
}

func (q *quotaRoot) quotaData() *imapserver.QuotaData {
	q.mutex.Lock()
	defer q.mutex.Unlock()

	resources := make(map[imap.QuotaResourceType]*imapserver.QuotaResourceData)
	for resourceType, limit := range q.limits {
		resource := &imapserver.QuotaResourceData{Limit: limit}
		switch resourceType {
		case imap.QuotaResourceStorage:
			resource.Usage = q.usageStorage
		case imap.QuotaResourceMessage:
			resource.Usage = q.usageMessages
		}
		resources[resourceType] = resource
	}
	return &imapserver.QuotaData{
		Root:      q.name,
		Resources: resources,
	}
}

func (sess *UserSession) GetQuota(rootName string) (*imapserver.QuotaData, error) {
	root := sess.user.quotaRoot(rootName)
	if root == nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such quota root",
		}
	}
	return root.quotaData(), nil
}

func (sess *UserSession) GetQuotaRoot(mailbox string) (*imapserver.QuotaRootData, error) {
	mbox, err := sess.user.mailbox(mailbox)
	if err != nil {
		return nil, err
	}

	data := &imapserver.QuotaRootData{Mailbox: mailbox}
	if root := mbox.quotaRoot; root != nil {
		data.Roots = []string{root.name}
	}
	return data, nil
}

func (sess *UserSession) SetQuota(rootName string, resources map[imap.QuotaResourceType]int64) error {
	if !sess.user.admin {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNoPerm,
			Text: "Only administrators can set quotas",
		}
	}

	for resourceType, limit := range resources {
		switch resourceType {
		case imap.QuotaResourceStorage, imap.QuotaResourceMessage:
			// supported
		default:
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Text: "Unsupported quota resource: " + string(resourceType),
			}
		}
		if limit < 0 {
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Code: imap.ResponseCodeClientBug,
				Text: "Quota limit cannot be negative",
			}
		}
	}

	root := sess.user.quotaRoot(rootName)
	if root == nil {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such quota root",
		}
	}
	root.setLimits(resources)
	return nil
}

var _ imapserver.SessionQuotaSet = (*UserSession)(nil)
