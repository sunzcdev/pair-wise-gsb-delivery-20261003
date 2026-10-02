package lua

import (
	"testing"
)

func expectReadonlyPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%v expected a panic, but none occurred", positionString(1))
		}
	}()
	f()
}

func TestFreezeTableLuaWritesRejected(t *testing.T) {
	L := NewState()
	defer L.Close()

	tbl := L.NewTable()
	tbl.RawSetString("name", LString("frozen"))
	tbl.RawSetInt(1, LNumber(10))
	tbl.RawSetInt(2, LNumber(20))
	L.SetGlobal("t", tbl)

	L.FreezeTable(tbl)
	errorIfFalse(t, tbl.Readonly(), "table should be readonly")

	// Lua assignment to existing and new keys
	errorIfScriptNotFail(t, L, `t.name = "changed"`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `t.newkey = 1`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `t[1] = 99`, "attempt to update a readonly table")
	// rawset
	errorIfScriptNotFail(t, L, `rawset(t, "name", "changed")`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `rawset(t, 3, 30)`, "attempt to update a readonly table")
	// table.insert / remove / sort
	errorIfScriptNotFail(t, L, `table.insert(t, 30)`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `table.insert(t, 1, 30)`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `table.remove(t)`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `table.remove(t, 1)`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `table.sort(t)`, "attempt to update a readonly table")

	// contents are untouched
	errorIfScriptFail(t, L, `
		assert(t.name == "frozen")
		assert(t[1] == 10 and t[2] == 20)
		assert(#t == 2)
		assert(rawget(t, "name") == "frozen")
		local keys = 0
		for k, v in pairs(t) do keys = keys + 1 end
		assert(keys == 3)
		assert(next(t, nil) ~= nil)
	`)
}

func TestFreezeTableGoWritesRejected(t *testing.T) {
	L := NewState()
	defer L.Close()

	tbl := L.NewTable()
	tbl.RawSetString("name", LString("frozen"))
	tbl.RawSetInt(1, LNumber(10))
	L.FreezeTable(tbl)

	// LState-level Go API raises a Lua error
	errorIfGFuncNotFail(t, L, func(ls *LState) int {
		ls.RawSet(tbl, LString("name"), LString("changed"))
		return 0
	}, "attempt to update a readonly table")
	errorIfGFuncNotFail(t, L, func(ls *LState) int {
		ls.RawSetInt(tbl, 2, LNumber(20))
		return 0
	}, "attempt to update a readonly table")
	errorIfGFuncNotFail(t, L, func(ls *LState) int {
		ls.SetField(tbl, "name", LString("changed"))
		return 0
	}, "attempt to update a readonly table")
	errorIfGFuncNotFail(t, L, func(ls *LState) int {
		ls.SetTable(tbl, LString("name"), LString("changed"))
		return 0
	}, "attempt to update a readonly table")

	// direct LTable mutations panic and leave the table untouched
	expectReadonlyPanic(t, func() { tbl.RawSet(LString("name"), LString("changed")) })
	expectReadonlyPanic(t, func() { tbl.RawSetString("name", LString("changed")) })
	expectReadonlyPanic(t, func() { tbl.RawSetInt(2, LNumber(20)) })
	expectReadonlyPanic(t, func() { tbl.RawSetH(LString("name"), LString("changed")) })
	expectReadonlyPanic(t, func() { tbl.Append(LNumber(30)) })
	expectReadonlyPanic(t, func() { tbl.Insert(1, LNumber(30)) })
	expectReadonlyPanic(t, func() { tbl.Remove(1) })

	errorIfNotEqual(t, LString("frozen"), tbl.RawGetString("name"))
	errorIfNotEqual(t, LNumber(10), tbl.RawGetInt(1))
	errorIfNotEqual(t, 1, tbl.Len())
}

