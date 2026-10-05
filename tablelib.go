package lua

import (
	"sort"
)

func OpenTable(L *LState) int {
	tabmod := L.RegisterModule(TabLibName, tableFuncs)
	L.Push(tabmod)
	return 1
}

var tableFuncs = map[string]LGFunction{
	"getn":        tableGetN,
	"concat":      tableConcat,
	"insert":      tableInsert,
	"maxn":        tableMaxN,
	"remove":      tableRemove,
	"sort":        tableSort,
	"stable_sort": tableStableSort,
}

func tableSort(L *LState) int {
	tbl := L.CheckTable(1)
	sorter := lValueArraySorter{L, nil, tbl.array}
	if L.GetTop() != 1 {
		sorter.Fn = L.CheckFunction(2)
	}
	sort.Sort(sorter)
	return 0
}

func tableStableSort(L *LState) int {
	tbl := L.CheckTable(1)
	var fn *LFunction
	if L.GetTop() > 1 {
		fn = L.CheckFunction(2)
	}

	// Sparse arrays are sorted over the prefix defined by the Lua length
	// operator, read without invoking __len (matching table.sort).
	n := tbl.Len()

	// Take two independent copies of the entire backing slice. values is
	// the scratch area; only its [0:n) prefix is reordered. reference is
	// never reordered and proves afterwards that the comparator did not
	// write into the table while sorting, including trailing holes.
	// Neither copy may alias tbl.array or the writes would hide.
	origLen := len(tbl.array)
	reference := make([]LValue, origLen)
	copy(reference, tbl.array)
	values := make([]LValue, origLen)
	copy(values, reference)

	sort.SliceStable(values[:n], func(i, j int) bool {
		if fn != nil {
			L.Push(fn)
			L.Push(values[i])
			L.Push(values[j])
			L.Call(2, 1)
			return LVAsBool(L.reg.Pop())
		}
		return lessThan(L, values[i], values[j])
	})

	// The comparator may have errored (already propagated), triggered
	// metamethods, or written back into the table. Reject the commit if
	// the array part changed so the comparator's writes are not lost.
	if len(tbl.array) != origLen {
		L.RaiseError("invalid order function for sorting (table modified during stable_sort)")
	}
	for i, value := range reference {
		if tbl.array[i] != value {
			L.RaiseError("invalid order function for sorting (table modified during stable_sort)")
		}
	}

	copy(tbl.array, values[:n])
	return 0
}

func tableGetN(L *LState) int {
	L.Push(LNumber(L.CheckTable(1).Len()))
	return 1
}

func tableMaxN(L *LState) int {
	L.Push(LNumber(L.CheckTable(1).MaxN()))
	return 1
}

func tableRemove(L *LState) int {
	tbl := L.CheckTable(1)
	if L.GetTop() == 1 {
		L.Push(tbl.Remove(-1))
	} else {
		L.Push(tbl.Remove(L.CheckInt(2)))
	}
	return 1
}

func tableConcat(L *LState) int {
	tbl := L.CheckTable(1)
	sep := LString(L.OptString(2, ""))
	i := L.OptInt(3, 1)
	j := L.OptInt(4, tbl.Len())
	if L.GetTop() == 3 {
		if i > tbl.Len() || i < 1 {
			L.Push(emptyLString)
			return 1
		}
	}
	i = intMax(intMin(i, tbl.Len()), 1)
	j = intMin(intMin(j, tbl.Len()), tbl.Len())
	if i > j {
		L.Push(emptyLString)
		return 1
	}
	//TODO should flushing?
	retbottom := L.GetTop()
	for ; i <= j; i++ {
		v := tbl.RawGetInt(i)
		if !LVCanConvToString(v) {
			L.RaiseError("invalid value (%s) at index %d in table for concat", v.Type().String(), i)
		}
		L.Push(v)
		if i != j {
			L.Push(sep)
		}
	}
	L.Push(stringConcat(L, L.GetTop()-retbottom, L.reg.Top()-1))
	return 1
}

func tableInsert(L *LState) int {
	tbl := L.CheckTable(1)
	nargs := L.GetTop()
	if nargs == 1 {
		L.RaiseError("wrong number of arguments")
	}

	if L.GetTop() == 2 {
		tbl.Append(L.Get(2))
		return 0
	}
	tbl.Insert(int(L.CheckInt(2)), L.CheckAny(3))
	return 0
}

//
