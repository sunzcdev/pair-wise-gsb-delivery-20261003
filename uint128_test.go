// Copyright 2020 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"testing"
)

func TestUint128AddSub(t *testing.T) {
	const add1 = 1
	const sub1 = -1
	tests := []struct {
		in   uint128
		op   int // +1 or -1 to add vs subtract
		want uint128
	}{
		{uint128{0, 0}, add1, uint128{0, 1}},
		{uint128{0, 1}, add1, uint128{0, 2}},
		{uint128{1, 0}, add1, uint128{1, 1}},
		{uint128{0, ^uint64(0)}, add1, uint128{1, 0}},
		{uint128{^uint64(0), ^uint64(0)}, add1, uint128{0, 0}},

		{uint128{0, 0}, sub1, uint128{^uint64(0), ^uint64(0)}},
		{uint128{0, 1}, sub1, uint128{0, 0}},
		{uint128{0, 2}, sub1, uint128{0, 1}},
		{uint128{1, 0}, sub1, uint128{0, ^uint64(0)}},
		{uint128{1, 1}, sub1, uint128{1, 0}},
	}
	for _, tt := range tests {
		var got uint128
		switch tt.op {
		case add1:
			got = tt.in.addOne()
		case sub1:
			got = tt.in.subOne()
		default:
			panic("bogus op")
		}
		if got != tt.want {
			t.Errorf("%v add %d = %v; want %v", tt.in, tt.op, got, tt.want)
		}
	}
}

func TestUint129Arithmetic(t *testing.T) {
	max128 := uint128{hi: ^uint64(0), lo: ^uint64(0)}
	total := u129From128(max128).addOne().add(u129From128(uint128{lo: 1 << 32}))
	if total != (uint129{lo: uint128{lo: 1 << 32}, top: 1}) {
		t.Fatalf("dual-family address count = %v; want 2^128+2^32", total)
	}

	tests := []struct {
		name string
		in   uint129
		d    uint64
		quot uint129
		rem  uint64
	}{
		{
			name: "small",
			in:   uint129{lo: uint128{lo: 8}},
			d:    3,
			quot: uint129{lo: uint128{lo: 2}},
			rem:  2,
		},
		{
			name: "full family",
			in:   u129From128(max128).addOne(),
			d:    2,
			quot: uint129{lo: uint128{hi: 1 << 63}},
			rem:  0,
		},
		{
			name: "dual-family total halved",
			in:   total,
			d:    2,
			quot: uint129{lo: uint128{hi: 1 << 63, lo: 1 << 31}},
			rem:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotQuot, gotRem := tt.in.divmod(tt.d)
			if gotQuot != tt.quot || gotRem != tt.rem {
				t.Fatalf("divmod = (%v, %d); want (%v, %d)", gotQuot, gotRem, tt.quot, tt.rem)
			}
		})
	}
}

func TestBitsSetFrom(t *testing.T) {
	tests := []struct {
		bit  uint8
		want uint128
	}{
		{0, uint128{^uint64(0), ^uint64(0)}},
		{1, uint128{^uint64(0) >> 1, ^uint64(0)}},
		{63, uint128{1, ^uint64(0)}},
		{64, uint128{0, ^uint64(0)}},
		{65, uint128{0, ^uint64(0) >> 1}},
		{127, uint128{0, 1}},
		{128, uint128{0, 0}},
	}
	for _, tt := range tests {
		var zero uint128
		got := zero.bitsSetFrom(tt.bit)
		if got != tt.want {
			t.Errorf("0.bitsSetFrom(%d) = %064b want %064b", tt.bit, got, tt.want)
		}
	}
}

func TestBitsClearedFrom(t *testing.T) {
	tests := []struct {
		bit  uint8
		want uint128
	}{
		{0, uint128{0, 0}},
		{1, uint128{1 << 63, 0}},
		{63, uint128{^uint64(0) &^ 1, 0}},
		{64, uint128{^uint64(0), 0}},
		{65, uint128{^uint64(0), 1 << 63}},
		{127, uint128{^uint64(0), ^uint64(0) &^ 1}},
		{128, uint128{^uint64(0), ^uint64(0)}},
	}
	for _, tt := range tests {
		ones := uint128{^uint64(0), ^uint64(0)}
		got := ones.bitsClearedFrom(tt.bit)
		if got != tt.want {
			t.Errorf("ones.bitsClearedFrom(%d) = %064b want %064b", tt.bit, got, tt.want)
		}
	}
}
