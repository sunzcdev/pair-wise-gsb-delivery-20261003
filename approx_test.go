// Copyright 2020 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"math/big"
	"math/bits"
	"net/netip"
	"reflect"
	"sort"
	"testing"
)

func TestApproxPrefixes(t *testing.T) {
	tests := []struct {
		name   string
		add    []string
		k      int
		want   []string
		extra  *big.Int
		wantOK bool
	}{
		{
			name:   "empty",
			k:      0,
			extra:  big.NewInt(0),
			wantOK: true,
		},
		{
			name:   "invalid k",
			add:    []string{"192.0.2.1/32"},
			k:      -1,
			wantOK: false,
		},
		{
			name:   "dual stack needs two",
			add:    []string{"192.0.2.1/32", "2001:db8::1/128"},
			k:      1,
			wantOK: false,
		},
		{
			name:   "ipv4 tie order",
			add:    []string{"0.0.0.0/32", "0.0.0.2/32", "0.0.0.13/32", "0.0.0.15/32"},
			k:      3,
			want:   []string{"0.0.0.0/30", "0.0.0.13/32", "0.0.0.15/32"},
			extra:  big.NewInt(2),
			wantOK: true,
		},
		{
			name:   "ipv6 sparse one",
			add:    []string{"::1/128", "2001:db8::1/128"},
			k:      1,
			want:   []string{"::/0"},
			extra:  new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(2)),
			wantOK: true,
		},
		{
			name:   "ipv6 sparse exact",
			add:    []string{"::1/128", "2001:db8::1/128"},
			k:      2,
			want:   []string{"::1/128", "2001:db8::1/128"},
			extra:  big.NewInt(0),
			wantOK: true,
		},
		{
			name:   "dual stack exact",
			add:    []string{"192.0.2.1/32", "2001:db8::1/128"},
			k:      2,
			want:   []string{"192.0.2.1/32", "2001:db8::1/128"},
			extra:  big.NewInt(0),
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b IPSetBuilder
			for _, prefix := range tt.add {
				b.AddPrefix(netip.MustParsePrefix(prefix))
			}
			s, err := b.IPSet()
			if err != nil {
				t.Fatal(err)
			}
			got, extra, ok := s.ApproxPrefixes(tt.k)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			var want []netip.Prefix
			if len(tt.want) != 0 {
				want = pxv(tt.want...)
			}
			if !((len(got) == 0 && len(want) == 0) || reflect.DeepEqual(got, want)) {
				t.Errorf("prefixes=%v, want %v", got, want)
			}
			if extra.Cmp(tt.extra) != 0 {
				t.Errorf("extra=%v, want %v", extra, tt.extra)
			}
		})
	}
}

func TestApproxPrefixesExactWhenKLarge(t *testing.T) {
	var b IPSetBuilder
	b.AddPrefix(netip.MustParsePrefix("192.0.2.0/24"))
	b.RemovePrefix(netip.MustParsePrefix("192.0.2.8/32"))
	b.AddPrefix(netip.MustParsePrefix("2001:db8::/64"))
	b.RemovePrefix(netip.MustParsePrefix("2001:db8::1/128"))
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}
	want := s.Prefixes()
	got, extra, ok := s.ApproxPrefixes(len(want))
	if !ok {
		t.Fatal("expected feasible result")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prefixes=%v, want exact %v", got, want)
	}
	if extra.Sign() != 0 {
		t.Errorf("extra=%v, want zero", extra)
	}
}

