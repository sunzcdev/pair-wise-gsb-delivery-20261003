package ber

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const (
	// maxOIDArcs bounds the total number of arcs of an (absolute or
	// relative) object identifier, on both encode and decode.
	maxOIDArcs = 10000
	// maxOIDArcBits bounds the bit size of a single arc. Arcs are arbitrary
	// precision, but still capped so corrupt or malicious input cannot
	// force unbounded big.Int work. The merged first subidentifier of an
	// absolute OID (40*arc0 + arc1) counts as one arc against this limit.
	maxOIDArcBits = 512
)

// OIDBigFromString parses a dotted-decimal OID string (e.g. "1.2.840") into
// arbitrary-precision arcs. Every arc must be a non-negative decimal integer
// of at most maxOIDArcBits bits; empty and negative arcs are rejected. The
// result may be passed to [EncodeOIDBig] or [EncodeRelativeOIDBig].
func OIDBigFromString(oidString string) ([]*big.Int, error) {
	return parseDottedBigInts(oidString, "OID")
}

// OIDBigToString formats arbitrary-precision arcs as a dotted-decimal OID
// string. It is the inverse of [OIDBigFromString].
func OIDBigToString(oid []*big.Int) string {
	var s strings.Builder
	s.Grow(32)

	for i, arc := range oid {
		if i > 0 {
			s.WriteByte('.')
		}
		if arc == nil {
			s.WriteString("<nil>")
			continue
		}
		s.WriteString(arc.String())
	}

	return s.String()
}

// EncodeOIDBig encodes an absolute object identifier given as
// arbitrary-precision arcs into DER content octets. The constraint on the
// first two arcs (arc0 <= 2, and arc1 < 40 when arc0 < 2) is validated and
// the two arcs are merged (40*arc0 + arc1) in arbitrary precision, so arcs
// beyond int64 round-trip losslessly.
func EncodeOIDBig(oid []*big.Int) ([]byte, error) {
	if len(oid) < 2 {
		return nil, fmt.Errorf("invalid object identifier %v", oid)
	}
	if err := checkArcs(oid); err != nil {
		return nil, err
	}

	arc0, arc1 := oid[0], oid[1]
	two := big.NewInt(2)
	if arc0.Cmp(two) > 0 || (arc0.Cmp(two) < 0 && arc1.Cmp(big.NewInt(40)) >= 0) {
		return nil, fmt.Errorf("invalid object identifier %v", oid)
	}

	// The first two arcs are combined into a single value: 40*arc0 + arc1.
	merged := new(big.Int).Mul(arc0, big.NewInt(40))
	merged.Add(merged, arc1)
	if merged.BitLen() > maxOIDArcBits {
		return nil, fmt.Errorf("invalid object identifier %v: merged first arcs exceed %d bits", oid, maxOIDArcBits)
	}

	encoded := appendBase128Big(nil, merged)
	for _, arc := range oid[2:] {
		encoded = appendBase128Big(encoded, arc)
	}

	return encoded, nil
}

// EncodeRelativeOIDBig encodes a relative object identifier given as
// arbitrary-precision arcs into DER content octets, using the same base-128
// logic as [EncodeOIDBig].
func EncodeRelativeOIDBig(oid []*big.Int) ([]byte, error) {
	if len(oid) == 0 {
		return nil, errors.New("invalid relative object identifier: no arcs")
	}
	if err := checkArcs(oid); err != nil {
		return nil, err
	}

	var encoded []byte
	for _, arc := range oid {
		encoded = appendBase128Big(encoded, arc)
	}

	return encoded, nil
}

