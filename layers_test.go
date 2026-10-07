package aconfig

import (
	"reflect"
	"strings"
	"testing"
)

type layerNested struct {
	Host string `default:"localhost"`
	Port int    `default:"5432"`
}

type layerConfig struct {
	Str     string `default:"def-str" required:"true"`
	Num     int    `default:"42"`
	Ptr     *string
	Slice   []string
	Labels  map[string]string
	Nest    layerNested
	NestPtr *layerNested
	Token   string `required:"true"`
}

func strptr(s string) *string { return &s }

func testLayerLoader(cfg *layerConfig) *Loader {
	return LoaderFor(cfg, Config{
		SkipFiles: true,
		SkipEnv:   true,
		SkipFlags: true,
	})
}

func TestLayersThreeWayInterleaved(t *testing.T) {
	var cfg layerConfig
	loader := testLayerLoader(&cfg)

	layers := []Layer{
		{
			Name: "base",
			Values: map[string]any{
				"str":    "base",
				"num":    100,
				"ptr":    "p1",
				"slice":  []any{"a", "b"},
				"labels": map[string]any{"x": "1", "y": "2"},
				"nest":   map[string]any{"host": "l1-host"},
				"token":  "tok1",
			},
		},
		{
			Name: "override",
			Values: map[string]any{
				// revoke values set by the base layer
				"str": Delete,
				"ptr": Delete,
				// explicit zero value must win over the base layer
				"num": 0,
				// slices are replaced wholesale
				"slice": []any{"c"},
				// maps merge key by key, Delete removes a single key
				"labels": map[string]any{"y": Delete, "z": "3"},
				// nested structs merge leaf by leaf
				"nest": map[string]any{"host": Delete},
				// nested pointer structs are allocated on demand
				"nest_ptr": map[string]any{"host": "ptr-host"},
			},
		},
		{
			Name: "top",
			Values: map[string]any{
				// higher layers can re-assign after a delete
				"str":  "final",
				"ptr":  "p3",
				"nest": map[string]any{"host": "re-host"},
			},
		},
	}

	if err := loader.LoadLayers(layers, LayerPolicy{}); err != nil {
		t.Fatal(err)
	}

	want := layerConfig{
		Str:    "final",
		Num:    0, // explicit zero, not the default and not "base"
		Ptr:    strptr("p3"),
		Slice:  []string{"c"},
		Labels: map[string]string{"x": "1", "z": "3"},
		Nest: layerNested{
			Host: "re-host",
			Port: 5432, // default untouched by other layers
		},
		NestPtr: &layerNested{Host: "ptr-host", Port: 5432},
		Token:   "tok1",
	}

	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("\nhave: %+v\nwant: %+v", cfg, want)
	}
}

func TestLayersDeleteVsExplicitZeroVsMissing(t *testing.T) {
	var cfg layerConfig
	loader := testLayerLoader(&cfg)

	layers := []Layer{
		{
			Name: "base",
			Values: map[string]any{
				"str":   "base",
				"token": "tok",
				"ptr":   "p1",
			},
		},
		{
			Name: "top",
			Values: map[string]any{
				// explicit empty string is a legal value and satisfies required
				"str": "",
				// delete revokes the pointer: it must become nil
				"ptr": Delete,
			},
		},
	}

	if err := loader.LoadLayers(layers, LayerPolicy{}); err != nil {
		t.Fatal(err)
	}

	if cfg.Str != "" {
		t.Fatalf("explicit empty string lost, got %q", cfg.Str)
	}
	if cfg.Ptr != nil {
		t.Fatalf("deleted pointer must be nil, got %v", *cfg.Ptr)
	}
	if cfg.Num != 42 {
		t.Fatalf("never provided field must keep its default, got %d", cfg.Num)
	}
}