func TestFreezeTableShallowKeepsNestedWritable(t *testing.T) {
	L := NewState()
	defer L.Close()

	tbl := L.NewTable()
	sub := L.NewTable()
	sub.RawSetString("x", LNumber(1))
	tbl.RawSetString("sub", sub)
	L.SetGlobal("t", tbl)

	L.FreezeTable(tbl)
	errorIfFalse(t, !sub.Readonly(), "nested table should not be frozen by a shallow freeze")
	errorIfScriptFail(t, L, `t.sub.x = 2`)
	errorIfScriptNotFail(t, L, `t.sub = {}`, "attempt to update a readonly table")
}

func TestFreezeTableRecursive(t *testing.T) {
	L := NewState()
	defer L.Close()

	tbl := L.NewTable()
	sub := L.NewTable()
	sub.RawSetString("x", LNumber(1))
	shared := L.NewTable()
	shared.RawSetString("y", LNumber(2))
	tbl.RawSetString("sub", sub)
	tbl.RawSetString("a", shared)
	tbl.RawSetString("b", shared)
	tbl.RawSetString("self", tbl)                                            // cycle
	tbl.RawSetString("fn", L.NewFunction(func(ls *LState) int { return 0 })) // non-table leaf
	L.SetGlobal("t", tbl)

	err := L.FreezeTableRecursive(tbl)
	errorIfNotNil(t, err)
	errorIfFalse(t, tbl.Readonly(), "root should be frozen")
	errorIfFalse(t, sub.Readonly(), "nested table should be frozen")
	errorIfFalse(t, shared.Readonly(), "shared table should be frozen")

	errorIfScriptNotFail(t, L, `t.sub.x = 2`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `t.a.y = 3`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `t.b.y = 3`, "attempt to update a readonly table")
	errorIfScriptNotFail(t, L, `t.self.self.sub.x = 2`, "attempt to update a readonly table")
	errorIfScriptFail(t, L, `
		assert(t.sub.x == 1)
		assert(t.a.y == 2 and t.b.y == 2)
		assert(t.self == t)
		assert(type(t.fn) == "function")
	`)
}

func TestFreezeTableRecursiveMetatableRejected(t *testing.T) {
	L := NewState()
	defer L.Close()

	tbl := L.NewTable()
	sub := L.NewTable()
	deep := L.NewTable()
	sub.RawSetString("deep", deep)
	tbl.RawSetString("sub", sub)
	L.SetMetatable(deep, L.NewTable())

	err := L.FreezeTableRecursive(tbl)
	errorIfNil(t, err)
	// failure must not leave a half-frozen graph
	errorIfFalse(t, !tbl.Readonly(), "root must not be frozen after a failed recursive freeze")
	errorIfFalse(t, !sub.Readonly(), "subtable must not be frozen after a failed recursive freeze")
	errorIfFalse(t, !deep.Readonly(), "deep table must not be frozen after a failed recursive freeze")

	// the graph is still fully writable
	tbl.RawSetString("x", LNumber(1))
	sub.RawSetString("y", LNumber(2))
	errorIfNotEqual(t, LNumber(1), tbl.RawGetString("x"))
	errorIfNotEqual(t, LNumber(2), sub.RawGetString("y"))
}

func TestFreezeTableNewindexStillDelegated(t *testing.T) {
	L := NewState()
	defer L.Close()

	tbl := L.NewTable()
	backing := L.NewTable()
	mt := L.NewTable()
	mt.RawSetString("__newindex", backing)
	L.SetMetatable(tbl, mt)
	L.SetGlobal("t", tbl)
	L.SetGlobal("backing", backing)

	L.FreezeTable(tbl)
	// __newindex is honored for absent keys; the frozen table itself is untouched
	errorIfScriptFail(t, L, `t.newkey = 42`)
	errorIfScriptFail(t, L, `assert(rawget(backing, "newkey") == 42)`)
	errorIfScriptFail(t, L, `assert(rawget(t, "newkey") == nil)`)
}