func TestApproxPrefixesExhaustiveTiny(t *testing.T) {
	const w = 4
	const universe = 1 << w

	exact := make([][]approxPrefix, universe)
	count := make([]int, universe)
	for mask := 1; mask < universe; mask++ {
		exact[mask] = tinyExactPrefixes(uint(mask), w)
		count[mask] = len(exact[mask])
	}
	indices := make([]int, universe)
	for i := range indices {
		indices[i] = i
	}
	sort.Slice(indices, func(i, j int) bool {
		a, b := exact[indices[i]], exact[indices[j]]
		for n := 0; n < len(a) && n < len(b); n++ {
			if cmp := cmp128(a[n].addr, b[n].addr); cmp != 0 {
				return cmp < 0
			}
			if a[n].bits != b[n].bits {
				return a[n].bits < b[n].bits
			}
		}
		return len(a) < len(b)
	})
	rank := make([]int, universe)
	for i, mask := range indices {
		rank[mask] = i
	}

	best := make([][w + 1]int, universe)
	for occupied := 1; occupied < universe; occupied++ {
		for k := 1; k <= w; k++ {
			best[occupied][k] = -1
			for cover := occupied; cover < universe; cover = (cover + 1) | occupied {
				if count[cover] > k {
					continue
				}
				current := best[occupied][k]
				extra := bits.OnesCount64(uint64(cover ^ occupied))
				if current < 0 ||
					extra < bits.OnesCount64(uint64(current^occupied)) ||
					extra == bits.OnesCount64(uint64(current^occupied)) &&
						(count[cover] < count[current] ||
							count[cover] == count[current] && rank[cover] < rank[current]) {
					best[occupied][k] = cover
				}
			}
		}
	}

	for occupied := 1; occupied < universe; occupied++ {
		family := newTestApproxFamily(exact[occupied], w, w)
		for k := 1; k <= w; k++ {
			wantMask := best[occupied][k]
			wantExtra := bits.OnesCount64(uint64(wantMask ^ occupied))
			got, extra, ok := family.approxPrefixesForTest(k)
			if !ok {
				t.Fatalf("occupied=%04b k=%d: infeasible", occupied, k)
			}
			want := tinyPrefixesForTest(exact[wantMask])
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("occupied=%04b k=%d: %v (extra=%d), want %v (extra=%d)",
					occupied, k, got, extra, want, wantExtra)
			}
			if extra.Cmp(big.NewInt(int64(wantExtra))) != 0 {
				t.Fatalf("occupied=%04b k=%d: extra=%d, want %d", occupied, k, extra, wantExtra)
			}
		}
	}
}

func tinyExactPrefixes(mask uint, w uint) []approxPrefix {
	var prefixes []approxPrefix
	var recurse func(start, size uint)
	recurse = func(start, size uint) {
		blockMask := uint((1 << size) - 1)
		block := (mask >> start) & blockMask
		if block == 0 {
			return
		}
		if block == blockMask {
			prefixes = append(prefixes, approxPrefix{
				addr: tinyAddr(start, w),
				bits: uint8(128 - size),
			})
			return
		}
		half := size / 2
		recurse(start, half)
		recurse(start+half, half)
	}
	recurse(0, uint(1)<<w)
	return prefixes
}

func tinyAddr(x uint, w uint) uint128 {
	return uint128{hi: uint64(x) << (64 - w)}
}

func tinyPrefixesForTest(prefixes []approxPrefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		out = append(out, netip.PrefixFrom(prefix.addr.IP6(), int(prefix.bits)))
	}
	return out
}

func (f approxFamily) approxPrefixesForTest(k int) ([]netip.Prefix, *big.Int, bool) {
	if f.required > k {
		return nil, nil, false
	}
	n := k
	if n > f.required {
		n = f.required
	}
	return f.reconstruct(nil, f.states[n]), f.extra(n), true
}

func BenchmarkApproxPrefixesIPv6Sparse(b *testing.B) {
	var builder IPSetBuilder
	for i := 0; i < 128; i++ {
		addr := uint128{hi: uint64(i) << 57, lo: uint64(i)}
		builder.AddPrefix(netip.PrefixFrom(addr.IP6(), 128))
	}
	s, err := builder.IPSet()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prefixes, extra, ok := s.ApproxPrefixes(16)
		if !ok || extra == nil || len(prefixes) != 16 {
			b.Fatalf("got %d prefixes, extra=%v, ok=%v", len(prefixes), extra, ok)
		}
	}
}
