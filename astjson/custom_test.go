package astjson_test

import (
	"bytes"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/astjson"
	"github.com/yuin/goldmark/v2/text"
)

// CustomNode simulates a third-party extension node that carries an owned
// (generated) text value.
type CustomNode struct {
	ast.BaseInline
	Label string
	Count int
}

var customNodeKind = ast.NewNodeKind("example.com/CustomNode")

func (n *CustomNode) Kind() ast.NodeKind { return customNodeKind }
func (n *CustomNode) Dump(_ []byte) *ast.NodeDump {
	return ast.NewNodeDump(n, map[string]any{"Label": n.Label})
}

func TestCustomExtensionAdapter(t *testing.T) {
	doc := ast.NewDocument()
	para := ast.NewParagraph()
	para.AppendChild(&CustomNode{Label: "generated", Count: 3})
	doc.AppendChild(para)

	// Without a registered adapter the export must fail with a concrete error
	// rather than an empty object.
	if _, err := astjson.NewExporter().Marshal(doc, nil); err == nil {
		t.Fatal("expected unknown node error before registering adapter")
	}

	exp := astjson.NewExporter()
	exp.Register(customNodeKind, astjson.AdapterFunc(func(node ast.Node, c *astjson.Context) ([]astjson.Property, error) {
		n := node.(*CustomNode)
		owned := text.NewSingleLineValueFromString(n.Label, text.IdentityDecoder)
		return []astjson.Property{
			astjson.Prop("label", c.TextValue(owned)),
			astjson.Prop("count", c.Int(n.Count)),
		}, nil
	}))

	b, err := exp.Marshal(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"type": "example.com/CustomNode",`,
		`"label": "generated",`,
		`"count": 3,`,
		`"children": []`,
	} {
		if !bytes.Contains(b, []byte(want)) {
			t.Fatalf("custom adapter output missing %q:\n%s", want, b)
		}
	}
	// Properties must appear in the adapter's declared order: label before count.
	if bytes.Index(b, []byte(`"label"`)) > bytes.Index(b, []byte(`"count"`)) {
		t.Fatalf("property order not preserved:\n%s", b)
	}

	// Owned values carry no ranges even when source ranges are requested.
	expSrc := astjson.NewExporter(astjson.WithSourceRange(true))
	expSrc.Register(customNodeKind, astjson.AdapterFunc(func(node ast.Node, c *astjson.Context) ([]astjson.Property, error) {
		if !c.IncludeSource() {
			t.Fatal("IncludeSource must report true on the source-range exporter")
		}
		n := node.(*CustomNode)
		owned := text.NewSingleLineValueFromString(n.Label, text.IdentityDecoder)
		return []astjson.Property{astjson.Prop("label", c.TextValue(owned))}, nil
	}))
	bSrc, err := expSrc.Marshal(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(bSrc, []byte(`"text": "generated",`)) ||
		!bytes.Contains(bSrc, []byte(`"ranges": []`)) {
		t.Fatalf("owned value must keep an explicit empty ranges array:\n%s", bSrc)
	}
}

func TestRegistryRejectsNilAdapter(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register(nil) must panic")
		}
	}()
	astjson.NewRegistry().Register(customNodeKind, nil)
}

func TestAdapterFuncImplementsAdapter(t *testing.T) {
	var _ astjson.Adapter = astjson.AdapterFunc(func(_ ast.Node, _ *astjson.Context) ([]astjson.Property, error) {
		return nil, nil
	})
}
