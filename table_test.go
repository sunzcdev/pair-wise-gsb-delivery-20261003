package lua

import (
	"math"
	"strings"
	"testing"
)

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

func orderedEntries(t *testing.T, tb *LTable) []string {
	t.Helper()

	iterator, err := NewOrderedIterator(tb)
	if err != nil {
		t.Fatal(err)
	}

	entries := []string{}
	for {
		key, value, ok := iterator.Next()
		if !ok {
			return entries
		}
		entries = append(entries, key.String()+"="+value.String())
	}
}

func TestTableOrderedForEach(t *testing.T) {
	build := func(keys ...LValue) *LTable {
		tb := newLTable(0, 0)
		for _, key := range keys {
			tb.RawSet(key, LString(key.String()+"-v"))
		}
		return tb
	}

	keys := []LValue{
		LNumber(3), LString("banana"), LFalse, LNumber(-1), LNumber(2.5),
		LNumber(1), LString("apple"), LTrue,
		LNumber(2), LString("cherry"), LNumber(0), LNumber(-2),
		LNumber(MaxArrayIndex + 1),
	}
	reversed := []LValue{
		LNumber(MaxArrayIndex + 1), LNumber(-2), LNumber(0), LString("cherry"),
		LNumber(2), LString("apple"), LTrue, LNumber(1),
		LNumber(2.5), LNumber(-1), LFalse, LString("banana"), LNumber(3),
	}

	first := build(keys...)
	second := build(reversed...)
	expected := []string{
		"-2=-2-v", "-1=-1-v", "0=0-v",
		"1=1-v", "2=2-v", "2.5=2.5-v", "3=3-v",
		"67108865=67108865-v",
		"apple=apple-v", "banana=banana-v", "cherry=cherry-v",
		"false=false-v", "true=true-v",
	}

	errorIfNotEqual(t, strings.Join(expected, ","), strings.Join(orderedEntries(t, first), ","))
	errorIfNotEqual(t, strings.Join(expected, ","), strings.Join(orderedEntries(t, second), ","))
}

func TestTableOrderedIteratorSnapshot(t *testing.T) {
	tb := newLTable(0, 0)
	tb.RawSetInt(1, LString("one"))
	tb.RawSetInt(2, LString("two"))
	tb.RawSetString("a", LString("old"))

	iterator, err := NewOrderedIterator(tb)
	if err != nil {
		t.Fatal(err)
	}

	key, value, ok := iterator.Next()
	errorIfNotEqual(t, true, ok)
	errorIfNotEqual(t, LNumber(1), key)
	errorIfNotEqual(t, LString("one"), value)

	tb.RawSetInt(2, LNil)
	tb.RawSetInt(4, LString("four"))
	tb.RawSetString("a", LString("new"))
	tb.RawSetString("z", LString("new-key"))

	key, value, ok = iterator.Next()
	errorIfNotEqual(t, true, ok)
	errorIfNotEqual(t, LString("a"), key)
	errorIfNotEqual(t, LString("new"), value)

	_, _, ok = iterator.Next()
	errorIfNotEqual(t, false, ok)
}

func TestTableOrderedIteratorNoDuplicateLogicalKeys(t *testing.T) {
	tb := newLTable(0, 0)
	tb.array = []LValue{LString("array"), LNil}
	tb.strdict = map[string]LValue{"s": LString("string")}
	tb.dict = map[LValue]LValue{
		LNumber(1): LString("hash"),
		LNumber(2): LString("hole"),
		LTrue:      LString("true"),
	}
	tb.RawSetH(LNumber(math.Copysign(0, -1)), LString("negative-zero"))
	tb.RawSetH(LNumber(0), LString("zero"))

	entries := orderedEntries(t, tb)
	expected := []string{
		"0=zero", "1=array", "2=hole", "s=string", "true=true",
	}
	errorIfNotEqual(t, strings.Join(expected, ","), strings.Join(entries, ","))
}

func TestTableOrderedIteratorSpecialKeys(t *testing.T) {
	tb := newLTable(0, 0)
	tb.RawSet(LNumber(math.Copysign(0, -1)), LString("negative-zero"))
	tb.RawSet(LNumber(0), LString("zero"))

	entries := orderedEntries(t, tb)
	errorIfNotEqual(t, "0=zero", strings.Join(entries, ","))

	tb = newLTable(0, 0)
	tb.RawSet(LNumber(math.NaN()), LTrue)
	_, err := NewOrderedIterator(tb)
	if err == nil {
		t.Fatal("expected NaN key to be rejected")
	}

	tb = newLTable(0, 0)
	tb.RawSet(newLTable(0, 0), LTrue)
	_, err = NewOrderedIterator(tb)
	if err == nil {
		t.Fatal("expected reference key to be rejected")
	}
}

func TestTableOrderedIteratorStateEntry(t *testing.T) {
	L := NewState(Options{})
	tb := L.NewTable()
	tb.RawSetString("b", LNumber(2))
	tb.RawSetString("a", LNumber(1))

	entries := []string{}
	err := L.OrderedForEach(tb, func(key, value LValue) {
		entries = append(entries, key.String()+"="+value.String())
	})
	if err != nil {
		t.Fatal(err)
	}
	errorIfNotEqual(t, "a=1,b=2", strings.Join(entries, ","))
}

func TestTableNextStillSupportsReferenceKeys(t *testing.T) {
	tb := newLTable(0, 0)
	referenceKey := newLTable(0, 0)
	tb.RawSetH(referenceKey, LString("value"))

	key, value := tb.Next(LNil)
	errorIfNotEqual(t, referenceKey, key)
	errorIfNotEqual(t, LString("value"), value)
}
