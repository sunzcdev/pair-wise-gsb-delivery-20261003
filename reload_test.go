package aconfig

import (
	"reflect"
	"strings"
	"testing"
)

type reloadSub struct {
	Host string
	Port int
}

type reloadConfig struct {
	Name     string   `reload:"hot"`
	LogLevel string   `reload:"hot"`
	WorkDir  string   `reload:"restart"`
	DataDir  string   `reload:"forbidden"`
	Password string   `reload:"hot" sensitive:"true"`
	Backends []string `reload:"hot"`
	Labels   map[string]string
	Database *reloadSub `reload_group:"database"`
	PoolSize int        `reload:"hot" reload_group:"database"`
	Timeout  *Duration
	Plain    int
}

// Duration is a custom config type compared through a custom EqualsFunc.
type Duration struct {
	Seconds int
}

func changePaths(changes []*FieldChange) []string {
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	return paths
}

func groupNames(groups []*ChangeGroup) []string {
	names := make([]string, len(groups))
	for i, g := range groups {
		names[i] = g.Name
	}
	return names
}

func TestPlanClassifiesActions(t *testing.T) {
	planner, err := PlannerFor(&reloadConfig{})
	if err != nil {
		t.Fatal(err)
	}

	old := &reloadConfig{
		Name:     "svc",
		LogLevel: "info",
		WorkDir:  "/srv",
		DataDir:  "/data",
		Password: "secret-a",
		Plain:    1,
	}
	newCfg := &reloadConfig{
		Name:     "svc2",
		LogLevel: "debug",
		WorkDir:  "/srv-new",
		DataDir:  "/data-new",
		Password: "secret-b",
		Plain:    2,
	}

	plan, err := planner.Plan(old, newCfg)
	if err != nil {
		t.Fatal(err)
	}

	if got := groupNames(plan.Hot); !reflect.DeepEqual(got, []string{"LogLevel", "Name", "Password"}) {
		t.Fatalf("hot groups = %v", got)
	}
	if got := groupNames(plan.Restart); !reflect.DeepEqual(got, []string{"Plain", "WorkDir"}) {
		t.Fatalf("restart groups = %v", got)
	}
	if got := groupNames(plan.Blocked); !reflect.DeepEqual(got, []string{"DataDir"}) {
		t.Fatalf("blocked groups = %v", got)
	}
	if plan.ApplyNow() {
		t.Fatal("plan with restart/forbidden changes must not apply now")
	}

	var passwordChange *FieldChange
	for _, c := range plan.Changes {
		if c.Path == "Password" {
			passwordChange = c
		}
	}
	if passwordChange == nil {
		t.Fatal("password change missing")
	}
	if !passwordChange.Sensitive || passwordChange.Old != nil || passwordChange.New != nil {
		t.Fatalf("sensitive change leaked values: %+v", passwordChange)
	}
	if rendered := passwordChange.String(); strings.Contains(rendered, "secret") {
		t.Fatalf("sensitive value rendered: %s", rendered)
	}
}

