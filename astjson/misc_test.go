package astjson_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/yuin/goldmark/v2/astjson"
	"github.com/yuin/goldmark/v2/parser"
)

func TestHardLineBreaks(t *testing.T) {
	source := "foo  \nbar\n"
	doc := parser.New().Parse([]byte(source))
	b, err := astjson.NewExporter().Marshal(doc, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Fatalf("invalid json: %s", b)
	}
	if !bytes.Contains(b, []byte(`"HardLineBreak"`)) {
		t.Fatalf("hard line break flag missing:\n%s", b)
	}
}

func TestEmptyDocument(t *testing.T) {
	doc := parser.New().Parse(nil)
	b, err := astjson.NewExporter().Marshal(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Fatalf("invalid json: %s", b)
	}
	if !bytes.Contains(b, []byte(`"children": []`)) {
		t.Fatalf("empty document must have an empty children array:\n%s", b)
	}
}

func TestParsedHeadingAttributesKeepSourceOrder(t *testing.T) {
	source := "# title {.zebra #anchor}\n"
	doc := parser.New(parser.WithAttribute()).Parse([]byte(source))
	b, err := astjson.NewExporter().Marshal(doc, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	classIdx := bytes.Index(b, []byte(`"class"`))
	idIdx := bytes.Index(b, []byte(`"id"`))
	if classIdx < 0 || idIdx < 0 {
		t.Fatalf("heading attributes missing:\n%s", b)
	}
	// .zebra appears before #anchor in source and must stay first despite
	// conventional map sorting favoring class/id in the opposite direction.
	if classIdx > idIdx {
		t.Fatalf("attributes must follow source order, got:\n%s", b)
	}
}
