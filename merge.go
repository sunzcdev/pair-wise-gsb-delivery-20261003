package scs

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ConflictError is returned by SessionManager.Commit when the session store
// implements VersionedStore and the changes made during the current request
// cycle conflict with changes made by another concurrent request: both
// requests modified the same key in different ways relative to the baseline
// that was loaded from the store. The conflicting keys are listed in the
// Keys field, and the conflicting changes are not persisted. It is the
// application's responsibility to decide how to handle the conflict (e.g.
// retry the operation, or inform the user).
type ConflictError struct {
	Keys []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("scs: session commit conflict on key(s): %s", strings.Join(e.Keys, ", "))
}

// ErrConcurrentModification is returned by SessionManager.Commit when the
// session data keeps being concurrently modified by other requests and the
// merge could not be completed within a bounded number of attempts.
var ErrConcurrentModification = errors.New("scs: session commit aborted due to repeated concurrent modifications")

// maxMergeAttempts bounds how many times a commit will be retried when the
// stored session data changes between the versioned find and the
// compare-and-swap commit.
const maxMergeAttempts = 3

// commitWithMerge commits the session data using a three-way merge between
// the baseline loaded from the store, the data currently in the store, and
// the changes made during this request cycle. It must only be called when
// the store implements VersionedStore, and sd.mu must be held.
func (s *SessionManager) commitWithMerge(sd *sessionData) error {
	vs := s.Store.(VersionedStore)

	token := sd.token
	if s.HashTokenInStore {
		token = hashToken(token)
	}

	for attempt := 0; attempt < maxMergeAttempts; attempt++ {
		b, version, found, err := vs.FindVersioned(token)
		if err != nil {
			return err
		}

		var current map[string]interface{}
		if found {
			_, current, err = s.Codec.Decode(b)
			if err != nil {
				return err
			}
		} else if sd.loaded {
			// The session existed in the store when it was loaded at the
			// start of this request cycle, but has since been removed (e.g.
			// destroyed by another concurrent request, or expired). Discard
			// the changes instead of recreating a destroyed session.
			return nil
		}

		merged, conflicts := threeWayMerge(sd.baseline, current, sd.values)
		if len(conflicts) > 0 {
			return &ConflictError{Keys: conflicts}
		}

		enc, err := s.Codec.Encode(sd.deadline, merged)
		if err != nil {
			return err
		}

		committed, err := vs.CommitVersioned(token, enc, sd.expiry, version)
		if err != nil {
			return err
		}
		if !committed {
			// Another request committed to (or deleted) the session between
			// our find and commit. Retry the merge against the newer state.
			continue
		}

		// Reflect the merged state (which may include changes made by other
		// requests) in the in-memory session data, and use it as the new
		// baseline for any subsequent commits in this request cycle.
		sd.values = merged
		sd.baseline = cloneValues(merged)
		sd.loaded = true
		return nil
	}

	return ErrConcurrentModification
}

// threeWayMerge merges the local changes and the remote (currently stored)
// changes relative to their common baseline, on a per-key basis. A key
// changed by only one side takes that side's value; a key changed by both
// sides in the same way takes that value; a key changed by both sides in
// different ways (including one side deleting it) is reported as a conflict
// and excluded from the merged result.
func threeWayMerge(baseline, current, local map[string]interface{}) (merged map[string]interface{}, conflicts []string) {
	keys := make(map[string]struct{})
	for key := range baseline {
		keys[key] = struct{}{}
	}
	for key := range current {
		keys[key] = struct{}{}
	}
	for key := range local {
		keys[key] = struct{}{}
	}

	merged = make(map[string]interface{}, len(keys))

	for key := range keys {
		baseVal, baseOK := baseline[key]
		curVal, curOK := current[key]
		localVal, localOK := local[key]

		localChanged := !fieldEqual(baseVal, baseOK, localVal, localOK)
		remoteChanged := !fieldEqual(baseVal, baseOK, curVal, curOK)

		switch {
		case localChanged && remoteChanged:
			if fieldEqual(curVal, curOK, localVal, localOK) {
				// Both sides made the same change; not a conflict.
				if localOK {
					merged[key] = localVal
				}
			} else {
				conflicts = append(conflicts, key)
			}
		case localChanged:
			if localOK {
				merged[key] = localVal
			}
		case remoteChanged:
			if curOK {
				merged[key] = curVal
			}
		default:
			if baseOK {
				merged[key] = baseVal
			}
		}
	}

	sort.Strings(conflicts)
	return merged, conflicts
}

func fieldEqual(baseVal interface{}, baseOK bool, val interface{}, ok bool) bool {
	if baseOK != ok {
		return false
	}
	if !baseOK {
		return true
	}
	return reflect.DeepEqual(baseVal, val)
}

func cloneValues(values map[string]interface{}) map[string]interface{} {
	clone := make(map[string]interface{}, len(values))
	for key, val := range values {
		clone[key] = val
	}
	return clone
}
