// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package iradix

// ShadowReason explains why a candidate rule along the lookup path did
// not become the effective rule for a query.
type ShadowReason int

const (
	// ShadowedByBarrier means the rule was inherited from an ancestor
	// prefix, but a barrier declared closer to the key cut off
	// inheritance before the rule could take effect.
	ShadowedByBarrier ShadowReason = iota

	// ShadowedByPriority means the rule competed in the final candidate
	// pool but lost to the winning rule's higher priority.
	ShadowedByPriority

	// ShadowedByTieBreak means the rule had the same priority as the
	// winning rule but lost the stable rule ID tie-break.
	ShadowedByTieBreak
)

// String returns a human readable name for the shadow reason.
func (r ShadowReason) String() string {
	switch r {
	case ShadowedByBarrier:
		return "shadowed-by-barrier"
	case ShadowedByPriority:
		return "shadowed-by-priority"
	case ShadowedByTieBreak:
		return "shadowed-by-tie-break"
	default:
		return "unknown"
	}
}

// Rule is a single routing rule attached to a prefix. The ID must be
// stable for the lifetime of the rule: it identifies the rule across
// snapshots and acts as the deterministic tie-breaker when two candidate
// rules share the same priority. The lexicographically smaller ID wins.
type Rule[T any] struct {
	ID       string
	Priority int
	Value    T
}

// ruleScope is the value stored at each radix leaf: all rules declared
// at one prefix plus an optional barrier flag.
type ruleScope[T any] struct {
	rules   []Rule[T]
	barrier bool
}

// ShadowedRule describes one rule that was considered (or excluded)
// while resolving a key, together with the reason it did not win.
type ShadowedRule[T any] struct {
	// Prefix is the prefix at which the shadowed rule was declared.
	Prefix []byte

	// Rule is the shadowed rule itself.
	Rule Rule[T]

	// Reason explains why the rule did not take effect.
	Reason ShadowReason

	// Winner is the effective rule that outranked this one. It is only
	// set for ShadowedByPriority and ShadowedByTieBreak.
	Winner Rule[T]

	// WinnerPrefix is the prefix at which the winning rule was declared.
	// It is only set for ShadowedByPriority and ShadowedByTieBreak.
	WinnerPrefix []byte

	// BarrierPrefix is the prefix at which the barrier that excluded
	// this rule was declared. It is only set for ShadowedByBarrier.
	BarrierPrefix []byte
}

// Decision is the result of resolving a key against a RuleTree. It
// reports the final effective rule and explains every rule along the
// lookup path that was shadowed.
type Decision[T any] struct {
	// Key is the queried key.
	Key []byte

	// Found reports whether any rule survived to take effect.
	Found bool

	// Rule is the final effective rule.
	Rule Rule[T]

	// Prefix is the prefix at which the effective rule was declared.
	Prefix []byte

	// Shadowed lists the rules encountered along the path that did not
	// take effect, in path order (shallowest prefix first).
	Shadowed []ShadowedRule[T]
}

// RuleTree is an immutable, radix-backed view over routing rules. Rules
// are declared at prefixes; querying a key walks the prefixes along the
// key's path and resolves inheritance, barriers and local candidates
// into a single effective rule. Because the underlying tree is
// immutable, a RuleTree snapshot can be shared and queried concurrently
// without coordination, and batch updates are applied by committing a
// transaction into a new snapshot.
type RuleTree[T any] struct {
	tree *Tree[*ruleScope[T]]
}

// NewRuleTree returns an empty rule tree.
func NewRuleTree[T any]() *RuleTree[T] {
	return &RuleTree[T]{tree: New[*ruleScope[T]]()}
}

// Len returns the number of prefixes that carry a scope (rules or a
// barrier).
func (t *RuleTree[T]) Len() int {
	return t.tree.Len()
}

// Txn starts a transaction that batches updates into a new immutable
// snapshot. The transaction is not thread safe.
func (t *RuleTree[T]) Txn() *RuleTxn[T] {
	return &RuleTxn[T]{txn: t.tree.Txn()}
}

// better reports whether rule a outranks rule b: higher priority first,
// then the lexicographically smaller stable ID.
func better[T any](a, b Rule[T]) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.ID < b.ID
}

