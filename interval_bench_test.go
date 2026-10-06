package btree

import (
	"strconv"
	"testing"
)

// BenchmarkIntervalPointQuery measures overlap lookups among many short
// disjoint intervals: max-end augmentation lets the traversal skip whole
// subtrees instead of scanning from the start point.
func BenchmarkIntervalPointQuery(b *testing.B) {
	for _, n := range []int{1000, 100000} {
		tr := NewIntervalTreeG[int, int](lessInt, lessInt, 32)
		for i := 0; i < n; i++ {
			tr.Set(toInterval(rec{start: 2 * i, end: 2*i + 1,
				lc: true, rc: false, id: i + 1}))
		}
		q := toInterval(rec{start: 2 * (n - 1), end: 2 * (n - 1),
			lc: true, rc: true})
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			var sink int
			for i := 0; i < b.N; i++ {
				tr.Overlaps(q, func(iv Interval[int, int]) bool {
					sink = iv.ID
					return true
				})
			}
			_ = sink
		})
	}
}
