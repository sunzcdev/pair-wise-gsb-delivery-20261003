package lua

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

// callRandom invokes the Lua math.random function on the given state from
// Go and returns its result.
func callRandom(t *testing.T, L *LState, args ...LValue) LValue {
	t.Helper()
	mathMod := L.GetGlobal("math").(*LTable)
	random := mathMod.RawGetString("random")
	L.Push(random)
	for _, a := range args {
		L.Push(a)
	}
	if err := L.PCall(len(args), 1, nil); err != nil {
		t.Fatalf("math.random failed: %v", err)
	}
	v := L.Get(-1)
	L.Pop(1)
	return v
}

func toInt(t *testing.T, v LValue) int64 {
	t.Helper()
	n, ok := v.(LNumber)
	if !ok {
		t.Fatalf("number expected, got %v", v.Type())
	}
	return int64(n)
}

// Independent NewStates seeded identically must replay the same sequence,
// and seeding one state must not affect another.
func TestMathRandomStateIsolation(t *testing.T) {
	const n = 32

	a := NewState()
	defer a.Close()
	b := NewState()
	defer b.Close()
	c := NewState()
	defer c.Close()

	a.SetRandomSeed(1234)
	b.SetRandomSeed(1234)
	c.SetRandomSeed(4321)

	for i := 0; i < n; i++ {
		va := callRandom(t, a)
		vb := callRandom(t, b)
		vc := callRandom(t, c)
		if va != vb {
			t.Fatalf("identically seeded states diverged at %v: %v != %v", i, va, vb)
		}
		if va == vc {
			// Extremely unlikely for every draw, but only fail if they
			// match for the entire stream.
			if i == n-1 {
				t.Fatalf("differently seeded states produced identical streams")
			}
		}
	}

	// Re-seeding state a must leave b's sequence untouched. After the loop
	// b is n draws ahead of its seed; align a reference the same way and
	// confirm b's next draw is unaffected by re-seeding state a.
	bRef := rand.New(rand.NewSource(1234))
	for i := 0; i < n; i++ {
		bRef.Float64()
	}
	wantNext := LNumber(bRef.Float64())
	a.SetRandomSeed(1234)
	if got := callRandom(t, b); got != wantNext {
		t.Fatalf("seeding another state changed this state's sequence")
	}
}

// Interleaving two independent states must produce the same per-state
// sequences as running each state on its own.
func TestMathRandomInterleavedStates(t *testing.T) {
	const n = 24

	solo := NewState()
	defer solo.Close()
	solo.SetRandomSeed(55)
	soloSeq := make([]LValue, n)
	for i := range soloSeq {
		soloSeq[i] = callRandom(t, solo)
	}

	x := NewState()
	defer x.Close()
	y := NewState()
	defer y.Close()
	x.SetRandomSeed(55)
	y.SetRandomSeed(999)

	for i := 0; i < n; i++ {
		var got LValue
		if i%2 == 0 {
			got = callRandom(t, x)
			callRandom(t, y) // perturb the other state
		} else {
			callRandom(t, y)
			got = callRandom(t, x)
		}
		if got != soloSeq[i] {
			t.Fatalf("interleaved sequence diverged at %v: %v != %v", i, got, soloSeq[i])
		}
	}
}

// A coroutine created from a main state shares the main state's global
// state and therefore its random sequence. Interleaving the two threads
// draws one continuous sequence identical to a single reference source.
func TestMathRandomCoroutineSharesSource(t *testing.T) {
	const seed int64 = 20260101

	L := NewState()
	defer L.Close()
	th, cancel := L.NewThread()
	if cancel != nil {
		defer cancel()
	}
	defer th.Close()

	L.SetRandomSeed(seed)
	ref := rand.New(rand.NewSource(seed))

	checkFloat := func(state *LState) {
		got := float64(callRandom(t, state).(LNumber))
		want := ref.Float64()
		if got != want {
			t.Fatalf("coroutine/main float mismatch: got %v want %v", got, want)
		}
	}
	checkInt := func(state *LState, args ...LValue) {
		got := toInt(t, callRandom(t, state, args...))
		want := int64(randUint64n(ref, 100)) + 1
		if got != want {
			t.Fatalf("coroutine/main int mismatch: got %v want %v", got, want)
		}
	}

	for i := 0; i < 16; i++ {
		if i%2 == 0 {
			checkFloat(L)
			checkInt(th, LNumber(100))
		} else {
			checkFloat(th)
			checkInt(L, LNumber(1), LNumber(100))
		}
	}
}

