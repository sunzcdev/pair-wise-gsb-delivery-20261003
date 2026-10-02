package jsonpatch

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/evanphx/json-patch/v5/internal/json"
)

// inverseResolver completes the inverse operations for a single forward
// operation once that operation has been applied. It is needed for operations
// whose inverse can only be determined from the post-operation state, such as
// a "move" targeting the "-" (end) of an array.
type inverseResolver func(doc *container) (Patch, error)

// ApplyWithInverse mutates a JSON document according to the patch, and returns
// the new document along with an inverse patch that, when applied to the new
// document, restores a document semantically equal to the original one.
//
// If the patch cannot be applied, both the resulting document and the inverse
// patch are nil: a failed application never yields a partial inverse patch.
func (p Patch) ApplyWithInverse(doc []byte) ([]byte, Patch, error) {
	return p.ApplyWithInverseWithOptions(doc, NewApplyOptions())
}

// ApplyWithInverseWithOptions mutates a JSON document according to the patch
// and the passed in ApplyOptions, and returns the new document along with an
// inverse patch that undoes it.
//
// EnsurePathExistsOnAdd is not supported: the intermediate containers it
// creates cannot be undone by a plain inverse patch, so an error is returned
// instead of an incorrect inverse.
func (p Patch) ApplyWithInverseWithOptions(doc []byte, options *ApplyOptions) ([]byte, Patch, error) {
	if options.EnsurePathExistsOnAdd {
		return nil, nil, fmt.Errorf("ApplyWithInverse does not support EnsurePathExistsOnAdd: %w", ErrInvalid)
	}

	if len(doc) == 0 {
		return doc, nil, nil
	}

	if !json.Valid(doc) {
		return nil, nil, ErrInvalid
	}

	raw := json.RawMessage(doc)
	self := newLazyNode(&raw)

	var pd container
	if doc[0] == '[' {
		pd = &partialArray{
			self: self,
		}
	} else {
		pd = &partialDoc{
			self: self,
			opts: options,
		}
	}

	err := unmarshal(doc, pd)

	if err != nil {
		return nil, nil, err
	}

	var inverse Patch
	var accumulatedCopySize int64

	for _, op := range p {
		inv, resolve, err := p.invertOperation(&pd, op, options)
		if err != nil {
			return nil, nil, err
		}

		if err := p.applyOperation(&pd, op, &accumulatedCopySize, options); err != nil {
			return nil, nil, err
		}

		if resolve != nil {
			inv, err = resolve(&pd)
			if err != nil {
				return nil, nil, err
			}
		}

		// Undoing a sequence of operations requires applying the inverse of
		// each operation in reverse order.
		inverse = append(inv, inverse...)
	}

	data, err := json.MarshalEscaped(pd, options.EscapeHTML)
	if err != nil {
		return nil, nil, err
	}

	return data, inverse, nil
}

// invertOperation computes the operations that undo a single forward
// operation. It must be called before the operation is applied, and the
// returned resolver (if any) must be called after it is applied.
func (p Patch) invertOperation(doc *container, op Operation, options *ApplyOptions) (Patch, inverseResolver, error) {
	switch op.Kind() {
	case "add":
		inv, err := invertAdd(doc, op, options)
		return inv, nil, err
	case "remove":
		inv, err := invertRemove(doc, op, options)
		return inv, nil, err
	case "replace":
		inv, err := invertReplace(doc, op, options)
		return inv, nil, err
	case "move":
		return invertMove(doc, op, options)
	case "copy":
		inv, err := invertCopy(doc, op, options)
		return inv, nil, err
	case "test":
		// A test does not mutate the document; re-testing the same value
		// keeps the inverse patch honest about the state it expects.
		return Patch{copyOperation(op)}, nil, nil
	default:
		return nil, nil, fmt.Errorf("Unexpected kind: %s", op.Kind())
	}
}

