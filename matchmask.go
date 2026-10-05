// Copyright 2026 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"math/bits"
	"net/netip"
	"strconv"
)

// ErrMatchMaskTooManyRanges is returned by IPSetFromMatchMask and
// IPSetBuilder.AddMatchMask when converting a bit-match pattern would
// require more output ranges than the caller is willing to accept.
//
// The count returned alongside it is the exact number of ranges
// required and may be larger than fits in an int. No result
// construction is begun when this error is reported.
var ErrMatchMaskTooManyRanges = errors.New("netipx: match mask requires more output ranges than allowed")

// IPSetFromMatchMask builds an IPSet from an address value and a
// bit-match mask, the form produced by some network devices instead
// of a CIDR prefix.
//
// An address x matches the pattern when, for every mask bit that is
// set, the corresponding bit of x equals that bit of addr; mask zero
// bits are unconstrained. The matching set is represented exactly as
// a minimal, sorted list of contiguous address ranges: an address
// that does not match is never included, and unconstrained bits of
// addr are ignored rather than treated as a fixed prefix. The mask
// need not be contiguous. Both IPv4 and IPv6 are supported.
//
// maxRanges bounds the size of the result: if the pattern requires
// more than maxRanges output ranges, IPSetFromMatchMask returns a nil
// set, ErrMatchMaskTooManyRanges, and the exact number of ranges
// required, without beginning construction of the large result. The
// count is exact even for IPv6 patterns that require up to 2^127
// ranges.
//
// addr and mask must both be valid, must not carry zones, and must
// belong to the same address family; otherwise an error is returned.
// An all-zero mask matches the whole address family; an all-one mask
// matches the single address addr.
func IPSetFromMatchMask(addr, mask netip.Addr, maxRanges int) (*IPSet, error) {
	var b IPSetBuilder
	if _, err := b.AddMatchMask(addr, mask, maxRanges); err != nil {
		return nil, err
	}
	s, _ := b.IPSet()
	return s, nil
}

// AddMatchMask adds all addresses matching the bit-match pattern
// (addr, mask) to s; see IPSetFromMatchMask for pattern semantics.
//
// It returns the exact number of minimal contiguous ranges the
// pattern expands to. If that number exceeds maxRanges, the builder
// is left unmodified and AddMatchMask returns
// ErrMatchMaskTooManyRanges together with the exact count. Any other
// input error also leaves the builder unmodified and returns a nil
// count.
func (s *IPSetBuilder) AddMatchMask(addr, mask netip.Addr, maxRanges int) (*big.Int, error) {
	if err := validateMatchPair(addr, mask); err != nil {
		return nil, err
	}
	if maxRanges < 1 {
		return nil, fmt.Errorf("netipx: maxRanges must be at least 1, got %d", maxRanges)
	}

	var (
		v, m uint128
		n    uint8
		isV4 = addr.Is4()
	)
	if isV4 {
		v = v4As128(addr)
		m = v4As128(mask)
		n = 32
	} else {
		v = addr16As128(addr)
		m = addr16As128(mask)
		n = 128
	}

	ranges, count := matchMaskRanges(v, m, n, maxRanges)
	if ranges == nil {
		return count, ErrMatchMaskTooManyRanges
	}
	for _, rr := range ranges {
		if isV4 {
			s.AddRange(IPRangeFrom(rr.lo.IP4(), rr.hi.IP4()))
		} else {
			s.AddRange(IPRangeFrom(rr.lo.IP6(), rr.hi.IP6()))
		}
	}
	return count, nil
}

func validateMatchPair(addr, mask netip.Addr) error {
	if !addr.IsValid() {
		return errors.New("netipx: match mask addr is invalid")
	}
	if !mask.IsValid() {
		return errors.New("netipx: match mask is invalid")
	}
	if addr.Zone() != "" {
		return fmt.Errorf("netipx: match mask addr %v has a zone", addr)
	}
	if mask.Zone() != "" {
		return fmt.Errorf("netipx: match mask %v has a zone", mask)
	}
	if addr.Is4() != mask.Is4() {
		return fmt.Errorf("netipx: match mask addr %v and mask %v are different address families", addr, mask)
	}
	return nil
}

// matchRange is one inclusive range produced by a bit-match pattern.
type matchRange struct {
	lo, hi uint128
}

