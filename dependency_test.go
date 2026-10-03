package gojq_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/itchyny/gojq"
)

func writeDependencyFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

const (
	initModuleContent = `def init_func: 1;`
	moduleAContent    = `import "b" as b; def f: b::g;`
	moduleBContent    = `def g: 2;`
	jsonDataContent   = `{"x": 1}`
)

func setupDependencyModules(t *testing.T) (string, *gojq.Query) {
	t.Helper()
	dir := t.TempDir()
	writeDependencyFile(t, filepath.Join(dir, ".jq"), initModuleContent)
	writeDependencyFile(t, filepath.Join(dir, "a.jq"), moduleAContent)
	writeDependencyFile(t, filepath.Join(dir, "b.jq"), moduleBContent)
	writeDependencyFile(t, filepath.Join(dir, "data.json"), jsonDataContent)
	query, err := gojq.Parse(`import "a" as a; import "data" as $d; a::f`)
	if err != nil {
		t.Fatal(err)
	}
	return dir, query
}

func dependencyModuleLoader(dir string) gojq.ModuleLoader {
	return gojq.NewModuleLoader([]string{filepath.Join(dir, ".jq"), dir})
}

func TestDependencyRecorder(t *testing.T) {
	dir, query := setupDependencyModules(t)
	recorder := &gojq.DependencyRecorder{}
	if _, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(dependencyModuleLoader(dir)),
		gojq.WithDependencyRecorder(recorder),
	); err != nil {
		t.Fatal(err)
	}
	want := []gojq.Dependency{
		{
			Kind:   gojq.DependencyKindModule,
			Name:   filepath.Join(dir, ".jq"),
			Source: filepath.Join(dir, ".jq"),
			Digest: contentDigest(initModuleContent),
		},
		{
			Kind:   gojq.DependencyKindModule,
			Name:   "a",
			Source: filepath.Join(dir, "a.jq"),
			Digest: contentDigest(moduleAContent),
		},
		{
			Kind:   gojq.DependencyKindModule,
			Name:   "b",
			Source: filepath.Join(dir, "b.jq"),
			Digest: contentDigest(moduleBContent),
		},
		{
			Kind:   gojq.DependencyKindJSON,
			Name:   "data",
			Source: filepath.Join(dir, "data.json"),
			Digest: contentDigest(jsonDataContent),
		},
	}
	if diff := cmp.Diff(want, recorder.Manifest().Dependencies); diff != "" {
		t.Fatalf("manifest mismatch (-want +got):\n%s", diff)
	}
}

func recordDependencies(t *testing.T, dir string, query *gojq.Query) *gojq.DependencyManifest {
	t.Helper()
	recorder := &gojq.DependencyRecorder{}
	if _, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(dependencyModuleLoader(dir)),
		gojq.WithDependencyRecorder(recorder),
	); err != nil {
		t.Fatal(err)
	}
	return recorder.Manifest()
}

func TestDependencyLock(t *testing.T) {
	dir, query := setupDependencyModules(t)
	manifest := recordDependencies(t, dir, query)
	// The manifest is marshaled to JSON and used as the lock input of the
	// next compilation.
	cnt, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var locked gojq.DependencyManifest
	if err := json.Unmarshal(cnt, &locked); err != nil {
		t.Fatal(err)
	}
	if _, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(dependencyModuleLoader(dir)),
		gojq.WithDependencyLock(&locked),
	); err != nil {
		t.Fatal(err)
	}
}

func TestDependencyLockContentChanged(t *testing.T) {
	dir, query := setupDependencyModules(t)
	manifest := recordDependencies(t, dir, query)
	// The module name stays the same but its content changes.
	writeDependencyFile(t, filepath.Join(dir, "b.jq"), `def g: 3;`)
	_, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(dependencyModuleLoader(dir)),
		gojq.WithDependencyLock(manifest),
	)
	var lockErr *gojq.DependencyLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *gojq.DependencyLockError but got: %v", err)
	}
	if lockErr.Kind != gojq.DependencyKindModule || lockErr.Name != "b" {
		t.Fatalf("unexpected locked dependency: %v", lockErr)
	}
	if !strings.Contains(lockErr.Error(), "content digest changed") {
		t.Fatalf("unexpected error message: %v", lockErr)
	}
}

func TestDependencyLockSourceChanged(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeDependencyFile(t, filepath.Join(dir1, "m.jq"), `def f: 1;`)
	writeDependencyFile(t, filepath.Join(dir2, "m.jq"), `def f: 1;`)
	query, err := gojq.Parse(`import "m" as m; m::f`)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &gojq.DependencyRecorder{}
	if _, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(gojq.NewModuleLoader([]string{dir1})),
		gojq.WithDependencyRecorder(recorder),
	); err != nil {
		t.Fatal(err)
	}
	// The same module name and content, but resolved from another source.
	_, err = gojq.Compile(
		query,
		gojq.WithModuleLoader(gojq.NewModuleLoader([]string{dir2})),
		gojq.WithDependencyLock(recorder.Manifest()),
	)
	var lockErr *gojq.DependencyLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *gojq.DependencyLockError but got: %v", err)
	}
	if !strings.Contains(lockErr.Error(), "source changed") {
		t.Fatalf("unexpected error message: %v", lockErr)
	}
}

