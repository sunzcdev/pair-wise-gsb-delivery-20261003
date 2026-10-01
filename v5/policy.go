package jsonpatch

import (
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/evanphx/json-patch/v5/internal/json"
)

// Permission identifies the kind of document access a patch operation
// attempted when it was denied by an AccessPolicy.
type Permission string

const (
	// PermissionRead indicates that an operation attempted to read a
	// document path the policy does not allow reading.
	PermissionRead Permission = "read"
	// PermissionWrite indicates that an operation attempted to modify a
	// document path the policy does not allow writing.
	PermissionWrite Permission = "write"
)

// PathPolicy reports whether the document node at the given JSON pointer
// path (RFC 6901) may be accessed. The root document itself is addressed
// by the empty path "".
type PathPolicy func(path string) bool

// AccessPolicy describes which document paths a patch is allowed to read
// and to write when applied with ApplyWithPolicy or ApplyIndentWithPolicy.
//
// A nil PathPolicy (or a nil *AccessPolicy) allows every path, so a policy
// that only restricts writes can leave Read nil and vice versa.
type AccessPolicy struct {
	// Read reports whether a path may be read. "move" and "copy" operations
	// read their "from" subtree, and "test" operations read the value they
	// compare against (which would otherwise leak whether a protected value
	// matches), so those paths and all of their children must be readable
	// for the operation to be allowed.
	Read PathPolicy
	// Write reports whether a path may be written. Every path whose value
	// differs between the document before and after an operation must be
	// writable. This includes protected children that are replaced or
	// removed when a whole parent object is written, and array elements
	// whose position shifts after an insertion or removal.
	Write PathPolicy
}

func (p *AccessPolicy) allowsRead(path string) bool {
	return p == nil || p.Read == nil || p.Read(path)
}

func (p *AccessPolicy) allowsWrite(path string) bool {
	return p == nil || p.Write == nil || p.Write(path)
}

// ApplyWithPolicy mutates a JSON document according to the patch, checking
// every operation against policy. An operation is only applied if the
// policy allows every read and write it performs; otherwise a
// *PolicyViolationError is returned and the document is left unmodified by
// the offending operation. A nil policy allows everything, matching Apply.
func (p Patch) ApplyWithPolicy(doc []byte, policy *AccessPolicy) ([]byte, error) {
	return p.ApplyIndentWithPolicy(doc, "", NewApplyOptions(), policy)
}

// ApplyIndentWithPolicy mutates a JSON document according to the patch and
// the passed in ApplyOptions, checking every operation against policy, and
// returns the new document indented.
func (p Patch) ApplyIndentWithPolicy(doc []byte, indent string, options *ApplyOptions, policy *AccessPolicy) ([]byte, error) {
	return p.applyIndent(doc, indent, options, policy)
}

// checkReadAccess verifies that the operation does not read any document
// path the policy forbids. doc is a snapshot of the document before the
// operation is applied.
func checkReadAccess(doc interface{}, op Operation, policy *AccessPolicy, opIndex int) error {
	var path string

	switch op.Kind() {
	case "move", "copy":
		from, err := op.From()
		if err != nil {
			// The operation itself will report the missing "from".
			return nil
		}
		path = from
	case "test":
		target, err := op.Path()
		if err != nil {
			return nil
		}
		path = target
	default:
		return nil
	}

	sub, ok := valueAtPath(doc, path)
	if !ok {
		// Nothing to read; the operation will fail on its own.
		return nil
	}

	denied := ""
	found := false
	walkPaths(sub, path, func(p string) bool {
		if !policy.allowsRead(p) {
			denied = p
			found = true
			return false
		}
		return true
	})

	if found {
		return &PolicyViolationError{OpIndex: opIndex, Permission: PermissionRead, Path: denied}
	}

	return nil
}

// checkWriteAccess verifies that every path that changed between the
// document snapshots taken before and after the operation is writable.
func checkWriteAccess(before, after interface{}, policy *AccessPolicy, opIndex int) error {
	changed := map[string]struct{}{}
	collectChangedPaths(before, after, "", changed)

	paths := make([]string, 0, len(changed))
	for path := range changed {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		if !policy.allowsWrite(path) {
			return &PolicyViolationError{OpIndex: opIndex, Permission: PermissionWrite, Path: path}
		}
	}

	return nil
}

