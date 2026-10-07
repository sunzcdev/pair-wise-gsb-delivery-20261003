// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"io"
	"strings"
	"testing"
)

// chunkReader delivers the input across Read boundaries with the requested
// chunk size, so multi-byte UTF-8 sequences are forced to straddle reads.
type chunkReader struct {
	data []byte
	size int
	pos  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := c.size
	if n > len(p) {
		n = len(p)
	}
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

func parseVersioned(t *testing.T, input string, v XMLVersion) *Document {
	t.Helper()
	doc := NewDocument()
	doc.ReadSettings.Version = v
	if err := doc.ReadFromString(input); err != nil {
		t.Fatalf("XML %s parse failed for %q: %v", v, input, err)
	}
	return doc
}

func parseVersionedBytesChunks(t *testing.T, input []byte, v XMLVersion, chunk int) *Document {
	t.Helper()
	doc := NewDocument()
	doc.ReadSettings.Version = v
	if _, err := doc.ReadFrom(&chunkReader{data: input, size: chunk}); err != nil {
		t.Fatalf("XML %s chunked parse failed at chunk size %d: %v", v, chunk, err)
	}
	return doc
}

// TestVersionParseMatrix runs the same W3C-conformance-style samples through
// both selected versions and asserts which must be accepted or rejected.
func TestVersionParseMatrix(t *testing.T) {
	type tc struct {
		name  string
		input string
		v10   string // "ok" or expected error substring
		v11   string
	}
	cases := []tc{
		{
			name:  "restricted C0 reference",
			input: `<?xml version="%s"?><a>x&#1;y</a>`,
			v10:   "not legal in XML 1.0",
			v11:   "ok",
		},
		{
			name:  "restricted 0x7F reference",
			input: `<?xml version="%s"?><a>&#x7F;</a>`,
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "restricted C1 0x84 reference",
			input: `<?xml version="%s"?><a>&#x84;</a>`,
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "NEL reference is not normalized",
			input: `<?xml version="%s"?><a>&#x85;</a>`,
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "line separator reference is not normalized",
			input: `<?xml version="%s"?><a>&#x2028;</a>`,
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "NUL reference forbidden both versions",
			input: `<?xml version="%s"?><a>&#0;</a>`,
			v10:   "not legal",
			v11:   "not legal",
		},
		{
			name:  "surrogate reference forbidden both versions",
			input: `<?xml version="%s"?><a>&#xD800;</a>`,
			v10:   "not legal",
			v11:   "not legal",
		},
		{
			name:  "noncharacter FFFE forbidden both versions",
			input: `<?xml version="%s"?><a>&#xFFFE;</a>`,
			v10:   "not legal",
			v11:   "not legal",
		},
		{
			name:  "out of range reference forbidden both versions",
			input: `<?xml version="%s"?><a>&#x110000;</a>`,
			v10:   "not legal",
			v11:   "not legal",
		},
		{
			name:  "raw NEL line ending",
			input: "<?xml version=\"%s\"?><a>x\u0085y</a>",
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "raw LS line ending",
			input: "<?xml version=\"%s\"?><a>x\u2028y</a>",
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "raw restricted C0 in content",
			input: "<?xml version=\"%s\"?><a>x\u0001y</a>",
			v10:   "illegal character",
			v11:   "must be a character reference",
		},
		{
			name:  "raw restricted C1 in attribute",
			input: "<?xml version=\"%s\"?><a x=\"v\u0084w\"/>",
			v10:   "ok",
			v11:   "must be a character reference",
		},
		{
			name:  "restricted reference in attribute 1.1",
			input: `<?xml version="%s"?><a x="v&#x1;w"/>`,
			v10:   "not legal",
			v11:   "ok",
		},
		{
			name:  "CRLF in attribute normalized",
			input: "<?xml version=\"%s\"?><a x=\"a\r\nb\"/>",
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "lone CR in content normalized",
			input: "<?xml version=\"%s\"?><a>a\rb</a>",
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "NEL in attribute normalized",
			input: "<?xml version=\"%s\"?><a x=\"a\u0085b\"/>",
			v10:   "ok",
			v11:   "ok",
		},
		{
			name:  "restricted char raw in CDATA",
			input: "<?xml version=\"%s\"?><a><![CDATA[x\u0001y]]></a>",
			v10:   "illegal character",
			v11:   "ok",
		},
		{
			name:  "restricted char raw in comment 1.1",
			input: "<?xml version=\"%s\"?><a><!--x\u0001y--></a>",
			v10:   "illegal character",
			v11:   "ok",
		},
		{
			name:  "double hyphen in comment rejected",
			input: `<?xml version="%s"?><a><!--a--b--></a>`,
			v10:   "may not contain",
			v11:   "may not contain",
		},
		{
			name:  "DOCTYPE rejected",
			input: `<?xml version="%s"?><!DOCTYPE a><a/>`,
			v10:   "DTD",
			v11:   "DTD",
		},
		{
			name:  "internal DTD subset rejected",
			input: `<?xml version="%s"?><!DOCTYPE a [<!ELEMENT a EMPTY>]><a/>`,
			v10:   "DTD",
			v11:   "DTD",
		},
		{
			name:  "non-UTF8 encoding rejected",
			input: `<?xml version="%s" encoding="ISO-8859-1"?><a/>`,
			v10:   "only UTF-8",
			v11:   "only UTF-8",
		},
		{
			name:  "standalone order",
			input: `<?xml version="%s" standalone="yes"?>`,
			v10:   "no root element",
			v11:   "no root element",
		},
	}

	run := func(t *testing.T, c tc, v XMLVersion, want string) {
		ver := v.String()
		input := strings.Replace(c.input, "%s", ver, 1)
		doc := NewDocument()
		doc.ReadSettings.Version = v
		err := doc.ReadFromString(input)
		if want == "ok" {
			if err != nil {
				t.Errorf("%s [%s]: expected success, got %v", c.name, ver, err)
			}
			return
		}
		if err == nil {
			t.Errorf("%s [%s]: expected error containing %q, got nil", c.name, ver, want)
			return
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s [%s]: expected error containing %q, got %v", c.name, ver, want, err)
		}
	}

	for _, c := range cases {
		t.Run(c.name+"/1.0", func(t *testing.T) { run(t, c, Version10, c.v10) })
		t.Run(c.name+"/1.1", func(t *testing.T) { run(t, c, Version11, c.v11) })
	}
}
