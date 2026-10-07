// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"errors"
	"fmt"
)

// XMLVersion selects the version of the W3C XML recommendation used when
// reading or writing a document.
type XMLVersion int

const (
	// VersionUnspecified selects the default, backward-compatible behavior.
	// Reads use the Go standard library's encoding/xml decoder, and writes
	// emit no XML declaration. This is the zero value of XMLVersion.
	VersionUnspecified XMLVersion = 0

	// Version10 selects the strict XML 1.0 (Fifth Edition) rules.
	Version10 XMLVersion = 1

	// Version11 selects the strict XML 1.1 (Second Edition) rules.
	Version11 XMLVersion = 2
)

// valid returns true if the version selects a strict XML mode (1.0 or 1.1).
func (v XMLVersion) valid() bool {
	return v == Version10 || v == Version11
}

// String returns the version's XML declaration string, e.g. "1.1".
func (v XMLVersion) String() string {
	switch v {
	case Version10:
		return "1.0"
	case Version11:
		return "1.1"
	default:
		return ""
	}
}

// ErrUnsupportedDTD is returned when a strict XML parser or writer
// encounters a document type declaration (DTD) or other DTD-level markup
// declaration. The strict XML 1.0/1.1 modes support only DTD-free
// documents.
var ErrUnsupportedDTD = errors.New("etree: DTDs are not supported in strict XML mode")

// A SyntaxError describes a well-formedness violation encountered while
// reading an XML document in a strict XML 1.0 or 1.1 mode.
type SyntaxError struct {
	// Msg describes the violation.
	Msg string

	// Byte is the 1-based byte offset at which the violation was detected.
	Byte int64

	// Line is the 1-based line number at which the violation was detected.
	Line int

	// Col is the 1-based byte offset of the violation within its line.
	Col int

	// Err is wrapped when available, e.g. ErrUnsupportedDTD.
	Err error
}

func (e *SyntaxError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("etree: XML syntax error on line %d, column %d (byte %d): %s",
			e.Line, e.Col, e.Byte, e.Msg)
	}
	return fmt.Sprintf("etree: XML syntax error at byte %d: %s", e.Byte, e.Msg)
}

func (e *SyntaxError) Unwrap() error {
	return e.Err
}

// A TreeError describes an illegal value found in the element tree while
// writing a document in a strict XML 1.0 or 1.1 mode. Path identifies the
// tree node at which the violation was detected.
type TreeError struct {
	// Version is the strict XML version in effect.
	Version XMLVersion

	// Path locates the offending node, e.g. "/root/book/@id" or
	// "/root/book/text()". Document-level nodes use the path "(document)".
	Path string

	// Msg describes the violation.
	Msg string

	// Err is wrapped when available, e.g. ErrUnsupportedDTD.
	Err error
}

func (e *TreeError) Error() string {
	return fmt.Sprintf("etree: invalid XML %s value at %s: %s",
		e.Version, e.Path, e.Msg)
}

func (e *TreeError) Unwrap() error {
	return e.Err
}

// isXMLChar returns true if r is a legal Char under the selected XML
// version. Surrogates, U+FFFE, U+FFFF and code points above U+10FFFF are
// excluded by the ranges below.
func isXMLChar(v XMLVersion, r rune) bool {
	switch {
	case r == 0x09 || r == 0x0A || r == 0x0D:
		return true
	case r >= 0x20 && r <= 0x7E:
		return true
	case r >= 0xA0 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return true
	case r >= 0x10000 && r <= 0x10FFFF:
		return true
	case v == Version11 && r >= 0x01 && r <= 0x1F:
		return true
	case v == Version11 && r >= 0x7F && r <= 0x9F:
		return true
	default:
		return false
	}
}

// isRestrictedChar11 returns true if r is an XML 1.1 RestrictedChar. Such
// characters may only appear in an XML 1.1 document as character
// references.
func isRestrictedChar11(r rune) bool {
	switch {
	case r >= 0x01 && r <= 0x08:
		return true
	case r >= 0x0B && r <= 0x0C:
		return true
	case r >= 0x0E && r <= 0x1F:
		return true
	case r >= 0x7F && r <= 0x84:
		return true
	case r >= 0x86 && r <= 0x9F:
		return true
	default:
		return false
	}
}

// isNCNameStartChar reports whether r may begin an XML non-colonized name.
// The XML 1.0 Fifth Edition and XML 1.1 Second Edition name ranges agree.
func isNCNameStartChar(r rune) bool {
	return r >= 'a' && r <= 'z' ||
		r >= 'A' && r <= 'Z' ||
		r == '_' ||
		r >= 0xC0 && r <= 0xD6 ||
		r >= 0xD8 && r <= 0xF6 ||
		r >= 0xF8 && r <= 0x2FF ||
		r >= 0x370 && r <= 0x37D ||
		r >= 0x37F && r <= 0x1FFF ||
		r == 0x200C || r == 0x200D ||
		r >= 0x2070 && r <= 0x218F ||
		r >= 0x2C00 && r <= 0x2FEF ||
		r >= 0x3001 && r <= 0xD7FF ||
		r >= 0xF900 && r <= 0xFDCF ||
		r >= 0xFDF0 && r <= 0xFFFD ||
		r >= 0x10000 && r <= 0xEFFFF
}

// isNCNameChar reports whether r may follow the first character of an XML
// non-colonized name.
func isNCNameChar(r rune) bool {
	return isNCNameStartChar(r) ||
		r == '-' || r == '.' ||
		r >= '0' && r <= '9' ||
		r == 0xB7 ||
		r >= 0x0300 && r <= 0x036F ||
		r >= 0x203F && r <= 0x2040
}

// predefinedEntities are the five entities predefined by the XML
// recommendation. Strict modes permit no other entities because entity
// declarations require a DTD.
var predefinedEntities = map[string]string{
	"lt":   "<",
	"gt":   ">",
	"amp":  "&",
	"apos": "'",
	"quot": `"`,
}
