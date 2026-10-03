// Package infer derives a JSON Schema 2020-12 contract from example TOML
// documents.
//
// The inferred schema is a structural contract only: it records the shapes
// observed in the samples, never the business rules behind them. Samples
// cannot prove that a key is mandatory for every valid document, that numbers
// are bounded, that strings match a pattern, or that defaults exist, so the
// output contains no "required" claims beyond the intersection of keys
// actually present, no minimum/maximum keywords, no patterns, and no default
// values. The result is deterministic: feeding the same samples in different
// orders produces byte-identical output.
//
// Supported JSON Schema 2020-12 subset:
//
//   - objects: "type": "object" with "properties" and, when non-empty,
//     "required" listing the intersection of keys present in every object
//     sample observed at that path (including every member of array tables);
//
//   - arrays: "type": "array" with a single "items" schema. Members of all
//     array samples (including TOML arrays of tables) are merged into that
//     schema. An empty array contributes no element evidence and never causes
//     an "items" schema to be guessed;
//
//   - scalars: "type" is one of "string", "integer", "number", "boolean".
//     When different scalar (or composite) kinds occur at the same path, the
//     path is described with "anyOf";
//
//   - TOML temporal types are emitted as "type": "string" with an
//     "x-toml-type" annotation rather than being reflected as Go structs:
//
//     x-toml-type        TOML value
//     "offset-date-time" 1985-04-12T23:20:50.52-07:00
//     "local-date-time"  1985-04-12T23:20:50.52
//     "local-date"       1985-04-12
//     "local-time"       23:20:50.52
//
// Nothing else is emitted: no enums, formats, min/max annotations, patterns,
// defaults, or additionalProperties claims.
package infer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

const schemaURI = "https://json-schema.org/draft/2020-12/schema"

// kind identifies the structural shape of an inferred schema node.
type kind int

const (
	kLeaf kind = iota
	kObject
	kArray
	kUnion
)

// leaf describes a scalar schema. Non-empty xTOMLType marks a TOML temporal
// value rendered as a string.
type leaf struct {
	jsonType  string
	xTOMLType string
}

// node is an inferred schema fragment. Fields are used depending on kind:
//
//   - kLeaf: leaf;
//   - kObject: props, objectCount, presentCount;
//   - kArray: items (nil until at least one non-empty array is observed);
//   - kUnion: variants.
type node struct {
	k kind
	l leaf

	// props and presentCount are used for kObject nodes. objectCount is the
	// number of object samples folded into the node; presentCount[key] is the
	// number of those samples in which key appeared, so the intersection is
	// presentCount[key] == objectCount.
	props        map[string]*node
	presentCount map[string]int
	objectCount  int

	items *node

	variants []*node
}

// Schema reads one or more example TOML documents and returns the JSON Schema
// 2020-12 contract inferred from all of them. TOML documents are always
// tables, so the top-level schema is an object.
//
// Each reader is consumed independently; a parse failure in any sample
// aborts inference. With no samples there is no evidence to infer from, so
// Schema returns an error instead of inventing an empty contract.
func Schema(samples ...io.Reader) (json.RawMessage, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("infer: at least one TOML sample is required")
	}

	root := &node{k: kObject, props: map[string]*node{}, presentCount: map[string]int{}}

	for i, sample := range samples {
		var doc map[string]interface{}
		if err := toml.NewDecoder(sample).Decode(&doc); err != nil {
			return nil, fmt.Errorf("infer: parsing sample %d: %w", i+1, err)
		}

		root.mergeObject(doc)
	}

	rendered := root.render()
	rendered["$schema"] = schemaURI

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rendered); err != nil {
		return nil, fmt.Errorf("infer: encoding schema: %w", err)
	}

	return json.RawMessage(buf.Bytes()), nil
}

// nodeFromValue converts a value decoded from TOML into a fresh schema node
// describing exactly that value.
func nodeFromValue(value interface{}) *node {
	switch v := value.(type) {
	case map[string]interface{}:
		return objectNode(v)
	case []interface{}:
		n := &node{k: kArray}
		for _, elem := range v {
			n.items = merge(n.items, nodeFromValue(elem))
		}
		return n
	case string:
		return &node{k: kLeaf, l: leaf{jsonType: "string"}}
	case int64:
		return &node{k: kLeaf, l: leaf{jsonType: "integer"}}
	case float64:
		return &node{k: kLeaf, l: leaf{jsonType: "number"}}
	case bool:
		return &node{k: kLeaf, l: leaf{jsonType: "boolean"}}
	case time.Time:
		return &node{k: kLeaf, l: leaf{jsonType: "string", xTOMLType: "offset-date-time"}}
	case toml.LocalDateTime:
		return &node{k: kLeaf, l: leaf{jsonType: "string", xTOMLType: "local-date-time"}}
	case toml.LocalDate:
		return &node{k: kLeaf, l: leaf{jsonType: "string", xTOMLType: "local-date"}}
	case toml.LocalTime:
		return &node{k: kLeaf, l: leaf{jsonType: "string", xTOMLType: "local-time"}}
	default:
		// Values come from toml.Decoder, which only produces the types above.
		panic(fmt.Sprintf("infer: unsupported decoded value type %T", value))
	}
}

