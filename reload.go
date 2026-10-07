package aconfig

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ReloadAction describes what an operator has to do for a field change to take effect.
type ReloadAction int

const (
	// ActionHot marks a field that can be reloaded in place without restarting.
	ActionHot ReloadAction = iota
	// ActionRestart marks a field that only takes effect after a process restart.
	ActionRestart
	// ActionForbidden marks a field that is not allowed to change at runtime.
	ActionForbidden
)

// String returns the tag/CLI friendly name of the action.
func (a ReloadAction) String() string {
	switch a {
	case ActionHot:
		return "hot"
	case ActionRestart:
		return "restart"
	case ActionForbidden:
		return "forbidden"
	default:
		return "unknown"
	}
}

// EqualsFunc compares old and new value of a field and reports whether they
// are equivalent. It must be a pure function: the planner never applies the
// result and the function must not perform external side effects.
type EqualsFunc func(oldValue, newValue any) bool

// FieldChange is a single leaf field that differs between two configs.
//
// For sensitive fields Old and New are always nil, only the fact of the
// change (and the field path) is exposed.
type FieldChange struct {
	Path      string
	Action    ReloadAction
	Sensitive bool
	Old       any
	New       any
}

// String renders the change, redacting sensitive values.
func (c *FieldChange) String() string {
	if c.Sensitive {
		return fmt.Sprintf("%s changed (sensitive)", c.Path)
	}
	return fmt.Sprintf("%s: %v -> %v", c.Path, c.Old, c.New)
}

// ChangeGroup is an atomic unit of change: either a declared field group or a
// single ungrouped field. A group containing a restart field is reported as a
// whole restart unit, its hot peers are never offered for a standalone apply.
type ChangeGroup struct {
	Name    string
	Fields  []*FieldChange
	Action  ReloadAction
	Reasons []string
}

// Plan is the result of comparing two parsed configurations.
type Plan struct {
	// Changes holds every changed leaf field, sorted by path.
	Changes []*FieldChange
	// Hot groups can be applied immediately.
	Hot []*ChangeGroup
	// Restart groups take effect only after a process restart.
	Restart []*ChangeGroup
	// Blocked groups contain at least one field that must never change.
	Blocked []*ChangeGroup
}

// ApplyNow reports whether every change can be applied without a restart and
// without touching forbidden fields.
func (p *Plan) ApplyNow() bool {
	return len(p.Restart) == 0 && len(p.Blocked) == 0
}

// String renders the plan grouped by the required action.
func (p *Plan) String() string {
	var b strings.Builder
	writeGroups := func(title string, groups []*ChangeGroup) {
		if len(groups) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s:\n", title)
		for _, g := range groups {
			paths := make([]string, len(g.Fields))
			for i, f := range g.Fields {
				paths[i] = f.Path
			}
			fmt.Fprintf(&b, "  - %s: %s\n", g.Name, strings.Join(paths, ", "))
			for _, reason := range g.Reasons {
				fmt.Fprintf(&b, "      %s\n", reason)
			}
		}
	}
	writeGroups("hot reload", p.Hot)
	writeGroups("restart required", p.Restart)
	writeGroups("blocked", p.Blocked)
	return strings.TrimRight(b.String(), "\n")
}

// Planner compares two parsed configurations and decides which changes can be
// hot-reloaded, which require a restart and which are forbidden.
//
// The planner is read-only: Plan neither mutates its inputs nor performs any
// external side effect, it can be called repeatedly with the same result.
type Planner struct {
	defaultAction ReloadAction
	actions       map[string]ReloadAction
	groups        map[string]string
	sensitive     map[string]bool
	equals        map[string]EqualsFunc
	optionPaths   []string
}

// PlannerOption configures a Planner.
type PlannerOption func(*Planner) error

// WithDefaultAction sets the action for fields without an explicit mark.
// Defaults to ActionRestart, the safe choice for unmarked fields.
func WithDefaultAction(action ReloadAction) PlannerOption {
	return func(p *Planner) error {
		if !validAction(action) {
			return fmt.Errorf("aconfig: invalid default reload action %d", action)
		}
		p.defaultAction = action
		return nil
	}
}

