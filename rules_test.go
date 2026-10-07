// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package iradix

import (
	"testing"
)

func shadowReasons[T any](d Decision[T]) map[string]ShadowReason {
	out := make(map[string]ShadowReason, len(d.Shadowed))
	for _, s := range d.Shadowed {
		out[s.Rule.ID] = s.Reason
	}
	return out
}

func TestRuleTree_EmptyPrefixDefault(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule(nil, Rule[string]{ID: "default", Priority: 1, Value: "fallback"})
	rt = txn.Commit()

	d := rt.Decide([]byte("/any/key/at/all"))
	if !d.Found {
		t.Fatal("expected the empty-prefix default rule to apply")
	}
	if d.Rule.ID != "default" || string(d.Prefix) != "" {
		t.Fatalf("bad decision: %+v", d)
	}
	if len(d.Shadowed) != 0 {
		t.Fatalf("expected no shadowed rules, got %+v", d.Shadowed)
	}
}

func TestRuleTree_ExactKeyHit(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule([]byte("/app"), Rule[string]{ID: "app", Priority: 5, Value: "app"})
	txn.AddRule([]byte("/app/admin"), Rule[string]{ID: "admin", Priority: 5, Value: "admin"})
	rt = txn.Commit()

	// The key lands exactly on a rule node: both the node itself and its
	// ancestor are candidates, deeper rules are not.
	d := rt.Decide([]byte("/app"))
	if !d.Found || d.Rule.ID != "app" {
		t.Fatalf("bad decision: %+v", d)
	}
	if string(d.Prefix) != "/app" {
		t.Fatalf("bad winner prefix: %q", d.Prefix)
	}

	d = rt.Decide([]byte("/app/admin"))
	if !d.Found || d.Rule.ID != "admin" {
		t.Fatalf("bad decision: %+v", d)
	}
	shadowed := shadowReasons(d)
	if shadowed["app"] != ShadowedByTieBreak {
		t.Fatalf("expected app shadowed by tie-break, got %+v", d.Shadowed)
	}
}

func TestRuleTree_NotJustLongestPrefix(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	// A shallow rule with high priority must beat a deeper low-priority
	// rule; plain LongestPrefix would pick the deep one.
	txn.AddRule([]byte("/app"), Rule[string]{ID: "shallow", Priority: 100, Value: "shallow"})
	txn.AddRule([]byte("/app/admin"), Rule[string]{ID: "deep", Priority: 1, Value: "deep"})
	rt = txn.Commit()

	d := rt.Decide([]byte("/app/admin/users"))
	if !d.Found || d.Rule.ID != "shallow" {
		t.Fatalf("expected shallow high-priority rule to win, got %+v", d)
	}
	shadowed := shadowReasons(d)
	if shadowed["deep"] != ShadowedByPriority {
		t.Fatalf("expected deep shadowed by priority, got %+v", d.Shadowed)
	}
	if string(d.Shadowed[0].WinnerPrefix) != "/app" {
		t.Fatalf("bad winner prefix in shadow record: %+v", d.Shadowed[0])
	}
}

func TestRuleTree_TieBreakByStableID(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule([]byte("/app"), Rule[string]{ID: "rule-b", Priority: 10})
	txn.AddRule([]byte("/app"), Rule[string]{ID: "rule-a", Priority: 10})
	rt = txn.Commit()

	d := rt.Decide([]byte("/app/x"))
	if !d.Found || d.Rule.ID != "rule-a" {
		t.Fatalf("expected lexicographically smaller ID to win, got %+v", d)
	}
	shadowed := shadowReasons(d)
	if shadowed["rule-b"] != ShadowedByTieBreak {
		t.Fatalf("expected rule-b shadowed by tie-break, got %+v", d.Shadowed)
	}
	if d.Shadowed[0].Winner.ID != "rule-a" {
		t.Fatalf("bad winner in shadow record: %+v", d.Shadowed[0])
	}
}

