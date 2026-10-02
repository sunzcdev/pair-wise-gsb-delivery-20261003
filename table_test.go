package lua

import (
	"math"
	"testing"
)

func orderedTableEntries(tb *LTable) []string {
	entries := []string{}
	tb.ForEachOrdered(func(key, value LValue) {
		entries = append(entries, key.String()+"="+value.String())
	})
	return entries
}

func buildOrderedTestTable(forward bool) *LTable {
	tb := newLTable(0, 0)

	setValues := func(values ...LValue) {
		for i, value := range values {
			if forward {
				tb.RawSetH(LNumber(math.Float64frombits(0x7ff8000000000001+uint64(i))), value)
			} else {
				tb.RawSetH(LNumber(math.Float64frombits(0x7ff8000000000002-uint64(i))), value)
			}
		}
	}
	if forward {
		tb.RawSet(LNumber(1), LString("one"))
		tb.RawSet(LNumber(-1), LString("minus"))
		tb.RawSet(LNumber(2.5), LString("decimal"))
		tb.RawSet(LNumber(0), LString("zero"))
		setValues(LString("n1"), LString("n2"))
		tb.RawSetString("a", LString("lower"))
		tb.RawSetString("A", LString("upper"))
		tb.RawSetString("b", LString("bee"))
		tb.RawSetH(LTrue, LString("tb"))
		tb.RawSetH(LFalse, LString("fb"))
	} else {
		tb.RawSetH(LFalse, LString("fb"))
		tb.RawSetH(LTrue, LString("tb"))
		tb.RawSetString("b", LString("bee"))
		tb.RawSetString("A", LString("upper"))
		tb.RawSetString("a", LString("lower"))
		setValues(LString("n2"), LString("n1"))
		tb.RawSetH(LNumber(math.Copysign(0, -1)), LString("zero"))
		tb.RawSet(LNumber(2.5), LString("decimal"))
		tb.RawSet(LNumber(-1), LString("minus"))
		tb.RawSet(LNumber(1), LString("one"))
	}

	return tb
}

func TestTableForEachOrdered(t *testing.T) {
	expected := []string{
		"NaN=n1",
		"NaN=n2",
		"-1=minus",
		"0=zero",
		"1=one",
		"2.5=decimal",
		"A=upper",
		"a=lower",
		"b=bee",
		"false=fb",
		"true=tb",
	}

	for i := 0; i < 20; i++ {
		errorIfNotEqual(t, len(expected), len(orderedTableEntries(buildOrderedTestTable(true))))
		errorIfNotEqual(t, len(expected), len(orderedTableEntries(buildOrderedTestTable(false))))

		forward := orderedTableEntries(buildOrderedTestTable(true))
		reverse := orderedTableEntries(buildOrderedTestTable(false))
		for index := range expected {
			errorIfNotEqual(t, expected[index], forward[index])
			errorIfNotEqual(t, expected[index], reverse[index])
		}
	}
}

func TestTableOrderedIteratorUsesKeySnapshot(t *testing.T) {
	tb := newLTable(0, 0)
	tb.RawSetInt(1, LString("one"))
	tb.RawSetInt(2, LString("two"))
	tb.RawSetString("a", LString("a"))
	tb.RawSetString("b", LString("b"))
	tb.RawSetH(LTrue, LString("true"))
	tb.RawSetH(LFalse, LString("false"))

	entries := []string{}
	tb.ForEachOrdered(func(key, value LValue) {
		if key == LNumber(1) {
			tb.RawSetInt(2, LNil)
			tb.RawSetInt(2, LString("readded"))
			tb.RawSetInt(3, LString("added"))
			tb.RawSetString("b", LNil)
			tb.RawSetH(LTrue, LString("changed"))
		}
		entries = append(entries, key.String()+"="+value.String())
	})

	expected := []string{
		"1=one",
		"2=readded",
		"a=a",
		"false=false",
		"true=changed",
	}
	errorIfNotEqual(t, len(expected), len(entries))
	for index := range expected {
		errorIfNotEqual(t, expected[index], entries[index])
	}
}

func TestTableOrderedIteratorSkipsDuplicateStorage(t *testing.T) {
	tb := newLTable(0, 0)
	tb.RawSetInt(1, LString("array"))
	tb.dict = map[LValue]LValue{LNumber(1): LString("hash")}
	tb.dict[LNumber(math.Copysign(0, -1))] = LString("zero-hash")
	tb.RawSetH(LNumber(0), LString("zero"))
	tb.dict[LString("duplicate")] = LString("dict-string")
	tb.RawSetString("duplicate", LString("strdict-string"))

	entries := orderedTableEntries(tb)
	errorIfNotEqual(t, 3, len(entries))
	errorIfNotEqual(t, "0=zero", entries[0])
	errorIfNotEqual(t, "1=array", entries[1])
	errorIfNotEqual(t, "duplicate=strdict-string", entries[2])
}

