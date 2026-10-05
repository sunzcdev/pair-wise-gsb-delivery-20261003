package mapstructure

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func assertParity(t *testing.T, cfg DecoderConfig, input any) {
	t.Helper()

	direct := reflect.New(reflect.TypeOf(cfg.Result).Elem()).Interface()
	c1 := cfg
	c1.Result = direct
	dec1, err := NewDecoder(&c1)
	if err != nil {
		t.Fatalf("direct decoder: %v", err)
	}
	directErr := dec1.Decode(input)

	planned := reflect.New(reflect.TypeOf(cfg.Result).Elem()).Interface()
	c2 := cfg
	c2.Result = planned
	dec2, err := NewDecoder(&c2)
	if err != nil {
		t.Fatalf("planned decoder: %v", err)
	}
	plan, planErr := dec2.Plan(input)

	if (directErr == nil) != (planErr == nil) {
		t.Fatalf("error parity mismatch: direct=%v plan=%v", directErr, planErr)
	}
	if directErr != nil {
		return
	}
	if plan == nil {
		t.Fatal("nil plan")
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !reflect.DeepEqual(direct, planned) {
		t.Fatalf("parity mismatch:\ndirect=%#v\nplanned=%#v", direct, planned)
	}
}

func TestPlanDecodeParity(t *testing.T) {
	type leaf struct {
		S  string
		I  int
		B  bool
		U  uint
		F  float64
		P  *int
		PP *leaf
	}
	type root struct {
		A      leaf
		As     []leaf
		Iface  any
		Slice  []int
		Array  [3]int
		StrMap map[string]leaf
		AnyMap map[string]any
		Nested map[string]map[string]int
	}

	i := 5
	cases := []struct {
		name string
		cfg  DecoderConfig
		in   any
	}{
		{
			name: "basic",
			cfg:  DecoderConfig{},
			in: map[string]any{
				"A":     map[string]any{"S": "s", "I": 1, "B": true, "U": 2, "F": 1.5, "P": &i, "PP": map[string]any{"S": "inner"}},
				"As":    []map[string]any{{"S": "x"}, {"S": "y"}},
				"Iface": "hello",
				"Slice": []int{1, 2, 3},
				"Array": []int{4, 5, 6},
				"StrMap": map[string]map[string]any{
					"k": {"S": "v", "I": 9},
				},
				"AnyMap": map[string]any{"n": 3, "s": "str", "nested": map[string]any{"q": 1}},
				"Nested": map[string]map[string]int{"a": {"b": 1}},
			},
		},
		{
			name: "weak",
			cfg:  DecoderConfig{WeaklyTypedInput: true},
			in: map[string]any{
				"A":     map[string]any{"S": 7, "I": "11", "B": "1"},
				"Slice": []string{"1", "2"},
				"Array": []string{"7", "8", "9"},
			},
		},
		{
			name: "nils",
			cfg:  DecoderConfig{},
			in: map[string]any{
				"A":      map[string]any{"P": nil, "PP": nil},
				"Slice":  nil,
				"StrMap": nil,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Result = &root{}
			assertParity(t, cfg, tc.in)
		})
		t.Run(tc.name+"/zero", func(t *testing.T) {
			cfg := tc.cfg
			cfg.Result = &root{}
			cfg.ZeroFields = true
			assertParity(t, cfg, tc.in)
		})
	}
}

func TestPlanMergeParityPresetData(t *testing.T) {
	type root struct {
		M map[string]int
		S []int
		P *int
	}

	// Merge into pre-populated maps without ZeroFields.
	preset := func() *root {
		return &root{
			M: map[string]int{"a": 1, "b": 2, "c": 3},
			S: []int{9, 8, 7, 6},
		}
	}

	in := map[string]any{
		"M": map[string]int{"b": 20, "d": 40},
		"S": []int{1, 2},
		"P": 123,
	}

	direct := preset()
	d1, _ := NewDecoder(&DecoderConfig{Result: direct})
	if err := d1.Decode(in); err != nil {
		t.Fatal(err)
	}

	planned := preset()
	d2, _ := NewDecoder(&DecoderConfig{Result: planned})
	plan, err := d2.Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct, planned) {
		t.Fatalf("merge parity mismatch:\ndirect=%#v\nplanned=%#v", direct, planned)
	}
}

