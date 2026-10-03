package bitset

import (
	"math/rand"
	"testing"
)

type runInterval struct {
	start, end uint
}

func collectForward(c *RunCursor) []runInterval {
	var runs []runInterval
	for start, end, ok := c.Next(); ok; start, end, ok = c.Next() {
		runs = append(runs, runInterval{start, end})
	}
	return runs
}

func collectBackward(c *RunCursor) []runInterval {
	var runs []runInterval
	for start, end, ok := c.Prev(); ok; start, end, ok = c.Prev() {
		runs = append(runs, runInterval{start, end})
	}
	return runs
}

func equalRuns(a, b []runInterval) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func reversedRuns(runs []runInterval) []runInterval {
	out := make([]runInterval, len(runs))
	for i, r := range runs {
		out[len(runs)-1-i] = r
	}
	return out
}

func TestRunCursorAllClear(t *testing.T) {
	b := New(130)
	if runs := collectForward(b.Runs(true)); len(runs) != 0 {
		t.Fatalf("set-bit runs of all-clear set: got %v, want none", runs)
	}
	if runs := collectBackward(b.Runs(true)); len(runs) != 0 {
		t.Fatalf("backward set-bit runs of all-clear set: got %v, want none", runs)
	}
	want := []runInterval{{0, 130}}
	if runs := collectForward(b.Runs(false)); !equalRuns(runs, want) {
		t.Fatalf("clear-bit runs of all-clear set: got %v, want %v", runs, want)
	}
	if runs := collectBackward(b.Runs(false)); !equalRuns(runs, want) {
		t.Fatalf("backward clear-bit runs of all-clear set: got %v, want %v", runs, want)
	}
}

func TestRunCursorAllSet(t *testing.T) {
	b := New(130).SetAll()
	want := []runInterval{{0, 130}}
	if runs := collectForward(b.Runs(true)); !equalRuns(runs, want) {
		t.Fatalf("set-bit runs of all-set bitset: got %v, want %v", runs, want)
	}
	if runs := collectBackward(b.Runs(true)); !equalRuns(runs, want) {
		t.Fatalf("backward set-bit runs of all-set bitset: got %v, want %v", runs, want)
	}
	if runs := collectForward(b.Runs(false)); len(runs) != 0 {
		t.Fatalf("clear-bit runs of all-set bitset: got %v, want none", runs)
	}
	if runs := collectBackward(b.Runs(false)); len(runs) != 0 {
		t.Fatalf("backward clear-bit runs of all-set bitset: got %v, want none", runs)
	}
}

func TestRunCursorEmptySet(t *testing.T) {
	var b BitSet
	if _, _, ok := b.Runs(true).Next(); ok {
		t.Fatal("empty bitset reported a set-bit run")
	}
	if _, _, ok := b.Runs(false).Next(); ok {
		t.Fatal("empty bitset reported a clear-bit run")
	}
	if _, _, ok := b.Runs(false).Prev(); ok {
		t.Fatal("empty bitset reported a backward clear-bit run")
	}
}

func TestRunCursorPartialLastWord(t *testing.T) {
	b := New(70)
	b.Set(64).Set(65).Set(66).Set(67).Set(68).Set(69)
	wantOnes := []runInterval{{64, 70}}
	if runs := collectForward(b.Runs(true)); !equalRuns(runs, wantOnes) {
		t.Fatalf("set-bit runs: got %v, want %v", runs, wantOnes)
	}
	if runs := collectBackward(b.Runs(true)); !equalRuns(runs, wantOnes) {
		t.Fatalf("backward set-bit runs: got %v, want %v", runs, wantOnes)
	}
	wantZeros := []runInterval{{0, 64}}
	if runs := collectForward(b.Runs(false)); !equalRuns(runs, wantZeros) {
		t.Fatalf("clear-bit runs: got %v, want %v", runs, wantZeros)
	}
	c := b.Runs(false)
	if _, _, ok := c.NextFrom(64); ok {
		t.Fatal("clear-bit run reported in padding bits beyond length")
	}
}

func TestRunCursorMidRunStart(t *testing.T) {
	b := New(100)
	b.SetRange(10, 50)
	start, end, ok := b.Runs(true).NextFrom(20)
	if !ok || start != 20 || end != 50 {
		t.Fatalf("NextFrom(20) inside run: got [%d,%d),%v, want [20,50),true", start, end, ok)
	}
	start, end, ok = b.Runs(true).PrevFrom(40)
	if !ok || start != 10 || end != 41 {
		t.Fatalf("PrevFrom(40) inside run: got [%d,%d),%v, want [10,41),true", start, end, ok)
	}
	start, end, ok = b.Runs(false).NextFrom(60)
	if !ok || start != 60 || end != 100 {
		t.Fatalf("clear-bit NextFrom(60): got [%d,%d),%v, want [60,100),true", start, end, ok)
	}
}

