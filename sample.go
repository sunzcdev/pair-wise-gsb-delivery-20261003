package netipx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/netip"
	"sort"
)

var (
	// ErrSampleNegativeCount is returned by IPSet.Sample when it is
	// asked for a negative number of addresses.
	ErrSampleNegativeCount = errors.New("netipx: Sample count must not be negative")

	// ErrSampleTooMany is returned by IPSet.Sample when it is asked
	// for more addresses than the IPSet contains.
	ErrSampleTooMany = errors.New("netipx: Sample count exceeds IPSet cardinality")
)

// Sample uniformly selects n distinct members of s without
// replacement, using randomness read from r.
//
// Every IP address in s has equal probability of being selected,
// regardless of which range or address family it belongs to: the
// members of s are treated as one ordered sequence, ranges are
// weighted by the number of addresses they contain (not the number
// of ranges), and large ranges such as ::/0 are handled without
// ever materializing the range.
//
// Random integers are drawn from r with rejection sampling, so a
// stream of uniform bytes yields a uniform selection without the
// modulo bias of taking random values modulo the population size.
// On any read error, including io.EOF when more bytes are needed,
// Sample returns a non-nil error wrapping the read failure and no
// partial results. If n exceeds the number of addresses in s, it
// returns ErrSampleTooMany.
//
// The returned addresses are sorted in ascending address order for
// reproducible output; that ordering is applied after selection and
// does not affect which addresses are selected or their
// distribution.
//
// If n is zero, Sample returns an empty slice and does not read
// from r.
func (s *IPSet) Sample(n int, r io.Reader) ([]netip.Addr, error) {
	if n < 0 {
		return nil, ErrSampleNegativeCount
	}
	if n == 0 {
		return []netip.Addr{}, nil
	}

	// Sizes of the normalized ranges, and the total cardinality.
	// The union of 0.0.0.0/0 and ::/0 has 2^32 + 2^128 members, which
	// does not fit in 128 bits, hence the uint129 bookkeeping.
	sizes := make([]uint129, len(s.rr))
	total := uint129{}
	for i, rng := range s.rr {
		size := ipRangeSize(rng)
		sizes[i] = size
		total = total.add(size)
	}

	want := uint129FromUint64(uint64(n))
	if total.less(want) {
		return nil, fmt.Errorf("%w: requested %d, IPSet contains %s addresses",
			ErrSampleTooMany, n, total.decimalString())
	}

	if r == nil {
		return nil, errors.New("netipx: Sample requires a non-nil random source")
	}

	// Partial Fisher–Yates shuffle over the implicit index space
	// [0, total). Each iteration draws an index in [0, remaining),
	// maps it to the address at that position, and records the swap
	// with the last still-live position without ever building the
	// full slice. Each unordered n-element subset is equally likely.
	swapped := make(map[uint129]uint129, n)
	out := make([]netip.Addr, 0, n)

	remaining := total
	addrAt := func(idx uint129) netip.Addr {
		for i, size := range sizes {
			if idx.less(size) {
				rng := s.rr[i]
				off := u128From16(rng.from.As16()).add128WithCarry(idx.lo, idx.hi)
				if rng.from.Is4() {
					return off.IP4()
				}
				return off.IP6()
			}
			idx = idx.sub(size)
		}
		panic("netipx: Sample index out of range")
	}

	for i := 0; i < n; i++ {
		k, err := randomUint129Below(r, remaining)
		if err != nil {
			return nil, fmt.Errorf("netipx: reading randomness for sample index: %w", err)
		}
		last := remaining.sub(uint129FromUint64(1))

		kIdx, kHit := swapped[k]
		if !kHit {
			kIdx = k
		}
		out = append(out, addrAt(kIdx))

		lastIdx, lastHit := swapped[last]
		if !lastHit {
			lastIdx = last
		}
		swapped[k] = lastIdx

		remaining = last
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out, nil
}

// uint129 is an unsigned 129-bit integer. It exists because the
// union of the entire IPv4 and IPv6 spaces has 2^32 + 2^128
// members, one bit more than a uint128 can hold.
type uint129 struct {
	hi uint8   // bit 128, always 0 or 1
	lo uint128 // bits 0..127
}

func uint129FromUint64(v uint64) uint129 {
	return uint129{lo: uint128{lo: v}}
}

func (u uint129) add(v uint129) uint129 {
	lo, carry := u.lo.add128(v.lo)
	return uint129{hi: u.hi + v.hi + uint8(carry), lo: lo}
}

func (u uint129) sub(v uint129) uint129 {
	lo, borrow := u.lo.sub128(v.lo)
	return uint129{hi: u.hi - v.hi - uint8(borrow), lo: lo}
}

func (u uint129) less(v uint129) bool {
	if u.hi != v.hi {
		return u.hi < v.hi
	}
	if u.lo.hi != v.lo.hi {
		return u.lo.hi < v.lo.hi
	}
	return u.lo.lo < v.lo.lo
}

// bitLen returns the number of bits required to represent u.
func (u uint129) bitLen() uint {
	if u.hi != 0 {
		return 129
	}
	return u.lo.bitLen()
}

func (u uint129) decimalString() string {
	var buf [17]byte
	buf[0] = u.hi
	binary.BigEndian.PutUint64(buf[1:9], u.lo.hi)
	binary.BigEndian.PutUint64(buf[9:17], u.lo.lo)
	return new(big.Int).SetBytes(buf[:]).String()
}

// randomUint129Below returns a uniformly random uint129 in [0, m)
// by reading whole bytes from r and rejecting values that would
// introduce modulo bias. m must be at least 1.
func randomUint129Below(r io.Reader, m uint129) (uint129, error) {
	nbytes := (m.bitLen() + 7) / 8
	buf := make([]byte, nbytes)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return uint129{}, err
		}
		// Mask the unused leading bits so the byte stream maps
		// uniformly onto [0, 2^bits), then reject values in
		// [m, 2^bits) to avoid modulo bias.
		if bits := m.bitLen(); bits%8 != 0 {
			buf[0] &= (1 << (bits % 8)) - 1
		}
		v := bytesTo129(buf)
		if v.less(m) {
			return v, nil
		}
	}
}

// bytesTo129 reads between 1 and 17 big-endian bytes into a uint129.
// With 17 bytes the first byte contributes bit 128; the caller is
// responsible for masking its unused high bits.
func bytesTo129(b []byte) uint129 {
	var v uint129
	switch {
	case len(b) == 17:
		v.hi = b[0]
		v.lo.hi = binary.BigEndian.Uint64(b[1:9])
		v.lo.lo = binary.BigEndian.Uint64(b[9:17])
	case len(b) > 8:
		head := b[:len(b)-8]
		var hiBuf [8]byte
		copy(hiBuf[8-len(head):], head)
		v.lo.hi = binary.BigEndian.Uint64(hiBuf[:])
		v.lo.lo = binary.BigEndian.Uint64(b[len(b)-8:])
	default:
		var loBuf [8]byte
		copy(loBuf[8-len(b):], b)
		v.lo.lo = binary.BigEndian.Uint64(loBuf[:])
	}
	return v
}

// ipRangeSize returns the number of addresses in rng as a uint129.
// A /0 range holds exactly 2^32 or 2^128 addresses, which is a
// uint129 with only hi set and lo == 0.
func ipRangeSize(rng IPRange) uint129 {
	from := u128From16(rng.from.As16())
	to := u128From16(rng.to.As16())
	diff, borrow := to.sub128(from)
	size, carry := diff.add128(uint128{lo: 1})
	return uint129{hi: uint8(carry) - uint8(borrow), lo: size}
}