// WithFieldAction marks a field path (or a whole sub-config prefix) with an
// action. A more specific path overrides a less specific one.
func WithFieldAction(path string, action ReloadAction) PlannerOption {
	return func(p *Planner) error {
		if path == "" {
			return errors.New("aconfig: field path cannot be empty")
		}
		if !validAction(action) {
			return fmt.Errorf("aconfig: invalid reload action %d for %q", action, path)
		}
		p.actions[path] = action
		p.optionPaths = append(p.optionPaths, path)
		return nil
	}
}

// WithSensitiveField marks a field path as sensitive so that a change exposes
// only the fact that the value changed, never the values themselves.
func WithSensitiveField(path string) PlannerOption {
	return func(p *Planner) error {
		if path == "" {
			return errors.New("aconfig: field path cannot be empty")
		}
		p.sensitive[path] = true
		p.optionPaths = append(p.optionPaths, path)
		return nil
	}
}

// WithReloadGroup declares that the given fields must switch together as one
// atomic unit. A restart or forbidden field in the group upgrades the whole
// group, its other members are never applied standalone.
func WithReloadGroup(name string, fields ...string) PlannerOption {
	return func(p *Planner) error {
		if name == "" {
			return errors.New("aconfig: reload group name cannot be empty")
		}
		for _, field := range fields {
			if field == "" {
				return fmt.Errorf("aconfig: reload group %q contains an empty field path", name)
			}
			p.groups[field] = name
			p.optionPaths = append(p.optionPaths, field)
		}
		return nil
	}
}

// WithEquals registers a custom equivalence function for a field path. When
// present it replaces the default type-based comparison for the whole field,
// including pointer and struct fields.
func WithEquals(path string, fn EqualsFunc) PlannerOption {
	return func(p *Planner) error {
		if path == "" {
			return errors.New("aconfig: field path cannot be empty")
		}
		if fn == nil {
			return fmt.Errorf("aconfig: equals function for %q cannot be nil", path)
		}
		p.equals[path] = fn
		p.optionPaths = append(p.optionPaths, path)
		return nil
	}
}

// NewPlanner creates a Planner configured only through options.
func NewPlanner(opts ...PlannerOption) (*Planner, error) {
	p := &Planner{
		defaultAction: ActionRestart,
		actions:       map[string]ReloadAction{},
		groups:        map[string]string{},
		sensitive:     map[string]bool{},
		equals:        map[string]EqualsFunc{},
	}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// PlannerFor creates a Planner from the struct tags of sample and additional
// options. Options take precedence over tags.
//
// Recognized tags:
//
//	reload:"hot|restart|forbidden" - how the field can be changed
//	reload_group:"name"            - the field switches together with its group
//	sensitive:"true"               - values are never exposed in the plan
//
// Tags on a struct field apply to the whole sub-config unless a nested field
// overrides them. The sample may be a value or a pointer to the config struct.
func PlannerFor(sample any, opts ...PlannerOption) (*Planner, error) {
	if sample == nil {
		return nil, errors.New("aconfig: sample config cannot be nil")
	}
	t := reflect.TypeOf(sample)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("aconfig: sample config must be a struct, got %s", t.Kind())
	}

	p, err := NewPlanner()
	if err != nil {
		return nil, err
	}
	if err := p.scanTags(t, "", ""); err != nil {
		return nil, err
	}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}

	known := map[string]struct{}{}
	collectPaths(t, "", known)
	for _, path := range p.optionPaths {
		if _, ok := known[path]; !ok {
			return nil, fmt.Errorf("aconfig: unknown field path %q", path)
		}
	}
	return p, nil
}

// Plan compares two parsed configurations and returns a reload plan.
func (p *Planner) Plan(oldValue, newValue any) (*Plan, error) {
	oldV, newV, err := prepareValues(oldValue, newValue)
	if err != nil {
		return nil, err
	}

	changes := []*FieldChange{}
	p.walk(oldV, newV, "", &changes)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	plan := &Plan{Changes: changes}
	grouped := map[string][]*FieldChange{}
	var singles []*FieldChange
	for _, change := range changes {
		if name, ok := longestPrefix(p.groups, change.Path); ok {
			grouped[name] = append(grouped[name], change)
		} else {
			singles = append(singles, change)
		}
	}

	names := make([]string, 0, len(grouped))
	for name := range grouped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		plan.addGroup(name, grouped[name])
	}
	for _, change := range singles {
		plan.addGroup(change.Path, []*FieldChange{change})
	}

	sortGroups(plan.Hot)
	sortGroups(plan.Restart)
	sortGroups(plan.Blocked)
	return plan, nil
}

