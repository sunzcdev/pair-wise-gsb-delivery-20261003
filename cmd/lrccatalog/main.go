// Command lrccatalog enumerates every failure combination of a small
// locally repairable layout and prints which combinations are recoverable,
// which tier serves them, and how many shard reads they cost versus a flat
// (k, g) MDS Reed-Solomon stripe.
//
// It is a documentation/analysis helper, not part of the library API.
package main

import (
	"fmt"
	"os"

	"github.com/klauspost/reedsolomon"
)

func main() {
	catalog(mustLayout(6, 2, 2))
	catalog(mustLayout(12, 3, 2))
}

func mustLayout(data, groups, globals int) reedsolomon.LRCLayoutParams {
	p, err := reedsolomon.NewUniformLRC(data, groups, globals)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return p
}

func catalog(p reedsolomon.LRCLayoutParams) {
	codec, err := reedsolomon.NewLRCCodec(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	names := labels(p)
	n := p.TotalShards()

	fmt.Printf("\n=== %s  total shards=%d  storage overhead=%.3fx ===\n",
		p, n, p.StorageOverhead())
	fmt.Printf("flat MDS (%d,%d) always reads %d shards for any reconstruction\n\n",
		p.DataShards, p.GlobalParityShards, p.DataShards)

	var local, global, unrecoverable int
	// Enumerate by popcount so the output is grouped by number of losses.
	for losses := 1; losses <= n; losses++ {
		var locRows, globRows, deadRows [][]int
		locCnt, globCnt, deadCnt := 0, 0, 0
		eachSubset(n, losses, func(kill []int) {
			present := make([]bool, n)
			for i := range present {
				present[i] = true
			}
			for _, idx := range kill {
				present[idx] = false
			}
			plan, err := codec.PlanFromPresent(present)
			if err != nil {
				unrecoverable++
				deadCnt++
				if losses <= 3 {
					deadRows = append(deadRows, append([]int(nil), kill...))
				}
				return
			}
			switch plan.Strategy {
			case reedsolomon.RepairLocal:
				local++
				locCnt++
				if losses <= 2 {
					locRows = append(locRows, append([]int(nil), kill...))
				}
			case reedsolomon.RepairGlobal:
				global++
				globCnt++
				if losses <= 3 {
					globRows = append(globRows, append([]int(nil), kill...))
				}
			}
		})
		fmt.Printf("losses=%d: local=%d global=%d unrecoverable=%d\n",
			losses, locCnt, globCnt, deadCnt)
		emit := func(title string, rows [][]int, withReads bool) {
			if len(rows) == 0 {
				return
			}
			fmt.Printf("  %s:\n", title)
			for _, kill := range rows {
				present := make([]bool, n)
				for i := range present {
					present[i] = true
				}
				for _, idx := range kill {
					present[idx] = false
				}
				plan, _ := codec.PlanFromPresent(present)
				line := "    " + joinNames(kill, names)
				if withReads {
					line += fmt.Sprintf("  -> reads=%d (MDS=%d)", plan.ReadCount, plan.MDSReadCount)
				}
				fmt.Println(line)
			}
		}
		if losses <= 2 {
			emit("local examples", locRows, true)
		}
		if losses <= 3 {
			emit("global examples (first shown)", first(globRows, 8), true)
			emit("unrecoverable examples", first(deadRows, 8), false)
		}
	}
	fmt.Printf("TOTALS over %d non-empty failure subsets: local=%d global=%d unrecoverable=%d\n",
		(1<<n)-1, local, global, unrecoverable)
}

func first(rows [][]int, n int) [][]int {
	if len(rows) <= n {
		return rows
	}
	return rows[:n]
}

func labels(p reedsolomon.LRCLayoutParams) []string {
	names := make([]string, p.TotalShards())
	for i := 0; i < p.DataShards; i++ {
		names[i] = fmt.Sprintf("D%d", i)
	}
	for g := range p.GroupSizes {
		idx, _ := p.LocalParityIndex(g)
		names[idx] = fmt.Sprintf("L%d", g)
	}
	for j := 0; j < p.GlobalParityShards; j++ {
		idx, _ := p.GlobalParityIndex(j)
		names[idx] = fmt.Sprintf("G%d", j)
	}
	return names
}

func joinNames(idx []int, names []string) string {
	s := "{"
	for i, x := range idx {
		if i > 0 {
			s += ","
		}
		s += names[x]
	}
	return s + "}"
}

// eachSubset invokes fn with every combination of size r from [0,n).
func eachSubset(n, r int, fn func([]int)) {
	comb := make([]int, r)
	var rec func(start, depth int)
	rec = func(start, depth int) {
		if depth == r {
			fn(append([]int(nil), comb...))
			return
		}
		for i := start; i <= n-(r-depth); i++ {
			comb[depth] = i
			rec(i+1, depth+1)
		}
	}
	rec(0, 0)
}
