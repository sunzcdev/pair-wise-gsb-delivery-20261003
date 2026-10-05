package ast

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/yuin/goldmark/v2/text"
)

func exportCanonicalNode(node Node, source []byte, cfg canonicalCfg, path []string) (*CanonicalNode, error) {
	kind := node.Kind()
	properties, err := canonicalProperties(node, source, cfg)
	if err != nil {
		return nil, &CanonicalExportError{
			Kind: kind.String(),
			Path: cloneKindPath(path),
			Err:  err,
		}
	}

	out := &CanonicalNode{
		Kind:       kind.String(),
		Properties: properties,
		withSource: cfg.withSource,
	}

	for _, attr := range node.Attributes() {
		out.Attributes = append(out.Attributes, CanonicalProperty{
			Name:  attr.Name,
			Value: attr.Value.Value(source),
		})
	}

	if cfg.withSource {
		if start, stop, hasStart, hasStop := canonicalRange(node, source); hasStart {
			out.Start, out.HasStart = start, true
			if hasStop {
				out.Stop, out.HasStop = stop, true
			}
		}
	}

	childPath := append(cloneKindPath(path), kind.String())
	for c := node.FirstChild(); c != nil; c = c.NextSibling() {
		child, err := exportCanonicalNode(c, source, cfg, childPath)
		if err != nil {
			return nil, err
		}
		out.Children = append(out.Children, child)
	}
	return out, nil
}

func cloneKindPath(path []string) []string {
	if len(path) == 0 {
		return nil
	}
	out := make([]string, len(path))
	copy(out, path)
	return out
}

func canonicalProperties(node Node, source []byte, cfg canonicalCfg) ([]CanonicalProperty, error) {
	var props []CanonicalProperty

	switch n := node.(type) {
	case *Document:
		// No semantic properties beyond those shared by every block node.
	case *Paragraph:
	case *Heading:
		props = append(props,
			CanonicalProperty{"level", n.Level},
			CanonicalProperty{"headingKind", n.HeadingKind.String()},
		)
	case *ThematicBreak:
	case *CodeBlock:
		props = append(props, CanonicalProperty{"codeBlockKind", n.CodeBlockKind.String()})
		if !n.Info.IsEmpty() {
			props = append(props, CanonicalProperty{"info", n.Info.Value(source)})
		}
		props = append(props, CanonicalProperty{"value", linesValue(n.Value, source)})
	case *Blockquote:
	case *List:
		props = append(props,
			CanonicalProperty{"ordered", n.IsOrdered()},
			CanonicalProperty{"marker", string([]byte{n.Marker})},
			CanonicalProperty{"tight", n.IsTight},
		)
		if n.IsOrdered() {
			props = append(props, CanonicalProperty{"startIndex", n.Start})
		}
	case *ListItem:
	case *HTMLBlock:
		props = append(props,
			CanonicalProperty{"htmlBlockKind", n.HTMLBlockKind.String()},
			CanonicalProperty{"value", linesValue(n.Value, source)},
		)
	case *LinkReferenceDefinition:
		props = append(props,
			CanonicalProperty{"label", n.Label.Value(source)},
			CanonicalProperty{"destination", n.Destination.Value(source)},
		)
		if !n.Title.IsEmpty() {
			props = append(props, CanonicalProperty{"title", n.Title.Value(source)})
		}
	case *Text:
		props = append(props, CanonicalProperty{"value", n.Value.Value(source)})
		if n.SoftLineBreak() {
			props = append(props, CanonicalProperty{"softLineBreak", true})
		}
		if n.HardLineBreak() {
			props = append(props, CanonicalProperty{"hardLineBreak", true})
		}
	case *CodeSpan:
		props = append(props, CanonicalProperty{"value", n.Value.Value(source)})
	case *Emphasis:
	case *Strong:
	case *Link:
		props = linkLikeProperties(n.Destination, n.Title, n.Reference, source)
	case *Image:
		props = linkLikeProperties(n.Destination, n.Title, n.Reference, source)
	case *AutoLink:
		props = append(props,
			CanonicalProperty{"destination", n.Destination.Value(source)},
			CanonicalProperty{"label", n.Label.Value(source)},
			CanonicalProperty{"text", n.Text.Value(source)},
		)
	case *RawHTML:
		props = append(props, CanonicalProperty{"value", n.Value.Value(source)})
	default:
		adapter := cfg.adapters[node.Kind()]
		if adapter == nil {
			adapter = canonicalAdapters[node.Kind()]
		}
		if adapter == nil {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedCanonicalNode, node.Kind())
		}
		var err error
		props, err = adapter(node, source)
		if err != nil {
			return nil, err
		}
	}

	if block, ok := node.(BlockNode); ok {
		props = append(props, CanonicalProperty{"blankPreviousLines", block.HasBlankPreviousLines()})
	}

	for _, p := range props {
		if _, reserved := reservedCanonicalKeys[p.Name]; reserved {
			return nil, fmt.Errorf("ast: canonical property %q uses a reserved key", p.Name)
		}
		if err := validateCanonicalValue(p.Value); err != nil {
			return nil, fmt.Errorf("ast: canonical property %q: %w", p.Name, err)
		}
	}
	return props, nil
}

func linkLikeProperties(
	destination text.SingleLineValue,
	title text.MultiLineValue,
	reference *ReferenceLink,
	source []byte,
) []CanonicalProperty {
	props := []CanonicalProperty{
		{Name: "destination", Value: destination.Value(source)},
	}
	if !title.IsEmpty() {
		props = append(props, CanonicalProperty{"title", title.Value(source)})
	}
	if reference != nil {
		props = append(props, CanonicalProperty{
			Name: "reference",
			Value: CanonicalObject{
				{Name: "kind", Value: reference.ReferenceLinkKind.String()},
				{Name: "value", Value: reference.Value.Value(source)},
			},
		})
	}
	return props
}

func linesValue(lines text.Lines, source []byte) string {
	var buf bytes.Buffer
	_, _ = lines.WriteTo(&buf, source)
	return buf.String()
}

// validateCanonicalValue checks that v only contains values the canonical
// encoder can emit deterministically. In particular, maps other than
// map[string]any are rejected because their key order cannot be guaranteed.
func validateCanonicalValue(v any) error {
	switch v := v.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number, json.RawMessage:
		return nil
	case []any:
		for _, item := range v {
			if err := validateCanonicalValue(item); err != nil {
				return err
			}
		}
	case CanonicalObject:
		return validateCanonicalObject(v)
	case []CanonicalProperty:
		return validateCanonicalObject(CanonicalObject(v))
	case map[string]any:
		for _, item := range v {
			if err := validateCanonicalValue(item); err != nil {
				return err
			}
		}
	case *CanonicalNode:
		// Nested canonical nodes are built by the exporter itself.
	default:
		return fmt.Errorf("unsupported canonical value of type %T", v)
	}
	return nil
}

func validateCanonicalObject(obj CanonicalObject) error {
	seen := map[string]struct{}{}
	for _, p := range obj {
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("duplicate object key %q", p.Name)
		}
		seen[p.Name] = struct{}{}
		if err := validateCanonicalValue(p.Value); err != nil {
			return err
		}
	}
	return nil
}