// invertAdd computes the inverse of an "add" operation. Adding to an object
// key that already exists overwrites it, so the inverse must restore the old
// value with "replace" rather than blindly removing the key.
func invertAdd(doc *container, op Operation, options *ApplyOptions) (Patch, error) {
	path, err := op.Path()
	if err != nil {
		return nil, fmt.Errorf("add operation failed to decode path: %w", ErrMissing)
	}

	// Adding to the root replaces the whole document, so the inverse restores
	// the old document.
	if path == "" {
		old, err := rawContainer(doc, options)
		if err != nil {
			return nil, err
		}
		return Patch{makeOperation("replace", "", old)}, nil
	}

	return invertSetTarget(doc, path, 0, options)
}

// invertCopy computes the inverse of a "copy" operation, which inserts or
// overwrites at its destination exactly like "add".
func invertCopy(doc *container, op Operation, options *ApplyOptions) (Patch, error) {
	path, err := op.Path()
	if err != nil {
		return nil, fmt.Errorf("copy operation failed to decode path: %w", ErrMissing)
	}

	return invertSetTarget(doc, path, 0, options)
}

// invertSetTarget computes the inverse of inserting or overwriting the value
// at path. dashDelta resolves a trailing "-" array index relative to the
// current array length: 0 when the element is about to be appended, -1 when
// it has just been appended.
func invertSetTarget(doc *container, path string, dashDelta int, options *ApplyOptions) (Patch, error) {
	con, key := findObject(doc, path, options)

	if con == nil {
		return nil, fmt.Errorf("operation does not apply: doc is missing path: \"%s\": %w", path, ErrMissing)
	}

	switch c := con.(type) {
	case *partialDoc:
		if old, ok := c.obj[key]; ok {
			raw, err := rawValue(old, options)
			if err != nil {
				return nil, err
			}
			return Patch{makeOperation("replace", path, raw)}, nil
		}
		return Patch{makeOperation("remove", path, nil)}, nil
	case *partialArray:
		resolved := path
		if key == "-" {
			resolved = strings.TrimSuffix(path, "-") + strconv.Itoa(len(c.nodes)+dashDelta)
		}
		return Patch{makeOperation("remove", resolved, nil)}, nil
	default:
		return nil, fmt.Errorf("operation does not apply: doc is missing path: \"%s\": %w", path, ErrMissing)
	}
}

// invertRemove computes the inverse of a "remove" operation by capturing the
// value about to be removed so it can be added back.
func invertRemove(doc *container, op Operation, options *ApplyOptions) (Patch, error) {
	path, err := op.Path()
	if err != nil {
		return nil, fmt.Errorf("remove operation failed to decode path: %w", ErrMissing)
	}

	con, key := findObject(doc, path, options)

	if con == nil {
		if options.AllowMissingPathOnRemove {
			return nil, nil
		}
		return nil, fmt.Errorf("remove operation does not apply: doc is missing path: \"%s\": %w", path, ErrMissing)
	}

	val, err := con.get(key, options)
	if err != nil {
		if options.AllowMissingPathOnRemove {
			return nil, nil
		}
		return nil, fmt.Errorf("remove operation does not apply: doc is missing key: %s: %w", path, ErrMissing)
	}

	raw, err := rawValue(val, options)
	if err != nil {
		return nil, err
	}

	return Patch{makeOperation("add", path, raw)}, nil
}

// invertReplace computes the inverse of a "replace" operation by capturing
// the value about to be replaced.
func invertReplace(doc *container, op Operation, options *ApplyOptions) (Patch, error) {
	path, err := op.Path()
	if err != nil {
		return nil, fmt.Errorf("replace operation failed to decode path: %w", err)
	}

	if path == "" {
		old, err := rawContainer(doc, options)
		if err != nil {
			return nil, err
		}
		return Patch{makeOperation("replace", "", old)}, nil
	}

	con, key := findObject(doc, path, options)

	if con == nil {
		return nil, fmt.Errorf("replace operation does not apply: doc is missing path: %s: %w", path, ErrMissing)
	}

	val, err := con.get(key, options)
	if err != nil {
		return nil, fmt.Errorf("replace operation does not apply: doc is missing key: %s: %w", path, ErrMissing)
	}

	raw, err := rawValue(val, options)
	if err != nil {
		return nil, err
	}

	return Patch{makeOperation("replace", path, raw)}, nil
}