func TestPlanSquashEmbedParity(t *testing.T) {
	type Embedded struct {
		X int
		Y int
	}
	type withEmbed struct {
		Embedded `mapstructure:",squash"`
		Name     string
	}
	type ptrEmbed struct {
		*Embedded `mapstructure:",squash"`
		Z         int
	}

	run := func(t *testing.T, result any, in any) {
		c1 := DecoderConfig{Result: reflect.New(reflect.ValueOf(result).Elem().Type()).Interface()}
		c2 := DecoderConfig{Result: reflect.New(reflect.ValueOf(result).Elem().Type()).Interface()}
		d1, _ := NewDecoder(&c1)
		if err := d1.Decode(in); err != nil {
			t.Fatalf("decode: %v", err)
		}
		d2, _ := NewDecoder(&c2)
		plan, err := d2.Plan(in)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		if err := plan.Apply(nil); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if !reflect.DeepEqual(c1.Result, c2.Result) {
			t.Fatalf("squash mismatch:\n%#v\n%#v", c1.Result, c2.Result)
		}
	}

	t.Run("value-embed", func(t *testing.T) {
		run(t, &withEmbed{}, map[string]any{"X": 1, "Y": 2, "Name": "n"})
	})
	t.Run("pointer-embed", func(t *testing.T) {
		run(t, &ptrEmbed{}, map[string]any{"X": 3, "Y": 4, "Z": 5})
	})
}

func TestPlanTopLevelMapParity(t *testing.T) {
	in := map[string]any{
		"a": 1,
		"b": map[string]any{"nested": 2},
		"c": "three",
	}
	direct := map[string]any{}
	planned := map[string]any{}

	d1, _ := NewDecoder(&DecoderConfig{Result: &direct})
	if err := d1.Decode(in); err != nil {
		t.Fatal(err)
	}
	d2, _ := NewDecoder(&DecoderConfig{Result: &planned})
	plan, err := d2.Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct, planned) {
		t.Fatalf("top-level map mismatch:\n%#v\n%#v", direct, planned)
	}
}

func TestPlanSliceOfStructsParity(t *testing.T) {
	type item struct {
		N string
		V int
	}
	type root struct {
		Items []item
	}
	in := map[string]any{
		"Items": []map[string]any{{"N": "a", "V": 1}, {"N": "b", "V": 2}},
	}
	r1 := &root{}
	r2 := &root{}
	d1, _ := NewDecoder(&DecoderConfig{Result: r1})
	if err := d1.Decode(in); err != nil {
		t.Fatal(err)
	}
	d2, _ := NewDecoder(&DecoderConfig{Result: r2})
	plan, err := d2.Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("slice structs mismatch:\n%#v\n%#v", r1, r2)
	}
}

func TestPlanErrorParity(t *testing.T) {
	type tgt struct {
		N int
	}
	d1, _ := NewDecoder(&DecoderConfig{Result: &tgt{}})
	directErr := d1.Decode(map[string]any{"N": "not-a-number"})

	d2, _ := NewDecoder(&DecoderConfig{Result: &tgt{}})
	_, planErr := d2.Plan(map[string]any{"N": "not-a-number"})

	if (directErr == nil) != (planErr == nil) {
		t.Fatalf("error parity: direct=%v plan=%v", directErr, planErr)
	}
	if planErr == nil {
		t.Fatal("expected planning error")
	}
	if !strings.Contains(planErr.Error(), "N") {
		t.Fatalf("error should name field N: %v", planErr)
	}
}

func TestPlanInputNotMutated(t *testing.T) {
	type tgt struct {
		M map[string]int
		S []int
	}
	input := map[string]any{
		"M": map[string]int{"a": 1},
		"S": []int{1, 2},
	}
	dec, _ := NewDecoder(&DecoderConfig{Result: &tgt{}})
	if _, err := dec.Plan(input); err != nil {
		t.Fatal(err)
	}
	if input["M"] == nil || input["S"] == nil {
		t.Fatal("input map structure changed")
	}
	// The input map itself must remain the same identity/content.
	if len(input["M"].(map[string]int)) != 1 || len(input["S"].([]int)) != 2 {
		t.Fatalf("input content changed: %+v", input)
	}
}

