package paseto

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// V4PublicKeySetKey is a single v4 public key together with its stable ID
// and the window of time during which it may be used to verify tokens.
type V4PublicKeySetKey struct {
	ID        string
	Key       V4AsymmetricPublicKey
	NotBefore time.Time
	NotAfter  time.Time
}

type v4PublicKeySetSnapshot struct {
	keys map[string]V4PublicKeySetKey
}

// V4PublicKeySet is a concurrency-safe set of v4 public keys, used to accept
// a small number of valid keys while keys are being rotated. The number of
// keys is bounded, and each parse is evaluated against a single snapshot of
// the set and a single "now".
type V4PublicKeySet struct {
	maxKeys int
	now     func() time.Time

	mu       sync.Mutex
	snapshot atomic.Pointer[v4PublicKeySetSnapshot]
}

// NewV4PublicKeySet creates an empty key set which holds at most maxKeys
// keys. maxKeys must be at least 1, and bounds the number of candidate keys
// tried during any single parse.
func NewV4PublicKeySet(maxKeys int) (*V4PublicKeySet, error) {
	if maxKeys < 1 {
		return nil, errorKeySetSizeInvalid
	}

	keySet := &V4PublicKeySet{maxKeys: maxKeys, now: time.Now}
	keySet.snapshot.Store(&v4PublicKeySetSnapshot{keys: map[string]V4PublicKeySetKey{}})

	return keySet, nil
}

func validateKeySetKey(key V4PublicKeySetKey) error {
	if key.ID == "" {
		return errorKeySetKeyIDEmpty
	}

	if !key.NotBefore.Before(key.NotAfter) {
		return errorKeySetInvalidWindow
	}

	return nil
}

// AddKey registers a key in the set. It errors if the ID is already present
// (key IDs must be stable and unique), if the set is full, or if the
// activation window is invalid.
func (s *V4PublicKeySet) AddKey(key V4PublicKeySetKey) error {
	if err := validateKeySetKey(key); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.snapshot.Load()
	if _, exists := current.keys[key.ID]; exists {
		return errorKeySetDuplicateID
	}

	if len(current.keys) >= s.maxKeys {
		return errorKeySetFull
	}

	next := make(map[string]V4PublicKeySetKey, len(current.keys)+1)
	for id, existing := range current.keys {
		next[id] = existing
	}
	next[key.ID] = key

	s.snapshot.Store(&v4PublicKeySetSnapshot{keys: next})

	return nil
}

// RemoveKey removes the key with the given ID from the set. Removing an
// unknown ID is a no-op.
func (s *V4PublicKeySet) RemoveKey(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.snapshot.Load()
	if _, exists := current.keys[id]; !exists {
		return
	}

	next := make(map[string]V4PublicKeySetKey, len(current.keys)-1)
	for existingID, existing := range current.keys {
		if existingID != id {
			next[existingID] = existing
		}
	}

	s.snapshot.Store(&v4PublicKeySetSnapshot{keys: next})
}

// ReplaceKeys atomically replaces the entire contents of the set. It errors
// (leaving the set unchanged) if any IDs are duplicated, if any activation
// window is invalid, or if more keys than the set's limit are given.
func (s *V4PublicKeySet) ReplaceKeys(keys ...V4PublicKeySetKey) error {
	if len(keys) > s.maxKeys {
		return errorKeySetFull
	}

	next := make(map[string]V4PublicKeySetKey, len(keys))
	for _, key := range keys {
		if err := validateKeySetKey(key); err != nil {
			return err
		}

		if _, exists := next[key.ID]; exists {
			return errorKeySetDuplicateID
		}

		next[key.ID] = key
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshot.Store(&v4PublicKeySetSnapshot{keys: next})

	return nil
}

type v4PublicKeySetCandidate struct {
	id  string
	key V4AsymmetricPublicKey
}

// footerKeyIDHint extracts a key ID routing hint from a token footer. The
// returned ID is UNTRUSTED: the footer is not yet cryptographically
// verified, so the hint may only be used to order candidate keys which are
// already present in the key set. It must never be used to fetch keys from
// anywhere else.
func footerKeyIDHint(footer []byte) (string, bool) {
	var parsed struct {
		KeyID string `json:"kid"`
	}

	if err := json.Unmarshal(footer, &parsed); err != nil || parsed.KeyID == "" {
		return "", false
	}

	return parsed.KeyID, true
}

// candidates returns the keys which are active at the current time, ordered
// so that a key matching the (untrusted) footer hint is tried first. The
// snapshot and the current time are each read exactly once, so a single
// parse is always evaluated against one version of the key set and one
// "now". The number of candidates is bounded by the set's key limit.
func (s *V4PublicKeySet) candidates(footer []byte) []v4PublicKeySetCandidate {
	snapshot := s.snapshot.Load()
	now := s.now()

	hint, hasHint := footerKeyIDHint(footer)

	candidates := make([]v4PublicKeySetCandidate, 0, len(snapshot.keys))
	for id, key := range snapshot.keys {
		if now.Before(key.NotBefore) || !now.Before(key.NotAfter) {
			continue
		}

		candidate := v4PublicKeySetCandidate{id, key.Key}
		if hasHint && id == hint {
			candidates = append([]v4PublicKeySetCandidate{candidate}, candidates...)
		} else {
			candidates = append(candidates, candidate)
		}
	}

	return candidates
}

// ParseV4PublicWithKeySet will parse and verify a v4 public paseto against
// the currently active keys in the given key set, and validate against any
// parser rules. Only the v4.public protocol is accepted; there is no
// fallback to any other protocol. On success the verified token and the ID
// of the key which actually verified it are returned. Error if parsing,
// verification with every candidate key, or any rule fails.
func (p Parser) ParseV4PublicWithKeySet(keySet *V4PublicKeySet, tainted string, implicit []byte) (*Token, string, error) {
	var msg message
	if err := newMessage(V4Public, tainted).Ok(&msg); err != nil {
		return nil, "", err
	}

	candidates := keySet.candidates(msg.footer)
	if len(candidates) == 0 {
		return nil, "", errorKeySetNoActiveKeys
	}

	for _, candidate := range candidates {
		var token Token
		if err := msg.v4Verify(candidate.key, implicit).Ok(&token); err != nil {
			continue
		}

		if err := p.validate(token).Ok(&token); err != nil {
			return nil, "", err
		}

		return &token, candidate.id, nil
	}

	return nil, "", errorKeySetNoKeyVerified
}
