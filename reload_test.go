package aconfig

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// reloadCI is compared through a type-registered EqualFunc.
type reloadCI string

// reloadKind implements Comparer itself.
type reloadKind string

func (k reloadKind) Equal(other any) bool {
	o, ok := other.(reloadKind)
	if !ok {
		return false
	}
	return reloadNormalize(string(k)) == reloadNormalize(string(o))
}

func reloadNormalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

type reloadSub struct {
	Addr string
}

type reloadRoot struct {
	Host    string
	Port    int    `reload:"restart"`
	Secret  string `sensitive:"true"`
	Forbid  string `reload:"never"`
	Timeout *int

	Labels map[string]string
	Hosts  []string
	Kind   reloadKind
	CI     reloadCI

	DB struct {
		Name string `reload_group:"db"`
		DSN  string `reload:"restart" reload_group:"db"`
	}

	TLS struct {
		Cert string `reload_group:"tls"`
		Prov string `reload:"never" reload_group:"tls"`
	}

	Sub *reloadSub `reload:"restart"`
}

type reloadPtrRoot struct {
	Srv *reloadSub
}

func reloadClone(t *testing.T, v reloadRoot) reloadRoot {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out reloadRoot
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func reloadFindField(changes []Change, path string) (Change, bool) {
	for _, c := range changes {
		if c.Path == path {
			return c, true
		}
	}
	return Change{}, false
}

func reloadFindGroup(groups []GroupPlan, name string) (GroupPlan, bool) {
	for _, g := range groups {
		if g.Name == name {
			return g, true
		}
	}
	return GroupPlan{}, false
}

func TestPlanner_BasicClassification(t *testing.T) {
	oldCfg := reloadRoot{Host: "127.0.0.1", Port: 8080, Forbid: "v1"}
	newCfg := reloadRoot{Host: "0.0.0.0", Port: 9090, Forbid: "v2"}

	plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
	if err != nil {
		t.Fatal(err)
	}

	hot, ok := reloadFindField(plan.HotFields, "Host")
	if !ok || hot.Action != ActionHotReload {
		t.Fatalf("Host must be hot-reloadable, got %+v", plan.HotFields)
	}
	if hot.OldValue != "127.0.0.1" || hot.NewValue != "0.0.0.0" {
		t.Fatalf("unexpected values: %v -> %v", hot.OldValue, hot.NewValue)
	}

	if _, ok := reloadFindField(plan.RestartFields, "Port"); !ok {
		t.Fatalf("Port must require restart, got %+v", plan.RestartFields)
	}

	blocked, ok := reloadFindField(plan.Blocked, "Forbid")
	if !ok || blocked.Action != ActionBlocked {
		t.Fatalf("Forbid must be blocked, got %+v", plan.Blocked)
	}
	if !plan.HasBlocked() || len(plan.BlockedReasons()) == 0 {
		t.Fatalf("plan must report blocked reasons")
	}
}

func TestPlanner_NoChanges(t *testing.T) {
	cfg := reloadRoot{Host: "h", Port: 1}
	plan, err := NewPlanner().Plan(&cfg, &reloadRoot{Host: "h", Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.HotFields)+len(plan.RestartFields)+len(plan.Blocked)+len(plan.Groups) != 0 {
		t.Fatalf("expected empty plan, got %+v", plan)
	}
}

func TestPlanner_PointerSubconfig(t *testing.T) {
	t.Run("nil_to_set", func(t *testing.T) {
		oldCfg := reloadPtrRoot{}
		newCfg := reloadPtrRoot{Srv: &reloadSub{Addr: ":8080"}}

		plan, err := NewPlanner().Plan(oldCfg, newCfg)
		if err != nil {
			t.Fatal(err)
		}
		hot, ok := reloadFindField(plan.HotFields, "Srv.Addr")
		if !ok {
			t.Fatalf("Srv.Addr must be reported, got %+v", plan.HotFields)
		}
		if hot.OldValue != "" || hot.NewValue != ":8080" {
			t.Fatalf("unexpected values %v -> %v", hot.OldValue, hot.NewValue)
		}
	})

	t.Run("set_to_nil", func(t *testing.T) {
		oldCfg := reloadPtrRoot{Srv: &reloadSub{Addr: ":8080"}}
		newCfg := reloadPtrRoot{}

		plan, err := NewPlanner().Plan(oldCfg, newCfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reloadFindField(plan.HotFields, "Srv.Addr"); !ok {
			t.Fatalf("Srv.Addr removal must be reported, got %+v", plan.HotFields)
		}
	})

	t.Run("tag_propagates_to_pointer_subconfig", func(t *testing.T) {
		oldCfg := reloadRoot{}
		newCfg := reloadRoot{Sub: &reloadSub{Addr: ":9090"}}

		plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reloadFindField(plan.RestartFields, "Sub.Addr"); !ok {
			t.Fatalf("Sub.Addr must inherit restart tag, got %+v", plan.RestartFields)
		}
	})

	t.Run("pointer_leaf", func(t *testing.T) {
		oldV, newV := 1, 2
		oldCfg := reloadRoot{Timeout: &oldV}
		newCfg := reloadRoot{Timeout: &newV}

		plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reloadFindField(plan.HotFields, "Timeout"); !ok {
			t.Fatalf("Timeout change must be reported, got %+v", plan.HotFields)
		}
	})
}

func TestPlanner_SliceOrderMatters(t *testing.T) {
	oldCfg := reloadRoot{Hosts: []string{"a", "b"}}
	newCfg := reloadRoot{Hosts: []string{"b", "a"}}

	plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := reloadFindField(plan.HotFields, "Hosts")
	if !ok {
		t.Fatalf("Hosts reorder must be a change, got %+v", plan.HotFields)
	}
	if !reflect.DeepEqual(c.OldValue, []string{"a", "b"}) ||
		!reflect.DeepEqual(c.NewValue, []string{"b", "a"}) {
		t.Fatalf("unexpected slice values: %v -> %v", c.OldValue, c.NewValue)
	}
}

func TestPlanner_MapOrderIgnored(t *testing.T) {
	oldCfg := reloadRoot{Labels: map[string]string{"a": "1", "b": "2"}}
	newCfg := reloadRoot{Labels: map[string]string{"b": "2", "a": "1"}}

	plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.HotFields)+len(plan.RestartFields)+len(plan.Blocked)+len(plan.Groups) != 0 {
		t.Fatalf("map iteration order must not be a change, got %+v", plan)
	}
}

func TestPlanner_CustomComparerInterface(t *testing.T) {
	planner := NewPlanner()

	equalCfg := reloadRoot{Kind: "  Hot "}
	plan, err := planner.Plan(&equalCfg, &reloadRoot{Kind: "hot"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.HotFields) != 0 {
		t.Fatalf("Equal implementation must treat values as equal, got %+v", plan.HotFields)
	}

	changedCfg := reloadRoot{Kind: "cold"}
	plan, err = planner.Plan(&changedCfg, &reloadRoot{Kind: "hot"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloadFindField(plan.HotFields, "Kind"); !ok {
		t.Fatalf("Kind change must be reported, got %+v", plan.HotFields)
	}
}

func TestPlanner_CustomEqualFuncs(t *testing.T) {
	typeEqual := func(a, b any) bool {
		return strings.EqualFold(string(a.(reloadCI)), string(b.(reloadCI)))
	}
	pathEqual := func(a, b any) bool {
		return strings.TrimSpace(a.(string)) == strings.TrimSpace(b.(string))
	}
	planner := NewPlanner(
		WithTypeEqual(reloadCI(""), typeEqual),
		WithPathEqual("Host", pathEqual),
	)

	plan, err := planner.Plan(
		&reloadRoot{Host: "  host  ", CI: "ABC"},
		&reloadRoot{Host: "host", CI: "abc"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.HotFields) != 0 {
		t.Fatalf("custom funcs must treat values as equal, got %+v", plan.HotFields)
	}

	plan, err = NewPlanner().Plan(
		&reloadRoot{CI: "ABC"},
		&reloadRoot{CI: "abc"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloadFindField(plan.HotFields, "CI"); !ok {
		t.Fatalf("without custom func CI must differ, got %+v", plan.HotFields)
	}
}

func TestPlanner_GroupSwitchTogether(t *testing.T) {
	t.Run("complete_group_hot", func(t *testing.T) {
		oldCfg := reloadRoot{}
		oldCfg.DB.Name = "db1"
		oldCfg.DB.DSN = "dsn1"

		newCfg := reloadRoot{}
		newCfg.DB.Name = "db2"
		newCfg.DB.DSN = "dsn2"

		plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
		if err != nil {
			t.Fatal(err)
		}
		group, ok := reloadFindGroup(plan.Groups, "db")
		if !ok {
			t.Fatalf("db group must be planned, got %+v", plan.Groups)
		}
		if group.Action != ActionRestart {
			t.Fatalf("restart member must escalate whole group, got %s", group.Action)
		}
		for _, c := range group.Changes {
			if c.Action != ActionRestart {
				t.Fatalf("no group member may be listed as standalone hot change: %+v", c)
			}
		}
		if len(plan.HotFields) != 0 {
			t.Fatalf("grouped fields must not appear in HotFields, got %+v", plan.HotFields)
		}
	})

	t.Run("partial_group_is_blocked", func(t *testing.T) {
		oldCfg := reloadRoot{}
		oldCfg.DB.Name = "db1"
		oldCfg.DB.DSN = "dsn1"

		newCfg := reloadRoot{}
		newCfg.DB.Name = "db2"
		newCfg.DB.DSN = "dsn1"

		plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
		if err != nil {
			t.Fatal(err)
		}
		group, ok := reloadFindGroup(plan.Groups, "db")
		if !ok || group.Action != ActionBlocked {
			t.Fatalf("partial group must be blocked, got %+v", plan.Groups)
		}
		if len(group.Reasons) == 0 ||
			!strings.Contains(strings.Join(group.Reasons, ";"), "DB.DSN") {
			t.Fatalf("reason must name unchanged member, got %+v", group.Reasons)
		}
		if !plan.HasBlocked() {
			t.Fatalf("HasBlocked must be true")
		}
	})

	t.Run("forbidden_member_blocks_group", func(t *testing.T) {
		oldCfg := reloadRoot{}
		oldCfg.TLS.Cert = "cert1"
		oldCfg.TLS.Prov = "p1"

		newCfg := reloadRoot{}
		newCfg.TLS.Cert = "cert2"
		newCfg.TLS.Prov = "p2"

		plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
		if err != nil {
			t.Fatal(err)
		}
		group, ok := reloadFindGroup(plan.Groups, "tls")
		if !ok || group.Action != ActionBlocked {
			t.Fatalf("never member must block whole group, got %+v", plan.Groups)
		}
		joined := strings.Join(group.Reasons, ";")
		if !strings.Contains(joined, "TLS.Prov") {
			t.Fatalf("reason must name forbidden field, got %+v", group.Reasons)
		}

		for _, c := range group.Changes {
			if c.Action == ActionHotReload {
				t.Fatalf("peer of a forbidden field cannot be hot-applied: %+v", c)
			}
		}
	})
}

func TestPlanner_SensitiveField(t *testing.T) {
	oldCfg := reloadRoot{Secret: "super-secret"}
	newCfg := reloadRoot{Secret: "another-secret"}

	plan, err := NewPlanner().Plan(&oldCfg, &newCfg)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := reloadFindField(plan.HotFields, "Secret")
	if !ok {
		t.Fatalf("sensitive change must still be reported, got %+v", plan.HotFields)
	}
	if !c.Sensitive {
		t.Fatalf("Secret must be marked sensitive")
	}
	if c.OldValue != nil || c.NewValue != nil {
		t.Fatalf("sensitive values must not be exposed, got %v -> %v", c.OldValue, c.NewValue)
	}
}

func TestPlanner_DoesNotModifyInputs(t *testing.T) {
	oldV := 1
	oldCfg := reloadRoot{
		Host:    "h",
		Port:    80,
		Secret:  "s1",
		Forbid:  "f1",
		Timeout: &oldV,
		Labels:  map[string]string{"a": "1"},
		Hosts:   []string{"a"},
	}
	oldCfg.DB.Name = "n1"
	oldCfg.DB.DSN = "d1"

	newV := 2
	newCfg := reloadRoot{
		Host:    "h2",
		Port:    81,
		Secret:  "s2",
		Forbid:  "f2",
		Timeout: &newV,
		Labels:  map[string]string{"a": "2"},
		Hosts:   []string{"b"},
		Sub:     &reloadSub{Addr: ":1"},
	}

	oldWant := reloadClone(t, oldCfg)
	newWant := reloadClone(t, newCfg)

	planner := NewPlanner(WithPathEqual("Host", func(a, b any) bool { return false }))
	if _, err := planner.Plan(&oldCfg, &newCfg); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(oldCfg, oldWant) {
		t.Fatalf("old config was modified:\nhave %+v\nwant %+v", oldCfg, oldWant)
	}
	if !reflect.DeepEqual(newCfg, newWant) {
		t.Fatalf("new config was modified:\nhave %+v\nwant %+v", newCfg, newWant)
	}
}

func TestPlanner_PlanHasNoSideEffects(t *testing.T) {
	calls := 0
	planner := NewPlanner(WithPathEqual("Host", func(a, b any) bool {
		calls++
		return a == b
	}))

	oldCfg := reloadRoot{Host: "h1"}
	newCfg := reloadRoot{Host: "h2"}

	plan, err := planner.Plan(&oldCfg, &newCfg)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("equal func must run only for its field, got %d calls", calls)
	}

	// The returned plan is plain data: running it twice must not change state.
	again, err := planner.Plan(&oldCfg, &newCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, again) {
		t.Fatalf("planning is not deterministic:\nfirst %+v\nagain %+v", plan, again)
	}
	if calls != 2 {
		t.Fatalf("unexpected call count %d", calls)
	}
}

func TestPlanner_Errors(t *testing.T) {
	t.Run("nil_config", func(t *testing.T) {
		if _, err := NewPlanner().Plan(nil, &reloadRoot{}); err == nil {
			t.Fatal("nil config must fail")
		}
	})

	t.Run("non_struct", func(t *testing.T) {
		if _, err := NewPlanner().Plan(1, 2); err == nil {
			t.Fatal("non-struct config must fail")
		}
	})

	t.Run("type_mismatch", func(t *testing.T) {
		if _, err := NewPlanner().Plan(&reloadRoot{}, &reloadPtrRoot{}); err == nil {
			t.Fatal("different config types must fail")
		}
	})

	t.Run("invalid_tag", func(t *testing.T) {
		type badReload struct {
			X string `reload:"maybe"`
		}
		if _, err := NewPlanner().Plan(&badReload{}, &badReload{}); err == nil {
			t.Fatal("invalid reload tag must fail")
		}

		type badSensitive struct {
			X string `sensitive:"yes"`
		}
		if _, err := NewPlanner().Plan(&badSensitive{}, &badSensitive{}); err == nil {
			t.Fatal("invalid sensitive tag must fail")
		}
	})
}