// DecodeOIDBig decodes the DER content octets of an absolute OBJECT
// IDENTIFIER into arbitrary-precision arcs.
func DecodeOIDBig(bytes []byte) ([]*big.Int, error) {
	if len(bytes) == 0 {
		return nil, errors.New("zero length OBJECT IDENTIFIER")
	}

	// The first varint is 40*arc0 + arc1:
	// According to this packing, arc0 can take the values 0, 1 and 2 only.
	// When arc0 = 0 or arc0 = 1, then arc1 is <= 39. When arc0 = 2,
	// then there are no restrictions on arc1.
	v, offset, err := parseBase128Big(bytes, 0)
	if err != nil {
		return nil, err
	}

	arcs := make([]*big.Int, 0, min(len(bytes)+1, maxOIDArcs))
	if v.Cmp(big.NewInt(80)) < 0 {
		arc0, arc1 := new(big.Int), new(big.Int)
		arc0.QuoRem(v, big.NewInt(40), arc1)
		arcs = append(arcs, arc0, arc1)
	} else {
		arcs = append(arcs, big.NewInt(2), new(big.Int).Sub(v, big.NewInt(80)))
	}

	return decodeRemainingArcs(bytes, offset, arcs)
}

// DecodeRelativeOIDBig decodes the DER content octets of a RELATIVE-OID into
// arbitrary-precision arcs, using the same base-128 logic as [DecodeOIDBig].
func DecodeRelativeOIDBig(bytes []byte) ([]*big.Int, error) {
	if len(bytes) == 0 {
		return nil, errors.New("zero length RELATIVE OBJECT IDENTIFIER")
	}

	return decodeRemainingArcs(bytes, 0, make([]*big.Int, 0, min(len(bytes), maxOIDArcs)))
}

// parseDottedBigInts splits a dotted-decimal OID string (e.g. "1.2.840") into
// its arbitrary-precision arcs. label names the OID kind for error messages.
func parseDottedBigInts(oidString, label string) ([]*big.Int, error) {
	parts := strings.Split(oidString, ".")
	if len(parts) > maxOIDArcs {
		return nil, fmt.Errorf("%s has too many arcs (%d > %d)", label, len(parts), maxOIDArcs)
	}

	oid := make([]*big.Int, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("invalid %s part '%s': empty arc", label, part)
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return nil, fmt.Errorf("invalid %s part '%s': not a non-negative decimal integer", label, part)
			}
		}
		arc, ok := new(big.Int).SetString(part, 10)
		if !ok {
			return nil, fmt.Errorf("invalid %s part '%s'", label, part)
		}
		if err := checkArcSize(arc); err != nil {
			return nil, fmt.Errorf("invalid %s part '%s': %w", label, part, err)
		}
		oid[i] = arc
	}

	return oid, nil
}

// checkArcs validates every arc of an OID about to be encoded.
func checkArcs(oid []*big.Int) error {
	if len(oid) > maxOIDArcs {
		return fmt.Errorf("object identifier has too many arcs (%d > %d)", len(oid), maxOIDArcs)
	}
	for _, arc := range oid {
		if arc == nil {
			return errors.New("object identifier has a nil arc")
		}
		if arc.Sign() < 0 {
			return fmt.Errorf("object identifier has a negative arc %v", arc)
		}
		if err := checkArcSize(arc); err != nil {
			return err
		}
	}
	return nil
}

func checkArcSize(arc *big.Int) error {
	if arc.BitLen() > maxOIDArcBits {
		return fmt.Errorf("arc exceeds %d bits", maxOIDArcBits)
	}
	return nil
}

// decodeRemainingArcs decodes successive base-128 arcs from bytes[offset:],
// appending them to arcs.
func decodeRemainingArcs(bytes []byte, offset int, arcs []*big.Int) ([]*big.Int, error) {
	for offset < len(bytes) {
		if len(arcs) == maxOIDArcs {
			return nil, fmt.Errorf("object identifier has too many arcs (>%d)", maxOIDArcs)
		}
		arc, next, err := parseBase128Big(bytes, offset)
		if err != nil {
			return nil, err
		}
		arcs = append(arcs, arc)
		offset = next
	}
	return arcs, nil
}

