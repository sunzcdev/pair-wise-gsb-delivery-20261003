package lua

import "testing"

func TestTableFreezeSortComparatorNotCalled(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	tb := L.NewTable()
	tb.Append(LNumber(1))
	tb.Append(LNumber(2))
	tb.Freeze()
	L.SetGlobal("t", tb)
	// frozen check happens before comparator touches the table
	errorIfScriptNotFail(t, L, `
		local called = false
		table.sort(t, function(a, b) called = true; return a < b end)
		assert(not called)
	`, "read-only")
}
