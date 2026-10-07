package etree

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type c14nVector struct {
	Name         string   `json:"name"`
	XML          string   `json:"xml"`
	Mode         string   `json:"mode"`
	WithComments bool     `json:"with_comments"`
	Inclusive    []string `json:"inclusive"`
	Expected     string   `json:"expected"`
	Path         string   `json:"path"`
}

type c14nVectorFile struct {
	Cases  []c14nVector `json:"cases"`
	Errors []struct {
		Name string `json:"name"`
		XML  string `json:"xml"`
	} `json:"errors"`
}

func loadVectors(t *testing.T) *c14nVectorFile {
	t.Helper()
	data, err := os.ReadFile("testdata/c14n_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf c14nVectorFile
	if err := json.Unmarshal(data, &vf); err != nil {
		t.Fatal(err)
	}
	return &vf
}

func loadJSONVectors(t *testing.T, path string) []c14nVector {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []c14nVector
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// TestExcC14NDetachedSubtree verifies that canonicalizing a subtree whose
// namespace prefix is declared only on an ancestor outside the node set
// emits the required namespace declaration on the subtree root, and that
// doing so leaves the original element tree unmodified.
func TestExcC14NDetachedSubtree(t *testing.T) {
	const docXML = `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">` +
		`<soap:Body>` +
		`<ds:SignedInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#" Id="x">` +
		`<ds:CanonicalizationMethod Algorithm="http://www.w3.org/2001/10/xml-exc-c14n#"/>` +
		`</ds:SignedInfo>` +
		`</soap:Body>` +
		`</soap:Envelope>`

	doc := NewDocument()
	if err := doc.ReadFromString(docXML); err != nil {
		t.Fatal(err)
	}

	signedInfo := doc.FindElement("//ds:SignedInfo")
	if signedInfo == nil {
		t.Fatal("subtree element not found")
	}

	// The element itself carries only the ds declaration needed by its tag.
	// Detach it to simulate signing a fragment that is later re-embedded.
	detached := signedInfo.Copy()

	before, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}

	got, err := NewExclusiveC14N().CanonicalizeElement(detached)
	if err != nil {
		t.Fatal(err)
	}

	const expected = `<ds:SignedInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#" Id="x">` +
		`<ds:CanonicalizationMethod Algorithm="http://www.w3.org/2001/10/xml-exc-c14n#"></ds:CanonicalizationMethod>` +
		`</ds:SignedInfo>`
	if string(got) != expected {
		t.Errorf("detached subtree mismatch\nexpected: %s\n     got: %s", expected, string(got))
	}

	// The ancestor-declared prefix must be re-emitted even when the subtree
	// is still attached to its original document.
	attached, err := NewExclusiveC14N().CanonicalizeElement(signedInfo)
	if err != nil {
		t.Fatal(err)
	}
	if string(attached) != expected {
		t.Errorf("attached subtree mismatch\nexpected: %s\n     got: %s",
			expected, string(attached))
	}

	// Canonicalization must not mutate the input tree.
	if len(signedInfo.Attr) != 2 {
		t.Errorf("subtree attributes were modified: %d attrs remain", len(signedInfo.Attr))
	}
	if child := signedInfo.SelectElement("ds:CanonicalizationMethod"); child == nil {
		t.Fatal("child element missing after canonicalization")
	} else if len(child.Attr) != 1 {
		t.Errorf("child attributes were modified: %d attrs remain", len(child.Attr))
	}

	after, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("document tree was modified by canonicalization\nbefore: %s\nafter:  %s",
			before, after)
	}
}

func elementAtPath(e *Element, path string) *Element {
	if path == "" {
		return e
	}
	node := e
	for _, idx := range splitDigits(path) {
		count := idx
		for _, ch := range node.ChildElements() {
			if count == 0 {
				node = ch
				break
			}
			count--
		}
	}
	return node
}

func splitDigits(path string) []int {
	var out []int
	cur := -1
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c >= '0' && c <= '9' {
			if cur < 0 {
				cur = 0
			}
			cur = cur*10 + int(c-'0')
		} else {
			if cur >= 0 {
				out = append(out, cur)
			}
			cur = -1
		}
	}
	if cur >= 0 {
		out = append(out, cur)
	}
	return out
}

func TestExcC14NVectors(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Cases {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			doc := NewDocument()
			if err := doc.ReadFromString(v.XML); err != nil {
				t.Fatalf("parse: %v", err)
			}
			c := &ExclusiveC14N{WithComments: v.WithComments, InclusivePrefixes: v.Inclusive}
			var got []byte
			var err error
			if v.Mode == "subtree" {
				root := doc.Root()
				sub := elementAtPath(root, v.Path)
				got, err = c.CanonicalizeElement(sub)
			} else {
				got, err = c.CanonicalizeDocument(doc)
			}
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if string(got) != v.Expected {
				t.Errorf("mismatch\nexpected: %q\n     got: %q", v.Expected, string(got))
			}
		})
	}
}

