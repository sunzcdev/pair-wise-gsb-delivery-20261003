package casbin

import (
	"reflect"
	"testing"

	"github.com/casbin/casbin/v3/constant"
	"github.com/casbin/casbin/v3/model"
)

func TestEnforceDecisionAllowOverride(t *testing.T) {
	e := newDecisionTestEnforcer(t, constant.AllowOverrideEffect)

	result, err := e.EnforceDecision("alice", "data", "read", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionPending || !reflect.DeepEqual(result.MissingAttrs, []string{"r.ctx.Age"}) {
		t.Fatalf("expected pending for age, got %+v", result)
	}

	result, err = e.EnforceDecision("bob", "data", "read", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionDeny || len(result.MissingAttrs) != 0 {
		t.Fatalf("expected deny without irrelevant attrs, got %+v", result)
	}

	result, err = e.EnforceDecision("alice", "data", "read", map[string]interface{}{"Age": 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionDeny {
		t.Fatalf("expected deny after completing attrs, got %+v", result)
	}

	result, err = e.EnforceDecision("alice", "data", "read", map[string]interface{}{"Age": 21})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionAllow {
		t.Fatalf("expected allow after completing attrs, got %+v", result)
	}

	for _, age := range []interface{}{20, 21} {
		ctx := map[string]interface{}{"Age": age}
		allowed, err := e.Enforce("alice", "data", "read", ctx)
		if err != nil {
			t.Fatal(err)
		}
		result, err = e.EnforceDecision("alice", "data", "read", ctx)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != (result.Decision == DecisionAllow) {
			t.Fatalf("decision mismatch for age %v: Enforce=%v result=%+v", age, allowed, result)
		}
	}
}

func TestEnforceDecisionDenyOverride(t *testing.T) {
	e := newDecisionTestEnforcer(t, constant.DenyOverrideEffect)

	result, err := e.EnforceDecision("alice", "data", "read", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionPending || !reflect.DeepEqual(result.MissingAttrs, []string{"r.ctx.Age"}) {
		t.Fatalf("expected pending while deny rule is undecided, got %+v", result)
	}

	result, err = e.EnforceDecision("bob", "data", "read", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionAllow {
		t.Fatalf("expected allow with no matched deny, got %+v", result)
	}

	result, err = e.EnforceDecision("alice", "data", "read", map[string]interface{}{"Age": 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionDeny {
		t.Fatalf("expected deny after completing attrs, got %+v", result)
	}

	result, err = e.EnforceDecision("alice", "data", "read", map[string]interface{}{"Age": 21})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionAllow {
		t.Fatalf("expected allow after completing attrs, got %+v", result)
	}

	for _, age := range []interface{}{20, 21} {
		ctx := map[string]interface{}{"Age": age}
		allowed, err := e.Enforce("alice", "data", "read", ctx)
		if err != nil {
			t.Fatal(err)
		}
		result, err = e.EnforceDecision("alice", "data", "read", ctx)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != (result.Decision == DecisionAllow) {
			t.Fatalf("decision mismatch for age %v: Enforce=%v result=%+v", age, allowed, result)
		}
	}
}

func TestEnforceDecisionOnlyRelevantPendingEffects(t *testing.T) {
	m := model.NewModel()
	m.AddDef("r", "r", "sub, ctx")
	m.AddDef("p", "p", "sub, cond, eft")
	m.AddDef("m", "m", "r.sub == p.sub && eval(p.cond)")

	allowModel := m
	allowModel.AddDef("e", "e", constant.AllowOverrideEffect)
	allowEnforcer, err := NewEnforcer(allowModel)
	if err != nil {
		t.Fatal(err)
	}
	_, err = allowEnforcer.AddPolicy("alice", "r.ctx.AllowKnown == false", "deny")
	if err != nil {
		t.Fatal(err)
	}
	_, err = allowEnforcer.AddPolicy("alice", "r.ctx.AllowMissing == true", "allow")
	if err != nil {
		t.Fatal(err)
	}

	result, err := allowEnforcer.EnforceDecision("alice", map[string]interface{}{"AllowKnown": false})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionPending || !reflect.DeepEqual(result.MissingAttrs, []string{"r.ctx.AllowMissing"}) {
		t.Fatalf("allow-override should only wait on pending allow, got %+v", result)
	}

	m = model.NewModel()
	m.AddDef("r", "r", "sub, ctx")
	m.AddDef("p", "p", "sub, cond, eft")
	m.AddDef("e", "e", constant.DenyOverrideEffect)
	m.AddDef("m", "m", "r.sub == p.sub && eval(p.cond)")
	denyEnforcer, err := NewEnforcer(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = denyEnforcer.AddPolicy("alice", "r.ctx.AllowMissing == true", "allow")
	if err != nil {
		t.Fatal(err)
	}

	result, err = denyEnforcer.EnforceDecision("alice", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionAllow || len(result.MissingAttrs) != 0 {
		t.Fatalf("deny-override should not wait on pending allow, got %+v", result)
	}
}

func TestEnforceDecisionMatcherShortCircuit(t *testing.T) {
	m := model.NewModel()
	m.AddDef("r", "r", "sub, ctx")
	m.AddDef("p", "p", "sub, cond, eft")
	m.AddDef("e", "e", constant.AllowOverrideEffect)
	m.AddDef("m", "m", "r.sub == p.sub && eval(p.cond)")
	e, err := NewEnforcer(m)
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.AddPolicy("alice", "r.ctx.Age >= 21 && r.ctx.Risk == 'high'", "allow")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.AddPolicy("bob", "r.ctx.Age < 18 || r.ctx.Risk == 'low'", "allow")
	if err != nil {
		t.Fatal(err)
	}

	result, err := e.EnforceDecision("alice", map[string]interface{}{"Age": 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionDeny || len(result.MissingAttrs) != 0 {
		t.Fatalf("AND should short-circuit and ignore r.ctx.Risk, got %+v", result)
	}

	result, err = e.EnforceDecision("bob", map[string]interface{}{"Age": 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionAllow {
		t.Fatalf("OR should short-circuit to allow, got %+v", result)
	}

	result, err = e.EnforceDecision("bob", map[string]interface{}{"Age": 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionPending || !reflect.DeepEqual(result.MissingAttrs, []string{"r.ctx.Risk"}) {
		t.Fatalf("OR should require only unresolved right operand, got %+v", result)
	}
}

func TestEnforceDecisionParenthesizedShortCircuit(t *testing.T) {
	m := model.NewModel()
	m.AddDef("r", "r", "sub, ctx")
	m.AddDef("p", "p", "sub, cond, eft")
	m.AddDef("e", "e", constant.AllowOverrideEffect)
	m.AddDef("m", "m", "r.sub == p.sub && eval(p.cond)")
	e, err := NewEnforcer(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.AddPolicy("alice", "(r.ctx.Age < 18 || r.ctx.Age > 65) && r.ctx.Risk == 'high'", "allow")
	if err != nil {
		t.Fatal(err)
	}

	result, err := e.EnforceDecision("alice", map[string]interface{}{"Age": 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionDeny || len(result.MissingAttrs) != 0 {
		t.Fatalf("parenthesized OR should short-circuit AND, got %+v", result)
	}
}

func TestEnforceDecisionPriorityPrunesLowerPriorityAttrs(t *testing.T) {
	m := model.NewModel()
	m.AddDef("r", "r", "sub, ctx")
	m.AddDef("p", "p", "sub, cond, eft")
	m.AddDef("e", "e", constant.PriorityEffect)
	m.AddDef("m", "m", "r.sub == p.sub && eval(p.cond)")
	e, err := NewEnforcer(m)
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.AddPolicy("alice", "r.ctx.LowPriority == true", "allow")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.AddPolicy("alice", "r.ctx.HighPriority == true", "deny")
	if err != nil {
		t.Fatal(err)
	}

	result, err := e.EnforceDecision("alice", map[string]interface{}{"HighPriority": true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionDeny || len(result.MissingAttrs) != 0 {
		t.Fatalf("higher-priority deny should not report lower-priority attrs, got %+v", result)
	}

	result, err = e.EnforceDecision("alice", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionPending || !reflect.DeepEqual(result.MissingAttrs, []string{"r.ctx.HighPriority"}) {
		t.Fatalf("pending should report only highest-priority attrs, got %+v", result)
	}
}

func newDecisionTestEnforcer(t *testing.T, effect string) *Enforcer {
	t.Helper()
	m := model.NewModel()
	m.AddDef("r", "r", "sub, obj, act, ctx")
	m.AddDef("p", "p", "sub, obj, act, cond, eft")
	m.AddDef("e", "e", effect)
	m.AddDef("m", "m", "r.sub == p.sub && r.obj == p.obj && r.act == p.act && eval(p.cond)")
	e, err := NewEnforcer(m)
	if err != nil {
		t.Fatal(err)
	}

	cond := "r.ctx.Age < 21"
	if effect == constant.AllowOverrideEffect {
		cond = "r.ctx.Age >= 65"
	}
	_, err = e.AddPolicy("alice", "data", "read", cond, "deny")
	if err != nil {
		t.Fatal(err)
	}
	if effect == constant.AllowOverrideEffect {
		_, err = e.AddPolicy("alice", "data", "read", "r.ctx.Age >= 21", "allow")
	} else {
		_, err = e.AddPolicy("alice", "data", "read", "r.ctx.Age >= 21", "allow")
	}
	if err != nil {
		t.Fatal(err)
	}
	return e
}
