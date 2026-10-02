package jsonpatch

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/evanphx/json-patch/v5/internal/json"
)

// ApplyWithInverse mutates a JSON document according to the patch and returns
// both the new document and an inverse patch. Applying the inverse patch to
// the new document restores the original document.
//
// A "test" operation that passes is carried into the inverse patch so that the
// undo verifies the restored value. When a "test" fails (or any other operation
// fails), no patch is applied and the returned inverse patch is nil.
func (p Patch) ApplyWithInverse(doc []byte) ([]byte, Patch, error) {
	return p.ApplyWithInverseAndOptions(doc, NewApplyOptions())
}

// ApplyWithInverseAndOptions is ApplyWithInverse with explicit ApplyOptions.
func (p Patch) ApplyWithInverseAndOptions(doc []byte, options *ApplyOptions) ([]byte, Patch, error) {
	return p.ApplyIndentWithInverseAndOptions(doc, "", options)
}

// ApplyIndentWithInverseAndOptions mutates a JSON document according to the
// patch and the passed in ApplyOptions. It returns the new document, indented
// when indent is non-empty, and an inverse patch that restores the original
// document.
func (p Patch) ApplyIndentWithInverseAndOptions(doc []byte, indent string, options *ApplyOptions) ([]byte, Patch, error) {
	data, inverse, err := p.applyCore(doc, options, true)
	if err != nil {
		return nil, nil, err
	}

	if indent == "" {
		return data, inverse, nil
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", indent); err != nil {
		return nil, nil, err
	}

	return buf.Bytes(), inverse, nil
}

func prependOps(existing Patch, ops Patch) Patch {
	if len(ops) == 0 {
		return existing
	}

	combined := make(Patch, 0, len(existing)+len(ops))
	combined = append(combined, ops...)
	combined = append(combined, existing...)
	return combined
}

var (
	rawAdd     = json.RawMessage(`"add"`)
	rawRemove  = json.RawMessage(`"remove"`)
	rawReplace = json.RawMessage(`"replace"`)
)

func makeInverseOp(kind *json.RawMessage, path string, value *lazyNode, options *ApplyOptions) Operation {
	pathRaw, err := json.MarshalEscaped(path, options.EscapeHTML)
	if err != nil {
		pathRaw = []byte(`""`)
	}

	op := Operation{
		"op":   kind,
		"path": newRawMessage(pathRaw),
	}

	if value != nil {
		raw, merr := nodeRaw(value, options)
		if merr != nil {
			return op
		}
		op["value"] = newRawMessage(raw)
	}

	return op
}

func addInverseOp(path string, value *lazyNode, options *ApplyOptions) Operation {
	return makeInverseOp(&rawAdd, path, value, options)
}

func removeInverseOp(path string) Operation {
	op := Operation{
		"op":   &rawRemove,
		"path": newRawMessage(mustMarshalString(path)),
	}
	return op
}

func replaceInverseOp(path string, value *lazyNode, options *ApplyOptions) Operation {
	return makeInverseOp(&rawReplace, path, value, options)
}

func mustMarshalString(s string) []byte {
	raw, err := json.Marshal(s)
	if err != nil {
		return []byte(`""`)
	}
	return raw
}

// nodeRaw returns a compact, standalone copy of the node's JSON encoding.
func nodeRaw(node *lazyNode, options *ApplyOptions) ([]byte, error) {
	if node == nil {
		return rawJSONNull, nil
	}

	data, err := json.MarshalEscaped(node, options.EscapeHTML)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// replaceRootInverse captures the whole current document so that an "add" or
// "replace" against the root pointer can be undone by replacing the root
// again.
func replaceRootInverse(doc container, options *ApplyOptions) (Patch, error) {
	data, err := json.MarshalEscaped(doc, options.EscapeHTML)
	if err != nil {
		return nil, err
	}

	return Patch{replaceInverseOp("", newLazyNode(newRawMessage(data)), options)}, nil
}

// addIndex resolves the concrete index at which a partialArray add places a
// value, mirroring partialArray.add's handling of "-" and negative indices.
func addIndex(ary *partialArray, key string, options *ApplyOptions) (int, error) {
	if key == "-" {
		return len(ary.nodes), nil
	}

	idx, err := strconv.Atoi(key)
	if err != nil {
		return 0, fmt.Errorf("value was not a proper array index: '%s': %w", key, err)
	}

	length := len(ary.nodes) + 1

	if idx >= length {
		return 0, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
	}

	if idx < 0 {
		if !options.SupportNegativeIndices {
			return 0, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		if idx < -length {
			return 0, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		idx += length
	}

	return idx, nil
}

// existingIndex resolves a concrete index into an existing array, mirroring
// partialArray.get's handling of negative indices.
func existingIndex(length int, key string, options *ApplyOptions) (int, error) {
	idx, err := strconv.Atoi(key)
	if err != nil {
		return 0, err
	}

	if idx >= length {
		return 0, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
	}

	if idx < 0 {
		if !options.SupportNegativeIndices {
			return 0, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		if idx < -length {
			return 0, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		idx += length
	}

	return idx, nil
}

// withIndex replaces the final reference token of path with a concrete index.
// Parent tokens are left untouched, so escaped keys survive unchanged.
func withIndex(path string, idx int) string {
	last := strings.LastIndexByte(path, '/')
	if last < 0 {
		return path
	}

	return path[:last+1] + strconv.Itoa(idx)
}

// pathMissingDepth returns how many trailing reference tokens of path lead
// through nodes that do not exist yet. A return value of 0 means the whole
// parent chain exists. The value is computed before the patch runs, so it
// identifies containers created by EnsurePathExistsOnAdd.
func pathMissingDepth(doc *container, path string, options *ApplyOptions) int {
	split := strings.Split(path, "/")
	if len(split) < 2 {
		return 0
	}

	parts := split[1:]
	cur := *doc
	depth := 0

	for i, part := range parts {
		if i == len(parts)-1 {
			break
		}

		next, err := cur.get(decodePatchKey(part), options)
		if next == nil || err != nil {
			depth = len(parts) - i
			break
		}

		if isArray(*next.raw) {
			ary, aerr := next.intoAry()
			if aerr != nil {
				return 0
			}
			cur = ary
		} else {
			obj, oerr := next.intoDoc(options)
			if oerr != nil {
				return 0
			}
			cur = obj
		}
	}

	return depth
}

// inversePathCleanup builds remove operations for newly created intermediate
// nodes, deepest first. The leaf change itself is handled by the caller.
func inversePathCleanup(path string, depth int) Patch {
	split := strings.Split(path, "/")
	if depth <= 1 || depth > len(split)-1 {
		return nil
	}

	ops := make(Patch, 0, depth-1)

	// split layout: ["", token0, token1, ...]; intermediate nodes run up to
	// the final key. Remove the deepest new container first.
	for i := 0; i < depth-1; i++ {
		cut := len(split) - 2 - i
		if cut < 1 {
			break
		}
		parentPath := strings.Join(split[:cut+1], "/")
		ops = append(ops, removeInverseOp(parentPath))
	}

	return ops
}
