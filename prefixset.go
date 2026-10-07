package geohash

import (
	"sort"
	"strings"
)

// PrefixSet is a set of geohash prefixes kept in canonical form. Every region
// it covers is represented exactly once: inserting a prefix removes any
// descendants it contains, and whenever all 32 character-level children of a
// prefix are present they are folded back up into the parent.
//
// A prefix is a valid geohash string of at most 12 characters; the empty
// prefix represents the whole globe. Because each geohash character encodes
// 5 bits, a node has up to 32 children, never 2.
//
// PrefixSet is not safe for concurrent use.
type PrefixSet struct {
	set map[string]struct{}
}

// NewPrefixSet returns an empty PrefixSet.
func NewPrefixSet() *PrefixSet {
	return &PrefixSet{set: make(map[string]struct{})}
}

// Add inserts prefix and normalizes the set. Descendants already covered by
// prefix become redundant and are removed; the set folds upwards whenever 32
// siblings become present. Adding a prefix the set already covers is a
// no-op. An invalid prefix (illegal character or more than 12 characters) is
// rejected without modifying the set. The empty prefix represents the whole
// globe.
func (p *PrefixSet) Add(prefix string) error {
	if err := Validate(prefix); err != nil {
		return err
	}

	if p.covers(prefix) {
		return nil
	}

	// Drop everything contained in the new prefix.
	for existing := range p.set {
		if strings.HasPrefix(existing, prefix) {
			delete(p.set, existing)
		}
	}

	p.set[prefix] = struct{}{}
	p.fold(prefix)

	return nil
}

// Subtract removes the region covered by prefix from the set, expanding an
// enclosing prefix one character at a time when necessary. Expansion only
// ever reaches the length of prefix: removing a shallow region never forces
// expansion all the way down to the maximum precision. Subtracting a region
// the set does not cover is a no-op. As with Add, an invalid prefix is
// rejected and the set is left unchanged.
func (p *PrefixSet) Subtract(prefix string) error {
	if err := Validate(prefix); err != nil {
		return err
	}

	// Remove any entries located inside prefix.
	for existing := range p.set {
		if strings.HasPrefix(existing, prefix) {
			delete(p.set, existing)
		}
	}

	// Find the canonical prefix that covers prefix, if any.
	ancestor := ""
	found := false
	for existing := range p.set {
		if strings.HasPrefix(prefix, existing) {
			if !found || len(existing) > len(ancestor) {
				ancestor = existing
				found = true
			}
		}
	}
	if !found {
		return nil
	}

	// Remove the enclosing prefix before re-adding everything except the
	// branch that leads to prefix.
	delete(p.set, ancestor)

	// Expand ancestor, skipping the branch that leads to prefix. Expansion
	// stops at len(prefix), so a shallow subtraction never fans out to
	// maximum precision.
	frontier := []string{ancestor}
	for len(frontier) > 0 {
		current := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]

		if len(current) >= len(prefix) {
			continue
		}

		nextDepth := len(current) + 1
		for _, c := range base32encoding.encode {
			child := current + string(c)
			if strings.HasPrefix(prefix, child) && nextDepth <= len(prefix) {
				// This branch still contains the removed region; keep
				// expanding it rather than adding the child outright.
				frontier = append(frontier, child)
				continue
			}
			p.set[child] = struct{}{}
		}
	}

	return nil
}

// Covered reports whether the region of the (validated) prefix is fully
// covered by the set.
func (p *PrefixSet) Covered(prefix string) bool {
	return p.covers(prefix)
}

// Len returns the number of prefixes in the canonical representation.
func (p *PrefixSet) Len() int {
	return len(p.set)
}

// Prefixes returns the canonical prefixes in sorted order.
func (p *PrefixSet) Prefixes() []string {
	out := make([]string, 0, len(p.set))
	for prefix := range p.set {
		out = append(out, prefix)
	}
	sort.Strings(out)
	return out
}

// covers reports whether some stored prefix is an ancestor of (or equal to)
// prefix.
func (p *PrefixSet) covers(prefix string) bool {
	for ancestor := range p.set {
		if strings.HasPrefix(prefix, ancestor) {
			return true
		}
	}
	return false
}

// fold repeatedly replaces 32 complete siblings by their parent, starting at
// the level of start and walking up toward the root. Each geohash character
// encodes 5 bits, hence the arity is 32.
func (p *PrefixSet) fold(start string) {
	for len(start) > 0 {
		parent := start[:len(start)-1]
		complete := true
		for _, c := range base32encoding.encode {
			if _, ok := p.set[parent+string(c)]; !ok {
				complete = false
				break
			}
		}
		if !complete {
			return
		}
		for _, c := range base32encoding.encode {
			delete(p.set, parent+string(c))
		}
		p.set[parent] = struct{}{}
		start = parent
	}
}
