package mapstructure

import (
	"reflect"
	"strings"
	"testing"
)

type maskAddress struct {
	Street string
	City   string
}

type maskPerson struct {
	Name    string
	Age     int
	Address maskAddress
	Labels  map[string]string
	Tags    []string
}

type MaskSquashInner struct {
	Name string
	Age  int
}

type MaskSquashOuter struct {
	MaskSquashInner `mapstructure:",squash"`
	Active          bool
}

type MaskSquashPtrOuter struct {
	*MaskSquashInner `mapstructure:",squash"`
	Active           bool
}

func maskDecoderConfig(result any, mask ...string) *DecoderConfig {
	return &DecoderConfig{
		Result:    result,
		FieldMask: mask,
	}
}

func maskDecode(t *testing.T, config *DecoderConfig, input any) {
	t.Helper()

	decoder, err := NewDecoder(config)
	if err != nil {
		t.Fatalf("NewDecoder returned error: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("Decode returned error: %s", err)
	}
}

func TestFieldMask_LeafSelection(t *testing.T) {
	result := maskPerson{
		Name: "old-name",
		Age:  42,
	}

	input := map[string]any{
		"name": "new-name",
		"age":  7,
	}

	maskDecode(t, maskDecoderConfig(&result, "name"), input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
	if result.Age != 42 {
		t.Errorf("Age: got %d, want old value 42", result.Age)
	}
}

func TestFieldMask_ParentSelectsSubtree(t *testing.T) {
	result := maskPerson{
		Name: "old-name",
		Address: maskAddress{
			Street: "old-street",
			City:   "old-city",
		},
	}

	input := map[string]any{
		"name": "new-name",
		"address": map[string]any{
			"street": "new-street",
			"city":   "new-city",
		},
	}

	maskDecode(t, maskDecoderConfig(&result, "address"), input)

	if result.Address != (maskAddress{Street: "new-street", City: "new-city"}) {
		t.Errorf("Address: got %+v, want fully decoded subtree", result.Address)
	}
	if result.Name != "old-name" {
		t.Errorf("Name: got %q, want old value %q", result.Name, "old-name")
	}
}

func TestFieldMask_NestedLeafKeepsSiblings(t *testing.T) {
	result := maskPerson{
		Name: "old-name",
		Address: maskAddress{
			Street: "old-street",
			City:   "old-city",
		},
	}

	input := map[string]any{
		"name": "new-name",
		"address": map[string]any{
			"street": "new-street",
			"city":   "new-city",
		},
	}

	maskDecode(t, maskDecoderConfig(&result, "address.city"), input)

	want := maskPerson{
		Name: "old-name",
		Address: maskAddress{
			Street: "old-street",
			City:   "new-city",
		},
	}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("got %+v, want %+v", result, want)
	}
}

func TestFieldMask_InvalidPaths(t *testing.T) {
	cases := map[string][]string{
		"unknown field":          {"nope"},
		"unknown nested field":   {"address.nope"},
		"empty path":             {""},
		"empty segment":          {"address..city"},
		"trailing segment":       {"address."},
		"into scalar":            {"name.first"},
		"into map":               {"labels.foo"},
		"into slice":             {"tags.foo"},
		"slice index as segment": {"tags[0]"},
	}

	for name, mask := range cases {
		t.Run(name, func(t *testing.T) {
			var result maskPerson
			_, err := NewDecoder(maskDecoderConfig(&result, mask...))
			if err == nil {
				t.Fatalf("NewDecoder(%v) expected error, got nil", mask)
			}
			if !strings.Contains(err.Error(), "invalid field mask path") {
				t.Errorf("unexpected error: %s", err)
			}
		})
	}
}

func TestFieldMask_InvalidPathsReportedBeforeInput(t *testing.T) {
	var result maskPerson

	decoder, err := NewDecoder(maskDecoderConfig(&result, "nope", "address"))
	if decoder != nil {
		t.Fatalf("expected nil decoder, got %#v", decoder)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("error should mention the invalid path: %s", err)
	}
}

func TestFieldMask_NonStructResult(t *testing.T) {
	var result map[string]any

	_, err := NewDecoder(maskDecoderConfig(&result, "name"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "non-struct type") {
		t.Errorf("unexpected error: %s", err)
	}
}

func TestFieldMask_SquashPromotedField(t *testing.T) {
	result := MaskSquashOuter{
		MaskSquashInner: MaskSquashInner{Name: "old-name", Age: 42},
		Active:          true,
	}

	input := map[string]any{
		"name":   "new-name",
		"age":    7,
		"active": false,
	}

	maskDecode(t, maskDecoderConfig(&result, "name"), input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
	if result.Age != 42 {
		t.Errorf("Age: got %d, want old value 42", result.Age)
	}
	if !result.Active {
		t.Error("Active: got false, want old value true")
	}
}

func TestFieldMask_SquashFieldByOwnName(t *testing.T) {
	result := MaskSquashOuter{
		MaskSquashInner: MaskSquashInner{Name: "old-name", Age: 42},
		Active:          true,
	}

	input := map[string]any{
		"name":   "new-name",
		"age":    7,
		"active": false,
	}

	maskDecode(t, maskDecoderConfig(&result, "MaskSquashInner"), input)

	if result.Name != "new-name" || result.Age != 7 {
		t.Errorf("inner: got %+v, want fully selected subtree", result.MaskSquashInner)
	}
	if !result.Active {
		t.Error("Active: got false, want old value true")
	}
}

func TestFieldMask_SquashNilPointerNotAllocated(t *testing.T) {
	result := MaskSquashPtrOuter{Active: false}

	input := map[string]any{
		"name":   "new-name",
		"active": true,
	}

	maskDecode(t, maskDecoderConfig(&result, "active"), input)

	if !result.Active {
		t.Error("Active: got false, want true")
	}
	if result.MaskSquashInner != nil {
		t.Errorf("masked-out embedded pointer was modified: %+v", result.MaskSquashInner)
	}
}

func TestFieldMask_SquashPointerSelected(t *testing.T) {
	result := MaskSquashPtrOuter{}

	input := map[string]any{
		"name":   "new-name",
		"age":    7,
		"active": true,
	}

	maskDecode(t, maskDecoderConfig(&result, "name"), input)

	if result.MaskSquashInner == nil {
		t.Fatal("embedded pointer was not allocated for selected field")
	}
	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
	if result.Age != 0 {
		t.Errorf("Age: got %d, want zero value", result.Age)
	}
	if result.Active {
		t.Error("Active: got true, want zero value")
	}
}

type maskPtrTarget struct {
	Inner *maskAddress
	Name  string
}

func TestFieldMask_PointerTraversal(t *testing.T) {
	result := maskPtrTarget{
		Inner: &maskAddress{Street: "old-street", City: "old-city"},
		Name:  "old-name",
	}

	input := map[string]any{
		"inner": map[string]any{
			"street": "new-street",
			"city":   "new-city",
		},
		"name": "new-name",
	}

	maskDecode(t, maskDecoderConfig(&result, "inner.city"), input)

	if result.Inner == nil {
		t.Fatal("Inner: got nil, want preserved pointer")
	}
	if result.Inner.City != "new-city" {
		t.Errorf("Inner.City: got %q, want %q", result.Inner.City, "new-city")
	}
	if result.Inner.Street != "old-street" {
		t.Errorf("Inner.Street: got %q, want old value %q", result.Inner.Street, "old-street")
	}
	if result.Name != "old-name" {
		t.Errorf("Name: got %q, want old value %q", result.Name, "old-name")
	}
}

func TestFieldMask_MapIsLeaf(t *testing.T) {
	result := maskPerson{
		Name:   "old-name",
		Labels: map[string]string{"keep": "me"},
	}

	input := map[string]any{
		"name":   "new-name",
		"labels": map[string]any{"env": "prod"},
	}

	maskDecode(t, maskDecoderConfig(&result, "labels"), input)

	if !reflect.DeepEqual(result.Labels, map[string]string{"keep": "me", "env": "prod"}) {
		t.Errorf("Labels: got %v, want merged map", result.Labels)
	}
	if result.Name != "old-name" {
		t.Errorf("Name: got %q, want old value %q", result.Name, "old-name")
	}
}

func TestFieldMask_SliceIsLeaf(t *testing.T) {
	result := maskPerson{
		Name: "old-name",
		Tags: []string{"old"},
	}

	input := map[string]any{
		"name": "new-name",
		"tags": []any{"a", "b"},
	}

	maskDecode(t, maskDecoderConfig(&result, "tags"), input)

	if !reflect.DeepEqual(result.Tags, []string{"a", "b"}) {
		t.Errorf("Tags: got %v, want [a b]", result.Tags)
	}
	if result.Name != "old-name" {
		t.Errorf("Name: got %q, want old value %q", result.Name, "old-name")
	}
}

func TestFieldMask_HookNotCalledForMaskedFields(t *testing.T) {
	calls := 0
	hook := DecodeHookFuncValue(func(from, to reflect.Value) (any, error) {
		calls++
		return from.Interface(), nil
	})

	input := map[string]any{
		"name": "new-name",
		"age":  7,
		"address": map[string]any{
			"street": "new-street",
			"city":   "new-city",
		},
	}

	var result maskPerson
	config := maskDecoderConfig(&result, "address.city")
	config.DecodeHook = hook
	maskDecode(t, config, input)

	// The hook runs for the root struct, the address struct and the city
	// field only. Masked-out fields (name, age, address.street) must not
	// trigger it.
	if calls != 3 {
		t.Errorf("hook calls: got %d, want 3", calls)
	}

	calls = 0
	var unmasked maskPerson
	config = &DecoderConfig{Result: &unmasked, DecodeHook: hook}
	maskDecode(t, config, input)

	if calls != 6 {
		t.Errorf("unmasked hook calls: got %d, want 6", calls)
	}
}

func TestFieldMask_ErrorUnusedIgnoresMaskedByDefault(t *testing.T) {
	var result maskPerson
	config := maskDecoderConfig(&result, "name")
	config.ErrorUnused = true

	input := map[string]any{
		"name": "new-name",
		"age":  7,
	}

	maskDecode(t, config, input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
}

func TestFieldMask_ErrorUnusedMasked(t *testing.T) {
	var result maskPerson
	config := maskDecoderConfig(&result, "name")
	config.ErrorUnused = true
	config.ErrorUnusedMasked = true

	input := map[string]any{
		"name": "new-name",
		"age":  7,
	}

	decoder, err := NewDecoder(config)
	if err != nil {
		t.Fatalf("NewDecoder returned error: %s", err)
	}
	err = decoder.Decode(input)
	if err == nil {
		t.Fatal("Decode expected unused key error, got nil")
	}
	if !strings.Contains(err.Error(), "age") {
		t.Errorf("error should mention the masked key: %s", err)
	}
}

func TestFieldMask_MetadataUnused(t *testing.T) {
	input := map[string]any{
		"name": "new-name",
		"age":  7,
	}

	var result maskPerson
	metadata := &Metadata{}
	config := maskDecoderConfig(&result, "name")
	config.Metadata = metadata
	maskDecode(t, config, input)

	if len(metadata.Unused) != 0 {
		t.Errorf("Unused: got %v, want none", metadata.Unused)
	}

	result = maskPerson{}
	metadata = &Metadata{}
	config = maskDecoderConfig(&result, "name")
	config.Metadata = metadata
	config.ErrorUnusedMasked = true
	maskDecode(t, config, input)

	if !reflect.DeepEqual(metadata.Unused, []string{"age"}) {
		t.Errorf("Unused: got %v, want [age]", metadata.Unused)
	}
}

func TestFieldMask_ErrorUnsetIgnoresMaskedFields(t *testing.T) {
	var result maskPerson
	config := maskDecoderConfig(&result, "name")
	config.ErrorUnset = true

	input := map[string]any{
		"name": "new-name",
	}

	maskDecode(t, config, input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
}

func TestFieldMask_TagNames(t *testing.T) {
	type tagged struct {
		UserName string `mapstructure:"user_name"`
		Other    string `mapstructure:"other"`
	}

	result := tagged{Other: "old-other"}

	input := map[string]any{
		"user_name": "new-name",
		"other":     "new-other",
	}

	maskDecode(t, maskDecoderConfig(&result, "user_name"), input)

	if result.UserName != "new-name" {
		t.Errorf("UserName: got %q, want %q", result.UserName, "new-name")
	}
	if result.Other != "old-other" {
		t.Errorf("Other: got %q, want old value %q", result.Other, "old-other")
	}
}

func TestFieldMask_CaseInsensitive(t *testing.T) {
	var result maskPerson

	input := map[string]any{
		"name": "new-name",
	}

	maskDecode(t, maskDecoderConfig(&result, "NAME"), input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
}

func TestFieldMask_RemainMaskedOut(t *testing.T) {
	type remainTarget struct {
		Name  string
		Extra map[string]any `mapstructure:",remain"`
	}

	result := remainTarget{Extra: map[string]any{"keep": "me"}}

	input := map[string]any{
		"name": "new-name",
		"foo":  "bar",
	}

	maskDecode(t, maskDecoderConfig(&result, "name"), input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
	if !reflect.DeepEqual(result.Extra, map[string]any{"keep": "me"}) {
		t.Errorf("Extra: got %v, want old value preserved", result.Extra)
	}
}

func TestFieldMask_RemainSelected(t *testing.T) {
	type remainTarget struct {
		Name  string
		Extra map[string]any `mapstructure:",remain"`
	}

	var result remainTarget

	input := map[string]any{
		"name": "new-name",
		"foo":  "bar",
	}

	maskDecode(t, maskDecoderConfig(&result, "name", "extra"), input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
	if !reflect.DeepEqual(result.Extra, map[string]any{"foo": "bar"}) {
		t.Errorf("Extra: got %v, want leftover keys", result.Extra)
	}
}

func TestFieldMask_StructInSliceAndMap(t *testing.T) {
	type container struct {
		Servers []maskAddress
		Lookup  map[string]maskAddress
		Name    string
	}

	var result container

	input := map[string]any{
		"servers": []any{
			map[string]any{"street": "s1", "city": "c1"},
		},
		"lookup": map[string]any{
			"main": map[string]any{"street": "s2", "city": "c2"},
		},
		"name": "new-name",
	}

	maskDecode(t, maskDecoderConfig(&result, "servers", "lookup"), input)

	if !reflect.DeepEqual(result.Servers, []maskAddress{{Street: "s1", City: "c1"}}) {
		t.Errorf("Servers: got %+v", result.Servers)
	}
	if !reflect.DeepEqual(result.Lookup, map[string]maskAddress{"main": {Street: "s2", City: "c2"}}) {
		t.Errorf("Lookup: got %+v", result.Lookup)
	}
	if result.Name != "" {
		t.Errorf("Name: got %q, want zero value", result.Name)
	}
}

func TestFieldMask_SquashNestedPathThroughOwnName(t *testing.T) {
	result := MaskSquashOuter{
		MaskSquashInner: MaskSquashInner{Name: "old-name", Age: 42},
		Active:          true,
	}

	input := map[string]any{
		"name":   "new-name",
		"age":    7,
		"active": false,
	}

	maskDecode(t, maskDecoderConfig(&result, "MaskSquashInner.Name"), input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
	if result.Age != 42 {
		t.Errorf("Age: got %d, want old value 42", result.Age)
	}
	if !result.Active {
		t.Error("Active: got false, want old value true")
	}
}

func TestFieldMask_SquashConfigOption(t *testing.T) {
	type outer struct {
		MaskSquashInner
		Active bool
	}

	result := outer{
		MaskSquashInner: MaskSquashInner{Name: "old-name", Age: 42},
		Active:          true,
	}

	input := map[string]any{
		"name":   "new-name",
		"age":    7,
		"active": false,
	}

	config := maskDecoderConfig(&result, "name")
	config.Squash = true
	maskDecode(t, config, input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
	if result.Age != 42 {
		t.Errorf("Age: got %d, want old value 42", result.Age)
	}
	if !result.Active {
		t.Error("Active: got false, want old value true")
	}
}

func TestFieldMask_RootName(t *testing.T) {
	result := maskPerson{
		Name: "old-name",
		Age:  42,
	}

	input := map[string]any{
		"name": "new-name",
		"age":  7,
	}

	config := maskDecoderConfig(&result, "name")
	config.RootName = "cfg"
	maskDecode(t, config, input)

	if result.Name != "new-name" {
		t.Errorf("Name: got %q, want %q", result.Name, "new-name")
	}
	if result.Age != 42 {
		t.Errorf("Age: got %d, want old value 42", result.Age)
	}
}
