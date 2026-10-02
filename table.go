package lua

import (
	"fmt"
	"math"
	"sort"
)

const defaultArrayCap = 32
const defaultHashCap = 32

type lValueArraySorter struct {
	L      *LState
	Fn     *LFunction
	Values []LValue
}

func (lv lValueArraySorter) Len() int {
	return len(lv.Values)
}

func (lv lValueArraySorter) Swap(i, j int) {
	lv.Values[i], lv.Values[j] = lv.Values[j], lv.Values[i]
}

func (lv lValueArraySorter) Less(i, j int) bool {
	if lv.Fn != nil {
		lv.L.Push(lv.Fn)
		lv.L.Push(lv.Values[i])
		lv.L.Push(lv.Values[j])
		lv.L.Call(2, 1)
		return LVAsBool(lv.L.reg.Pop())
	}
	return lessThan(lv.L, lv.Values[i], lv.Values[j])
}

func newLTable(acap int, hcap int) *LTable {
	if acap < 0 {
		acap = 0
	}
	if hcap < 0 {
		hcap = 0
	}
	tb := &LTable{}
	tb.Metatable = LNil
	if acap != 0 {
		tb.array = make([]LValue, 0, acap)
	}
	if hcap != 0 {
		tb.strdict = make(map[string]LValue, hcap)
	}
	return tb
}

// Len returns length of this LTable without using __len.
func (tb *LTable) Len() int {
	if tb.array == nil {
		return 0
	}
	var prev LValue = LNil
	for i := len(tb.array) - 1; i >= 0; i-- {
		v := tb.array[i]
		if prev == LNil && v != LNil {
			return i + 1
		}
		prev = v
	}
	return 0
}

// Append appends a given LValue to this LTable.
func (tb *LTable) Append(value LValue) {
	if value == LNil {
		return
	}
	if tb.array == nil {
		tb.array = make([]LValue, 0, defaultArrayCap)
	}
	if len(tb.array) == 0 || tb.array[len(tb.array)-1] != LNil {
		tb.array = append(tb.array, value)
	} else {
		i := len(tb.array) - 2
		for ; i >= 0; i-- {
			if tb.array[i] != LNil {
				break
			}
		}
		tb.array[i+1] = value
	}
}

// Insert inserts a given LValue at position `i` in this table.
func (tb *LTable) Insert(i int, value LValue) {
	if tb.array == nil {
		tb.array = make([]LValue, 0, defaultArrayCap)
	}
	if i > len(tb.array) {
		tb.RawSetInt(i, value)
		return
	}
	if i <= 0 {
		tb.RawSet(LNumber(i), value)
		return
	}
	i -= 1
	tb.array = append(tb.array, LNil)
	copy(tb.array[i+1:], tb.array[i:])
	tb.array[i] = value
}

// MaxN returns a maximum number key that nil value does not exist before it.
func (tb *LTable) MaxN() int {
	if tb.array == nil {
		return 0
	}
	for i := len(tb.array) - 1; i >= 0; i-- {
		if tb.array[i] != LNil {
			return i + 1
		}
	}
	return 0
}

// Remove removes from this table the element at a given position.
func (tb *LTable) Remove(pos int) LValue {
	if tb.array == nil {
		return LNil
	}
	larray := len(tb.array)
	if larray == 0 {
		return LNil
	}
	i := pos - 1
	oldval := LNil
	switch {
	case i >= larray:
		// nothing to do
	case i == larray-1 || i < 0:
		oldval = tb.array[larray-1]
		tb.array = tb.array[:larray-1]
	default:
		oldval = tb.array[i]
		copy(tb.array[i:], tb.array[i+1:])
		tb.array[larray-1] = nil
		tb.array = tb.array[:larray-1]
	}
	return oldval
}

// RawSet sets a given LValue to a given index without the __newindex metamethod.
// It is recommended to use `RawSetString` or `RawSetInt` for performance
// if you already know the given LValue is a string or number.
func (tb *LTable) RawSet(key LValue, value LValue) {
	switch v := key.(type) {
	case LNumber:
		if isArrayKey(v) {
			if tb.array == nil {
				tb.array = make([]LValue, 0, defaultArrayCap)
			}
			index := int(v) - 1
			alen := len(tb.array)
			switch {
			case index == alen:
				tb.array = append(tb.array, value)
			case index > alen:
				for i := 0; i < (index - alen); i++ {
					tb.array = append(tb.array, LNil)
				}
				tb.array = append(tb.array, value)
			case index < alen:
				tb.array[index] = value
			}
			return
		}
	case LString:
		tb.RawSetString(string(v), value)
		return
	}

	tb.RawSetH(key, value)
}

