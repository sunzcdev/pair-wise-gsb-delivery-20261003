package lua

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const moduleBSourceV1 = `return {version = 1}`
const moduleBSourceV2 = `return {version = 2}`
const moduleBSourceError = `error("bad b")`

func moduleASource(name string) string {
	return `
local b = require("b")
return {name = "` + name + `", version = b.version, get = function() return b.version end}
`
}

func setupModuleLoader(t *testing.T, L *LState, loaderName string, sources map[string]string) {
	t.Helper()

	if loaderName == "preload" {
		pkg := L.GetGlobal("package")
		preloadTable := L.GetField(pkg, "preload").(*LTable)
		for name := range sources {
			fn, err := L.LoadString(sources[name])
			if err != nil {
				t.Fatal(err)
			}
			preloadTable.RawSetString(name, fn)
		}
		return
	}

	dir := t.TempDir()
	for name, source := range sources {
		if err := os.WriteFile(filepath.Join(dir, name+".lua"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pkg := L.GetGlobal("package")
	L.SetField(pkg, "path", LString(filepath.Join(dir, "?.lua")))
}

func updateFileModule(t *testing.T, L *LState, name, source string) {
	t.Helper()
	pkg := L.GetGlobal("package")
	path := string(L.GetField(pkg, "path").(LString))
	dir := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(dir, name+".lua"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
}

func updateModule(t *testing.T, L *LState, loaderName, name, source string, sources map[string]string) {
	t.Helper()
	sources[name] = source
	if loaderName == "preload" {
		pkg := L.GetGlobal("package")
		preloadTable := L.GetField(pkg, "preload").(*LTable)
		fn, err := L.LoadString(source)
		if err != nil {
			t.Fatal(err)
		}
		preloadTable.RawSetString(name, fn)
		return
	}
	updateFileModule(t, L, name, source)
}

func assertGraphDependencies(t *testing.T, graph *moduleGraph, module string, dependencies ...string) {
	t.Helper()
	actual := graph.dependencies[module]
	if len(actual) != len(dependencies) {
		t.Fatalf("%s dependencies = %v, want %v", module, actual, dependencies)
	}
	for _, dependency := range dependencies {
		if _, ok := actual[dependency]; !ok {
			t.Fatalf("%s is missing dependency %s; got %v", module, dependency, actual)
		}
	}
}

func TestReloadModule(t *testing.T) {
	for _, loaderName := range []string{"preload", "file"} {
		t.Run(loaderName, func(t *testing.T) {
			L := NewState()
			defer L.Close()

			sources := map[string]string{
				"a": moduleASource("a"),
				"b": moduleBSourceV1,
				"c": moduleASource("c"),
			}
			setupModuleLoader(t, L, loaderName, sources)

			if err := L.DoString(`A = require("a"); C = require("c")`); err != nil {
				t.Fatal(err)
			}

			loaded := L.GetField(L.Get(RegistryIndex), "_LOADED").(*LTable)
			graph := L.G.moduleGraph
			assertGraphDependencies(t, graph, "a", "b")
			assertGraphDependencies(t, graph, "c", "b")

			oldB := loaded.RawGetString("b")
			oldA := loaded.RawGetString("a")
			oldC := loaded.RawGetString("c")
			if version := L.GetField(oldB, "version").(LNumber); int(version) != 1 {
				t.Fatalf("b version = %v, want 1", version)
			}

			updateModule(t, L, loaderName, "b", moduleBSourceV2, sources)
			if err := L.ReloadModule("b", false); err != nil {
				t.Fatal(err)
			}

			newB := loaded.RawGetString("b")
			if newB == oldB {
				t.Fatal("reload did not replace package.loaded.b")
			}
			if version := L.GetField(newB, "version").(LNumber); int(version) != 2 {
				t.Fatalf("reloaded b version = %v, want 2", version)
			}
			if loaded.RawGetString("a") != oldA || loaded.RawGetString("c") != oldC {
				t.Fatal("reloading b invalidated a or c")
			}
			if err := L.DoString(`assert(A.get() == 1); assert(C.get() == 1)`); err != nil {
				t.Fatal(err)
			}

			if err := L.ReloadModule("b", true); err != nil {
				t.Fatal(err)
			}
			if loaded.RawGetString("a") != LNil || loaded.RawGetString("c") != LNil {
				t.Fatal("dependents were not invalidated")
			}
			if err := L.DoString(`A = require("a"); C = require("c"); assert(A.get() == 2); assert(C.get() == 2)`); err != nil {
				t.Fatal(err)
			}
			currentA := loaded.RawGetString("a")
			currentC := loaded.RawGetString("c")
			currentB := loaded.RawGetString("b")
			if currentA == oldA || currentC == oldC {
				t.Fatal("dependents were not reloaded")
			}
			assertGraphDependencies(t, graph, "a", "b")
			assertGraphDependencies(t, graph, "c", "b")

			updateModule(t, L, loaderName, "b", moduleBSourceError, sources)
			err := L.ReloadModule("b", true)
			if err == nil || !strings.Contains(err.Error(), "bad b") {
				t.Fatalf("reload error = %v, want bad b", err)
			}
			if loaded.RawGetString("b") != currentB {
				t.Fatal("failed reload did not restore b")
			}
			if loaded.RawGetString("a") != currentA || loaded.RawGetString("c") != currentC {
				t.Fatal("failed reload did not restore invalidated dependents")
			}
			if err := L.DoString(`assert(A.get() == 2); assert(C.get() == 2)`); err != nil {
				t.Fatalf("old modules are not usable after failed reload: %v", err)
			}
			assertGraphDependencies(t, graph, "a", "b")
			assertGraphDependencies(t, graph, "c", "b")
		})
	}
}

func TestRequireCycle(t *testing.T) {
	for _, loaderName := range []string{"preload", "file"} {
		t.Run(loaderName, func(t *testing.T) {
			L := NewState()
			defer L.Close()

			sources := map[string]string{
				"x": `return require("y")`,
				"y": `return require("x")`,
			}
			setupModuleLoader(t, L, loaderName, sources)

			err := L.DoString(`require("x")`)
			if err == nil {
				t.Fatal("circular require succeeded")
			}
			if !strings.Contains(err.Error(), "x -> y -> x") {
				t.Fatalf("circular require error = %q, want chain x -> y -> x", err.Error())
			}

			loaded := L.GetField(L.Get(RegistryIndex), "_LOADED").(*LTable)
			if loaded.RawGetString("x") != LNil || loaded.RawGetString("y") != LNil {
				t.Fatal("failed circular require left modules cached")
			}
			if len(L.G.moduleGraph.dependencies) != 0 {
				t.Fatalf("failed circular require left dependencies: %v", L.G.moduleGraph.dependencies)
			}
		})
	}
}

func TestRequireCycleBrokenByLoaded(t *testing.T) {
	for _, loaderName := range []string{"preload", "file"} {
		t.Run(loaderName, func(t *testing.T) {
			L := NewState()
			defer L.Close()

			sources := map[string]string{
				"broken": `package.loaded.broken = 25; return require("broken")`,
			}
			setupModuleLoader(t, L, loaderName, sources)

			if err := L.DoString(`assert(require("broken") == 25)`); err != nil {
				t.Fatal(err)
			}
			if _, ok := L.G.moduleGraph.dependencies["broken"]; ok {
				t.Fatal("breaking a self-cycle with package.loaded recorded a self-dependency")
			}
		})
	}
}

func TestRequireMutualCycleBrokenByLoaded(t *testing.T) {
	for _, loaderName := range []string{"preload", "file"} {
		t.Run(loaderName, func(t *testing.T) {
			L := NewState()
			defer L.Close()

			sources := map[string]string{
				"x": `
package.loaded.x = {from = "x"}
local y = require("y")
return package.loaded.x
`,
				"y": `
package.loaded.y = {from = "y"}
local x = require("x")
return package.loaded.y
`,
			}
			setupModuleLoader(t, L, loaderName, sources)

			if err := L.DoString(`
local x = require("x")
local y = require("y")
assert(x.from == "x")
assert(y.from == "y")
`); err != nil {
				t.Fatal(err)
			}
			assertGraphDependencies(t, L.G.moduleGraph, "x", "y")
			assertGraphDependencies(t, L.G.moduleGraph, "y", "x")
		})
	}
}
