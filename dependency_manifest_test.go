package gojq_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/itchyny/gojq"
)

func writeDependencyTestFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"outer.jq": `
			import "inner" as inner;
			def outerf: inner::innerf;
		`,
		"inner.jq":    `def innerf: . + 1;`,
		"data.json":   `{"answer": 42}`,
		"nested.jq":   `def nestedf: 100;`,
		"nested.json": `[1, 2, 3]`,
		".jq":         `def initf: 7;`,
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func compileWithManifest(t *testing.T, src string, dir string, manifest *gojq.DependencyManifest) *gojq.Code {
	t.Helper()
	q, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		q,
		gojq.WithModuleLoader(gojq.NewModuleLoader([]string{
			filepath.Join(dir, ".jq"), dir,
		})),
		gojq.WithDependencyManifest(manifest),
	)
	if err != nil {
		t.Fatalf("Compile failed: %v", err)
	}
	return code
}

func TestDependencyManifestRecordsActualLoads(t *testing.T) {
	dir := writeDependencyTestFiles(t)
	manifest := gojq.NewDependencyManifest()
	code := compileWithManifest(t,
		`import "outer" as outer; import "data" as $d; outer::outerf + $d[0].answer`,
		dir, manifest)

	deps := manifest.Dependencies()
	if len(deps) != 4 {
		t.Fatalf("expected 4 dependencies (init, outer, inner, data), got %d: %+v", len(deps), deps)
	}
	wantKinds := []gojq.DependencyKind{
		gojq.DependencyInitModule,
		gojq.DependencyModule,
		gojq.DependencyModule,
		gojq.DependencyJSON,
	}
	for i, dep := range deps {
		if dep.Kind != wantKinds[i] {
			t.Errorf("dependency %d kind: got %s, want %s", i, dep.Kind, wantKinds[i])
		}
		if dep.Source == "" || !filepath.IsAbs(dep.Source) {
			t.Errorf("dependency %d source should be an absolute path, got %q", i, dep.Source)
		}
		if dep.Size == 0 || !strings.HasPrefix(dep.Digest, "sha256:") {
			t.Errorf("dependency %d missing content summary: size=%d digest=%q", i, dep.Size, dep.Digest)
		}
	}
	if deps[0].Source != filepath.Join(dir, ".jq") {
		t.Errorf("init module source: got %q", deps[0].Source)
	}
	if deps[1].Name != "outer" || deps[2].Name != "inner" {
		t.Errorf("nested imports not recorded in order: %+v", deps[1:3])
	}
	if !strings.HasSuffix(deps[1].Source, "outer.jq") ||
		!strings.HasSuffix(deps[2].Source, "inner.jq") ||
		!strings.HasSuffix(deps[3].Source, "data.json") {
		t.Errorf("unexpected sources: %+v", deps)
	}

	iter := code.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("no result")
	}
	if err, ok := v.(error); ok {
		t.Fatal(err)
	}
	if diff := cmp.Diff(int(43), v); diff != "" {
		// innerf on null => null + 1 = 1; 1 + 42
		t.Errorf("result mismatch (-want +got):\n%s", diff)
	}

	locked, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var lock gojq.DependencyManifest
	if err := json.Unmarshal(locked, &lock); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(manifest.Dependencies(), lock.Dependencies()); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
	compileWithManifest(t,
		`import "outer" as outer; import "data" as $d; outer::outerf + $d[0].answer`,
		dir, &lock)
}

