// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strconv"

	"github.com/x448/float16"
)

// DeterministicProfile identifies a named set of deterministic encoding rules
// used by ValidateDeterministic to check whether received CBOR bytes are
// encoded in a deterministic form.
type DeterministicProfile int

const (
	// ProfileCoreDeterministic enforces "Core Deterministic Encoding"
	// defined in RFC 8949 Section 4.2.1:
	//   - integers, lengths, and tag numbers use the shortest possible form
	//   - floating-point values use the preferred (shortest) form and NaN is 0xf97e00
	//   - byte strings, text strings, arrays, and maps are definite-length
	//   - map keys are sorted in the bytewise lexicographic order of their
	//     deterministic encodings (duplicate keys are not allowed)
	ProfileCoreDeterministic DeterministicProfile = iota

	// ProfileCanonical enforces "Canonical CBOR" defined in RFC 7049 Section 3.9:
	//   - integers, lengths, and tag numbers use the shortest possible form
	//   - byte strings, text strings, arrays, and maps are definite-length
	//   - map keys are sorted length-first, then bytewise lexicographically
	//     (duplicate keys are not allowed)
	//
	// Unlike ProfileCoreDeterministic, RFC 7049 canonical CBOR does not
	// constrain floating-point value widths.
	ProfileCanonical
)

func (p DeterministicProfile) valid() bool {
	return p == ProfileCoreDeterministic || p == ProfileCanonical
}

// Rule names identifying the deterministic encoding rule violated,
// reported in DeterministicError.Rule.
const (
	// RuleShortestNumericForm indicates an integer, length, tag number, or
	// floating-point value is not encoded in its shortest (preferred) form.
	RuleShortestNumericForm = "shortest-numeric-form"

	// RuleContainerForm indicates an indefinite-length byte string, text
	// string, array, or map is used where a definite-length container is
	// required.
	RuleContainerForm = "container-form"

	// RuleMapKeyOrder indicates map keys are not sorted in the order
	// required by the selected profile, or a map contains duplicate keys.
	RuleMapKeyOrder = "map-key-order"
)

// DeterministicError describes the first violation of the selected
// deterministic encoding profile found in well-formed CBOR data.
type DeterministicError struct {
	// Offset is the byte offset of the head of the data item (or map key)
	// that violates the rule.
	Offset int

	// Rule is the name of the violated rule, one of RuleShortestNumericForm,
	// RuleContainerForm, or RuleMapKeyOrder.
	Rule string

	msg string
}

func (e *DeterministicError) Error() string {
	return "cbor: deterministic encoding violation at offset " + strconv.Itoa(e.Offset) +
		" (" + e.Rule + "): " + e.msg
}

// ValidateDeterministic checks whether data is a single well-formed CBOR data
// item encoded in compliance with the selected deterministic encoding profile.
//
// The received bytes are inspected directly; the data is never decoded and
// re-encoded to decide conformance.
//
// The result is one of three outcomes:
//   - nil: data conforms to the selected profile.
//   - *DeterministicError: data is well-formed CBOR but not deterministic;
//     the error reports the byte offset and rule name of the first violation.
//   - any other error (io.EOF, io.ErrUnexpectedEOF, *SyntaxError,
//     *ExtraneousDataError, resource limit errors, etc.): data is malformed
//     or exceeds default limits (see DecMode.Wellformed).
func ValidateDeterministic(data []byte, profile DeterministicProfile) error {
	if !profile.valid() {
		return errors.New("cbor: invalid deterministic profile " + strconv.Itoa(int(profile)))
	}
	if err := defaultDecMode.Wellformed(data); err != nil {
		return err
	}
	c := detChecker{data: data, profile: profile}
	return c.checkItem()
}

// detChecker scans well-formed CBOR data and reports the first violation of
// the selected deterministic encoding profile.
// Precondition: data contains exactly one well-formed CBOR data item.
type detChecker struct {
	data    []byte
	off     int
	profile DeterministicProfile
}

func (c *detChecker) checkItem() error {
	headOffset := c.off
	t, ai, val := c.head()

	if err := c.checkShortestForm(headOffset, t, ai, val); err != nil {
		return err
	}

	switch t {
	case cborTypeByteString, cborTypeTextString:
		if additionalInformation(ai).isIndefiniteLength() {
			return &DeterministicError{
				Offset: headOffset,
				Rule:   RuleContainerForm,
				msg:    "indefinite-length " + t.String() + " is not allowed",
			}
		}
		c.off += int(val) //nolint:gosec

	case cborTypeArray:
		if additionalInformation(ai).isIndefiniteLength() {
			return &DeterministicError{
				Offset: headOffset,
				Rule:   RuleContainerForm,
				msg:    "indefinite-length array is not allowed",
			}
		}
		for i := uint64(0); i < val; i++ {
			if err := c.checkItem(); err != nil {
				return err
			}
		}

	case cborTypeMap:
		if additionalInformation(ai).isIndefiniteLength() {
			return &DeterministicError{
				Offset: headOffset,
				Rule:   RuleContainerForm,
				msg:    "indefinite-length map is not allowed",
			}
		}
		var prevKey []byte
		for i := uint64(0); i < val; i++ {
			keyOffset := c.off
			if err := c.checkItem(); err != nil {
				return err
			}
			key := c.data[keyOffset:c.off]
			if prevKey != nil && !c.keysOrdered(prevKey, key) {
				return &DeterministicError{
					Offset: keyOffset,
					Rule:   RuleMapKeyOrder,
					msg:    "map keys are not sorted in the order required by the selected profile (or are duplicated)",
				}
			}
			prevKey = key
			if err := c.checkItem(); err != nil {
				return err
			}
		}

	case cborTypeTag:
		// Check tag content.
		return c.checkItem()
	}

	return nil
}

