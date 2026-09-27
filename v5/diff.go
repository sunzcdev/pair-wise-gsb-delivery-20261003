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
	// ErrMissingIdentity is returned (or triggers a positional fallback) when
	// an element of an array configured with WithArrayIdentity does not
	// contain the configured identity key.
	ErrMissingIdentity = errors.New("missing identity key")
	// ErrDuplicateIdentity is returned (or triggers a positional fallback)
	// when two elements of an identity-keyed array share the same identity
	// value.
	ErrDuplicateIdentity = errors.New("duplicate identity key")
	// ErrInvalidIdentity is returned (or triggers a positional fallback) when
	// an element of an identity-keyed array is not an object, or its identity
	// value is neither a string nor a number.
	ErrInvalidIdentity = errors.New("invalid identity key")
)

// IdentityMismatchMode controls how CreatePatch reacts when the elements of
// an identity-keyed array have missing, duplicate or invalid identity values.
type IdentityMismatchMode int

const (
	// IdentityMismatchError makes CreatePatch fail with a descriptive error
	// wrapping ErrMissingIdentity, ErrDuplicateIdentity or
	// ErrInvalidIdentity. This is the default.
	IdentityMismatchError IdentityMismatchMode = iota
	// IdentityMismatchPositional deterministically falls back to a plain
	// index-by-index diff for the offending array only.
	IdentityMismatchPositional
)

type diffOptions struct {
	arrayIdentity map[string]string
	mismatchMode  IdentityMismatchMode
}

// DiffOption configures the behavior of CreatePatch.
type DiffOption func(*diffOptions)

// WithArrayIdentity declares that the array at the given RFC 6901 JSON
// Pointer path ("" addresses the root document) holds elements with a stable
// identity stored in the object field key. CreatePatch matches elements of
// such arrays by identity instead of by index, emitting "move" operations
// for reordered elements and precise add/remove operations for elements that
// appear or disappear, rather than replacing the whole array.
func WithArrayIdentity(path, key string) DiffOption {
	return func(o *diffOptions) {
		o.arrayIdentity[path] = key
	}
}

// WithIdentityMismatchMode sets how identity problems (missing, duplicate or
// invalid identity values) are handled. The default is
// IdentityMismatchError.
func WithIdentityMismatchMode(mode IdentityMismatchMode) DiffOption {
	return func(o *diffOptions) {
		o.mismatchMode = mode
	}
}

// CreatePatch calculates an RFC 6902 JSON Patch that transforms the original
// document into the modified document. Applying the returned patch to
// original yields a document deeply equal to modified.
//
// Objects are diffed key by key. Arrays are diffed index by index, unless
// their path was registered with WithArrayIdentity, in which case elements
// are matched by their identity key and reorderings are expressed with
// "move" operations.
//
// The generated operations are deterministic: object keys are visited in
// sorted order and identity mismatches never cause arbitrary pairings.
func CreatePatch(original, modified []byte, opts ...DiffOption) (Patch, error) {
	cfg := diffOptions{arrayIdentity: map[string]string{}}
	for _, opt := range opts {
		opt(&cfg)
	}

	oldDoc, err := decodeDiffDoc(original)
	if err != nil {
		return nil, fmt.Errorf("invalid original document: %w", err)
	}
	newDoc, err := decodeDiffDoc(modified)
	if err != nil {
		return nil, fmt.Errorf("invalid modified document: %w", err)
	}

	d := &differ{opts: cfg, ops: Patch{}}
	if err := d.diffValue("", oldDoc, newDoc); err != nil {
		return nil, err
	}
	return d.ops, nil
}

