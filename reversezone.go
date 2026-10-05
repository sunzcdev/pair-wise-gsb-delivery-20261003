// Copyright 2023 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"fmt"
	"math"
	"math/bits"
	"net/netip"
	"strconv"
	"strings"
)

// ReverseZone is one DNS reverse-map delegation zone: a fully
// qualified reverse-lookup zone name together with the exact IP
// prefix the zone is delegated for.
//
// Zone boundaries follow the DNS reverse tree granularity: IPv4 zones
// end at octet boundaries (/8, /16, /24, /32) and IPv6 zones end at
// nibble boundaries (/4, /8, ..., /128). Prefixes that do not end on
// such a boundary expand into multiple boundary-aligned zones, so the
// union of the Prefix values is exactly the originating set and never
// larger. For example, a /25 expands into 128 host-level zones rather
// than one /24; RFC 2317 classless delegation is not generated.
type ReverseZone struct {
	// Zone is the fully qualified zone name, ending in a dot.
	Zone string
	// Prefix is the exact boundary-aligned prefix covered by Zone.
	Prefix netip.Prefix
}

// ReverseZoneLimitError reports that producing reverse zones for an
// IPSet would yield more zones than the caller-supplied limit.
//
// The error is returned before any result slice is allocated, so Got
// is the exact predicted number of zones rather than a post-hoc count.
type ReverseZoneLimitError struct {
	// Got is the predicted number of zones that would be produced.
	Got int64
	// Limit is the maxZones value supplied by the caller.
	Limit int64
}

func (e ReverseZoneLimitError) Error() string {
	return fmt.Sprintf("netipx: reverse zone count %d exceeds limit %d", e.Got, e.Limit)
}

// CountReverseZones returns the number of reverse zones that cover s,
// without producing them. A nil *IPSet and the empty set both yield 0.
//
// The result saturates at math.MaxInt64 rather than wrapping.
func (s *IPSet) CountReverseZones() int64 {
	if s == nil {
		return 0
	}
	return countReverseZones(s.Prefixes())
}

// ReverseZones returns the exact list of DNS reverse zones covering s.
//
// maxZones caps the number of zones produced; a non-positive value
// means no limit. If the predicted count exceeds maxZones, a
// ReverseZoneLimitError is returned before the result is allocated.
//
// Zones are emitted in IPSet address order: IPv4 zones first, then
// IPv6 zones, each family in ascending address order. A nil *IPSet or
// the empty set yields an empty (possibly nil) slice and no error.
//
// A prefix is decomposed only at reverse-tree boundaries: IPv4 prefixes
// are split at octet boundaries and IPv6 prefixes at nibble boundaries.
// RFC 2317 (classless in-addr.arpa delegation) is not implemented, so
// prefixes such as a /25 expand into individual host-level zones.
func (s *IPSet) ReverseZones(maxZones int) ([]ReverseZone, error) {
	return s.AppendReverseZones(nil, maxZones)
}

// AppendReverseZones is the append variant of ReverseZones: it appends
// the zones covering s to dst. If the predicted count exceeds maxZones
// it returns dst unmodified together with a ReverseZoneLimitError.
func (s *IPSet) AppendReverseZones(dst []ReverseZone, maxZones int) ([]ReverseZone, error) {
	if s == nil {
		return dst, nil
	}
	prefixes := s.Prefixes()
	count := countReverseZones(prefixes)
	if maxZones > 0 && count > int64(maxZones) {
		return dst, ReverseZoneLimitError{Got: count, Limit: int64(maxZones)}
	}
	if count == 0 {
		return dst, nil
	}
	out := dst
	// Pre-size only when the predicted length is representable as an
	// int; otherwise let append grow instead of panicking in make.
	if count <= int64(math.MaxInt)-int64(len(dst)) {
		out = make([]ReverseZone, len(dst), int64(len(dst))+count)
		copy(out, dst)
	}
	for _, p := range prefixes {
		out = appendReverseZonesForPrefix(out, p.Masked())
	}
	return out, nil
}

// reverseBoundary returns the smallest IPv4 octet boundary (multiple
// of 8) or IPv6 nibble boundary (multiple of 4) that is >= bits.
func reverseBoundary(bits int, is4 bool) int {
	step := 4
	if is4 {
		step = 8
	}
	return (bits + step - 1) / step * step
}

func reverseZoneCount(p netip.Prefix) int64 {
	is4 := p.Addr().Is4()
	maxBits := 128
	if is4 {
		maxBits = 32
	}
	boundary := reverseBoundary(p.Bits(), is4)
	if boundary > maxBits {
		boundary = maxBits
	}
	return int64(1) << uint(boundary-p.Bits())
}

func countReverseZones(prefixes []netip.Prefix) int64 {
	var n int64
	for _, p := range prefixes {
		add := reverseZoneCount(p)
		if n > math.MaxInt64-add {
			return math.MaxInt64
		}
		n += add
	}
	return n
}

// appendReverseZonesForPrefix emits the boundary-aligned zones whose
// prefixes partition p exactly.
func appendReverseZonesForPrefix(dst []ReverseZone, p netip.Prefix) []ReverseZone {
	is4 := p.Addr().Is4()
	maxBits := 128
	if is4 {
		maxBits = 32
	}
	bits := p.Bits()
	boundary := reverseBoundary(bits, is4)
	if boundary > maxBits {
		boundary = maxBits
	}
	nZones := 1 << uint(boundary-bits)

	cur := u128From16(p.Addr().As16())
	// IPv4 addresses occupy the low 32 bits of their v4-mapped
	// 16-byte representation, so their zone step is offset by 96 bits.
	stepBits := boundary
	if is4 {
		stepBits += 12 * 8
	}
	inc := u128ZonesStep(stepBits)
	for i := 0; i < nZones; i++ {
		var addr netip.Addr
		if is4 {
			addr = cur.IP4()
		} else {
			addr = cur.IP6()
		}
		zp := netip.PrefixFrom(addr, boundary)
		dst = append(dst, ReverseZone{reverseZoneName(zp), zp})
		cur = u128Add(cur, inc)
	}
	return dst
}