func TestTableOrderedIteratorRejectsReferenceKeys(t *testing.T) {
	tb := newLTable(0, 0)
	tb.RawSetH(newLTable(0, 0), LTrue)

	defer func() {
		errorIfFalse(t, recover() != nil, "ordered iteration should reject reference keys")
	}()
	tb.ForEachOrdered(func(LValue, LValue) {})
}

func TestTableNewLTable(t *testing.T) {
	tbl := newLTable(-1, -2)
	errorIfNotEqual(t, 0, cap(tbl.array))

	tbl = newLTable(10, 9)
	errorIfNotEqual(t, 10, cap(tbl.array))
}

func TestTableLen(t *testing.T) {
	tbl := newLTable(0, 0)
	tbl.RawSetInt(10, LNil)
	tbl.RawSetInt(9, LNumber(10))
	tbl.RawSetInt(8, LNil)
	tbl.RawSetInt(7, LNumber(10))
	errorIfNotEqual(t, 9, tbl.Len())

	tbl = newLTable(0, 0)
	tbl.Append(LTrue)
	tbl.Append(LTrue)
	tbl.Append(LTrue)
	errorIfNotEqual(t, 3, tbl.Len())
}

func TestTableLenType(t *testing.T) {
	L := NewState(Options{})
	err := L.DoString(`
        mt = {
            __index = mt,
            __len = function (self)
                return {hello = "world"}
            end
        }

        v = {}
        v.__index = v

        setmetatable(v, mt)

        assert(#v ~= 0, "#v should return a table reference in this case")

        print(#v)
    `)
	if err != nil {
		t.Error(err)
	}
}

func TestTableAppend(t *testing.T) {
	tbl := newLTable(0, 0)
	tbl.RawSetInt(1, LNumber(1))
	tbl.RawSetInt(2, LNumber(2))
	tbl.RawSetInt(3, LNumber(3))
	errorIfNotEqual(t, 3, tbl.Len())

	tbl.RawSetInt(1, LNil)
	tbl.RawSetInt(2, LNil)
	errorIfNotEqual(t, 3, tbl.Len())

	tbl.Append(LNumber(4))
	errorIfNotEqual(t, 4, tbl.Len())

	tbl.RawSetInt(3, LNil)
	tbl.RawSetInt(4, LNil)
	errorIfNotEqual(t, 0, tbl.Len())

	tbl.Append(LNumber(5))
	errorIfNotEqual(t, 1, tbl.Len())
}

func TestTableInsert(t *testing.T) {
	tbl := newLTable(0, 0)
	tbl.Append(LTrue)
	tbl.Append(LTrue)
	tbl.Append(LTrue)

	tbl.Insert(5, LFalse)
	errorIfNotEqual(t, LFalse, tbl.RawGetInt(5))
	errorIfNotEqual(t, 5, tbl.Len())

	tbl.Insert(-10, LFalse)
	errorIfNotEqual(t, LFalse, tbl.RawGet(LNumber(-10)))
	errorIfNotEqual(t, 5, tbl.Len())

	tbl = newLTable(0, 0)
	tbl.Append(LNumber(1))
	tbl.Append(LNumber(2))
	tbl.Append(LNumber(3))
	tbl.Insert(1, LNumber(10))
	errorIfNotEqual(t, LNumber(10), tbl.RawGetInt(1))
	errorIfNotEqual(t, LNumber(1), tbl.RawGetInt(2))
	errorIfNotEqual(t, LNumber(2), tbl.RawGetInt(3))
	errorIfNotEqual(t, LNumber(3), tbl.RawGetInt(4))
	errorIfNotEqual(t, 4, tbl.Len())

	tbl = newLTable(0, 0)
	tbl.Insert(5, LNumber(10))
	errorIfNotEqual(t, LNumber(10), tbl.RawGetInt(5))

}

func TestTableMaxN(t *testing.T) {
	tbl := newLTable(0, 0)
	tbl.Append(LTrue)
	tbl.Append(LTrue)
	tbl.Append(LTrue)
	errorIfNotEqual(t, 3, tbl.MaxN())

	tbl = newLTable(0, 0)
	errorIfNotEqual(t, 0, tbl.MaxN())

	tbl = newLTable(10, 0)
	errorIfNotEqual(t, 0, tbl.MaxN())
}

