// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"bytes"
	"encoding/binary"
	"math"
	"strconv"

	"github.com/x448/float16"
)

// DeterministicRule names a deterministic encoding rule that can be checked
// by DeterministicProfile.Validate.
type DeterministicRule string

const (
	// RuleShortestHead requires integers, lengths, and tag numbers to use the
	// shortest form of the head (the initial byte and its argument).
	RuleShortestHead DeterministicRule = "shortest-head"

	// RuleShortestFloat requires floating-point values to use the shortest
	// width that preserves the value, and NaN to be encoded as 0xf97e00.
	RuleShortestFloat DeterministicRule = "shortest-float"

	// RuleDefiniteLength forbids indefinite-length strings, arrays, and maps.
	RuleDefiniteLength DeterministicRule = "definite-length"

	// RuleMapKeyOrder requires map keys to be sorted according to the
	// profile's MapKeyOrderMode.
	RuleMapKeyOrder DeterministicRule = "map-key-order"

	// RuleNoDuplicateMapKey forbids duplicate map keys.
	RuleNoDuplicateMapKey DeterministicRule = "no-duplicate-map-key"
)

// DeterministicError describes the first violation of a deterministic
// encoding rule found by DeterministicProfile.Validate.
type DeterministicError struct {
	// Offset is the byte offset of the first data item that violates the rule.
	Offset int

	// Rule is the name of the violated deterministic encoding rule.
	Rule DeterministicRule

	detail string
}

func (e *DeterministicError) Error() string {
	return "cbor: deterministic encoding violation at offset " + strconv.Itoa(e.Offset) +
		" (" + string(e.Rule) + "): " + e.detail
}

// MapKeyOrderMode specifies the required ordering of map keys.
type MapKeyOrderMode int

const (
	// MapKeyOrderNone doesn't require map keys to be sorted.
	MapKeyOrderNone MapKeyOrderMode = iota

	// MapKeyOrderLengthFirst requires map keys to be sorted by encoded length
	// first, then bytewise lexicographically. This is the ordering required by
	// RFC 8949 Section 4.2.1 (Core Deterministic Encoding) and by RFC 7049
	// Section 3.9 (Canonical CBOR).
	MapKeyOrderLengthFirst

	// MapKeyOrderLexicographic requires map keys to be sorted bytewise
	// lexicographically by their encoded bytes.
	MapKeyOrderLexicographic

	maxMapKeyOrderMode
)

func (m MapKeyOrderMode) valid() bool {
	return m >= 0 && m < maxMapKeyOrderMode
}

// DeterministicProfile specifies which deterministic encoding rules are
// enforced when validating an encoded CBOR data item.
//
// Validation scans the encoded bytes directly. It never decodes the data
// into Go values and never re-encodes anything, so acceptance can't be
// influenced by encoder behavior.
type DeterministicProfile struct {
	// ShortestHead requires integers, lengths, and tag numbers to use the
	// shortest form of the head.
	ShortestHead bool

	// ShortestFloat requires floating-point values to use the shortest width
	// that preserves the value, and NaN to be encoded as 0xf97e00.
	ShortestFloat bool

	// DefiniteLengthOnly forbids indefinite-length strings, arrays, and maps.
	DefiniteLengthOnly bool

	// MapKeyOrder specifies the required ordering of map keys.
	MapKeyOrder MapKeyOrderMode

	// RejectDuplicateMapKeys forbids duplicate map keys.
	RejectDuplicateMapKeys bool
}

// CoreDeterministicProfile returns the profile for Core Deterministic
// Encoding as defined in RFC 8949 Section 4.2.1.
func CoreDeterministicProfile() DeterministicProfile {
	return DeterministicProfile{
		ShortestHead:           true,
		ShortestFloat:          true,
		DefiniteLengthOnly:     true,
		MapKeyOrder:            MapKeyOrderLengthFirst,
		RejectDuplicateMapKeys: true,
	}
}

// CanonicalDeterministicProfile returns the profile for Canonical CBOR as
// defined in RFC 7049 Section 3.9. Unlike Core Deterministic Encoding, it
// doesn't require floats to use the shortest width.
func CanonicalDeterministicProfile() DeterministicProfile {
	return DeterministicProfile{
		ShortestHead:           true,
		DefiniteLengthOnly:     true,
		MapKeyOrder:            MapKeyOrderLengthFirst,
		RejectDuplicateMapKeys: true,
	}
}

