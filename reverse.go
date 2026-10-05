package netipx

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// ReverseZone is a DNS reverse-mapping zone and the IP prefix that it
// delegates.
type ReverseZone struct {
	name   string
	prefix netip.Prefix
}

// Name returns the fully qualified reverse zone name.
func (z ReverseZone) Name() string { return z.name }

// Prefix returns the IP prefix represented by the zone.
func (z ReverseZone) Prefix() netip.Prefix { return z.prefix }

// String returns the fully qualified reverse zone name.
func (z ReverseZone) String() string { return z.name }

// ReverseZoneCount returns the exact number of reverse zones required to
// represent s without covering addresses outside s.
func (s *IPSet) ReverseZoneCount() int {
	if s == nil {
		return 0
	}

	var count int
	var prefixes []netip.Prefix
	for _, r := range s.rr {
		prefixes = r.AppendPrefixes(prefixes[:0])
		for _, prefix := range prefixes {
			boundary := reverseZoneBoundaryBits(prefix)
			count += 1 << uint(boundary-prefix.Bits())
		}
	}
	return count
}

// ReverseZones returns the exact reverse DNS zones for s.
//
// IPv4 zones are aligned to octet boundaries (/0, /8, /16, /24, and
// /32), while IPv6 zones are aligned to nibble boundaries (/0, /4, /8,
// and so on through /128). Prefixes between two delegation boundaries
// are expanded to the next finer boundary. For example, a /25 expands
// to 128 host-level /32 zones rather than one /24.
//
// RFC 2317 classless in-addr.arpa delegation is not represented.
//
// Zones are returned in IPSet address order. If max is positive and the
// exact number of zones is greater than max, no zones are allocated and
// a *ReverseZoneLimitError is returned. A max of zero means no limit.
func (s *IPSet) ReverseZones(max int) ([]ReverseZone, error) {
	if max < 0 {
		return nil, errors.New("netipx: reverse zone limit must be non-negative")
	}

	count := s.ReverseZoneCount()
	if max > 0 && count > max {
		return nil, &ReverseZoneLimitError{Count: count, Max: max}
	}

	zones := make([]ReverseZone, 0, count)
	if s == nil {
		return zones, nil
	}

	var prefixes []netip.Prefix
	for _, r := range s.rr {
		prefixes = r.AppendPrefixes(prefixes[:0])
		for _, prefix := range prefixes {
			zones = appendPrefixReverseZones(zones, prefix)
		}
	}
	return zones, nil
}

// ReverseZoneLimitError reports that an IPSet requires more reverse
// zones than a caller is willing to accept.
type ReverseZoneLimitError struct {
	// Count is the exact number of zones required by the IPSet.
	Count int

	// Max is the caller's limit.
	Max int
}

func (e *ReverseZoneLimitError) Error() string {
	return fmt.Sprintf("netipx: %d reverse zones required, limit is %d", e.Count, e.Max)
}

func reverseZoneBoundaryBits(p netip.Prefix) int {
	bits := p.Bits()
	boundaryBits := 8
	if p.Addr().Is6() {
		boundaryBits = 4
	}
	if rem := bits % boundaryBits; rem != 0 {
		return bits + boundaryBits - rem
	}
	return bits
}

func appendPrefixReverseZones(dst []ReverseZone, p netip.Prefix) []ReverseZone {
	boundary := reverseZoneBoundaryBits(p)
	count := 1 << uint(boundary-p.Bits())
	addr := p.Masked().Addr()

	for i := 0; i < count; i++ {
		zone := newReverseZone(netip.PrefixFrom(addr, boundary))
		dst = append(dst, zone)
		if i+1 < count {
			addr = PrefixLastIP(zone.prefix).Next()
		}
	}
	return dst
}

func newReverseZone(p netip.Prefix) ReverseZone {
	p = p.Masked()
	var b strings.Builder

	if p.Addr().Is4() {
		octets := p.Addr().As4()
		labels := p.Bits() / 8
		b.Grow(labels*4 + len("in-addr.arpa."))
		for i := labels - 1; i >= 0; i-- {
			b.WriteString(strconv.Itoa(int(octets[i])))
			b.WriteByte('.')
		}
		b.WriteString("in-addr.arpa.")
	} else {
		raw := p.Addr().As16()
		labels := p.Bits() / 4
		b.Grow(labels*2 + len("ip6.arpa."))
		for i := labels - 1; i >= 0; i-- {
			nibble := raw[i/2] >> 4
			if i%2 == 1 {
				nibble = raw[i/2] & 0x0f
			}
			if nibble < 10 {
				b.WriteByte('0' + nibble)
			} else {
				b.WriteByte('a' + nibble - 10)
			}
			b.WriteByte('.')
		}
		b.WriteString("ip6.arpa.")
	}

	return ReverseZone{name: b.String(), prefix: p}
}

// ParseReverseZone parses a reverse DNS zone name and returns it with
// the corresponding masked IP prefix.
//
// Both in-addr.arpa and ip6.arpa names are accepted, with or without a
// trailing dot. Names must use canonical numeric labels.
func ParseReverseZone(name string) (ReverseZone, error) {
	qualified := name
	if strings.HasSuffix(qualified, ".") {
		qualified = strings.TrimSuffix(qualified, ".")
	}

	labels := strings.Split(qualified, ".")
	if len(labels) < 2 {
		return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", name)
	}

	suffixStart := len(labels) - 2
	reversed := labels[:suffixStart]
	if !strings.EqualFold(labels[suffixStart], "in-addr") &&
		!strings.EqualFold(labels[suffixStart], "ip6") ||
		!strings.EqualFold(labels[suffixStart+1], "arpa") {
		return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", name)
	}

	var p netip.Prefix
	switch strings.ToLower(labels[suffixStart]) {
	case "in-addr":
		if len(reversed) > 4 {
			return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", name)
		}
		var octets [4]byte
		for i, label := range reversed {
			value, err := strconv.Atoi(label)
			if err != nil || value < 0 || value > 255 || strconv.Itoa(value) != label {
				return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", name)
			}
			octets[len(reversed)-1-i] = byte(value)
		}
		p = netip.PrefixFrom(netip.AddrFrom4(octets), len(reversed)*8)
	case "ip6":
		if len(reversed) > 32 {
			return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", name)
		}
		var raw [16]byte
		for i, label := range reversed {
			if len(label) != 1 {
				return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", name)
			}
			value, ok := parseHexNibble(label[0])
			if !ok {
				return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", name)
			}
			nibbleIndex := len(reversed) - 1 - i
			if nibbleIndex%2 == 0 {
				raw[nibbleIndex/2] |= value << 4
			} else {
				raw[nibbleIndex/2] |= value
			}
		}
		p = netip.PrefixFrom(netip.AddrFrom16(raw), len(reversed)*4)
	}

	zone := newReverseZone(p.Masked())
	if !strings.EqualFold(zone.name, name) &&
		zone.name != strings.ToLower(qualified)+"." {
		return ReverseZone{}, fmt.Errorf("netipx: invalid reverse zone %q", name)
	}
	return zone, nil
}

func parseHexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}