func TestDependencyManifestLocksContentChange(t *testing.T) {
	dir := writeDependencyTestFiles(t)
	src := `import "outer" as outer; outer::outerf`

	manifest := gojq.NewDependencyManifest()
	compileWithManifest(t, src, dir, manifest)
	locked, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	// The module name and the loaded path stay the same, but the content
	// changes (including the nested module). The next compile must fail.
	original, err := os.ReadFile(filepath.Join(dir, "inner.jq"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "inner.jq"), []byte(`def innerf: . + 2;`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(dir, "inner.jq"), original, 0o644)
	})

	var lock gojq.DependencyManifest
	if err := json.Unmarshal(locked, &lock); err != nil {
		t.Fatal(err)
	}
	q, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(
		q,
		gojq.WithModuleLoader(gojq.NewModuleLoader([]string{
			filepath.Join(dir, ".jq"), dir,
		})),
		gojq.WithDependencyManifest(&lock),
	)
	if err == nil {
		t.Fatal("expected lock failure on content change")
	}
	if !strings.Contains(err.Error(), "content changed") ||
		!strings.Contains(err.Error(), "inner") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Restoring the content makes the locked compile pass again.
	if err := os.WriteFile(filepath.Join(dir, "inner.jq"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(locked, &lock); err != nil {
		t.Fatal(err)
	}
	compileWithManifest(t, src, dir, &lock)
}

func TestDependencyManifestLocksJSONChange(t *testing.T) {
	dir := writeDependencyTestFiles(t)
	src := `import "data" as $d; $d[0].answer`
	manifest := gojq.NewDependencyManifest()
	compileWithManifest(t, src, dir, manifest)
	locked, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "data.json"), []byte(`{"answer": 43}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var lock gojq.DependencyManifest
	if err := json.Unmarshal(locked, &lock); err != nil {
		t.Fatal(err)
	}
	q, _ := gojq.Parse(src)
	_, err = gojq.Compile(
		q,
		gojq.WithModuleLoader(gojq.NewModuleLoader([]string{dir})),
		gojq.WithDependencyManifest(&lock),
	)
	if err == nil || !strings.Contains(err.Error(), "content changed") {
		t.Fatalf("expected JSON content changed error, got: %v", err)
	}
}

func TestDependencyManifestMissingAndUnusedLocks(t *testing.T) {
	dir := writeDependencyTestFiles(t)

	t.Run("missing", func(t *testing.T) {
		manifest := gojq.NewDependencyManifest()
		compileWithManifest(t, `import "inner" as inner; inner::innerf`, dir, manifest)
		locked, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		var lock gojq.DependencyManifest
		if err := json.Unmarshal(locked, &lock); err != nil {
			t.Fatal(err)
		}
		q, _ := gojq.Parse(`import "outer" as outer; outer::outerf`)
		_, err = gojq.Compile(
			q,
			gojq.WithModuleLoader(gojq.NewModuleLoader([]string{dir})),
			gojq.WithDependencyManifest(&lock),
		)
		if err == nil || !strings.Contains(err.Error(), "is not locked") {
			t.Fatalf("expected missing lock error, got: %v", err)
		}
	})

	t.Run("unused", func(t *testing.T) {
		manifest := gojq.NewDependencyManifest()
		compileWithManifest(t, `import "outer" as outer; outer::outerf`, dir, manifest)
		locked, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		var lock gojq.DependencyManifest
		if err := json.Unmarshal(locked, &lock); err != nil {
			t.Fatal(err)
		}
		q, _ := gojq.Parse(`import "inner" as inner; inner::innerf`)
		_, err = gojq.Compile(
			q,
			gojq.WithModuleLoader(gojq.NewModuleLoader([]string{dir})),
			gojq.WithDependencyManifest(&lock),
		)
		if err == nil || !strings.Contains(err.Error(), "not loaded by the query") {
			t.Fatalf("expected unused lock error, got: %v", err)
		}
	})
}

type evidenceLessLoader struct{}

func (*evidenceLessLoader) LoadModule(name string) (*gojq.Query, error) {
	return gojq.Parse(`def f: 1;`)
}

func TestDependencyManifestRequiresEvidence(t *testing.T) {
	q, err := gojq.Parse(`import "m" as m; m::f`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(
		q,
		gojq.WithModuleLoader(&evidenceLessLoader{}),
		gojq.WithDependencyManifest(gojq.NewDependencyManifest()),
	)
	if err == nil {
		t.Fatal("expected evidence error")
	}
	if !strings.Contains(err.Error(), "cannot lock") ||
		!strings.Contains(err.Error(), "LoadModuleWithEvidence") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Without a dependency manifest, the same loader keeps working and
	// never needs any evidence or file access.
	code, err := gojq.Compile(
		q,
		gojq.WithModuleLoader(&evidenceLessLoader{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("no result")
	}
	if err, ok := v.(error); ok {
		t.Fatal(err)
	}
	if v != int(1) {
		t.Fatalf("unexpected result: %#v", v)
	}
}

type evidenceLoader struct {
	modules map[string]string
}

func (l *evidenceLoader) LoadModuleWithEvidence(name string, _ map[string]any) (*gojq.ModuleEvidence, error) {
	cnt, ok := l.modules[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	q, err := gojq.Parse(cnt)
	if err != nil {
		return nil, err
	}
	return &gojq.ModuleEvidence{
		Query:   q,
		Source:  "memory://" + name + ".jq",
		Content: []byte(cnt),
	}, nil
}

func TestDependencyManifestCustomEvidenceLoader(t *testing.T) {
	loader := &evidenceLoader{map[string]string{
		"m": `def f: 42;`,
	}}
	manifest := gojq.NewDependencyManifest()
	q, _ := gojq.Parse(`import "m" as m; m::f`)
	code, err := gojq.Compile(
		q,
		gojq.WithModuleLoader(loader),
		gojq.WithDependencyManifest(manifest),
	)
	if err != nil {
		t.Fatal(err)
	}
	deps := manifest.Dependencies()
	if len(deps) != 1 || deps[0].Source != "memory://m.jq" {
		t.Fatalf("unexpected dependencies: %+v", deps)
	}
	locked, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	loader.modules["m"] = `def f: 43;`
	var lock gojq.DependencyManifest
	if err := json.Unmarshal(locked, &lock); err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(
		q,
		gojq.WithModuleLoader(loader),
		gojq.WithDependencyManifest(&lock),
	)
	if err == nil || !strings.Contains(err.Error(), "content changed") {
		t.Fatalf("expected content changed error, got: %v", err)
	}

	loader.modules["m"] = `def f: 42;`
	if err := json.Unmarshal(locked, &lock); err != nil {
		t.Fatal(err)
	}
	code2, err := gojq.Compile(
		q,
		gojq.WithModuleLoader(loader),
		gojq.WithDependencyManifest(&lock),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code2.Run(nil)
	v, _ := iter.Next()
	if v != int(42) {
		t.Fatalf("unexpected result: %#v", v)
	}
	_ = code
}

func TestDependencyManifestLocksInitModule(t *testing.T) {
	dir := writeDependencyTestFiles(t)
	src := `initf`
	loader := gojq.NewModuleLoader([]string{filepath.Join(dir, ".jq"), dir})

	manifest := gojq.NewDependencyManifest()
	q, _ := gojq.Parse(src)
	code, err := gojq.Compile(
		q,
		gojq.WithModuleLoader(loader),
		gojq.WithDependencyManifest(manifest),
	)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := code.Run(nil).Next(); v != int(7) {
		t.Fatalf("unexpected init module result: %#v", v)
	}
	deps := manifest.Dependencies()
	if len(deps) != 1 || deps[0].Kind != gojq.DependencyInitModule {
		t.Fatalf("expected only the init module, got %+v", deps)
	}
	locked, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(dir, ".jq"), []byte(`def initf: 8;`), 0o644); err != nil {
		t.Fatal(err)
	}
	var lock gojq.DependencyManifest
	if err := json.Unmarshal(locked, &lock); err != nil {
		t.Fatal(err)
	}
	q2, _ := gojq.Parse(src)
	_, err = gojq.Compile(
		q2,
		gojq.WithModuleLoader(
			gojq.NewModuleLoader([]string{filepath.Join(dir, ".jq"), dir})),
		gojq.WithDependencyManifest(&lock),
	)
	if err == nil || !strings.Contains(err.Error(), "content changed") {
		t.Fatalf("expected init module content changed error, got: %v", err)
	}
}

type initEvidenceLessLoader struct{}

func (*initEvidenceLessLoader) LoadInitModules() ([]*gojq.Query, error) {
	q, _ := gojq.Parse(`def f: 1;`)
	return []*gojq.Query{q}, nil
}

func TestDependencyManifestInitRequiresEvidence(t *testing.T) {
	q, err := gojq.Parse(`f`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(
		q,
		gojq.WithModuleLoader(&initEvidenceLessLoader{}),
		gojq.WithDependencyManifest(gojq.NewDependencyManifest()),
	)
	if err == nil {
		t.Fatal("expected init evidence error")
	}
	if !strings.Contains(err.Error(), "cannot lock initialization modules") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Plain Compile keeps using the loader without evidence requirements.
	code, err := gojq.Compile(
		q,
		gojq.WithModuleLoader(&initEvidenceLessLoader{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := code.Run(nil).Next(); v != int(1) {
		t.Fatalf("unexpected result: %#v", v)
	}
}

func TestDependencyManifestIncludeAndDedup(t *testing.T) {
	dir := writeDependencyTestFiles(t)
	manifest := gojq.NewDependencyManifest()
	// The same module imported twice is recorded once, and an "include"
	// declaration is recorded just like an "import" declaration.
	compileWithManifest(t,
		`include "inner"; include "inner"; innerf`,
		dir, manifest)
	deps := manifest.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("expected init and one deduplicated module, got %+v", deps)
	}
	if deps[1].Kind != gojq.DependencyModule || deps[1].Name != "inner" {
		t.Fatalf("unexpected module dependency: %+v", deps[1])
	}
}

func TestDependencyManifestEmptyLockRejectsLoads(t *testing.T) {
	dir := writeDependencyTestFiles(t)
	var lock gojq.DependencyManifest
	if err := json.Unmarshal([]byte(`{"version":1,"dependencies":[]}`), &lock); err != nil {
		t.Fatal(err)
	}
	q, _ := gojq.Parse(`import "inner" as inner; inner::innerf`)
	_, err := gojq.Compile(
		q,
		gojq.WithModuleLoader(gojq.NewModuleLoader([]string{dir})),
		gojq.WithDependencyManifest(&lock),
	)
	if err == nil || !strings.Contains(err.Error(), "is not locked") {
		t.Fatalf("expected not locked error, got: %v", err)
	}
}

func TestDependencyManifestInvalidJSON(t *testing.T) {
	for _, cnt := range []string{
		`{"version":2,"dependencies":[]}`,
		`{"version":1,"dependencies":[{"kind":"weird","source":"x","size":0,"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`,
		`{"version":1,"dependencies":[{"kind":"module","name":"m","source":"","size":0,"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`,
		`{"version":1,"dependencies":[{"kind":"module","name":"m","source":"s","size":1,"digest":"md5:00"}]}`,
	} {
		var lock gojq.DependencyManifest
		if err := json.Unmarshal([]byte(cnt), &lock); err == nil {
			t.Fatalf("expected unmarshal error for %s", cnt)
		}
	}
}
