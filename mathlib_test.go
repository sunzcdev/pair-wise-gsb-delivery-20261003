package lua

import (
	"math"
	"math/rand"
	"testing"
)

func newSeededMathState(t *testing.T, seed int64) *LState {
	t.Helper()
	return NewState(Options{RandomSeed: &seed})
}

func mathRandomValues(t *testing.T, L *LState, args ...LValue) []LNumber {
	t.Helper()
	mathTable := L.GetGlobal("math").(*LTable)
	random := L.GetField(mathTable, "random").(*LFunction)
	if err := L.CallByParam(P{Fn: random, NRet: 1, Protect: true}, args...); err != nil {
		t.Fatal(err)
	}

	values := make([]LNumber, 1)
	values[0] = L.Get(-1).(LNumber)
	L.Pop(1)
	return values
}

func setMathRandomSeed(t *testing.T, L *LState, seed int64) {
	t.Helper()
	mathTable := L.GetGlobal("math").(*LTable)
	randomseed := L.GetField(mathTable, "randomseed").(*LFunction)
	if err := L.CallByParam(P{Fn: randomseed, NRet: 0, Protect: true}, LNumber(seed)); err != nil {
		t.Fatal(err)
	}
}

func TestMathRandomDeterministicSeed(t *testing.T) {
	seed := int64(123)
	first := newSeededMathState(t, seed)
	defer first.Close()
	second := newSeededMathState(t, seed)
	defer second.Close()

	for i := 0; i < 16; i++ {
		want := mathRandomValues(t, first)[0]
		got := mathRandomValues(t, second)[0]
		if got != want {
			t.Fatalf("random value %d = %v, want %v", i, got, want)
		}
	}
}

func TestMathRandomStatesAreIsolated(t *testing.T) {
	first := newSeededMathState(t, 1)
	defer first.Close()
	second := newSeededMathState(t, 2)
	defer second.Close()

	firstValues := make([]LNumber, 8)
	secondValues := make([]LNumber, 8)
	for i := range firstValues {
		firstValues[i] = mathRandomValues(t, first)[0]
		secondValues[i] = mathRandomValues(t, second)[0]
	}

	setMathRandomSeed(t, first, 100)
	setMathRandomSeed(t, second, 2)
	for i, want := range secondValues {
		if got := mathRandomValues(t, second)[0]; got != want {
			t.Fatalf("second state value %d = %v, want %v after first was reseeded", i, got, want)
		}
	}

	first = newSeededMathState(t, 1)
	for i, want := range firstValues {
		if got := mathRandomValues(t, first)[0]; got != want {
			t.Fatalf("reseeded first state value %d = %v, want %v", i, got, want)
		}
	}
}

func TestMathRandomInterleavedStatesKeepSequences(t *testing.T) {
	first := newSeededMathState(t, 7)
	defer first.Close()
	second := newSeededMathState(t, 7)
	defer second.Close()

	firstAlone := make([]LNumber, 4)
	secondAlone := make([]LNumber, 4)
	for i := range firstAlone {
		firstAlone[i] = mathRandomValues(t, first)[0]
		secondAlone[i] = mathRandomValues(t, second)[0]
	}

	setMathRandomSeed(t, first, 7)
	setMathRandomSeed(t, second, 7)
	for i := 0; i < len(firstAlone); i++ {
		if got := mathRandomValues(t, first)[0]; got != firstAlone[i] {
			t.Fatalf("interleaved first value %d = %v, want %v", i, got, firstAlone[i])
		}
		if got := mathRandomValues(t, second)[0]; got != secondAlone[i] {
			t.Fatalf("interleaved second value %d = %v, want %v", i, got, secondAlone[i])
		}
	}
}

