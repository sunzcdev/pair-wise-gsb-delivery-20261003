// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func replaceAll(s, old, new []byte) []byte {
	return bytes.ReplaceAll(s, old, new)
}

// TestXMLConformanceSamples runs the W3C-conformance-style sample corpus
// under testdata/xmlconf. Each sample's directory layout determines the
// expected outcome:
//
//	valid/1.0, valid/1.1   - must parse only under that version
//	valid/common           - must parse under both versions
//	invalid/1.0-only       - rejected by 1.0, accepted by 1.1
//	invalid/1.1-only       - rejected by 1.1, accepted by 1.0
//	invalid/common         - rejected by both; files suffixed -10/-11
//	                         carry the matching declaration
//
// Every valid sample is also parsed through a one-byte-at-a-time reader so
// that multi-byte UTF-8 sequences are forced across Read boundaries.
func TestXMLConformanceSamples(t *testing.T) {
	root := filepath.Join("testdata", "xmlconf")

	type expectation struct {
		v10, v11 bool // true => accepted
	}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".xml") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		var expect expectation
		switch {
		case strings.HasPrefix(rel, "valid/common/"):
			expect.v10, expect.v11 = true, true
		case strings.HasPrefix(rel, "valid/1.0/"):
			expect.v10 = true
		case strings.HasPrefix(rel, "valid/1.1/"):
			expect.v11 = true
		case strings.HasPrefix(rel, "invalid/1.0-only/"):
			expect.v11 = true
		case strings.HasPrefix(rel, "invalid/1.1-only/"):
			expect.v10 = true
		case strings.HasPrefix(rel, "invalid/common/"):
			// Declaration-bearing samples select their own version by the
			// -10/-11 filename suffix.
			switch {
			case strings.HasSuffix(rel, "-10.xml"):
				expect.v10, expect.v11 = false, false
			case strings.HasSuffix(rel, "-11.xml"):
				expect.v10, expect.v11 = false, false
			default:
				expect.v10, expect.v11 = false, false
			}
		default:
			t.Fatalf("sample in unexpected directory: %s", rel)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Declaration-version-pinned samples (suffix -10/-11) deliberately
		// disagree with the other selection and only run on one side.
		pinned := strings.HasSuffix(rel, "-10.xml") || strings.HasSuffix(rel, "-11.xml")

		runOne := func(t *testing.T, v XMLVersion, shouldParse bool) {
			t.Helper()
			if pinned && !shouldParse {
				if strings.HasSuffix(rel, "-"+v.String()+".xml") {
					// Expected to fail on the pinned version itself.
				} else {
					t.Skip("version-pinned sample")
				}
			}

			// Rewrite the declaration's version to match the selected
			// version for all non-pinned samples, so the same corpus can be
			// exercised under both interpretations.
			input := data
			if !pinned {
				s := string(data)
				s = strings.Replace(s, `version="1.0"`, `version="`+v.String()+`"`, 1)
				s = strings.Replace(s, `version="1.1"`, `version="`+v.String()+`"`, 1)
				input = []byte(s)
			}

			doc := NewDocument()
			doc.ReadSettings.Version = v

			// First via bytes.
			err := doc.ReadFromBytes(input)
			if shouldParse {
				if err != nil {
					t.Fatalf("%s v%s: expected accept, got %v", rel, v, err)
				}
			} else {
				if err == nil {
					t.Fatalf("%s v%s: expected rejection", rel, v)
				}
				return
			}

			// Then one byte per Read, exercising UTF-8 reassembly.
			chunked := NewDocument()
			chunked.ReadSettings.Version = v
			if _, err := chunked.ReadFrom(&chunkReader{data: input, size: 1}); err != nil {
				t.Fatalf("%s v%s chunked: %v", rel, v, err)
			}
		}

		t.Run(rel+"/1.0", func(t *testing.T) { runOne(t, Version10, expect.v10) })
		t.Run(rel+"/1.1", func(t *testing.T) { runOne(t, Version11, expect.v11) })
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

// TestXMLConformanceSameInputBothVersions feeds each valid/common sample to
// both versions and asserts the parsed trees are textually equivalent.
func TestXMLConformanceSameInputBothVersions(t *testing.T) {
	commonDir := filepath.Join("testdata", "xmlconf", "valid", "common")
	entries, err := os.ReadDir(commonDir)
	if err != nil {
		t.Fatal(err)
	}
	// Raw NEL/LS differ only because XML 1.1 normalizes them to LF; they are
	// compared after the same normalization. They still parse under both.
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, "\u0085", "\n")
		s = strings.ReplaceAll(s, "\u2028", "\n")
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
		return s
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(commonDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		d10 := NewDocument()
		d10.ReadSettings.Version = Version10
		d10data := replaceAll(data, []byte(`version="1.1"`), []byte(`version="1.0"`))
		if err := d10.ReadFromBytes(d10data); err != nil {
			t.Fatalf("1.0: %v", err)
		}
		d11 := NewDocument()
		d11.ReadSettings.Version = Version11
		rewritten := data
		rewritten = replaceAll(rewritten, []byte(`version="1.0"`), []byte(`version="1.1"`))
		if err := d11.ReadFromBytes(rewritten); err != nil {
			t.Fatalf("1.1: %v", err)
		}
		compareTrees(t, e.Name(), d10.Root(), d11.Root(), normalize)
	}
}

// compareTrees recursively compares two parsed elements for semantic
// equality: tag, attributes, child element sequence, and character data
// (after version-specific normalization).
func compareTrees(t *testing.T, label string, a, b *Element, norm func(string) string) {
	t.Helper()
	if a.FullTag() != b.FullTag() {
		t.Fatalf("%s tag: %q vs %q", label, a.FullTag(), b.FullTag())
	}
	if len(a.Attr) != len(b.Attr) {
		t.Fatalf("%s attr count: %d vs %d", label, len(a.Attr), len(b.Attr))
	}
	for i := range a.Attr {
		if a.Attr[i].FullKey() != b.Attr[i].FullKey() ||
			norm(a.Attr[i].Value) != norm(b.Attr[i].Value) {
			t.Fatalf("%s attr %d: %q=%q vs %q=%q", label, i,
				a.Attr[i].FullKey(), a.Attr[i].Value,
				b.Attr[i].FullKey(), b.Attr[i].Value)
		}
	}

	var aText, bText []string
	var aKids, bKids []*Element
	collect := func(el *Element) ([]string, []*Element) {
		var text []string
		var kids []*Element
		for _, c := range el.Child {
			switch k := c.(type) {
			case *CharData:
				if !k.IsCData() {
					text = append(text, k.Data)
				} else {
					text = append(text, "CDATA:"+k.Data)
				}
			case *Element:
				kids = append(kids, k)
			case *Comment:
				text = append(text, "COMMENT:"+k.Data)
			case *ProcInst:
				text = append(text, "PI:"+k.Target)
			}
		}
		return text, kids
	}
	aText, aKids = collect(a)
	bText, bKids = collect(b)
	if len(aText) != len(bText) {
		t.Fatalf("%s text segments: %q vs %q", label, aText, bText)
	}
	for i := range aText {
		if norm(aText[i]) != norm(bText[i]) {
			t.Fatalf("%s text %d: %q vs %q", label, i, aText[i], bText[i])
		}
	}
	if len(aKids) != len(bKids) {
		t.Fatalf("%s child elements: %d vs %d", label, len(aKids), len(bKids))
	}
	for i := range aKids {
		compareTrees(t, label, aKids[i], bKids[i], norm)
	}
}