func TestDependencyLockNotInManifest(t *testing.T) {
	dir, query := setupDependencyModules(t)
	_, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(dependencyModuleLoader(dir)),
		gojq.WithDependencyLock(&gojq.DependencyManifest{}),
	)
	var lockErr *gojq.DependencyLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *gojq.DependencyLockError but got: %v", err)
	}
	if !strings.Contains(lockErr.Error(), "not found in the dependency lock manifest") {
		t.Fatalf("unexpected error message: %v", lockErr)
	}
}

type noEvidenceModuleLoader struct{}

func (noEvidenceModuleLoader) LoadModule(name string) (*gojq.Query, error) {
	return gojq.Parse(`def f: 1;`)
}

func TestDependencyEvidenceRequired(t *testing.T) {
	query, err := gojq.Parse(`import "m" as m; m::f`)
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range [][]gojq.CompilerOption{
		{gojq.WithDependencyRecorder(&gojq.DependencyRecorder{})},
		{gojq.WithDependencyLock(&gojq.DependencyManifest{})},
	} {
		_, err := gojq.Compile(query,
			append([]gojq.CompilerOption{gojq.WithModuleLoader(noEvidenceModuleLoader{})}, options...)...)
		var evidenceErr *gojq.DependencyEvidenceError
		if !errors.As(err, &evidenceErr) {
			t.Fatalf("expected *gojq.DependencyEvidenceError but got: %v", err)
		}
		if !strings.Contains(evidenceErr.Error(), "cannot lock") {
			t.Fatalf("unexpected error message: %v", evidenceErr)
		}
	}
}

type noEvidenceInitModuleLoader struct{}

func (noEvidenceInitModuleLoader) LoadInitModules() ([]*gojq.Query, error) {
	query, err := gojq.Parse(`def f: 1;`)
	if err != nil {
		return nil, err
	}
	return []*gojq.Query{query}, nil
}

func TestDependencyEvidenceRequiredForInitModules(t *testing.T) {
	query, err := gojq.Parse(`f`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(
		query,
		gojq.WithModuleLoader(noEvidenceInitModuleLoader{}),
		gojq.WithDependencyRecorder(&gojq.DependencyRecorder{}),
	)
	var evidenceErr *gojq.DependencyEvidenceError
	if !errors.As(err, &evidenceErr) {
		t.Fatalf("expected *gojq.DependencyEvidenceError but got: %v", err)
	}
	if !strings.Contains(evidenceErr.Error(), "cannot lock init modules") {
		t.Fatalf("unexpected error message: %v", evidenceErr)
	}
}

type evidenceModuleLoader struct {
	digest string
}

func (*evidenceModuleLoader) LoadModule(name string) (*gojq.Query, error) {
	return gojq.Parse(`def f: 1;`)
}

func (l *evidenceModuleLoader) LoadDependencyEvidence(kind gojq.DependencyKind, name string, meta map[string]any) (*gojq.DependencyEvidence, error) {
	return &gojq.DependencyEvidence{
		Name:   name,
		Source: "mem:///" + name,
		Digest: l.digest,
	}, nil
}

func TestDependencyEvidenceCustomLoader(t *testing.T) {
	query, err := gojq.Parse(`import "m" as m; m::f`)
	if err != nil {
		t.Fatal(err)
	}
	loader := &evidenceModuleLoader{digest: "sha256:aaa"}
	recorder := &gojq.DependencyRecorder{}
	if _, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(loader),
		gojq.WithDependencyRecorder(recorder),
	); err != nil {
		t.Fatal(err)
	}
	want := []gojq.Dependency{
		{Kind: gojq.DependencyKindModule, Name: "m", Source: "mem:///m", Digest: "sha256:aaa"},
	}
	if diff := cmp.Diff(want, recorder.Manifest().Dependencies); diff != "" {
		t.Fatalf("manifest mismatch (-want +got):\n%s", diff)
	}
	if _, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(loader),
		gojq.WithDependencyLock(recorder.Manifest()),
	); err != nil {
		t.Fatal(err)
	}
	// The module name stays the same but the reported content changes.
	loader.digest = "sha256:bbb"
	_, err = gojq.Compile(
		query,
		gojq.WithModuleLoader(loader),
		gojq.WithDependencyLock(recorder.Manifest()),
	)
	var lockErr *gojq.DependencyLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *gojq.DependencyLockError but got: %v", err)
	}
	if !strings.Contains(lockErr.Error(), "content digest changed") {
		t.Fatalf("unexpected error message: %v", lockErr)
	}
}

type spyingModuleLoader struct {
	evidenceCalled bool
}

func (*spyingModuleLoader) LoadModule(name string) (*gojq.Query, error) {
	return gojq.Parse(`def f: 1;`)
}

func (l *spyingModuleLoader) LoadDependencyEvidence(kind gojq.DependencyKind, name string, meta map[string]any) (*gojq.DependencyEvidence, error) {
	l.evidenceCalled = true
	return nil, fmt.Errorf("evidence must not be requested without recording or locking")
}

func TestCompileWithoutDependencyRecording(t *testing.T) {
	query, err := gojq.Parse(`import "m" as m; m::f`)
	if err != nil {
		t.Fatal(err)
	}
	loader := &spyingModuleLoader{}
	code, err := gojq.Compile(query, gojq.WithModuleLoader(loader))
	if err != nil {
		t.Fatal(err)
	}
	if loader.evidenceCalled {
		t.Fatal("evidence was requested without recording or locking")
	}
	iter := code.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("expected a value")
	}
	if err, ok := v.(error); ok {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("expected 1 but got: %v", v)
	}
}