// appendBase128Big appends the minimal base-128 (varint) encoding of the
// non-negative arbitrary-precision integer n to dst. For values that fit in
// an int64 the encoding is identical to the historical int64-based encoder.
func appendBase128Big(dst []byte, n *big.Int) []byte {
	groups := (n.BitLen() + 6) / 7
	if groups == 0 {
		groups = 1
	}

	for i := groups - 1; i >= 0; i-- {
		var o byte
		for j := 0; j < 7; j++ {
			o |= byte(n.Bit(i*7+j)) << uint(j)
		}
		if i != 0 {
			o |= 0x80
		}
		dst = append(dst, o)
	}

	return dst
}

// parseBase128Big parses a base-128 encoded arbitrary-precision integer from
// the given offset in the given byte slice. It returns the value and the new
// offset. Non-minimally encoded, unterminated, and oversized integers are
// rejected.
func parseBase128Big(bytes []byte, initOffset int) (ret *big.Int, offset int, err error) {
	offset = initOffset
	ret = new(big.Int)
	tmp := new(big.Int)
	for shifted := 0; offset < len(bytes); shifted++ {
		b := bytes[offset]
		// integers should be minimally encoded, so the leading octet should
		// never be 0x80
		if shifted == 0 && b == 0x80 {
			return nil, 0, errors.New("integer is not minimally encoded")
		}
		ret.Lsh(ret, 7)
		tmp.SetInt64(int64(b & 0x7f))
		ret.Or(ret, tmp)
		offset++
		if ret.BitLen() > maxOIDArcBits {
			return nil, 0, errors.New("base 128 integer too large")
		}
		if b&0x80 == 0 {
			return ret, offset, nil
		}
	}
	return nil, 0, errors.New("truncated base 128 integer")
}

// encodeOID takes a string representation of an OID and returns its DER-encoded byte slice along with any error.
func encodeOID(oidString string) ([]byte, error) {
	oid, err := parseDottedBigInts(oidString, "OID")
	if err != nil {
		return nil, err
	}
	return EncodeOIDBig(oid)
}

func encodeRelativeOID(oidString string) ([]byte, error) {
	oid, err := parseDottedBigInts(oidString, "RELATIVE OID")
	if err != nil {
		return nil, err
	}
	return EncodeRelativeOIDBig(oid)
}

func OIDToString(oi []int) string {
	var s strings.Builder
	s.Grow(32)

	buf := make([]byte, 0, 19)
	for i, v := range oi {
		if i > 0 {
			s.WriteByte('.')
		}
		s.Write(strconv.AppendInt(buf, int64(v), 10))
	}

	return s.String()
}

// parseObjectIdentifier parses an OBJECT IDENTIFIER from the given bytes and
// returns it. An object identifier is a sequence of variable length integers
// that are assigned in a hierarchy. Arcs that do not fit in an int are
// rejected; use [DecodeOIDBig] to decode arbitrary-precision arcs.
func parseObjectIdentifier(bytes []byte) ([]int, error) {
	arcs, err := DecodeOIDBig(bytes)
	if err != nil {
		return nil, err
	}
	return bigArcsToInts(arcs)
}

func parseRelativeObjectIdentifier(bytes []byte) ([]int, error) {
	arcs, err := DecodeRelativeOIDBig(bytes)
	if err != nil {
		return nil, err
	}
	return bigArcsToInts(arcs)
}

// bigArcsToInts converts arbitrary-precision arcs to ints, rejecting any arc
// that does not fit in an int on all platforms.
func bigArcsToInts(arcs []*big.Int) ([]int, error) {
	out := make([]int, len(arcs))
	for i, arc := range arcs {
		if !arc.IsInt64() || arc.Int64() > math.MaxInt32 {
			return nil, errors.New("base 128 integer too large")
		}
		out[i] = int(arc.Int64())
	}
	return out, nil
}
