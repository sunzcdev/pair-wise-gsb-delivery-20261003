// Copyright 2020 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"math/big"
	"math/bits"
	"net/netip"
	"sort"
)

// ApproxPrefixes returns at most k canonical, non-overlapping IP prefixes that
// cover every address in s. The returned count is the number of covered
// addresses that are not in s.
//
// ApproxPrefixes minimizes that extra address count first, then the number of
// returned prefixes. Remaining ties are resolved by the lexicographically
// smaller prefix list, comparing prefix addresses and then prefix lengths.
//
// IPv4 and IPv6 are summarized independently and cannot share a prefix. If both
// families are present, k must be at least two.
func (s *IPSet) ApproxPrefixes(k int) (prefixes []netip.Prefix, extra *big.Int, ok bool) {
	if k < 0 {
		return nil, big.NewInt(0), false
	}
	exact := s.Prefixes()
	split := len(exact)
	for i, p := range exact {
		if p.Addr().Is6() {
			split = i
			break
		}
	}

	v4 := newApproxFamily(exact[:split], true, k)
	v6 := newApproxFamily(exact[split:], false, k)
	if v4.required+v6.required > k {
		return nil, big.NewInt(0), false
	}
	if len(v4.prefixes)+len(v6.prefixes) == 0 {
		return []netip.Prefix{}, big.NewInt(0), true
	}

	var best *approxCombination
	for i := range v4.states {
		for j := range v6.states {
			n := i + j
			if n == 0 || n > k {
				continue
			}
			candidate := &approxCombination{
				v4Count: i,
				v6Count: j,
				extra:   new(big.Int).Add(v4.extra(i), v6.extra(j)),
			}
			if best == nil || candidate.less(candidate, best, &v4, &v6) {
				best = candidate
			}
		}
	}

	prefixes = make([]netip.Prefix, 0, best.v4Count+best.v6Count)
	if v4.root != nil {
		prefixes = v4.reconstruct(prefixes, v4.states[best.v4Count])
	}
	if v6.root != nil {
		prefixes = v6.reconstruct(prefixes, v6.states[best.v6Count])
	}
	return prefixes, best.extra, true
}

type approxCombination struct {
	v4Count int
	v6Count int
	extra   *big.Int
}

func (c *approxCombination) less(a, b *approxCombination, v4, v6 *approxFamily) bool {
	if cmp := a.extra.Cmp(b.extra); cmp != 0 {
		return cmp < 0
	}
	if an, bn := a.v4Count+a.v6Count, b.v4Count+b.v6Count; an != bn {
		return an < bn
	}
	ao := v4.order(a.v4Count)
	bo := v4.order(b.v4Count)
	if ao != bo {
		return ao < bo
	}
	return v6.order(a.v6Count) < v6.order(b.v6Count)
}

type approxFamily struct {
	prefixes []approxPrefix
	nodes    []*approxNode
	root     *approxNode
	states   []approxState
	required int
	ipv4     bool
}

type approxPrefix struct {
	addr uint128
	bits uint8
}

type approxNode struct {
	addr     uint128
	bits     uint8
	leaves   int
	occupied uint128
	full     bool
	left     *approxNode
	right    *approxNode
	states   []approxState
}

type approxState struct {
	cost    uint128
	left    int
	right   int
	order   int
	virtual bool
}

func newApproxFamily(prefixes []netip.Prefix, ipv4 bool, k int) approxFamily {
	internal := make([]approxPrefix, len(prefixes))
	for i, p := range prefixes {
		addr16 := p.Masked().Addr().As16()
		addr := u128From16(addr16)
		bits := uint8(p.Bits())
		if ipv4 {
			addr = uint128{hi: addr.lo << 32}
		}
		internal[i] = approxPrefix{
			addr: addr,
			bits: bits,
		}
	}
	return solveApproxFamily(internal, ipv4, k)
}

func newTestApproxFamily(prefixes []approxPrefix, rootBits uint8, k int) approxFamily {
	internal := append([]approxPrefix(nil), prefixes...)
	for i := range internal {
		x := internal[i].addr.hi >> (64 - rootBits)
		internal[i].addr = uint128{hi: x << (64 - rootBits)}
		internal[i].bits -= 128 - rootBits
	}
	return solveApproxFamily(internal, false, k)
}