// CTAP2DeterministicProfile returns the profile for the CTAP2 canonical CBOR
// encoding form. Floats aren't used by CTAP2, so float shortening isn't
// required.
func CTAP2DeterministicProfile() DeterministicProfile {
	return DeterministicProfile{
		ShortestHead:           true,
		DefiniteLengthOnly:     true,
		MapKeyOrder:            MapKeyOrderLengthFirst,
		RejectDuplicateMapKeys: true,
	}
}

// Validate checks whether data is a single well-formed CBOR data item that
// conforms to this deterministic encoding profile.
//
// Validate returns nil if data conforms to the profile. If data isn't
// well-formed CBOR (or has extraneous trailing data), the well-formedness
// error (e.g. *SyntaxError, io.ErrUnexpectedEOF, *ExtraneousDataError) is
// returned instead of a *DeterministicError. Otherwise, the returned error is
// a *DeterministicError describing the first violation, including its byte
// offset and rule name.
func (p DeterministicProfile) Validate(data []byte) error {
	if !p.MapKeyOrder.valid() {
		return &DeterministicError{Offset: 0, Rule: RuleMapKeyOrder, detail: "invalid MapKeyOrderMode " + strconv.Itoa(int(p.MapKeyOrder))}
	}
	if err := defaultDecMode.Wellformed(data); err != nil {
		return err
	}
	s := deterministicScanner{data: data, p: p}
	return s.scanItem()
}

// deterministicScanner scans well-formed CBOR data and reports the first
// deterministic encoding violation. Input is guaranteed to be well-formed by
// a well-formedness pass before scanning, so depth is bounded by the default
// decoder's max nested levels.
type deterministicScanner struct {
	data []byte
	off  int
	p    DeterministicProfile
}

func (s *deterministicScanner) scanItem() error { //nolint:gocyclo
	headStart := s.off
	t, ai := parseInitialByte(s.data[s.off])
	s.off++

	if additionalInformation(ai).isIndefiniteLength() {
		if s.p.DefiniteLengthOnly {
			return &DeterministicError{
				Offset: headStart,
				Rule:   RuleDefiniteLength,
				detail: "indefinite-length " + t.String() + " isn't allowed",
			}
		}
		return s.scanIndefiniteItem(t)
	}

	val, err := s.readArgument(ai)
	if err != nil {
		return err
	}

	if s.p.ShortestHead && t != cborTypePrimitives && violatesShortestHead(ai, val) {
		return &DeterministicError{
			Offset: headStart,
			Rule:   RuleShortestHead,
			detail: t.String() + " doesn't use the shortest form of the head",
		}
	}

	switch t {
	case cborTypeByteString, cborTypeTextString:
		s.off += int(val) //nolint:gosec // well-formedness pass already verified the length

	case cborTypeArray:
		for i := uint64(0); i < val; i++ {
			if err := s.scanItem(); err != nil {
				return err
			}
		}

	case cborTypeMap:
		return s.scanMapPairs(val)

	case cborTypeTag:
		return s.scanItem()

	case cborTypePrimitives:
		if s.p.ShortestFloat && violatesShortestFloat(ai, val) {
			return &DeterministicError{
				Offset: headStart,
				Rule:   RuleShortestFloat,
				detail: "floating-point value doesn't use the shortest form that preserves the value",
			}
		}
	}
	return nil
}

// scanIndefiniteItem scans an indefinite-length string, array, or map. It is
// only called when the profile allows indefinite-length items.
func (s *deterministicScanner) scanIndefiniteItem(t cborType) error {
	switch t {
	case cborTypeByteString, cborTypeTextString:
		// Chunks are definite-length data items and are checked individually.
		for !s.atBreak() {
			if err := s.scanItem(); err != nil {
				return err
			}
		}
		s.off++

	case cborTypeArray:
		for !s.atBreak() {
			if err := s.scanItem(); err != nil {
				return err
			}
		}
		s.off++

	case cborTypeMap:
		var prevKey []byte
		seen := s.newSeenMap()
		for !s.atBreak() {
			key, err := s.scanMapKey(prevKey, seen)
			if err != nil {
				return err
			}
			prevKey = key
			if err := s.scanItem(); err != nil {
				return err
			}
		}
		s.off++
	}
	return nil
}

func (s *deterministicScanner) scanMapPairs(numPairs uint64) error {
	var prevKey []byte
	seen := s.newSeenMap()
	for i := uint64(0); i < numPairs; i++ {
		key, err := s.scanMapKey(prevKey, seen)
		if err != nil {
			return err
		}
		prevKey = key
		if err := s.scanItem(); err != nil {
			return err
		}
	}
	return nil
}

// newSeenMap returns a map used to detect duplicate keys when keys aren't
// sorted, or nil when duplicates are detected by comparing adjacent keys.
func (s *deterministicScanner) newSeenMap() map[string]struct{} {
	if s.p.MapKeyOrder == MapKeyOrderNone && s.p.RejectDuplicateMapKeys {
		return make(map[string]struct{})
	}
	return nil
}