// u128ZonesStep returns the address distance between two adjacent zones
// ending at boundary: 1 << (128-boundary), or zero for boundary 0.
func u128ZonesStep(boundary int) uint128 {
	switch {
	case boundary == 0:
		return uint128{}
	case 128-boundary >= 64:
		return uint128{hi: uint64(1) << uint(64-boundary)}
	default:
		return uint128{lo: uint64(1) << uint(128-boundary)}
	}
}

func u128Add(u, v uint128) uint128 {
	lo, carry := bits.Add64(u.lo, v.lo, 0)
	hi, _ := bits.Add64(u.hi, v.hi, carry)
	return uint128{hi: hi, lo: lo}
}

// reverseZoneName builds the fully qualified reverse zone name for a
// boundary-aligned prefix. It is the inverse of ParseReverseZone.
func reverseZoneName(p netip.Prefix) string {
	if p.Addr().Is4() {
		b := p.Addr().As4()
		nLabels := p.Bits() / 8
		var sb strings.Builder
		for i := 0; i < nLabels; i++ {
			sb.WriteString(strconv.Itoa(int(b[nLabels-1-i])))
			sb.WriteByte('.')
		}
		sb.WriteString("in-addr.arpa.")
		return sb.String()
	}
	b := p.Addr().As16()
	nLabels := p.Bits() / 4
	var sb strings.Builder
	for i := 0; i < nLabels; i++ {
		// A nibble-aligned zone names exactly its nLabels network
		// nibbles, least significant of those first. These are the
		// nLabels most significant address nibbles (indices 0..n-1).
		nibble := msbNibble(b, nLabels-1-i)
		sb.WriteByte(hexLUT[nibble])
		sb.WriteByte('.')
	}
	sb.WriteString("ip6.arpa.")
	return sb.String()
}

var hexLUT = "0123456789abcdef"

// msbNibble returns nibble j of the address with nibbles numbered from
// the most significant end (j in 0..31): the high nibble of byte j/2
// when j is even, the low nibble when j is odd.
func msbNibble(b [16]byte, j int) byte {
	if j%2 == 0 {
		return b[j/2] >> 4
	}
	return b[j/2] & 0x0f
}

// setMsbNibble sets nibble j (numbered from the MSB; see msbNibble).
func setMsbNibble(b *[16]byte, j int, v byte) {
	if j%2 == 0 {
		b[j/2] = (b[j/2] & 0x0f) | (v << 4)
	} else {
		b[j/2] = (b[j/2] & 0xf0) | v
	}
}

// ParseReverseZone parses a fully qualified (or unqualified) DNS
// reverse zone name into the ReverseZone it delegates.
//
// Accepted names end in "in-addr.arpa" (0 to 4 dotted-octet labels) or
// "ip6.arpa" (0 to 32 dotted-nibble labels). The bare suffixes parse as
// 0.0.0.0/0 and ::/0 respectively. Comparison is case-insensitive and
// the trailing dot is optional; the returned Zone is always canonical
// (lowercase, trailing dot) and the returned Prefix is masked.
func ParseReverseZone(zone string) (ReverseZone, error) {
	name := strings.ToLower(zone)
	name = strings.TrimSuffix(name, ".")
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", zone)
	}
	n := len(labels)
	switch {
	case labels[n-2] == "in-addr" && labels[n-1] == "arpa":
		octets := labels[:n-2]
		if len(octets) > 4 {
			return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q: too many octet labels", zone)
		}
		var b [4]byte
		for i, label := range octets {
			v, ok := parseReverseByteLabel(label)
			if !ok {
				return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q: bad octet %q", zone, label)
			}
			b[len(octets)-1-i] = byte(v)
		}
		p := netip.PrefixFrom(netip.AddrFrom4(b), len(octets)*8)
		return ReverseZone{Zone: reverseZoneName(p), Prefix: p}, nil
	case labels[n-2] == "ip6" && labels[n-1] == "arpa":
		nibbles := labels[:n-2]
		if len(nibbles) > 32 {
			return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q: too many nibble labels", zone)
		}
		var b [16]byte
		for i, label := range nibbles {
			if len(label) != 1 {
				return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q: bad nibble %q", zone, label)
			}
			v, ok := parseReverseNibble(label[0])
			if !ok {
				return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q: bad nibble %q", zone, label)
			}
			setMsbNibble(&b, len(nibbles)-1-i, v)
		}
		p := netip.PrefixFrom(netip.AddrFrom16(b), len(nibbles)*4)
		return ReverseZone{Zone: reverseZoneName(p), Prefix: p}, nil
	default:
		return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", zone)
	}
}

// parseReverseByteLabel parses a canonical decimal octet label: it must
// be 1 to 3 digits, 0..255, with no leading zero (except "0" itself).
func parseReverseByteLabel(label string) (int, bool) {
	if len(label) < 1 || len(label) > 3 {
		return 0, false
	}
	v := 0
	for i := 0; i < len(label); i++ {
		c := label[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if len(label) > 1 && i == 0 && c == '0' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	if v > 255 {
		return 0, false
	}
	return v, true
}

func parseReverseNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}