func TestExcC14NErrors(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Errors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			doc := NewDocument()
			parseErr := doc.ReadFromString(v.XML)
			if parseErr != nil {
				return // parser itself rejects; acceptable
			}
			c := NewExclusiveC14N()
			if _, err := c.CanonicalizeDocument(doc); err == nil {
				t.Errorf("expected error for %s", v.Name)
			}
			if root := doc.Root(); root != nil {
				if _, err := c.CanonicalizeElement(root); err == nil {
					t.Errorf("expected subtree error for %s", v.Name)
				}
			}
		})
	}
}

func TestExcC14NW3CVectors(t *testing.T) {
	for _, v := range loadJSONVectors(t, "testdata/c14n_w3c.json") {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			doc := NewDocument()
			if err := doc.ReadFromString(v.XML); err != nil {
				t.Fatalf("parse: %v", err)
			}
			c := &ExclusiveC14N{WithComments: v.WithComments, InclusivePrefixes: v.Inclusive}
			got, err := c.CanonicalizeDocument(doc)
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if string(got) != v.Expected {
				t.Errorf("mismatch\nexpected: %q\n     got: %q", v.Expected, string(got))
			}
		})
	}
}

func TestExcC14NFuzz(t *testing.T) {
	for _, v := range loadJSONVectors(t, "testdata/c14n_fuzz.json") {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			doc := NewDocument()
			if err := doc.ReadFromString(v.XML); err != nil {
				t.Fatalf("parse: %v", err)
			}
			c := &ExclusiveC14N{WithComments: v.WithComments, InclusivePrefixes: v.Inclusive}
			var got []byte
			var err error
			if v.Mode == "subtree" {
				got, err = c.CanonicalizeElement(elementAtPath(doc.Root(), v.Path))
			} else {
				got, err = c.CanonicalizeDocument(doc)
			}
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if string(got) != v.Expected {
				t.Errorf("mismatch\ninput: %s\nexpected: %q\n     got: %q",
					v.XML, v.Expected, string(got))
			}
		})
	}
}

// TestExcC14NEmptyDocument canonicalizes a document without a root
// element.
func TestExcC14NEmptyDocument(t *testing.T) {
	doc := NewDocument()
	doc.CreateProcInst("pi", "data")
	out, err := NewExclusiveC14NWithComments().CanonicalizeDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "<?pi data?>" {
		t.Errorf("got %q", out)
	}

	var buf bytes.Buffer
	if _, err := NewExclusiveC14N().WriteDocumentTo(&buf, doc); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "<?pi data?>" {
		t.Errorf("write got %q", buf.String())
	}
}

