package netipx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net/netip"
	"strings"
	"testing"
)

// scriptReader serves a fixed, controllable sequence of random
// bytes and records how many bytes Sample consumed.
type scriptReader struct {
	script []byte
	pos    int
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.script) {
		return 0, io.EOF
	}
	n := copy(p, r.script[r.pos:])
	r.pos += n
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (r *scriptReader) fullyConsumed() bool { return r.pos == len(r.script) }

// errReader always fails with errBoom.
type errReader struct{}

var errBoom = errors.New("boom")

func (errReader) Read([]byte) (int, error) { return 0, errBoom }

// chunkReader serves data in fixed-size chunks, forcing callers to
// assemble reads across Read boundaries.
type chunkReader struct {
	data  []byte
	chunk int
	pos   int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}
	n = copy(p[:n], r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func u32Bytes(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// u128Bytes encodes v as exactly nbytes big-endian bytes.
func u128Bytes(v uint128, nbytes int) []byte {
	var full [16]byte
	binary.BigEndian.PutUint64(full[:8], v.hi)
	binary.BigEndian.PutUint64(full[8:], v.lo)
	return append([]byte(nil), full[16-nbytes:]...)
}

// u129Bytes encodes v as nbytes big-endian bytes, supporting up to
// 17 bytes (the leading byte carries bit 128 in its LSB position).
func u129Bytes(v uint129, nbytes int) []byte {
	b := u128Bytes(v.lo, 16)
	full := append([]byte{v.hi}, b...)
	return append([]byte(nil), full[17-nbytes:]...)
}

func mustSampleSet(t *testing.T, ranges []IPRange) *IPSet {
	t.Helper()
	var b IPSetBuilder
	for _, r := range ranges {
		b.AddRange(r)
	}
	s, err := b.IPSet()
	if err != nil {
		t.Fatalf("IPSet: %v", err)
	}
	return s
}

func assertAddrs(t *testing.T, got, want []netip.Addr) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d addrs %v, want %d addrs %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSampleZeroDoesNotRead(t *testing.T) {
	s := mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("192.0.2.0"), mustIP("192.0.2.2")),
	})
	r := &scriptReader{}
	got, err := s.Sample(0, r)
	if err != nil {
		t.Fatalf("Sample(0): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Sample(0) = %v, want empty", got)
	}
	if r.pos != 0 {
		t.Fatalf("Sample(0) consumed %d random bytes", r.pos)
	}

	// Zero samples from the empty set is also fine and reads nothing.
	var empty IPSet
	got, err = empty.Sample(0, &scriptReader{})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty Sample(0) = %v, %v", got, err)
	}
}

func TestSampleErrors(t *testing.T) {
	s := mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("192.0.2.0"), mustIP("192.0.2.1")),
	})

	r := &scriptReader{}
	if got, err := s.Sample(-1, r); !errors.Is(err, ErrSampleNegativeCount) {
		t.Fatalf("negative n: got %v, %v", got, err)
	}
	if r.pos != 0 {
		t.Fatalf("negative n consumed %d random bytes", r.pos)
	}

	got, err := s.Sample(3, r)
	if !errors.Is(err, ErrSampleTooMany) {
		t.Fatalf("too many: got %v, %v", got, err)
	}
	if got != nil {
		t.Fatalf("too many returned partial results: %v", got)
	}
	if r.pos != 0 {
		t.Fatalf("cardinality failure consumed %d random bytes", r.pos)
	}

	var empty IPSet
	if _, err := empty.Sample(1, &scriptReader{}); !errors.Is(err, ErrSampleTooMany) {
		t.Fatalf("empty set Sample(1): %v", err)
	}

	if _, err := s.Sample(1, nil); err == nil {
		t.Fatalf("nil random source: want error")
	}
}

func TestSampleTooManyReportsCardinality(t *testing.T) {
	s := mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("192.0.2.0"), mustIP("192.0.2.2")),
	})
	_, err := s.Sample(4, nil)
	if !errors.Is(err, ErrSampleTooMany) {
		t.Fatalf("cardinality: %v", err)
	}
	if !strings.Contains(err.Error(), "contains 3 addresses") {
		t.Fatalf("error %q does not report cardinality", err)
	}
}

