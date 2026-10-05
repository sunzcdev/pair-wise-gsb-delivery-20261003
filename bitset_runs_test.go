package bitset

import (
	"fmt"
	"reflect"
	"testing"
)

type runInterval struct {
	start uint
	end   uint
}

func collectNext(c RunCursor) []runInterval {
	var runs []runInterval
	for start, end, ok := c.Next(); ok; start, end, ok = c.Next() {
		runs = append(runs, runInterval{start, end})
	}
	return runs
}

func collectPrev(c RunCursor) []runInterval {
	var runs []runInterval
	for start, end, ok := c.Prev(); ok; start, end, ok = c.Prev() {
		runs = append(runs, runInterval{start, end})
	}
	return runs
}

func reversedRuns(runs []runInterval) []runInterval {
	out := make([]runInterval, len(runs))
	for i, r := range runs {
		out[len(runs)-1-i] = r
	}
	return out
}

func TestRunsEmptyBitSet(t *testing.T) {
	var b BitSet
	if runs := collectNext(b.Runs()); len(runs) != 0 {
		t.Errorf("expected no set runs, got %v", runs)
	}
	if runs := collectNext(b.ClearRuns()); len(runs) != 0 {
		t.Errorf("expected no clear runs, got %v", runs)
	}
}

func TestRunsAllZero(t *testing.T) {
	b := New(200)
	if runs := collectNext(b.Runs()); len(runs) != 0 {
		t.Errorf("expected no set runs, got %v", runs)
	}
	want := []runInterval{{0, 200}}
	if runs := collectNext(b.ClearRuns()); !reflect.DeepEqual(runs, want) {
		t.Errorf("expected %v, got %v", want, runs)
	}
	if runs := collectPrev(b.ClearRuns()); !reflect.DeepEqual(runs, want) {
		t.Errorf("backward: expected %v, got %v", want, runs)
	}
}

func TestRunsAllOne(t *testing.T) {
	b := New(200)
	for i := uint(0); i < 200; i++ {
		b.Set(i)
	}
	want := []runInterval{{0, 200}}
	if runs := collectNext(b.Runs()); !reflect.DeepEqual(runs, want) {
		t.Errorf("expected %v, got %v", want, runs)
	}
	if runs := collectPrev(b.Runs()); !reflect.DeepEqual(runs, want) {
		t.Errorf("backward: expected %v, got %v", want, runs)
	}
	if runs := collectNext(b.ClearRuns()); len(runs) != 0 {
		t.Errorf("expected no clear runs, got %v", runs)
	}
}

func TestRunsPartialLastWord(t *testing.T) {
	// length 100: the last word holds only bits 64..99
	b := New(100)
	for i := uint(95); i < 100; i++ {
		b.Set(i)
	}
	want := []runInterval{{95, 100}}
	if runs := collectNext(b.Runs()); !reflect.DeepEqual(runs, want) {
		t.Errorf("expected %v, got %v", want, runs)
	}
	wantClear := []runInterval{{0, 95}}
	if runs := collectNext(b.ClearRuns()); !reflect.DeepEqual(runs, wantClear) {
		t.Errorf("expected %v, got %v", wantClear, runs)
	}

	// stray bits beyond the logical length must never be reported
	raw := FromWithLength(100, []uint64{allBits, allBits})
	if runs := collectNext(raw.Runs()); !reflect.DeepEqual(runs, []runInterval{{0, 100}}) {
		t.Errorf("runs must be clipped to length, got %v", runs)
	}
	if runs := collectNext(raw.ClearRuns()); len(runs) != 0 {
		t.Errorf("expected no clear runs within length, got %v", runs)
	}
}

func TestRunsForwardBackwardAreReverse(t *testing.T) {
	b := New(1000)
	// runs of various sizes, crossing word boundaries, with empty words in between
	for _, r := range [][2]uint{{0, 3}, {60, 70}, {64, 128}, {129, 130}, {300, 301}, {500, 640}, {990, 1000}} {
		for i := r[0]; i < r[1]; i++ {
			b.Set(i)
		}
	}

	forward := collectNext(b.Runs())
	c := b.Runs()
	c.SeekBackward(b.Len() - 1)
	backward := collectPrev(c)
	if !reflect.DeepEqual(forward, reversedRuns(backward)) {
		t.Errorf("forward %v != reverse of backward %v", forward, backward)
	}

	forwardClear := collectNext(b.ClearRuns())
	cc := b.ClearRuns()
	cc.SeekBackward(b.Len() - 1)
	backwardClear := collectPrev(cc)
	if !reflect.DeepEqual(forwardClear, reversedRuns(backwardClear)) {
		t.Errorf("clear runs: forward %v != reverse of backward %v", forwardClear, backwardClear)
	}
}