// snapshotContainer renders the document and decodes it into a generic
// value so it can be navigated and diffed by the policy checks.
func snapshotContainer(pd *container, options *ApplyOptions) (interface{}, error) {
	data, err := json.MarshalEscaped(pd, options.EscapeHTML)
	if err != nil {
		return nil, err
	}

	var v interface{}
	if err := unmarshal(data, &v); err != nil {
		return nil, err
	}

	return v, nil
}

var rfc6901Encoder = strings.NewReplacer("~", "~0", "/", "~1")

func joinPath(base, key string) string {
	return base + "/" + rfc6901Encoder.Replace(key)
}

// valueAtPath navigates a generic JSON value along an RFC 6901 pointer.
func valueAtPath(doc interface{}, path string) (interface{}, bool) {
	if path == "" {
		return doc, true
	}

	if !strings.HasPrefix(path, "/") {
		return nil, false
	}

	cur := doc
	for _, part := range strings.Split(path, "/")[1:] {
		key := decodePatchKey(part)

		switch c := cur.(type) {
		case map[string]interface{}:
			v, ok := c[key]
			if !ok {
				return nil, false
			}
			cur = v
		case []interface{}:
			idx, err := strconv.Atoi(key)
			if err != nil {
				return nil, false
			}
			if idx < 0 {
				idx += len(c)
			}
			if idx < 0 || idx >= len(c) {
				return nil, false
			}
			cur = c[idx]
		default:
			return nil, false
		}
	}

	return cur, true
}

// walkPaths calls visit for base and every path below it, stopping early
// when visit returns false.
func walkPaths(v interface{}, base string, visit func(path string) bool) bool {
	if !visit(base) {
		return false
	}

	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if !walkPaths(child, joinPath(base, k), visit) {
				return false
			}
		}
	case []interface{}:
		for i, child := range t {
			if !walkPaths(child, joinPath(base, strconv.Itoa(i)), visit) {
				return false
			}
		}
	}

	return true
}

// collectChangedPaths records every path whose value differs between two
// generic JSON values. Subtrees that appear or disappear wholesale have all
// of their paths recorded.
func collectChangedPaths(before, after interface{}, base string, changed map[string]struct{}) {
	switch b := before.(type) {
	case map[string]interface{}:
		a, ok := after.(map[string]interface{})
		if !ok {
			markSubtree(before, base, changed)
			markSubtree(after, base, changed)
			return
		}

		for k, bv := range b {
			child := joinPath(base, k)
			av, exists := a[k]
			if !exists {
				markSubtree(bv, child, changed)
				continue
			}
			collectChangedPaths(bv, av, child, changed)
		}
		for k, av := range a {
			if _, exists := b[k]; !exists {
				markSubtree(av, joinPath(base, k), changed)
			}
		}
	case []interface{}:
		a, ok := after.([]interface{})
		if !ok {
			markSubtree(before, base, changed)
			markSubtree(after, base, changed)
			return
		}

		n := len(b)
		if len(a) < n {
			n = len(a)
		}
		for i := 0; i < n; i++ {
			collectChangedPaths(b[i], a[i], joinPath(base, strconv.Itoa(i)), changed)
		}
		for i := n; i < len(b); i++ {
			markSubtree(b[i], joinPath(base, strconv.Itoa(i)), changed)
		}
		for i := n; i < len(a); i++ {
			markSubtree(a[i], joinPath(base, strconv.Itoa(i)), changed)
		}
	default:
		if !reflect.DeepEqual(before, after) {
			changed[base] = struct{}{}
		}
	}
}

func markSubtree(v interface{}, base string, changed map[string]struct{}) {
	changed[base] = struct{}{}

	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			markSubtree(child, joinPath(base, k), changed)
		}
	case []interface{}:
		for i, child := range t {
			markSubtree(child, joinPath(base, strconv.Itoa(i)), changed)
		}
	}
}