func (p *Plan) addGroup(name string, fields []*FieldChange) {
	group := &ChangeGroup{Name: name, Fields: fields, Action: ActionHot}
	for _, field := range fields {
		switch field.Action {
		case ActionForbidden:
			group.Action = ActionForbidden
			group.Reasons = append(group.Reasons, fmt.Sprintf("field %q is not allowed to change", field.Path))
		case ActionRestart:
			if group.Action != ActionForbidden {
				group.Action = ActionRestart
			}
			group.Reasons = append(group.Reasons, fmt.Sprintf("field %q requires a restart", field.Path))
		}
	}
	switch group.Action {
	case ActionHot:
		p.Hot = append(p.Hot, group)
	case ActionRestart:
		p.Restart = append(p.Restart, group)
	case ActionForbidden:
		p.Blocked = append(p.Blocked, group)
	}
}

func validAction(action ReloadAction) bool {
	return action == ActionHot || action == ActionRestart || action == ActionForbidden
}

func sortGroups(groups []*ChangeGroup) {
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
}

func prepareValues(oldValue, newValue any) (reflect.Value, reflect.Value, error) {
	if oldValue == nil || newValue == nil {
		return reflect.Value{}, reflect.Value{}, errors.New("aconfig: compared configs cannot be nil")
	}
	oldV := reflect.ValueOf(oldValue)
	newV := reflect.ValueOf(newValue)
	if oldV.Type() != newV.Type() {
		return reflect.Value{}, reflect.Value{}, fmt.Errorf("aconfig: cannot compare %s with %s", oldV.Type(), newV.Type())
	}
	for oldV.Kind() == reflect.Ptr {
		if oldV.IsNil() || newV.IsNil() {
			return reflect.Value{}, reflect.Value{}, errors.New("aconfig: cannot compare a nil root config")
		}
		oldV = oldV.Elem()
		newV = newV.Elem()
	}
	if oldV.Kind() != reflect.Struct {
		return reflect.Value{}, reflect.Value{}, fmt.Errorf("aconfig: config must be a struct, got %s", oldV.Kind())
	}
	return oldV, newV, nil
}

func (p *Planner) walk(oldV, newV reflect.Value, prefix string, changes *[]*FieldChange) {
	t := oldV.Type()
	count := t.NumField()
	for i := 0; i < count; i++ {
		structField := t.Field(i)
		if structField.PkgPath != "" {
			continue // unexported field
		}
		oldField := oldV.Field(i)
		newField := newV.Field(i)

		if structField.Anonymous && isEmbedStruct(structField.Type) {
			// Anonymous sub-configs are flattened, mirroring aconfig naming.
			path := prefix
			if oldField.Kind() == reflect.Ptr {
				p.walkEmbeddedPtr(oldField, newField, path, changes)
				continue
			}
			p.walk(oldField, newField, path, changes)
			continue
		}

		p.walkField(oldField, newField, joinPath(prefix, structField.Name), changes)
	}
}

func (p *Planner) walkEmbeddedPtr(oldV, newV reflect.Value, path string, changes *[]*FieldChange) {
	if oldV.IsNil() || newV.IsNil() {
		if oldV.IsNil() != newV.IsNil() && path != "" {
			p.record(path, oldV, newV, changes)
		}
		return
	}
	p.walk(oldV.Elem(), newV.Elem(), path, changes)
}

func isEmbedStruct(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && countExportedFields(t) > 0
}

func (p *Planner) walkField(oldV, newV reflect.Value, path string, changes *[]*FieldChange) {
	if fn, ok := p.equals[path]; ok {
		if !fn(oldV.Interface(), newV.Interface()) {
			p.record(path, oldV, newV, changes)
		}
		return
	}

	switch oldV.Kind() {
	case reflect.Ptr:
		switch {
		case oldV.IsNil() && newV.IsNil():
			return
		case oldV.IsNil() != newV.IsNil():
			p.record(path, oldV, newV, changes)
			return
		}
		elemType := oldV.Type().Elem()
		if elemType.Kind() == reflect.Struct && countExportedFields(elemType) > 0 {
			p.walk(oldV.Elem(), newV.Elem(), path, changes)
			return
		}
		p.walkField(oldV.Elem(), newV.Elem(), path, changes)

	case reflect.Struct:
		if countExportedFields(oldV.Type()) == 0 {
			if !reflect.DeepEqual(oldV.Interface(), newV.Interface()) {
				p.record(path, oldV, newV, changes)
			}
			return
		}
		p.walk(oldV, newV, path, changes)

	default:
		// reflect.DeepEqual is order-sensitive for slices and order-insensitive
		// for maps, exactly the semantics the planner requires.
		if !reflect.DeepEqual(oldV.Interface(), newV.Interface()) {
			p.record(path, oldV, newV, changes)
		}
	}
}