// head parses the head of the data item at c.off and advances the offset past
// it, returning the major type, additional information, and head argument.
// Precondition: the data item is well-formed.
func (c *detChecker) head() (t cborType, ai byte, val uint64) {
	t, ai = parseInitialByte(c.data[c.off])
	c.off++
	switch ai {
	case additionalInformationWith1ByteArgument:
		val = uint64(c.data[c.off])
		c.off++
	case additionalInformationWith2ByteArgument:
		val = uint64(binary.BigEndian.Uint16(c.data[c.off : c.off+2]))
		c.off += 2
	case additionalInformationWith4ByteArgument:
		val = uint64(binary.BigEndian.Uint32(c.data[c.off : c.off+4]))
		c.off += 4
	case additionalInformationWith8ByteArgument:
		val = binary.BigEndian.Uint64(c.data[c.off : c.off+8])
		c.off += 8
	default:
		val = uint64(ai)
	}
	return t, ai, val
}

// checkShortestForm checks that integers, lengths, tag numbers, and
// floating-point values are encoded in their shortest (preferred) form.
func (c *detChecker) checkShortestForm(offset int, t cborType, ai byte, val uint64) error {
	if ai < additionalInformationWith1ByteArgument || ai > additionalInformationWith8ByteArgument {
		return nil
	}

	if t == cborTypePrimitives {
		if c.profile != ProfileCoreDeterministic {
			return nil
		}
		switch ai {
		case additionalInformationAsFloat16:
			// Preferred serialization requires NaN to be encoded as 0xf97e00.
			if isNaN16(uint16(val)) && val != 0x7e00 {
				return &DeterministicError{
					Offset: offset,
					Rule:   RuleShortestNumericForm,
					msg:    "NaN is not encoded in preferred form 0xf97e00",
				}
			}
		case additionalInformationAsFloat32:
			f := math.Float32frombits(uint32(val))
			if math.IsNaN(float64(f)) {
				return &DeterministicError{
					Offset: offset,
					Rule:   RuleShortestNumericForm,
					msg:    "NaN is not encoded in preferred form 0xf97e00",
				}
			}
			if float16.Fromfloat32(f).Float32() == f {
				return &DeterministicError{
					Offset: offset,
					Rule:   RuleShortestNumericForm,
					msg:    "float32 value is representable as float16 and is not in shortest form",
				}
			}
		case additionalInformationAsFloat64:
			f := math.Float64frombits(val)
			if math.IsNaN(f) {
				return &DeterministicError{
					Offset: offset,
					Rule:   RuleShortestNumericForm,
					msg:    "NaN is not encoded in preferred form 0xf97e00",
				}
			}
			if float64(float32(f)) == f {
				return &DeterministicError{
					Offset: offset,
					Rule:   RuleShortestNumericForm,
					msg:    "float64 value is representable as float32 and is not in shortest form",
				}
			}
		}
		return nil
	}

	// Integers, string lengths, container lengths, and tag numbers must use
	// the smallest argument size that can hold the value.
	var min uint64
	switch ai {
	case additionalInformationWith1ByteArgument:
		min = 24
	case additionalInformationWith2ByteArgument:
		min = 1 << 8
	case additionalInformationWith4ByteArgument:
		min = 1 << 16
	case additionalInformationWith8ByteArgument:
		min = 1 << 32
	}
	if val < min {
		return &DeterministicError{
			Offset: offset,
			Rule:   RuleShortestNumericForm,
			msg:    t.String() + " value " + strconv.FormatUint(val, 10) + " is not encoded in shortest form",
		}
	}
	return nil
}

// keysOrdered reports whether prev sorts strictly before cur under the
// selected profile's map key ordering.
func (c *detChecker) keysOrdered(prev, cur []byte) bool {
	if c.profile == ProfileCanonical {
		// RFC 7049 Section 3.9: shorter keys sort first; keys of equal
		// length sort in bytewise lexicographic order.
		if len(prev) != len(cur) {
			return len(prev) < len(cur)
		}
		return bytes.Compare(prev, cur) < 0
	}
	// RFC 8949 Section 4.2.1: keys sort in bytewise lexicographic order of
	// their deterministic encodings.
	return bytes.Compare(prev, cur) < 0
}

// isNaN16 reports whether bits is the bit representation of a float16 NaN.
func isNaN16(bits uint16) bool {
	return bits&0x7c00 == 0x7c00 && bits&0x03ff != 0
}
