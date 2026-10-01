package jsonpatch

import (
	"fmt"
	"strconv"
	"strings"
)

// Permission identifies the kind of access a patch operation requires for a
// document path.
type Permission string

const (
	// PermissionRead indicates that an operation reads the value at a path,
	// e.g. the source of a "copy" or "move" operation, or the value compared
	// by a "test" operation.
	PermissionRead Permission = "read"
	// PermissionWrite indicates that an operation adds, replaces or removes
	// the value at a path.
	PermissionWrite Permission = "write"
)

// PathPolicy reports whether access to the given RFC 6901 JSON pointer path
// is permitted. The path of the document root is "".
type PathPolicy func(path string) bool

// AccessPolicy describes which document paths a patch is allowed to read and
// to write. A nil PathPolicy permits every access of that kind.
//
// Policies are evaluated against the document as it exists at each step of the
// patch: writing to a path also requires write permission for every protected
// path currently below it, because replacing or removing a parent affects its
// children. Likewise, reading a path (via "copy", "move" or "test") requires
// read permission for the whole subtree, since the operation observes or
// copies every value beneath it.
type AccessPolicy struct {
	// Read reports whether the value at a path may be observed by the patch.
	Read PathPolicy
	// Write reports whether the value at a path may be modified or removed.
	Write PathPolicy
}

// PolicyViolationError is returned when a patch operation attempts to read or
// write a document path that the AccessPolicy does not permit. It identifies
// the operation and the kind of access that was denied, but never carries
// document values, so protected data cannot leak through error messages.
type PolicyViolationError struct {
	// OpIndex is the zero-based index of the offending operation in the patch.
	OpIndex int
	// Operation is the kind of the offending operation, e.g. "replace".
	Operation string
	// Permission is the kind of access that was denied.
	Permission Permission
	// Path is the document path whose access was denied.
	Path string
}

// Error implements the error interface.
func (e *PolicyViolationError) Error() string {
	return fmt.Sprintf("policy violation in operation %d (%s): %s access to path %q is not permitted",
		e.OpIndex, e.Operation, e.Permission, e.Path)
}

func (policy *AccessPolicy) allows(perm Permission, path string) bool {
	if policy == nil {
		return true
	}

	if perm == PermissionRead {
		return policy.Read == nil || policy.Read(path)
	}

	return policy.Write == nil || policy.Write(path)
}

var rfc6901Encoder = strings.NewReplacer("~", "~0", "/", "~1")

func encodePatchKey(k string) string {
	return rfc6901Encoder.Replace(k)
}

// forEachExistingPath calls visit for path and every path below it that
// currently exists in the document, stopping early when visit returns false.
func forEachExistingPath(node *lazyNode, path string, visit func(string) bool) bool {
	if node == nil {
		return true
	}

	if !visit(path) {
		return false
	}

	if node.which == eDoc || node.tryDoc() {
		for _, k := range node.doc.keys {
			if !forEachExistingPath(node.doc.obj[k], path+"/"+encodePatchKey(k), visit) {
				return false
			}
		}
		return true
	}

	if node.which == eAry || node.tryAry() {
		for i, child := range node.ary.nodes {
			if !forEachExistingPath(child, path+"/"+strconv.Itoa(i), visit) {
				return false
			}
		}
	}

	return true
}

// nodeAtPath resolves path against the current document, returning nil when
// the path does not (yet) exist.
func nodeAtPath(doc *container, path string, options *ApplyOptions) *lazyNode {
	if path == "" {
		switch c := (*doc).(type) {
		case *partialDoc:
			return &lazyNode{doc: c, which: eDoc}
		case *partialArray:
			return &lazyNode{ary: c, which: eAry}
		}
		return nil
	}

	con, key := findObject(doc, path, options)
	if con == nil {
		return nil
	}

	node, err := con.get(key, options)
	if err != nil {
		return nil
	}

	return node
}

// checkAccess verifies that perm access is allowed for path and for every
// existing path below it in the current document.
func checkAccess(doc *container, perm Permission, path string, op Operation, opIndex int, policy *AccessPolicy, options *ApplyOptions) error {
	if !policy.allows(perm, path) {
		return &PolicyViolationError{OpIndex: opIndex, Operation: op.Kind(), Permission: perm, Path: path}
	}

	node := nodeAtPath(doc, path, options)
	if node == nil {
		return nil
	}

	denied := ""
	forEachExistingPath(node, path, func(sub string) bool {
		if policy.allows(perm, sub) {
			return true
		}
		denied = sub
		return false
	})

	if denied != "" {
		return &PolicyViolationError{OpIndex: opIndex, Operation: op.Kind(), Permission: perm, Path: denied}
	}

	return nil
}

// checkPolicy verifies a single operation against the access policy, using
// the document state produced by all preceding operations. Operations whose
// path or from fields cannot be decoded are skipped here; applying them will
// report the decoding error.
func checkPolicy(doc *container, op Operation, opIndex int, policy *AccessPolicy, options *ApplyOptions) error {
	check := func(perm Permission, path string) error {
		return checkAccess(doc, perm, path, op, opIndex, policy, options)
	}

	switch op.Kind() {
	case "add", "replace", "remove":
		path, err := op.Path()
		if err != nil {
			return nil
		}
		return check(PermissionWrite, path)
	case "test":
		// A test observes the value it compares against, so it requires read
		// access; otherwise its result would leak whether a restricted value
		// matches.
		path, err := op.Path()
		if err != nil {
			return nil
		}
		return check(PermissionRead, path)
	case "move":
		// A move reads the source value and removes it, so it requires both
		// read and write access to the source, plus write access to the
		// destination.
		from, err := op.From()
		if err != nil {
			return nil
		}
		if err := check(PermissionRead, from); err != nil {
			return err
		}
		if err := check(PermissionWrite, from); err != nil {
			return err
		}
		path, err := op.Path()
		if err != nil {
			return nil
		}
		return check(PermissionWrite, path)
	case "copy":
		from, err := op.From()
		if err != nil {
			return nil
		}
		if err := check(PermissionRead, from); err != nil {
			return err
		}
		path, err := op.Path()
		if err != nil {
			return nil
		}
		return check(PermissionWrite, path)
	}

	return nil
}
