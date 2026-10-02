package batch

type vkind int

const (
	vAny vkind = iota
	vBool
	vString
	vInt
	vInt64
	vFloat64
	vFloat32
	vInt8
	vInt16
	vInt32
	vUint
	vUint16
	vUint32
	vUint64
)

// vector is a column of values. Typed slices avoid boxing values as any in
// the hot paths; nulls can only be represented with the generic vAny slice.
type vector struct {
	kind vkind

	anys     []any
	bools    []bool
	strings  []string
	ints     []int
	int64s   []int64
	float64s []float64
	float32s []float32
	int8s    []int8
	int16s   []int16
	int32s   []int32
	uints    []uint
	uint16s  []uint16
	uint32s  []uint32
	uint64s  []uint64
}

func (v vector) len() int {
	switch v.kind {
	case vAny:
		return len(v.anys)
	case vBool:
		return len(v.bools)
	case vString:
		return len(v.strings)
	case vInt:
		return len(v.ints)
	case vInt64:
		return len(v.int64s)
	case vFloat64:
		return len(v.float64s)
	case vFloat32:
		return len(v.float32s)
	case vInt8:
		return len(v.int8s)
	case vInt16:
		return len(v.int16s)
	case vInt32:
		return len(v.int32s)
	case vUint:
		return len(v.uints)
	case vUint16:
		return len(v.uint16s)
	case vUint32:
		return len(v.uint32s)
	case vUint64:
		return len(v.uint64s)
	}
	return 0
}

func (v vector) at(i int) any {
	switch v.kind {
	case vAny:
		return v.anys[i]
	case vBool:
		return v.bools[i]
	case vString:
		return v.strings[i]
	case vInt:
		return v.ints[i]
	case vInt64:
		return v.int64s[i]
	case vFloat64:
		return v.float64s[i]
	case vFloat32:
		return v.float32s[i]
	case vInt8:
		return v.int8s[i]
	case vInt16:
		return v.int16s[i]
	case vInt32:
		return v.int32s[i]
	case vUint:
		return v.uints[i]
	case vUint16:
		return v.uint16s[i]
	case vUint32:
		return v.uint32s[i]
	case vUint64:
		return v.uint64s[i]
	}
	return nil
}

// get returns the value for logical row i, broadcasting a constant.
func (v vector) get(i int) any {
	return v.at(v.index(i))
}

func (v vector) getBool(i int) bool {
	switch v.kind {
	case vBool:
		return v.bools[i]
	case vAny:
		return v.anys[i].(bool)
	}
	return false
}

func (v vector) getString(i int) string {
	switch v.kind {
	case vString:
		return v.strings[i]
	case vAny:
		return v.anys[i].(string)
	}
	return ""
}

func (v vector) getInt(i int) int {
	switch v.kind {
	case vInt:
		return v.ints[i]
	case vAny:
		return v.anys[i].(int)
	}
	return 0
}

func (v vector) getInt64(i int) int64 {
	switch v.kind {
	case vInt64:
		return v.int64s[i]
	case vAny:
		return v.anys[i].(int64)
	}
	return 0
}

func (v vector) getFloat64(i int) float64 {
	switch v.kind {
	case vFloat64:
		return v.float64s[i]
	case vAny:
		return v.anys[i].(float64)
	}
	return 0
}

func (v vector) getFloat32(i int) float32 {
	switch v.kind {
	case vFloat32:
		return v.float32s[i]
	case vAny:
		return v.anys[i].(float32)
	}
	return 0
}

// isNull reports whether the element at i is null. Only vAny elements can be
// null; typed slices never contain nulls.
// index returns the backing position for logical row i.
func (v vector) index(i int) int {
	return i
}

func newAnys(n int) vector {
	return vector{kind: vAny, anys: make([]any, n)}
}

func newBools(n int) vector {
	return vector{kind: vBool, bools: make([]bool, n)}
}

func newStrings(n int) vector {
	return vector{kind: vString, strings: make([]string, n)}
}

func newInts(n int) vector {
	return vector{kind: vInt, ints: make([]int, n)}
}

func newInt64s(n int) vector {
	return vector{kind: vInt64, int64s: make([]int64, n)}
}

func newFloat64s(n int) vector {
	return vector{kind: vFloat64, float64s: make([]float64, n)}
}

func newFloat32s(n int) vector {
	return vector{kind: vFloat32, float32s: make([]float32, n)}
}

// setAny stores a generic value.
func (v vector) setAny(i int, value any) {
	v.anys[i] = value
}
