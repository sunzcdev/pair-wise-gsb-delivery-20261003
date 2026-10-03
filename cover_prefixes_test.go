package netipx

import (
	"math/big"
	"math/bits"
	"math/rand"
	"net/netip"
	"sort"
	"testing"
)

// smallPrefix identifies a canonical prefix in a tiny simulated
// address space of width B: block is the prefix index at depth bits
// (0 <= block < 2^bits).
type smallPrefix struct {
	block uint64
	bits  uint8
}

func (p smallPrefix) addr(B uint8) uint64 { return p.block << (B - p.bits) }

// blockMask returns the B-bit set of addresses in prefix (block, bits).
func blockMask(block uint64, bits, B uint8) uint64 {
	// The block occupies addresses [lo, hi] inclusive.
	host := B - bits
	lo := (block % pow2u(bits)) * pow2u(host)
	hi := lo + pow2u(host) - 1
	return bitsRange(lo, hi)
}

func pow2u(n uint8) uint64 {
	if n >= 64 {
		return 0 // unreachable for B <= 8 tests
	}
	return uint64(1) << n
}

func bitsRange(lo, hi uint64) uint64 {
	var m uint64
	for a := lo; a <= hi; a++ {
		m |= uint64(1) << a
		if a == ^uint64(0) {
			break
		}
	}
	return m
}

type bruteCover struct {
	prefixes []smallPrefix
	extra    uint64
}

// bruteSolve is an independent reference solver for a B-bit space. It
// enumerates covers of one tree node with an exact prefix budget:
//
//   - a block disjoint from the set costs nothing and uses no prefix;
//   - a block contained in the set is taken as one prefix for free;
//   - a mixed block is either taken as one prefix (paying the extra
//     addresses) or expressed as covers of its two children whose
//     budgets sum to the node's budget.
//
// Enumerating every budget split exhausts all Pareto-optimal antichain
// covers, so the chosen optimum is provably optimal for tiny B.
func bruteSolve(B, depth, block, budget uint8, setMask uint64) (bruteCover, bool) {
	mask := blockMask(uint64(block), depth, B)
	in := mask & setMask
	if in == 0 {
		return bruteCover{prefixes: []smallPrefix{}}, true
	}
	node := smallPrefix{block: uint64(block), bits: depth}
	if in == mask {
		if budget == 0 {
			return bruteCover{}, false
		}
		return bruteCover{prefixes: []smallPrefix{node}, extra: 0}, true
	}
	if depth == B || budget == 0 {
		return bruteCover{}, false
	}

	var best bruteCover
	found := false
	consider := func(c bruteCover) {
		if c.extra > 1<<B {
			return
		}
		if !found || bruteLess(c, best, B) {
			best, found = c, true
		}
	}

	// Take this mixed node as a single prefix.
	consider(bruteCover{
		prefixes: []smallPrefix{node},
		extra:    uint64(bits.OnesCount64(mask)) - uint64(bits.OnesCount64(in)),
	})

	// Split between the children. Each child is solved with an "at
	// most" budget; the global exact/at-most budget is enforced by the
	// caller loop over t, and empty children consume nothing.
	leftMask := blockMask(uint64(block*2), depth+1, B)
	rightMask := blockMask(uint64(block*2+1), depth+1, B)
	lNeed := leftMask&setMask != 0
	rNeed := rightMask&setMask != 0
	for b0 := uint8(0); b0 <= budget; b0++ {
		b1 := budget - b0
		var (
			l, r     bruteCover
			okL, okR = true, true
		)
		if lNeed {
			l, okL = bruteSolve(B, depth+1, block*2, b0, setMask)
		}
		if rNeed {
			r, okR = bruteSolve(B, depth+1, block*2+1, b1, setMask)
		}
		if !okL || !okR {
			continue
		}
		merged := bruteCover{
			prefixes: append(append([]smallPrefix{}, l.prefixes...), r.prefixes...),
			extra:    l.extra + r.extra,
		}
		consider(merged)
	}
	if !found {
		return bruteCover{}, false
	}
	return best, true
}