// scanMapKey scans one map key and checks it against the previous key (or the
// set of all previous keys when keys aren't sorted). It returns the key's
// encoded bytes.
func (s *deterministicScanner) scanMapKey(prevKey []byte, seen map[string]struct{}) ([]byte, error) {
	keyStart := s.off
	if err := s.scanItem(); err != nil {
		return nil, err
	}
	key := s.data[keyStart:s.off]

	if s.p.MapKeyOrder == MapKeyOrderNone {
		if s.p.RejectDuplicateMapKeys {
			k := string(key)
			if _, ok := seen[k]; ok {
				return nil, &DeterministicError{
					Offset: keyStart,
					Rule:   RuleNoDuplicateMapKey,
					detail: "map key is a duplicate of a previous key",
				}
			}
			seen[k] = struct{}{}
		}
		return key, nil
	}

	if prevKey != nil {
		cmp := compareMapKeys(prevKey, key, s.p.MapKeyOrder)
		if cmp == 0 && s.p.RejectDuplicateMapKeys {
			return nil, &DeterministicError{
				Offset: keyStart,
				Rule:   RuleNoDuplicateMapKey,
				detail: "map key is a duplicate of the previous key",
			}
		}
		if cmp >= 0 {
			return nil, &DeterministicError{
				Offset: keyStart,
				Rule:   RuleMapKeyOrder,
				detail: "map keys aren't in sorted order",
			}
		}
	}
	return key, nil
}

// readArgument consumes the head argument (0, 1, 2, 4, or 8 bytes) and
// returns its value. Input is well-formed, so the argument is always present.
func (s *deterministicScanner) readArgument(ai byte) (uint64, error) {
	switch {
	case ai <= maxAdditionalInformationWithoutArgument:
		return uint64(ai), nil
	case ai == additionalInformationWith1ByteArgument:
		val := uint64(s.data[s.off])
		s.off++
		return val, nil
	case ai == additionalInformationWith2ByteArgument:
		val := uint64(binary.BigEndian.Uint16(s.data[s.off:]))
		s.off += 2
		return val, nil
	case ai == additionalInformationWith4ByteArgument:
		val := uint64(binary.BigEndian.Uint32(s.data[s.off:]))
		s.off += 4
		return val, nil
	default: // ai == additionalInformationWith8ByteArgument
		val := binary.BigEndian.Uint64(s.data[s.off:])
		s.off += 8
		return val, nil
	}
}

func (s *deterministicScanner) atBreak() bool {
	return isBreakFlag(s.data[s.off])
}

// violatesShortestHead reports whether a head with the given additional
// information and argument value isn't in shortest form. It doesn't apply to
// major type 7 (simple values and floats).
func violatesShortestHead(ai byte, val uint64) bool {
	switch ai {
	case additionalInformationWith1ByteArgument:
		return val < 24
	case additionalInformationWith2ByteArgument:
		return val <= 0xff
	case additionalInformationWith4ByteArgument:
		return val <= 0xffff
	case additionalInformationWith8ByteArgument:
		return val <= 0xffffffff
	}
	return false
}

// violatesShortestFloat reports whether a float16/32/64 isn't in the shortest
// form that preserves its value. NaN is only shortest as 0xf97e00.
func violatesShortestFloat(ai byte, val uint64) bool {
	switch ai {
	case additionalInformationAsFloat16:
		f16 := float16.Frombits(uint16(val))
		return f16.IsNaN() && val != 0x7e00

	case additionalInformationAsFloat32:
		f32 := math.Float32frombits(uint32(val))
		if math.IsNaN(float64(f32)) {
			return true
		}
		p := float16.PrecisionFromfloat32(f32)
		if p == float16.PrecisionUnknown {
			// Roundtrip float32->float16->float32 to determine if float32 can
			// fit into float16.
			if float16.Fromfloat32(f32).Float32() == f32 {
				p = float16.PrecisionExact
			}
		}
		return p == float16.PrecisionExact

	case additionalInformationAsFloat64:
		f64 := math.Float64frombits(val)
		if math.IsNaN(f64) {
			return true
		}
		return float64(float32(f64)) == f64
	}
	return false
}

// compareMapKeys compares two encoded map keys according to the ordering
// mode, returning a negative number, 0, or a positive number.
func compareMapKeys(a, b []byte, mode MapKeyOrderMode) int {
	if mode == MapKeyOrderLengthFirst {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
	}
	return bytes.Compare(a, b)
}
