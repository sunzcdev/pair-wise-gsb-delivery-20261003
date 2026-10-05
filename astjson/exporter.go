package astjson

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/text"
)

// Context is passed to [Adapter] implementations while exporting a tree.
type Context struct {
	source        []byte
	includeSource bool
}

// Source returns the source bytes of the document being exported.
func (c *Context) Source() []byte {
	return c.source
}

// IncludeSource reports whether source ranges are included in the output.
func (c *Context) IncludeSource() bool {
	return c.includeSource
}

// TextValue materializes an inline text value to its decoded content.
//
// When source ranges are requested the result is an object containing both the
// materialized text and the ordered source ranges; otherwise it is the
// materialized JSON string.
func (c *Context) TextValue(v text.Value) jsonValue {
	return c.materialize(v.Value(c.source), v.Indices())
}

// LinesValue materializes raw block lines to their actual content. Unlike
// [Context.TextValue], no decoding (such as entity resolution) is applied.
func (c *Context) LinesValue(v text.Lines) jsonValue {
	return c.materializeLines(v)
}

// Bool returns a JSON boolean property value.
func (c *Context) Bool(v bool) jsonValue { return v }

// Int returns a JSON integer property value.
func (c *Context) Int(v int) jsonValue { return v }

// String returns a JSON string property value. The content is emitted verbatim
// and must already be materialized; use [Context.TextValue] for values still
// backed by the source.
func (c *Context) String(v string) jsonValue { return v }

// Object returns a JSON object with fields in the given order.
//
// Callers must pass an explicitly ordered field list; do not build the fields
// by ranging over a map.
func (c *Context) Object(fields ...orderedField) jsonValue {
	return newObject(fields...)
}

// Field builds an [orderedField] for [Context.Object].
func (c *Context) Field(key string, value jsonValue) orderedField {
	return orderedField{key: key, value: value}
}

// Null returns a JSON null value.
func (c *Context) Null() jsonValue { return nil }

func (c *Context) materialize(content string, indices []text.Index) jsonValue {
	if !c.includeSource {
		return content
	}
	ranges := make(jsonArray, 0, len(indices))
	for _, idx := range indices {
		if idx.Start == 0 && idx.Stop == 0 {
			continue
		}
		ranges = append(ranges, jsonArray{idx.Start, idx.Stop})
	}
	return newObject(
		orderedField{key: "text", value: content},
		orderedField{key: "ranges", value: ranges},
	)
}

func (c *Context) materializeLines(lines text.Lines) jsonValue {
	content := lines.Str(c.source)
	if !c.includeSource {
		return content
	}
	segs := lines.Segments()
	ranges := make(jsonArray, 0, len(segs))
	for _, seg := range segs {
		ranges = append(ranges, jsonArray{seg.Start, seg.Stop})
	}
	return newObject(
		orderedField{key: "text", value: content},
		orderedField{key: "ranges", value: ranges},
	)
}

// Option configures an [Exporter].
type Option func(*exporterConfig)

type exporterConfig struct {
	includeSource bool
	registry      *Registry
}

// WithSourceRange configures whether source byte ranges are included in the
// output. It defaults to false.
//
// Without source ranges the output depends only on the semantic tree and
// materialized text, so inputs that differ only in line wrapping can produce
// identical output.
func WithSourceRange(include bool) Option {
	return func(cfg *exporterConfig) {
		cfg.includeSource = include
	}
}

// WithRegistry configures the [Registry] used to resolve node adapters.
//
// When no registry is configured, [NewExporter] starts from
// [DefaultRegistry].
func WithRegistry(registry *Registry) Option {
	return func(cfg *exporterConfig) {
		cfg.registry = registry
	}
}

// An Exporter serializes AST nodes to the canonical JSON format.
//
// An Exporter is safe for concurrent use.
type Exporter struct {
	cfg exporterConfig
}