// invertMove computes the inverse of a "move" operation. A move is undone by
// removing (or restoring, if it overwrote an existing key) the destination
// and adding the captured value back at the source. Simply swapping "from"
// and "path" is not sufficient: moving within a single array shifts indices,
// and the destination may have held a value that would be lost.
func invertMove(doc *container, op Operation, options *ApplyOptions) (Patch, inverseResolver, error) {
	from, err := op.From()
	if err != nil {
		return nil, nil, fmt.Errorf("move operation failed to decode from: %w", err)
	}

	path, err := op.Path()
	if err != nil {
		return nil, nil, fmt.Errorf("move operation failed to decode path: %w", err)
	}

	con, key := findObject(doc, from, options)

	if con == nil {
		return nil, nil, fmt.Errorf("move operation does not apply: doc is missing from path: %s: %w", from, ErrMissing)
	}

	val, err := con.get(key, options)
	if err != nil {
		return nil, nil, fmt.Errorf("error in move for path: '%s': %w", from, err)
	}

	raw, err := rawValue(val, options)
	if err != nil {
		return nil, nil, err
	}

	// If the destination is an existing object key, the move overwrites it;
	// capture the old value so it can be restored.
	var destInv Patch
	if path != "" {
		if destCon, destKey := findObject(doc, path, options); destCon != nil {
			if destDoc, ok := destCon.(*partialDoc); ok {
				if old, exists := destDoc.obj[destKey]; exists {
					oldRaw, err := rawValue(old, options)
					if err != nil {
						return nil, nil, err
					}
					destInv = Patch{makeOperation("replace", path, oldRaw)}
				}
			}
		}
	}

	resolve := func(doc *container) (Patch, error) {
		inv := destInv
		if inv == nil {
			// The destination did not exist before, so undoing the move
			// removes it. A trailing "-" can only be resolved against the
			// post-move array.
			resolved, err := resolveTrailingDash(doc, path, options)
			if err != nil {
				return nil, err
			}
			inv = Patch{makeOperation("remove", resolved, nil)}
		}
		return append(inv, makeOperation("add", from, raw)), nil
	}

	return nil, resolve, nil
}

// resolveTrailingDash rewrites a trailing "-" array index in path to the
// index of the last element of the array it refers to.
func resolveTrailingDash(doc *container, path string, options *ApplyOptions) (string, error) {
	if !strings.HasSuffix(path, "/-") {
		return path, nil
	}

	con, _ := findObject(doc, path, options)

	if con == nil {
		return "", fmt.Errorf("operation does not apply: doc is missing path: \"%s\": %w", path, ErrMissing)
	}

	ary, ok := con.(*partialArray)
	if !ok {
		// The parent is an object, so "-" is a literal key, not the
		// end-of-array marker.
		return path, nil
	}

	return strings.TrimSuffix(path, "-") + strconv.Itoa(len(ary.nodes)-1), nil
}

// rawContainer serializes a container back to raw JSON.
func rawContainer(c *container, options *ApplyOptions) (*json.RawMessage, error) {
	buf, err := json.MarshalEscaped(*c, options.EscapeHTML)
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(buf)
	return &raw, nil
}

// rawValue serializes a node back to raw JSON.
func rawValue(n *lazyNode, options *ApplyOptions) (*json.RawMessage, error) {
	copied, _, err := deepCopy(n, options)
	if err != nil {
		return nil, err
	}
	return copied.raw, nil
}

// makeOperation builds a single patch operation. The value may be nil for
// operations that do not carry one.
func makeOperation(kind, path string, value *json.RawMessage) Operation {
	op := Operation{}

	setString := func(key, val string) {
		raw := json.RawMessage(strconv.Quote(val))
		op[key] = &raw
	}

	setString("op", kind)
	setString("path", path)

	if value != nil {
		op["value"] = value
	}

	return op
}

// copyOperation returns a deep copy of a single patch operation.
func copyOperation(op Operation) Operation {
	copied := make(Operation, len(op))
	for key, val := range op {
		if val == nil {
			copied[key] = nil
			continue
		}
		raw := make(json.RawMessage, len(*val))
		copy(raw, *val)
		copied[key] = &raw
	}
	return copied
}
