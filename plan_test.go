package mapstructure

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func findStep(t *testing.T, plan *AssignmentPlan, path string) PlanStep {
	t.Helper()
	for _, s := range plan.Steps {
		if s.Path == path {
			return s
		}
	}
	t.Fatalf("no step with path %q in plan: %v", path, plan.Steps)
	return PlanStep{}
}

func TestPlanStructSteps(t *testing.T) {
	type Server struct {
		Name    string
		Port    int
		Tags    []string
		Missing string
	}

	target := &Server{}
	input := map[string]any{
		"name": "web",
		"port": "8080",
		"tags": []any{"a", "b"},
	}
	config := &DecoderConfig{Result: target, WeaklyTypedInput: true}

	plan, err := Plan(config, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	if plan.Opaque() {
		t.Fatalf("plan should not be opaque: %v", plan.OpaqueSteps())
	}

	name := findStep(t, plan, "Name")
	if name.Action != ActionSet || name.Conversion != "string -> string" {
		t.Errorf("unexpected Name step: %+v", name)
	}

	port := findStep(t, plan, "Port")
	if port.Action != ActionSet || !strings.Contains(port.Conversion, "string -> int") || !strings.Contains(port.Conversion, "weak") {
		t.Errorf("unexpected Port step: %+v", port)
	}

	tags := findStep(t, plan, "Tags")
	if tags.Action != ActionSet {
		t.Errorf("unexpected Tags step: %+v", tags)
	}
	findStep(t, plan, "Tags[0]")
	findStep(t, plan, "Tags[1]")

	missing := findStep(t, plan, "Missing")
	if missing.Action != ActionPreserve {
		t.Errorf("expected Missing to be preserved, got %+v", missing)
	}

	// Planning must not modify the target.
	if !reflect.DeepEqual(*target, Server{}) {
		t.Errorf("target was modified during planning: %+v", *target)
	}

	// Apply after approval.
	if err := plan.Apply(target, nil); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}
	expected := Server{Name: "web", Port: 8080, Tags: []string{"a", "b"}}
	if !reflect.DeepEqual(*target, expected) {
		t.Errorf("unexpected result: got %+v, want %+v", *target, expected)
	}
}

func TestPlanNestedMapMerge(t *testing.T) {
	type Config struct {
		Labels map[string]string
	}

	input := map[string]any{
		"labels": map[string]any{"env": "prod"},
	}

	// ZeroFields off: the existing map is merged.
	target := &Config{Labels: map[string]string{"existing": "keep"}}
	plan, err := Plan(&DecoderConfig{Result: target}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	labels := findStep(t, plan, "Labels")
	if labels.Action != ActionSet || !strings.Contains(labels.Note, "merged") {
		t.Errorf("expected merge note, got %+v", labels)
	}
	env := findStep(t, plan, "Labels[env]")
	if env.Action != ActionSet {
		t.Errorf("unexpected Labels[env] step: %+v", env)
	}

	// Planning must not touch the existing map.
	if !reflect.DeepEqual(target.Labels, map[string]string{"existing": "keep"}) {
		t.Errorf("target map modified during planning: %v", target.Labels)
	}

	if err := plan.Apply(target, nil); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}
	expected := map[string]string{"existing": "keep", "env": "prod"}
	if !reflect.DeepEqual(target.Labels, expected) {
		t.Errorf("unexpected merged map: got %v, want %v", target.Labels, expected)
	}

	// ZeroFields on: the existing map is replaced.
	zeroTarget := &Config{Labels: map[string]string{"existing": "keep"}}
	zeroPlan, err := Plan(&DecoderConfig{Result: zeroTarget, ZeroFields: true}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}
	zeroLabels := findStep(t, zeroPlan, "Labels")
	if !strings.Contains(zeroLabels.Note, "ZeroFields") {
		t.Errorf("expected ZeroFields note, got %+v", zeroLabels)
	}
	if err := zeroPlan.Apply(zeroTarget, nil); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}
	if !reflect.DeepEqual(zeroTarget.Labels, map[string]string{"env": "prod"}) {
		t.Errorf("unexpected replaced map: %v", zeroTarget.Labels)
	}
}

