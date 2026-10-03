package paseto

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

// maxV4KeySetCandidates bounds the number of public keys a single parse may
// attempt. A small set keeps verification cheap during rotation; it is not a
// means to accept an unbounded number of keys.
const maxV4KeySetCandidates = 8

// V4KeyWindow is the half-open time interval [NotBefore, Expires) during which
// a key is considered enabled. A zero time means unbounded on that side: the
// zero value therefore enables a key forever.
type V4KeyWindow struct {
	NotBefore time.Time
	Expires   time.Time
}

// activeAt reports whether the window is enabled at the fixed instant now.
func (w V4KeyWindow) activeAt(now time.Time) bool {
	if !w.NotBefore.IsZero() && now.Before(w.NotBefore) {
		return false
	}
	if !w.Expires.IsZero() && !now.Before(w.Expires) {
		return false
	}
	return true
}

func (w V4KeyWindow) valid() bool {
	return w.Expires.IsZero() || w.NotBefore.IsZero() || w.Expires.After(w.NotBefore)
}

// V4KeyEntry pairs a v4.public verification key with its stable identifier and
// enable/disable window.
type V4KeyEntry struct {
	ID     string
	Key    V4AsymmetricPublicKey
	Window V4KeyWindow
}

// V4PublicKeySet is a small, updatable set of v4.public verification keys used
// to accept tokens across a key rotation. The zero value is an empty, ready to
// use set.
//
// A V4PublicKeySet is safe for concurrent use: verification snapshots the set,
// while Add and Remove mutate it.
type V4PublicKeySet struct {
	mu      sync.RWMutex
	entries []V4KeyEntry
}

// NewV4PublicKeySet constructs a key set from the given entries. It rejects an
// empty ID, an empty window (Expires at or before NotBefore), a duplicate ID,
// and any set larger than the candidate limit.
func NewV4PublicKeySet(entries []V4KeyEntry) (*V4PublicKeySet, error) {
	set := &V4PublicKeySet{}
	for _, entry := range entries {
		if err := set.Add(entry); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// Add inserts an entry, rejecting an empty ID, an empty window, a duplicate ID,
// or a set that would exceed the candidate limit.
func (s *V4PublicKeySet) Add(entry V4KeyEntry) error {
	if entry.ID == "" {
		return errorKeySetEmptyID
	}
	if !entry.Window.valid() {
		return errorKeySetEmptyWindow(entry.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) >= maxV4KeySetCandidates {
		return errorKeySetTooMany(maxV4KeySetCandidates)
	}
	for _, existing := range s.entries {
		if existing.ID == entry.ID {
			return errorKeySetDuplicateID(entry.ID)
		}
	}

	s.entries = append(s.entries, entry)
	return nil
}

// Remove deletes the entry with the given ID, reporting whether an entry was
// removed.
func (s *V4PublicKeySet) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, entry := range s.entries {
		if entry.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return true
		}
	}
	return false
}

// Len reports the number of entries currently held (enabled or not).
func (s *V4PublicKeySet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.entries)
}

// activeEntriesAt returns a copy of the entries enabled at now, so a single
// parse always reasons about one version of the set and one instant.
func (s *V4PublicKeySet) activeEntriesAt(now time.Time) []V4KeyEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	active := make([]V4KeyEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		if entry.Window.activeAt(now) {
			active = append(active, entry)
		}
	}
	return active
}

// ParseV4PublicWithKeySet parses and verifies a v4.public token against the
// currently enabled keys in the set, and returns the ID of the key that
// actually verified the signature.
//
// The footer is not cryptographically verified until the signature checks out,
// so any key ID carried in it is treated solely as an untrusted routing hint.
// An unknown hint neither triggers any external key fetch nor changes the
// protocol attempted: only keys already in the set and enabled at the single
// "now" of this parse are candidates.
func (p Parser) ParseV4PublicWithKeySet(
	set *V4PublicKeySet,
	tainted string,
	implicit []byte,
) (*Token, string, error) {
	now := time.Now()

	// Reject anything that is not v4.public up front. There is no fallback to
	// any other version or purpose.
	var msg message
	if err := newMessage(V4Public, tainted).Ok(&msg); err != nil {
		return nil, "", err
	}

	candidates := set.activeEntriesAt(now)
	if len(candidates) == 0 {
		return nil, "", errorNoActiveKeys
	}

	orderCandidatesByHint(candidates, footerKeyIDHint(msg))

	for _, entry := range candidates {
		verified := msg.v4Verify(entry.Key, implicit)
		var token Token
		if err := verified.Ok(&token); err != nil {
			continue
		}

		if err := p.validate(token).Err(); err != nil {
			return &token, entry.ID, err
		}

		return &token, entry.ID, nil
	}

	return nil, "", errorNoKeyVerified
}

// orderCandidatesByHint moves an exact hint match to the front. The hint can
// only order already-accepted candidates; it can never add a key.
func orderCandidatesByHint(candidates []V4KeyEntry, hint string) {
	if hint == "" {
		return
	}
	for i := range candidates {
		if candidates[i].ID == hint {
			candidates[0], candidates[i] = candidates[i], candidates[0]
			return
		}
	}
}

// footerKeyIDHint extracts a "kid" string from a JSON object footer. The footer
// is attacker-controlled until verified, so any malformed or non-string result
// yields no hint rather than an error.
func footerKeyIDHint(msg message) string {
	var parsed struct {
		KeyID string `json:"kid"`
	}
	decoder := json.NewDecoder(bytes.NewReader(msg.footer))
	if err := decoder.Decode(&parsed); err != nil {
		return ""
	}
	return parsed.KeyID
}
