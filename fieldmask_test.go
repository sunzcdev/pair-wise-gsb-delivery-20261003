package mapstructure

import (
	"reflect"
	"strings"
	"testing"
)

type fieldMaskLogging struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
}

type fieldMaskServer struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	TLS  bool   `mapstructure:"tls"`
}

type fieldMaskConfig struct {
	Name    string            `mapstructure:"name"`
	Server  fieldMaskServer   `mapstructure:"server"`
	Logging *fieldMaskLogging `mapstructure:"logging"`
	Labels  map[string]string `mapstructure:"labels"`
}

func fieldMaskConfigDecoder(result *fieldMaskConfig, mask []string) (*Decoder, error) {
	return NewDecoder(&DecoderConfig{
		Result:    result,
		FieldMask: mask,
	})
}

// Selecting only leaf paths updates exactly those fields and preserves
// the neighboring fields of an object carrying old values.
func TestFieldMask_LeafPreservesSiblings(t *testing.T) {
	result := fieldMaskConfig{
		Name:   "old-name",
		Server: fieldMaskServer{Host: "old-host", Port: 8080, TLS: true},
		Labels: map[string]string{"env": "prod"},
	}

	input := map[string]any{
		"name": "new-name",
		"server": map[string]any{
			"host": "new-host",
			"port": 9090,
			"tls":  false,
		},
		"labels": map[string]string{"env": "dev"},
	}

	decoder, err := fieldMaskConfigDecoder(&result, []string{"name", "server.host"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	expected := fieldMaskConfig{
		Name:   "new-name",
		Server: fieldMaskServer{Host: "new-host", Port: 8080, TLS: true},
		Labels: map[string]string{"env": "prod"},
	}
	if !reflect.DeepEqual(result, expected) {
		t.Fatalf("bad result: got %#v, expected %#v", result, expected)
	}
}

// Selecting a parent node includes its entire subtree.
func TestFieldMask_ParentIncludesSubtree(t *testing.T) {
	result := fieldMaskConfig{
		Name:   "old-name",
		Server: fieldMaskServer{Host: "old-host", Port: 8080, TLS: true},
	}

	input := map[string]any{
		"name": "new-name",
		"server": map[string]any{
			"host": "new-host",
			"port": 9090,
			"tls":  false,
		},
	}

	decoder, err := fieldMaskConfigDecoder(&result, []string{"server"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	expected := fieldMaskConfig{
		Name:   "old-name",
		Server: fieldMaskServer{Host: "new-host", Port: 9090, TLS: false},
	}
	if !reflect.DeepEqual(result, expected) {
		t.Fatalf("bad result: got %#v, expected %#v", result, expected)
	}
}

// Masked-out fields must not reach the DecodeHook. A counting hook
// verifies the exact decoding boundary.
func TestFieldMask_CountingHook(t *testing.T) {
	var result fieldMaskConfig

	var calls []any
	hook := DecodeHookFuncValue(func(from, to reflect.Value) (any, error) {
		calls = append(calls, from.Interface())
		return from.Interface(), nil
	})

	decoder, err := NewDecoder(&DecoderConfig{
		Result:     &result,
		DecodeHook: hook,
		FieldMask:  []string{"server.host"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	input := map[string]any{
		"name": "new-name",
		"server": map[string]any{
			"host": "new-host",
			"port": 9090,
		},
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// The hook fires for the root map, the "server" map and
	// "server.host" only: 3 calls.
	if len(calls) != 3 {
		t.Fatalf("bad hook call count: got %d, expected 3 (%#v)", len(calls), calls)
	}
	for _, call := range calls {
		if call == "new-name" || call == 9090 {
			t.Fatalf("hook invoked for masked-out input: %#v", calls)
		}
	}

	if result.Server.Host != "new-host" {
		t.Fatalf("bad result: got %#v", result)
	}
}

// Invalid mask paths are reported by NewDecoder before any input is
// processed.
func TestFieldMask_InvalidPaths(t *testing.T) {
	cases := []string{
		"servers",           // unknown top-level field
		"server.ports",      // unknown nested field
		"server.host.name",  // cannot descend into a string
		"labels.env",        // cannot descend into a map value
		"logging.level.foo", // cannot descend into a string behind a pointer
		"name.",             // empty path segment
		"..name",            // empty path segment
		"",                  // empty path
	}

	for _, mask := range cases {
		result := fieldMaskConfig{Name: "old-name"}
		_, err := fieldMaskConfigDecoder(&result, []string{mask})
		if err == nil {
			t.Errorf("expected error for mask %q, got none", mask)
			continue
		}
		if !strings.Contains(err.Error(), "field mask") {
			t.Errorf("error for mask %q does not mention the field mask: %s", mask, err)
		}
		// The target must remain untouched.
		if result.Name != "old-name" {
			t.Errorf("result modified for invalid mask %q: %#v", mask, result)
		}
	}
}

// A mask requires a struct result.
func TestFieldMask_NonStructResult(t *testing.T) {
	var result map[string]any
	_, err := NewDecoder(&DecoderConfig{
		Result:    &result,
		FieldMask: []string{"name"},
	})
	if err == nil {
		t.Fatal("expected error for non-struct result, got none")
	}
}

// Squashed embedded structs contribute their fields at the parent level.
func TestFieldMask_Squash(t *testing.T) {
	type Inner struct {
		Host string `mapstructure:"host"`
		Port int    `mapstructure:"port"`
	}
	type Outer struct {
		Inner `mapstructure:",squash"`
		Name  string `mapstructure:"name"`
	}

	result := Outer{
		Inner: Inner{Host: "old-host", Port: 8080},
		Name:  "old-name",
	}

	input := map[string]any{
		"host": "new-host",
		"port": 9090,
		"name": "new-name",
	}

	decoder, err := NewDecoder(&DecoderConfig{
		Result:    &result,
		FieldMask: []string{"host"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	expected := Outer{
		Inner: Inner{Host: "new-host", Port: 8080},
		Name:  "old-name",
	}
	if !reflect.DeepEqual(result, expected) {
		t.Fatalf("bad result: got %#v, expected %#v", result, expected)
	}

	// Squashed fields are addressed at the parent level: addressing them
	// through the embedded field name is invalid.
	if _, err := NewDecoder(&DecoderConfig{
		Result:    &Outer{},
		FieldMask: []string{"Inner.host"},
	}); err == nil {
		t.Fatal("expected error for path through squashed field, got none")
	}
}

// Mask paths traverse pointers to structs; a nil pointer is allocated
// only when a selected field below it is decoded.
func TestFieldMask_Pointer(t *testing.T) {
	result := fieldMaskConfig{
		Logging: &fieldMaskLogging{Level: "info", Format: "json"},
	}

	input := map[string]any{
		"logging": map[string]any{
			"level":  "debug",
			"format": "text",
		},
	}

	decoder, err := fieldMaskConfigDecoder(&result, []string{"logging.level"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	expected := &fieldMaskLogging{Level: "debug", Format: "json"}
	if !reflect.DeepEqual(result.Logging, expected) {
		t.Fatalf("bad result: got %#v, expected %#v", result.Logging, expected)
	}

	// A masked-out pointer field is left nil.
	result = fieldMaskConfig{}
	decoder, err = fieldMaskConfigDecoder(&result, []string{"name"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if result.Logging != nil {
		t.Fatalf("expected logging to stay nil, got %#v", result.Logging)
	}
}

// Map fields are leaves: selecting the field includes the whole map
// value, and paths extending into the map are invalid.
func TestFieldMask_MapLeaf(t *testing.T) {
	result := fieldMaskConfig{
		Name:   "old-name",
		Labels: map[string]string{"env": "prod"},
	}

	input := map[string]any{
		"name":   "new-name",
		"labels": map[string]string{"env": "dev", "team": "infra"},
	}

	decoder, err := fieldMaskConfigDecoder(&result, []string{"labels"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if result.Name != "old-name" {
		t.Fatalf("bad name: got %q", result.Name)
	}
	expected := map[string]string{"env": "dev", "team": "infra"}
	if !reflect.DeepEqual(result.Labels, expected) {
		t.Fatalf("bad labels: got %#v, expected %#v", result.Labels, expected)
	}
}

// By default, input keys for masked-out fields are silently ignored even
// when ErrorUnused is set. FieldMaskErrorUnused makes them surface.
func TestFieldMask_ErrorUnusedOption(t *testing.T) {
	// Default: masked-out keys are not reported as unused.
	var result fieldMaskConfig
	var metadata Metadata
	decoder, err := NewDecoder(&DecoderConfig{
		Result:      &result,
		Metadata:    &metadata,
		ErrorUnused: true,
		FieldMask:   []string{"name"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(map[string]any{
		"name":   "new-name",
		"server": map[string]any{"host": "new-host"},
	}); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if len(metadata.Unused) != 0 {
		t.Fatalf("bad unused keys: got %#v", metadata.Unused)
	}

	// Opt-in: masked-out keys are reported as unused.
	result = fieldMaskConfig{}
	metadata = Metadata{}
	decoder, err = NewDecoder(&DecoderConfig{
		Result:               &result,
		Metadata:             &metadata,
		FieldMask:            []string{"name"},
		FieldMaskErrorUnused: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(map[string]any{
		"name":   "new-name",
		"server": map[string]any{"host": "new-host"},
	}); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !reflect.DeepEqual(metadata.Unused, []string{"server"}) {
		t.Fatalf("bad unused keys: got %#v", metadata.Unused)
	}

	// Opt-in combined with ErrorUnused: masked-out keys are an error.
	result = fieldMaskConfig{}
	decoder, err = NewDecoder(&DecoderConfig{
		Result:               &result,
		ErrorUnused:          true,
		FieldMask:            []string{"name"},
		FieldMaskErrorUnused: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	err = decoder.Decode(map[string]any{
		"name":   "new-name",
		"server": map[string]any{"host": "new-host"},
	})
	if err == nil {
		t.Fatal("expected unused-key error, got none")
	}
	if !strings.Contains(err.Error(), "server") {
		t.Fatalf("error does not mention masked-out key: %s", err)
	}
}

// Masked-out fields are not reported as unset when ErrorUnset is set.
func TestFieldMask_ErrorUnset(t *testing.T) {
	var result fieldMaskConfig
	decoder, err := NewDecoder(&DecoderConfig{
		Result:     &result,
		ErrorUnset: true,
		FieldMask:  []string{"name"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(map[string]any{"name": "new-name"}); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

// The mask also applies when decoding from a struct (not a map): the
// direct-assign shortcut must not bypass it.
func TestFieldMask_StructInput(t *testing.T) {
	result := fieldMaskConfig{
		Name:   "old-name",
		Server: fieldMaskServer{Host: "old-host", Port: 8080},
	}

	input := fieldMaskConfig{
		Name:   "new-name",
		Server: fieldMaskServer{Host: "new-host", Port: 9090},
	}

	decoder, err := fieldMaskConfigDecoder(&result, []string{"server"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(input); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if result.Name != "old-name" {
		t.Fatalf("bad name: got %q", result.Name)
	}
	if result.Server.Host != "new-host" || result.Server.Port != 9090 {
		t.Fatalf("bad server: got %#v", result.Server)
	}
}

// Selecting both a parent and a path below it selects the whole subtree.
func TestFieldMask_ParentAndChild(t *testing.T) {
	var result fieldMaskConfig
	decoder, err := fieldMaskConfigDecoder(&result, []string{"server.host", "server"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(map[string]any{
		"server": map[string]any{"host": "new-host", "port": 9090},
	}); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if result.Server.Host != "new-host" || result.Server.Port != 9090 {
		t.Fatalf("bad server: got %#v", result.Server)
	}
}