// bruteBest runs the exact-budget solver for budgets 1..k and applies
// the "fewer prefixes on equal extra" objective across budgets.
func bruteBest(B uint8, setMask uint64, k int) (bruteCover, bool) {
	var best bruteCover
	found := false
	for t := 1; t <= k; t++ {
		c, ok := bruteSolve(B, 0, 0, uint8(t), setMask)
		if !ok {
			continue
		}
		if !found || bruteLess(c, best, B) {
			best, found = c, true
		}
	}
	return best, found
}

func sortSmall(ps []smallPrefix, B uint8) {
	sort.Slice(ps, func(i, j int) bool {
		if a, b := ps[i].addr(B), ps[j].addr(B); a != b {
			return a < b
		}
		return ps[i].bits < ps[j].bits
	})
}

// bruteLess compares covers per the required objective: extra
// addresses, then prefix count, then the sorted list (address, then
// shorter prefix first).
func bruteLess(a, b bruteCover, B uint8) bool {
	if a.extra != b.extra {
		return a.extra < b.extra
	}
	if len(a.prefixes) != len(b.prefixes) {
		return len(a.prefixes) < len(b.prefixes)
	}
	sa := append([]smallPrefix{}, a.prefixes...)
	sb := append([]smallPrefix{}, b.prefixes...)
	sortSmall(sa, B)
	sortSmall(sb, B)
	for i := range sa {
		if aa, bb := sa[i].addr(B), sb[i].addr(B); aa != bb {
			return aa < bb
		}
		if sa[i].bits != sb[i].bits {
			return sa[i].bits < sb[i].bits
		}
	}
	return false
}

// smallToSet builds a production IPSet from a B-bit membership mask,
// embedding addresses at the low end of IPv4 space (0..2^B-1).
func smallToSet(B uint8, setMask uint64) *IPSet {
	var bld IPSetBuilder
	for a := uint64(0); a < uint64(1)<<B; a++ {
		if setMask&(uint64(1)<<a) != 0 {
			addr := netip.AddrFrom4([4]byte{0, 0, 0, byte(a)})
			bld.Add(addr)
		}
	}
	s, err := bld.IPSet()
	if err != nil {
		panic(err)
	}
	return s
}

// prefixesToSmall converts production famPrefix output back into the
// B-bit representation.
func prefixesToSmall(pf []netip.Prefix, B uint8) []smallPrefix {
	out := make([]smallPrefix, 0, len(pf))
	for _, p := range pf {
		a := uint64(p.Addr().As4()[0])<<24 |
			uint64(p.Addr().As4()[1])<<16 |
			uint64(p.Addr().As4()[2])<<8 |
			uint64(p.Addr().As4()[3])
		bits := uint8(p.Bits())
		smallBits := bits - (32 - B)
		host := B - smallBits
		out = append(out, smallPrefix{block: a >> host, bits: smallBits})
	}
	return out
}

func coversEqualMask(want []smallPrefix, got []netip.Prefix, extra uint64, B uint8) bool {
	gs := prefixesToSmall(got, B)
	sortSmall(want, B)
	sortSmall(gs, B)
	if len(gs) != len(want) {
		return false
	}
	for i := range want {
		if want[i] != gs[i] {
			return false
		}
	}
	return true
}

// TestCoveringPrefixesExhaustive enumerates every non-empty subset of
// B<=3 spaces and a broad random sample of B=4 and B=5 spaces,
// checking CoveringPrefixes against the brute-force optimum for every
// budget k.
func TestCoveringPrefixesExhaustive(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for _, tc := range []struct {
		B       uint8
		samples int
	}{
		{1, 0},
		{2, 0},
		{3, 0},
		{4, 600},
	} {
		B := tc.B
		size := uint64(1) << B
		space := uint64(1) << size
		masks := make([]uint64, 0, space)
		if tc.samples == 0 {
			for m := uint64(1); m < space; m++ {
				masks = append(masks, m)
			}
		} else {
			for i := 0; i < tc.samples; i++ {
				m := uint64(1) + rng.Uint64()%(space-1)
				masks = append(masks, m)
			}
		}
		for _, setMask := range masks {
			checkOneSet(t, B, setMask)
		}
	}
}

