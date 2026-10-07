package rules

import (
	"testing"
)

func rule(id string, prio int) Rule {
	return Rule{ID: id, Action: "action-" + id, Priority: prio}
}

// The empty prefix is the default entry and applies to every key, but a
// local rule on the key's own node overrides it.
func TestEmptyPrefixDefault(t *testing.T) {
	r := New().Batch().
		SetRules(nil, []Rule{rule("default", 10)}).
		SetRules([]byte("api/"), []Rule{rule("api", 20)}).
		Commit()

	// Key with no specific entry: default applies.
	d := r.Decide([]byte("anything/here"))
	if !d.Hit || d.Effective.ID != "default" {
		t.Fatalf("expected default to win, got hit=%v rule=%+v", d.Hit, d.Effective)
	}
	if len(d.Shadows) != 0 {
		t.Fatalf("expected no shadows, got %+v", d.Shadows)
	}
	if string(d.MatchPrefix) != "" {
		t.Fatalf("expected match at empty prefix, got %q", d.MatchPrefix)
	}

	// Key under api/: the more specific, higher-priority rule wins and the
	// default is explained as shadowed.
	d = r.Decide([]byte("api/users"))
	if !d.Hit || d.Effective.ID != "api" {
		t.Fatalf("expected api to win, got %+v", d.Effective)
	}
	if len(d.Shadows) != 1 {
		t.Fatalf("expected 1 shadow, got %+v", d.Shadows)
	}
	sh := d.Shadows[0]
	if sh.Rule.ID != "default" || sh.Reason != ShadowByRule || sh.Winner.ID != "api" {
		t.Fatalf("unexpected shadow explanation: %+v", sh)
	}
	if string(sh.Prefix) != "" {
		t.Fatalf("shadowed rule should come from empty prefix, got %q", sh.Prefix)
	}
}

// Multiple rules on one prefix compete; priority decides, equal priority
// is decided by the smaller stable ID.
func TestMultipleRulesSamePrefix(t *testing.T) {
	r := New().Batch().
		SetRules([]byte("a/"), []Rule{
			rule("zzz", 5),
			rule("aaa", 5),
			rule("hi", 100),
		}).
		Commit()

	d := r.Decide([]byte("a/k"))
	if !d.Hit || d.Effective.ID != "hi" {
		t.Fatalf("expected highest-priority rule hi, got %+v", d.Effective)
	}
	if len(d.Shadows) != 2 {
		t.Fatalf("expected the two lower rules shadowed, got %+v", d.Shadows)
	}

	// Equal priority tie is broken by stable ID, deterministically.
	r2 := New().Batch().
		SetRules([]byte("a/"), []Rule{rule("zzz", 5), rule("aaa", 5)}).
		Commit()
	d = r2.Decide([]byte("a/k"))
	if d.Effective.ID != "aaa" {
		t.Fatalf("expected smaller ID aaa to win the tie, got %q", d.Effective.ID)
	}
	if d.Shadows[0].Rule.ID != "zzz" || d.Shadows[0].Winner.ID != "aaa" {
		t.Fatalf("expected zzz shadowed by aaa, got %+v", d.Shadows[0])
	}

	// Insertion order must not change the tie-break.
	r3 := New().Batch().
		SetRules([]byte("a/"), []Rule{rule("aaa", 5), rule("zzz", 5)}).
		Commit()
	if got := r3.Decide([]byte("a/k")).Effective.ID; got != "aaa" {
		t.Fatalf("tie-break must be insertion-order independent, got %q", got)
	}
}

// An exact key match is a regular entry on the path: it participates like
// any other node rather than requiring special longest-prefix handling.
func TestExactKeyNode(t *testing.T) {
	r := New().Batch().
		SetRules([]byte("api/users"), []Rule{rule("exact", 1)}).
		SetRules([]byte("api/"), []Rule{rule("dir", 50)}).
		Commit()

	// The directory rule outranks the exact node's low-priority rule even
	// though the query lands precisely on the rule node.
	d := r.Decide([]byte("api/users"))
	if !d.Hit || d.Effective.ID != "dir" {
		t.Fatalf("expected inherited dir rule to win on priority, got %+v", d.Effective)
	}
	if string(d.MatchPrefix) != "api/" {
		t.Fatalf("winner should come from api/, got %q", d.MatchPrefix)
	}
	if len(d.Shadows) != 1 || d.Shadows[0].Rule.ID != "exact" {
		t.Fatalf("expected exact rule shadowed, got %+v", d.Shadows)
	}

	// Flip priority: the node hit exactly by the key now wins.
	r2 := New().Batch().
		SetRules([]byte("api/users"), []Rule{rule("exact", 99)}).
		SetRules([]byte("api/"), []Rule{rule("dir", 50)}).
		Commit()
	d = r2.Decide([]byte("api/users"))
	if d.Effective.ID != "exact" || string(d.MatchPrefix) != "api/users" {
		t.Fatalf("expected exact node rule to win, got %+v prefix %q", d.Effective, d.MatchPrefix)
	}
	if d.Shadows[0].Rule.ID != "dir" {
		t.Fatalf("expected dir shadowed, got %+v", d.Shadows)
	}

	// A key that merely shares the exact node's bytes must not treat it as
	// on the path: api/users is not a prefix of api/usersx.
	miss := r2.Decide([]byte("api/usersx"))
	for _, sh := range miss.Shadows {
		if sh.Rule.ID == "exact" {
			t.Fatalf("exact node must not be visited for api/usersx: %+v", miss.Shadows)
		}
	}

	// An unrelated subtree sees neither the directory nor the exact rule.
	unrelated := r2.Decide([]byte("web/index"))
	if unrelated.Hit {
		t.Fatalf("expected no rule under web/, got %+v", unrelated.Effective)
	}
}