func TestRuleTree_BarrierCutsInheritance(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule(nil, Rule[string]{ID: "root", Priority: 1000, Value: "root"})
	txn.AddRule([]byte("/app"), Rule[string]{ID: "app", Priority: 1, Value: "app"})
	txn.SetBarrier([]byte("/app/admin"))
	txn.AddRule([]byte("/app/admin"), Rule[string]{ID: "admin", Priority: 1, Value: "admin"})
	rt = txn.Commit()

	// Above the barrier, inheritance works as usual.
	d := rt.Decide([]byte("/app/page"))
	if !d.Found || d.Rule.ID != "root" {
		t.Fatalf("bad decision above barrier: %+v", d)
	}

	// Below the barrier, ancestor rules are excluded even though the
	// root rule has a much higher priority.
	d = rt.Decide([]byte("/app/admin/panel"))
	if !d.Found || d.Rule.ID != "admin" {
		t.Fatalf("expected local rule to win below barrier, got %+v", d)
	}
	shadowed := shadowReasons(d)
	if shadowed["root"] != ShadowedByBarrier || shadowed["app"] != ShadowedByBarrier {
		t.Fatalf("expected ancestors shadowed by barrier, got %+v", d.Shadowed)
	}
	for _, s := range d.Shadowed {
		if s.Reason == ShadowedByBarrier && string(s.BarrierPrefix) != "/app/admin" {
			t.Fatalf("bad barrier prefix: %+v", s)
		}
	}
}

func TestRuleTree_BarrierWithoutLocalRules(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule(nil, Rule[string]{ID: "root", Priority: 10})
	txn.SetBarrier([]byte("/isolated"))
	rt = txn.Commit()

	d := rt.Decide([]byte("/isolated/thing"))
	if d.Found {
		t.Fatalf("expected no effective rule below bare barrier, got %+v", d)
	}
	shadowed := shadowReasons(d)
	if shadowed["root"] != ShadowedByBarrier {
		t.Fatalf("expected root shadowed by barrier, got %+v", d.Shadowed)
	}
}

func TestRuleTree_SiblingBranchesDoNotParticipate(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule([]byte("/app"), Rule[string]{ID: "app", Priority: 1})
	txn.AddRule([]byte("/other"), Rule[string]{ID: "other", Priority: 1000})
	rt = txn.Commit()

	d := rt.Decide([]byte("/app/x"))
	if !d.Found || d.Rule.ID != "app" {
		t.Fatalf("sibling branch rule leaked into decision: %+v", d)
	}
	if len(d.Shadowed) != 0 {
		t.Fatalf("expected no shadowed rules, got %+v", d.Shadowed)
	}
}

func TestRuleTree_SnapshotIsolation(t *testing.T) {
	base := NewRuleTree[string]()
	txn := base.Txn()
	txn.AddRule(nil, Rule[string]{ID: "root", Priority: 1, Value: "v1"})
	txn.AddRule([]byte("/app"), Rule[string]{ID: "app", Priority: 2, Value: "v1"})
	old := txn.Commit()

	oldDecision := old.Decide([]byte("/app/x"))

	// Batch-replace: rewrite the root rule, remove the /app rule and add
	// a barrier plus a fresh local rule, all in one commit.
	txn = old.Txn()
	txn.AddRule(nil, Rule[string]{ID: "root", Priority: 1, Value: "v2"})
	txn.RemoveRule([]byte("/app"), "app")
	txn.SetBarrier([]byte("/app"))
	txn.AddRule([]byte("/app"), Rule[string]{ID: "app2", Priority: 3, Value: "v2"})
	updated := txn.Commit()

	// The old snapshot must keep its original interpretation.
	again := old.Decide([]byte("/app/x"))
	if !again.Found || again.Rule.ID != "app" || again.Rule.Value != "v1" {
		t.Fatalf("old snapshot changed after commit: %+v", again)
	}
	if again.Rule != oldDecision.Rule || len(again.Shadowed) != len(oldDecision.Shadowed) {
		t.Fatalf("old snapshot not stable: %+v vs %+v", again, oldDecision)
	}

	// The new snapshot sees the batch atomically.
	d := updated.Decide([]byte("/app/x"))
	if !d.Found || d.Rule.ID != "app2" || d.Rule.Value != "v2" {
		t.Fatalf("bad decision on updated snapshot: %+v", d)
	}
	shadowed := shadowReasons(d)
	if shadowed["root"] != ShadowedByBarrier {
		t.Fatalf("expected root shadowed by barrier, got %+v", d.Shadowed)
	}
}

