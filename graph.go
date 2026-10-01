package validator

import "reflect"

// Synthetic tags reported on the FieldError produced by the reference-graph
// traversal guards. They are not user-registerable tags and never run through
// the normal validation plan.
const (
	// cycleTag is reported when an object that is already present on the
	// current validation path is reached again, i.e. a real reference cycle.
	cycleTag = "cycle"

	// maxDepthTag is reported when the nested-struct depth of a single path
	// exceeds the configured limit.
	maxDepthTag = "maxdepth"

	// maxNodesTag is reported when the total amount of work done during one
	// validation call exceeds the configured limit.
	maxNodesTag = "maxnodes"
)

// CyclePolicy determines how the validator reacts when a value is reached
// that is already present on the current traversal path, i.e. when the
// validated object graph contains a reference cycle.
type CyclePolicy uint8

const (
	// RejectCycles reports a ValidationError carrying the path at which the
	// cycle was reached and stops traversing the cyclic edge. It is the
	// default and prevents unbounded recursion on cyclic input while still
	// validating everything reachable without going around the cycle.
	RejectCycles CyclePolicy = iota

	// TruncateCycles silently cuts traversal at a cyclic edge. The cycle is
	// not reported as an error and no values reachable only through the
	// back-edge are validated. This is useful when cyclic structures are
	// expected and cycle edges are intentionally not validated.
	TruncateCycles
)

// edgeIdentity returns a stable, comparable identity for the reference-typed
// edge through which a value is reached.
//
// It must be evaluated before the value is dereferenced: the executor unwraps
// pointers and interfaces prior to descending into a struct, so by the time a
// struct is visited its pointer identity has already been lost.
//
// A non-zero identity is returned for pointers, interfaces wrapping a pointer
// and slice/map headers: the only values that can be referenced from more
// than one place and so the only edges through which a reference cycle can
// exist. Struct values reached by value intentionally return 0, including
// addressable fields embedded in their parent; they are not independently
// referenceable storage, and tracking their address yields false positives
// for inlined fields.
//
// Identity is only meaningful within a single validation call. It must not be
// shared between calls or field paths; cycle detection keeps a per-path stack
// so that diamond references (the same object reached via different paths
// under potentially different constraints) are validated on every path.
func edgeIdentity(current reflect.Value) uintptr {
	switch current.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Map:
		return current.Pointer()

	case reflect.Interface:
		if current.IsNil() {
			return 0
		}
		// unwrap exactly the interface; an interface holding a pointer shares
		// the pointee's identity, an interface holding a struct value holds a
		// private copy with no reference identity.
		return edgeIdentity(current.Elem())
	}
	return 0
}

// pathContainsIdentity reports whether id already appears on the per-path
// ancestor stack. The path is typically short, so a linear scan keeps the
// common, acyclic case allocation-free.
func (v *validate) pathContainsIdentity(id uintptr) bool {
	for i := len(v.path) - 1; i >= 0; i-- {
		if v.path[i] == id {
			return true
		}
	}
	return false
}

// resetGraphState clears the per-call traversal state of a pooled validate
// instance. It must be invoked by every public entry point before traversal
// begins so that limits and the path stack never leak between calls.
func (v *validate) resetGraphState() {
	v.path = v.path[:0]
	v.depth = 0
	v.visited = 0
}

// exitStruct unwinds the per-path traversal state after one struct has been
// visited. pushed reports whether this visit pushed its identity onto the
// ancestor stack.
func (v *validate) exitStruct(pushed bool) {
	v.depth--
	if pushed {
		v.path = v.path[:len(v.path)-1]
	}
}

// appendGraphError records a FieldError emitted by a graph traversal guard
// (cycle rejection, depth or work limit). The reported namespace is the full
// dotted path at which the guard triggered, and the field name is its final
// segment, so the error keeps the path by which the object was reached.
func (v *validate) appendGraphError(tag string, ns []byte, structNs []byte, kind reflect.Kind, typ reflect.Type) {
	altPath := trimTrailingDot(ns)
	structPath := trimTrailingDot(structNs)

	altLen := uint8(len(lastPathSegment(altPath)))
	structLen := uint8(len(lastPathSegment(structPath)))

	v.errs = append(v.errs,
		&fieldError{
			v:              v.v,
			tag:            tag,
			actualTag:      tag,
			ns:             string(altPath),
			structNs:       string(structPath),
			fieldLen:       altLen,
			structfieldLen: structLen,
			kind:           kind,
			typ:            typ,
		},
	)
}

func trimTrailingDot(ns []byte) []byte {
	if len(ns) > 0 && ns[len(ns)-1] == '.' {
		return ns[:len(ns)-1]
	}
	return ns
}

func lastPathSegment(ns []byte) []byte {
	for i := len(ns) - 1; i >= 0; i-- {
		if ns[i] == '.' {
			return ns[i+1:]
		}
	}
	return ns
}
