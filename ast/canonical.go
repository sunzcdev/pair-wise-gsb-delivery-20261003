package ast

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// CanonicalFormatVersion is the version of the canonical AST JSON format
// produced by [ExportCanonical] and [MarshalCanonical].
//
// The version is bumped whenever the on-disk format changes in an
// incompatible way. Format v1 nodes are JSON objects with keys in this order:
//
//   - "type": stable node kind name (string)
//   - node-specific semantic properties (zero or more, fixed order per kind)
//   - "start"/"stop": source byte offsets (only when source ranges are enabled)
//   - "attributes": attributes as an array of {"name", "value"} objects in
//     document order (only when the node has attributes)
//   - "children": ordered child nodes (always present)
//
// Textual property values are always materialized to their decoded content;
// no source offsets, pointers or runtime addresses are emitted.
const CanonicalFormatVersion = 1

// CanonicalProperty is a single named semantic property of a node in the
// canonical format. Properties are exported in slice order, so exporters do
// not depend on the randomized iteration order of maps.
type CanonicalProperty struct {
	// Name is the property key. It must not collide with the reserved keys
	// "type", "start", "stop", "attributes" or "children".
	Name string

	// Value must be a canonical value: nil, bool, string, a JSON-compatible
	// number, []any, map[string]any, [CanonicalObject], [json.RawMessage],
	// or a nested *[CanonicalNode].
	Value any
}

// CanonicalObject is an ordered JSON object in the canonical format.
// Adapters must use it instead of a map whenever key order matters.
type CanonicalObject []CanonicalProperty

// CanonicalNodeAdapter produces the semantic properties of a node that the
// built-in exporter does not know about (typically extension nodes).
//
// source is the original parser input and is used to materialize textual
// values. The returned properties are exported in order. If the adapter
// cannot export the node, it returns a non-nil error that is wrapped and
// reported by [ExportCanonical].
type CanonicalNodeAdapter func(node Node, source []byte) ([]CanonicalProperty, error)

// CanonicalSourceRanger is an optional interface a [Node] can implement to
// explicitly provide its source byte range. Both offsets are byte positions
// in the parser input; stop is exclusive.
type CanonicalSourceRanger interface {
	CanonicalSourceRange() (start, stop int)
}

var reservedCanonicalKeys = map[string]struct{}{
	"type":       {},
	"start":      {},
	"stop":       {},
	"attributes": {},
	"children":   {},
}

var canonicalAdapters = map[NodeKind]CanonicalNodeAdapter{}

// RegisterCanonicalNodeAdapter registers a [CanonicalNodeAdapter] for nodes
// of the given kind. It is intended to be called by extension packages,
// usually from an init function.
//
// Registering another adapter for the same kind panics.
func RegisterCanonicalNodeAdapter(kind NodeKind, adapter CanonicalNodeAdapter) {
	if adapter == nil {
		panic("ast: canonical node adapter must not be nil")
	}
	if _, exists := canonicalAdapters[kind]; exists {
		panic(fmt.Sprintf("ast: a canonical adapter for kind %q is already registered", kind))
	}
	canonicalAdapters[kind] = adapter
}

type canonicalCfg struct {
	withSource bool
	adapters   map[NodeKind]CanonicalNodeAdapter
}

// CanonicalOption configures a canonical export.
type CanonicalOption func(*canonicalCfg)

// WithCanonicalSource enables source byte ranges ("start"/"stop") in the
// exported nodes.
func WithCanonicalSource() CanonicalOption {
	return func(cfg *canonicalCfg) {
		cfg.withSource = true
	}
}

// WithCanonicalNodeAdapters supplies additional adapters for a single
// export. They take precedence over globally registered adapters.
func WithCanonicalNodeAdapters(adapters map[NodeKind]CanonicalNodeAdapter) CanonicalOption {
	return func(cfg *canonicalCfg) {
		if cfg.adapters == nil {
			cfg.adapters = map[NodeKind]CanonicalNodeAdapter{}
		}
		for kind, adapter := range adapters {
			cfg.adapters[kind] = adapter
		}
	}
}

// CanonicalNode is the canonical, in-memory representation of an exported
// AST node.
type CanonicalNode struct {
	// Kind is the stable node kind name.
	Kind string

	// Properties are the node's semantic properties in export order.
	Properties []CanonicalProperty

	// Start and Stop are source byte offsets. Meaningful only when HasStart
	// (or HasStop) is true and source ranges were requested.
	Start    int
	Stop     int
	HasStart bool
	HasStop  bool

	// Attributes are the node's attributes in document order.
	Attributes CanonicalObject

	// Children are the ordered child nodes.
	Children []*CanonicalNode

	withSource bool
}

// CanonicalDocument is the canonical, in-memory representation of an
// exported AST tree.
type CanonicalDocument struct {
	// Version is [CanonicalFormatVersion].
	Version int

	// Root is the exported root node.
	Root *CanonicalNode
}

// ErrUnsupportedCanonicalNode is returned when a node has neither a
// built-in canonical representation nor a registered adapter.
var ErrUnsupportedCanonicalNode = errors.New("ast: no canonical adapter registered for node kind")

// CanonicalExportError wraps a failure while exporting a node.
type CanonicalExportError struct {
	// Kind is the kind name of the offending node.
	Kind string

	// Path is the chain of kind names from the exported root to the node.
	Path []string

	// Err is the underlying error.
	Err error
}

// Error implements error.
func (e *CanonicalExportError) Error() string {
	return fmt.Sprintf("ast: canonical export failed at %s (kind %s): %v",
		joinKindPath(e.Path), e.Kind, e.Err)
}

// Unwrap returns the underlying error.
func (e *CanonicalExportError) Unwrap() error { return e.Err }

func joinKindPath(path []string) string {
	if len(path) == 0 {
		return "<root>"
	}
	var b bytes.Buffer
	for i, p := range path {
		if i > 0 {
			b.WriteString(" > ")
		}
		b.WriteString(p)
	}
	return b.String()
}

// ExportCanonical exports the AST rooted at node into a versioned canonical
// representation. source is the original parser input and is used to
// materialize textual values and, optionally, source ranges.
func ExportCanonical(node Node, source []byte, opts ...CanonicalOption) (*CanonicalDocument, error) {
	cfg := canonicalCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}
	root, err := exportCanonicalNode(node, source, cfg, nil)
	if err != nil {
		return nil, err
	}
	return &CanonicalDocument{Version: CanonicalFormatVersion, Root: root}, nil
}

// MarshalCanonical exports the AST and writes its canonical JSON encoding to
// w. The output is indented with two spaces, does not escape HTML-sensitive
// characters, and ends with a newline.
func MarshalCanonical(w io.Writer, node Node, source []byte, opts ...CanonicalOption) error {
	doc, err := ExportCanonical(node, source, opts...)
	if err != nil {
		return err
	}
	out, err := doc.MarshalJSON()
	if err != nil {
		return err
	}
	if _, err := w.Write(out); err != nil {
		return err
	}
	_, err = w.Write([]byte{'\n'})
	return err
}

// MarshalCanonicalToBytes is like [MarshalCanonical] but returns the JSON
// bytes. The result ends with a newline.
func MarshalCanonicalToBytes(node Node, source []byte, opts ...CanonicalOption) ([]byte, error) {
	var buf bytes.Buffer
	if err := MarshalCanonical(&buf, node, source, opts...); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
