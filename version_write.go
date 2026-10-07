// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// escapeVersionedRune returns the byte sequence to substitute for r in the
// given versioned escape mode, or nil to emit the rune literally.
// Structural characters (&, <, >, quotes) and the \t \n \r cases are
// handled by escapeString before this function is reached.
//
// Attribute values always reference NEL and LINE SEPARATOR because
// attribute-value normalization would otherwise alter them, and XML 1.1
// text references them as well (they are line endings in 1.1). XML 1.1
// text and attribute values reference every restricted control so the
// XML 1.1 parser's literal-character rules are satisfied.
func escapeVersionedRune(m escapeMode, r rune) []byte {
	attr := m == escapeV10Attr || m == escapeV11Attr

	if attr && (r == 0x85 || r == 0x2028) {
		return []byte(fmt.Sprintf("&#x%X;", r))
	}
	if m == escapeV11Text && (r == 0x85 || r == 0x2028) {
		return []byte(fmt.Sprintf("&#x%X;", r))
	}
	if (m == escapeV11Text || m == escapeV11Attr) && versionIsRestricted(r) {
		return []byte(fmt.Sprintf("&#x%X;", r))
	}
	return nil
}

// This file validates element trees before strict serialization and scans
// the tree values for runes that cannot legally be represented in the
// selected XML version.

// validateDocumentChildren validates the document's direct children (Misc*,
// one element, Misc*) and then walks the root subtree.
func validateDocumentChildren(doc *Element, v XMLVersion) error {
	var roots int
	var xmlDeclSeen bool
	for _, t := range doc.Child {
		switch c := t.(type) {
		case *Element:
			roots++
			if roots > 1 {
				return invalidAt(v, "", "", "document", utf8.RuneError, 0)
			}
			if err := validateElement(c, v); err != nil {
				return err
			}
		case *CharData:
			if !c.IsWhitespace() {
				return invalidAt(v, "/", "", "text", firstRune(c.Data), 0)
			}
			if r, off := validateLiteralChars(v, c.Data); off >= 0 {
				return invalidAt(v, "/", "", "text", r, off)
			}
		case *Comment:
			if r, off := validateLiteralChars(v, c.Data); off >= 0 {
				return invalidAt(v, "/", "", "comment", r, off)
			}
		case *ProcInst:
			if c.Target == "xml" {
				if xmlDeclSeen {
					return invalidAt(v, "/", "", "processing instruction", 'x', 0)
				}
				xmlDeclSeen = true
				if err := validateXMLDeclTree(c, v); err != nil {
					return err
				}
				continue
			}
			if bad, off, ok := validateName(v, "", c.Target); !ok {
				return invalidAt(v, "/", "", "processing instruction", bad, off)
			}
			if r, off := validateLiteralChars(v, c.Inst); off >= 0 {
				return invalidAt(v, "/", "", "processing instruction", r, off)
			}
		case *Directive:
			// All directives are rejected. DOCTYPE specifically maps to
			// ErrDTD.
			if isDOCTYPE(c.Data) {
				return ErrDTD
			}
			return invalidAt(v, "/", "", "directive", firstRune(c.Data), 0)
		}
	}
	if roots == 0 {
		return invalidAt(v, "/", "", "document", utf8.RuneError, 0)
	}
	return nil
}

func validateElement(el *Element, v XMLVersion) error {
	path := el.GetPath()

	if bad, off, ok := validateName(v, el.Space, el.Tag); !ok {
		return invalidAt(v, path, "", "element name", bad, off)
	}

	for _, a := range el.Attr {
		if r, off, ok := validateName(v, a.Space, a.Key); !ok {
			return invalidAt(v, path, a.FullKey(), "attribute name", r, off)
		}
		if r, off := validateLiteralChars(v, a.Value); off >= 0 {
			return invalidAt(v, path, a.FullKey(), "attribute value", r, off)
		}
	}

	for _, t := range el.Child {
		switch c := t.(type) {
		case *Element:
			if err := validateElement(c, v); err != nil {
				return err
			}
		case *CharData:
			kind := "text"
			if c.IsCData() {
				kind = "cdata"
			}
			if r, off := validateLiteralChars(v, c.Data); off >= 0 {
				return invalidAt(v, path, "", kind, r, off)
			}
		case *Comment:
			if r, off := validateLiteralChars(v, c.Data); off >= 0 {
				return invalidAt(v, path, "", "comment", r, off)
			}
		case *ProcInst:
			if c.Target == "xml" {
				return invalidAt(v, path, "", "processing instruction", 'x', 0)
			}
			if bad, off, ok := validateName(v, "", c.Target); !ok {
				return invalidAt(v, path, "", "processing instruction", bad, off)
			}
			if r, off := validateLiteralChars(v, c.Inst); off >= 0 {
				return invalidAt(v, path, "", "processing instruction", r, off)
			}
		case *Directive:
			if isDOCTYPE(c.Data) {
				return ErrDTD
			}
			return invalidAt(v, path, "", "directive", firstRune(c.Data), 0)
		}
	}
	return nil
}

// validateXMLDeclTree validates an existing <?xml?> processing instruction
// against the selected write version.
func validateXMLDeclTree(pi *ProcInst, v XMLVersion) error {
	attrs, err := parsePseudoAttributes(strings.TrimPrefix(pi.Inst, " "))
	if err != nil {
		return invalidAt(v, "/", "", "encoding declaration", utf8.RuneError, 0)
	}
	for _, a := range attrs {
		switch a.key {
		case "version":
			if a.value != v.String() {
				return &InvalidValueError{
					Version: v,
					Path:    "/",
					Kind:    "XML declaration",
					Rune:    firstRune(a.value),
				}
			}
		case "encoding":
			if !strings.EqualFold(a.value, "UTF-8") {
				return invalidAt(v, "/", "", "encoding declaration", firstRune(a.value), 0)
			}
		case "standalone":
			if a.value != "yes" && a.value != "no" {
				return invalidAt(v, "/", "", "encoding declaration", firstRune(a.value), 0)
			}
		default:
			return invalidAt(v, "/", "", "encoding declaration", firstRune(a.key), 0)
		}
	}
	return nil
}

// validateLiteralChars returns the first rune that cannot be serialized at
// all under the selected version. XML 1.1 restricted characters are not
// reported here because text and attribute values express them as numeric
// references and they are legal literally in CDATA, comments and processing
// instructions.
func validateLiteralChars(v XMLVersion, s string) (rune, int) {
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			return r, i
		}
		// Line endings in the tree are already normalized to '\n' by the
		// versioned reader; treat it as legal in every context.
		if r == '\n' {
			i += width
			continue
		}
		if !versionIsChar(v, r) {
			return r, i
		}
		i += width
	}
	return 0, -1
}

func isDOCTYPE(data string) bool {
	data = strings.TrimLeft(data, " \t\n\r")
	if len(data) < 7 {
		return false
	}
	return strings.EqualFold(data[:7], "DOCTYPE")
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return utf8.RuneError
}

func invalidAt(v XMLVersion, path, attr, kind string, r rune, off int) error {
	return &InvalidValueError{
		Version:    v,
		Path:       path,
		Attr:       attr,
		Kind:       kind,
		Rune:       r,
		ByteOffset: off,
	}
}
