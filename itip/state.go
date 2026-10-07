// Package itip merges iTIP scheduling messages (RFC 5546) into a server side
// calendar state.
//
// iTIP messages are allowed to arrive out of order: a reschedule may be
// delivered before the original invitation, an attendee reply may refer to a
// version that has already been superseded, and a cancel may race with a
// request. The Processor in this package takes already parsed iMIP/iTIP
// messages together with the authenticated sender identity supplied by the
// caller and folds them into a Store, returning an explicit Applied, Ignored
// or Conflict outcome for every VEVENT in the message.
//
// Authorization is never decided from the ORGANIZER property inside the ICS
// payload. The organizer of a calendar object is bound to the authenticated
// sender that first creates it, and a payload whose ORGANIZER property
// disagrees with that identity is rejected rather than trusted.
package itip

import (
	"sync"
	"time"

	ical "github.com/arran4/golang-ical"
)

// Revision identifies one organizer issued version of a component. SEQUENCE is
// the primary ordering key per RFC 5546 3.1.6, with DTSTAMP as the tie
// breaker. Digest captures the exact bytes of the component so that two
// revisions carrying the same SEQUENCE and DTSTAMP are detectable.
type Revision struct {
	Sequence int
	DtStamp  time.Time
	Digest   string
}

// Compare reports whether rev is older than (-1), equal to (0) or newer than
// (1) other, comparing SEQUENCE first and DTSTAMP second. Digest is not part
// of the ordering.
func (rev Revision) Compare(other Revision) int {
	switch {
	case rev.Sequence < other.Sequence:
		return -1
	case rev.Sequence > other.Sequence:
		return 1
	case rev.DtStamp.Before(other.DtStamp):
		return -1
	case rev.DtStamp.After(other.DtStamp):
		return 1
	default:
		return 0
	}
}

// ReplyState records the latest REPLY accepted from a single attendee for one
// component. It is keyed by the normalized attendee email address.
type ReplyState struct {
	Sequence int
	DtStamp  time.Time
	Status   ical.ParticipationStatus
	Digest   string
}

// Compare orders two replies the same way as Revision.Compare.
func (rs ReplyState) Compare(other ReplyState) int {
	switch {
	case rs.Sequence < other.Sequence:
		return -1
	case rs.Sequence > other.Sequence:
		return 1
	case rs.DtStamp.Before(other.DtStamp):
		return -1
	case rs.DtStamp.After(other.DtStamp):
		return 1
	default:
		return 0
	}
}

// ComponentState is the merged server state of either the master VEVENT or a
// single recurring instance identified by RECURRENCE-ID.
type ComponentState struct {
	// RecurrenceID is the parsed RECURRENCE-ID; it is the zero time for the
	// master component.
	RecurrenceID time.Time

	// Event is the current organizer description of the component. It is
	// owned by the ComponentState and mutated in place when replies update
	// PARTSTAT.
	Event *ical.VEvent

	// Revision is the organizer revision (REQUEST or CANCEL) currently held.
	Revision Revision

	// Cancelled reports that the latest organizer revision cancelled the
	// component.
	Cancelled bool

	// Replies holds the newest accepted reply per normalized attendee email.
	Replies map[string]ReplyState
}

// Object is all server held state sharing one UID: the master component plus
// per-instance overrides keyed by their RECURRENCE-ID instant.
type Object struct {
	// UID is the iCalendar UID shared by every component.
	UID string

	// Organizer is the normalized email of the authenticated sender that
	// created the object. It is the source of truth for authorization; the
	// ORGANIZER property in incoming ICS is only cross checked against it.
	Organizer string

	// Master is the state of the component without RECURRENCE-ID, which may
	// be nil while the first message is in flight.
	Master *ComponentState

	// Instances holds overrides keyed by their parsed RECURRENCE-ID.
	Instances map[time.Time]*ComponentState
}

func newObject(uid, organizer string) *Object {
	return &Object{
		UID:       uid,
		Organizer: organizer,
		Instances: map[time.Time]*ComponentState{},
	}
}

// Store persists merged objects. Implementations are expected to provide
// their own transactional guarantees; the MemoryStore implementation in this
// package is safe for concurrent use.
type Store interface {
	// Get returns the object for uid and true when it exists.
	Get(uid string) (*Object, bool)

	// Put stores obj under its UID.
	Put(obj *Object)
}

// MemoryStore is an in-memory Store.
type MemoryStore struct {
	mu      sync.RWMutex
	objects map[string]*Object
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{objects: map[string]*Object{}}
}

// Get returns a copy-independent pointer to the stored object. The pointer
// remains valid until the next Put or Delete for the same UID.
func (s *MemoryStore) Get(uid string) (*Object, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[uid]
	return obj, ok
}

// Put stores obj.
func (s *MemoryStore) Put(obj *Object) {
	if obj == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[obj.UID] = obj
}

// Delete removes uid. It is primarily useful for tests.
func (s *MemoryStore) Delete(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, uid)
}

// UIDs returns a snapshot of stored UIDs.
func (s *MemoryStore) UIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uids := make([]string, 0, len(s.objects))
	for uid := range s.objects {
		uids = append(uids, uid)
	}
	return uids
}
