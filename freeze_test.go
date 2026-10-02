package lua

import (
	"errors"
	"testing"
)

func recoverFrozenPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		rcv := recover()
		if rcv == nil {
			t.Errorf("%v mutating a frozen table should panic", positionString(1))
			return
		}
		if err, ok := rcv.(error); !ok || !errors.Is(err, ErrTableFrozen) {
			t.Errorf("%v expected ErrTableFrozen, got %v", positionString(1), rcv)
		}
	}()
	fn()
}

func TestTableFreezeShallow(t *testing.T) {
	tb := newLTable(0, 0)
	tb.RawSetInt(1, LNumber(1))
	tb.RawSetString("a", LString("x"))

	errorIfFalse(t, !tb.IsFrozen(), "fresh table should not be frozen")
	tb.Freeze()
	errorIfFalse(t, tb.IsFrozen(), "table should be frozen")
	tb.Freeze() // idempotent
	errorIfFalse(t, tb.IsFrozen(), "table should stay frozen")

	// reads and traversal keep working
	errorIfNotEqual(t, LNumber(1), tb.RawGetInt(1))
	errorIfNotEqual(t, LString("x"), tb.RawGetString("a"))
	errorIfNotEqual(t, LNil, tb.RawGetString("missing"))
	errorIfNotEqual(t, 1, tb.Len())
	count := 0
	tb.ForEach(func(LValue, LValue) { count++ })
	errorIfNotEqual(t, 2, count)

	// every direct Go mutator refuses without changing contents
	recoverFrozenPanic(t, func() { tb.RawSetInt(1, LNumber(2)) })
	recoverFrozenPanic(t, func() { tb.RawSetInt(2, LNumber(2)) })
	recoverFrozenPanic(t, func() { tb.RawSetString("a", LString("y")) })
	recoverFrozenPanic(t, func() { tb.RawSetString("b", LString("y")) })
	recoverFrozenPanic(t, func() { tb.RawSet(LNumber(1), LNumber(2)) })
	recoverFrozenPanic(t, func() { tb.RawSetH(LString("a"), LString("y")) })
	recoverFrozenPanic(t, func() { tb.Append(LNumber(9)) })
	recoverFrozenPanic(t, func() { tb.Insert(1, LNumber(9)) })
	recoverFrozenPanic(t, func() { tb.Remove(1) })

	errorIfNotEqual(t, LNumber(1), tb.RawGetInt(1))
	errorIfNotEqual(t, LString("x"), tb.RawGetString("a"))
	errorIfNotEqual(t, LNil, tb.RawGetString("b"))
	errorIfNotEqual(t, 1, tb.Len())

	// shallow freeze does not touch nested tables
	inner := newLTable(0, 0)
	inner.RawSetString("k", LNumber(1))
	parent := newLTable(0, 0)
	parent.RawSetString("child", inner)
	parent.Freeze()
	errorIfFalse(t, parent.IsFrozen(), "parent should be frozen")
	errorIfFalse(t, !inner.IsFrozen(), "nested table must stay mutable after shallow freeze")
	inner.RawSetString("k", LNumber(2))
	errorIfNotEqual(t, LNumber(2), inner.RawGetString("k"))
}