// Decide resolves key to its final effective rule. It walks only the
// prefixes on the key's own path (sibling branches never participate),
// applies barriers to cut off inherited rules, and then lets the
// surviving local and inherited candidates compete by priority with the
// stable rule ID as tie-breaker. Every rule that was encountered but did
// not win is reported in the decision's Shadowed list.
func (t *RuleTree[T]) Decide(key []byte) Decision[T] {
	type candidate struct {
		prefix []byte
		rule   Rule[T]
	}

	d := Decision[T]{Key: key}
	var pool []candidate

	t.tree.Root().WalkPath(key, func(prefix []byte, scope *ruleScope[T]) bool {
		if scope.barrier {
			// A barrier cuts off inheritance: every rule collected from
			// ancestor prefixes so far is excluded from here on.
			for _, c := range pool {
				d.Shadowed = append(d.Shadowed, ShadowedRule[T]{
					Prefix:        c.prefix,
					Rule:          c.rule,
					Reason:        ShadowedByBarrier,
					BarrierPrefix: prefix,
				})
			}
			pool = pool[:0]
		}
		for _, r := range scope.rules {
			pool = append(pool, candidate{prefix: prefix, rule: r})
		}
		return false
	})

	if len(pool) == 0 {
		return d
	}

	win := 0
	for i := 1; i < len(pool); i++ {
		if better(pool[i].rule, pool[win].rule) {
			win = i
		}
	}

	d.Found = true
	d.Rule = pool[win].rule
	d.Prefix = pool[win].prefix
	for i, c := range pool {
		if i == win {
			continue
		}
		reason := ShadowedByTieBreak
		if c.rule.Priority != d.Rule.Priority {
			reason = ShadowedByPriority
		}
		d.Shadowed = append(d.Shadowed, ShadowedRule[T]{
			Prefix:       c.prefix,
			Rule:         c.rule,
			Reason:       reason,
			Winner:       d.Rule,
			WinnerPrefix: d.Prefix,
		})
	}
	return d
}

// RuleTxn batches rule updates against a RuleTree. Committing the
// transaction produces a new immutable snapshot; previously committed
// snapshots keep their original interpretation of every key.
type RuleTxn[T any] struct {
	txn *Txn[*ruleScope[T]]

	// cache memoizes scopes already cloned during this transaction so
	// repeated writes to the same prefix reuse one writable copy.
	cache map[string]*ruleScope[T]
}

// scopeForWrite returns a private, writable copy of the scope at prefix,
// cloning the committed scope (if any) so existing snapshots are never
// disturbed.
func (t *RuleTxn[T]) scopeForWrite(prefix []byte) *ruleScope[T] {
	if t.cache != nil {
		if s, ok := t.cache[string(prefix)]; ok {
			return s
		}
	}
	next := &ruleScope[T]{}
	if cur, ok := t.txn.Get(prefix); ok && cur != nil {
		next.rules = append(next.rules, cur.rules...)
		next.barrier = cur.barrier
	}
	t.txn.Insert(prefix, next)
	if t.cache == nil {
		t.cache = make(map[string]*ruleScope[T])
	}
	t.cache[string(prefix)] = next
	return next
}

// AddRule attaches a rule to prefix. If a rule with the same stable ID
// already exists at that prefix it is replaced, keeping declaration
// order otherwise. An empty prefix declares a default rule that is
// inherited by every key.
func (t *RuleTxn[T]) AddRule(prefix []byte, rule Rule[T]) {
	scope := t.scopeForWrite(prefix)
	for i, r := range scope.rules {
		if r.ID == rule.ID {
			scope.rules[i] = rule
			return
		}
	}
	scope.rules = append(scope.rules, rule)
}

// RemoveRule detaches the rule with the given ID from prefix. It returns
// true if such a rule existed. If the prefix is left with no rules and
// no barrier, its scope is removed entirely.
func (t *RuleTxn[T]) RemoveRule(prefix []byte, id string) bool {
	cur, ok := t.txn.Get(prefix)
	if !ok || cur == nil {
		return false
	}
	found := false
	for _, r := range cur.rules {
		if r.ID == id {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	scope := t.scopeForWrite(prefix)
	kept := scope.rules[:0]
	for _, r := range scope.rules {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	scope.rules = kept
	t.pruneIfEmpty(prefix, scope)
	return true
}

// SetBarrier declares that keys under prefix must not inherit rules from
// ancestor prefixes. Rules declared at or below prefix still apply.
func (t *RuleTxn[T]) SetBarrier(prefix []byte) {
	t.scopeForWrite(prefix).barrier = true
}

// ClearBarrier removes the barrier declared at prefix, restoring
// inheritance from ancestor prefixes. It returns false if no barrier was
// declared there.
func (t *RuleTxn[T]) ClearBarrier(prefix []byte) bool {
	cur, ok := t.txn.Get(prefix)
	if !ok || cur == nil || !cur.barrier {
		return false
	}
	scope := t.scopeForWrite(prefix)
	scope.barrier = false
	t.pruneIfEmpty(prefix, scope)
	return true
}

// DeletePrefix removes the entire scope (all rules and any barrier)
// declared at prefix. It returns true if such a scope existed.
func (t *RuleTxn[T]) DeletePrefix(prefix []byte) bool {
	if _, ok := t.txn.Get(prefix); !ok {
		return false
	}
	t.txn.Delete(prefix)
	if t.cache != nil {
		delete(t.cache, string(prefix))
	}
	return true
}

// pruneIfEmpty drops the scope at prefix once it carries neither rules
// nor a barrier, keeping the tree sparse.
func (t *RuleTxn[T]) pruneIfEmpty(prefix []byte, scope *ruleScope[T]) {
	if len(scope.rules) == 0 && !scope.barrier {
		t.txn.Delete(prefix)
		if t.cache != nil {
			delete(t.cache, string(prefix))
		}
	}
}

// Commit finalizes the transaction and returns the new immutable rule
// tree. The transaction must not be used afterwards.
func (t *RuleTxn[T]) Commit() *RuleTree[T] {
	return &RuleTree[T]{tree: t.txn.Commit()}
}