func solveApproxFamily(prefixes []approxPrefix, ipv4 bool, k int) approxFamily {
	family := approxFamily{
		prefixes: prefixes,
		ipv4:     ipv4,
	}
	if len(prefixes) == 0 {
		family.states = []approxState{{}}
		return family
	}
	sort.Slice(family.prefixes, func(i, j int) bool {
		a, b := family.prefixes[i], family.prefixes[j]
		if a.addr != b.addr {
			return a.addr.hi < b.addr.hi || a.addr.hi == b.addr.hi && a.addr.lo < b.addr.lo
		}
		return a.bits < b.bits
	})

	family.root = family.build(0, len(family.prefixes), 0)
	family.required = 1
	if k < family.required {
		family.states = []approxState{{}}
		return family
	}
	family.solve(family.root, k)

	family.states = make([]approxState, family.root.leaves+1)
	for i := 1; i <= family.root.leaves && i <= k; i++ {
		family.states[i] = family.root.states[i]
	}
	root := approxState{cost: family.extraAt(family.root, 255)}
	if cmp128(root.cost, family.states[1].cost) <= 0 {
		root.virtual = true
		family.states[1] = root
	}
	return family
}

func (f *approxFamily) build(lo, hi int, depth uint8) *approxNode {
	first, last := f.prefixes[lo], f.prefixes[hi-1]
	if lo+1 == hi {
		node := f.newNode(first.addr, first.bits)
		node.leaves = 1
		node.full = true
		node.occupied = f.prefixSize(first.bits)
		return node
	}

	common := first.addr.commonPrefixLen(last.addr)
	splitBit := common
	mid := lo + sort.Search(hi-lo, func(i int) bool {
		return bit128(f.prefixes[lo+i].addr, splitBit)
	})
	node := f.newNode(first.addr.bitsClearedFrom(common), common)
	node.left = f.build(lo, mid, common+1)
	node.right = f.build(mid, hi, common+1)
	node.leaves = node.left.leaves + node.right.leaves
	node.occupied = add128(node.left.occupied, node.right.occupied)

	if node.left.full && node.right.full &&
		node.left.bits == common+1 && node.right.bits == common+1 {
		node.full = true
		if common != 0 {
			node.occupied = f.prefixSize(common)
		}
		node.left = nil
		node.right = nil
	}
	return node
}

func (f *approxFamily) newNode(addr uint128, bits uint8) *approxNode {
	node := &approxNode{addr: addr, bits: bits}
	f.nodes = append(f.nodes, node)
	return node
}

func (f *approxFamily) solve(node *approxNode, k int) {
	if node.full {
		node.states = make([]approxState, min2(k+1, 2))
		node.states[1] = approxState{cost: f.extraAt(node, node.bits), order: 0}
		return
	}

	f.solve(node.left, k)
	f.solve(node.right, k)
	node.states = make([]approxState, min2(k+1, node.leaves+1))
	node.states[1] = approxState{cost: f.extraAt(node, node.bits), order: 0}

	type candidate struct {
		cost  uint128
		left  int
		right int
		order [2]int
	}
	var splits []candidate
	for leftN := 1; leftN <= node.left.leaves && leftN < k; leftN++ {
		for rightN := 1; rightN <= node.right.leaves && leftN+rightN <= k; rightN++ {
			left := node.left.states[leftN]
			right := node.right.states[rightN]
			splits = append(splits, candidate{
				cost:  add128(left.cost, right.cost),
				left:  leftN,
				right: rightN,
				order: [2]int{left.order, right.order},
			})
		}
	}
	sort.Slice(splits, func(i, j int) bool {
		a, b := splits[i], splits[j]
		if an, bn := a.left+a.right, b.left+b.right; an != bn {
			return an < bn
		}
		if cmp := cmp128(a.cost, b.cost); cmp != 0 {
			return cmp < 0
		}
		if a.order[0] != b.order[0] {
			return a.order[0] < b.order[0]
		}
		return a.order[1] < b.order[1]
	})

	counts := make(map[int]struct{})
	for _, split := range splits {
		counts[split.left+split.right] = struct{}{}
	}
	for n := range counts {
		orders := make(map[[2]int]int)
		nextOrder := 1
		for _, split := range splits {
			if split.left+split.right != n {
				continue
			}
			order, seen := orders[split.order]
			if !seen {
				order = nextOrder
				orders[split.order] = nextOrder
				nextOrder++
			}
			candidate := approxState{
				cost:  split.cost,
				left:  split.left,
				right: split.right,
				order: order,
			}
			best := node.states[n]
			if best.left == 0 || cmp128(candidate.cost, best.cost) < 0 ||
				candidate.cost == best.cost && candidate.order < best.order {
				node.states[n] = candidate
			}
		}
	}
}

