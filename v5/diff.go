package jsonpatch

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/evanphx/json-patch/v5/internal/json"
)

var (
	// ErrDuplicateIdentity is returned (or triggers a fallback, depending on
	// DiffOptions.OnIdentityMismatch) when two elements of an array configured
	// with an identity key share the same identity value.
	ErrDuplicateIdentity = errors.New("duplicate identity value")
	// ErrMissingIdentity is returned (or triggers a fallback, depending on
	// DiffOptions.OnIdentityMismatch) when an element of an array configured
	// with an identity key does not carry a usable identity value.
	ErrMissingIdentity = errors.New("missing identity value")
)

// IdentityMismatchPolicy determines how CreatePatchWithOptions reacts when an
// array configured with an identity key contains duplicate or missing
// identity values.
type IdentityMismatchPolicy int

const (
	// IdentityMismatchError makes the diff fail with an error wrapping
	// ErrDuplicateIdentity or ErrMissingIdentity. This is the default.
	IdentityMismatchError IdentityMismatchPolicy = iota
	// IdentityMismatchFallback makes the diff deterministically fall back to
	// plain index-based comparison for the offending array. Elements are
	// never paired up at random.
	IdentityMismatchFallback
)

// DiffOptions configures CreatePatchWithOptions.
type DiffOptions struct {
	// ArrayIdentityKeys maps a JSON Pointer (RFC 6901) addressing an array in
	// the document (e.g. "/servers" or "/spec/containers") to the name of the
	// object member that uniquely identifies each element of that array
	// (e.g. "name"). Arrays listed here are diffed by identity: reordered
	// elements produce "move" operations and content changes produce targeted
	// "replace"/"add"/"remove" operations, instead of replacing the array.
	// Identity values must be strings or numbers.
	ArrayIdentityKeys map[string]string
	// OnIdentityMismatch selects the behavior when identity values are
	// duplicated or missing. The zero value is IdentityMismatchError.
	OnIdentityMismatch IdentityMismatchPolicy
}

// CreatePatch returns an RFC 6902 patch capable of converting originalJSON
// into modifiedJSON. Arrays are compared element by index; use
// CreatePatchWithOptions to match array elements by an identity key instead.
//
// The generated operations are deterministic: object members are visited in
// sorted order and array elements in index order.
func CreatePatch(originalJSON, modifiedJSON []byte) (Patch, error) {
	return CreatePatchWithOptions(originalJSON, modifiedJSON, nil)
}

// CreatePatchWithOptions behaves like CreatePatch, but honors the supplied
// DiffOptions, in particular identity-keyed array diffs that emit "move"
// operations for reordered elements.
func CreatePatchWithOptions(originalJSON, modifiedJSON []byte, opts *DiffOptions) (Patch, error) {
	if opts == nil {
		opts = &DiffOptions{}
	}

	original, err := decodeDiffValue(originalJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid original document: %w", err)
	}

	modified, err := decodeDiffValue(modifiedJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid modified document: %w", err)
	}

	d := &differ{opts: opts}
	if err := d.diff("", original, modified); err != nil {
		return nil, err
	}

	return Patch(d.ops), nil
}

func decodeDiffValue(data []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}

	return v, nil
}

type differ struct {
	opts *DiffOptions
	ops  []Operation
}

func (d *differ) diff(path string, a, b interface{}) error {
	if matchesValue(a, b) {
		return nil
	}

	switch av := a.(type) {
	case map[string]interface{}:
		if bv, ok := b.(map[string]interface{}); ok {
			return d.diffDoc(path, av, bv)
		}
	case []interface{}:
		if bv, ok := b.([]interface{}); ok {
			return d.diffArray(path, av, bv)
		}
	}

	return d.emitValueOp("replace", path, b)
}

func (d *differ) diffDoc(path string, a, b map[string]interface{}) error {
	for _, k := range sortedKeys(a) {
		if _, ok := b[k]; !ok {
			if err := d.emitOp("remove", path+"/"+escapeDiffKey(k), "", nil); err != nil {
				return err
			}
		}
	}

	for _, k := range sortedKeys(b) {
		av, ok := a[k]
		child := path + "/" + escapeDiffKey(k)
		if !ok {
			if err := d.emitValueOp("add", child, b[k]); err != nil {
				return err
			}
			continue
		}
		if err := d.diff(child, av, b[k]); err != nil {
			return err
		}
	}

	return nil
}

func (d *differ) diffArray(path string, a, b []interface{}) error {
	if key, ok := d.opts.ArrayIdentityKeys[path]; ok {
		return d.diffKeyedArray(path, key, a, b)
	}

	return d.diffArrayByIndex(path, a, b)
}

// diffArrayByIndex compares two arrays positionally: common indices are
// diffed recursively, surplus original elements are removed (highest index
// first) and surplus modified elements are appended.
func (d *differ) diffArrayByIndex(path string, a, b []interface{}) error {
	common := len(a)
	if len(b) < common {
		common = len(b)
	}

	for i := 0; i < common; i++ {
		if err := d.diff(path+"/"+strconv.Itoa(i), a[i], b[i]); err != nil {
			return err
		}
	}

	for i := len(a) - 1; i >= common; i-- {
		if err := d.emitOp("remove", path+"/"+strconv.Itoa(i), "", nil); err != nil {
			return err
		}
	}

	for i := common; i < len(b); i++ {
		if err := d.emitValueOp("add", path+"/"+strconv.Itoa(i), b[i]); err != nil {
			return err
		}
	}

	return nil
}