// RawSetInt sets a given LValue at a position `key` without the __newindex metamethod.
func (tb *LTable) RawSetInt(key int, value LValue) {
	if key < 1 || key >= MaxArrayIndex {
		tb.RawSetH(LNumber(key), value)
		return
	}
	if tb.array == nil {
		tb.array = make([]LValue, 0, 32)
	}
	index := key - 1
	alen := len(tb.array)
	switch {
	case index == alen:
		tb.array = append(tb.array, value)
	case index > alen:
		for i := 0; i < (index - alen); i++ {
			tb.array = append(tb.array, LNil)
		}
		tb.array = append(tb.array, value)
	case index < alen:
		tb.array[index] = value
	}
}

// RawSetString sets a given LValue to a given string index without the __newindex metamethod.
func (tb *LTable) RawSetString(key string, value LValue) {
	if tb.strdict == nil {
		tb.strdict = make(map[string]LValue, defaultHashCap)
	}
	if tb.keys == nil {
		tb.keys = []LValue{}
		tb.k2i = map[LValue]int{}
	}

	if value == LNil {
		// TODO tb.keys and tb.k2i should also be removed
		delete(tb.strdict, key)
	} else {
		tb.strdict[key] = value
		lkey := LString(key)
		if _, ok := tb.k2i[lkey]; !ok {
			tb.k2i[lkey] = len(tb.keys)
			tb.keys = append(tb.keys, lkey)
		}
	}
}

// RawSetH sets a given LValue to a given index without the __newindex metamethod.
func (tb *LTable) RawSetH(key LValue, value LValue) {
	if s, ok := key.(LString); ok {
		tb.RawSetString(string(s), value)
		return
	}
	if tb.dict == nil {
		tb.dict = make(map[LValue]LValue, len(tb.strdict))
	}
	if tb.keys == nil {
		tb.keys = []LValue{}
		tb.k2i = map[LValue]int{}
	}

	if value == LNil {
		// TODO tb.keys and tb.k2i should also be removed
		delete(tb.dict, key)
	} else {
		tb.dict[key] = value
		if _, ok := tb.k2i[key]; !ok {
			tb.k2i[key] = len(tb.keys)
			tb.keys = append(tb.keys, key)
		}
	}
}

// RawGet returns an LValue associated with a given key without __index metamethod.
func (tb *LTable) RawGet(key LValue) LValue {
	switch v := key.(type) {
	case LNumber:
		if isArrayKey(v) {
			if tb.array == nil {
				return LNil
			}
			index := int(v) - 1
			if index >= len(tb.array) {
				return LNil
			}
			return tb.array[index]
		}
	case LString:
		if tb.strdict == nil {
			return LNil
		}
		if ret, ok := tb.strdict[string(v)]; ok {
			return ret
		}
		return LNil
	}
	if tb.dict == nil {
		return LNil
	}
	if v, ok := tb.dict[key]; ok {
		return v
	}
	return LNil
}

// RawGetInt returns an LValue at position `key` without __index metamethod.
func (tb *LTable) RawGetInt(key int) LValue {
	if tb.array == nil {
		return LNil
	}
	index := int(key) - 1
	if index >= len(tb.array) || index < 0 {
		return LNil
	}
	return tb.array[index]
}

// RawGet returns an LValue associated with a given key without __index metamethod.
func (tb *LTable) RawGetH(key LValue) LValue {
	if s, sok := key.(LString); sok {
		if tb.strdict == nil {
			return LNil
		}
		if v, vok := tb.strdict[string(s)]; vok {
			return v
		}
		return LNil
	}
	if tb.dict == nil {
		return LNil
	}
	if v, ok := tb.dict[key]; ok {
		return v
	}
	return LNil
}

// RawGetString returns an LValue associated with a given key without __index metamethod.
func (tb *LTable) RawGetString(key string) LValue {
	if tb.strdict == nil {
		return LNil
	}
	if v, vok := tb.strdict[string(key)]; vok {
		return v
	}
	return LNil
}

// ForEach iterates over this table of elements, yielding each in turn to a given function.
func (tb *LTable) ForEach(cb func(LValue, LValue)) {
	if tb.array != nil {
		for i, v := range tb.array {
			if v != LNil {
				cb(LNumber(i+1), v)
			}
		}
	}
	if tb.strdict != nil {
		for k, v := range tb.strdict {
			if v != LNil {
				cb(LString(k), v)
			}
		}
	}
	if tb.dict != nil {
		for k, v := range tb.dict {
			if v != LNil {
				cb(k, v)
			}
		}
	}
}

// OrderedIterator iterates a table in deterministic key order.
//
// Keys are snapshotted when the iterator is created: deleting a key skips it,
// while adding a key after creation does not visit it. Current values are read
// from the table when Next is called.
type OrderedIterator struct {
	table *LTable
	keys  []LValue
	index int
}