func TestRunCursorFromBoundaries(t *testing.T) {
	b := New(200)
	b.SetRange(10, 20).SetRange(64, 128).SetRange(130, 131)
	start, end, ok := b.Runs(true).NextFrom(20)
	if !ok || start != 64 || end != 128 {
		t.Fatalf("NextFrom(20): got [%d,%d),%v, want [64,128),true", start, end, ok)
	}
	start, end, ok = b.Runs(true).PrevFrom(63)
	if !ok || start != 10 || end != 20 {
		t.Fatalf("PrevFrom(63): got [%d,%d),%v, want [10,20),true", start, end, ok)
	}
	if _, _, ok := b.Runs(true).NextFrom(200); ok {
		t.Fatal("NextFrom at length reported a run")
	}
	start, end, ok = b.Runs(true).PrevFrom(1000)
	if !ok || start != 130 || end != 131 {
		t.Fatalf("PrevFrom beyond length: got [%d,%d),%v, want [130,131),true", start, end, ok)
	}
	c := b.Runs(true)
	c.NextFrom(131)
	if _, _, ok := c.Next(); ok {
		t.Fatal("Next past last run reported a run")
	}
	c.PrevFrom(9)
	if _, _, ok := c.Prev(); ok {
		t.Fatal("Prev before first run reported a run")
	}
}

func TestRunCursorForwardBackwardReverse(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for _, length := range []uint{1, 63, 64, 65, 127, 128, 200, 1000} {
		b := New(length)
		for i := uint(0); i < length; i++ {
			if rng.Intn(2) == 0 {
				b.Set(i)
			}
		}
		for _, ones := range []bool{true, false} {
			forward := collectForward(b.Runs(ones))
			backward := collectBackward(b.Runs(ones))
			if !equalRuns(forward, reversedRuns(backward)) {
				t.Fatalf("length=%d ones=%v: forward %v != reverse of backward %v",
					length, ones, forward, backward)
			}
			for _, r := range forward {
				if r.start >= r.end || r.end > length {
					t.Fatalf("length=%d ones=%v: invalid interval [%d,%d)", length, ones, r.start, r.end)
				}
			}
		}
	}
}

func TestRunCursorRunsAreMaximalAndMatchBits(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	b := New(300)
	for i := uint(0); i < 300; i++ {
		if rng.Intn(3) == 0 {
			b.Set(i)
		}
	}
	for _, ones := range []bool{true, false} {
		covered := uint(0)
		for _, r := range collectForward(b.Runs(ones)) {
			for i := r.start; i < r.end; i++ {
				if b.Test(i) != ones {
					t.Fatalf("ones=%v: bit %d inside run [%d,%d) has value %v",
						ones, i, r.start, r.end, !ones)
				}
			}
			if r.start > 0 && b.Test(r.start-1) == ones {
				t.Fatalf("ones=%v: run [%d,%d) not maximal on the left", ones, r.start, r.end)
			}
			if r.end < 300 && b.Test(r.end) == ones {
				t.Fatalf("ones=%v: run [%d,%d) not maximal on the right", ones, r.start, r.end)
			}
			covered += r.end - r.start
		}
		if ones && covered != b.Count() {
			t.Fatalf("set-bit runs cover %d bits, Count() = %d", covered, b.Count())
		}
		if !ones && covered != 300-b.Count() {
			t.Fatalf("clear-bit runs cover %d bits, want %d", covered, 300-b.Count())
		}
	}
}

func TestRunCursorLiveReads(t *testing.T) {
	b := New(100)
	b.SetRange(10, 20)
	c := b.Runs(true)
	start, end, ok := c.Next()
	if !ok || start != 10 || end != 20 {
		t.Fatalf("first run: got [%d,%d),%v, want [10,20),true", start, end, ok)
	}
	b.SetRange(30, 40)
	start, end, ok = c.Next()
	if !ok || start != 30 || end != 40 {
		t.Fatalf("run after mutation: got [%d,%d),%v, want [30,40),true", start, end, ok)
	}
	b.ClearAll()
	b.SetRange(50, 60)
	b.Clear(55)
	start, end, ok = c.Next()
	if !ok || start != 50 || end != 55 {
		t.Fatalf("run after clear: got [%d,%d),%v, want [50,55),true", start, end, ok)
	}
}
