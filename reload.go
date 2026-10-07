package aconfig

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Action describes what is required to apply a configuration change.
type Action int

const (
	// ActionHotReload means the change can be applied without a restart.
	ActionHotReload Action = iota
	// ActionRestart means a process restart is required to apply the change.
	ActionRestart
	// ActionBlocked means the change is forbidden or cannot be applied.
	ActionBlocked
)

func (a Action) String() string {
	switch a {
	case ActionHotReload:
		return "hot-reload"
	case ActionRestart:
		return "restart"
	case ActionBlocked:
		return "blocked"
	default:
		return fmt.Sprintf("unknown(%d)", int(a))
	}
}

// Change is a single field that differs between two parsed configurations.
type Change struct {
	// Path is the dot-separated path of the field, e.g. "Server.Addr".
	Path string

	// Action required to apply the change.
	Action Action

	// Group the field belongs to, empty when the field is not grouped.
	Group string

	// Sensitive is true for fields declared with `sensitive:"true"`.
	// OldValue and NewValue are left nil for such changes so that only
	// the fact of the change is exposed.
	Sensitive bool

	// OldValue and NewValue are nil for sensitive fields.
	OldValue any
	NewValue any
}

// GroupPlan is a set of grouped fields that must be switched together.
type GroupPlan struct {
	// Name of the group as declared via the `reload_group` tag.
	Name string

	// Action required for the whole group. A single restart-required
	// member escalates the whole group to ActionRestart; a forbidden
	// member or an incomplete change set escalates it to ActionBlocked.
	Action Action

	// Changes are the changed fields that belong to the group.
	Changes []Change

	// Reasons explains why the group is blocked; empty otherwise.
	Reasons []string
}

// Plan describes how to transition from one parsed configuration to another.
type Plan struct {
	// HotFields are standalone changed fields applicable without a restart.
	HotFields []Change

	// RestartFields are standalone changed fields requiring a restart.
	RestartFields []Change

	// Groups are changed field groups declared to switch together.
	Groups []GroupPlan

	// Blocked holds standalone changes that cannot be applied.
	Blocked []Change
}

// HasBlocked reports whether the plan contains any change that cannot be
// applied, either a standalone forbidden field or a blocked group.
func (p Plan) HasBlocked() bool {
	if len(p.Blocked) > 0 {
		return true
	}
	for i := range p.Groups {
		if p.Groups[i].Action == ActionBlocked {
			return true
		}
	}
	return false
}

// BlockedReasons returns human-readable reasons for every blocked standalone
// change and every blocked group.
func (p Plan) BlockedReasons() []string {
	var reasons []string
	for _, c := range p.Blocked {
		reasons = append(reasons, fmt.Sprintf(
			"field %q is marked %q and cannot be changed", c.Path, "never"))
	}
	for i := range p.Groups {
		if p.Groups[i].Action != ActionBlocked {
			continue
		}
		reasons = append(reasons, p.Groups[i].Reasons...)
	}
	return reasons
}

// EqualFunc reports whether two values are equivalent.
// It must be a pure comparison: it is invoked while planning and must not
// perform external side effects or modify its arguments.
type EqualFunc func(a, b any) bool

// Comparer is implemented by configuration types that define their own
// equivalence. The receiver is compared against other, which has the same
// dynamic type (or a pointer to it).
type Comparer interface {
	Equal(other any) bool
}

// Planner compares two parsed configurations and builds a reload Plan.
//
// Fields are classified with the `reload` struct tag:
//
//   - `reload:"hot"` (default): the change applies immediately;
//   - `reload:"restart"`: the change requires a process restart;
//   - `reload:"never"`: changing the field is forbidden.
//
// Fields declared with the same `reload_group:"name"` tag must be switched
// together: a partial change blocks the whole group, and a single
// restart-required member escalates the whole group to a restart. Fields
// marked `sensitive:"true"` are compared normally, but their old and new
// values are never put into the resulting Plan.
type Planner struct {
	typeEquals map[reflect.Type]EqualFunc
	pathEquals map[string]EqualFunc
}

// PlannerOption configures a Planner.
type PlannerOption func(*Planner)