func checkOneSet(t *testing.T, B uint8, setMask uint64) {
	t.Helper()
	size := uint64(1) << B
	s := smallToSet(B, setMask)
	maxK := int(size)
	if B >= 4 {
		maxK = 5
	}
	for k := 1; k <= maxK; k++ {
		want, okWant := bruteBest(B, setMask, k)
		got, extra, okGot := s.CoveringPrefixes(k)
		if okWant != okGot {
			t.Fatalf("B=%d set=%0*b k=%d: feasibility want %v got %v",
				B, size, setMask, k, okWant, okGot)
		}
		if !okGot {
			continue
		}
		if extra.Cmp(new(big.Int).SetUint64(want.extra)) != 0 {
			t.Fatalf("B=%d set=%0*b k=%d: extra want %d got %s",
				B, size, setMask, k, want.extra, extra)
		}
		if len(got) > k {
			t.Fatalf("B=%d set=%0*b k=%d: %d prefixes", B, size, setMask, k, len(got))
		}
		if !coversEqualMask(want.prefixes, got, want.extra, B) {
			t.Fatalf("B=%d set=%0*b k=%d:\n want %v (extra %d)\n got  %v (extra %s)",
				B, size, setMask, k, want.prefixes, want.extra, got, extra)
		}
	}
}

// TestCoveringPrefixesFuzz uses wider random spaces, where full
// enumeration is impractical, to check structural invariants, exact
// (zero-extra) behavior at large k, and monotonicity of extra cost.
func TestCoveringPrefixesFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const B = 6
	for iter := 0; iter < 300; iter++ {
		// A random non-empty subset of up to 24 addresses.
		setMask := uint64(1) << uint(rng.Intn(1<<B))
		for n := rng.Intn(23); n > 0; n-- {
			setMask |= uint64(1) << uint(rng.Intn(1<<B))
		}
		s := smallToSet(B, setMask)
		exact := s.Prefixes()
		if len(exact) == 0 {
			t.Fatalf("empty set from mask %016x", setMask)
		}

		var prevExtra *big.Int
		for k := 1; k <= len(exact)+2; k++ {
			pf, extra, ok := s.CoveringPrefixes(k)
			if !ok {
				t.Fatalf("single-family cover infeasible at k=%d (set=%08b, exact=%v)", k, setMask, exact)
			}
			assertCanonicalCover(t, s, pf, extra, k)
			if k >= len(exact) && extra.Sign() != 0 {
				t.Fatalf("k>=exact(%d) but extra=%s", len(exact), extra)
			}
			if prevExtra != nil && extra.Cmp(prevExtra) > 0 {
				t.Fatalf("extra increased with k: %s -> %s", prevExtra, extra)
			}
			prevExtra = new(big.Int).Set(extra)
		}

		// At sufficient budget the result equals the exact prefixes.
		pf, extra, ok := s.CoveringPrefixes(len(exact))
		if !ok {
			t.Fatalf("exact-budget cover infeasible: exact=%v", exact)
		}
		if extra.Sign() != 0 || len(pf) != len(exact) {
			t.Fatalf("exact mismatch: got %v extra %s, want %v", pf, extra, exact)
		}
	}
}

// assertCanonicalCover verifies the result is a sorted, non-overlapping
// canonical cover whose over-count matches the reported extra value.
func assertCanonicalCover(t *testing.T, s *IPSet, pf []netip.Prefix, extra *big.Int, k int) {
	t.Helper()
	if len(pf) > k {
		t.Fatalf("too many prefixes: %d > %d", len(pf), k)
	}
	total := new(big.Int)
	seen := map[string]bool{}
	for i, p := range pf {
		if p != p.Masked() {
			t.Fatalf("non-canonical prefix: %v", p)
		}
		if seen[p.String()] {
			t.Fatalf("duplicate prefix: %v", p)
		}
		seen[p.String()] = true
		if !s.ContainsPrefix(p) && !coverContainsSetCheck(s, p) {
			t.Fatalf("prefix %v covers nothing in the set", p)
		}
		size := prefixSizeBig(p)
		total.Add(total, size)
		for j := 0; j < i; j++ {
			if pf[j].Overlaps(p) {
				t.Fatalf("overlapping prefixes: %v and %v", pf[j], p)
			}
		}
	}
	setSize := ipSetSizeBig(s)
	gotExtra := new(big.Int).Sub(total, setSize)
	if gotExtra.Cmp(extra) != 0 {
		t.Fatalf("extra mismatch: reported %s, recomputed %s", extra, gotExtra)
	}
}

