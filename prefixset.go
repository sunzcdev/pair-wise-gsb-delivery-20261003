package geohash

import "strings"

// MaxPrefixLength is the maximum number of characters in a geohash prefix.
// Each character encodes 5 bits, and a geohash holds at most 64 bits.
const MaxPrefixLength = 12

// PrefixSet is a set of geohash prefixes maintained in canonical form.
//
// A prefix stands for the whole region of the geohash grid beneath it; the
// empty prefix represents the entire globe. The set never stores a prefix
// together with any of its descendants, and whenever all 32 character-level
// children of a prefix are present they are folded into the prefix itself.
// Consequently every region covered by the set has a unique representation,
// independent of the order prefixes were added.
type PrefixSet struct {
	prefixes map[string]struct{}
}

// NewPrefixSet returns an empty PrefixSet.
func NewPrefixSet() *PrefixSet {
	return &PrefixSet{prefixes: make(map[string]struct{})}
}

// Contains reports whether prefix is a member of the canonical set.
func (s *PrefixSet) Contains(prefix string) bool {
	_, ok := s.prefixes[prefix]
	return ok
}

// Len returns the number of prefixes in the canonical set.
func (s *PrefixSet) Len() int {
	return len(s.prefixes)
}

// Prefixes returns the members of the set as a slice. The order of the
// returned slice is not guaranteed.
func (s *PrefixSet) Prefixes() []string {
	out := make([]string, 0, len(s.prefixes))
	for prefix := range s.prefixes {
		out = append(out, prefix)
	}
	return out
}

// Add inserts a prefix region into the set. Any descendants of prefix already
// in the set become redundant and are removed; when the insertion completes a
// full set of 32 children at any level, that level folds into its parent.
//
// An invalid prefix (illegal character or more than MaxPrefixLength
// characters) is rejected and the set is left unchanged.
func (s *PrefixSet) Add(prefix string) error {
	if err := Validate(prefix); err != nil {
		return err
	}
	s.add(prefix)
	return nil
}

// add inserts an already-validated prefix.
func (s *PrefixSet) add(prefix string) {
	// Nothing to do if an ancestor already covers this region.
	if _, ok := s.ancestor(prefix); ok {
		return
	}

	// Remove any descendants now covered by prefix.
	for p := range s.prefixes {
		if strings.HasPrefix(p, prefix) && p != prefix {
			delete(s.prefixes, p)
		}
	}

	s.prefixes[prefix] = struct{}{}

	// Collapse complete levels, walking back up toward the root.
	for len(prefix) > 0 {
		parent := prefix[:len(prefix)-1]
		if !s.childrenComplete(parent) {
			break
		}
		for i := 0; i < len(base32encoding.encode); i++ {
			delete(s.prefixes, parent+string(base32encoding.encode[i]))
		}
		s.prefixes[parent] = struct{}{}
		prefix = parent
	}
}

// childrenComplete reports whether all 32 one-character children of parent
// are members of the set.
func (s *PrefixSet) childrenComplete(parent string) bool {
	for i := 0; i < len(base32encoding.encode); i++ {
		if _, ok := s.prefixes[parent+string(base32encoding.encode[i])]; !ok {
			return false
		}
	}
	return true
}

// ancestor returns a member of the set that is an ancestor (inclusive) of
// prefix.
func (s *PrefixSet) ancestor(prefix string) (string, bool) {
	if _, ok := s.prefixes[prefix]; ok {
		return prefix, true
	}
	for i := len(prefix) - 1; i >= 0; i-- {
		if _, ok := s.prefixes[prefix[:i]]; ok {
			return prefix[:i], true
		}
	}
	return "", false
}

// Covers reports whether the point identified by hash lies within a region
// represented in the set.
func (s *PrefixSet) Covers(hash string) bool {
	_, ok := s.ancestor(hash)
	return ok
}

// Subtract removes the region covered by prefix from the set. A stored prefix
// strictly containing prefix is expanded, one level at a time, only as deep as
// prefix itself: the branch toward prefix is followed while the 31 sibling
// branches are retained, so expansion never runs beyond the subtracted
// prefix's precision.
//
// An invalid prefix (illegal character or more than MaxPrefixLength
// characters) is rejected and the set is left unchanged.
func (s *PrefixSet) Subtract(prefix string) error {
	if err := Validate(prefix); err != nil {
		return err
	}

	// Find the covering ancestor, if any, and expand the path toward prefix.
	ancestor, ok := s.ancestor(prefix)
	if !ok {
		return nil
	}

	// Remove the ancestor along with any members inside the removed region.
	delete(s.prefixes, ancestor)
	for p := range s.prefixes {
		if p == prefix || strings.HasPrefix(p, prefix) {
			delete(s.prefixes, p)
		}
	}

	// At each depth retain the 31 siblings of the branch leading to prefix.
	for depth := len(ancestor); depth < len(prefix); depth++ {
		branch := prefix[depth]
		parent := prefix[:depth]
		for i := 0; i < len(base32encoding.encode); i++ {
			c := base32encoding.encode[i]
			if c == branch {
				continue
			}
			s.prefixes[parent+string(c)] = struct{}{}
		}
	}

	return nil
}
