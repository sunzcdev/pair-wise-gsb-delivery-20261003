// Copyright 2023 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"errors"
	"fmt"
	"math/big"
	"math/bits"
	"net/netip"
)

// An AddrMask matches IP addresses against an address value paired
// with a per-bit match mask, as exported by some network equipment
// ACLs. A mask bit of 1 means the corresponding address bit must
// equal the same bit in Addr; a mask bit of 0 means that address bit
// is unconstrained ("don't care") and may be either 0 or 1.
//
// Unlike a CIDR mask, the 1 bits of an AddrMask mask need not be
// contiguous. For example, 255.0.255.0 matches every address whose
// first and third octets equal those of Addr, regardless of the
// second and fourth octets.
//
// AddrMask values are normalized: address bits at positions where the
// mask bit is 0 are cleared, because those bits are not part of the
// pattern. The zero AddrMask is not valid; construct one with
// AddrMaskFrom.
type AddrMask struct {
	addr netip.Addr // normalized; wildcard bits cleared, no zone
	mask netip.Addr // canonical 4- or 16-byte form, no zone
}

// AddrMaskFrom returns an AddrMask with the given address value and
// per-bit match mask.
//
// It reports an error if addr or mask are invalid, if they are from
// different address families (including an IPv4-mapped IPv6 address
// paired with an IPv6 mask or vice versa), or if either carries a
// zone. Address bits at wildcard (mask bit 0) positions are ignored
// and normalized away rather than treated as a fixed prefix.
func AddrMaskFrom(addr, mask netip.Addr) (AddrMask, error) {
	if !addr.IsValid() {
		return AddrMask{}, errors.New("AddrMaskFrom: invalid address")
	}
	if !mask.IsValid() {
		return AddrMask{}, errors.New("AddrMaskFrom: invalid mask")
	}
	if addr.Zone() != "" || mask.Zone() != "" {
		return AddrMask{}, errors.New("AddrMaskFrom: zones are not supported")
	}
	if is4In6(addr) || is4In6(mask) {
		return AddrMask{}, errors.New("AddrMaskFrom: IPv4-mapped IPv6 addresses are not supported; call Unmap first")
	}
	if addr.Is4() != mask.Is4() {
		return AddrMask{}, fmt.Errorf("AddrMaskFrom: address %v and mask %v are from different address families", addr, mask)
	}

	var am AddrMask
	if addr.Is4() {
		a := addr.As4()
		m := mask.As4()
		for i := range a {
			a[i] &= m[i]
		}
		am.addr = netip.AddrFrom4(a)
		am.mask = netip.AddrFrom4(m)
	} else {
		a := addr.As16()
		m := mask.As16()
		for i := range a {
			a[i] &= m[i]
		}
		am.addr = netip.AddrFrom16(a)
		am.mask = netip.AddrFrom16(m)
	}
	return am, nil
}

// is4In6 reports whether a is a 16-byte IPv4-mapped IPv6 address
// (::ffff:a.b.c.d). It avoids netip.Addr.Is4In6, which requires Go
// 1.20.
func is4In6(a netip.Addr) bool {
	if !a.IsValid() || a.Is4() {
		return false
	}
	b := a.As16()
	return b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 0 &&
		b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] == 0 &&
		b[8] == 0 && b[9] == 0 && b[10] == 0xff && b[11] == 0xff
}

// MustAddrMaskFrom is like AddrMaskFrom but panics on error.
func MustAddrMaskFrom(addr, mask netip.Addr) AddrMask {
	am, err := AddrMaskFrom(addr, mask)
	if err != nil {
		panic(err)
	}
	return am
}

// Valid reports whether am is a valid AddrMask: one constructed with
// AddrMaskFrom whose address and mask have not since been zeroed.
func (am AddrMask) Valid() bool {
	return am.addr.IsValid() && am.mask.IsValid()
}

// Addr returns the normalized address value: Addr has a zero bit at
// every wildcard position and the fixed value bits elsewhere.
func (am AddrMask) Addr() netip.Addr { return am.addr }

// Mask returns the per-bit match mask.
func (am AddrMask) Mask() netip.Addr { return am.mask }

// Is4 reports whether am is an IPv4 AddrMask.
func (am AddrMask) Is4() bool { return am.addr.Is4() }