func TestRunsSeekMidRun(t *testing.T) {
	b := New(300)
	for i := uint(10); i < 50; i++ {
		b.Set(i)
	}
	for i := uint(100); i < 200; i++ {
		b.Set(i)
	}

	// forward from inside a run: only the in-range part is returned
	c := b.Runs()
	c.SeekForward(20)
	start, end, ok := c.Next()
	if !ok || start != 20 || end != 50 {
		t.Errorf("expected [20,50), got [%d,%d) ok=%v", start, end, ok)
	}

	// backward from inside a run: only the in-range part is returned
	c.SeekBackward(30)
	start, end, ok = c.Prev()
	if !ok || start != 10 || end != 31 {
		t.Errorf("expected [10,31), got [%d,%d) ok=%v", start, end, ok)
	}

	// forward from a run boundary skips to the next run
	c.SeekForward(50)
	start, end, ok = c.Next()
	if !ok || start != 100 || end != 200 {
		t.Errorf("expected [100,200), got [%d,%d) ok=%v", start, end, ok)
	}

	// backward from just before a run skips to the previous run
	c.SeekBackward(99)
	start, end, ok = c.Prev()
	if !ok || start != 10 || end != 50 {
		t.Errorf("expected [10,50), got [%d,%d) ok=%v", start, end, ok)
	}

	// seeks outside the logical length find nothing
	c.SeekForward(300)
	if _, _, ok = c.Next(); ok {
		t.Error("expected no run past the logical length")
	}
	c.SeekBackward(10000)
	if _, _, ok = c.Prev(); !ok {
		t.Error("expected seek past the end to cover the whole bitset")
	}
}

func TestRunsSkipsEmptyWords(t *testing.T) {
	var b BitSet
	b.Set(0).Set(1).Set(100000).Set(100001)
	want := []runInterval{{0, 2}, {100000, 100002}}
	if runs := collectNext(b.Runs()); !reflect.DeepEqual(runs, want) {
		t.Errorf("expected %v, got %v", want, runs)
	}
}

func TestRunsNextPrevMeetInMiddle(t *testing.T) {
	b := New(500)
	for _, r := range [][2]uint{{0, 5}, {64, 100}, {200, 260}, {400, 500}} {
		for i := r[0]; i < r[1]; i++ {
			b.Set(i)
		}
	}
	c := b.Runs()
	var interleaved []runInterval
	for {
		start, end, ok := c.Next()
		if !ok {
			break
		}
		interleaved = append(interleaved, runInterval{start, end})
		start, end, ok = c.Prev()
		if !ok {
			break
		}
		interleaved = append(interleaved, runInterval{start, end})
	}
	want := []runInterval{{0, 5}, {400, 500}, {64, 100}, {200, 260}}
	if !reflect.DeepEqual(interleaved, want) {
		t.Errorf("expected %v, got %v", want, interleaved)
	}
}

func TestRunsMutationDuringIteration(t *testing.T) {
	b := New(300)
	for i := uint(10); i < 20; i++ {
		b.Set(i)
	}
	c := b.Runs()
	start, end, ok := c.Next()
	if !ok || start != 10 || end != 20 {
		t.Fatalf("expected [10,20), got [%d,%d) ok=%v", start, end, ok)
	}

	// no snapshot: bits set after cursor creation are observed
	for i := uint(30); i < 40; i++ {
		b.Set(i)
	}
	start, end, ok = c.Next()
	if !ok || start != 30 || end != 40 {
		t.Fatalf("expected [30,40), got [%d,%d) ok=%v", start, end, ok)
	}

	// shrinking the bitset clips the cursor range to the new length
	for i := uint(100); i < 200; i++ {
		b.Set(i)
	}
	b.Shrink(149) // new length is 150
	start, end, ok = c.Next()
	if !ok || start != 100 || end != 150 {
		t.Fatalf("expected [100,150), got [%d,%d) ok=%v", start, end, ok)
	}
	if _, _, ok = c.Next(); ok {
		t.Fatal("expected cursor to be exhausted at the new length")
	}
}

func TestClearRunsSeekMidRun(t *testing.T) {
	b := New(300)
	for i := uint(0); i < 300; i++ {
		b.Set(i)
	}
	for i := uint(100); i < 200; i++ {
		b.Clear(i)
	}

	c := b.ClearRuns()
	c.SeekForward(150)
	start, end, ok := c.Next()
	if !ok || start != 150 || end != 200 {
		t.Errorf("expected [150,200), got [%d,%d) ok=%v", start, end, ok)
	}

	c.SeekBackward(150)
	start, end, ok = c.Prev()
	if !ok || start != 100 || end != 151 {
		t.Errorf("expected [100,151), got [%d,%d) ok=%v", start, end, ok)
	}
}

func ExampleBitSet_Runs() {
	b := New(130)
	b.Set(2).Set(3).Set(4).Set(65).Set(66).Set(129)

	c := b.Runs()
	for start, end, ok := c.Next(); ok; start, end, ok = c.Next() {
		fmt.Printf("[%d, %d)\n", start, end)
	}
	// Output:
	// [2, 5)
	// [65, 67)
	// [129, 130)
}