// objectNode builds an object node from one object sample.
func objectNode(values map[string]interface{}) *node {
	n := &node{
		k:            kObject,
		props:        map[string]*node{},
		presentCount: map[string]int{},
		objectCount:  1,
	}
	for key, value := range values {
		n.presentCount[key] = 1
		n.props[key] = nodeFromValue(value)
	}
	return n
}

// merge folds evidence from src into dst and returns the combined node. The
// result is independent of the order in which samples are merged: leaves are
// combined by identity, objects and arrays always merge with their own kind,
// and unions are rendered in canonical order.
func merge(dst, src *node) *node {
	if dst == nil {
		return src
	}

	if dst.k == kUnion {
		dst.absorb(src)
		return dst
	}
	if src.k == kUnion {
		src.absorb(dst)
		return src
	}

	if dst.k != src.k {
		return &node{k: kUnion, variants: []*node{dst, src}}
	}

	switch dst.k {
	case kLeaf:
		if dst.l != src.l {
			return &node{k: kUnion, variants: []*node{dst, src}}
		}
		return dst
	case kObject:
		dst.objectCount += src.objectCount
		for key, child := range src.props {
			dst.presentCount[key] += src.presentCount[key]
			if existing, ok := dst.props[key]; ok {
				dst.props[key] = merge(existing, child)
			} else {
				dst.props[key] = child
			}
		}
	case kArray:
		if src.items != nil {
			dst.items = merge(dst.items, src.items)
		}
	}

	return dst
}

// mergeObject folds a single decoded object sample into the node, which must
// itself be an object node.
func (n *node) mergeObject(values map[string]interface{}) {
	n.objectCount++
	for key, value := range values {
		n.presentCount[key]++
		child := nodeFromValue(value)
		if existing, ok := n.props[key]; ok {
			n.props[key] = merge(existing, child)
		} else {
			n.props[key] = child
		}
	}
}

// absorb merges other into the union, reusing an existing compatible variant
// instead of appending a duplicate.
func (n *node) absorb(other *node) {
	if other.k == kUnion {
		for _, variant := range other.variants {
			n.absorb(variant)
		}
		return
	}

	for _, variant := range n.variants {
		if compatible(variant, other) {
			merge(variant, other)
			return
		}
	}

	n.variants = append(n.variants, other)
}

// compatible reports whether two non-union nodes can share a single union
// variant. Objects merge with objects and arrays with arrays; leaves merge
// only when they describe the same scalar type.
func compatible(a, b *node) bool {
	if a.k != b.k {
		return false
	}
	if a.k == kLeaf {
		return a.l == b.l
	}
	return true
}

// render produces the JSON Schema representation of the node as a map ready
// for deterministic encoding by encoding/json (which sorts map keys).
func (n *node) render() map[string]interface{} {
	switch n.k {
	case kLeaf:
		m := map[string]interface{}{"type": n.l.jsonType}
		if n.l.xTOMLType != "" {
			m["x-toml-type"] = n.l.xTOMLType
		}
		return m
	case kObject:
		m := map[string]interface{}{"type": "object"}
		props := make(map[string]interface{}, len(n.props))
		for key, child := range n.props {
			props[key] = child.render()
		}
		m["properties"] = props

		required := make([]string, 0, len(n.props))
		for key := range n.props {
			if n.presentCount[key] == n.objectCount {
				required = append(required, key)
			}
		}
		if len(required) > 0 {
			sort.Strings(required)
			m["required"] = required
		}
		return m
	case kArray:
		m := map[string]interface{}{"type": "array"}
		if n.items != nil {
			m["items"] = n.items.render()
		}
		return m
	case kUnion:
		variants := make([]interface{}, len(n.variants))
		for i, variant := range n.variants {
			variants[i] = variant.render()
		}
		sort.SliceStable(variants, func(i, j int) bool {
			return canonical(variants[i]) < canonical(variants[j])
		})
		return map[string]interface{}{"anyOf": variants}
	default:
		panic(fmt.Sprintf("infer: unknown node kind %d", n.k))
	}
}

// canonical returns the compact JSON encoding used to order anyOf variants
// deterministically regardless of sample order.
func canonical(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
