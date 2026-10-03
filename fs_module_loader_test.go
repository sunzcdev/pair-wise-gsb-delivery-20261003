package gojq_test

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/itchyny/gojq"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		// Two modules with the same base name in different directories.
		"lib/a/a.jq": {Data: []byte("module { name: \"a\" };\ndef f: \"a:\" + (tostring);\n")},
		"lib/b/a.jq": {Data: []byte("module { name: \"a\" };\ndef f: \"b:\" + (tostring);\n")},
		// Entry module pulling in a sibling through a relative ".." import.
		"lib/app/app.jq": {Data: []byte("module { name: \"app\" };\nimport \"../a/a\" as dep;\ndef run: dep::f;\n")},
		// Nested relative reference keeps the importing module's directory.
		"lib/app/sub/sub.jq":  {Data: []byte("import \"./util\" as u;\ndef g: u::v;\n")},
		"lib/app/sub/util.jq": {Data: []byte("def v: 7;\n")},
		// JSON data under a root.
		"data/values.json": {Data: []byte("{\"answer\": 42}\n")},
		// search meta pointing inside the same FS.
		"searched/s/s.jq":   {Data: []byte("def h: \"searched\";\n")},
		"lib/withsearch.jq": {Data: []byte("import \"s/s\" as x { search: \"searched\" };\ndef k: x::h;\n")},
		// Init module at the root.
		".jq": {Data: []byte("def greet: \"hello\";\n")},
		// Module attempting to escape the root through a nested import.
		"lib/evil/evil.jq": {Data: []byte("import \"../../../etc/passwd\" as p;\ndef bad: p::x;\n")},
		// Module whose search meta tries to escape the root.
		"lib/evilsearch.jq": {Data: []byte("import \"x\" as y { search: \"../../..\" };\ndef bad: y::x;\n")},
	}
}

func compileWithFS(t *testing.T, src string, fsys fstest.MapFS, roots ...string) *gojq.Code {
	t.Helper()
	query, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(gojq.NewFSModuleLoader(fsys, roots...)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func compileWithFSMustError(t *testing.T, src string, fsys fstest.MapFS, roots ...string) error {
	t.Helper()
	query, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(
		query,
		gojq.WithModuleLoader(gojq.NewFSModuleLoader(fsys, roots...)),
	)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	return err
}

func runCode(t *testing.T, code *gojq.Code, input any) any {
	t.Helper()
	iter := code.Run(input)
	var values []any
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
	if len(values) != 1 {
		t.Fatalf("expected exactly one result, got %#v", values)
	}
	return values[0]
}

func TestFSModuleLoader_sameNameModules(t *testing.T) {
	for _, tc := range []struct {
		importPath string
		expected   string
	}{
		{"lib/a/a", "a:3"},
		{"lib/b/a", "b:3"},
	} {
		code := compileWithFS(t, `import "`+tc.importPath+`" as m; m::f`, testFS())
		if got := runCode(t, code, 3); got != tc.expected {
			t.Errorf("import %q: expected %q, got %q", tc.importPath, tc.expected, got)
		}
	}
}

func TestFSModuleLoader_relativeImportPreservesSourceDir(t *testing.T) {
	code := compileWithFS(t, `import "lib/app/app" as app; app::run`, testFS())
	if got := runCode(t, code, 3); got != "a:3" {
		t.Errorf("expected %q, got %q", "a:3", got)
	}

	code = compileWithFS(t, `import "lib/app/sub/sub" as s; s::g`, testFS())
	if got := runCode(t, code, nil); got != 7 {
		t.Errorf("expected %v, got %v", 7, got)
	}
}

func TestFSModuleLoader_rootRelativeSearch(t *testing.T) {
	code := compileWithFS(t, `import "lib/withsearch" as w; w::k`, testFS())
	if got := runCode(t, code, nil); got != "searched" {
		t.Errorf("expected %q, got %q", "searched", got)
	}
}

func TestFSModuleLoader_jsonData(t *testing.T) {
	code := compileWithFS(t, `import "data/values" as $v; $v[0].answer`, testFS())
	got := runCode(t, code, nil)
	if number, ok := got.(json.Number); !ok || number.String() != "42" {
		t.Errorf("expected json.Number 42, got %#v", got)
	}

	// JSON imported from a module resolves relative to the source directory.
	fsys := fstest.MapFS{
		"pkg/m/m.jq":      {Data: []byte("import \"../config\" as $c;\ndef answer: $c[0].n;\n")},
		"pkg/config.json": {Data: []byte("{\"n\": 5}\n")},
	}
	code = compileWithFS(t, `import "pkg/m/m" as m; m::answer`, fsys)
	got = runCode(t, code, nil)
	if number, ok := got.(json.Number); !ok || number.String() != "5" {
		t.Errorf("expected json.Number 5, got %#v", got)
	}
}

func TestFSModuleLoader_initModule(t *testing.T) {
	code := compileWithFS(t, `greet`, testFS())
	if got := runCode(t, code, nil); got != "hello" {
		t.Errorf("expected %q, got %q", "hello", got)
	}
}

func TestFSModuleLoader_rejectsRootEscape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    string
		substr string
	}{
		{"absolute", `import "/etc/passwd" as x; x::f`, "absolute module path is not allowed"},
		{"top level dotdot", `import "../secret" as x; x::f`, "escapes the file system root"},
		{"nested dotdot", `import "lib/evil/evil" as x; x::bad`, "escapes the file system root"},
		{"search dotdot", `import "lib/evilsearch" as x; x::bad`, "module not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compileWithFSMustError(t, tc.src, testFS())
			if !strings.Contains(err.Error(), tc.substr) {
				t.Errorf("expected error containing %q, got %q", tc.substr, err.Error())
			}
			if strings.Contains(err.Error(), "C:\\") {
				t.Errorf("error must not leak a host path: %q", err.Error())
			}
		})
	}
}

