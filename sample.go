// Copyright 2023 The Go4 Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/bits"
	"net/netip"
	"sort"
)

// ErrSampleTooLarge is returned by IPSet.Sample when the requested
// number of addresses exceeds the number of addresses in the set.
var ErrSampleTooLarge = errors.New("requested sample size exceeds set size")

// Sample returns n distinct addresses drawn uniformly at random,
// without replacement, from the addresses in s.
//
// Randomness is read from rand, which must be a cryptographically
// secure source such as crypto/rand.Reader if cryptographic
// randomness is required.
//
// Every possible n-element subset of s is equally likely. In
// particular, ranges are weighted by the number of addresses they
// contain, not by the number of ranges: a range holding 2^128
// addresses is sampled from proportionally more often than one
// holding a handful, regardless of address family. The set is never
// enumerated, so sets covering large portions of IPv6 space can be
// sampled cheaply.
//
// The returned addresses are sorted in ascending order. Sorting does
// not alter the sampling distribution.
//
// If n is negative or greater than the number of addresses in s,
// Sample returns ErrSampleTooLarge (wrapped, for n negative) and
// consumes no bytes from rand. If n is zero, Sample returns an empty
// slice and consumes no bytes from rand. If rand fails before enough
// random bytes are available, including io.ErrUnexpectedEOF on a
// short read, the returned error is non-nil and no partial sample is
// returned.
func (s *IPSet) Sample(n int, rand io.Reader) ([]netip.Addr, error) {
	if n < 0 {
		return nil, fmt.Errorf("%w: sample size must not be negative", ErrSampleTooLarge)
	}
	if n == 0 {
		return []netip.Addr{}, nil
	}

	// starts[i] is the virtual index of the first member of the ith
	// range, i.e. the sum of the sizes of ranges 0..i-1. Members of
	// range i occupy virtual indices [starts[i], starts[i+1]).
	starts := make([]*big.Int, len(s.rr)+1)
	starts[0] = new(big.Int)
	for i, r := range s.rr {
		starts[i+1] = new(big.Int).Add(starts[i], rangeSize(r))
	}
	total := starts[len(starts)-1]

	if big.NewInt(int64(n)).Cmp(total) > 0 {
		return nil, fmt.Errorf("%w: set contains %s addresses, %d requested", ErrSampleTooLarge, total, n)
	}
	if rand == nil {
		return nil, errors.New("netipx: nil random source")
	}

	// Floyd's algorithm for sampling without replacement. At step t
	// (running from N-n+1 through N), draw j uniformly from [0, t);
	// if j was not already selected, insert j, otherwise insert
	// t-1. Every n-subset of the virtual index space is produced with
	// equal probability, and each draw is itself unbiased, so ranges
	// are weighted by their member counts rather than by the number
	// of ranges.
	selected := make(map[string]*big.Int, n)
	bound := new(big.Int).Sub(total, big.NewInt(int64(n)-1))
	one := big.NewInt(1)
	var buf []byte
	for i := 0; i < n; i++ {
		j, err := randInt(rand, bound, buf)
		if err != nil {
			return nil, err
		}
		buf = j.buf
		pick := j.val
		key := pick.String()
		if _, ok := selected[key]; !ok {
			selected[key] = new(big.Int).Set(pick)
		} else {
			end := new(big.Int).Sub(bound, one)
			selected[end.String()] = end
		}
		bound.Add(bound, one)
	}

	idx := make([]*big.Int, 0, n)
	for _, v := range selected {
		idx = append(idx, v)
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i].Cmp(idx[j]) < 0 })

	out := make([]netip.Addr, n)
	for i, v := range idx {
		out[i] = indexToAddr(s.rr, starts, v)
	}
	return out, nil
}

// randomIntResult is the value drawn by randInt together with the
// backing buffer it was decoded from, so callers can reuse the
// buffer across draws.
type randomIntResult struct {
	val *big.Int
	buf []byte
}