// A barrier mid-path clears inherited rules; the barrier's own local
// candidates still apply and compete with rules deeper than the barrier.
func TestMidPathBarrier(t *testing.T) {
	r := New().Batch().
		SetRules(nil, []Rule{rule("default", 100)}).
		SetRules([]byte("public/"), []Rule{rule("pub-inherit", 50)}).
		Replace([]byte("api/"), []Rule{rule("api-local", 5)}, true).
		SetRules([]byte("api/v2/"), []Rule{rule("v2", 9)}).
		Commit()

	d := r.Decide([]byte("api/v2/users"))

	// The default (priority 100) was cleared by the barrier at api/, so
	// the lower-priority local rules are free to compete. v2 wins.
	if !d.Hit || d.Effective.ID != "v2" {
		t.Fatalf("expected v2 to win after barrier, got %+v", d.Effective)
	}
	if len(d.Barriers) != 1 || string(d.Barriers[0].Prefix) != "api/" {
		t.Fatalf("expected one barrier at api/, got %+v", d.Barriers)
	}
	if d.Barriers[0].Cleared != 1 {
		t.Fatalf("barrier should clear exactly the default rule, cleared=%d", d.Barriers[0].Cleared)
	}

	// The default must be reported as barrier-shadowed, api-local as
	// rule-shadowed by v2.
	var sawDefaultBarrier, sawApiLocalRule bool
	for _, sh := range d.Shadows {
		if sh.Rule.ID == "default" && sh.Reason == ShadowByBarrier &&
			string(sh.BarrierPrefix) == "api/" {
			sawDefaultBarrier = true
		}
		if sh.Rule.ID == "api-local" && sh.Reason == ShadowByRule && sh.Winner.ID == "v2" {
			sawApiLocalRule = true
		}
	}
	if !sawDefaultBarrier || !sawApiLocalRule {
		t.Fatalf("missing expected shadows: defaultBarrier=%v apiLocalRule=%v all=%+v",
			sawDefaultBarrier, sawApiLocalRule, d.Shadows)
	}

	// A key ending inside the barrier node keeps the barrier's local rule
	// but the default is still gone.
	d = r.Decide([]byte("api/ping"))
	if d.Effective.ID != "api-local" {
		t.Fatalf("expected api-local inside barrier node, got %+v", d.Effective)
	}

	// Outside the barrier subtree, inheritance is untouched.
	d = r.Decide([]byte("public/index"))
	if d.Effective.ID != "default" {
		t.Fatalf("expected default to win outside barrier, got %+v", d.Effective)
	}
	for _, sh := range d.Shadows {
		if sh.Rule.ID == "pub-inherit" && sh.Reason != ShadowByRule {
			t.Fatalf("pub-inherit should be rule-shadowed, not barrier: %+v", sh)
		}
	}
}

// Rules on sibling branches never participate in a decision, even when
// they carry higher priority.
func TestSiblingsDoNotParticipate(t *testing.T) {
	r := New().Batch().
		SetRules([]byte("a/"), []Rule{rule("a", 1)}).
		SetRules([]byte("b/"), []Rule{rule("b", 999)}).
		SetRules([]byte("a/x/"), []Rule{rule("ax", 2)}).
		Commit()

	d := r.Decide([]byte("a/x/k"))
	if d.Effective.ID != "ax" {
		t.Fatalf("expected ax, got %+v", d.Effective)
	}
	for _, sh := range d.Shadows {
		if sh.Rule.ID == "b" {
			t.Fatalf("sibling rule b must never be considered: %+v", d.Shadows)
		}
	}
	if len(d.Shadows) != 1 || d.Shadows[0].Rule.ID != "a" {
		t.Fatalf("only ancestor a should be shadowed, got %+v", d.Shadows)
	}
}

// No rule anywhere: a miss with an empty, non-nil explanation.
func TestNoRule(t *testing.T) {
	r := New()
	d := r.Decide([]byte("nope"))
	if d.Hit || d.MatchPrefix != nil || d.Shadows != nil || d.Barriers != nil {
		t.Fatalf("expected clean miss, got %+v", d)
	}
	if string(d.Key) != "nope" {
		t.Fatalf("key should be recorded, got %q", d.Key)
	}
}