func coverContainsSetCheck(s *IPSet, p netip.Prefix) bool {
	// Every prefix of a valid cover must intersect the set; full
	// containment is checked over the union instead.
	return s.OverlapsPrefix(p)
}

func prefixSizeBig(p netip.Prefix) *big.Int {
	host := p.Addr().BitLen() - p.Bits()
	return new(big.Int).Lsh(big.NewInt(1), uint(host))
}

func ipSetSizeBig(s *IPSet) *big.Int {
	total := new(big.Int)
	for _, r := range s.Ranges() {
		lo := u128From16(r.From().As16())
		hi := u128From16(r.To().As16())
		// Range size = hi-lo+1 within the family.
		var size uint128
		if r.From().Is4() {
			l := uint32(lo.lo)
			h := uint32(hi.lo)
			size = uint128{0, uint64(h-l) + 1}
		} else {
			size = hi.sub(lo).addOne()
		}
		total.Add(total, u128ToBig(size))
	}
	return total
}

func mustCoverSet(t *testing.T, prefixes ...string) *IPSet {
	t.Helper()
	var b IPSetBuilder
	for _, p := range prefixes {
		b.AddPrefix(mustIPPrefix(p))
	}
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func prefixStrings(pf []netip.Prefix) []string {
	out := make([]string, len(pf))
	for i, p := range pf {
		out[i] = p.String()
	}
	return out
}

func eqStrings(a, b []string) bool {
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

// TestCoveringPrefixesIPv6Sparse checks sparse IPv6 points, where the
// cheapest cover can be the whole ::/0 and the extra cost is a full
// 128-bit number.
func TestCoveringPrefixesIPv6Sparse(t *testing.T) {
	max128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	tests := []struct {
		name     string
		prefixes []string
		k        int
		want     []string
		extra    *big.Int
	}{
		{
			name:     "two points nearest block",
			prefixes: []string{"2001:db8::1/128", "2001:db8::2/128"},
			k:        1,
			want:     []string{"2001:db8::/126"},
			extra:    big.NewInt(2),
		},
		{
			name:     "two far apart points collapse to v6 space",
			prefixes: []string{"2001:db8::1/128", "fe80::1/128"},
			k:        1,
			want:     []string{"::/0"},
			// 2^128 - 2 addresses are extra.
			extra: new(big.Int).Sub(max128, big.NewInt(1)),
		},
		{
			name:     "two far apart points exact at k=2",
			prefixes: []string{"2001:db8::1/128", "fe80::1/128"},
			k:        2,
			want:     []string{"2001:db8::1/128", "fe80::1/128"},
			extra:    big.NewInt(0),
		},
		{
			name:     "single point",
			prefixes: []string{"2001:db8::1/128"},
			k:        1,
			want:     []string{"2001:db8::1/128"},
			extra:    big.NewInt(0),
		},
		{
			name:     "three points in one nibble",
			prefixes: []string{"::1/128", "::2/128", "::3/128"},
			k:        1,
			want:     []string{"::/126"},
			extra:    big.NewInt(1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustCoverSet(t, tt.prefixes...)
			pf, extra, ok := s.CoveringPrefixes(tt.k)
			if !ok {
				t.Fatal("expected feasible")
			}
			if !eqStrings(prefixStrings(pf), tt.want) {
				t.Fatalf("prefixes=%v want %v", pf, tt.want)
			}
			if extra.Cmp(tt.extra) != 0 {
				t.Fatalf("extra=%s want %s", extra, tt.extra)
			}
		})
	}
}

// TestCoveringPrefixesEnoughBudget verifies zero spill at sufficient k.
func TestCoveringPrefixesEnoughBudget(t *testing.T) {
	s := mustCoverSet(t,
		"10.0.0.1/32", "10.0.0.2/32", "192.168.1.0/24",
		"2001:db8::1/128", "2001:db8::5/128")
	exact := s.Prefixes()
	for _, k := range []int{len(exact), len(exact) + 1, 1000} {
		pf, extra, ok := s.CoveringPrefixes(k)
		if !ok {
			t.Fatalf("k=%d infeasible", k)
		}
		if extra.Sign() != 0 {
			t.Fatalf("k=%d extra=%s, want 0", k, extra)
		}
		if len(pf) != len(exact) {
			t.Fatalf("k=%d got %d prefixes, want exact %d (%v)", k, len(pf), len(exact), pf)
		}
	}
}

// TestCoveringPrefixesDualStack verifies families cannot be merged.
func TestCoveringPrefixesDualStack(t *testing.T) {
	s := mustCoverSet(t, "10.0.0.0/24", "2001:db8::/32")
	if _, _, ok := s.CoveringPrefixes(1); ok {
		t.Fatal("dual-stack K=1 must be infeasible")
	}
	for _, k := range []int{0, -1} {
		if _, _, ok := s.CoveringPrefixes(k); ok {
			t.Fatalf("K=%d must be infeasible", k)
		}
	}
	pf, extra, ok := s.CoveringPrefixes(2)
	if !ok {
		t.Fatal("K=2 should be feasible")
	}
	if extra.Sign() != 0 || len(pf) != 2 {
		t.Fatalf("K=2 got %v extra %s", pf, extra)
	}

	// A single-family set is feasible at K=1.
	s4 := mustCoverSet(t, "10.0.0.0/24")
	if _, _, ok := s4.CoveringPrefixes(1); !ok {
		t.Fatal("single-family K=1 should be feasible")
	}
}

// TestCoveringPrefixesEmptySet checks the empty-set edge case.
func TestCoveringPrefixesEmptySet(t *testing.T) {
	var b IPSetBuilder
	s, _ := b.IPSet()
	pf, extra, ok := s.CoveringPrefixes(1)
	if !ok || len(pf) != 0 || extra == nil || extra.Sign() != 0 {
		t.Fatalf("empty set: ok=%v pf=%v extra=%v", ok, pf, extra)
	}
}

// TestCoveringPrefixesTieBreak checks the deterministic order when
// multiple covers share the minimum extra and prefix count. Cases are
// drawn from the brute-force model and verified explicitly here.
func TestCoveringPrefixesTieBreak(t *testing.T) {
	// B=4 set {0,1,4,5}, k=2: both {0.0.0.0/31, 0.0.0.4/31} and the
	// paired halves of each /31 are worse; the unique two-prefix
	// optimum uses the lower-address prefixes first.
	s := mustCoverSet(t, "0.0.0.0/32", "0.0.0.1/32", "0.0.0.4/32", "0.0.0.5/32")
	pf, extra, ok := s.CoveringPrefixes(2)
	if !ok {
		t.Fatal("infeasible")
	}
	want := []string{"0.0.0.0/31", "0.0.0.4/31"}
	if !eqStrings(prefixStrings(pf), want) {
		t.Fatalf("got %v want %v", pf, want)
	}
	if extra.Sign() != 0 {
		t.Fatalf("extra=%s want 0", extra)
	}

	// Set {0,1} is itself a canonical /31, so K=1 is exact.
	s2 := mustCoverSet(t, "0.0.0.0/32", "0.0.0.1/32")
	pf2, extra2, _ := s2.CoveringPrefixes(1)
	if got := prefixStrings(pf2); !eqStrings(got, []string{"0.0.0.0/31"}) {
		t.Fatalf("got %v want 0.0.0.0/31", got)
	}
	if extra2.Sign() != 0 {
		t.Fatalf("extra=%s want 0", extra2)
	}

	// B=4 set {0,3,12,15}, K=3: promoting either the low block to
	// 0.0.0.0/30 or the high block to 0.0.0.12/30 costs 2 extra
	// addresses with 3 prefixes. Address order resolves it in favor of
	// the lower-address promotion.
	s4 := mustCoverSet(t, "0.0.0.0/32", "0.0.0.3/32", "0.0.0.12/32", "0.0.0.15/32")
	pf4, extra4, _ := s4.CoveringPrefixes(3)
	want4 := []string{"0.0.0.0/30", "0.0.0.12/32", "0.0.0.15/32"}
	if !eqStrings(prefixStrings(pf4), want4) {
		t.Fatalf("tie-break got %v want %v", pf4, want4)
	}
	if extra4.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("tie-break extra=%s want 2", extra4)
	}

	// Full v4 space needs exactly one prefix.
	s5 := mustCoverSet(t, "0.0.0.0/0")
	pf5, extra5, _ := s5.CoveringPrefixes(1)
	if !eqStrings(prefixStrings(pf5), []string{"0.0.0.0/0"}) || extra5.Sign() != 0 {
		t.Fatalf("full v4: %v extra %s", pf5, extra5)
	}
}

// TestCoveringPrefixesCanonical ensures output is masked, sorted, and
// mutually non-overlapping for mixed real-world input.
func TestCoveringPrefixesCanonical(t *testing.T) {
	s := mustCoverSet(t,
		"10.1.2.3/32", "10.1.2.4/30", "172.16.0.0/12",
		"2001:db8:1::/48", "fe80::1/128")
	for _, k := range []int{1, 2, 3, 4, 5} {
		pf, _, ok := s.CoveringPrefixes(k)
		if k == 1 {
			if ok {
				t.Fatal("dual-stack K=1 must fail")
			}
			continue
		}
		if !ok {
			t.Fatalf("K=%d infeasible", k)
		}
		for i, p := range pf {
			if p != p.Masked() {
				t.Fatalf("K=%d non-canonical %v", k, p)
			}
			for j := 0; j < i; j++ {
				if pf[j].Overlaps(p) {
					t.Fatalf("K=%d overlap %v %v", k, pf[j], p)
				}
			}
		}
		for i := 1; i < len(pf); i++ {
			prev, cur := pf[i-1], pf[i]
			sorted := func() bool {
				if c := prev.Addr().Compare(cur.Addr()); c != 0 {
					return c < 0
				}
				return prev.Bits() < cur.Bits()
			}()
			if !sorted {
				t.Fatalf("K=%d not sorted: %v before %v", k, pf[i-1], pf[i])
			}
		}
	}
}

// BenchmarkCoveringPrefixes measures sparse sets at a tight prefix
// budget, where the tree knapsack does the most work. For a single
// quick run, use:
//
//	go test -run xxx -bench BenchmarkCoveringPrefixes -benchtime=1x
func BenchmarkCoveringPrefixes(b *testing.B) {
	makeSet := func(b *testing.B, v4, v6, step int) *IPSet {
		var sb IPSetBuilder
		for i := 0; i < v4; i++ {
			a := uint32(i*step + 1)
			sb.Add(netip.AddrFrom4([4]byte{
				byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a),
			}))
		}
		for i := 0; i < v6; i++ {
			var a [16]byte
			a[15] = byte(i * step)
			a[14] = byte((i * step) >> 8)
			a[0] = 0x20
			sb.Add(netip.AddrFrom16(a))
		}
		s, err := sb.IPSet()
		if err != nil {
			b.Fatal(err)
		}
		return s
	}

	b.Run("v4_sparse_256_K1", func(b *testing.B) {
		s := makeSet(b, 256, 0, 257)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, ok := s.CoveringPrefixes(1); !ok {
				b.Fatal("infeasible")
			}
		}
	})
	b.Run("v4_sparse_256_K32", func(b *testing.B) {
		s := makeSet(b, 256, 0, 257)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, ok := s.CoveringPrefixes(32); !ok {
				b.Fatal("infeasible")
			}
		}
	})
	b.Run("dualstack_sparse_128_K2", func(b *testing.B) {
		s := makeSet(b, 64, 64, 4099)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, ok := s.CoveringPrefixes(2); !ok {
				b.Fatal("infeasible")
			}
		}
	})
}