func TestFSModuleLoader_errorHasImportChain(t *testing.T) {
	fsys := fstest.MapFS{
		"top.jq":           {Data: []byte("import \"mid/mid\" as m;\ndef a: m::b;\n")},
		"mid/mid.jq":       {Data: []byte("import \"leaf/leaf\" as l;\ndef b: l::c;\n")},
		"mid/leaf/leaf.jq": {Data: []byte("import \"missing\" as q;\ndef c: q::d;\n")},
	}
	err := compileWithFSMustError(t, `import "top" as t; t::a`, fsys)
	msg := err.Error()
	for _, want := range []string{"top", "mid/mid", "mid/leaf/leaf", "missing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got %q", want, msg)
		}
	}
}

func TestFSModuleLoader_moduleNotFound(t *testing.T) {
	err := compileWithFSMustError(t, `import "nope/nope" as x; x::f`, testFS())
	if !strings.Contains(err.Error(), "module not found") {
		t.Fatalf("expected a module-not-found error, got %v", err)
	}
}

func TestFSModuleLoader_rejectsBackslash(t *testing.T) {
	err := compileWithFSMustError(t, `import "lib\\a\\a" as x; x::f`, testFS())
	if !strings.Contains(err.Error(), "backslashes are not allowed") {
		t.Errorf("expected a backslash error, got %v", err)
	}
}

func TestFSModuleLoader_parseErrorHasLogicalName(t *testing.T) {
	fsys := fstest.MapFS{
		"parent.jq":      {Data: []byte("import \"child/child\" as c;\ndef p: c::q;\n")},
		"child/child.jq": {Data: []byte("def q: ;\n")},
	}
	err := compileWithFSMustError(t, `import "parent" as p; p::p`, fsys)
	msg := err.Error()
	if !strings.HasPrefix(msg, "invalid query: child/child.jq:") {
		t.Errorf("expected logical file name in parse error, got %q", msg)
	}
	if strings.Contains(msg, "C:\\") {
		t.Errorf("parse error must not leak a host path: %q", msg)
	}
}

func TestFSModuleLoader_subdirLayout(t *testing.T) {
	// "<name>/<base>.jq" layout: importing "mylib" resolves mylib/mylib.jq.
	fsys := fstest.MapFS{
		"mylib/mylib.jq": {Data: []byte("def v: 11;\n")},
	}
	code := compileWithFS(t, `import "mylib" as lib; lib::v`, fsys)
	if got := runCode(t, code, nil); got != 11 {
		t.Errorf("expected 11, got %v", got)
	}
}