// Boundary values must stay in range, including degenerate and negative
// intervals.
func TestMathRandomRanges(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.SetRandomSeed(7)

	for i := 0; i < 500; i++ {
		if v := toInt(t, callRandom(t, L, LNumber(1))); v != 1 {
			t.Fatalf("random(1) = %v, want 1", v)
		}
		if v := toInt(t, callRandom(t, L, LNumber(-5), LNumber(-5))); v != -5 {
			t.Fatalf("random(-5,-5) = %v, want -5", v)
		}
		if v := toInt(t, callRandom(t, L, LNumber(10))); v < 1 || v > 10 {
			t.Fatalf("random(10) out of range: %v", v)
		}
		if v := toInt(t, callRandom(t, L, LNumber(-3), LNumber(7))); v < -3 || v > 7 {
			t.Fatalf("random(-3,7) out of range: %v", v)
		}
	}

	// A large span that exceeds MaxInt32 but fits in int64 must not panic
	// and must stay within bounds.
	const (
		lo = -(1 << 40)
		hi = (1 << 40) - 1
	)
	for i := 0; i < 200; i++ {
		v := toInt(t, callRandom(t, L, LNumber(lo), LNumber(hi)))
		if v < lo || v > hi {
			t.Fatalf("large range out of bounds: %v", v)
		}
	}

	// A large, exactly-float-representable span near the int64 limits must
	// not panic and must stay in bounds when driven through the Lua entry.
	for i := 0; i < 200; i++ {
		if err := L.DoString(`
			local v = math.random(-9223372036854775808, 9223372036854774784)
			assert(v >= -9223372036854775808 and v <= 9223372036854774784)
		`); err != nil {
			t.Fatalf("near-full-range math.random raised an error: %v", err)
		}
	}

	// The true full int64 domain cannot be reached via float64 arguments
	// (MaxInt64 rounds to +Inf and converts back to MinInt64), so verify the
	// overflow-safe helper directly: it must not panic and must cover both
	// halves of the domain.
	seenNeg, seenPos := false, false
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		v := randomInt64n(r, math.MinInt64, math.MinInt64) // wrapped hi+1
		if v < 0 {
			seenNeg = true
		} else {
			seenPos = true
		}
	}
	if !seenNeg || !seenPos {
		t.Fatalf("full-domain draw did not cover both halves: neg=%v pos=%v", seenNeg, seenPos)
	}

	// Sanity check for modulo bias: every value of a tiny interval appears.
	counts := map[int64]int{}
	for i := 0; i < 3000; i++ {
		counts[toInt(t, callRandom(t, L, LNumber(1), LNumber(3)))]++
	}
	for _, v := range []int64{1, 2, 3} {
		if counts[v] == 0 {
			t.Fatalf("value %v never produced (modulo bias?): %v", v, counts)
		}
	}
}

// Empty / inverted intervals raise a Lua error instead of panicking Go.
func TestMathRandomEmptyIntervals(t *testing.T) {
	L := NewState()
	defer L.Close()

	for _, script := range []string{
		`math.random(0)`,
		`math.random(-1)`,
		`math.random(5, 2)`,
	} {
		errorIfScriptNotFail(t, L, script, "interval is empty")
	}

	// MaxInt64 is not representable as a Lua number; a +Inf upper bound
	// converts to MinInt64, inverting the range against a finite lower
	// bound. This must raise a Lua error instead of panicking. Push +Inf
	// directly to avoid relying on the source parser's literal rounding.
	L.Push(L.NewFunction(mathRandom))
	L.Push(LNumber(1))
	L.Push(LNumber(math.Inf(1)))
	err := L.PCall(2, 1, nil)
	if err == nil {
		t.Fatal("math.random with a +Inf upper bound should raise an error")
	}
	if !strings.Contains(err.Error(), "interval is empty") {
		t.Fatalf("expected 'interval is empty', got %v", err.Error())
	}
}

// Host-injected sources drive math.random for replay.
func TestMathRandomSetSource(t *testing.T) {
	L := NewState()
	defer L.Close()

	L.SetRandomSource(rand.New(rand.NewSource(321)))
	ref := rand.New(rand.NewSource(321))

	for i := 0; i < 16; i++ {
		got := float64(callRandom(t, L).(LNumber))
		if want := ref.Float64(); got != want {
			t.Fatalf("injected source diverged at %v: %v != %v", i, got, want)
		}
	}

	// math.randomseed replaces an injected source, still deterministically.
	L.SetRandomSeed(9)
	ref2 := rand.New(rand.NewSource(9))
	if got, want := float64(callRandom(t, L).(LNumber)), ref2.Float64(); got != want {
		t.Fatalf("reseed after injection diverged: %v != %v", got, want)
	}

	// A nil source is a programming error and panics rather than segfaulting.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for nil random source")
			}
		}()
		L.SetRandomSource(nil)
	}()
}

// The math.randomseed Lua entry point reseeds the state source.
func TestMathRandomseedFromLua(t *testing.T) {
	L := NewState()
	defer L.Close()

	errorIfScriptFail(t, L, `math.randomseed(424242)`)
	first := callRandom(t, L)
	errorIfScriptFail(t, L, `math.randomseed(424242)`)
	if callRandom(t, L) != first {
		t.Fatal("math.randomseed did not produce a repeatable sequence")
	}

	ref := rand.New(rand.NewSource(424242))
	if float64(first.(LNumber)) != ref.Float64() {
		t.Fatal("math.randomseed sequence does not match a matching Go source")
	}
}