func TestTableRemove(t *testing.T) {
	tbl := newLTable(0, 0)
	errorIfNotEqual(t, LNil, tbl.Remove(10))
	tbl.Append(LTrue)
	errorIfNotEqual(t, LNil, tbl.Remove(10))

	tbl.Append(LFalse)
	tbl.Append(LTrue)
	errorIfNotEqual(t, LFalse, tbl.Remove(2))
	errorIfNotEqual(t, 2, tbl.MaxN())
	tbl.Append(LFalse)
	errorIfNotEqual(t, LFalse, tbl.Remove(-1))
	errorIfNotEqual(t, 2, tbl.MaxN())

}

func TestTableRawSetInt(t *testing.T) {
	tbl := newLTable(0, 0)
	tbl.RawSetInt(MaxArrayIndex+1, LTrue)
	errorIfNotEqual(t, 0, tbl.MaxN())
	errorIfNotEqual(t, LTrue, tbl.RawGet(LNumber(MaxArrayIndex+1)))

	tbl.RawSetInt(1, LTrue)
	tbl.RawSetInt(3, LTrue)
	errorIfNotEqual(t, 3, tbl.MaxN())
	errorIfNotEqual(t, LTrue, tbl.RawGetInt(1))
	errorIfNotEqual(t, LNil, tbl.RawGetInt(2))
	errorIfNotEqual(t, LTrue, tbl.RawGetInt(3))
	tbl.RawSetInt(2, LTrue)
	errorIfNotEqual(t, LTrue, tbl.RawGetInt(1))
	errorIfNotEqual(t, LTrue, tbl.RawGetInt(2))
	errorIfNotEqual(t, LTrue, tbl.RawGetInt(3))
}

func TestTableRawSetH(t *testing.T) {
	tbl := newLTable(0, 0)
	tbl.RawSetH(LString("key"), LTrue)
	tbl.RawSetH(LString("key"), LNil)
	_, found := tbl.dict[LString("key")]
	errorIfNotEqual(t, false, found)

	tbl.RawSetH(LTrue, LTrue)
	tbl.RawSetH(LTrue, LNil)
	_, foundb := tbl.dict[LTrue]
	errorIfNotEqual(t, false, foundb)
}

func TestTableRawGetH(t *testing.T) {
	tbl := newLTable(0, 0)
	errorIfNotEqual(t, LNil, tbl.RawGetH(LNumber(1)))
	errorIfNotEqual(t, LNil, tbl.RawGetH(LString("key0")))
	tbl.RawSetH(LString("key0"), LTrue)
	tbl.RawSetH(LString("key1"), LFalse)
	tbl.RawSetH(LNumber(1), LTrue)
	errorIfNotEqual(t, LTrue, tbl.RawGetH(LString("key0")))
	errorIfNotEqual(t, LTrue, tbl.RawGetH(LNumber(1)))
	errorIfNotEqual(t, LNil, tbl.RawGetH(LString("notexist")))
	errorIfNotEqual(t, LNil, tbl.RawGetH(LTrue))
}

func TestTableForEach(t *testing.T) {
	tbl := newLTable(0, 0)
	tbl.Append(LNumber(1))
	tbl.Append(LNumber(2))
	tbl.Append(LNumber(3))
	tbl.Append(LNil)
	tbl.Append(LNumber(5))

	tbl.RawSetH(LString("a"), LString("a"))
	tbl.RawSetH(LString("b"), LString("b"))
	tbl.RawSetH(LString("c"), LString("c"))

	tbl.RawSetH(LTrue, LString("true"))
	tbl.RawSetH(LFalse, LString("false"))

	tbl.ForEach(func(key, value LValue) {
		switch k := key.(type) {
		case LBool:
			switch bool(k) {
			case true:
				errorIfNotEqual(t, LString("true"), value)
			case false:
				errorIfNotEqual(t, LString("false"), value)
			default:
				t.Fail()
			}
		case LNumber:
			switch int(k) {
			case 1:
				errorIfNotEqual(t, LNumber(1), value)
			case 2:
				errorIfNotEqual(t, LNumber(2), value)
			case 3:
				errorIfNotEqual(t, LNumber(3), value)
			case 4:
				errorIfNotEqual(t, LNumber(5), value)
			default:
				t.Fail()
			}
		case LString:
			switch string(k) {
			case "a":
				errorIfNotEqual(t, LString("a"), value)
			case "b":
				errorIfNotEqual(t, LString("b"), value)
			case "c":
				errorIfNotEqual(t, LString("c"), value)
			default:
				t.Fail()
			}
		}
	})
}