func TestPlanZeroFieldsNilInput(t *testing.T) {
	type Config struct {
		Name string
	}

	input := map[string]any{"name": nil}

	// With ZeroFields a nil input zeroes the field.
	target := &Config{Name: "old"}
	plan, err := Plan(&DecoderConfig{Result: target, ZeroFields: true}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}
	step := findStep(t, plan, "Name")
	if step.Action != ActionZero {
		t.Errorf("expected zero action, got %+v", step)
	}
	if target.Name != "old" {
		t.Errorf("target modified during planning: %q", target.Name)
	}
	if err := plan.Apply(target, nil); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}
	if target.Name != "" {
		t.Errorf("expected field to be zeroed, got %q", target.Name)
	}

	// Without ZeroFields a nil input preserves the field.
	target2 := &Config{Name: "old"}
	plan2, err := Plan(&DecoderConfig{Result: target2}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}
	step2 := findStep(t, plan2, "Name")
	if step2.Action != ActionPreserve {
		t.Errorf("expected preserve action, got %+v", step2)
	}
	if err := plan2.Apply(target2, nil); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}
	if target2.Name != "old" {
		t.Errorf("expected field to be preserved, got %q", target2.Name)
	}
}

func TestPlanNilPointerAllocation(t *testing.T) {
	type Inner struct {
		Port int
	}
	type Config struct {
		Inner *Inner
	}

	target := &Config{}
	input := map[string]any{
		"inner": map[string]any{"port": 80},
	}

	plan, err := Plan(&DecoderConfig{Result: target}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	inner := findStep(t, plan, "Inner")
	if inner.Action != ActionSet || !strings.Contains(inner.Note, "allocated") {
		t.Errorf("expected allocation note, got %+v", inner)
	}
	port := findStep(t, plan, "Inner.Port")
	if port.Action != ActionSet {
		t.Errorf("unexpected Inner.Port step: %+v", port)
	}

	// Planning must not allocate the pointer on the target.
	if target.Inner != nil {
		t.Errorf("target pointer allocated during planning: %+v", target.Inner)
	}

	if err := plan.Apply(target, nil); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}
	if target.Inner == nil || target.Inner.Port != 80 {
		t.Errorf("unexpected result: %+v", target.Inner)
	}
}

func TestPlanOpaqueDecodeHook(t *testing.T) {
	type Config struct {
		Name string
	}

	hook := DecodeHookFuncType(func(from, to reflect.Type, data any) (any, error) {
		if s, ok := data.(string); ok {
			return strings.TrimSpace(s), nil
		}
		return data, nil
	})

	target := &Config{}
	input := map[string]any{"name": "  web  "}
	plan, err := Plan(&DecoderConfig{Result: target, DecodeHook: hook}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	if !plan.Opaque() {
		t.Fatal("plan with a decode hook must be marked opaque")
	}
	step := findStep(t, plan, "Name")
	if !step.Opaque || !strings.Contains(step.Reason, "hook") {
		t.Errorf("expected opaque hook step, got %+v", step)
	}

	// The caller can reject execution of opaque plans.
	if err := plan.Apply(target, nil); !errors.Is(err, ErrOpaquePlan) {
		t.Errorf("expected ErrOpaquePlan, got %v", err)
	}
	if target.Name != "" {
		t.Errorf("target modified despite rejection: %q", target.Name)
	}

	// Explicit approval runs the hook.
	if err := plan.Apply(target, &ApplyOptions{AllowOpaque: true}); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}
	if target.Name != "web" {
		t.Errorf("expected hook to trim the value, got %q", target.Name)
	}
}

type planUnmarshalerString string

func (s *planUnmarshalerString) UnmarshalMapstructure(input any) error {
	str, ok := input.(string)
	if !ok {
		return errors.New("expected string")
	}
	*s = planUnmarshalerString("custom:" + str)
	return nil
}

func TestPlanOpaqueUnmarshaler(t *testing.T) {
	type Config struct {
		Value planUnmarshalerString
	}

	target := &Config{}
	input := map[string]any{"value": "x"}
	plan, err := Plan(&DecoderConfig{Result: target}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	step := findStep(t, plan, "Value")
	if !step.Opaque || !strings.Contains(step.Reason, "Unmarshaler") {
		t.Errorf("expected opaque Unmarshaler step, got %+v", step)
	}

	if err := plan.Apply(target, nil); !errors.Is(err, ErrOpaquePlan) {
		t.Errorf("expected ErrOpaquePlan, got %v", err)
	}

	if err := plan.Apply(target, &ApplyOptions{AllowOpaque: true}); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}
	if target.Value != "custom:x" {
		t.Errorf("unexpected value: %q", target.Value)
	}
}