func TestMathRandomThreadSharesStateSequence(t *testing.T) {
	L := newSeededMathState(t, 42)
	defer L.Close()

	script := `
local co = coroutine.create(function ()
	coroutine.yield(math.random())
	coroutine.yield(math.random(5))
end)

local ok1, first = coroutine.resume(co)
local middle = math.random(-10, 10)
local ok3, second = coroutine.resume(co)
assert(ok1 and ok3)
return first, middle, second
`
	if err := L.DoString(script); err != nil {
		t.Fatal(err)
	}

	threadValues := []LNumber{
		L.Get(-3).(LNumber),
		L.Get(-2).(LNumber),
		L.Get(-1).(LNumber),
	}
	L.SetTop(0)

	setMathRandomSeed(t, L, 42)
	alone := []LNumber{
		mathRandomValues(t, L)[0],
		mathRandomValues(t, L, LNumber(-10), LNumber(10))[0],
		mathRandomValues(t, L, LNumber(5))[0],
	}
	for i := range threadValues {
		if threadValues[i] != alone[i] {
			t.Fatalf("shared thread value %d = %v, want %v", i, threadValues[i], alone[i])
		}
	}
}

type recordingRandomSource struct {
	values []int64
	calls  int
	seeds  []int64
}

func (source *recordingRandomSource) Int63() int64 {
	value := source.values[source.calls%len(source.values)]
	source.calls++
	return value & (1<<63 - 1)
}

func (source *recordingRandomSource) Seed(seed int64) {
	source.seeds = append(source.seeds, seed)
}

func TestMathRandomCustomSource(t *testing.T) {
	source := &recordingRandomSource{values: []int64{1<<62 - 1, 7, 12345}}
	L := NewState(Options{
		RandomSource: source,
		RandomSeed:   ptrInt64(99),
	})
	defer L.Close()

	value := mathRandomValues(t, L)[0]
	if value != LNumber(float64(source.values[0])/(1<<63)) {
		t.Fatalf("custom float = %v", value)
	}
	if len(source.seeds) != 1 || source.seeds[0] != 99 {
		t.Fatalf("initial seeds = %v, want [99]", source.seeds)
	}

	setMathRandomSeed(t, L, 44)
	if len(source.seeds) != 2 || source.seeds[1] != 44 {
		t.Fatalf("seeds = %v, want [99 44]", source.seeds)
	}
}

func TestMathRandomIntervals(t *testing.T) {
	L := newSeededMathState(t, 11)
	defer L.Close()

	cases := []struct {
		args []LValue
		min  LNumber
		max  LNumber
	}{
		{[]LValue{LNumber(1)}, 1, 1},
		{[]LValue{LNumber(5)}, 1, 5},
		{[]LValue{LNumber(-10), LNumber(0)}, -10, 0},
		{[]LValue{LNumber(-10), LNumber(10)}, -10, 10},
		{[]LValue{LNumber(-1024), LNumber(9223372036854774784)}, -1024, 9223372036854774784},
		{[]LValue{LNumber(0), LNumber(9223372036854774784)}, 0, 9223372036854774784},
	}

	for _, tc := range cases {
		for i := 0; i < 1000; i++ {
			value := mathRandomValues(t, L, tc.args...)[0]
			if value < tc.min || value > tc.max {
				t.Fatalf("random(%v) = %v outside [%v, %v]", tc.args, value, tc.min, tc.max)
			}
		}
	}

	for _, args := range [][]LValue{
		{LNumber(0)},
		{LNumber(-1)},
		{LNumber(5), LNumber(4)},
		{LNumber(math.NaN())},
		{LNumber(math.Inf(1))},
		{LNumber(9223372036854775808)},
	} {
		mathTable := L.GetGlobal("math").(*LTable)
		random := L.GetField(mathTable, "random").(*LFunction)
		err := L.CallByParam(P{Fn: random, NRet: 1, Protect: true}, args...)
		if err == nil {
			t.Fatalf("random(%v) unexpectedly succeeded", args)
		}
		L.SetTop(0)
	}
}

func TestStateRandomIntervalFullInt64(t *testing.T) {
	random := newStateRandom(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		value := random.interval(math.MinInt64, math.MaxInt64)
		if value < math.MinInt64 || value > math.MaxInt64 {
			t.Fatalf("full interval value %d out of range", value)
		}
	}
}

func ptrInt64(value int64) *int64 {
	return &value
}
