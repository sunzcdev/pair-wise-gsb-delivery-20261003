// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"errors"
	"io"
	"strconv"
	"strings"
)

// XMLVersion selects the version of the W3C XML recommendation used to
// interpret or serialize a document.
//
// The zero value is VersionAuto, which preserves the package's historical
// behavior: documents are read and written through the Go standard library's
// encoding/xml package without strict XML version processing.
type XMLVersion int

const (
	// VersionAuto (the default) preserves backward-compatible behavior.
	// Reading and writing are performed through encoding/xml and no XML
	// version rules are enforced.
	VersionAuto XMLVersion = iota

	// Version10 selects strict processing according to the W3C XML 1.0
	// recommendation (5th edition). Only UTF-8 documents without a DTD are
	// supported.
	Version10

	// Version11 selects processing according to the W3C XML 1.1
	// recommendation (2nd edition). XML 1.1 extends XML 1.0 with its own
	// rules for character and character-reference validity, restricted
	// characters, and line-ending normalization (including NEL, U+0085, and
	// LINE SEPARATOR, U+2028). Only UTF-8 documents without a DTD are
	// supported.
	Version11
)

// String returns the version string used in an XML declaration ("1.0" or
// "1.1"). The zero value VersionAuto returns the empty string.
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

// ErrDTD is returned when an explicit-version read or write encounters a
// document type declaration (DOCTYPE) or an internal DTD subset. The first
// version of explicit XML version processing supports only DTD-free
// documents.
var ErrDTD = errors.New("etree: DTD (DOCTYPE) is not supported in explicit XML version mode")

// errVersionUnsupported is returned by the ReadFrom* functions when the
// requested explicit XML version is combined with settings the versioned
// parser does not implement.
var errVersionUnsupported = errors.New("etree: unsupported option in explicit XML version mode")

// InvalidValueError is returned by the WriteTo* functions when an explicit
// XML version is selected and the element tree contains a value that cannot
// legally be serialized under the selected version's rules. The position
// fields identify the offending tree location rather than an input byte
// offset. Use errors.As to inspect it.
type InvalidValueError struct {
	// Version is the XML version under which the value is illegal.
	Version XMLVersion

	// Path is the path of the element that owns the illegal value, using the
	// same syntax as Element.GetPath. Document-level tokens use "/".
	Path string

	// Attr is the name of the offending attribute when Kind is "attribute";
	// otherwise it is empty.
	Attr string

	// Kind identifies the offending token type: "text", "cdata", "comment",
	// "processing instruction", "directive", "element name", "attribute
	// name", "attribute value", "encoding declaration" or "document".
	Kind string

	// Rune is the offending Unicode code point. It is RuneError (0xFFFD)
	// when the value contains an invalid UTF-8 sequence rather than a single
	// offending code point.
	Rune rune

	// ByteOffset is the offset in bytes of the offending rune (or invalid
	// sequence) within the offending string value.
	ByteOffset int
}

func (e *InvalidValueError) Error() string {
	var b strings.Builder
	b.WriteString("etree: invalid ")
	b.WriteString(e.Kind)
	if e.Path != "" {
		b.WriteString(" in ")
		b.WriteString(e.Path)
	}
	if e.Attr != "" {
		b.WriteString(" (attribute ")
		b.WriteString(e.Attr)
		b.WriteByte(')')
	}
	b.WriteString(" for XML ")
	b.WriteString(e.Version.String())
	if e.Rune == 0xFFFD {
		b.WriteString(": invalid UTF-8 sequence at byte ")
	} else {
		b.WriteString(": illegal character ")
		b.WriteString(strconv.QuoteRune(e.Rune))
		b.WriteString(" (U+")
		b.WriteString(strconv.FormatInt(int64(e.Rune), 16))
		b.WriteString(") at byte ")
	}
	b.WriteString(strconv.Itoa(e.ByteOffset))
	return b.String()
}

// versionSettingsValid verifies that the read settings are compatible with
// the versioned parser. The first version supports only UTF-8, DTD-free
// documents and does not implement lenient parsing.
func validateVersionedReadSettings(s ReadSettings) error {
	switch {
	case s.Version != Version10 && s.Version != Version11:
		return errVersionUnsupported
	case s.CharsetReader != nil:
		// Only UTF-8 input is accepted, so no charset conversion hooks are
		// permitted.
		return errVersionUnsupported
	case s.Permissive || s.AutoClose != nil:
		return errVersionUnsupported
	}
	return nil
}

// readFromVersioned parses XML from the reader 'r' into element 'e' using
// the strict, version-aware parser.
func (e *Element) readFromVersioned(ri io.Reader, settings ReadSettings) (int64, error) {
	if err := validateVersionedReadSettings(settings); err != nil {
		return 0, err
	}
	p := newVersionParser(ri, settings)
	err := p.parse(e)
	return p.cr.n, err
}

// validateDocumentVersion returns an *InvalidValueError describing the
// first value in the document that cannot be legally serialized under
// version v. It returns nil for VersionAuto and when the tree is legal.
func validateDocumentVersion(doc *Element, v XMLVersion) error {
	if v != Version10 && v != Version11 {
		return nil
	}
	return validateDocumentChildren(doc, v)
}