func TestPlanStaleInput(t *testing.T) {
	type Config struct {
		Name string
	}

	input := map[string]any{"name": "web"}
	plan, err := Plan(&DecoderConfig{Result: &Config{}}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	// Mutating the input after planning invalidates the plan.
	input["name"] = "changed"
	if err := plan.Apply(&Config{}, nil); !errors.Is(err, ErrStalePlan) {
		t.Errorf("expected ErrStalePlan, got %v", err)
	}
}

func TestPlanStaleNestedInput(t *testing.T) {
	type Config struct {
		Labels map[string]string
	}

	labels := map[string]any{"env": "prod"}
	input := map[string]any{"labels": labels}
	plan, err := Plan(&DecoderConfig{Result: &Config{}}, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	// Mutating a nested map after planning invalidates the plan.
	labels["env"] = "dev"
	if err := plan.Apply(&Config{}, nil); !errors.Is(err, ErrStalePlan) {
		t.Errorf("expected ErrStalePlan, got %v", err)
	}
}

func TestPlanStaleConfig(t *testing.T) {
	type Config struct {
		Labels map[string]string
	}

	config := &DecoderConfig{Result: &Config{}}
	input := map[string]any{"labels": map[string]any{"env": "prod"}}
	plan, err := Plan(config, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	// Changing the configuration after planning invalidates the plan.
	config.ZeroFields = true
	if err := plan.Apply(&Config{}, nil); !errors.Is(err, ErrStalePlan) {
		t.Errorf("expected ErrStalePlan, got %v", err)
	}
}

func TestPlanApplyMatchesDecode(t *testing.T) {
	type Inner struct {
		Port int
	}
	type Config struct {
		Name   string
		Inner  *Inner
		Labels map[string]string
	}

	input := map[string]any{
		"name":   "web",
		"inner":  map[string]any{"port": "8080"},
		"labels": map[string]any{"env": "prod"},
	}
	config := &DecoderConfig{Result: &Config{}, WeaklyTypedInput: true}

	plan, err := Plan(config, input)
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	planned := &Config{}
	if err := plan.Apply(planned, nil); err != nil {
		t.Fatalf("Apply failed: %s", err)
	}

	direct := &Config{}
	decoder, err := NewDecoder(&DecoderConfig{Result: direct, WeaklyTypedInput: true})
	if err != nil {
		t.Fatalf("NewDecoder failed: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("Decode failed: %s", err)
	}

	if !reflect.DeepEqual(planned, direct) {
		t.Errorf("planned apply differs from direct decode: got %+v, want %+v", planned, direct)
	}
}

func TestPlanTargetTypeMismatch(t *testing.T) {
	type Config struct {
		Name string
	}
	type Other struct {
		Name string
	}

	plan, err := Plan(&DecoderConfig{Result: &Config{}}, map[string]any{"name": "web"})
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	if err := plan.Apply(&Other{}, nil); err == nil {
		t.Error("expected error for mismatched target type")
	}
	if err := plan.Apply(nil, nil); err == nil {
		t.Error("expected error for nil target")
	}
}

func TestPlanIncompatibleConversion(t *testing.T) {
	type Config struct {
		Port int
	}

	plan, err := Plan(&DecoderConfig{Result: &Config{}}, map[string]any{"port": "not-a-number"})
	if err != nil {
		t.Fatalf("Plan failed: %s", err)
	}

	step := findStep(t, plan, "Port")
	if step.Issue == "" {
		t.Errorf("expected an issue for string -> int without WeaklyTypedInput, got %+v", step)
	}
}

func TestPlanRequiresResult(t *testing.T) {
	if _, err := Plan(&DecoderConfig{}, map[string]any{}); err == nil {
		t.Error("expected error when config.Result is nil")
	}
	if _, err := Plan(nil, map[string]any{}); err == nil {
		t.Error("expected error when config is nil")
	}
}