// WithTypeEqual registers a custom equivalence for values of the sample's
// type. The sample must not be nil; pass a zero value of the type, e.g.
//
//	WithTypeEqual(MyDuration(0), func(a, b any) bool { ... })
func WithTypeEqual(sample any, eq EqualFunc) PlannerOption {
	return func(p *Planner) {
		if sample == nil || eq == nil {
			return
		}
		p.typeEquals[reflect.TypeOf(sample)] = eq
	}
}

// WithPathEqual registers a custom equivalence for a single field path,
// e.g. "Server.Addr". It takes precedence over a type-registered comparison.
func WithPathEqual(path string, eq EqualFunc) PlannerOption {
	return func(p *Planner) {
		if path == "" || eq == nil {
			return
		}
		p.pathEquals[path] = eq
	}
}

// NewPlanner creates a Planner with the given comparison options.
func NewPlanner(opts ...PlannerOption) *Planner {
	p := &Planner{
		typeEquals: map[reflect.Type]EqualFunc{},
		pathEquals: map[string]EqualFunc{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Plan compares two parsed configurations and returns the reload plan.
// Both arguments must be non-nil structs (or pointers to structs) of the
// same type; neither is modified and no external side effects are performed.
func (p *Planner) Plan(oldCfg, newCfg any) (Plan, error) {
	ov, err := configValue(oldCfg)
	if err != nil {
		return Plan{}, fmt.Errorf("old config: %w", err)
	}
	nv, err := configValue(newCfg)
	if err != nil {
		return Plan{}, fmt.Errorf("new config: %w", err)
	}
	if ov.Type() != nv.Type() {
		return Plan{}, fmt.Errorf(
			"config types differ: %q and %q", ov.Type(), nv.Type())
	}

	decls, groupMembers, err := p.declare(ov.Type(), nil, "", nodeMeta{})
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{}
	groupChanges := map[string][]Change{}

	for _, d := range decls {
		av := valueAt(ov, d.index)
		bv := valueAt(nv, d.index)
		if p.equal(av, bv, d.path) {
			continue
		}

		change := Change{
			Path:      d.path,
			Action:    d.mode,
			Group:     d.group,
			Sensitive: d.sensitive,
		}
		if !d.sensitive {
			change.OldValue = av.Interface()
			change.NewValue = bv.Interface()
		}

		if d.group != "" {
			groupChanges[d.group] = append(groupChanges[d.group], change)
			continue
		}
		switch d.mode {
		case ActionRestart:
			plan.RestartFields = append(plan.RestartFields, change)
		case ActionBlocked:
			plan.Blocked = append(plan.Blocked, change)
		default:
			plan.HotFields = append(plan.HotFields, change)
		}
	}

	groupNames := make([]string, 0, len(groupChanges))
	for name := range groupChanges {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)

	for _, name := range groupNames {
		plan.Groups = append(plan.Groups, p.planGroup(
			name, groupChanges[name], groupMembers[name]))
	}
	return plan, nil
}

type nodeMeta struct {
	mode      Action
	group     string
	sensitive bool
}

type declaration struct {
	index     []int
	path      string
	mode      Action
	group     string
	sensitive bool
}

// declare walks the configuration type and collects every leaf field
// together with its reload metadata. Tags set on a struct propagate to all
// of its leaves unless overridden on a nested field.
func (p *Planner) declare(t reflect.Type, index []int, path string, meta nodeMeta) ([]declaration, map[string][]string, error) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	decls := []declaration{}
	groupMembers := map[string][]string{}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		fieldPath := field.Name
		if path != "" {
			fieldPath = path + "." + field.Name
		}
		fieldIndex := append(append([]int(nil), index...), i)
		fieldMeta := meta

		if tag := field.Tag.Get("reload"); tag != "" {
			switch tag {
			case "hot":
				fieldMeta.mode = ActionHotReload
			case "restart":
				fieldMeta.mode = ActionRestart
			case "never":
				fieldMeta.mode = ActionBlocked
			default:
				return nil, nil, fmt.Errorf(
					"aconfig: invalid `reload` tag %q on field %q, want hot, restart or never",
					tag, fieldPath)
			}
		}
		if tag := field.Tag.Get("reload_group"); tag != "" {
			fieldMeta.group = tag
		}
		if tag := field.Tag.Get("sensitive"); tag != "" {
			if tag != "true" {
				return nil, nil, fmt.Errorf(
					"aconfig: invalid `sensitive` tag %q on field %q, want \"true\"",
					tag, fieldPath)
			}
			fieldMeta.sensitive = true
		}

		fieldType := field.Type
		if fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct {
			sub, subGroups, err := p.declare(field.Type, fieldIndex, fieldPath, fieldMeta)
			if err != nil {
				return nil, nil, err
			}
			decls = append(decls, sub...)
			for group, members := range subGroups {
				groupMembers[group] = append(groupMembers[group], members...)
			}
			continue
		}

		d := declaration{
			index:     fieldIndex,
			path:      fieldPath,
			mode:      fieldMeta.mode,
			group:     fieldMeta.group,
			sensitive: fieldMeta.sensitive,
		}
		decls = append(decls, d)
		if d.group != "" {
			groupMembers[d.group] = append(groupMembers[d.group], d.path)
		}
	}
	return decls, groupMembers, nil
}

func (p *Planner) planGroup(name string, changes []Change, members []string) GroupPlan {
	group := GroupPlan{
		Name:    name,
		Changes: changes,
	}

	action := ActionHotReload
	changed := make(map[string]struct{}, len(changes))
	for _, c := range changes {
		changed[c.Path] = struct{}{}
		if c.Action > action {
			action = c.Action
		}
	}

	var unchanged []string
	for _, member := range members {
		if _, ok := changed[member]; !ok {
			unchanged = append(unchanged, member)
		}
	}
	sort.Strings(unchanged)

	sort.SliceStable(changes, func(i, j int) bool {
		return changes[i].Path < changes[j].Path
	})

	switch {
	case len(unchanged) > 0:
		group.Action = ActionBlocked
		group.Reasons = append(group.Reasons, fmt.Sprintf(
			"group %q must be switched together, unchanged member(s): %s",
			name, strings.Join(unchanged, ", ")))
	case action == ActionBlocked:
		group.Action = ActionBlocked
	default:
		group.Action = action
	}

	for i := range group.Changes {
		group.Changes[i].Action = group.Action
	}

	for _, c := range changes {
		if c.Action == ActionBlocked {
			group.Reasons = append(group.Reasons, fmt.Sprintf(
				"field %q in group %q is marked %q and cannot be changed",
				c.Path, name, "never"))
		}
	}
	return group
}

func (p *Planner) equal(av, bv reflect.Value, path string) bool {
	if eq := p.pathEquals[path]; eq != nil {
		return eq(av.Interface(), bv.Interface())
	}
	if eq := p.typeEquals[av.Type()]; eq != nil {
		return eq(av.Interface(), bv.Interface())
	}

	ai, bi := av.Interface(), bv.Interface()
	if cmp, ok := ai.(Comparer); ok {
		return cmp.Equal(bi)
	}
	if av.CanAddr() && bv.CanAddr() {
		if cmp, ok := av.Addr().Interface().(Comparer); ok {
			return cmp.Equal(bv.Addr().Interface())
		}
	}
	return reflect.DeepEqual(ai, bi)
}

func configValue(cfg any) (reflect.Value, error) {
	if cfg == nil {
		return reflect.Value{}, fmt.Errorf("configuration is nil")
	}
	v := reflect.ValueOf(cfg)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}, fmt.Errorf("configuration is nil")
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("configuration must be a struct, got %q", v.Kind())
	}
	return v, nil
}

// valueAt walks the index path into the configuration. A nil pointer to a
// sub-struct is replaced with a fresh zero value so the missing side
// compares as zero-valued; the input configuration is never modified.
func valueAt(root reflect.Value, index []int) reflect.Value {
	v := root
	for _, i := range index {
		if v.Kind() == reflect.Ptr && v.Type().Elem().Kind() == reflect.Struct {
			if v.IsNil() {
				v = reflect.New(v.Type().Elem()).Elem()
			} else {
				v = v.Elem()
			}
		}
		v = v.Field(i)
	}
	return v
}