func TestRuleTree_NoRule(t *testing.T) {
	rt := NewRuleTree[string]()
	d := rt.Decide([]byte("/nothing"))
	if d.Found {
		t.Fatalf("expected no rule, got %+v", d)
	}
	if len(d.Shadowed) != 0 {
		t.Fatalf("expected empty shadow list, got %+v", d.Shadowed)
	}
}

func TestRuleTree_UpdateSameIDReplaces(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule([]byte("/app"), Rule[string]{ID: "r", Priority: 1, Value: "old"})
	txn.AddRule([]byte("/app"), Rule[string]{ID: "r", Priority: 1, Value: "new"})
	rt = txn.Commit()

	d := rt.Decide([]byte("/app"))
	if !d.Found || d.Rule.Value != "new" {
		t.Fatalf("expected replacement by stable ID, got %+v", d)
	}
	if len(d.Shadowed) != 0 {
		t.Fatalf("replaced rule should not linger, got %+v", d.Shadowed)
	}
}

func TestRuleTree_RemoveAndClearBarrier(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule(nil, Rule[string]{ID: "root", Priority: 1})
	txn.AddRule([]byte("/app"), Rule[string]{ID: "app", Priority: 1})
	txn.SetBarrier([]byte("/app"))
	rt = txn.Commit()

	if rt.Len() != 2 {
		t.Fatalf("expected 2 scopes, got %d", rt.Len())
	}

	txn = rt.Txn()
	if !txn.ClearBarrier([]byte("/app")) {
		t.Fatal("expected barrier to be cleared")
	}
	if txn.ClearBarrier([]byte("/app")) {
		t.Fatal("expected second clear to report no barrier")
	}
	if !txn.RemoveRule([]byte("/app"), "app") {
		t.Fatal("expected rule removal")
	}
	if txn.RemoveRule([]byte("/app"), "app") {
		t.Fatal("expected second removal to report missing rule")
	}
	rt = txn.Commit()

	if rt.Len() != 1 {
		t.Fatalf("expected empty scope to be pruned, got %d scopes", rt.Len())
	}
	d := rt.Decide([]byte("/app/x"))
	if !d.Found || d.Rule.ID != "root" {
		t.Fatalf("expected inheritance restored, got %+v", d)
	}
}

func TestRuleTree_DeletePrefix(t *testing.T) {
	rt := NewRuleTree[string]()
	txn := rt.Txn()
	txn.AddRule([]byte("/app"), Rule[string]{ID: "app", Priority: 1})
	rt = txn.Commit()

	txn = rt.Txn()
	if !txn.DeletePrefix([]byte("/app")) {
		t.Fatal("expected prefix deletion")
	}
	if txn.DeletePrefix([]byte("/app")) {
		t.Fatal("expected second deletion to report missing prefix")
	}
	rt = txn.Commit()

	if rt.Len() != 0 {
		t.Fatalf("expected empty tree, got %d scopes", rt.Len())
	}
	if d := rt.Decide([]byte("/app")); d.Found {
		t.Fatalf("expected no rule after delete, got %+v", d)
	}
}

func TestShadowReason_String(t *testing.T) {
	cases := map[ShadowReason]string{
		ShadowedByBarrier:  "shadowed-by-barrier",
		ShadowedByPriority: "shadowed-by-priority",
		ShadowedByTieBreak: "shadowed-by-tie-break",
		ShadowReason(99):   "unknown",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Fatalf("ShadowReason(%d).String() = %q, want %q", int(r), got, want)
		}
	}
}