// matchMaskRanges expands the n-bit bit-match pattern (value, mask)
// into its minimal sorted list of inclusive ranges: a mask bit of 1
// fixes that bit to the corresponding value bit, while a mask bit of
// 0 leaves it unconstrained. n must be 8, 32, or 128.
//
// The exact number of ranges, at most 2 to the number of free bits,
// is computed before any result allocation. If it exceeds maxRanges,
// matchMaskRanges returns a nil slice and the exact count without
// beginning construction; otherwise it returns the ranges. It expands
// by combinations of free bits rather than by enumerating addresses.
func matchMaskRanges(value, mask uint128, n uint8, maxRanges int) ([]matchRange, *big.Int) {
	if mask.isZero() {
		ones := []matchRange{{uint128{}, nBitsOnes(n)}}
		return ones, big.NewInt(1)
	}

	tz := mask.trailingZeros128()        // free bits below the lowest fixed bit
	ones := popcountN(mask, n)           // number of fixed bits
	exp := uint(int(n) - int(tz) - ones) // number of combinatorial blocks
	// Build 2^exp directly. The value needs at most two words on a
	// 64-bit platform, so the over-limit path allocates only the
	// big.Int and its magnitude.
	count := new(big.Int)
	setPow2(count, exp)
	if pow2Exceeds(exp, maxRanges) {
		return nil, count
	}

	// Bits below the lowest fixed bit are free and make every block
	// 2^tz addresses wide; only the top (n-tz)-bit prefix distinguishes
	// blocks.
	width := uint(n - tz)
	mp := shr128(mask, tz)
	vp := shr128(value, tz).and(mp)

	// Enumerate every width-bit prefix q with q&mp == vp in ascending
	// order. Fixed bits start equal to value; free bits are added
	// least-significant first so that each doubling appends the larger
	// half, keeping the output sorted.
	prefixes := make([]uint128, 1, 1<<exp)
	prefixes[0] = vp
	for i := uint(0); i < width; i++ {
		bit := numericBit128(i)
		if !mp.and(bit).isZero() {
			continue // fixed bit already placed
		}
		old := prefixes
		prefixes = prefixes[:2*len(prefixes)]
		for j, p := range old {
			prefixes[len(old)+j] = p.or(bit)
		}
	}

	blockOnes := lowOnes128(tz)
	out := make([]matchRange, 0, len(prefixes))
	for _, p := range prefixes {
		lo := shl128(p, tz)
		out = append(out, matchRange{lo, lo.or(blockOnes)})
	}
	return out, count
}

// v4As128 packs an IPv4 address into the low 32 bits of a uint128.
// IPv4 addresses serialize in IPv4-mapped form, so the high 96 bits
// are cleared.
func v4As128(addr netip.Addr) uint128 {
	u := u128From16(addr.As16())
	u.hi = 0
	u.lo &= 0xffffffff
	return u
}

// addr16As128 converts a 16-byte (IPv6) address to a uint128.
func addr16As128(addr netip.Addr) uint128 {
	a := addr.As16()
	return uint128{
		binary.BigEndian.Uint64(a[:8]),
		binary.BigEndian.Uint64(a[8:]),
	}
}

// nBitsOnes returns a uint128 with the low n bits set.
func nBitsOnes(n uint8) uint128 {
	if n == 128 {
		return uint128{^uint64(0), ^uint64(0)}
	}
	return uint128{0, 0}.bitsSetFrom(128 - n)
}

// lowOnes128 returns a uint128 with the low n bits set.
func lowOnes128(n uint8) uint128 {
	switch {
	case n == 0:
		return uint128{}
	case n < 64:
		return uint128{0, ^uint64(0) >> (64 - n)}
	case n == 64:
		return uint128{0, ^uint64(0)}
	default:
		return uint128{^uint64(0) >> (128 - n), ^uint64(0)}
	}
}

// popcountN counts the set bits in the low n bits of u.
func popcountN(u uint128, n uint8) int {
	switch {
	case n <= 64:
		mask := ^uint64(0)
		if n < 64 {
			mask = ^uint64(0) >> (64 - n)
		}
		return bits.OnesCount64(u.lo & mask)
	default:
		c := bits.OnesCount64(u.hi)
		if n < 128 {
			c += bits.OnesCount64(u.lo & (^uint64(0) >> (128 - n)))
		} else {
			c += bits.OnesCount64(u.lo)
		}
		return c
	}
}

// trailingZeros128 returns the number of trailing zero bits in u.
// u must be non-zero.
func (u uint128) trailingZeros128() uint8 {
	if u.lo != 0 {
		return uint8(bits.TrailingZeros64(u.lo))
	}
	return uint8(64 + bits.TrailingZeros64(u.hi))
}

// shr128 returns u >> k.
func shr128(u uint128, k uint8) uint128 {
	switch {
	case k == 0:
		return u
	case k < 64:
		return uint128{u.hi >> k, u.hi<<(64-k) | u.lo>>k}
	case k == 64:
		return uint128{0, u.hi}
	default:
		return uint128{0, u.hi >> (k - 64)}
	}
}

// shl128 returns u << k.
func shl128(u uint128, k uint8) uint128 {
	switch {
	case k == 0:
		return u
	case k < 64:
		return uint128{u.hi<<k | u.lo>>(64-k), u.lo << k}
	case k == 64:
		return uint128{u.lo, 0}
	default:
		return uint128{u.lo << (k - 64), 0}
	}
}

// numericBit128 returns a uint128 with only numeric bit i set, where
// bit 0 is the least significant bit.
func numericBit128(i uint) uint128 {
	if i < 64 {
		return uint128{0, 1 << i}
	}
	return uint128{1 << (i - 64), 0}
}

// pow2Exceeds reports whether 2^exp > limit, without allocating a
// big.Int. exp is at most 127 (the number of free bits in an IPv6
// pattern whose lowest bit is fixed).
func pow2Exceeds(exp uint, limit int) bool {
	if uint64(exp) >= 63 {
		// 2^63 and above cannot be exceeded by a positive int on a
		// 32- or 64-bit platform; exp==63 with limit==2^63-1 exceeds.
		return true
	}
	return uint64(1)<<exp > uint64(limit)
}

// setPow2 sets z to 2^exp using z's existing word capacity.
func setPow2(z *big.Int, exp uint) {
	wordBits := uint(strconv.IntSize)
	words := make([]big.Word, exp/wordBits+1)
	words[exp/wordBits] = big.Word(1) << (exp % wordBits)
	z.SetBits(words)
}
