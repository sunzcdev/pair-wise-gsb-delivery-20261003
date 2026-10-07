// Package rules layers an "effective rule" view on top of an immutable
// radix tree.
//
// Multiple rules can be attached to the same prefix. A query for a key
// considers every rule attached to an ancestor prefix of the key, in path
// order. An entry may declare a barrier ("inherit no further"), which
// discards every rule inherited from strict ancestors before the entry's
// own rules are considered. The surviving rule with the highest priority
// wins; ties are broken by the stable rule ID in ascending order.
//
// A Rules value is an immutable snapshot. Mutations are staged on a Batch
// (backed by a radix transaction) and applied atomically with Commit,
// producing a new Rules value. The receiver snapshot keeps its previous
// contents and query explanations.
package rules

import (
	"sort"

	iradix "github.com/hashicorp/go-immutable-radix/v2"
)

// Rule is a single routing rule attached to a prefix.
type Rule struct {
	// ID is the stable identifier, also used as the tie-breaker when two
	// rules have the same priority.
	ID string

	// Action is the payload carried by the rule.
	Action string

	// Priority selects the winner among visible rules; higher wins.
	Priority int
}

// ShadowReason describes why an ancestor rule did not survive.
type ShadowReason int

const (
	// ShadowByBarrier means a barrier on a descendant entry cleared the
	// rule before any further rule could compete with it.
	ShadowByBarrier ShadowReason = iota

	// ShadowByRule means another rule visible at the query point had
	// strictly higher priority, or the same priority with a smaller ID.
	ShadowByRule
)

// Shadow records one rule that was visible at some point but did not
// survive to the final decision, together with what eliminated it.
type Shadow struct {
	// Prefix is the path entry the shadowed rule was attached to.
	Prefix []byte

	// Rule is the rule that lost.
	Rule Rule

	// Reason classifies how the rule was eliminated.
	Reason ShadowReason

	// BarrierPrefix is set to the prefix that declared the barrier when
	// Reason is ShadowByBarrier.
	BarrierPrefix []byte

	// Winner is the rule that beat it when Reason is ShadowByRule.
	Winner Rule
}

// Barrier records a barrier entry encountered while walking the key.
type Barrier struct {
	// Prefix is the entry that declared the barrier.
	Prefix []byte

	// Cleared reports how many inherited rules the barrier discarded.
	Cleared int
}

// Decision is the explanation of a key query.
type Decision struct {
	// Key is the queried key.
	Key []byte

	// Effective is the winning rule. It is the zero value when no rule
	// applies; check Hit.
	Effective Rule

	// Hit reports whether any rule survived.
	Hit bool

	// MatchPrefix is the prefix entry the winning rule came from. Nil when
	// Hit is false.
	MatchPrefix []byte

	// Shadows lists every rule that was considered but eliminated, in the
	// order it happened along the path.
	Shadows []Shadow

	// Barriers lists the barrier entries encountered along the path, in
	// path order.
	Barriers []Barrier
}

// ruleSet is the value stored at each prefix.
type ruleSet struct {
	barrier bool
	rules   []Rule
}

// candidate pairs a visible rule with the prefix entry it came from.
type candidate struct {
	prefix []byte
	rule   Rule
}

// Rules is an immutable snapshot of the prefix rule configuration.
type Rules struct {
	tree *iradix.Tree[ruleSet]
}

// New returns an empty rule view.
func New() *Rules {
	return &Rules{tree: iradix.New[ruleSet]()}
}

// fromTree wraps an existing radix tree as a snapshot.
func fromTree(t *iradix.Tree[ruleSet]) *Rules {
	return &Rules{tree: t}
}

// Len returns the number of prefix entries that carry rules or a barrier.
func (r *Rules) Len() int {
	return r.tree.Len()
}

// Prefix returns a copy of the configuration attached at prefix: the rules
// at the entry (not including inherited rules) and whether the entry
// declares a barrier. The boolean reports whether the entry exists.
func (r *Rules) Prefix(prefix []byte) (rules []Rule, barrier, ok bool) {
	set, found := r.tree.Get(prefix)
	if !found {
		return nil, false, false
	}
	return append([]Rule(nil), set.rules...), set.barrier, true
}

// Decide returns the effective rule and a full shadowing explanation for
// key. A non-nil Decision is returned even when no rule applies.
func (r *Rules) Decide(key []byte) *Decision {
	decision := &Decision{Key: cloneBytes(key)}

	// survivors always holds the single best visible rule. PathIterator
	// visits exactly the entries whose keys are prefixes of the queried
	// key, from the empty prefix down to the longest match. Sibling
	// branches are never visited and therefore cannot influence the
	// decision.
	var survivors []candidate

	iter := r.tree.Root().PathIterator(key)
	for k, set, ok := iter.Next(); ok; k, set, ok = iter.Next() {
		if set.barrier {
			cleared := len(survivors)
			for _, cand := range survivors {
				decision.Shadows = append(decision.Shadows, Shadow{
					Prefix:        cloneBytes(cand.prefix),
					Rule:          cand.rule,
					Reason:        ShadowByBarrier,
					BarrierPrefix: cloneBytes(k),
				})
			}
			survivors = survivors[:0]
			decision.Barriers = append(decision.Barriers, Barrier{
				Prefix:  cloneBytes(k),
				Cleared: cleared,
			})
		}
		for _, rule := range set.rules {
			survivors = reduce(survivors, k, rule, decision)
		}
	}

	if len(survivors) == 0 {
		return decision
	}
	decision.Effective = survivors[0].rule
	decision.MatchPrefix = cloneBytes(survivors[0].prefix)
	decision.Hit = true
	return decision
}

// reduce folds a local rule into the visible candidate set. The set holds
// at most the current winner; every loser immediately gets an explanation,
// so the final Decision reports precisely which rules were shadowed and by
// what.
func reduce(survivors []candidate, prefix []byte, rule Rule, decision *Decision) []candidate {
	if len(survivors) == 0 {
		return append(survivors, candidate{prefix: cloneBytes(prefix), rule: rule})
	}

	best := survivors[0]
	switch {
	case outranks(rule, best.rule):
		decision.Shadows = append(decision.Shadows, Shadow{
			Prefix: cloneBytes(best.prefix),
			Rule:   best.rule,
			Reason: ShadowByRule,
			Winner: rule,
		})
		return []candidate{{prefix: cloneBytes(prefix), rule: rule}}
	case outranks(best.rule, rule):
		decision.Shadows = append(decision.Shadows, Shadow{
			Prefix: cloneBytes(prefix),
			Rule:   rule,
			Reason: ShadowByRule,
			Winner: best.rule,
		})
		return survivors
	default:
		// Same priority and same ID: the same stable rule reached via
		// another entry. It shadows nothing and is not stored twice.
		return survivors
	}
}

// outranks reports whether a beats b: higher priority wins, then the
// smaller stable ID wins.
func outranks(a, b Rule) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.ID < b.ID
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return []byte{}
	}
	return append([]byte(nil), b...)
}

// normalizeRules returns a de-duplicated, deterministic copy of rules:
// highest priority first, then ascending ID. Duplicate IDs keep only the
// first occurrence in the caller-supplied order.
func normalizeRules(rules []Rule) []Rule {
	if len(rules) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(rules))
	out := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		if _, dup := seen[rule.ID]; dup {
			continue
		}
		seen[rule.ID] = struct{}{}
		out = append(out, rule)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}