// randInt returns a uniformly distributed value in [0, bound) read
// from r. Uniformity is achieved by rejection sampling rather than
// reduction modulo bound, so bound need not divide 2^bitLen. The
// returned error wraps the underlying reader error, and is
// io.ErrUnexpectedEOF if r ends before producing a full draw.
func randInt(r io.Reader, bound *big.Int, buf []byte) (randomIntResult, error) {
	if bound.Sign() <= 0 {
		return randomIntResult{}, errors.New("netipx: internal error: non-positive sampling bound")
	}
	if bound.Cmp(big.NewInt(1)) == 0 {
		return randomIntResult{val: new(big.Int), buf: buf}, nil
	}

	byteLen := (bound.BitLen() + 7) / 8
	if cap(buf) < byteLen {
		buf = make([]byte, byteLen)
	} else {
		buf = buf[:byteLen]
	}

	// Mask off the unused high bits of the first byte so that draws
	// are uniform over [0, 2^bitLen). A draw is accepted only when it
	// falls in [0, limit), where limit is the largest multiple of
	// bound not exceeding 2^bitLen; accepted draws reduced modulo
	// bound are therefore uniform. Rejecting the tail instead of
	// reducing modulo bound avoids skew when bound does not divide
	// 2^bitLen.
	excess := uint(byteLen*8 - bound.BitLen())
	pow2 := new(big.Int).Lsh(big.NewInt(1), uint(bound.BitLen()))
	limit := new(big.Int).Mul(new(big.Int).Quo(pow2, bound), bound)
	val := new(big.Int)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return randomIntResult{buf: buf}, fmt.Errorf("netipx: reading random bytes: %w", err)
		}
		if excess > 0 {
			buf[0] &= byte(0xff) >> excess
		}
		val.SetBytes(buf)
		if val.Cmp(limit) < 0 {
			return randomIntResult{val: new(big.Int).Mod(val, bound), buf: buf}, nil
		}
	}
}

// rangeSize returns the number of addresses in r as a big.Int. The
// to-from difference plus one is computed via uint128 arithmetic so
// that a full-family range (whose difference wraps to 2^128-1) is
// reported correctly.
func rangeSize(r IPRange) *big.Int {
	from := u128From16(r.from.As16())
	to := u128From16(r.to.As16())
	diffLo, borrow := bits.Sub64(to.lo, from.lo, 0)
	diffHi, _ := bits.Sub64(to.hi, from.hi, borrow)
	diff := u128ToBig(uint128{hi: diffHi, lo: diffLo})
	return diff.Add(diff, big.NewInt(1))
}

// u128ToBig returns v as a non-negative big.Int.
func u128ToBig(v uint128) *big.Int {
	return new(big.Int).Or(
		new(big.Int).Lsh(new(big.Int).SetUint64(v.hi), 64),
		new(big.Int).SetUint64(v.lo),
	)
}

// indexToAddr maps a virtual member index into the set back to the
// concrete address at that position. ranges is sorted and
// non-overlapping, and starts[i] is the virtual index of ranges[i]'s
// first member.
func indexToAddr(ranges []IPRange, starts []*big.Int, index *big.Int) netip.Addr {
	i := sort.Search(len(starts)-1, func(i int) bool {
		return index.Cmp(starts[i+1]) < 0
	})
	offset := new(big.Int).Sub(index, starts[i])

	base := u128From16(ranges[i].from.As16())
	addr := new(big.Int).Add(u128ToBig(base), offset)
	ip := netip.AddrFrom16(addr16FromBig(addr))
	if ranges[i].from.Is4() {
		return ip.Unmap()
	}
	return ip
}

// addr16FromBig returns the 16-byte big-endian representation of v,
// which must fit in 128 bits.
func addr16FromBig(v *big.Int) [16]byte {
	var b [16]byte
	raw := v.Bytes()
	copy(b[16-len(raw):], raw)
	return b
}