// TestExcC14NReusable verifies that a configured ExclusiveC14N can be
// applied repeatedly without leaking traversal state between calls.
func TestExcC14NReusable(t *testing.T) {
	c := NewExclusiveC14N().SetInclusivePrefixes("a")
	d1 := NewDocument()
	if err := d1.ReadFromString(`<r xmlns:a="urn:a"/>`); err != nil {
		t.Fatal(err)
	}
	d2 := NewDocument()
	if err := d2.ReadFromString(`<x xmlns:a="urn:a"><a:y/></x>`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CanonicalizeDocument(d1); err != nil {
		t.Fatal(err)
	}
	out, err := c.CanonicalizeDocument(d2)
	if err != nil {
		t.Fatal(err)
	}
	if want := `<x xmlns:a="urn:a"><a:y></a:y></x>`; string(out) != want {
		t.Errorf("got %q want %q", out, want)
	}
}

// TestExcC14NWriteTo verifies the io.Writer-based entry points.
func TestExcC14NWriteTo(t *testing.T) {
	doc := NewDocument()
	if err := doc.ReadFromString(`<r xmlns:a="urn:a"><a:x/></r>`); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := NewExclusiveC14N().WriteDocumentTo(&buf, doc)
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != buf.Len() {
		t.Errorf("reported byte count %d, wrote %d", n, buf.Len())
	}
	if got, want := buf.String(), `<r><a:x xmlns:a="urn:a"></a:x></r>`; got != want {
		t.Errorf("WriteDocumentTo: got %q want %q", got, want)
	}

	buf.Reset()
	n, err = NewExclusiveC14N().WriteElementTo(&buf, doc.Root().SelectElement("a:x"))
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != buf.Len() {
		t.Errorf("reported byte count %d, wrote %d", n, buf.Len())
	}
	if got, want := buf.String(), `<a:x xmlns:a="urn:a"></a:x>`; got != want {
		t.Errorf("WriteElementTo: got %q want %q", got, want)
	}
}

// TestExcC14NErrorSentinels verifies that illegal inputs are reported with
// the documented sentinel errors.
func TestExcC14NErrorSentinels(t *testing.T) {
	tests := []struct {
		name string
		xml  string
		want error
	}{
		{"relative-prefix-uri", `<r xmlns:a="relative/uri"><a:x/></r>`, ErrC14NRelativeURI},
		{"relative-default-uri", `<r xmlns="relative/uri"><x/></r>`, ErrC14NRelativeURI},
		{"empty-prefixed-ns", `<r xmlns:a=""><x/></r>`, ErrC14NInvalidNamespace},
		{"rebound-xml-prefix", `<r xmlns:xml="wrong" xml:lang="en"/>`, ErrC14NInvalidNamespace},
		{"rebound-xmlns-prefix", `<r xmlns:xmlns="http://www.w3.org/2000/xmlns/"/>`, ErrC14NInvalidNamespace},
		{"undeclared-prefix", `<r a:k="1"/>`, ErrC14NUndeclaredPrefix},
		{"undeclared-element-prefix", `<a:r/>`, ErrC14NUndeclaredPrefix},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			doc := NewDocument()
			if err := doc.ReadFromString(tc.xml); err != nil {
				return // parser rejects; acceptable
			}
			_, err := NewExclusiveC14N().CanonicalizeDocument(doc)
			if !errors.Is(err, tc.want) {
				t.Errorf("document: got %v, want %v", err, tc.want)
			}
			_, err = NewExclusiveC14N().CanonicalizeElement(doc.Root())
			if !errors.Is(err, tc.want) {
				t.Errorf("element: got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestExcC14NSubtreeAncestorURI verifies that a subtree canonicalization
// resolves prefix bindings declared on ancestors and rejects relative
// namespace URIs found only on the ancestor chain.
func TestExcC14NSubtreeAncestorURI(t *testing.T) {
	doc := NewDocument()
	if err := doc.ReadFromString(`<r xmlns:a="relative/uri"><w><x/></w></r>`); err != nil {
		t.Fatal(err)
	}
	x := doc.FindElement("//x")
	if _, err := NewExclusiveC14N().CanonicalizeElement(x); !errors.Is(err, ErrC14NRelativeURI) {
		t.Fatalf("got %v, want ErrC14NRelativeURI", err)
	}
}

// TestExcC14NInclusivePrefixes verifies prefix-list behavior at the
// document and subtree levels.
func TestExcC14NInclusivePrefixes(t *testing.T) {
	doc := NewDocument()
	if err := doc.ReadFromString(
		`<r xmlns:a="urn:a" xmlns:b="urn:b"><child><b:x/></child></r>`); err != nil {
		t.Fatal(err)
	}
	got, err := NewExclusiveC14N().SetInclusivePrefixes("b").CanonicalizeDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := `<r xmlns:b="urn:b"><child><b:x></b:x></child></r>`
	if string(got) != want {
		t.Errorf("got %q want %q", got, want)
	}

	doc2 := NewDocument()
	if err := doc2.ReadFromString(
		`<root xmlns:a="urn:a"><wrap><inner><a:x/></inner></wrap></root>`); err != nil {
		t.Fatal(err)
	}
	sub := doc2.FindElement("//wrap")
	got, err = NewExclusiveC14N().SetInclusivePrefixes("a").CanonicalizeElement(sub)
	if err != nil {
		t.Fatal(err)
	}
	want = `<wrap xmlns:a="urn:a"><inner><a:x></a:x></inner></wrap>`
	if string(got) != want {
		t.Errorf("subtree: got %q want %q", got, want)
	}
}

// TestExcC14NCommentsModes verifies that comments are retained only by the
// WithComments variant and that processing instructions survive both.
func TestExcC14NCommentsModes(t *testing.T) {
	doc := NewDocument()
	if err := doc.ReadFromString(`<r><!--c--><?p v?></r>`); err != nil {
		t.Fatal(err)
	}
	if got, err := NewExclusiveC14N().CanonicalizeDocument(doc); err != nil {
		t.Fatal(err)
	} else if string(got) != `<r><?p v?></r>` {
		t.Errorf("no-comments: got %q", got)
	}
	if got, err := NewExclusiveC14NWithComments().CanonicalizeDocument(doc); err != nil {
		t.Fatal(err)
	} else if string(got) != `<r><!--c--><?p v?></r>` {
		t.Errorf("with-comments: got %q", got)
	}
}
