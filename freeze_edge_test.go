package lua

import "testing"

func TestTableFreezeExtraEdges(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()

	// deleting (rawset nil) is also a write
	tb := L.NewTable()
	tb.RawSetString("k", LString("v"))
	tb.Freeze()
	L.SetGlobal("t", tb)
	errorIfScriptNotFail(t, L, `rawset(t, "k", nil)`, "read-only")
	errorIfNotEqual(t, LString("v"), tb.RawGetString("k"))
	errorIfGFuncNotFail(t, L, func(*LState) int {
		L.RawSet(tb, LString("k"), LNil)
		return 0
	}, "read-only")
	errorIfNotEqual(t, LString("v"), tb.RawGetString("k"))

	// metatable chain: writes absent on proxy are redirected through
	// __newindex to a frozen table and must be rejected there
	proxy := L.NewTable()
	frozen := L.NewTable()
	frozen.RawSetString("x", LNumber(1))
	mt := L.NewTable()
	mt.RawSetString("__index", frozen)
	mt.RawSetString("__newindex", frozen)
	L.SetMetatable(proxy, mt)
	frozen.Freeze()
	L.SetGlobal("p", proxy)
	errorIfScriptFail(t, L, `assert(p.x == 1)`)
	errorIfScriptNotFail(t, L, `p.x = 2`, "read-only")
	errorIfNotEqual(t, LNumber(1), frozen.RawGetString("x"))
	errorIfNotEqual(t, LNil, proxy.RawGetString("x"))

	// table.sort on a frozen table without an array part still refuses
	hashOnly := L.NewTable()
	hashOnly.RawSetString("a", LNumber(3))
	hashOnly.RawSetString("b", LNumber(1))
	hashOnly.Freeze()
	L.SetGlobal("h", hashOnly)
	errorIfScriptNotFail(t, L, `table.sort(h)`, "read-only")
	errorIfNotEqual(t, LNumber(3), hashOnly.RawGetString("a"))
}
