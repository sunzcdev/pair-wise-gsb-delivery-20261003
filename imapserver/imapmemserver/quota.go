package imapmemserver

import (
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// DefaultQuotaRoot is the name of the quota root which, in the in-memory
// backend, covers every mailbox of a user.
//
// A single quota root spanning multiple mailboxes makes the sharing
// semantics concrete: two different mailboxes draw from the same pool of
// resources.
const DefaultQuotaRoot = ""

// storageUnit is the size, in octets, of one STORAGE unit.
//
// RFC 9208 section 5 defines the STORAGE resource unit as 1024 octets.
const storageUnit = 1024

// quotaResourceDelta is the amount of resources consumed (positive) or
// released (negative) by an operation.
type quotaResourceDelta struct {
	messages int64
	storage  int64 // in STORAGE units (kibibytes)
}

// quotaRoot holds the limits and the running usage for one quota root.
//
// All fields are protected by the mutex. The mutex is the single place where
// quota availability is decided: check and reservation happen atomically, so
// two concurrent sessions operating on different mailboxes sharing the root
// cannot both observe a stale balance and overshoot it.
type quotaRoot struct {
	mutex sync.Mutex
	usage quotaResourceDelta
	limit quotaResourceDelta
}

// overQuotaError rejects an operation which would exceed a resource limit.
func overQuotaError() error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeOverQuota,
		Text: "Quota exceeded",
	}
}

// storageUnits converts a message size in octets to STORAGE units, rounding
// up: RFC 9208 measures storage in units of 1024 octets and a message always
// consumes at least one unit.
func storageUnits(size int64) int64 {
	return (size + storageUnit - 1) / storageUnit
}

// reserve atomically checks that the delta fits and, if so, accounts for it.
//
// On failure the usage is left untouched. A limit of 0 means the resource is
// not limited.
func (root *quotaRoot) reserve(delta quotaResourceDelta) error {
	root.mutex.Lock()
	defer root.mutex.Unlock()

	if !root.fitsLocked(root.usage.messages+delta.messages, root.limit.messages) {
		return overQuotaError()
	}
	if !root.fitsLocked(root.usage.storage+delta.storage, root.limit.storage) {
		return overQuotaError()
	}

	root.usage.messages += delta.messages
	root.usage.storage += delta.storage
	return nil
}

// release reverses a previous reservation.
func (root *quotaRoot) release(delta quotaResourceDelta) {
	root.mutex.Lock()
	root.usage.messages -= delta.messages
	root.usage.storage -= delta.storage
	root.mutex.Unlock()
}

// move atomically relocates a delta from src to dst.
//
// When src and dst are the same root, the usage does not change: only the
// configured limits are checked, so a move within a root never transiently
// counts the messages twice and never fails merely because the root is full.
//
// For distinct roots the destination balance is checked and updated
// atomically. The source share is released by the caller only once the
// operation reaches its success boundary (see moveCommit); move reserves the
// destination share here.
func moveReserve(src, dst *quotaRoot, delta quotaResourceDelta) error {
	if src == dst {
		src.mutex.Lock()
		defer src.mutex.Unlock()
		if !src.fitsLocked(src.usage.messages, src.limit.messages) ||
			!src.fitsLocked(src.usage.storage, src.limit.storage) {
			return overQuotaError()
		}
		return nil
	}
	return dst.reserve(delta)
}

// moveCommit releases the source share after a cross-root move has succeeded.
// It is a no-op for same-root moves.
func moveCommit(src, dst *quotaRoot, delta quotaResourceDelta) {
	if src != dst {
		src.release(delta)
	}
}

// moveRollback undoes a moveReserve when the operation failed before commit.
func moveRollback(src, dst *quotaRoot, delta quotaResourceDelta) {
	if src != dst {
		dst.release(delta)
	}
}

func (*quotaRoot) fitsLocked(value, limit int64) bool {
	return limit == 0 || value <= limit
}

// snapshot returns the current usage and limits for all tracked resources.
//
// Resources without a configured limit are reported with limit 0
// (unlimited), as required by RFC 9208.
func (root *quotaRoot) snapshot() map[imap.QuotaResourceType]imapserver.QuotaResourceData {
	root.mutex.Lock()
	defer root.mutex.Unlock()

	return map[imap.QuotaResourceType]imapserver.QuotaResourceData{
		imap.QuotaResourceStorage: {
			Usage: root.usage.storage,
			Limit: root.limit.storage,
		},
		imap.QuotaResourceMessage: {
			Usage: root.usage.messages,
			Limit: root.limit.messages,
		},
	}
}

// setLimits replaces the configured resource limits.
func (root *quotaRoot) setLimits(limits map[imap.QuotaResourceType]int64) {
	root.mutex.Lock()
	defer root.mutex.Unlock()

	if storage, ok := limits[imap.QuotaResourceStorage]; ok {
		root.limit.storage = storage
	}
	if messages, ok := limits[imap.QuotaResourceMessage]; ok {
		root.limit.messages = messages
	}
}
