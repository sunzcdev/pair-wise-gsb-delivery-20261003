package gojq_test

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/itchyny/gojq"
)

func compileFSQuery(t *testing.T, fsys fstest.MapFS, paths []string, src string) (*gojq.Code, error) {
	t.Helper()
	query, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	return gojq.Compile(
		query,
		gojq.WithModuleLoader(gojq.NewModuleFSLoader(fsys, paths)),
	)
}

func runFSQuery(t *testing.T, fsys fstest.MapFS, paths []string, src string, input any) []any {
	t.Helper()
	code, err := compileFSQuery(t, fsys, paths, src)
	if err != nil {
		t.Fatal(err)
	}
	var values []any
	iter := code.Run(input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			t.Fatal(err)
		}
		values = append(values, v)
	}
	return values
}

func TestModuleFSLoader(t *testing.T) {
	fsys := fstest.MapFS{
		"lib/greet.jq":  {Data: []byte(`def greet: "hello, " + . + "!";`)},
		"lib/data.json": {Data: []byte("{\"a\": 1}\n[2, 3]")},
	}
	values := runFSQuery(t, fsys, []string{"lib"},
		`import "greet" as g; import "data" as $d; . | g::greet, ($d[0].a + $d[1][0])`,
		"world",
	)
	if expected := []any{"hello, world!", 3}; !reflect.DeepEqual(values, expected) {
		t.Errorf("expected: %v, got: %v", expected, values)
	}
}

func TestModuleFSLoader_DefaultRootPath(t *testing.T) {
	fsys := fstest.MapFS{
		"mod.jq":         {Data: []byte(`def f: 1;`)},
		"lib/pkg.jq":     {Data: []byte(`def f: 2;`)},
		"lib/dir/dir.jq": {Data: []byte(`def f: 3;`)},
	}
	values := runFSQuery(t, fsys, nil, `import "mod" as m; m::f`, nil)
	if expected := []any{1}; !reflect.DeepEqual(values, expected) {
		t.Errorf("expected: %v, got: %v", expected, values)
	}
	values = runFSQuery(t, fsys, []string{"lib"}, `import "pkg" as p; p::f`, nil)
	if expected := []any{2}; !reflect.DeepEqual(values, expected) {
		t.Errorf("expected: %v, got: %v", expected, values)
	}
	values = runFSQuery(t, fsys, []string{"lib"}, `import "dir" as d; d::f`, nil)
	if expected := []any{3}; !reflect.DeepEqual(values, expected) {
		t.Errorf("expected: %v, got: %v", expected, values)
	}
}

func TestModuleFSLoader_SameNameModules(t *testing.T) {
	fsys := fstest.MapFS{
		"lib1/dup.jq": {Data: []byte(`def value: 1;`)},
		"lib2/dup.jq": {Data: []byte(`def value: 2;`)},
	}
	values := runFSQuery(t, fsys, []string{"lib1", "lib2"}, `import "dup" as d; d::value`, nil)
	if expected := []any{1}; !reflect.DeepEqual(values, expected) {
		t.Errorf("expected: %v, got: %v", expected, values)
	}
	values = runFSQuery(t, fsys, []string{"lib1", "lib2"},
		`import "dup" as d {search: "lib2"}; d::value`, nil)
	if expected := []any{2}; !reflect.DeepEqual(values, expected) {
		t.Errorf("expected: %v, got: %v", expected, values)
	}
}

func TestModuleFSLoader_NestedImportSourceDir(t *testing.T) {
	fsys := fstest.MapFS{
		"lib/a.jq":     {Data: []byte(`import "b" as b {search: "sub"}; def f: b::g + 1;`)},
		"lib/sub/b.jq": {Data: []byte(`import "c" as c {search: "."}; def g: c::h * 2;`)},
		"lib/sub/c.jq": {Data: []byte(`def h: 21;`)},
		"lib/c.jq":     {Data: []byte(`def h: 0;`)},
	}
	values := runFSQuery(t, fsys, []string{"lib"}, `import "a" as a; a::f`, nil)
	if expected := []any{43}; !reflect.DeepEqual(values, expected) {
		t.Errorf("expected: %v, got: %v", expected, values)
	}
}

func TestModuleFSLoader_RootEscape(t *testing.T) {
	fsys := fstest.MapFS{
		"evil.jq":   {Data: []byte(`def g: 0;`)},
		"evil.json": {Data: []byte(`0`)},
		"lib/a.jq":  {Data: []byte(`import "evil" as e {search: "../.."}; def f: e::g;`)},
	}
	for _, tc := range []struct {
		name     string
		src      string
		expected string
	}{
		{"dotdot module", `import "../evil" as e; e::g`, `invalid module name: "../evil"`},
		{"absolute module", `import "/evil" as e; e::g`, `invalid module name: "/evil"`},
		{"dotdot json", `import "../evil" as $e; $e`, `invalid module name: "../evil"`},
		{"search escape", `import "a" as a; a::f`, `module "a": module not found: "evil"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileFSQuery(t, fsys, []string{"lib"}, tc.src)
			if err == nil {
				t.Fatal("expected an error but got none")
			}
			if got := err.Error(); got != tc.expected {
				t.Errorf("expected: %v, got: %v", tc.expected, got)
			}
		})
	}
}

func TestModuleFSLoader_ImportChainError(t *testing.T) {
	fsys := fstest.MapFS{
		"lib/a.jq": {Data: []byte(`import "b" as b; def f: b::g;`)},
		"lib/b.jq": {Data: []byte(`import "c" as c; def g: c::h;`)},
	}
	_, err := compileFSQuery(t, fsys, []string{"lib"}, `import "a" as a; a::f`)
	if err == nil {
		t.Fatal("expected an error but got none")
	}
	if expected := `module "a": module "b": module not found: "c"`; err.Error() != expected {
		t.Errorf("expected: %v, got: %v", expected, err)
	}
}

func TestModuleFSLoader_ParseErrorLogicalPath(t *testing.T) {
	fsys := fstest.MapFS{
		"lib/a.jq":     {Data: []byte(`import "bad" as b; def f: b::g;`)},
		"lib/bad.jq":   {Data: []byte(`def g: ;`)},
		"lib/bad.json": {Data: []byte(`{invalid`)},
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		src      string
		contains string
	}{
		{"query", `import "a" as a; a::f`, `module "a": invalid query: lib/bad.jq:`},
		{"json", `import "bad" as $b; $b`, `invalid json: lib/bad.json:`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileFSQuery(t, fsys, []string{"lib"}, tc.src)
			if err == nil {
				t.Fatal("expected an error but got none")
			}
			if got := err.Error(); !strings.Contains(got, tc.contains) {
				t.Errorf("expected error to contain %q, got: %v", tc.contains, got)
			}
			if got := err.Error(); strings.Contains(got, wd) {
				t.Errorf("error leaks host path %q: %v", wd, got)
			}
		})
	}
}

func TestModuleFSLoader_InitModules(t *testing.T) {
	fsys := fstest.MapFS{
		".jq": {Data: []byte(`def initf: 42;`)},
	}
	values := runFSQuery(t, fsys, []string{".jq"}, `initf`, nil)
	if expected := []any{42}; !reflect.DeepEqual(values, expected) {
		t.Errorf("expected: %v, got: %v", expected, values)
	}
}