func TestUint129DecimalString(t *testing.T) {
	tests := []struct {
		v    uint129
		want string
	}{
		{uint129FromUint64(0), "0"},
		{uint129FromUint64(1), "1"},
		{uint129{lo: uint128{lo: 0x100000000}}, "4294967296"},
		{uint129{hi: 1}, "340282366920938463463374607431768211456"},
		{uint129{hi: 1, lo: uint128{lo: 0x100000000}},
			"340282366920938463463374607436063178752"},
	}
	for _, tt := range tests {
		if got := tt.v.decimalString(); got != tt.want {
			t.Errorf("decimalString = %s, want %s", got, tt.want)
		}
		// Independently cross-check the same 17-byte encoding with
		// math/big.
		var buf [17]byte
		buf[0] = tt.v.hi
		binary.BigEndian.PutUint64(buf[1:9], tt.v.lo.hi)
		binary.BigEndian.PutUint64(buf[9:17], tt.v.lo.lo)
		bigV := new(big.Int).SetBytes(buf[:])
		if bigV.String() != tt.want {
			t.Errorf("math/big says %s, want %s", bigV, tt.want)
		}
	}
}

func TestSampleRejection(t *testing.T) {
	// Range of 3 members: modulus 3 rejects byte 3, then accepts 0.
	s := mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("192.0.2.0"), mustIP("192.0.2.2")),
	})
	r := &scriptReader{script: []byte{3, 0}}
	got, err := s.Sample(1, r)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("192.0.2.0")})
	if !r.fullyConsumed() {
		t.Fatalf("rejected byte was not re-read; consumed %d of %d bytes", r.pos, len(r.script))
	}

	// 2 draws from 3 members: draw sequence 2 (accept), then a
	// rejected byte (2 >= remaining 2), then 1.
	r = &scriptReader{script: []byte{2, 2, 1}}
	got, err = s.Sample(2, r)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	// First draw picks member 2 (.2); the swap moves member 1 into
	// slot 2, so the second draw of 1 yields member 1 (.1).
	assertAddrs(t, got, []netip.Addr{mustIP("192.0.2.1"), mustIP("192.0.2.2")})
	if !r.fullyConsumed() {
		t.Fatalf("consumed %d of %d bytes", r.pos, len(r.script))
	}
}

func TestSampleRejectThenFailure(t *testing.T) {
	s := mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("192.0.2.0"), mustIP("192.0.2.2")),
	})

	// EOF on the first read.
	if got, err := s.Sample(1, &scriptReader{}); !errors.Is(err, io.EOF) || got != nil {
		t.Fatalf("immediate EOF: got %v, %v", got, err)
	}

	// A reader error on the first read is propagated.
	if got, err := s.Sample(1, errReader{}); !errors.Is(err, errBoom) || got != nil {
		t.Fatalf("immediate error: got %v, %v", got, err)
	}

	// The first byte is rejected, then the re-read hits a clean
	// EOF: no partial result may be returned.
	if got, err := s.Sample(1, &scriptReader{script: []byte{3}}); !errors.Is(err, io.EOF) || got != nil {
		t.Fatalf("reject-then-EOF: got %v, %v", got, err)
	}

	// A clean EOF before any byte arrives, and a short read
	// mid-stream (ErrUnexpectedEOF), are both hard failures.
	if got, err := s.Sample(1, bytes.NewReader(nil)); !errors.Is(err, io.EOF) || got != nil {
		t.Fatalf("empty bytes.Reader: got %v, %v", got, err)
	}
	var b IPSetBuilder
	b.AddPrefix(mustIPPrefix("0.0.0.0/0"))
	v4set := buildIPSet(&b)
	if got, err := v4set.Sample(1, &scriptReader{script: []byte{0}}); !errors.Is(err, io.ErrUnexpectedEOF) || got != nil {
		t.Fatalf("short read: got %v, %v", got, err)
	}
}

func TestSampleCrossRangeAndSorted(t *testing.T) {
	// Two normalized ranges, members [1..2] and [4..5], total 4.
	s := mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.2")),
		IPRangeFrom(mustIP("10.0.0.4"), mustIP("10.0.0.5")),
	})

	// Script 2, 2: first draw index 2 (10.0.0.4) crosses into the
	// second range; the value at the last slot (10.0.0.5) is swapped
	// into slot 2, so the second draw of 2 reads through the swap map
	// to land in the second range. Output must be sorted.
	r := &scriptReader{script: []byte{2, 2}}
	got, err := s.Sample(2, r)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("10.0.0.4"), mustIP("10.0.0.5")})

	// Script 3, 0: first draw index 3 (10.0.0.5); second draw 0
	// (10.0.0.1). Raw draw order is descending, but Sample returns
	// addresses in ascending order.
	r = &scriptReader{script: []byte{3, 0}}
	got, err = s.Sample(2, r)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("10.0.0.1"), mustIP("10.0.0.5")})
}