func TestPlanUnexportedFieldsSkipped(t *testing.T) {
	type tgt struct {
		Public string
		priv   string
	}
	target := &tgt{Public: "p", priv: "secret"}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	plan, err := dec.Plan(map[string]any{"Public": "new", "priv": "leak"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if target.Public != "new" || target.priv != "secret" {
		t.Fatalf("unexported handling wrong: %+v", target)
	}
}

func TestPlanNilInputLeavesTarget(t *testing.T) {
	type tgt struct {
		A string
		P *int
		M map[string]int
	}
	presetV := 3
	target := &tgt{A: "keep", P: &presetV, M: map[string]int{"k": 1}}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	plan, err := dec.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range plan.Ops() {
		if op.Kind != OpRetain {
			t.Fatalf("nil input without ZeroFields must only retain, got %+v", op)
		}
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if target.A != "keep" || target.P == nil || *target.P != 3 || target.M["k"] != 1 {
		t.Fatalf("nil input mutated target: %+v", target)
	}
}

func TestPlanNilInputZeroFields(t *testing.T) {
	type tgt struct {
		A string
		P *int
	}
	v := 3
	target := &tgt{A: "x", P: &v}
	dec, _ := NewDecoder(&DecoderConfig{Result: target, ZeroFields: true})
	plan, err := dec.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	anyWrite := false
	for _, op := range plan.Ops() {
		if op.Kind != OpRetain {
			anyWrite = true
		}
	}
	if !anyWrite {
		t.Fatalf("expected zeroing writes, got %+v", plan.Ops())
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if target.A != "" || target.P != nil {
		t.Fatalf("target not zeroed: %+v", target)
	}
}

type planPerson struct {
	Name string
	Age  int
}

type planAddress struct {
	City string
	Zip  int
}

type planNested struct {
	Person  planPerson
	Address *planAddress
	Tags    map[string]string
	Nested  map[string]map[string]string
}

func opMap(ops []Op) map[string]Op {
	m := make(map[string]Op, len(ops))
	for _, op := range ops {
		m[op.Name] = op
	}
	return m
}

func TestPlanNestedMapMerge(t *testing.T) {
	target := &planNested{
		Tags: map[string]string{"keep": "old", "gone": "stay"},
		Nested: map[string]map[string]string{
			"a": {"x": "1", "y": "2"},
		},
	}
	input := map[string]any{
		"Tags": map[string]string{"keep": "new", "added": "3"},
		"Nested": map[string]map[string]string{
			"a": {"x": "9", "z": "3"},
		},
	}

	dec, err := NewDecoder(&DecoderConfig{Result: target})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := dec.Plan(input)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	ops := opMap(plan.Ops())

	if op := ops["Tags[keep]"]; op.Kind != OpSet || op.Value != "new" {
		t.Fatalf("Tags[keep] = %+v", op)
	}
	if op := ops["Tags[added]"]; op.Kind != OpSet {
		t.Fatalf("Tags[added] = %+v", op)
	}
	if op := ops["Tags[gone]"]; op.Kind != OpRetain {
		t.Fatalf("Tags[gone] should be retained, got %+v", op)
	}
	if op := ops["Nested[a][x]"]; op.Kind != OpSet || op.Value != "9" {
		t.Fatalf("Nested[a][x] = %+v", op)
	}
	if op := ops["Nested[a][z]"]; op.Kind != OpSet {
		t.Fatalf("Nested[a][z] = %+v", op)
	}
	if op := ops["Nested[a][y]"]; op.Kind != OpRetain {
		t.Fatalf("Nested[a][y] should be retained, got %+v", op)
	}

	if err := plan.Apply(nil); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if target.Tags["keep"] != "new" || target.Tags["added"] != "3" || target.Tags["gone"] != "stay" {
		t.Fatalf("tags merge wrong: %+v", target.Tags)
	}
	if target.Nested["a"]["x"] != "9" || target.Nested["a"]["z"] != "3" || target.Nested["a"]["y"] != "2" {
		t.Fatalf("nested merge wrong: %+v", target.Nested)
	}
}

func TestPlanZeroFields(t *testing.T) {
	type zeroTarget struct {
		Person planPerson
		Tags   map[string]string
	}
	target := &zeroTarget{
		Person: planPerson{Name: "old", Age: 99},
		Tags:   map[string]string{"gone": "x", "keep": "0"},
	}
	input := map[string]any{
		"Person": map[string]any{"Name": "new"},
		"Tags":   map[string]string{"keep": "1", "added": "2"},
	}

	dec, _ := NewDecoder(&DecoderConfig{Result: target, ZeroFields: true})
	plan, err := dec.Plan(input)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	ops := opMap(plan.Ops())
	if op := ops["Person.Age"]; op.Kind != OpZero {
		t.Fatalf("Person.Age should be zeroed, got %+v", op)
	}
	if op := ops["Tags[gone]"]; op.Kind != OpZero {
		t.Fatalf("Tags[gone] should be zeroed (deleted), got %+v", op)
	}
	if op := ops["Tags[keep]"]; op.Kind != OpSet {
		t.Fatalf("Tags[keep] = %+v", op)
	}

	if err := plan.Apply(nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if target.Person.Age != 0 || target.Person.Name != "new" {
		t.Fatalf("zero fields wrong: %+v", target.Person)
	}
	if _, ok := target.Tags["gone"]; ok {
		t.Fatalf("gone should be deleted: %+v", target.Tags)
	}
	if target.Tags["keep"] != "1" || target.Tags["added"] != "2" {
		t.Fatalf("tags wrong: %+v", target.Tags)
	}
}

func TestPlanNilPointerAllocation(t *testing.T) {
	target := &planNested{}
	input := map[string]any{
		"Address": map[string]any{"City": "Berlin", "Zip": 10115},
	}

	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	plan, err := dec.Plan(input)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	ops := opMap(plan.Ops())
	if op := ops["Address"]; op.Kind != OpSet || op.TargetType.String() != "*mapstructure.planAddress" {
		t.Fatalf("Address allocation = %+v", op)
	}
	if op := ops["Address.City"]; op.Kind != OpSet || op.Value != "Berlin" {
		t.Fatalf("Address.City = %+v", op)
	}

	if target.Address != nil {
		t.Fatalf("target mutated during planning")
	}

	if err := plan.Apply(nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if target.Address == nil || target.Address.City != "Berlin" || target.Address.Zip != 10115 {
		t.Fatalf("address wrong: %+v", target.Address)
	}
}

func TestPlanDoesNotMutateTarget(t *testing.T) {
	target := &planNested{
		Person:  planPerson{Name: "old", Age: 1},
		Address: &planAddress{City: "old"},
		Tags:    map[string]string{"k": "v"},
	}
	input := map[string]any{
		"Person":  map[string]any{"Name": "new", "Age": 2},
		"Address": map[string]any{"City": "new"},
		"Tags":    map[string]string{"k": "new"},
	}

	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	if _, err := dec.Plan(input); err != nil {
		t.Fatalf("plan: %v", err)
	}

	if target.Person.Name != "old" || target.Person.Age != 1 ||
		target.Address.City != "old" || target.Tags["k"] != "v" {
		t.Fatalf("target changed during planning: %+v %+v %+v", target.Person, target.Address, target.Tags)
	}
}

func TestPlanConversions(t *testing.T) {
	type convTarget struct {
		Age  int
		Flag bool
	}
	target := &convTarget{}
	input := map[string]any{"Age": "42", "Flag": "true"}

	dec, _ := NewDecoder(&DecoderConfig{Result: target, WeaklyTypedInput: true})
	plan, err := dec.Plan(input)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	ops := opMap(plan.Ops())
	if c := ops["Age"].Conversion; !strings.Contains(c, "string") || !strings.Contains(c, "int") {
		t.Fatalf("Age conversion = %q", c)
	}
	if c := ops["Flag"].Conversion; !strings.Contains(c, "bool") {
		t.Fatalf("Flag conversion = %q", c)
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if target.Age != 42 || !target.Flag {
		t.Fatalf("conversion result wrong: %+v", target)
	}
}

type planTrim string

func (s *planTrim) UnmarshalMapstructure(input any) error {
	str, ok := input.(string)
	if !ok {
		return errors.New("expected string")
	}
	*s = planTrim("<" + str + ">")
	return nil
}

func TestPlanUnmarshalerMarkedAndGated(t *testing.T) {
	type umTarget struct {
		V planTrim
	}
	target := &umTarget{}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})

	plan, err := dec.Plan(map[string]any{"V": "hi"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.RequiresUserCode() {
		t.Fatal("expected RequiresUserCode")
	}
	uc := plan.UserCode()
	if len(uc) != 1 || uc[0].Callback != UnmarshalerCallback || uc[0].Name != "V" {
		t.Fatalf("user code ops = %+v", uc)
	}
	if string(target.V) != "" {
		t.Fatal("target mutated during planning")
	}

	if err := plan.Apply(nil); !errors.Is(err, ErrUserCodeRequired) {
		t.Fatalf("want ErrUserCodeRequired, got %v", err)
	}
	if err := plan.Apply(&ApplyOptions{AllowUserCode: true}); err != nil {
		t.Fatalf("approved apply: %v", err)
	}
	if string(target.V) != "<hi>" {
		t.Fatalf("unmarshaler result = %q", target.V)
	}
}

func TestPlanHookMarkedAndGated(t *testing.T) {
	type hookTarget struct {
		V int
	}
	target := &hookTarget{}
	called := false
	hook := DecodeHookFuncType(func(_, _ reflect.Type, data any) (any, error) {
		called = true
		if s, ok := data.(string); ok && s == "x" {
			return 7, nil
		}
		return data, nil
	})
	dec, _ := NewDecoder(&DecoderConfig{Result: target, DecodeHook: hook, WeaklyTypedInput: true})

	plan, err := dec.Plan(map[string]any{"V": "x"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if called {
		t.Fatal("hook must not run during planning")
	}
	if !plan.RequiresUserCode() || len(plan.UserCode()) == 0 {
		t.Fatalf("expected hook ops, got %+v", plan.UserCode())
	}
	foundHook := false
	for _, op := range plan.UserCode() {
		if op.Callback == HookCallback {
			foundHook = true
		}
	}
	if !foundHook {
		t.Fatal("no HookCallback op")
	}

	if err := plan.Apply(nil); !errors.Is(err, ErrUserCodeRequired) {
		t.Fatalf("want ErrUserCodeRequired, got %v", err)
	}
	if err := plan.Apply(&ApplyOptions{AllowUserCode: true}); err != nil {
		t.Fatalf("approved apply: %v", err)
	}
	if target.V != 7 {
		t.Fatalf("hook result = %d", target.V)
	}
}

func TestPlanStaleInput(t *testing.T) {
	target := &planPerson{}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	input := map[string]any{"Name": "a"}
	plan, err := dec.Plan(input)
	if err != nil {
		t.Fatal(err)
	}
	input["Name"] = "b"
	if err := plan.Apply(nil); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("want ErrPlanStale, got %v", err)
	}
	if target.Name != "" {
		t.Fatal("stale plan must not mutate target")
	}
}

func TestPlanStaleTarget(t *testing.T) {
	target := &planPerson{}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	plan, err := dec.Plan(map[string]any{"Name": "a"})
	if err != nil {
		t.Fatal(err)
	}
	target.Name = "changed-outside"
	if err := plan.Apply(nil); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("want ErrPlanStale, got %v", err)
	}
}

func TestPlanStaleConfig(t *testing.T) {
	target := &planPerson{}
	cfg := &DecoderConfig{Result: target}
	dec, _ := NewDecoder(cfg)
	plan, err := dec.Plan(map[string]any{"Age": 7})
	if err != nil {
		t.Fatal(err)
	}
	// Turn on weak typing after the plan was made: the recorded judgement was
	// made with strict decoding and must not be silently reused.
	cfg.WeaklyTypedInput = true
	if err := plan.Apply(nil); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("want ErrPlanStale, got %v", err)
	}
}

func TestPlanStrictRejectsWeakInput(t *testing.T) {
	// Without WeaklyTypedInput, "7" cannot be assigned to int; planning must
	// surface this as an error instead of silently coercing.
	target := &planPerson{}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	_, err := dec.Plan(map[string]any{"Age": "7"})
	if err == nil {
		t.Fatal("expected planning error for string -> int")
	}
	if target.Age != 0 {
		t.Fatal("target must stay unchanged on planning error")
	}
}

func TestPlanIntKeyedMap(t *testing.T) {
	target := &struct {
		M map[int]string
	}{M: map[int]string{1: "keep", 2: "gone"}}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	plan, err := dec.Plan(map[string]any{
		"M": map[int]string{1: "new", 3: "added"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := opMap(plan.Ops())
	if ops["M[1]"].Kind != OpSet || ops["M[3]"].Kind != OpSet || ops["M[2]"].Kind != OpRetain {
		t.Fatalf("int key ops wrong: %+v", plan.Ops())
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if target.M[1] != "new" || target.M[3] != "added" || target.M[2] != "gone" {
		t.Fatalf("int keyed map wrong: %+v", target.M)
	}
}

func TestPlanExistingPointerReused(t *testing.T) {
	type tgt struct {
		P *planPerson
	}
	preset := &planPerson{Name: "old", Age: 1}
	target := &tgt{P: preset}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	plan, err := dec.Plan(map[string]any{
		"P": map[string]any{"Name": "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// P itself should be retained (same pointer), pointee Name set.
	for _, op := range plan.Set() {
		if op.Name == "P" {
			t.Fatalf("existing non-nil pointer must not be replaced: %+v", op)
		}
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if target.P != preset {
		t.Fatal("pointer identity must be preserved when reused")
	}
	if target.P.Name != "new" || target.P.Age != 1 {
		t.Fatalf("pointee not updated correctly: %+v", target.P)
	}
}

func TestPlanZeroFieldsNestedMapDelete(t *testing.T) {
	type tgt struct {
		Nested map[string]map[string]int
	}
	target := &tgt{
		Nested: map[string]map[string]int{
			"a": {"x": 1, "gone": 2},
		},
	}
	dec, _ := NewDecoder(&DecoderConfig{Result: target, ZeroFields: true})
	plan, err := dec.Plan(map[string]any{
		"Nested": map[string]map[string]int{"a": {"x": 9, "new": 3}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Direct decode reference.
	ref := &tgt{Nested: map[string]map[string]int{"a": {"x": 1, "gone": 2}}}
	dref, _ := NewDecoder(&DecoderConfig{Result: ref, ZeroFields: true})
	if err := dref.Decode(map[string]any{
		"Nested": map[string]map[string]int{"a": {"x": 9, "new": 3}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ref, target) {
		t.Fatalf("nested map delete mismatch:\nref=%#v\ngot=%#v", ref, target)
	}
}

func TestPlanArrayAndSliceParity(t *testing.T) {
	type tgt struct {
		Arr [3]int
		S   []int
	}
	preset := func() *tgt {
		return &tgt{Arr: [3]int{1, 2, 3}, S: []int{10, 20, 30, 40}}
	}
	in := map[string]any{
		"Arr": []int{7, 8},
		"S":   []int{11, 22},
	}

	direct := preset()
	d1, _ := NewDecoder(&DecoderConfig{Result: direct})
	if err := d1.Decode(in); err != nil {
		t.Fatal(err)
	}
	planned := preset()
	d2, _ := NewDecoder(&DecoderConfig{Result: planned})
	plan, err := d2.Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct, planned) {
		t.Fatalf("array/slice parity:\ndirect=%#v\nplanned=%#v", direct, planned)
	}
}

func TestPlanSquashedNilPtrEmbed(t *testing.T) {
	type Inner struct {
		X int
	}
	type tgt struct {
		*Inner `mapstructure:",squash"`
		Name   string
	}
	target := &tgt{}
	dec, _ := NewDecoder(&DecoderConfig{Result: target})
	plan, err := dec.Plan(map[string]any{"X": 42, "Name": "n"})
	if err != nil {
		t.Fatal(err)
	}
	if target.Inner != nil {
		t.Fatal("target mutated during planning")
	}
	if err := plan.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if target.Inner == nil || target.X != 42 || target.Name != "n" {
		t.Fatalf("squashed ptr embed wrong: %+v", target)
	}
}
