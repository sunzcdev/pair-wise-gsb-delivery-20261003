package netipx

import (
	"math/big"
	"math/bits"
	"net/netip"
	"sort"
)

// u128Max is a value larger than any cost that can arise for a single
// address family (the largest possible cost is 2^128-1).
var u128Max = uint128{^uint64(0), ^uint64(0)}

// famPrefix is a single canonical prefix within one address family,
// expressed in that family's own numbering: for IPv4 the address value
// lives in the low 32 bits, for IPv6 it fills all 128 bits.
type famPrefix struct {
	addr uint128
	bits uint8
}

// coverNode is a node of the Patricia trie built from one family's
// exact prefixes. Leaves are the exact prefixes themselves; internal
// nodes are the branching points where a single promoting prefix can
// stand in for everything beneath it.
type coverNode struct {
	depth    uint8
	bits     uint8 // family width (32 or 128); node prefix length for leaves
	leaf     bool
	addr     uint128
	children []*coverNode

	// total is the number of addresses in the promoting prefix that
	// belong to the exact set; extra = prefix size - total.
	total, extra uint128

	// dp[r] is the minimum extra-address cost of covering this
	// subtree while reducing the number of emitted prefixes by r
	// relative to the exact (leaf) representation.
	dp []uint128
}

func (n *coverNode) leaves() int {
	if n.leaf {
		return 1
	}
	m := 0
	for _, c := range n.children {
		m += c.leaves()
	}
	return m
}

// famCommonLen reports the length of the common prefix of a and b
// within an address family of bitLen bits (32 or 128).
func famCommonLen(a, b uint128, bitLen uint8) uint8 {
	if bitLen == 128 {
		return a.commonPrefixLen(b)
	}
	return uint8(bits.LeadingZeros32(uint32(a.lo^b.lo))) - (32 - bitLen)
}

// oneShl128 returns 2^s as a uint128 (s may be 128, yielding 2^128
// represented as zero, which is only valid as an intermediate that is
// reduced by subtracting a positive total).
func oneShl128(s uint8) uint128 {
	if s < 64 {
		return uint128{0, uint64(1) << uint(s)}
	}
	return uint128{uint64(1) << uint(s-64), 0}
}

// famBitAt reports bit at depth within a family of width bitLen. IPv4
// values are stored in the low 32 bits.
func famBitAt(a uint128, depth, bitLen uint8) uint8 {
	if bitLen == 128 {
		return a.bitAt(depth)
	}
	return uint8((uint32(a.lo) >> (31 - depth)) & 1)
}

// famMaskedBase returns the base address of a length-depth prefix in
// a's family, given any canonical leaf address at or below depth.
func famMaskedBase(a uint128, depth, bitLen uint8) uint128 {
	if bitLen == 128 {
		return a.and(mask6[depth])
	}
	var mask uint32
	if host := bitLen - depth; host < 32 {
		mask = ^uint32(0) << host
	}
	return uint128{0, uint64(uint32(a.lo) & mask)}
}

// buildCoverTree builds the compressed binary trie of the canonical,
// pairwise non-overlapping prefixes ps within a family of width
// bitLen. ps must be sorted by address then prefix length.
func buildCoverTree(ps []famPrefix, bitLen uint8) *coverNode {
	if len(ps) == 1 {
		n := &coverNode{
			depth: ps[0].bits,
			bits:  ps[0].bits,
			leaf:  true,
			addr:  ps[0].addr,
		}
		n.total = oneShl128(bitLen - ps[0].bits)
		return n
	}

	depth := bitLen
	first, last := ps[0].addr, ps[len(ps)-1].addr
	if first != last {
		depth = famCommonLen(first, last, bitLen)
	}

	n := &coverNode{depth: depth, bits: bitLen, addr: famMaskedBase(first, depth, bitLen)}
	start := 0
	want := famBitAt(ps[0].addr, depth, bitLen)
	for i := range ps {
		if got := famBitAt(ps[i].addr, depth, bitLen); got != want {
			n.children = append(n.children, buildCoverTree(ps[start:i], bitLen))
			start = i
			want = got
		}
	}
	n.children = append(n.children, buildCoverTree(ps[start:], bitLen))

	for _, c := range n.children {
		n.total = n.total.add(c.total)
	}
	n.extra = famNodeExtra(n.total, bitLen-depth)
	return n
}

