package rules

import iradix "github.com/hashicorp/go-immutable-radix/v2"

// Batch stages mutations against a Rules snapshot and applies them
// atomically. It is backed by a radix transaction and is not safe for
// concurrent use. The snapshot a Batch was started from is never mutated;
// Commit returns a new snapshot.
type Batch struct {
	txn *iradix.Txn[ruleSet]
}

// Batch starts a batch of modifications against the snapshot.
func (r *Rules) Batch() *Batch {
	return &Batch{txn: r.tree.Txn()}
}

// SetRules replaces the complete rule list attached at prefix. It does not
// change whether the entry is a barrier. An empty list removes the entry's
// rules (the entry is kept only if it declares a barrier).
func (b *Batch) SetRules(prefix []byte, rules []Rule) *Batch {
	set, _ := b.txn.Get(prefix)
	set.rules = cloneRules(rules)
	b.write(prefix, set)
	return b
}

// SetBarrier declares or clears the barrier at prefix ("from this entry
// onward, rules from strict ancestors no longer inherit"). The entry's
// rules are left untouched.
func (b *Batch) SetBarrier(prefix []byte, barrier bool) *Batch {
	set, _ := b.txn.Get(prefix)
	set.barrier = barrier
	b.write(prefix, set)
	return b
}

// Replace atomically sets both the rules and barrier flag of prefix,
// fully replacing any previous entry. An empty rules list with barrier
// false removes the entry entirely.
func (b *Batch) Replace(prefix []byte, rules []Rule, barrier bool) *Batch {
	set := ruleSet{barrier: barrier, rules: cloneRules(rules)}
	b.write(prefix, set)
	return b
}

// Delete removes both the rules and the barrier declared at prefix.
func (b *Batch) Delete(prefix []byte) *Batch {
	b.txn.Delete(prefix)
	return b
}

// write stores set at prefix, dropping the radix entry when it carries no
// rules and no barrier.
func (b *Batch) write(prefix []byte, set ruleSet) {
	set.rules = normalizeRules(set.rules)
	if !set.barrier && len(set.rules) == 0 {
		b.txn.Delete(prefix)
		return
	}
	// The radix tree may retain slices from a prior entry; assign a fresh
	// normalized slice regardless so the stored value is self-contained.
	b.txn.Insert(append([]byte(nil), prefix...), set)
}

// Commit applies all staged changes atomically and returns the new
// immutable snapshot. The Batch must not be reused afterwards.
func (b *Batch) Commit() *Rules {
	return fromTree(b.txn.Commit())
}

// cloneRules returns an independent, normalized-free copy of rules.
func cloneRules(rules []Rule) []Rule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]Rule, len(rules))
	copy(out, rules)
	return out
}