func (p *Planner) record(path string, oldV, newV reflect.Value, changes *[]*FieldChange) {
	action := p.defaultAction
	if marked, ok := longestPrefixAction(p.actions, path); ok {
		action = marked
	}
	change := &FieldChange{
		Path:      path,
		Action:    action,
		Sensitive: p.isSensitive(path),
		Old:       oldV.Interface(),
		New:       newV.Interface(),
	}
	if change.Sensitive {
		change.Old = nil
		change.New = nil
	}
	*changes = append(*changes, change)
}

func (p *Planner) isSensitive(path string) bool {
	v, _ := longestPrefixBool(p.sensitive, path)
	return v
}

func (p *Planner) scanTags(t reflect.Type, prefix, inheritedGroup string) error {
	count := t.NumField()
	for i := 0; i < count; i++ {
		structField := t.Field(i)
		if structField.PkgPath != "" {
			continue
		}
		path := joinPath(prefix, structField.Name)
		if structField.Anonymous {
			path = prefix
		}

		if path != "" {
			switch value := structField.Tag.Get("reload"); value {
			case "":
			case "hot":
				p.actions[path] = ActionHot
			case "restart":
				p.actions[path] = ActionRestart
			case "forbidden":
				p.actions[path] = ActionForbidden
			default:
				return fmt.Errorf("aconfig: invalid reload tag %q on %q", value, path)
			}

			if value := structField.Tag.Get("sensitive"); value == "true" {
				p.sensitive[path] = true
			}
		}

		group := inheritedGroup
		if value := structField.Tag.Get("reload_group"); value != "" {
			group = value
		}
		if path != "" && group != "" {
			p.groups[path] = group
		}

		fieldType := structField.Type
		for fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct && countExportedFields(fieldType) > 0 {
			nestedPrefix := path
			if structField.Anonymous {
				nestedPrefix = prefix
			}
			if err := p.scanTags(fieldType, nestedPrefix, group); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectPaths(t reflect.Type, prefix string, paths map[string]struct{}) {
	count := t.NumField()
	for i := 0; i < count; i++ {
		structField := t.Field(i)
		if structField.PkgPath != "" {
			continue
		}
		if structField.Anonymous && isEmbedStruct(structField.Type) {
			fieldType := structField.Type
			for fieldType.Kind() == reflect.Ptr {
				fieldType = fieldType.Elem()
			}
			collectPaths(fieldType, prefix, paths)
			continue
		}

		path := joinPath(prefix, structField.Name)
		paths[path] = struct{}{}

		fieldType := structField.Type
		for fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct && countExportedFields(fieldType) > 0 {
			collectPaths(fieldType, path, paths)
		}
	}
}

func countExportedFields(t reflect.Type) int {
	count := 0
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).PkgPath == "" {
			count++
		}
	}
	return count
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func longestPrefix(m map[string]string, path string) (string, bool) {
	bestLen := -1
	var value string
	for key, v := range m {
		if key == path || strings.HasPrefix(path, key+".") {
			if len(key) > bestLen {
				bestLen, value = len(key), v
			}
		}
	}
	return value, bestLen >= 0
}

func longestPrefixAction(m map[string]ReloadAction, path string) (ReloadAction, bool) {
	bestLen := -1
	var action ReloadAction
	for key, value := range m {
		if (key == path || strings.HasPrefix(path, key+".")) && len(key) > bestLen {
			bestLen, action = len(key), value
		}
	}
	return action, bestLen >= 0
}

func longestPrefixBool(m map[string]bool, path string) (bool, bool) {
	bestLen := -1
	var value bool
	for key, v := range m {
		if (key == path || strings.HasPrefix(path, key+".")) && len(key) > bestLen {
			bestLen, value = len(key), v
		}
	}
	return value, bestLen >= 0
}