// famNodeExtra returns 2^s - total, without ever materializing 2^128
// (which does not fit a uint128). total is always positive for a
// non-empty subtree, so the result is at most 2^128-1.
func famNodeExtra(total uint128, s uint8) uint128 {
	if s == 128 {
		// 2^128 - total = (2^128 - 1) - total + 1.
		return u128Max.sub(total).addOne()
	}
	return oneShl128(s).sub(total)
}

// childCost is c.dp[r], treating a leaf as a one-element dp of {0}.
func childCost(c *coverNode, r int) uint128 {
	if c.leaf {
		if r == 0 {
			return uint128{}
		}
		return u128Max
	}
	if r < 0 || r >= len(c.dp) {
		return u128Max
	}
	return c.dp[r]
}

// computeDP fills n.dp for every node of the subtree. An internal node
// in a Patricia trie has exactly two children. For a subtree with m
// leaves, dp[r], 0 <= r < m, is the minimum extra cost of a cover
// using m-r prefixes. The largest reduction (r=m-1) is achieved by
// promoting the node; smaller reductions split across the children.
func computeDP(n *coverNode) {
	if n.leaf {
		return
	}
	for _, c := range n.children {
		computeDP(c)
	}
	left, right := n.children[0], n.children[1]
	m0, m1 := left.leaves(), right.leaves()
	m := m0 + m1
	dp := make([]uint128, m)
	for r := 0; r < m; r++ {
		best := u128Max
		// Split reductions r0+r1 = r across the children.
		for r0 := 0; r0 <= r; r0++ {
			r1 := r - r0
			c0 := childCost(left, r0)
			c1 := childCost(right, r1)
			if c0 == u128Max || c1 == u128Max {
				continue
			}
			if cand := c0.add(c1); lessU128(cand, best) {
				best = cand
			}
		}
		// Promotion covers all m leaves with one prefix (r=m-1). It is
		// lexicographically preferred over a split of equal cost.
		if r == m-1 && !lessU128(best, n.extra) {
			best = n.extra
		}
		dp[r] = best
	}
	n.dp = dp
}

func lessU128(a, b uint128) bool {
	if a.hi != b.hi {
		return a.hi < b.hi
	}
	return a.lo < b.lo
}

func eqU128(a, b uint128) bool { return a == b }

// reconstruct returns the lexicographically smallest minimum-extra
// cover of n's subtree using m-r prefixes, where m is the number of
// leaves. dp[r] is assumed to hold the minimum cost.
func reconstruct(n *coverNode, r int, bitLen uint8) []famPrefix {
	if n.leaf {
		return []famPrefix{{addr: n.addr, bits: n.bits}}
	}
	m := n.leaves()

	// The largest reduction is only reachable by promotion.
	if r == m-1 {
		return []famPrefix{{addr: n.addr, bits: n.depth}}
	}

	left, right := n.children[0], n.children[1]
	m0, m1 := left.leaves(), right.leaves()
	target := n.dp[r]

	// Every feasible split reaches the minimum; choose the one whose
	// left subtree cover is lexicographically smallest (left covers
	// strictly precede right ones in address order).
	bestR0 := -1
	var bestLeft []famPrefix
	for r0 := 0; r0 < m0 && r0 <= r; r0++ {
		r1 := r - r0
		if r1 < 0 || r1 >= m1 {
			continue
		}
		c0 := childCost(left, r0)
		c1 := childCost(right, r1)
		if c0 == u128Max || c1 == u128Max || !eqU128(c0.add(c1), target) {
			continue
		}
		head := reconstruct(left, r0, bitLen)
		if bestR0 < 0 || lexLessPrefixes(head, bestLeft) {
			bestR0 = r0
			bestLeft = head
		}
	}
	out := bestLeft
	out = append(out, reconstruct(right, r-bestR0, bitLen)...)
	return out
}

