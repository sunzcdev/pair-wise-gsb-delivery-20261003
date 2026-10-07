package aconfig

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

type includeTestConfig struct {
	Str   string `json:"str"`
	Port  int    `json:"port"`
	Host  string `json:"host"`
	Debug bool   `json:"debug"`
}

// countingDecoder counts how many times each file is decoded.
type countingDecoder struct {
	jsonDecoder
	decodes map[string]int
}

func (d *countingDecoder) DecodeFile(name string) (map[string]any, error) {
	d.decodes[name]++
	return d.jsonDecoder.DecodeFile(name)
}

func includeLoader(cfg *includeTestConfig, fsys fs.FS, files ...string) (*Loader, *countingDecoder) {
	dec := &countingDecoder{decodes: map[string]int{}}
	loader := LoaderFor(cfg, Config{
		NewParser:    newParser,
		SkipDefaults: true,
		SkipEnv:      true,
		SkipFlags:    true,
		FileSystem:   fsys,
		Files:        files,
		FileDecoders: map[string]FileDecoder{
			".json": dec,
		},
	})
	return loader, dec
}

func TestFileIncludesDiamond(t *testing.T) {
	mapFS := fstest.MapFS{
		"cfg/shared.json": &fstest.MapFile{Data: []byte(`{
			"str": "shared",
			"port": 1,
			"host": "shared-host"
		}`)},
		"cfg/a.json": &fstest.MapFile{Data: []byte(`{
			"include": "shared.json",
			"str": "a",
			"port": 10
		}`)},
		// references the same shared.json via a different relative spelling
		"cfg/b.json": &fstest.MapFile{Data: []byte(`{
			"include": "./sub/../shared.json",
			"str": "b",
			"debug": true
		}`)},
		"cfg/root.json": &fstest.MapFile{Data: []byte(`{
			"include": ["a.json", "b.json"],
			"port": 100
		}`)},
	}

	var cfg includeTestConfig
	loader, dec := includeLoader(&cfg, mapFS, "cfg/root.json")
	failIfErr(t, loader.Load())

	// load order: shared, a, b, root - later files override earlier ones
	want := includeTestConfig{
		Str:   "b", // b.json is loaded after a.json
		Port:  100, // root.json overrides its includes
		Host:  "shared-host",
		Debug: true,
	}
	mustEqual(t, cfg, want)

	// each file must be decoded exactly once, even shared.json which is
	// referenced by both a.json and b.json via different spellings
	for _, name := range []string{"cfg/root.json", "cfg/a.json", "cfg/b.json", "cfg/shared.json"} {
		mustEqual(t, dec.decodes[name], 1)
	}
}

func TestFileIncludesReadFailure(t *testing.T) {
	mapFS := fstest.MapFS{
		"good.json": &fstest.MapFile{Data: []byte(`{"port": 10}`)},
		"root.json": &fstest.MapFile{Data: []byte(`{
			"include": ["good.json", "missing.json"],
			"str": "root"
		}`)},
	}

	var cfg includeTestConfig
	loader, _ := includeLoader(&cfg, mapFS, "root.json")
	if err := loader.Load(); err == nil {
		t.Fatal("expected an error for a missing include")
	}

	// nothing may be applied, not even from successfully decoded files
	mustEqual(t, cfg, includeTestConfig{})
}

func TestFileIncludesCycle(t *testing.T) {
	mapFS := fstest.MapFS{
		"a.json": &fstest.MapFile{Data: []byte(`{"include": "b.json", "port": 1}`)},
		"b.json": &fstest.MapFile{Data: []byte(`{"include": "a.json", "port": 2}`)},
	}

	var cfg includeTestConfig
	loader, _ := includeLoader(&cfg, mapFS, "a.json")
	err := loader.Load()
	if err == nil {
		t.Fatal("expected a cycle error")
	}

	// the error must contain the full reference chain
	chain := "a.json -> b.json -> a.json"
	if !strings.Contains(err.Error(), chain) {
		t.Fatalf("error %q does not contain the reference chain %q", err, chain)
	}
	mustEqual(t, cfg, includeTestConfig{})
}

func TestFileIncludesSelfCycle(t *testing.T) {
	mapFS := fstest.MapFS{
		"self.json": &fstest.MapFile{Data: []byte(`{"include": "./self.json"}`)},
	}

	var cfg includeTestConfig
	loader, _ := includeLoader(&cfg, mapFS, "self.json")
	err := loader.Load()
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "self.json -> self.json") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFileIncludesEscapeRoot(t *testing.T) {
	mapFS := fstest.MapFS{
		"sub/root.json": &fstest.MapFile{Data: []byte(`{"include": "../../outside.json"}`)},
	}

	var cfg includeTestConfig
	loader, _ := includeLoader(&cfg, mapFS, "sub/root.json")
	err := loader.Load()
	if err == nil {
		t.Fatal("expected an escape error")
	}
	if !strings.Contains(err.Error(), "escapes the config file system root") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFileIncludesSharedDependencyOrder(t *testing.T) {
	// two top-level files share a dependency: the dependency is loaded once,
	// before both of them, and the top-level files apply in Files order
	mapFS := fstest.MapFS{
		"base.json":  &fstest.MapFile{Data: []byte(`{"str": "base", "port": 1, "host": "base"}`)},
		"first.json": &fstest.MapFile{Data: []byte(`{"include": "base.json", "str": "first"}`)},
		"last.json":  &fstest.MapFile{Data: []byte(`{"include": "base.json", "port": 2}`)},
	}

	var cfg includeTestConfig
	loader, dec := includeLoader(&cfg, mapFS, "first.json", "last.json")
	loader.config.MergeFiles = true
	failIfErr(t, loader.Load())

	want := includeTestConfig{
		Str:  "first",
		Port: 2, // last.json wins over first.json and base.json
		Host: "base",
	}
	mustEqual(t, cfg, want)
	mustEqual(t, dec.decodes["base.json"], 1)
}