func TestTableFreezeLuaWrites(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()

	tb := L.NewTable()
	tb.RawSetInt(1, LString("a"))
	tb.RawSetInt(2, LString("b"))
	tb.RawSetString("k", LString("v"))
	tb.Freeze()
	L.SetGlobal("t", tb)

	// assignment to existing keys, new keys, numeric and hash parts
	errorIfScriptNotFail(t, L, `t[1] = "z"`, "read-only")
	errorIfScriptNotFail(t, L, `t[3] = "z"`, "read-only")
	errorIfScriptNotFail(t, L, `t.k = "z"`, "read-only")
	errorIfScriptNotFail(t, L, `t.new = "z"`, "read-only")

	// rawset, table.insert and table.remove
	errorIfScriptNotFail(t, L, `rawset(t, "k2", "z")`, "read-only")
	errorIfScriptNotFail(t, L, `rawset(t, 3, "z")`, "read-only")
	errorIfScriptNotFail(t, L, `table.insert(t, "z")`, "read-only")
	errorIfScriptNotFail(t, L, `table.insert(t, 1, "z")`, "read-only")
	errorIfScriptNotFail(t, L, `table.remove(t)`, "read-only")
	errorIfScriptNotFail(t, L, `table.remove(t, 1)`, "read-only")

	// sort mutates the array part
	errorIfScriptNotFail(t, L, `table.sort(t)`, "read-only")

	// replacing the metatable is a write as well
	errorIfScriptNotFail(t, L, `setmetatable(t, {})`, "read-only")

	// Go-side LState write APIs also refuse
	errorIfGFuncNotFail(t, L, func(*LState) int {
		L.RawSet(tb, LString("k"), LString("z"))
		return 0
	}, "read-only")
	errorIfGFuncNotFail(t, L, func(*LState) int {
		L.RawSetInt(tb, 9, LString("z"))
		return 0
	}, "read-only")
	errorIfGFuncNotFail(t, L, func(*LState) int {
		L.SetMetatable(tb, L.NewTable())
		return 0
	}, "read-only")

	// contents unchanged after every rejected write
	errorIfScriptFail(t, L, `
		assert(t[1] == "a")
		assert(t[2] == "b")
		assert(t[3] == nil)
		assert(t.k == "v")
		assert(t.k2 == nil)
		assert(t.new == nil)
		assert(#t == 2)
		local seen = {}
		for k, v in pairs(t) do seen[k] = v end
		assert(seen[1] == "a" and seen[2] == "b" and seen.k == "v")
		local n = 0
		for i, v in ipairs(t) do n = i end
		assert(n == 2)
	`)
}

func TestTableFreezeNewindexNotBypassed(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()

	tb := L.NewTable()
	tb.RawSetString("present", LNumber(1))
	mt := L.NewTable()
	called := false
	mt.RawSetString("__newindex", L.NewFunction(func(*LState) int {
		called = true
		return 0
	}))
	L.SetMetatable(tb, mt)
	tb.Freeze()

	// writing an absent key must still fail; __newindex is not a way around it
	L.SetGlobal("t", tb)
	errorIfScriptNotFail(t, L, `t.absent = 2`, "read-only")
	errorIfFalse(t, !called, "__newindex must not run when writing to a frozen table")

	// and the existing-key path rejects too
	errorIfScriptNotFail(t, L, `t.present = 2`, "read-only")
	errorIfNotEqual(t, LNumber(1), tb.RawGetString("present"))
}

func TestTableFreezeRecursively(t *testing.T) {
	// graph:
	//   a.b -> c, a.self -> a (cycle), a.shared -> s, a.alsoShared -> s
	//   c.back -> a (cycle), s contains array and hash parts
	a := newLTable(0, 0)
	c := newLTable(0, 0)
	s := newLTable(0, 0)
	a.RawSetString("b", c)
	a.RawSetString("self", a)
	a.RawSetString("shared", s)
	c.RawSetString("back", a)
	c.RawSetString("shared", s)
	s.Append(LNumber(1))
	s.RawSetString("h", LString("hv"))

	if err := a.FreezeRecursively(); err != nil {
		t.Fatalf("freeze with cycles and sharing failed: %v", err)
	}
	for name, tb := range map[string]*LTable{"a": a, "c": c, "s": s} {
		errorIfFalse(t, tb.IsFrozen(), "table %s should be frozen", name)
	}

	// rerunning over an already frozen graph is a no-op success
	if err := a.FreezeRecursively(); err != nil {
		t.Fatalf("freeze with cycles and sharing failed: %v", err)
	}

	// contents survived
	errorIfNotEqual(t, LNumber(1), s.RawGetInt(1))
	errorIfNotEqual(t, LString("hv"), s.RawGetString("h"))

	// nested table rejects every write path now
	recoverFrozenPanic(t, func() { s.Append(LNumber(2)) })
	recoverFrozenPanic(t, func() { c.RawSetString("x", LTrue) })
	errorIfNotEqual(t, LNil, c.RawGetString("x"))
}