func lexLessPrefixes(a, b []famPrefix) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].addr != b[i].addr {
			return lessU128(a[i].addr, b[i].addr)
		}
		if a[i].bits != b[i].bits {
			return a[i].bits < b[i].bits
		}
	}
	return len(a) < len(b)
}

// familyCover holds the solved Patricia trie for one address family.
type familyCover struct {
	root   *coverNode
	leaves int
	bitLen uint8
}

func newFamilyCover(ps []famPrefix, bitLen uint8) *familyCover {
	root := buildCoverTree(ps, bitLen)
	computeDP(root)
	return &familyCover{root: root, leaves: len(ps), bitLen: bitLen}
}

// cover returns the optimal cover using exactly t prefixes and its
// extra-address cost.
func (f *familyCover) cover(t int) ([]famPrefix, uint128) {
	if t < 1 || t > f.leaves {
		return nil, u128Max
	}
	r := f.leaves - t
	out := reconstruct(f.root, r, f.bitLen)
	if f.root.leaf {
		return out, uint128{}
	}
	return out, f.root.dp[r]
}

func prefixToFam(p netip.Prefix) famPrefix {
	u := u128From16(p.Addr().As16())
	if p.Addr().Is4() {
		// As16 maps IPv4 into ::ffff:a.b.c.d; the address is the low
		// 32 bits of lo, i.e. uint32 of the mapped low word.
		u = uint128{0, uint64(uint32(u.lo))}
	}
	return famPrefix{addr: u, bits: uint8(p.Bits())}
}

func famToPrefix(fp famPrefix, bitLen uint8) netip.Prefix {
	var addr netip.Addr
	if bitLen == 4 {
		// uint128.IP4 expects the address in the low 32 bits of its
		// 8-byte view, i.e. the low 32 bits of lo, which is already
		// where the solver keeps it.
		addr = fp.addr.IP4()
	} else {
		addr = fp.addr.IP6()
	}
	return netip.PrefixFrom(addr, int(fp.bits))
}

func u128ToBig(u uint128) *big.Int {
	b := new(big.Int).SetUint64(u.hi)
	b.Lsh(b, 64)
	b.Or(b, new(big.Int).SetUint64(u.lo))
	return b
}

