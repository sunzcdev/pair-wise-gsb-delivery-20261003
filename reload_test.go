package lua

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireModule(L *LState, name string) (LValue, error) {
	L.Push(L.GetGlobal("require"))
	L.Push(LString(name))
	if err := L.PCall(1, 1, nil); err != nil {
		return nil, err
	}
	module := L.Get(-1)
	L.Pop(1)
	return module, nil
}

func requireTestModule(t *testing.T, L *LState, name string) *LTable {
	t.Helper()
	module, err := requireModule(L, name)
	if err != nil {
		t.Fatalf("require %s: %v", name, err)
	}
	return module.(*LTable)
}

func requireModuleInLoader(L *LState, name string) *LTable {
	module, err := requireModule(L, name)
	if err != nil {
		if apiErr, ok := err.(*ApiError); ok {
			panic(apiErr)
		}
		L.RaiseError("%s", err.Error())
	}
	return module.(*LTable)
}

func testModuleVersion(t *testing.T, module LValue, field string, want LNumber) {
	t.Helper()
	got := module.(*LTable).RawGetString(field)
	if got != want {
		t.Fatalf("%s = %s, want %s", field, got, want)
	}
}

func TestReloadModuleDependents(t *testing.T) {
	L := NewState()
	defer L.Close()

	bVersion := LNumber(1)
	L.PreloadModule("B", func(L *LState) int {
		module := L.NewTable()
		module.RawSetString("version", bVersion)
		L.Push(module)
		return 1
	})

	preloadDependent := func(name LString) {
		L.PreloadModule(string(name), func(L *LState) int {
			b := requireModuleInLoader(L, "B")
			module := L.NewTable()
			module.RawSetString("bversion", b.RawGetString("version"))
			L.Push(module)
			return 1
		})
	}
	preloadDependent("A")
	preloadDependent("C")

	var b LValue = requireTestModule(t, L, "B")
	a := requireTestModule(t, L, "A")
	c := requireTestModule(t, L, "C")
	testModuleVersion(t, b, "version", 1)
	testModuleVersion(t, a, "bversion", 1)
	testModuleVersion(t, c, "bversion", 1)

	bVersion = 2
	b, err := L.ReloadModule("B", false)
	if err != nil {
		t.Fatal(err)
	}
	testModuleVersion(t, b, "version", 2)
	if requireTestModule(t, L, "A") != a {
		t.Fatal("A was reloaded although dependent invalidation was disabled")
	}
	if requireTestModule(t, L, "C") != c {
		t.Fatal("C was reloaded although dependent invalidation was disabled")
	}

	bVersion = 3
	b, err = L.ReloadModule("B", true)
	if err != nil {
		t.Fatal(err)
	}
	testModuleVersion(t, b, "version", 3)
	loaded := L.GetField(L.GetField(L.GetGlobal("package"), "loaded"), "A")
	if loaded != LNil {
		t.Fatal("A was not invalidated")
	}
	loaded = L.GetField(L.GetField(L.GetGlobal("package"), "loaded"), "C")
	if loaded != LNil {
		t.Fatal("C was not invalidated")
	}
	a = requireTestModule(t, L, "A")
	c = requireTestModule(t, L, "C")
	testModuleVersion(t, a, "bversion", 3)
	testModuleVersion(t, c, "bversion", 3)

	oldB := requireTestModule(t, L, "B")
	bVersion = 4
	L.PreloadModule("B", func(L *LState) int {
		L.RaiseError("B reload failed")
		return 0
	})
	_, err = L.ReloadModule("B", true)
	if err == nil || !strings.Contains(err.Error(), "B reload failed") {
		t.Fatalf("reload error = %v, want B reload failed", err)
	}
	if got := requireTestModule(t, L, "B"); got != oldB {
		t.Fatal("B cache was not restored after reload failure")
	}
	if got := requireTestModule(t, L, "A"); got != a {
		t.Fatal("A cache was not restored after reload failure")
	}
	if got := requireTestModule(t, L, "C"); got != c {
		t.Fatal("C cache was not restored after reload failure")
	}
	graph := L.G.Registry.RawGetString(loRequireGraphKey).(*LTable)
	for _, dependent := range []string{"A", "C"} {
		dependencies := graph.RawGetString(dependent).(*LTable)
		if dependencies.RawGetString("B") != LTrue {
			t.Fatalf("dependency %s -> B was not restored", dependent)
		}
	}

	bVersion = 5
	L.PreloadModule("B", func(L *LState) int {
		module := L.NewTable()
		module.RawSetString("version", bVersion)
		L.Push(module)
		return 1
	})
	b, err = L.ReloadModule("B", true)
	if err != nil {
		t.Fatal(err)
	}
	testModuleVersion(t, b, "version", 5)
	a = requireTestModule(t, L, "A")
	c = requireTestModule(t, L, "C")
	testModuleVersion(t, a, "bversion", 5)
	testModuleVersion(t, c, "bversion", 5)
}

func TestRequireCyclePreload(t *testing.T) {
	L := NewState()
	defer L.Close()

	L.PreloadModule("cycle_x", func(L *LState) int {
		requireModuleInLoader(L, "cycle_y")
		return 0
	})
	L.PreloadModule("cycle_y", func(L *LState) int {
		requireModuleInLoader(L, "cycle_x")
		return 0
	})

	err := L.DoString(`require("cycle_x")`)
	if err == nil || !strings.Contains(err.Error(), "cycle_x -> cycle_y -> cycle_x") {
		t.Fatalf("cycle error = %v, want dependency chain", err)
	}
}

func TestRequireCycleFileLoader(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "filex.lua"), []byte(`require("filey")`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "filey.lua"), []byte(`require("filex")`), 0600); err != nil {
		t.Fatal(err)
	}

	L := NewState()
	defer L.Close()
	packageModule := L.GetGlobal("package")
	L.SetField(packageModule, "path", LString(filepath.Join(dir, "?.lua")))

	err := L.DoString(`require("filex")`)
	if err == nil || !strings.Contains(err.Error(), "filex -> filey -> filex") {
		t.Fatalf("cycle error = %v, want dependency chain", err)
	}
}