// diffKeyedArray diffs two arrays whose elements are objects identified by
// the value of their key member. It emits removes for deleted elements, moves
// for reordered ones, adds for new ones, and recursive diffs for elements
// whose content changed.
func (d *differ) diffKeyedArray(path, key string, a, b []interface{}) error {
	aIDs, aErr := arrayIdentities(path, key, a)
	bIDs, bErr := arrayIdentities(path, key, b)

	if aErr != nil || bErr != nil {
		if d.opts.OnIdentityMismatch == IdentityMismatchFallback {
			return d.diffArrayByIndex(path, a, b)
		}
		if aErr != nil {
			return aErr
		}
		return bErr
	}

	aIndex := make(map[string]int, len(aIDs))
	for i, id := range aIDs {
		aIndex[id] = i
	}
	bIndex := make(map[string]int, len(bIDs))
	for j, id := range bIDs {
		bIndex[id] = j
	}

	// 1. Remove deleted elements, highest original index first so that
	// earlier indices stay valid.
	for i := len(aIDs) - 1; i >= 0; i-- {
		if _, ok := bIndex[aIDs[i]]; !ok {
			if err := d.emitOp("remove", path+"/"+strconv.Itoa(i), "", nil); err != nil {
				return err
			}
		}
	}

	// 2. Reorder the surviving elements into their target relative order.
	var current, target []string
	for _, id := range aIDs {
		if _, ok := bIndex[id]; ok {
			current = append(current, id)
		}
	}
	for _, id := range bIDs {
		if _, ok := aIndex[id]; ok {
			target = append(target, id)
		}
	}
	if err := d.emitMoves(path, current, target); err != nil {
		return err
	}

	// 3. Insert new elements at their final index, in ascending order.
	for j, id := range bIDs {
		if _, ok := aIndex[id]; !ok {
			if err := d.emitValueOp("add", path+"/"+strconv.Itoa(j), b[j]); err != nil {
				return err
			}
		}
	}

	// 4. Recursively diff surviving elements, which now sit at their target
	// index.
	for j, id := range bIDs {
		i, ok := aIndex[id]
		if !ok {
			continue
		}
		if err := d.diff(path+"/"+strconv.Itoa(j), a[i], b[j]); err != nil {
			return err
		}
	}

	return nil
}

// emitMoves emits the "move" operations that transform current into target.
// Both slices must contain the same set of ids. The simulation mirrors RFC
// 6902 move semantics (remove at "from", then insert at "path"), so the
// emitted indices stay consistent as the patch is applied in order.
func (d *differ) emitMoves(path string, current, target []string) error {
	pos := make(map[string]int, len(current))
	for i, id := range current {
		pos[id] = i
	}

	for i, id := range target {
		p := pos[id]
		if p == i {
			continue
		}

		if err := d.emitOp("move", path+"/"+strconv.Itoa(i), path+"/"+strconv.Itoa(p), nil); err != nil {
			return err
		}

		// Splice id out of position p and insert it at position i.
		if p < i {
			copy(current[p:i], current[p+1:i+1])
			current[i] = id
			for k := p; k <= i; k++ {
				pos[current[k]] = k
			}
		} else {
			copy(current[i+1:p+1], current[i:p])
			current[i] = id
			for k := i; k <= p; k++ {
				pos[current[k]] = k
			}
		}
	}

	return nil
}

// arrayIdentities extracts the identity value of every element of ary. Each
// element must be an object whose key member is a string or a number, and all
// identities must be unique; otherwise an error wrapping ErrMissingIdentity
// or ErrDuplicateIdentity is returned.
func arrayIdentities(path, key string, ary []interface{}) ([]string, error) {
	ids := make([]string, len(ary))
	seen := make(map[string]int, len(ary))

	for i, elem := range ary {
		obj, ok := elem.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s[%d]: element is not an object: %w", path, i, ErrMissingIdentity)
		}

		id, ok := identityString(obj[key])
		if !ok {
			return nil, fmt.Errorf("%s[%d]: no usable identity member %q: %w", path, i, key, ErrMissingIdentity)
		}

		if first, dup := seen[id]; dup {
			return nil, fmt.Errorf("%s[%d]: identity %q already used by element %d: %w", path, i, id, first, ErrDuplicateIdentity)
		}
		seen[id] = i
		ids[i] = id
	}

	return ids, nil
}

func identityString(v interface{}) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

func (d *differ) emitValueOp(kind, path string, value interface{}) error {
	return d.emitOp(kind, path, "", value)
}

func (d *differ) emitOp(kind, path, from string, value interface{}) error {
	op := Operation{}

	kindRaw, err := diffRawMessage(kind)
	if err != nil {
		return err
	}
	op["op"] = kindRaw

	pathRaw, err := diffRawMessage(path)
	if err != nil {
		return err
	}
	op["path"] = pathRaw

	if from != "" {
		fromRaw, err := diffRawMessage(from)
		if err != nil {
			return err
		}
		op["from"] = fromRaw
	}

	if value != nil || kind == "add" || kind == "replace" {
		valueRaw, err := diffRawMessage(value)
		if err != nil {
			return err
		}
		op["value"] = valueRaw
	}

	d.ops = append(d.ops, op)
	return nil
}

func diffRawMessage(v interface{}) (*json.RawMessage, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(buf)
	return &raw, nil
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var rfc6901Encoder = strings.NewReplacer("~", "~0", "/", "~1")

func escapeDiffKey(k string) string {
	return rfc6901Encoder.Replace(k)
}