// Contains reports whether ip matches the AddrMask pattern.
//
// It returns false if am is not valid, ip is invalid, ip has a zone,
// or ip is from a different address family. Wildcard bits in the
// pattern and in ip are ignored; only bits at fixed (mask bit 1)
// positions are compared.
func (am AddrMask) Contains(ip netip.Addr) bool {
	if !am.Valid() || !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	if am.addr.Is4() != ip.Is4() {
		return false
	}
	if ip.Is4() {
		a := ip.As4()
		want := am.addr.As4()
		m := am.mask.As4()
		for i := range a {
			if a[i]&m[i] != want[i] {
				return false
			}
		}
		return true
	}
	a := ip.As16()
	want := am.addr.As16()
	m := am.mask.As16()
	for i := range a {
		if a[i]&m[i] != want[i] {
			return false
		}
	}
	return true
}

// RangeCount returns the exact number of disjoint, sorted IPRanges in
// the minimal range representation of the AddrMask. The result is
// always a power of two: 2**j, where j is the number of wildcard
// bits at or above the lowest fixed bit. A mask of all zero bits
// (the entire address space) and a mask of all one bits (a single
// address) both yield one range.
//
// The count may be as large as 2**31 for IPv4 or 2**127 for IPv6, so
// it is returned as a big.Int rather than an int. It returns nil for
// an invalid AddrMask.
func (am AddrMask) RangeCount() *big.Int {
	if !am.Valid() {
		return nil
	}
	return nBitRangeCount(u128From16(am.mask.As16()), uint8(am.bitLen()))
}

// nBitRangeCount returns the exact number of ranges produced by
// walkMaskRanges for an n-bit mask. All values use only the low n
// bits. The result is always a power of two: 2**j, where j is the
// number of wildcard bits at or above the lowest fixed bit. A mask of
// all zero bits and a mask of all one bits both yield one range.
func nBitRangeCount(mask uint128, n uint8) *big.Int {
	fixed, trailingZeros := popcountAndTrailingZeros(mask, n)
	// The lowest fixed bit is at trie index n-1-trailingZeros;
	// the wildcard bits that fragment the result are those with
	// index <= iLast, hence j = iLast+1-fixed.
	j := int(n) - trailingZeros - fixed
	if j < 0 {
		j = 0
	}
	return new(big.Int).Lsh(big.NewInt(1), uint(j))
}

// bitLen returns the address width in bits: 32 or 128.
func (am AddrMask) bitLen() int {
	if am.Is4() {
		return 32
	}
	return 128
}

// popcountAndTrailingZeros returns the number of set bits in the low
// n bits of mask and the count of consecutive zero bits in the least
// significant positions (capped at n).
func popcountAndTrailingZeros(mask uint128, n uint8) (fixed, trailingZeros int) {
	if n <= 64 {
		// Keep only the low n bits.
		var m uint64 = mask.lo
		if n < 64 {
			m &= (uint64(1) << n) - 1
		}
		tz := bits.TrailingZeros64(m)
		if tz > int(n) {
			tz = int(n)
		}
		return bits.OnesCount64(m), tz
	}
	fixed = bits.OnesCount64(mask.hi) + bits.OnesCount64(mask.lo)
	if mask.lo != 0 {
		trailingZeros = bits.TrailingZeros64(mask.lo)
	} else {
		trailingZeros = 64 + bits.TrailingZeros64(mask.hi)
	}
	if trailingZeros > int(n) {
		trailingZeros = int(n)
	}
	return fixed, trailingZeros
}

// ErrTooManyRanges is returned by AddrMask.IPSet when the exact
// number of ranges needed to represent the AddrMask exceeds the
// caller-supplied limit.
var ErrTooManyRanges = errors.New("AddrMask requires more IPRanges than the caller permits")