// NewExporter returns a new [Exporter] with the given options.
//
// By default the exporter uses a private copy of [DefaultRegistry], so
// registering additional adapters on the exporter (see
// [Exporter.Register]) never mutates global state.
func NewExporter(opts ...Option) *Exporter {
	cfg := exporterConfig{
		registry: DefaultRegistry.Clone(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.registry == nil {
		cfg.registry = NewRegistry()
	}
	return &Exporter{cfg: cfg}
}

// Register registers an adapter on the exporter's private registry.
//
// This is the intended way for extension users to add support for custom node
// kinds.
func (e *Exporter) Register(kind ast.NodeKind, adapter Adapter) {
	e.cfg.registry.Register(kind, adapter)
}

// ExportNode serializes node (typically a [ast.Document]) to compact
// canonical JSON bytes. The returned bytes are valid JSON and can be embedded
// or re-indented by callers; use [Exporter.Marshal] for the standard
// pretty-printed output.
func (e *Exporter) ExportNode(node ast.Node, source []byte) (json.RawMessage, error) {
	c := &Context{
		source:        source,
		includeSource: e.cfg.includeSource,
	}
	tree, err := e.exportNode(node, c)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := e.writeEnvelope(&buf, tree, true); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

// Marshal serializes node to canonical, indented JSON bytes.
//
// The output is deterministic for a given tree and option set: keys are emitted
// in a fixed order and children in document order. HTML characters such as
// ampersands and angle brackets are not escaped, so the bytes remain readable
// and stable.
func (e *Exporter) Marshal(node ast.Node, source []byte) ([]byte, error) {
	c := &Context{
		source:        source,
		includeSource: e.cfg.includeSource,
	}
	tree, err := e.exportNode(node, c)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := e.writeEnvelope(&buf, tree, false); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (e *Exporter) writeEnvelope(buf *bytes.Buffer, tree jsonValue, compact bool) error {
	envelope := newObject(
		orderedField{key: "version", value: FormatVersion},
		orderedField{key: "tree", value: tree},
	)
	if compact {
		return writeCompactValue(buf, envelope)
	}
	return writeIndentedValue(buf, envelope, "  ", 0)
}

// WriteTo serializes node and writes the canonical JSON to w, followed by a
// trailing newline.
func (e *Exporter) WriteTo(w io.Writer, node ast.Node, source []byte) error {
	b, err := e.Marshal(node, source)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = w.Write([]byte{'\n'})
	return err
}

func (e *Exporter) exportNode(node ast.Node, c *Context) (jsonValue, error) {
	adapter := e.cfg.registry.Lookup(node.Kind())
	if adapter == nil {
		return nil, &UnknownNodeError{Kind: node.Kind()}
	}
	properties, err := adapter.Export(node, c)
	if err != nil {
		return nil, err
	}

	fields := []orderedField{
		{key: "type", value: node.Kind().String()},
	}
	if c.includeSource {
		var position jsonValue = nil
		if pos := node.Pos(); pos >= 0 {
			position = pos
		}
		fields = append(fields, orderedField{key: "position", value: position})
	}
	for _, p := range properties {
		fields = append(fields, orderedField{key: p.Key, value: p.Value})
	}
	if block, ok := node.(ast.BlockNode); ok {
		if c.includeSource {
			ranges := make(jsonArray, 0)
			for _, seg := range block.Source() {
				ranges = append(ranges, jsonArray{seg.Start, seg.Stop})
			}
			fields = append(fields, orderedField{key: "source", value: ranges, omitEmpty: true})
		}
		fields = append(fields, orderedField{
			key:   "blankPreviousLines",
			value: block.HasBlankPreviousLines(),
		})
	}
	if attrs := node.Attributes(); len(attrs) > 0 {
		pairs := make(jsonArray, 0, len(attrs))
		for _, attr := range attrs {
			pairs = append(pairs, jsonArray{attr.Name, c.TextValue(attr.Value)})
		}
		fields = append(fields, orderedField{key: "attributes", value: pairs})
	}

	children := make(jsonArray, 0, node.ChildCount())
	for child := range node.Children() {
		exported, err := e.exportNode(child, c)
		if err != nil {
			return nil, err
		}
		children = append(children, exported)
	}
	fields = append(fields, orderedField{key: "children", value: children})

	return newObject(fields...), nil
}