func (f *approxFamily) prefixSize(bits uint8) uint128 {
	if bits == 255 {
		return uint128{}
	}
	return uint128{^uint64(0), ^uint64(0)}.and(mask6[bits].not())
}

func (f *approxFamily) extraAt(node *approxNode, sizeBits uint8) uint128 {
	if sizeBits == 255 {
		return sub128(f.prefixSize(255), node.occupied)
	}
	return sub128(f.prefixSize(sizeBits), node.occupied)
}

func (f *approxFamily) extra(n int) *big.Int {
	if n == 0 || f.root == nil {
		return big.NewInt(0)
	}
	return u128ToBig(f.states[n].cost)
}

func (f *approxFamily) order(n int) int {
	if n == 0 || f.root == nil {
		return 0
	}
	return f.states[n].order
}

func (f *approxFamily) reconstruct(dst []netip.Prefix, state approxState) []netip.Prefix {
	if state.virtual {
		return append(dst, f.prefix(f.root.addr, 0))
	}
	return f.reconstructNode(dst, f.root, state)
}

func (f *approxFamily) reconstructNode(dst []netip.Prefix, node *approxNode, state approxState) []netip.Prefix {
	if state.left == 0 && state.right == 0 {
		return append(dst, f.prefix(node.addr, node.bits))
	}
	dst = f.reconstructNode(dst, node.left, node.left.states[state.left])
	dst = f.reconstructNode(dst, node.right, node.right.states[state.right])
	return dst
}

func (f *approxFamily) prefix(addr uint128, bits uint8) netip.Prefix {
	ip := addr.IP6()
	prefixBits := int(bits)
	if f.ipv4 {
		addr = uint128{lo: addr.hi >> 32}
		ip = netip.AddrFrom4([4]byte{
			byte(addr.lo >> 24), byte(addr.lo >> 16),
			byte(addr.lo >> 8), byte(addr.lo),
		})
	}
	return netip.PrefixFrom(ip, prefixBits)
}

func bit128(v uint128, bit uint8) bool {
	if bit < 64 {
		return v.hi&(uint64(1)<<(63-bit)) != 0
	}
	return v.lo&(uint64(1)<<(127-bit)) != 0
}

func add128(a, b uint128) uint128 {
	lo, carry := bits.Add64(a.lo, b.lo, 0)
	hi, _ := bits.Add64(a.hi, b.hi, carry)
	return uint128{hi: hi, lo: lo}
}

func sub128(a, b uint128) uint128 {
	lo, borrow := bits.Sub64(a.lo, b.lo, 0)
	hi, _ := bits.Sub64(a.hi, b.hi, borrow)
	return uint128{hi: hi, lo: lo}
}

func less128(a, b uint128) bool {
	return cmp128(a, b) < 0
}

func cmp128(a, b uint128) int {
	if a.hi != b.hi {
		if a.hi < b.hi {
			return -1
		}
		return 1
	}
	if a.lo != b.lo {
		if a.lo < b.lo {
			return -1
		}
		return 1
	}
	return 0
}

func u128ToBig(v uint128) *big.Int {
	return new(big.Int).Add(
		new(big.Int).Lsh(new(big.Int).SetUint64(v.hi), 64),
		new(big.Int).SetUint64(v.lo),
	)
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
