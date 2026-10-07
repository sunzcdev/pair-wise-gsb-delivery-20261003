package aconfig

import (
	"reflect"
	"strings"
	"testing"
)

type layersSubConfig struct {
	Host string `json:"host" default:"localhost"`
	Port int    `json:"port" default:"8080"`
}

type layersConfig struct {
	Name   string            `json:"name" default:"app"`
	Token  string            `json:"token"`
	Level  int               `json:"level" default:"1"`
	Tags   []string          `json:"tags" default:"a,b"`
	Labels map[string]string `json:"labels" default:"env:dev"`
	Sub    layersSubConfig   `json:"sub"`
	Ptr    *layersSubConfig  `json:"ptr"`
}

func layersLoader(cfg *layersConfig, cfgOpts Config) *Loader {
	cfgOpts.SkipFiles = true
	cfgOpts.SkipEnv = true
	cfgOpts.SkipFlags = true
	return LoaderFor(cfg, cfgOpts)
}

func TestLoadLayersInterleaved(t *testing.T) {
	var cfg layersConfig
	loader := layersLoader(&cfg, Config{AllowDeleteDefaults: true})

	err := loader.LoadLayers(
		Layer{Values: map[string]any{
			"name":  "one",
			"level": 10,
			"token": "t1",
			"sub":   map[string]any{"host": "h1"},
		}},
		Layer{Values: map[string]any{
			"name":  Delete{},
			"level": 20,
			"token": "",
		}},
		Layer{Values: map[string]any{
			"name": "three",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Name != "three" {
		t.Fatalf("Name: got %q want %q", cfg.Name, "three")
	}
	if cfg.Token != "" {
		t.Fatalf("Token: got %q want explicit empty string", cfg.Token)
	}
	if cfg.Level != 20 {
		t.Fatalf("Level: got %d want 20", cfg.Level)
	}
	if cfg.Sub.Host != "h1" {
		t.Fatalf("Sub.Host: got %q want %q", cfg.Sub.Host, "h1")
	}
	if cfg.Sub.Port != 8080 {
		t.Fatalf("Sub.Port: got %d want 8080", cfg.Sub.Port)
	}
	if cfg.Ptr == nil || cfg.Ptr.Host != "localhost" {
		t.Fatalf("Ptr: got %+v want defaults applied", cfg.Ptr)
	}
}

func TestLoadLayersDeleteZeroAbsent(t *testing.T) {
	type reqConfig struct {
		Deleted string `json:"deleted" required:"true"`
		Zero    string `json:"zero" required:"true"`
		Absent  string `json:"absent" default:"c-def"`
	}

	t.Run("delete is not a value", func(t *testing.T) {
		var cfg reqConfig
		loader := LoaderFor(&cfg, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
		})
		err := loader.LoadLayers(Layer{Values: map[string]any{
			"deleted": Delete{},
			"zero":    "",
		}})
		if err == nil || !strings.Contains(err.Error(), "Deleted") {
			t.Fatalf("want required error for Deleted, got: %v", err)
		}
		if cfg != (reqConfig{}) {
			t.Fatalf("destination must be unchanged on error, got %+v", cfg)
		}
	})

	t.Run("explicit zero is a value", func(t *testing.T) {
		var cfg reqConfig
		loader := LoaderFor(&cfg, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
		})
		err := loader.LoadLayers(Layer{Values: map[string]any{
			"deleted": "set",
			"zero":    "",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Zero != "" {
			t.Fatalf("Zero: got %q want explicit empty string", cfg.Zero)
		}
		if cfg.Absent != "c-def" {
			t.Fatalf("Absent: got %q want default %q", cfg.Absent, "c-def")
		}
	})

	t.Run("higher layer can set after delete", func(t *testing.T) {
		var cfg reqConfig
		loader := LoaderFor(&cfg, Config{
			SkipFiles: true, SkipEnv: true, SkipFlags: true,
		})
		err := loader.LoadLayers(
			Layer{Values: map[string]any{"deleted": Delete{}, "zero": Delete{}}},
			Layer{Values: map[string]any{"deleted": "back", "zero": ""}},
		)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Deleted != "back" {
			t.Fatalf("Deleted: got %q want %q", cfg.Deleted, "back")
		}
	})
}

func TestLoadLayersDeleteDefaultPolicy(t *testing.T) {
	t.Run("forbidden by default", func(t *testing.T) {
		var cfg layersConfig
		loader := layersLoader(&cfg, Config{})
		err := loader.LoadLayers(Layer{Values: map[string]any{
			"name": Delete{},
		}})
		if err == nil || !strings.Contains(err.Error(), "AllowDeleteDefaults") {
			t.Fatalf("want policy error, got: %v", err)
		}
		if cfg.Name != "" || cfg.Level != 0 || cfg.Tags != nil || cfg.Labels != nil || cfg.Ptr == nil {
			t.Fatalf("destination must be unchanged on error, got %+v", cfg)
		}
	})

	t.Run("allowed by policy", func(t *testing.T) {
		var cfg layersConfig
		loader := layersLoader(&cfg, Config{AllowDeleteDefaults: true})
		err := loader.LoadLayers(Layer{Values: map[string]any{
			"name": Delete{},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Name != "" {
			t.Fatalf("Name: got %q want deleted to zero value", cfg.Name)
		}
		if cfg.Level != 1 {
			t.Fatalf("Level: got %d want default 1", cfg.Level)
		}
	})
}

func TestLoadLayersMergeKinds(t *testing.T) {
	var cfg layersConfig
	loader := layersLoader(&cfg, Config{})

	err := loader.LoadLayers(
		Layer{Values: map[string]any{
			"tags":   []any{"x", "y"},
			"labels": map[string]any{"a": "1"},
			"ptr":    map[string]any{"host": "ph"},
		}},
		Layer{Values: map[string]any{
			"tags":   []any{"z"},
			"labels": map[string]any{"b": "2", "env": Delete{}},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"z"}; !reflect.DeepEqual(cfg.Tags, want) {
		t.Fatalf("Tags: got %v want %v (slices are replaced)", cfg.Tags, want)
	}
	wantLabels := map[string]string{"a": "1", "b": "2"}
	if !reflect.DeepEqual(cfg.Labels, wantLabels) {
		t.Fatalf("Labels: got %v want %v (maps are merged, keys deletable)", cfg.Labels, wantLabels)
	}
	if cfg.Ptr == nil || cfg.Ptr.Host != "ph" || cfg.Ptr.Port != 8080 {
		t.Fatalf("Ptr: got %+v want field-wise merge with defaults", cfg.Ptr)
	}
}

func TestLoadLayersSubtreeDelete(t *testing.T) {
	var cfg layersConfig
	loader := layersLoader(&cfg, Config{AllowDeleteDefaults: true})

	err := loader.LoadLayers(
		Layer{Values: map[string]any{
			"sub": map[string]any{"host": "h1"},
			"ptr": map[string]any{"host": "p1"},
		}},
		Layer{Values: map[string]any{
			"sub": Delete{},
			"ptr": Delete{},
		}},
		Layer{Values: map[string]any{
			"sub": map[string]any{"port": 9090},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Sub.Host != "" {
		t.Fatalf("Sub.Host: got %q want deleted to zero value", cfg.Sub.Host)
	}
	if cfg.Sub.Port != 9090 {
		t.Fatalf("Sub.Port: got %d want 9090 (re-set after delete)", cfg.Sub.Port)
	}
	if cfg.Ptr == nil || cfg.Ptr.Host != "" || cfg.Ptr.Port != 0 {
		t.Fatalf("Ptr: got %+v want deleted subtree", cfg.Ptr)
	}
}

func TestLoadLayersAtomicOnError(t *testing.T) {
	cfg := layersConfig{Name: "keep", Level: 5}
	loader := layersLoader(&cfg, Config{})
	before := cfg

	err := loader.LoadLayers(
		Layer{Values: map[string]any{"name": "new"}},
		Layer{Values: map[string]any{"level": "not-a-number"}},
	)
	if err == nil {
		t.Fatal("want error")
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatalf("destination must be unchanged on error:\nhave: %+v\nwant: %+v", cfg, before)
	}
}

func TestLoadLayersNoReservedString(t *testing.T) {
	var cfg layersConfig
	loader := layersLoader(&cfg, Config{})

	err := loader.LoadLayers(Layer{Values: map[string]any{
		"name":  "Delete",
		"token": "null",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "Delete" {
		t.Fatalf("Name: got %q want literal string", cfg.Name)
	}
	if cfg.Token != "null" {
		t.Fatalf("Token: got %q want literal string", cfg.Token)
	}
}

func TestLoadLayersUnknownField(t *testing.T) {
	var cfg layersConfig
	loader := layersLoader(&cfg, Config{})
	err := loader.LoadLayers(Layer{Values: map[string]any{"nope": 1}})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown field error, got: %v", err)
	}

	var cfg2 layersConfig
	loader2 := layersLoader(&cfg2, Config{AllowUnknownFields: true})
	if err := loader2.LoadLayers(Layer{Values: map[string]any{"nope": 1}}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLayersEnvTag(t *testing.T) {
	var cfg layersConfig
	loader := layersLoader(&cfg, Config{})

	err := loader.LoadLayers(Layer{
		Tag: "env",
		Values: map[string]any{
			"NAME":     "from-env-layer",
			"SUB_HOST": "env-host",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "from-env-layer" {
		t.Fatalf("Name: got %q", cfg.Name)
	}
	if cfg.Sub.Host != "env-host" {
		t.Fatalf("Sub.Host: got %q", cfg.Sub.Host)
	}
}
