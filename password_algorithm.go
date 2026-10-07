// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"errors"
	"fmt"
)

// PasswordAlgorithmType is the algorithm identifier used in
// PASSWORD-ALGORITHM and PASSWORD-ALGORITHMS attributes.
type PasswordAlgorithmType uint16

// Password algorithms from RFC 8489 Section 18.5.
const (
	PasswordAlgorithmMD5    PasswordAlgorithmType = 0x0001 // MD5
	PasswordAlgorithmSHA256 PasswordAlgorithmType = 0x0002 // SHA-256
)

func (t PasswordAlgorithmType) String() string {
	switch t {
	case PasswordAlgorithmMD5:
		return "MD5"
	case PasswordAlgorithmSHA256:
		return "SHA-256"
	default:
		return fmt.Sprintf("0x%x", uint16(t))
	}
}

// PasswordAlgorithm represents PASSWORD-ALGORITHM attribute.
//
// RFC 8489 Section 18.5.1.
type PasswordAlgorithm struct {
	Type       PasswordAlgorithmType
	Parameters []byte
}

func (p PasswordAlgorithm) String() string {
	return fmt.Sprintf("%s (parameters: 0x%x)", p.Type, p.Parameters)
}

// passwordAlgorithmHeaderSize is size of algorithm (2 bytes) and
// parameters length (2 bytes) fields.
const passwordAlgorithmHeaderSize = 4

// ErrBadPasswordAlgorithmSize means that PASSWORD-ALGORITHM or
// PASSWORD-ALGORITHMS attribute value has invalid length.
var ErrBadPasswordAlgorithmSize = errors.New("bad PASSWORD-ALGORITHM size")

// AddTo adds PASSWORD-ALGORITHM attribute to message.
func (p PasswordAlgorithm) AddTo(m *Message) error {
	v := make([]byte, passwordAlgorithmHeaderSize+len(p.Parameters))
	bin.PutUint16(v[0:2], uint16(p.Type))
	bin.PutUint16(v[2:4], uint16(len(p.Parameters))) //nolint:gosec // G115
	copy(v[passwordAlgorithmHeaderSize:], p.Parameters)
	m.Add(AttrPasswordAlgorithm, v)

	return nil
}

// GetFrom gets PASSWORD-ALGORITHM from message.
func (p *PasswordAlgorithm) GetFrom(m *Message) error {
	v, err := m.Get(AttrPasswordAlgorithm)
	if err != nil {
		return err
	}

	return p.unmarshal(v)
}

func (p *PasswordAlgorithm) unmarshal(v []byte) error {
	if len(v) < passwordAlgorithmHeaderSize {
		return ErrBadPasswordAlgorithmSize
	}
	paramsLen := int(bin.Uint16(v[2:4]))
	if len(v) != passwordAlgorithmHeaderSize+paramsLen {
		return ErrBadPasswordAlgorithmSize
	}
	p.Type = PasswordAlgorithmType(bin.Uint16(v[0:2]))
	if paramsLen > 0 {
		p.Parameters = v[passwordAlgorithmHeaderSize:]
	} else {
		p.Parameters = nil
	}

	return nil
}

// PasswordAlgorithms represents PASSWORD-ALGORITHMS attribute.
//
// RFC 8489 Section 18.5.2.
type PasswordAlgorithms []PasswordAlgorithm

func (p PasswordAlgorithms) String() string {
	s := ""
	for i, a := range p {
		if i > 0 {
			s += ", "
		}
		s += a.String()
	}

	return s
}

// AddTo adds PASSWORD-ALGORITHMS attribute to message.
func (p PasswordAlgorithms) AddTo(m *Message) error {
	var v []byte
	for _, a := range p {
		v = append(v, 0, 0, 0, 0)
		bin.PutUint16(v[len(v)-4:len(v)-2], uint16(a.Type))
		bin.PutUint16(v[len(v)-2:], uint16(len(a.Parameters))) //nolint:gosec // G115
		v = append(v, a.Parameters...)
	}
	m.Add(AttrPasswordAlgorithms, v)

	return nil
}

// GetFrom gets PASSWORD-ALGORITHMS from message.
func (p *PasswordAlgorithms) GetFrom(m *Message) error {
	v, err := m.Get(AttrPasswordAlgorithms)
	if err != nil {
		return err
	}
	*p = (*p)[:0]
	for len(v) > 0 {
		if len(v) < passwordAlgorithmHeaderSize {
			return ErrBadPasswordAlgorithmSize
		}
		paramsLen := int(bin.Uint16(v[2:4]))
		if len(v) < passwordAlgorithmHeaderSize+paramsLen {
			return ErrBadPasswordAlgorithmSize
		}
		a := PasswordAlgorithm{
			Type: PasswordAlgorithmType(bin.Uint16(v[0:2])),
		}
		if paramsLen > 0 {
			a.Parameters = v[passwordAlgorithmHeaderSize : passwordAlgorithmHeaderSize+paramsLen]
		}
		*p = append(*p, a)
		v = v[passwordAlgorithmHeaderSize+paramsLen:]
	}

	return nil
}