func TestTableFreezeRecursivelyTableKeysAndValues(t *testing.T) {
	keyTable := newLTable(0, 0)
	keyTable.RawSetInt(1, LString("k"))
	valueTable := newLTable(0, 0)
	mt := newLTable(0, 0)
	root := newLTable(0, 0)
	root.RawSetH(keyTable, valueTable)
	root.Metatable = mt

	if err := root.FreezeRecursively(); err != nil {
		t.Fatalf("freeze failed: %v", err)
	}
	errorIfFalse(t, keyTable.IsFrozen(), "table used as key must be frozen")
	errorIfFalse(t, valueTable.IsFrozen(), "table used as value must be frozen")
	errorIfFalse(t, mt.IsFrozen(), "metatable must be frozen")
}

func TestTableFreezeRecursivelyLeafTypes(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()

	// functions and channels are opaque leaves and must not stop the freeze
	ch := LChannel(make(chan LValue, 1))
	root := newLTable(0, 0)
	root.RawSetString("fn", L.NewFunction(func(*LState) int { return 0 }))
	root.RawSetString("ch", ch)
	root.RawSetString("n", LNumber(1))
	root.RawSetString("s", LString("x"))
	root.RawSetString("b", LTrue)
	if err := root.FreezeRecursively(); err != nil {
		t.Fatalf("freeze failed: %v", err)
	}
	errorIfFalse(t, root.IsFrozen(), "root should be frozen")

	// userdata and threads are unsupported and abort the whole operation
	ud := L.NewUserData()
	thread, _ := L.NewThread()
	for name, bad := range map[string]LValue{"userdata": ud, "thread": thread} {
		root := newLTable(0, 0)
		child := newLTable(0, 0)
		root.RawSetString("child", child)
		child.RawSetString("bad", bad)
		err := root.FreezeRecursively()
		var target *ErrUnfreezableValue
		errorIfFalse(t, errors.As(err, &target), "%s: expected ErrUnfreezableValue, got %v", name, err)
		errorIfFalse(t, !root.IsFrozen(), "%s: root must not be partially frozen", name)
		errorIfFalse(t, !child.IsFrozen(), "%s: child must not be partially frozen", name)
	}

	// unsupported value as a key aborts too
	keyRoot := newLTable(0, 0)
	keyRoot.RawSetH(L.NewUserData(), LNumber(1))
	err := keyRoot.FreezeRecursively()
	var target *ErrUnfreezableValue
	errorIfFalse(t, errors.As(err, &target), "userdata key should abort freeze, got %v", err)
	errorIfFalse(t, !keyRoot.IsFrozen(), "root must not be frozen after abort")

	// nested failure freezes nothing, including siblings visited earlier
	left := newLTable(0, 0)
	right := newLTable(0, 0)
	nestedRoot := newLTable(0, 0)
	nestedRoot.RawSetString("left", left)
	nestedRoot.RawSetString("right", right)
	right.RawSetString("ud", L.NewUserData())
	err = nestedRoot.FreezeRecursively()
	errorIfFalse(t, err != nil, "nested unsupported value must fail")
	errorIfFalse(t, !nestedRoot.IsFrozen() && !left.IsFrozen() && !right.IsFrozen(),
		"no table may be frozen after an aborted operation")
	left.RawSetString("still", LString("mutable"))
	errorIfNotEqual(t, LString("mutable"), left.RawGetString("still"))
}

func TestTableFreezeLuaTraversalWorks(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	L2 := NewState(Options{})
	defer L2.Close()

	inner := L2.NewTable()
	inner.RawSetString("x", LNumber(42))
	outer := L.NewTable()
	outer.RawSetString("inner", inner)
	outer.RawSetInt(1, LString("one"))
	if err := outer.FreezeRecursively(); err != nil {
		t.Fatalf("freeze failed: %v", err)
	}
	L.SetGlobal("o", outer)

	errorIfScriptFail(t, L, `
		assert(o[1] == "one")
		assert(o.inner.x == 42)
		local n = 0
		for k, v in pairs(o) do n = n + 1 end
		assert(n == 2, "pairs must still iterate a frozen table")
		assert(#o == 1)
		assert(select("#", next(o)) >= 1)
	`)
}