func TestLayersDeleteRequiredFails(t *testing.T) {
	cfg := layerConfig{Str: "keep", Num: 7, Labels: map[string]string{"k": "v"}}
	loader := testLayerLoader(&cfg)

	layers := []Layer{
		{Name: "base", Values: map[string]any{"token": "tok"}},
		// delete the required field and never set it again
		{Name: "top", Values: map[string]any{"token": Delete}},
	}

	err := loader.LoadLayers(layers, LayerPolicy{})
	if err == nil {
		t.Fatal("expected required error after deleting a required field")
	}
	if !strings.Contains(err.Error(), "Token") {
		t.Fatalf("error should mention the required field, got: %v", err)
	}

	// the failed load must not modify the destination
	if cfg.Str != "keep" || cfg.Num != 7 {
		t.Fatalf("destination modified on error: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Labels, map[string]string{"k": "v"}) {
		t.Fatalf("map modified on error: %+v", cfg.Labels)
	}
}

func TestLayersDeleteDefaultPolicy(t *testing.T) {
	layers := []Layer{
		// Num still holds its default value at this point
		{Name: "top", Values: map[string]any{"num": Delete, "token": "tok"}},
	}

	t.Run("forbidden", func(t *testing.T) {
		cfg := layerConfig{Str: "keep"}
		loader := testLayerLoader(&cfg)

		err := loader.LoadLayers(layers, LayerPolicy{AllowDeleteDefault: false})
		if err == nil {
			t.Fatal("expected error when deleting a default value without policy opt-in")
		}
		if cfg.Str != "keep" || cfg.Num != 0 {
			t.Fatalf("destination modified on error: %+v", cfg)
		}
	})

	t.Run("allowed", func(t *testing.T) {
		var cfg layerConfig
		loader := testLayerLoader(&cfg)

		if err := loader.LoadLayers(layers, LayerPolicy{AllowDeleteDefault: true}); err != nil {
			t.Fatal(err)
		}
		if cfg.Num != 0 {
			t.Fatalf("deleted default must reset the field to zero, got %d", cfg.Num)
		}
	})

	t.Run("layer value is not a default", func(t *testing.T) {
		var cfg layerConfig
		loader := testLayerLoader(&cfg)

		layers := []Layer{
			{Name: "base", Values: map[string]any{"num": 100, "token": "tok"}},
			// deleting a value set by a layer is fine even without the policy
			{Name: "top", Values: map[string]any{"num": Delete}},
		}
		if err := loader.LoadLayers(layers, LayerPolicy{}); err != nil {
			t.Fatal(err)
		}
		if cfg.Num != 0 {
			t.Fatalf("delete must shadow the lower layer, got %d", cfg.Num)
		}
	})
}

func TestLayersSubtreeDelete(t *testing.T) {
	var cfg layerConfig
	loader := testLayerLoader(&cfg)

	layers := []Layer{
		{
			Name: "base",
			Values: map[string]any{
				"token": "tok",
				"nest":  map[string]any{"host": "h", "port": 1},
			},
		},
		{
			Name: "top",
			Values: map[string]any{
				// revoke the whole subtree with one marker
				"nest": Delete,
			},
		},
	}

	if err := loader.LoadLayers(layers, LayerPolicy{AllowDeleteDefault: true}); err != nil {
		t.Fatal(err)
	}
	if cfg.Nest.Host != "" || cfg.Nest.Port != 0 {
		t.Fatalf("subtree delete must reset all leaves, got %+v", cfg.Nest)
	}
}

func TestLayersUnknownFieldRestoresDestination(t *testing.T) {
	cfg := layerConfig{Str: "keep", Num: 7}
	loader := testLayerLoader(&cfg)

	layers := []Layer{
		{Name: "base", Values: map[string]any{"str": "changed", "token": "tok"}},
		{Name: "bad", Values: map[string]any{"nope": 1}},
	}

	err := loader.LoadLayers(layers, LayerPolicy{})
	if err == nil {
		t.Fatal("expected unknown field error")
	}
	if cfg.Str != "keep" || cfg.Num != 7 {
		t.Fatalf("destination modified on error: %+v", cfg)
	}
}

func TestLayersDeleteIsNotAReservedString(t *testing.T) {
	var cfg layerConfig
	loader := testLayerLoader(&cfg)

	layers := []Layer{
		{
			Name: "base",
			Values: map[string]any{
				// any string, including one that spells "Delete", is a value
				"str":   "Delete",
				"token": "tok",
			},
		},
	}

	if err := loader.LoadLayers(layers, LayerPolicy{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Str != "Delete" {
		t.Fatalf("a plain string must never act as a delete marker, got %q", cfg.Str)
	}
}

func TestLayersDefaultLoadUnchanged(t *testing.T) {
	var cfg layerConfig
	loader := testLayerLoader(&cfg)

	// defaults only, no layers involved: required fields are missing
	err := loader.Load()
	if err == nil {
		t.Fatal("expected required error from plain Load")
	}
	if cfg.Str != "def-str" || cfg.Num != 42 {
		t.Fatalf("plain Load must keep applying defaults, got %+v", cfg)
	}
}