func decodeDiffDoc(data []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

type differ struct {
	opts diffOptions
	ops  Patch
}

func (d *differ) diffValue(path string, oldV, newV interface{}) error {
	oldObj, oldIsObj := oldV.(map[string]interface{})
	newObj, newIsObj := newV.(map[string]interface{})
	if oldIsObj && newIsObj {
		return d.diffObject(path, oldObj, newObj)
	}

	oldArr, oldIsArr := oldV.([]interface{})
	newArr, newIsArr := newV.([]interface{})
	if oldIsArr && newIsArr {
		return d.diffArray(path, oldArr, newArr)
	}

	if !diffEqual(oldV, newV) {
		d.appendOp("replace", path, "", newV)
	}
	return nil
}

func (d *differ) diffObject(path string, oldObj, newObj map[string]interface{}) error {
	removed := []string{}
	common := []string{}
	added := []string{}
	for k := range oldObj {
		if _, ok := newObj[k]; ok {
			common = append(common, k)
		} else {
			removed = append(removed, k)
		}
	}
	for k := range newObj {
		if _, ok := oldObj[k]; !ok {
			added = append(added, k)
		}
	}
	sort.Strings(removed)
	sort.Strings(common)
	sort.Strings(added)

	for _, k := range removed {
		d.appendOp("remove", joinDiffPath(path, k), "", nil)
	}
	for _, k := range common {
		if err := d.diffValue(joinDiffPath(path, k), oldObj[k], newObj[k]); err != nil {
			return err
		}
	}
	for _, k := range added {
		d.appendOp("add", joinDiffPath(path, k), "", newObj[k])
	}
	return nil
}

func (d *differ) diffArray(path string, oldArr, newArr []interface{}) error {
	if key, ok := d.opts.arrayIdentity[path]; ok {
		return d.diffKeyedArray(path, key, oldArr, newArr)
	}
	return d.diffPositionalArray(path, oldArr, newArr)
}

func (d *differ) diffPositionalArray(path string, oldArr, newArr []interface{}) error {
	common := len(oldArr)
	if len(newArr) < common {
		common = len(newArr)
	}
	for i := 0; i < common; i++ {
		if err := d.diffValue(joinDiffPath(path, strconv.Itoa(i)), oldArr[i], newArr[i]); err != nil {
			return err
		}
	}
	// Remove trailing elements in descending order so earlier removals do
	// not shift the indexes of later ones.
	for i := len(oldArr) - 1; i >= common; i-- {
		d.appendOp("remove", joinDiffPath(path, strconv.Itoa(i)), "", nil)
	}
	for i := common; i < len(newArr); i++ {
		d.appendOp("add", joinDiffPath(path, strconv.Itoa(i)), "", newArr[i])
	}
	return nil
}

func (d *differ) diffKeyedArray(path, key string, oldArr, newArr []interface{}) error {
	oldIDs, err := identityList(oldArr, key)
	if err != nil {
		return d.identityFallback(path, oldArr, newArr, err)
	}
	newIDs, err := identityList(newArr, key)
	if err != nil {
		return d.identityFallback(path, oldArr, newArr, err)
	}

	oldElemByID := make(map[string]interface{}, len(oldArr))
	for i, id := range oldIDs {
		oldElemByID[id] = oldArr[i]
	}
	newSet := make(map[string]bool, len(newIDs))
	for _, id := range newIDs {
		newSet[id] = true
	}

	// Removals first, in descending index order, so indexes stay valid.
	for i := len(oldIDs) - 1; i >= 0; i-- {
		if !newSet[oldIDs[i]] {
			d.appendOp("remove", joinDiffPath(path, strconv.Itoa(i)), "", nil)
		}
	}

	// working holds the identities of the surviving elements in their
	// current order; it is kept in sync with every emitted operation.
	working := make([]string, 0, len(newIDs))
	for _, id := range oldIDs {
		if newSet[id] {
			working = append(working, id)
		}
	}

	// Reconcile working with the target order left to right. Positions
	// before i are already final, so a surviving element that belongs at
	// position i is always found at some j > i.
	for i := 0; i < len(newIDs); i++ {
		id := newIDs[i]
		if i < len(working) && working[i] == id {
			continue
		}
		if _, survived := oldElemByID[id]; !survived {
			d.appendOp("add", joinDiffPath(path, strconv.Itoa(i)), "", newArr[i])
			working = insertID(working, i, id)
			continue
		}
		j := i + 1
		for ; j < len(working); j++ {
			if working[j] == id {
				break
			}
		}
		d.appendOp("move", joinDiffPath(path, strconv.Itoa(i)), joinDiffPath(path, strconv.Itoa(j)), nil)
		working = insertID(removeID(working, j), i, id)
	}

	// Content updates last: at this point every surviving element sits at
	// its final (new) index, so update paths are stable.
	newIndexByID := make(map[string]int, len(newIDs))
	for i, id := range newIDs {
		newIndexByID[id] = i
	}
	for i, id := range oldIDs {
		newIndex, ok := newIndexByID[id]
		if !ok {
			continue
		}
		if err := d.diffValue(joinDiffPath(path, strconv.Itoa(newIndex)), oldArr[i], newArr[newIndex]); err != nil {
			return err
		}
	}
	return nil
}

func (d *differ) identityFallback(path string, oldArr, newArr []interface{}, err error) error {
	if d.opts.mismatchMode == IdentityMismatchPositional {
		return d.diffPositionalArray(path, oldArr, newArr)
	}
	return fmt.Errorf("array at %q: %w", path, err)
}

func identityList(arr []interface{}, key string) ([]string, error) {
	ids := make([]string, len(arr))
	seen := make(map[string]int, len(arr))
	for i, elem := range arr {
		id, err := identityFor(elem, key)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		if prev, dup := seen[id]; dup {
			return nil, fmt.Errorf("element %d: %w: %q (already seen at element %d)", i, ErrDuplicateIdentity, id, prev)
		}
		seen[id] = i
		ids[i] = id
	}
	return ids, nil
}

func identityFor(elem interface{}, key string) (string, error) {
	obj, ok := elem.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("%w: element is %T, expected object", ErrInvalidIdentity, elem)
	}
	v, ok := obj[key]
	if !ok {
		return "", fmt.Errorf("%w: key %q", ErrMissingIdentity, key)
	}
	switch id := v.(type) {
	case string:
		return id, nil
	case json.Number:
		return id.String(), nil
	default:
		return "", fmt.Errorf("%w: key %q holds %T, expected string or number", ErrInvalidIdentity, key, v)
	}
}

func insertID(ids []string, i int, id string) []string {
	ids = append(ids, "")
	copy(ids[i+1:], ids[i:])
	ids[i] = id
	return ids
}

func removeID(ids []string, i int) []string {
	return append(ids[:i], ids[i+1:]...)
}

func (d *differ) appendOp(op, path, from string, value interface{}) {
	o := Operation{
		"op":   diffRawMessage(op),
		"path": diffRawMessage(path),
	}
	if from != "" {
		o["from"] = diffRawMessage(from)
	}
	if op == "add" || op == "replace" {
		o["value"] = diffRawMessage(value)
	}
	d.ops = append(d.ops, o)
}

func diffRawMessage(v interface{}) *json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Only reachable for values that were successfully decoded before.
		panic(fmt.Sprintf("jsonpatch: unable to marshal diff operation value: %v", err))
	}
	r := json.RawMessage(b)
	return &r
}

func joinDiffPath(base, segment string) string {
	segment = strings.ReplaceAll(segment, "~", "~0")
	segment = strings.ReplaceAll(segment, "/", "~1")
	return base + "/" + segment
}

func diffEqual(a, b interface{}) bool {
	switch av := a.(type) {
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			w, ok := bv[k]
			if !ok || !diffEqual(v, w) {
				return false
			}
		}
		return true
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !diffEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && av == bv
	default:
		return a == b
	}
}
