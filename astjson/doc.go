// Package astjson provides a canonical, versioned JSON exporter for goldmark
// AST trees.
//
// Unlike [github.com/yuin/goldmark/v2/ast.NodeDump.PrettyPrint], which is
// human-oriented and explicitly unstable, the output of this package is a
// stable interchange format intended for comparing semantic trees produced by
// different parses, caching parse results, or snapshot testing.
//
// # Format
//
// The top-level value is an object with two keys, in this order:
//
//   - "version": the integer format version ([FormatVersion]).
//   - "tree": the exported root node.
//
// Every node is an object with the following keys, in this order:
//
//   - "type": the node kind name (see
//     [github.com/yuin/goldmark/v2/ast.NodeKind.String]).
//   - "position": present only when source ranges are requested; the node
//     start byte offset, or null when the node has no defined position.
//   - Node-specific semantic properties, in a fixed order defined by the
//     registered adapter.
//   - "source": present only for block nodes with at least one source segment
//     when source ranges are requested; a list of [start, stop) byte ranges
//     covering the block's source segments.
//   - "attributes": present only when the node has attributes; an array of
//     [name, value] pairs in attachment order.
//   - "blankPreviousLines": present only for block nodes.
//   - "children": an array of nodes in document order, always present.
//
// Text-bearing properties are materialized to their actual decoded content
// rather than source offsets. When source ranges are requested, each
// materialized text value is an object with two keys:
//
//   - "text": the materialized string content.
//   - "ranges": an array of [start, stop) byte ranges backing the value, in
//     order. The array is empty for generated text that does not come from the
//     source (owned values).
//
// When source ranges are not requested, text-bearing properties are emitted as
// plain JSON strings. In that mode, two inputs that parse to the same semantic
// tree produce identical output even when line wrapping differs, as long as
// the materialized content is the same.
//
// Parent and sibling pointers, runtime addresses and other non-serializable
// state are never emitted. Object keys are always emitted in the documented
// order; no map iteration order is relied upon.
//
// # Extensions
//
// Only nodes whose kind has a registered [Adapter] can be exported. Unknown
// node kinds cause [ErrUnknownNode] to be returned (wrapped with the node kind
// name) rather than being silently emitted as empty objects. Extension
// packages provide adapters for their custom nodes; see
// [github.com/yuin/goldmark/v2/extension] for the built-in extensions.
package astjson

// FormatVersion is the version of the canonical JSON format produced by this
// package.
//
// The version is incremented whenever the wire format changes in an
// incompatible way (renamed keys, changed key order, changed value encoding,
// changed node structure). Additive changes that do not break existing
// consumers do not necessarily increment it.
const FormatVersion = 1
