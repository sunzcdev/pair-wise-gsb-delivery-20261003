// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"errors"
	"fmt"
)

// PasswordAlgorithm is an algorithm used to encode a long-term password
// into the key of a STUN MESSAGE-INTEGRITY(-SHA256) attribute.
//
// RFC 8489 Section 15.3.
type PasswordAlgorithm uint16

// Password algorithms defined by RFC 8489 Section 15.3.
const (
	// PasswordAlgorithmReserved is the reserved algorithm value (0).
	PasswordAlgorithmReserved PasswordAlgorithm = 0x0000
	// PasswordAlgorithmMD5 derives the key with MD5 (RFC 5389).
	PasswordAlgorithmMD5 PasswordAlgorithm = 0x0001
	// PasswordAlgorithmSHA256 derives the key with SHA-256 (RFC 8489).
	PasswordAlgorithmSHA256 PasswordAlgorithm = 0x0002
)

func (a PasswordAlgorithm) String() string {
	switch a {
	case PasswordAlgorithmReserved:
		return "reserved"
	case PasswordAlgorithmMD5:
		return "MD5"
	case PasswordAlgorithmSHA256:
		return "SHA-256"
	default:
		return fmt.Sprintf("0x%x", uint16(a))
	}
}

const passwordAlgorithmHeaderSize = 4

// PasswordAlgorithmEntry is a single entry of the PASSWORD-ALGORITHMS
// attribute, or the whole value of the PASSWORD-ALGORITHM attribute.
//
// Algorithm is a 16-bit algorithm identifier and Parameters carries the
// algorithm-specific parameters.
//
// RFC 8489 Sections 15.3 and 15.4.
type PasswordAlgorithmEntry struct {
	Algorithm  PasswordAlgorithm
	Parameters []byte
}

func (e PasswordAlgorithmEntry) String() string {
	return fmt.Sprintf("%s: 0x%x", e.Algorithm, e.Parameters)
}

// PasswordAlgorithms represents the PASSWORD-ALGORITHMS attribute. It lists
// the algorithms the STUN agent supports for encoding the long-term password,
// ordered by decreasing preference.
//
// RFC 8489 Section 15.4.
type PasswordAlgorithms []PasswordAlgorithmEntry

// AddToAs serializes the list of algorithm entries into attribute t,
// checking that the encoded value fits into a single attribute.
func (a PasswordAlgorithms) AddToAs(m *Message, t AttrType) error {
	value := make([]byte, 0, len(a)*passwordAlgorithmHeaderSize)
	for _, algorithm := range a {
		value = bin.AppendUint16(value, uint16(algorithm.Algorithm))
		value = bin.AppendUint16(value, uint16(len(algorithm.Parameters))) //nolint:gosec // G115
		value = append(value, algorithm.Parameters...)
	}
	if err := CheckOverflow(t, len(value), 0xFFFF); err != nil {
		return err
	}
	m.Add(t, value)

	return nil
}

// AddTo adds PASSWORD-ALGORITHMS attribute to m.
func (a PasswordAlgorithms) AddTo(m *Message) error {
	return a.AddToAs(m, AttrPasswordAlgorithms)
}

// GetFromAs parses the value of attribute t into a.
func (a *PasswordAlgorithms) GetFromAs(m *Message, t AttrType) error {
	value, err := m.Get(t)
	if err != nil {
		return err
	}

	return a.decode(value, t)
}

// GetFrom parses PASSWORD-ALGORITHMS attribute from m.
func (a *PasswordAlgorithms) GetFrom(m *Message) error {
	return a.GetFromAs(m, AttrPasswordAlgorithms)
}

func (a *PasswordAlgorithms) decode(value []byte, t AttrType) error {
	*a = (*a)[:0]

	offset := 0
	for offset < len(value) {
		if len(value)-offset < passwordAlgorithmHeaderSize {
			return &DecodeErr{
				Place:   DecodeErrPlace{Parent: t.String(), Children: "header"},
				Message: "algorithm entry is truncated",
			}
		}

		algorithm := PasswordAlgorithm(bin.Uint16(value[offset : offset+2]))
		paramLen := int(bin.Uint16(value[offset+2 : offset+4])) //nolint:gosec // G115
		offset += passwordAlgorithmHeaderSize

		if paramLen > len(value)-offset {
			return &DecodeErr{
				Place:   DecodeErrPlace{Parent: t.String(), Children: "parameters"},
				Message: "algorithm parameters are truncated",
			}
		}

		var parameters []byte
		if paramLen > 0 {
			parameters = make([]byte, paramLen)
			copy(parameters, value[offset:offset+paramLen])
		}
		*a = append(*a, PasswordAlgorithmEntry{
			Algorithm:  algorithm,
			Parameters: parameters,
		})
		offset += paramLen
	}

	return nil
}

// PasswordAlgorithmValue represents the single PASSWORD-ALGORITHM attribute
// used by a STUN server to tell the client which password algorithm it
// selected for the challenge.
//
// RFC 8489 Section 15.3.
type PasswordAlgorithmValue struct {
	PasswordAlgorithmEntry
}

// NewPasswordAlgorithm returns PASSWORD-ALGORITHM value with the given
// algorithm and parameters.
func NewPasswordAlgorithm(algorithm PasswordAlgorithm, parameters []byte) PasswordAlgorithmValue {
	return PasswordAlgorithmValue{
		PasswordAlgorithmEntry: PasswordAlgorithmEntry{
			Algorithm:  algorithm,
			Parameters: parameters,
		},
	}
}

// AddTo adds PASSWORD-ALGORITHM attribute to m.
func (a PasswordAlgorithmValue) AddTo(m *Message) error {
	return PasswordAlgorithms{a.PasswordAlgorithmEntry}.AddToAs(m, AttrPasswordAlgorithm)
}

// GetFrom parses PASSWORD-ALGORITHM attribute from m.
func (a *PasswordAlgorithmValue) GetFrom(m *Message) error {
	var algorithms PasswordAlgorithms
	if err := algorithms.GetFromAs(m, AttrPasswordAlgorithm); err != nil {
		return err
	}
	if len(algorithms) != 1 {
		return errBadPasswordAlgorithm
	}
	a.PasswordAlgorithmEntry = algorithms[0]

	return nil
}

var errBadPasswordAlgorithm = errors.New("PASSWORD-ALGORITHM must contain a single algorithm")
