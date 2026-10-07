// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import "unicode/utf8"

// This file implements the character, character-reference and name
// productions of the W3C XML 1.0 (5th edition) and XML 1.1 (2nd edition)
// recommendations. All comparisons operate on decoded Unicode code points;
// invalid UTF-8 is rejected separately by the versioned scanner.

// versionIsChar reports whether r is a legal Char production for the
// selected XML version. XML 1.1 adds the C1 control range U+0080..U+009F
// (minus NEL, which is handled as a line ending) to XML 1.0's character
// range.
func versionIsChar(v XMLVersion, r rune) bool {
	switch v {
	case Version11:
		return (r >= 0x01 && r <= 0xD7FF) ||
			(r >= 0xE000 && r <= 0xFFFD) ||
			(r >= 0x10000 && r <= 0x10FFFF)
	default:
		return r == 0x09 ||
			r == 0x0A ||
			r == 0x0D ||
			(r >= 0x20 && r <= 0xD7FF) ||
			(r >= 0xE000 && r <= 0xFFFD) ||
			(r >= 0x10000 && r <= 0x10FFFF)
	}
}

// versionIsRestricted reports whether r is a "restricted" character under
// XML 1.1. Restricted characters may appear in a document only as character
// references, never literally. NEL (U+0085) is not restricted: it is an
// alternative line ending.
func versionIsRestricted(r rune) bool {
	switch {
	case r >= 0x01 && r <= 0x08,
		r == 0x0B,
		r == 0x0C,
		r >= 0x0E && r <= 0x1F,
		r >= 0x7F && r <= 0x84,
		r >= 0x86 && r <= 0x9F:
		return true
	}
	return false
}

// versionIsCharRef reports whether r may be expressed as a numeric
// character reference for the selected version. XML 1.0 forbids references
// to most C0/C1 controls and to non-characters; XML 1.1 permits references
// to its restricted characters.
func versionIsCharRef(v XMLVersion, r rune) bool {
	if !versionIsChar(v, r) {
		return false
	}
	switch v {
	case Version11:
		if versionIsRestricted(r) {
			return true
		}
		if r == 0x09 || r == 0x0A || r == 0x0D {
			return true
		}
		return r >= 0x20
	default:
		if r == 0x09 || r == 0x0A || r == 0x0D {
			return true
		}
		return r >= 0x20
	}
}

// versionIsNameStartChar reports whether r may begin an XML Name under the
// selected version. The NameStartChar production is identical in XML 1.0
// (5th edition) and XML 1.1 (2nd edition).
func versionIsNameStartChar(r rune) bool {
	switch {
	case r == ':' ||
		r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= 0xC0 && r <= 0xD6) ||
		(r >= 0xD8 && r <= 0xF6) ||
		(r >= 0xF8 && r <= 0x2FF) ||
		(r >= 0x370 && r <= 0x37D) ||
		(r >= 0x37F && r <= 0x1FFF) ||
		(r >= 0x200C && r <= 0x200D) ||
		(r >= 0x2070 && r <= 0x218F) ||
		(r >= 0x2C00 && r <= 0x2FEF) ||
		(r >= 0x3001 && r <= 0xD7FF) ||
		(r >= 0xF900 && r <= 0xFDCF) ||
		(r >= 0xFDF0 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0xEFFFF):
		return true
	}
	return false
}

// versionIsNameChar reports whether r may appear within an XML Name after
// its first code point under the selected version.
func versionIsNameChar(v XMLVersion, r rune) bool {
	if versionIsNameStartChar(r) {
		return true
	}
	switch {
	case r == '-' || r == '.' || r >= '0' && r <= '9' ||
		r == 0xB7 ||
		(r >= 0x0300 && r <= 0x036F) ||
		(r >= 0x203F && r <= 0x2040):
		return true
	}
	return false
}

// versionIsSpace reports whether r is an S (white space) character after
// line-ending normalization has been applied.
func versionIsSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n'
}

// validateChars scans s for the first rune that is not a legal literal
// character for the selected version. If forbidRestricted is true, XML 1.1
// restricted characters are also rejected (they must be character
// references in the source and so cannot occur literally). Line endings
// have already been normalized to U+000A by the scanner.
func validateChars(v XMLVersion, s string, forbidRestricted bool) (rune, int) {
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			return utf8.RuneError, i
		}
		if !versionIsChar(v, r) ||
			(v == Version11 && forbidRestricted && versionIsRestricted(r)) {
			return r, i
		}
		i += width
	}
	return 0, -1
}

// validateName checks a decomposed element or attribute name against the
// selected version's Name production. Each of the space (namespace prefix)
// and local parts must be a valid Name. It returns the offending rune and
// its byte offset along with ok=false, or ok=true for a legal name.
func validateName(v XMLVersion, space, local string) (r rune, off int, ok bool) {
	if space != "" {
		if r, off, ok := validateNamePart(v, space); !ok {
			return r, off, false
		}
	}
	if r, off, ok := validateNamePart(v, local); !ok {
		return r, off, false
	}
	return 0, 0, true
}

func validateNamePart(v XMLVersion, part string) (rune, int, bool) {
	if len(part) == 0 {
		return utf8.RuneError, -1, false
	}
	for i := 0; i < len(part); {
		r, width := utf8.DecodeRuneInString(part[i:])
		if r == utf8.RuneError && width == 1 {
			return r, i, false
		}
		valid := versionIsNameChar(v, r)
		if i == 0 {
			valid = versionIsNameStartChar(r)
		}
		if !valid {
			return r, i, false
		}
		i += width
	}
	return 0, -1, true
}