// IPSet returns an IPSet containing exactly the IP addresses matched
// by am, and the exact number of ranges in that representation.
//
// maxRanges bounds how many IPRanges the result may contain. If the
// exact range count, as returned by RangeCount, exceeds maxRanges,
// IPSet returns a nil set, the exact count, and an error wrapping
// ErrTooManyRanges without allocating or constructing the large
// result. Pass a negative maxRanges for no limit.
//
// Zones are not represented: the AddrMask constructor rejects them.
func (am AddrMask) IPSet(maxRanges int) (*IPSet, *big.Int, error) {
	count := am.RangeCount()
	if count == nil {
		return nil, nil, errors.New("IPSet: invalid AddrMask")
	}
	if maxRanges >= 0 && (!count.IsInt64() || count.Int64() > int64(maxRanges)) {
		return nil, count, fmt.Errorf("%w: need %s, max %d", ErrTooManyRanges, count, maxRanges)
	}
	rr := am.appendRanges(nil)
	return &IPSet{rr: rr}, count, nil
}

// Ranges returns the minimum and sorted set of IPRanges that covers
// exactly the addresses matched by am. It returns nil for an invalid
// AddrMask.
func (am AddrMask) Ranges() []IPRange {
	if !am.Valid() {
		return nil
	}
	return am.appendRanges(nil)
}

// appendRanges expands the AddrMask into disjoint, ascending IPRanges.
func (am AddrMask) appendRanges(dst []IPRange) []IPRange {
	if am.Is4() {
		a := am.addr.As4()
		m := am.mask.As4()
		base := uint128{0, uint64(a[0])<<24 | uint64(a[1])<<16 | uint64(a[2])<<8 | uint64(a[3])}
		mask := uint128{0, uint64(m[0])<<24 | uint64(m[1])<<16 | uint64(m[2])<<8 | uint64(m[3])}
		walkMaskRanges(base, mask, 32, func(lo, hi uint128) {
			dst = append(dst, IPRangeFrom(uint32IP(uint32(lo.lo)), uint32IP(uint32(hi.lo))))
		})
		return dst
	}
	walkMaskRanges(u128From16(am.addr.As16()), u128From16(am.mask.As16()), 128, func(lo, hi uint128) {
		dst = append(dst, IPRangeFrom(lo.IP6(), hi.IP6()))
	})
	return dst
}

// walkMaskRanges enumerates the minimum sorted set of disjoint value
// ranges covering exactly the n-bit (1 <= n <= 128) values v that
// satisfy v&mask == base, invoking emit for each inclusive [lo, hi]
// range. base and mask must use only the low n bits.
//
// It walks the prefix tree of the n-bit space, one bit at a time. A
// fixed bit (mask bit 1) keeps only the matching half; a wildcard bit
// (mask bit 0) keeps both halves, except when every remaining bit is
// also wildcard, in which case the whole subtree is one contiguous
// range and is emitted without descending. The work is proportional
// to the number of output ranges times the width, never to the number
// of individual addresses.
func walkMaskRanges(base, mask uint128, n uint8, emit func(lo, hi uint128)) {
	start := uint8(128) - n
	// allBits has the low n bits set, confining emitted suffixes to
	// the address width (matters for n < 128, e.g. IPv4 and tests).
	allBits := mask6[start].not()
	var walk func(bit uint8, prefix uint128)
	walk = func(bit uint8, prefix uint128) {
		d := start + bit
		if bit == n {
			emit(prefix, prefix)
			return
		}
		// mbit is the single address bit at this trie depth.
		mbit := mask6[d].xor(mask6[d+1])
		if mask.and(mbit).isZero() {
			// Wildcard bit. If all lower bits are wildcard too,
			// this subtree is the contiguous range
			// [prefix, prefix|(2**(n-bit)-1)].
			suffix := mask6[d].not().and(allBits)
			if mask.and(suffix).isZero() {
				emit(prefix, prefix.or(suffix))
				return
			}
			walk(bit+1, prefix)
			walk(bit+1, prefix.or(mbit))
		} else if !base.and(mbit).isZero() {
			walk(bit+1, prefix.or(mbit))
		} else {
			walk(bit+1, prefix)
		}
	}
	walk(0, uint128{})
}

// uint32IP converts a native 32-bit IPv4 value into a netip.Addr.
func uint32IP(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// String returns a stable representation of the AddrMask, of the form
// "addr/mask".
func (am AddrMask) String() string {
	if !am.Valid() {
		return "zero AddrMask"
	}
	return fmt.Sprintf("%v/%v", am.addr, am.mask)
}
