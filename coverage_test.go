package netipx

import (
	"fmt"
	"math/rand"
	"net/netip"
	"reflect"
	"testing"
)

func ci(r string, count int) CoverageInterval {
	return CoverageInterval{MustParseIPRange(r), count}
}

func TestCoverage(t *testing.T) {
	dup := mustIPSet("+10.0.0.0-10.0.0.10")
	tests := []struct {
		name string
		min  int
		sets []*IPSet
		want []CoverageInterval
	}{
		{
			name: "no sets",
			want: nil,
		},
		{
			name: "nil set",
			sets: []*IPSet{nil},
			want: nil,
		},
		{
			name: "empty set",
			sets: []*IPSet{mustIPSet()},
			want: nil,
		},
		{
			name: "single set",
			sets: []*IPSet{mustIPSet("+10.0.0.0-10.0.0.10")},
			want: []CoverageInterval{ci("10.0.0.0-10.0.0.10", 1)},
		},
		{
			name: "overlap",
			sets: []*IPSet{
				mustIPSet("+10.0.0.0-10.0.0.10"),
				mustIPSet("+10.0.0.5-10.0.0.20"),
			},
			want: []CoverageInterval{
				ci("10.0.0.0-10.0.0.4", 1),
				ci("10.0.0.5-10.0.0.10", 2),
				ci("10.0.0.11-10.0.0.20", 1),
			},
		},
		{
			name: "shared endpoint",
			sets: []*IPSet{
				mustIPSet("+10.0.0.10-10.0.0.20"),
				mustIPSet("+10.0.0.20-10.0.0.30"),
			},
			want: []CoverageInterval{
				ci("10.0.0.10-10.0.0.19", 1),
				ci("10.0.0.20-10.0.0.20", 2),
				ci("10.0.0.21-10.0.0.30", 1),
			},
		},
		{
			name: "adjacent same depth merges",
			sets: []*IPSet{
				mustIPSet("+10.0.0.10-10.0.0.20"),
				mustIPSet("+10.0.0.21-10.0.0.30"),
			},
			want: []CoverageInterval{ci("10.0.0.10-10.0.0.30", 1)},
		},
		{
			name: "single addresses",
			sets: []*IPSet{
				mustIPSet("+10.0.0.1-10.0.0.1"),
				mustIPSet("+10.0.0.1-10.0.0.2"),
			},
			want: []CoverageInterval{
				ci("10.0.0.1-10.0.0.1", 2),
				ci("10.0.0.2-10.0.0.2", 1),
			},
		},
		{
			name: "same pointer twice counts twice",
			sets: []*IPSet{dup, dup},
			want: []CoverageInterval{ci("10.0.0.0-10.0.0.10", 2)},
		},
		{
			name: "equal sets",
			sets: []*IPSet{
				mustIPSet("+10.0.0.0-10.0.0.10"),
				mustIPSet("+10.0.0.0-10.0.0.10"),
				mustIPSet("+10.0.0.0-10.0.0.10"),
			},
			want: []CoverageInterval{ci("10.0.0.0-10.0.0.10", 3)},
		},
		{
			name: "ipv4 all",
			sets: []*IPSet{mustIPSet("+0.0.0.0-255.255.255.255")},
			want: []CoverageInterval{ci("0.0.0.0-255.255.255.255", 1)},
		},
		{
			name: "ipv6 all",
			sets: []*IPSet{mustIPSet("+::-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")},
			want: []CoverageInterval{ci("::-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", 1)},
		},
		{
			name: "family boundary does not leak",
			sets: []*IPSet{
				mustIPSet("+255.255.255.0-255.255.255.255"),
				mustIPSet("+::-::1"),
			},
			want: []CoverageInterval{
				ci("255.255.255.0-255.255.255.255", 1),
				ci("::-::1", 1),
			},
		},
		{
			name: "families sorted v4 first",
			sets: []*IPSet{
				mustIPSet("+::-::1", "+10.0.0.0-10.0.0.1"),
			},
			want: []CoverageInterval{
				ci("10.0.0.0-10.0.0.1", 1),
				ci("::-::1", 1),
			},
		},
		{
			name: "min count keeps hole",
			min:  2,
			sets: []*IPSet{
				mustIPSet("+10.0.0.0-10.0.0.10"),
				mustIPSet("+10.0.0.0-10.0.0.4", "+10.0.0.6-10.0.0.10"),
			},
			want: []CoverageInterval{
				ci("10.0.0.0-10.0.0.4", 2),
				ci("10.0.0.6-10.0.0.10", 2),
			},
		},
		{
			name: "min count three",
			min:  3,
			sets: []*IPSet{
				mustIPSet("+10.0.0.0-10.0.0.10"),
				mustIPSet("+10.0.0.2-10.0.0.8"),
				mustIPSet("+10.0.0.4-10.0.0.6"),
			},
			want: []CoverageInterval{ci("10.0.0.4-10.0.0.6", 3)},
		},
		{
			name: "min count zero is one",
			min:  0,
			sets: []*IPSet{mustIPSet("+10.0.0.0-10.0.0.10")},
			want: []CoverageInterval{ci("10.0.0.0-10.0.0.10", 1)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			min := tt.min
			if min == 0 {
				min = 1
			}
			got := CoverageMin(tt.min, tt.sets...)
			if !equalCoverage(got, tt.want) {
				t.Errorf("CoverageMin(%d) = %v, want %v", tt.min, got, tt.want)
			}
			if tt.min <= 1 {
				if got := Coverage(tt.sets...); !equalCoverage(got, tt.want) {
					t.Errorf("Coverage() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func equalCoverage(a, b []CoverageInterval) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// naiveCoverage computes coverage by visiting every address in
// [from, to]. It is only usable for tiny address windows.
func naiveCoverage(from, to netip.Addr, minCount int, sets ...*IPSet) []CoverageInterval {
	if minCount < 1 {
		minCount = 1
	}
	var out []CoverageInterval
	depth := 0
	start := from
	close := func(end netip.Addr) {
		if depth > 0 && depth >= minCount {
			out = append(out, CoverageInterval{IPRangeFrom(start, end), depth})
		}
	}
	for ip := from; ; ip = ip.Next() {
		d := 0
		for _, s := range sets {
			if s.Contains(ip) {
				d++
			}
		}
		if d != depth {
			close(ip.Prev())
			depth = d
			start = ip
		}
		if ip == to {
			break
		}
	}
	close(to)
	return out
}

func TestCoverageAgainstNaive(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		v6 := r.Intn(2) == 0
		from, to := mustIP("10.0.0.0"), mustIP("10.0.0.255")
		if v6 {
			from, to = mustIP("::"), mustIP("::ff")
		}
		addr := func(n int) netip.Addr {
			if v6 {
				return netip.AddrFrom16([16]byte{15: byte(n)})
			}
			return netip.AddrFrom4([4]byte{10, 0, 0, byte(n)})
		}
		numSets := 1 + r.Intn(4)
		sets := make([]*IPSet, numSets)
		for j := range sets {
			var b IPSetBuilder
			for k := 0; k < r.Intn(5); k++ {
				lo := r.Intn(256)
				hi := lo + r.Intn(256-lo)
				b.AddRange(IPRangeFrom(addr(lo), addr(hi)))
			}
			sets[j] = buildIPSet(&b)
		}
		min := 1 + r.Intn(3)
		got := CoverageMin(min, sets...)
		want := naiveCoverage(from, to, min, sets...)
		if !equalCoverage(got, want) {
			t.Fatalf("iteration %d (v6=%v, min=%d):\n got %v\nwant %v", i, v6, min, got, want)
		}
	}
}

var sinkCoverage []CoverageInterval

// benchSets builds numSets sets of rangesPerSet random IPv6 prefixes,
// each spanning 2^spanBits addresses.
func benchSets(r *rand.Rand, numSets, rangesPerSet, spanBits int) []*IPSet {
	sets := make([]*IPSet, numSets)
	for i := range sets {
		var b IPSetBuilder
		for j := 0; j < rangesPerSet; j++ {
			var a [16]byte
			r.Read(a[:])
			b.AddPrefix(netip.PrefixFrom(netip.AddrFrom16(a), 128-spanBits).Masked())
		}
		sets[i] = buildIPSet(&b)
	}
	return sets
}

// BenchmarkCoverage scales the number of input ranges. The ranges
// span enormous swaths of the IPv6 space, so the running time cannot
// be related to the number of covered addresses.
func BenchmarkCoverage(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 10000} {
		sets := benchSets(rand.New(rand.NewSource(1)), 3, n, 64)
		b.Run(fmt.Sprintf("ranges=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sinkCoverage = Coverage(sets...)
			}
		})
	}
}

// BenchmarkCoverageAddressSpan holds the range count fixed and varies
// the number of addresses per range, showing that the work depends on
// the number of ranges, not addresses.
func BenchmarkCoverageAddressSpan(b *testing.B) {
	for _, spanBits := range []int{8, 32, 64, 96} {
		sets := benchSets(rand.New(rand.NewSource(1)), 3, 100, spanBits)
		b.Run(fmt.Sprintf("span=2^%d", spanBits), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sinkCoverage = Coverage(sets...)
			}
		})
	}
}