// NewOrderedIterator returns an iterator over numeric, string, and boolean
// keys. Numeric keys are ordered numerically, strings lexicographically, and
// booleans as false followed by true. Other key types are rejected.
func NewOrderedIterator(tb *LTable) (*OrderedIterator, error) {
	numbers := make([]LNumber, 0)
	strings := make([]string, 0)
	booleans := make([]LBool, 0, 2)
	seenNumbers := make(map[float64]struct{})
	seenStrings := make(map[string]struct{})
	seenBooleans := make(map[bool]struct{})

	addNumber := func(key LNumber) error {
		value := float64(key)
		if math.IsNaN(value) {
			return fmt.Errorf("ordered iteration does not support NaN keys")
		}
		if value == 0 {
			value = math.Copysign(0, 1)
		}
		if _, ok := seenNumbers[value]; ok {
			return nil
		}
		seenNumbers[value] = struct{}{}
		numbers = append(numbers, LNumber(value))
		return nil
	}

	addString := func(key LString) {
		value := string(key)
		if _, ok := seenStrings[value]; ok {
			return
		}
		seenStrings[value] = struct{}{}
		strings = append(strings, value)
	}

	addBool := func(key LBool) {
		value := bool(key)
		if _, ok := seenBooleans[value]; ok {
			return
		}
		seenBooleans[value] = struct{}{}
		booleans = append(booleans, LBool(value))
	}

	if tb.array != nil {
		for i, value := range tb.array {
			if value != LNil {
				if err := addNumber(LNumber(i + 1)); err != nil {
					return nil, err
				}
			}
		}
	}

	if tb.strdict != nil {
		for key, value := range tb.strdict {
			if value != LNil {
				addString(LString(key))
			}
		}
	}

	if tb.dict != nil {
		for key, value := range tb.dict {
			if value == LNil {
				continue
			}
			switch key := key.(type) {
			case LNumber:
				if err := addNumber(key); err != nil {
					return nil, err
				}
			case LString:
				addString(key)
			case LBool:
				addBool(key)
			default:
				return nil, fmt.Errorf("ordered iteration does not support %s keys", key.Type())
			}
		}
	}

	sort.Slice(numbers, func(i, j int) bool {
		return float64(numbers[i]) < float64(numbers[j])
	})
	sort.Strings(strings)
	sort.Slice(booleans, func(i, j int) bool {
		return !bool(booleans[i]) && bool(booleans[j])
	})

	keys := make([]LValue, 0, len(numbers)+len(strings)+len(booleans))
	for _, key := range numbers {
		keys = append(keys, key)
	}
	for _, key := range strings {
		keys = append(keys, LString(key))
	}
	for _, key := range booleans {
		keys = append(keys, key)
	}

	return &OrderedIterator{table: tb, keys: keys}, nil
}

// Next advances to the next key that still exists and returns its current
// value. It returns false when iteration is complete.
func (it *OrderedIterator) Next() (LValue, LValue, bool) {
	for it.index < len(it.keys) {
		key := it.keys[it.index]
		it.index++
		value := orderedValue(it.table, key)
		if value != LNil {
			return key, value, true
		}
	}
	return LNil, LNil, false
}

func orderedValue(tb *LTable, key LValue) LValue {
	if number, ok := key.(LNumber); ok && isArrayKey(number) {
		index := int(number) - 1
		if tb.array != nil && index >= 0 && index < len(tb.array) {
			if value := tb.array[index]; value != LNil {
				return value
			}
		}
	}
	return tb.RawGetH(key)
}

// OrderedForEach calls cb for every numeric, string, and boolean key in
// deterministic key order.
func (tb *LTable) OrderedForEach(cb func(LValue, LValue)) error {
	iterator, err := NewOrderedIterator(tb)
	if err != nil {
		return err
	}
	for {
		key, value, ok := iterator.Next()
		if !ok {
			return nil
		}
		cb(key, value)
	}
}

// This function is equivalent to lua_next ( http://www.lua.org/manual/5.1/manual.html#lua_next ).
func (tb *LTable) Next(key LValue) (LValue, LValue) {
	init := false
	if key == LNil {
		key = LNumber(0)
		init = true
	}

	if init || key != LNumber(0) {
		if kv, ok := key.(LNumber); ok && isInteger(kv) && int(kv) >= 0 && kv < LNumber(MaxArrayIndex) {
			index := int(kv)
			if tb.array != nil {
				for ; index < len(tb.array); index++ {
					if v := tb.array[index]; v != LNil {
						return LNumber(index + 1), v
					}
				}
			}
			if tb.array == nil || index == len(tb.array) {
				if (tb.dict == nil || len(tb.dict) == 0) && (tb.strdict == nil || len(tb.strdict) == 0) {
					return LNil, LNil
				}
				key = tb.keys[0]
				if v := tb.RawGetH(key); v != LNil {
					return key, v
				}
			}
		}
	}

	for i := tb.k2i[key] + 1; i < len(tb.keys); i++ {
		key := tb.keys[i]
		if v := tb.RawGetH(key); v != LNil {
			return key, v
		}
	}
	return LNil, LNil
}