func TestSampleCrossRangeSwap(t *testing.T) {
	// Members [1..4], script 0 then 0: the second draw reaches the
	// value swapped into slot 0 from the previously-live last slot,
	// i.e. 10.0.0.4, exercising the swap map across the full range.
	s := mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.4")),
	})
	r := &scriptReader{script: []byte{0, 0}}
	got, err := s.Sample(2, r)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("10.0.0.1"), mustIP("10.0.0.4")})

	// Members from different ranges reached through a swap: script
	// 1, 2 draws member .2 first, then the value (.4) swapped into
	// slot 2 when the live window shrank.
	s = mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.2")),
		IPRangeFrom(mustIP("10.0.0.4"), mustIP("10.0.0.5")),
	})
	r = &scriptReader{script: []byte{1, 2}}
	got, err = s.Sample(2, r)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("10.0.0.2"), mustIP("10.0.0.4")})
}

func TestSampleMaxAddresses(t *testing.T) {
	var b IPSetBuilder
	b.AddPrefix(mustIPPrefix("0.0.0.0/0"))
	v4set := buildIPSet(&b)
	// Modulus 2^32 has 33 significant bits, so 5 bytes are read and
	// the leading byte masks to one bit. A masked leading byte of 1
	// (v == 2^32) is rejected, then leading 0 plus ff ff ff ff
	// selects the max v4.
	rejectedDraw := make([]byte, 5)
	rejectedDraw[0] = 0x01
	acceptedDraw := append([]byte{0x00}, u32Bytes(0xffffffff)...)
	script := append(rejectedDraw, acceptedDraw...)
	r := &scriptReader{script: script}
	got, err := v4set.Sample(1, r)
	if err != nil {
		t.Fatalf("Sample reject-then-max v4: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("255.255.255.255")})
	if !r.fullyConsumed() {
		t.Fatalf("v4 rejection did not resample")
	}

	// Highest IPv6 address from ::/0: modulus is exactly 2^128, so
	// 17 bytes are read (129 significant bits). The masked leading
	// byte must be zero for the value to fall below 2^128; a leading
	// byte of 1 is rejected.
	b = IPSetBuilder{}
	b.AddPrefix(mustIPPrefix("::/0"))
	v6set := buildIPSet(&b)
	max128 := uint128{hi: ^uint64(0), lo: ^uint64(0)}
	r = &scriptReader{script: append([]byte{0x00}, u128Bytes(max128, 16)...)}
	got, err = v6set.Sample(1, r)
	if err != nil {
		t.Fatalf("Sample max v6: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{
		mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"),
	})

	// Same 17 bytes fed one byte at a time must give the same
	// result, so split reads do not change the distribution.
	cr := &chunkReader{data: append([]byte{0x00}, u128Bytes(max128, 16)...), chunk: 1}
	got, err = v6set.Sample(1, cr)
	if err != nil {
		t.Fatalf("Sample max v6 chunked: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{
		mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"),
	})

	// Union of both spaces: first range is v4 (2^32 members), second
	// is v6 (2^128). Index 2^32 is the first v6 member; index
	// 2^32+2^128-1 is the last v6 member. These exercise 129-bit
	// arithmetic at the boundary between address families.
	b = IPSetBuilder{}
	b.AddPrefix(mustIPPrefix("0.0.0.0/0"))
	b.AddPrefix(mustIPPrefix("::/0"))
	both := buildIPSet(&b)

	firstV6 := uint129{lo: uint128{lo: 0x100000000}}
	r = &scriptReader{script: u129Bytes(firstV6, 17)}
	got, err = both.Sample(1, r)
	if err != nil {
		t.Fatalf("Sample first v6 member: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("::")})

	// Reject an out-of-range 17-byte draw (index == total), then
	// accept ::. The 129-bit modulus masks the leading byte to 1 bit.
	rejected := uint129{hi: 1, lo: uint128{lo: 0x100000000}}
	r = &scriptReader{script: append(u129Bytes(rejected, 17), u129Bytes(firstV6, 17)...)}
	got, err = both.Sample(1, r)
	if err != nil {
		t.Fatalf("Sample rejected 129-bit draw: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("::")})
	if !r.fullyConsumed() {
		t.Fatalf("129-bit rejection did not resample; consumed %d of %d", r.pos, len(r.script))
	}

	// Maximum member of the union is the maximum IPv6 address, at
	// index total-1 = 2^128 + 2^32 - 1.
	lastV6 := uint129{hi: 1, lo: uint128{lo: 0xffffffff}}
	r = &scriptReader{script: u129Bytes(lastV6, 17)}
	got, err = both.Sample(1, r)
	if err != nil {
		t.Fatalf("Sample max union member: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{
		mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"),
	})

	// Two samples: indices 2^32 (first v6) and 0 (first v4). The
	// first draw crosses from the v4 range into the v6 range; after
	// it, 0 resolves through the swap map to the value moved there
	// (the first v4 member). Both draws need 17 bytes, since the
	// remaining population still has 129 significant bits. Output is
	// sorted v4-first.
	r = &scriptReader{script: append(u129Bytes(firstV6, 17), u129Bytes(uint129{}, 17)...)}
	got, err = both.Sample(2, r)
	if err != nil {
		t.Fatalf("Sample one per family: %v", err)
	}
	assertAddrs(t, got, []netip.Addr{mustIP("0.0.0.0"), mustIP("::")})
}

// TestSampleUniformitySmallSpace exhaustively walks every possible
// accepted random byte stream over tiny sets and checks that every
// n-element subset appears equally often. This is an exact
// distribution check rather than a flaky statistical assertion.
func TestSampleUniformitySmallSpace(t *testing.T) {
	tests := []struct {
		name   string
		ranges []IPRange
	}{
		{
			name: "single_range",
			ranges: []IPRange{
				IPRangeFrom(mustIP("192.0.2.0"), mustIP("192.0.2.2")),
			},
		},
		{
			name: "two_ranges_v4",
			ranges: []IPRange{
				IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.2")),
				IPRangeFrom(mustIP("10.0.0.4"), mustIP("10.0.0.5")),
			},
		},
		{
			name: "cross_family_equal_weight",
			ranges: []IPRange{
				IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.1")),
				IPRangeFrom(mustIP("2001:db8::1"), mustIP("2001:db8::1")),
			},
		},
		{
			name: "cross_family_skewed_weight",
			ranges: []IPRange{
				IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.1")),
				IPRangeFrom(mustIP("2001:db8::1"), mustIP("2001:db8::3")),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustSampleSet(t, tt.ranges)
			var members []netip.Addr
			for _, rng := range tt.ranges {
				for a := rng.from; a.Compare(rng.to) <= 0; a = a.Next() {
					members = append(members, a)
				}
			}
			n := len(members)

			for k := 1; k <= n; k++ {
				counts := map[string]int{}
				var successes int
				enumerateAcceptedStreams(n, k, func(stream []byte, weight int) {
					got, err := s.Sample(k, bytes.NewReader(stream))
					if err != nil {
						t.Fatalf("accepted stream failed: %v (%v)", err, stream)
					}
					if len(got) != k {
						t.Fatalf("got %d addrs, want %d", len(got), k)
					}
					for i := 1; i < len(got); i++ {
						if !got[i-1].Less(got[i]) {
							t.Fatalf("result not strictly ascending: %v", got)
						}
					}
					for _, a := range got {
						if !s.Contains(a) {
							t.Fatalf("sampled %v not in set", a)
						}
					}
					var sb strings.Builder
					for _, a := range got {
						sb.WriteString(a.String())
						sb.WriteByte('|')
					}
					counts[sb.String()] += weight
					successes += weight
				})

				if successes == 0 {
					t.Fatalf("k=%d: no accepted streams at all", k)
				}
				wantSubsets := 1
				for i := 0; i < k; i++ {
					wantSubsets = wantSubsets * (n - i) / (i + 1)
				}
				if len(counts) != wantSubsets {
					t.Fatalf("k=%d: observed %d distinct subsets, want %d: %v",
						k, len(counts), wantSubsets, counts)
				}
				var first int
				for _, c := range counts {
					if first == 0 {
						first = c
						continue
					}
					if c != first {
						t.Fatalf("k=%d: uneven subset frequencies: %v", k, counts)
					}
				}
			}
		})
	}
}

// enumerateAcceptedStreams walks the accepted values of each draw
// for the shrinking moduli n, n-1, ..., n-k+1 (all at most 4 here,
// so one byte per draw). Rejection sampling makes every accepted
// value in [0, mod) equiprobable: fn is therefore called once per
// accepted value with weight = 256^k, the common raw-byte
// denominator shared by every full accepted sequence. Rejection
// behavior itself is covered by TestSampleRejection and
// TestRandomUint129BelowMask.
func enumerateAcceptedStreams(n, k int, fn func(stream []byte, weight int)) {
	stream := make([]byte, k)
	weight := 1
	for i := 0; i < k; i++ {
		weight *= 256
	}
	var rec func(draw int)
	rec = func(draw int) {
		if draw == k {
			fn(append([]byte(nil), stream...), weight)
			return
		}
		for v := 0; v < n-draw; v++ {
			stream[draw] = byte(v)
			rec(draw + 1)
		}
	}
	rec(0)
}

func TestIPRangeSize(t *testing.T) {
	tests := []struct {
		from, to string
		want     uint129
	}{
		{"192.0.2.1", "192.0.2.1", uint129FromUint64(1)},
		{"192.0.2.1", "192.0.2.10", uint129FromUint64(10)},
		{"0.0.0.0", "255.255.255.255", uint129{lo: uint128{lo: 0x100000000}}},
		{"::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", uint129{hi: 1}},
		{"::", "::1", uint129FromUint64(2)},
	}
	for _, tt := range tests {
		r := IPRangeFrom(mustIP(tt.from), mustIP(tt.to))
		if got := ipRangeSize(r); got != tt.want {
			t.Errorf("size(%s..%s) = {%d %d %d}, want {%d %d %d}",
				tt.from, tt.to, got.hi, got.lo.hi, got.lo.lo,
				tt.want.hi, tt.want.lo.hi, tt.want.lo.lo)
		}
	}
}

func TestRandomUint129BelowMask(t *testing.T) {
	// Modulus 5 uses 3 bits: leading 5 bits must be masked off and
	// values >= 5 rejected. Bytes 0..4 are accepted as 0..4.
	for b := byte(0); b < 5; b++ {
		got, err := randomUint129Below(bytes.NewReader([]byte{b}), uint129FromUint64(5))
		if err != nil {
			t.Fatalf("byte %d: %v", b, err)
		}
		if got != uint129FromUint64(uint64(b)) {
			t.Fatalf("byte %d: got %v", b, got)
		}
	}
	// Byte 0b101 (5) is rejected; byte 0b1000_0101 (133) masks to
	// 0b101 (5) and is rejected too; byte 0b1000_0001 (129) masks to
	// 1 and is accepted, proving high bits are discarded.
	if _, err := randomUint129Below(bytes.NewReader([]byte{5, 0}), uint129FromUint64(5)); err != nil {
		t.Fatalf("rejected 5 then 0: %v", err)
	}
	got, err := randomUint129Below(bytes.NewReader([]byte{133, 129}), uint129FromUint64(5))
	if err != nil {
		t.Fatalf("masked 133 rejected, then 129: %v", err)
	}
	if got.lo.lo != 1 {
		t.Fatalf("133 masked: got %d, want 1", got.lo.lo)
	}

	// Modulus 2^128 needs 17 bytes (129 significant bits); with the
	// leading byte 0 all 128-bit values are accepted.
	r := &scriptReader{script: make([]byte, 17)}
	if _, err := randomUint129Below(r, uint129{hi: 1}); err != nil {
		t.Fatalf("2^128 modulus: %v", err)
	}
	if !r.fullyConsumed() {
		t.Fatalf("2^128 modulus unexpectedly rejected valid bytes")
	}
	// A leading byte that masks to 1 means v == 2^128, which is
	// rejected; the next 17-byte read with leading byte 0 succeeds.
	rejected128 := append([]byte{0x01}, make([]byte, 16)...)
	r = &scriptReader{script: append(rejected128, make([]byte, 17)...)}
	if _, err := randomUint129Below(r, uint129{hi: 1}); err != nil {
		t.Fatalf("2^128 modulus reject-then-accept: %v", err)
	}
	if !r.fullyConsumed() {
		t.Fatalf("2^128 modulus did not resample after rejection")
	}
}

func TestSampleDeterministic(t *testing.T) {
	s := mustSampleSet(t, []IPRange{
		IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.3")),
		IPRangeFrom(mustIP("2001:db8::1"), mustIP("2001:db8::2")),
	})
	script := []byte{4, 0}
	a, err := s.Sample(2, bytes.NewReader(script))
	if err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	b, err := s.Sample(2, bytes.NewReader(script))
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	assertAddrs(t, a, b)
}
