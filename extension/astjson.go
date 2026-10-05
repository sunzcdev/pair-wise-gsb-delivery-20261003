package extension

import (
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/astjson"
	east "github.com/yuin/goldmark/v2/extension/ast"
)

// RegisterASTJSONAdapters registers canonical JSON export adapters for the
// node kinds introduced by the built-in extensions (GFM tables,
// strikethrough, footnotes and definition lists) onto r.
//
// Typical usage:
//
//	exporter := extension.RegisterAdaptersOn(astjson.NewExporter())
//
// or, when building a custom registry:
//
//	registry := extension.RegisterASTJSONAdapters(astjson.DefaultRegistry.Clone())
func RegisterASTJSONAdapters(r *astjson.Registry) *astjson.Registry {
	registerASTJSONAdapters(func(kind ast.NodeKind, adapter astjson.Adapter) {
		r.Register(kind, adapter)
	})
	return r
}

// RegisterAdaptersOn registers the built-in extension adapters directly on e
// and returns e for chaining.
func RegisterAdaptersOn(e *astjson.Exporter) *astjson.Exporter {
	registerASTJSONAdapters(e.Register)
	return e
}

func registerASTJSONAdapters(register func(kind ast.NodeKind, adapter astjson.Adapter)) {
	noop := func(_ ast.Node, _ *astjson.Context) ([]astjson.Property, error) {
		return nil, nil
	}

	register(east.KindTable, astjson.AdapterFunc(noop))
	register(east.KindTableHeader, astjson.AdapterFunc(noop))
	register(east.KindTableBody, astjson.AdapterFunc(noop))
	register(east.KindTableRow, astjson.AdapterFunc(noop))
	register(east.KindStrikethrough, astjson.AdapterFunc(noop))
	register(east.KindDefinitionList, astjson.AdapterFunc(noop))
	register(east.KindDefinitionTerm, astjson.AdapterFunc(noop))

	register(east.KindTableCell, astjson.AdapterFunc(func(node ast.Node, c *astjson.Context) ([]astjson.Property, error) {
		n := node.(*east.TableCell)
		return []astjson.Property{
			astjson.Prop("alignment", c.String(n.Alignment.String())),
		}, nil
	}))
	register(east.KindDefinitionDescription, astjson.AdapterFunc(func(node ast.Node, c *astjson.Context) ([]astjson.Property, error) {
		n := node.(*east.DefinitionDescription)
		return []astjson.Property{
			astjson.Prop("tight", c.Bool(n.IsTight)),
		}, nil
	}))
	register(east.KindFootnoteReference, astjson.AdapterFunc(func(node ast.Node, c *astjson.Context) ([]astjson.Property, error) {
		n := node.(*east.FootnoteReference)
		return []astjson.Property{
			astjson.Prop("label", c.TextValue(n.Label)),
			astjson.Prop("index", c.Int(n.Index)),
			astjson.Prop("refIndex", c.Int(n.RefIndex)),
		}, nil
	}))
	register(east.KindFootnoteDefinition, astjson.AdapterFunc(func(node ast.Node, c *astjson.Context) ([]astjson.Property, error) {
		n := node.(*east.FootnoteDefinition)
		return []astjson.Property{
			astjson.Prop("label", c.TextValue(n.Label)),
		}, nil
	}))
}