// CoveringPrefixes returns at most k canonical, non-overlapping IP
// prefixes whose union contains every address in s, along with the
// number of extra addresses that the returned prefixes include but s
// does not.
//
// Among all feasible covers the result minimizes:
//  1. the number of extra addresses covered, then
//  2. the number of prefixes used, and finally
//  3. the sorted list of prefixes (compared by address, then by
//     prefix length, shorter first), for a deterministic result.
//
// IPv4 and IPv6 prefixes are never combined: if s contains addresses
// of both families at least two prefixes are required, so k must be at
// least two in that case. Address counts are exact even for IPv6; the
// returned count is therefore a *big.Int.
//
// If k is less than 1, or s contains both address families and k is 1,
// no such cover exists and ok is false. An empty set is covered by the
// empty prefix list at zero extra addresses for any positive k.
//
// The exact IPSet.Prefixes representation is unaffected by this
// method; if k is at least the number of exact prefixes in s, the
// returned cover is that exact list with zero extra addresses.
func (s *IPSet) CoveringPrefixes(k int) (prefixes []netip.Prefix, extra *big.Int, ok bool) {
	if k < 1 {
		return nil, nil, false
	}
	exact := s.Prefixes()
	if len(exact) == 0 {
		return []netip.Prefix{}, big.NewInt(0), true
	}

	var v4, v6 []famPrefix
	for _, p := range exact {
		fp := prefixToFam(p)
		if p.Addr().Is4() {
			v4 = append(v4, fp)
		} else {
			v6 = append(v6, fp)
		}
	}
	if (len(v4) > 0 && len(v6) > 0) && k < 2 {
		return nil, nil, false
	}

	var f4, f6 *familyCover
	if len(v4) > 0 {
		f4 = newFamilyCover(v4, 32)
	}
	if len(v6) > 0 {
		f6 = newFamilyCover(v6, 128)
	}

	if f6 == nil {
		return bestSingleFamily(f4, 32, k)
	}
	if f4 == nil {
		return bestSingleFamily(f6, 128, k)
	}

	var (
		bestCost  = u128Max
		bestCount = k + 1
		best4     []famPrefix
		best6     []famPrefix
	)
	max4 := k - 1
	if f4.leaves < max4 {
		max4 = f4.leaves
	}
	for t4 := 1; t4 <= max4; t4++ {
		max6 := k - t4
		if f6.leaves < max6 {
			max6 = f6.leaves
		}
		for t6 := 1; t6 <= max6; t6++ {
			p4, c4 := f4.cover(t4)
			p6, c6 := f6.cover(t6)
			if c4 == u128Max || c6 == u128Max {
				continue
			}
			cost := c4.add(c6)
			count := t4 + t6
			switch {
			case lessU128(cost, bestCost):
			case eqU128(cost, bestCost) && (count < bestCount ||
				(count == bestCount && lexLessCombined(p4, p6, best4, best6))):
			default:
				continue
			}
			bestCost, bestCount, best4, best6 = cost, count, p4, p6
		}
	}
	if best4 == nil {
		return nil, nil, false
	}
	return materializeFamilies(best4, best6), u128ToBig(bestCost), true
}

func bestSingleFamily(f *familyCover, bitLen uint8, k int) ([]netip.Prefix, *big.Int, bool) {
	var (
		bestCost  = u128Max
		bestCount = k + 1
		bestFam   []famPrefix
	)
	max := f.leaves
	if k < max {
		max = k
	}
	for t := 1; t <= max; t++ {
		list, cost := f.cover(t)
		switch {
		case lessU128(cost, bestCost):
		case cost == bestCost && (t < bestCount ||
			(t == bestCount && lexLessPrefixes(list, bestFam))):
		default:
			continue
		}
		bestCost, bestCount, bestFam = cost, t, list
	}
	if bitLen == 32 {
		return materializeFamilies(bestFam, nil), u128ToBig(bestCost), true
	}
	return materializeFamilies(nil, bestFam), u128ToBig(bestCost), true
}

func materializeFamilies(v4 []famPrefix, v6 []famPrefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(v4)+len(v6))
	for _, fp := range v4 {
		out = append(out, famToPrefix(fp, 4))
	}
	for _, fp := range v6 {
		out = append(out, famToPrefix(fp, 128))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Addr(), out[j].Addr()
		if c := a.Compare(b); c != 0 {
			return c < 0
		}
		return out[i].Bits() < out[j].Bits()
	})
	return out
}

// lexLessCombined compares two v4/v6 family-prefix pairs. All v4
// prefixes precede all v6 prefixes in the emitted order.
func lexLessCombined(a4, a6, b4, b6 []famPrefix) bool {
	if c := lexCompareFam(a4, b4); c != 0 {
		return c < 0
	}
	return lexCompareFam(a6, b6) < 0
}

func lexCompareFam(a, b []famPrefix) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i].addr != b[i].addr {
			if lessU128(a[i].addr, b[i].addr) {
				return -1
			}
			return 1
		}
		if a[i].bits != b[i].bits {
			if a[i].bits < b[i].bits {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}
