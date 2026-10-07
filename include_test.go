package aconfig

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

type includeTestConfig struct {
	Str string `json:"str"`
	Num int    `json:"num"`
	Who string `json:"who"`
}

// countingDecoder wraps the JSON decoder and counts file reads.
type countingDecoder struct {
	fsys  fs.FS
	reads map[string]int
}

func newCountingDecoder() *countingDecoder {
	return &countingDecoder{reads: map[string]int{}}
}

func (d *countingDecoder) Init(fsys fs.FS) { d.fsys = fsys }

func (d *countingDecoder) Format() string { return "json" }

func (d *countingDecoder) DecodeFile(filename string) (map[string]any, error) {
	d.reads[filename]++
	return (&jsonDecoder{fsys: d.fsys}).DecodeFile(filename)
}

func (d *countingDecoder) DecodeFileIncludes(filename string) (map[string]any, []string, error) {
	d.reads[filename]++
	return (&jsonDecoder{fsys: d.fsys}).DecodeFileIncludes(filename)
}

func includeLoader(t *testing.T, cfg *includeTestConfig, fsys fs.FS, dec FileDecoder, files ...string) error {
	t.Helper()
	loader := LoaderFor(cfg, Config{
		SkipEnv:    true,
		SkipFlags:  true,
		MergeFiles: true,
		FileSystem: fsys,
		Files:      files,
		FileDecoders: map[string]FileDecoder{
			".json": dec,
		},
	})
	return loader.Load()
}

func TestIncludeDiamond(t *testing.T) {
	fsys := fstest.MapFS{
		"base.json": &fstest.MapFile{Data: []byte(`{"str":"base","num":1,"who":"base"}`)},
		"mid1.json": &fstest.MapFile{Data: []byte(`{"include":"base.json","str":"mid1","who":"mid1"}`)},
		"mid2.json": &fstest.MapFile{Data: []byte(`{"include":"base.json","num":2,"who":"mid2"}`)},
		"top.json":  &fstest.MapFile{Data: []byte(`{"include":["mid1.json","mid2.json"],"who":"top"}`)},
	}
	dec := newCountingDecoder()

	var cfg includeTestConfig
	if err := includeLoader(t, &cfg, fsys, dec, "top.json"); err != nil {
		t.Fatal(err)
	}

	// Deterministic apply order: base, mid1, mid2, top.
	want := includeTestConfig{Str: "mid1", Num: 2, Who: "top"}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("have: %+v, want: %+v", cfg, want)
	}

	for _, file := range []string{"base.json", "mid1.json", "mid2.json", "top.json"} {
		if dec.reads[file] != 1 {
			t.Fatalf("file %q read %d times, want exactly 1", file, dec.reads[file])
		}
	}
}

func TestIncludeSameFileDifferentSpellings(t *testing.T) {
	fsys := fstest.MapFS{
		"dir/shared.json":     &fstest.MapFile{Data: []byte(`{"str":"shared","num":7}`)},
		"dir/main.json":       &fstest.MapFile{Data: []byte(`{"include":["shared.json","./shared.json","sub/../shared.json"]}`)},
		"dir/inner/leaf.json": &fstest.MapFile{Data: []byte(`{"include":"../shared.json","who":"leaf"}`)},
	}
	dec := newCountingDecoder()

	var cfg includeTestConfig
	if err := includeLoader(t, &cfg, fsys, dec, "dir/main.json", "dir/inner/leaf.json"); err != nil {
		t.Fatal(err)
	}

	want := includeTestConfig{Str: "shared", Num: 7, Who: "leaf"}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("have: %+v, want: %+v", cfg, want)
	}
	if dec.reads["dir/shared.json"] != 1 {
		t.Fatalf("dir/shared.json read %d times, want exactly 1", dec.reads["dir/shared.json"])
	}
}

func TestIncludeMissingFile(t *testing.T) {
	fsys := fstest.MapFS{
		"top.json": &fstest.MapFile{Data: []byte(`{"include":"missing.json","str":"top"}`)},
	}

	var cfg includeTestConfig
	err := includeLoader(t, &cfg, fsys, newCountingDecoder(), "top.json")
	if err == nil {
		t.Fatal("expected an error for a missing include")
	}
	if !strings.Contains(err.Error(), "missing.json") {
		t.Fatalf("error should mention the missing file, got: %v", err)
	}
	if !reflect.DeepEqual(cfg, includeTestConfig{}) {
		t.Fatalf("failed load must not leave half-loaded fields, got: %+v", cfg)
	}
}

func TestIncludeCycle(t *testing.T) {
	fsys := fstest.MapFS{
		"a.json": &fstest.MapFile{Data: []byte(`{"include":"b.json","str":"a"}`)},
		"b.json": &fstest.MapFile{Data: []byte(`{"include":"a.json","num":2}`)},
	}

	var cfg includeTestConfig
	err := includeLoader(t, &cfg, fsys, newCountingDecoder(), "a.json")
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	wantChain := "a.json -> b.json -> a.json"
	if !strings.Contains(err.Error(), wantChain) {
		t.Fatalf("error should contain the full chain %q, got: %v", wantChain, err)
	}
	if !reflect.DeepEqual(cfg, includeTestConfig{}) {
		t.Fatalf("failed load must not leave half-loaded fields, got: %+v", cfg)
	}
}

func TestIncludeSelfCycle(t *testing.T) {
	fsys := fstest.MapFS{
		"self.json": &fstest.MapFile{Data: []byte(`{"include":"self.json"}`)},
	}

	var cfg includeTestConfig
	err := includeLoader(t, &cfg, fsys, newCountingDecoder(), "self.json")
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "self.json -> self.json") {
		t.Fatalf("error should contain the cycle chain, got: %v", err)
	}
}

func TestIncludeEscapesRoot(t *testing.T) {
	fsys := fstest.MapFS{
		"top.json": &fstest.MapFile{Data: []byte(`{"include":"../outside.json"}`)},
	}

	var cfg includeTestConfig
	err := includeLoader(t, &cfg, fsys, newCountingDecoder(), "top.json")
	if err == nil {
		t.Fatal("expected an error for an include escaping the root")
	}
	if !strings.Contains(err.Error(), "escapes the config filesystem root") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(cfg, includeTestConfig{}) {
		t.Fatalf("failed load must not leave half-loaded fields, got: %+v", cfg)
	}
}

func TestIncludeRelativeToReferrer(t *testing.T) {
	fsys := fstest.MapFS{
		"shared.json":     &fstest.MapFile{Data: []byte(`{"str":"root-shared"}`)},
		"dir/shared.json": &fstest.MapFile{Data: []byte(`{"str":"dir-shared","num":3}`)},
		"dir/main.json":   &fstest.MapFile{Data: []byte(`{"include":"shared.json","who":"main"}`)},
	}
	dec := newCountingDecoder()

	var cfg includeTestConfig
	if err := includeLoader(t, &cfg, fsys, dec, "dir/main.json"); err != nil {
		t.Fatal(err)
	}

	// "shared.json" must resolve relative to dir/main.json, not to the root.
	want := includeTestConfig{Str: "dir-shared", Num: 3, Who: "main"}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("have: %+v, want: %+v", cfg, want)
	}
	if dec.reads["shared.json"] != 0 {
		t.Fatalf("root shared.json must not be read, got %d reads", dec.reads["shared.json"])
	}
}