func TestPlanPointerSubconfig(t *testing.T) {
	planner, err := PlannerFor(&reloadConfig{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nil to non-nil is one change", func(t *testing.T) {
		plan, err := planner.Plan(
			&reloadConfig{Database: nil},
			&reloadConfig{Database: &reloadSub{Host: "db", Port: 5432}},
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := changePaths(plan.Changes); !reflect.DeepEqual(got, []string{"Database"}) {
			t.Fatalf("changes = %v", got)
		}
	})

	t.Run("nested fields compared when both non-nil", func(t *testing.T) {
		plan, err := planner.Plan(
			&reloadConfig{Database: &reloadSub{Host: "db", Port: 5432}},
			&reloadConfig{Database: &reloadSub{Host: "db", Port: 6432}},
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := changePaths(plan.Changes); !reflect.DeepEqual(got, []string{"Database.Port"}) {
			t.Fatalf("changes = %v", got)
		}
		if len(plan.Changes) != 1 || plan.Changes[0].Action != ActionRestart {
			t.Fatalf("nested field must inherit default action, got %+v", plan.Changes)
		}
	})
}

func TestPlanSliceOrderIsChange(t *testing.T) {
	planner, err := PlannerFor(&reloadConfig{})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := planner.Plan(
		&reloadConfig{Backends: []string{"a", "b"}},
		&reloadConfig{Backends: []string{"b", "a"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != "Backends" {
		t.Fatalf("reordered slice must be a change, got %+v", plan.Changes)
	}
}

func TestPlanMapOrderIsNotChange(t *testing.T) {
	planner, err := PlannerFor(&reloadConfig{})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := planner.Plan(
		&reloadConfig{Labels: map[string]string{"a": "1", "b": "2"}},
		&reloadConfig{Labels: map[string]string{"b": "2", "a": "1"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("map iteration order must not be a change, got %+v", plan.Changes)
	}
}

func TestPlanCustomTypeWithEquals(t *testing.T) {
	calls := 0
	planner, err := PlannerFor(&reloadConfig{},
		WithFieldAction("Timeout", ActionHot),
		WithEquals("Timeout", func(oldValue, newValue any) bool {
			calls++
			oldD := oldValue.(*Duration)
			newD := newValue.(*Duration)
			if oldD == nil || newD == nil {
				return oldD == newD
			}
			return oldD.Seconds == newD.Seconds
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := planner.Plan(
		&reloadConfig{Timeout: &Duration{Seconds: 30}},
		&reloadConfig{Timeout: &Duration{Seconds: 30}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("custom equals decided values are equivalent, got %+v", plan.Changes)
	}

	plan, err = planner.Plan(
		&reloadConfig{Timeout: &Duration{Seconds: 30}},
		&reloadConfig{Timeout: &Duration{Seconds: 60}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != "Timeout" || plan.Changes[0].Action != ActionHot {
		t.Fatalf("custom type change misreported: %+v", plan.Changes)
	}
	if calls == 0 {
		t.Fatal("custom equals function was never called")
	}
}

func TestPlanGroupSwitchesAtomically(t *testing.T) {
	planner, err := PlannerFor(&reloadConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// PoolSize is hot but Database.Port defaults to restart; they share a group,
	// so PoolSize must not be offered as a standalone hot group.
	plan, err := planner.Plan(
		&reloadConfig{
			Database: &reloadSub{Host: "db", Port: 5432},
			PoolSize: 10,
		},
		&reloadConfig{
			Database: &reloadSub{Host: "db", Port: 6432},
			PoolSize: 20,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if got := groupNames(plan.Hot); len(got) != 0 {
		t.Fatalf("hot peers of a restart group cannot apply alone, got %v", got)
	}
	if len(plan.Restart) != 1 || plan.Restart[0].Name != "database" {
		t.Fatalf("group must be one restart unit, got %+v", plan.Restart)
	}
	group := plan.Restart[0]
	if got := changePaths(group.Fields); !reflect.DeepEqual(got, []string{"Database.Port", "PoolSize"}) {
		t.Fatalf("group fields = %v", got)
	}
	if len(group.Reasons) != 1 || !strings.Contains(group.Reasons[0], "requires a restart") {
		t.Fatalf("group reasons = %v", group.Reasons)
	}
}

func TestPlanGroupWithForbiddenIsBlocked(t *testing.T) {
	type cfg struct {
		A string `reload:"hot" reload_group:"g"`
		B string `reload:"forbidden" reload_group:"g"`
	}
	planner, err := PlannerFor(&cfg{})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := planner.Plan(&cfg{A: "1", B: "2"}, &cfg{A: "3", B: "4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Hot) != 0 || len(plan.Restart) != 0 {
		t.Fatalf("forbidden group cannot be hot or restart, got hot=%v restart=%v", plan.Hot, plan.Restart)
	}
	if len(plan.Blocked) != 1 || plan.Blocked[0].Name != "g" {
		t.Fatalf("group must be blocked, got %+v", plan.Blocked)
	}
	if len(plan.Blocked[0].Reasons) == 0 {
		t.Fatal("blocked group must explain why")
	}
}

func TestPlanDoesNotMutateOrHaveSideEffects(t *testing.T) {
	var equalsCalls int
	planner, err := NewPlanner(
		WithFieldAction("Name", ActionHot),
		WithEquals("Name", func(oldValue, newValue any) bool {
			equalsCalls++
			return oldValue == newValue
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	type plain struct {
		Name string
	}
	old := &plain{Name: "a"}
	newCfg := &plain{Name: "b"}

	plan1, err := planner.Plan(old, newCfg)
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := planner.Plan(old, newCfg)
	if err != nil {
		t.Fatal(err)
	}

	if old.Name != "a" || newCfg.Name != "b" {
		t.Fatalf("inputs were mutated: %+v %+v", old, newCfg)
	}
	if !reflect.DeepEqual(changePaths(plan1.Changes), changePaths(plan2.Changes)) {
		t.Fatal("repeated planning must be deterministic")
	}
	if equalsCalls != 2 {
		t.Fatalf("equals called %d times, expected 2 (pure, no caching side effects)", equalsCalls)
	}
}

func TestPlanNoChanges(t *testing.T) {
	planner, err := PlannerFor(&reloadConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &reloadConfig{Name: "svc", Database: &reloadSub{Host: "db"}}

	plan, err := planner.Plan(cfg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 || !plan.ApplyNow() {
		t.Fatalf("identical configs must yield empty plan, got %+v", plan)
	}
}

func TestPlannerErrors(t *testing.T) {
	if _, err := PlannerFor(123); err == nil {
		t.Fatal("non-struct sample must fail")
	}
	if _, err := PlannerFor(&reloadConfig{}, WithFieldAction("Nope", ActionHot)); err == nil {
		t.Fatal("unknown option path must fail")
	}
	if _, err := NewPlanner(WithFieldAction("X", ReloadAction(42))); err == nil {
		t.Fatal("invalid action must fail")
	}
	if _, err := NewPlanner(WithEquals("X", nil)); err == nil {
		t.Fatal("nil equals must fail")
	}

	type badTag struct {
		F string `reload:"maybe"`
	}
	if _, err := PlannerFor(&badTag{}); err == nil {
		t.Fatal("invalid reload tag value must fail")
	}

	planner, err := PlannerFor(&reloadConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Plan(&reloadConfig{}, &reloadSub{}); err == nil {
		t.Fatal("comparing different types must fail")
	}
}
