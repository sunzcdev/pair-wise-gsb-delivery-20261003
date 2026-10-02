// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package bizerror

import "errors"

// Bounds enforced by the wire protocol. They apply per envelope (i.e. per
// business error tree) and exist so that a peer cannot force unbounded
// allocation or recursion while decoding.
const (
	// MaxNodes bounds the number of error nodes in one error tree.
	MaxNodes = 64
	// MaxFields bounds the number of exported fields on one error node.
	MaxFields = 32
	// MaxFieldBytes bounds the encoded size of one field value.
	MaxFieldBytes = 64 * 1024
	// MaxCauses bounds the number of direct causes of one error node.
	MaxCauses = 8
	// MaxDepth bounds the unfolded depth while walking a chain.
	MaxDepth = 128
)

var (
	// ErrUnknownType marks an [Error] whose TypeID was not registered on
	// the receiving side. Match it with errors.Is; inspect the original
	// identifier via Error.TypeID.
	ErrUnknownType = errors.New("bizerror: unknown business error type")

	// ErrLimitExceeded replaces a node/cause whose wire representation
	// exceeded the protocol bounds (too many nodes, fields or causes).
	ErrLimitExceeded = errors.New("bizerror: error representation exceeded protocol limits")

	// ErrCyclicReference marks where a reference cycle in the sender's
	// error graph was broken. The receiver always gets an acyclic graph.
	ErrCyclicReference = errors.New("bizerror: cyclic error reference")

	// ErrBadReference replaces a cause that pointed at a node missing from
	// the envelope.
	ErrBadReference = errors.New("bizerror: error cause referenced a missing node")
)

// Error is the generic, registry-independent representation of a business
// error after it crosses the boundary.
//
// When the TypeID is known locally, the registered decode hook produces the
// concrete local type and Error values do not normally surface. When the
// TypeID is unknown, the node arrives verbatim as *Error carrying
// [ErrUnknownType], so the host still has a structured result instead of a
// flattened string.
type Error struct {
	// TypeID is the stable identifier agreed by both sides.
	TypeID string

	// Message is err.Error() captured on the sender, always safe to show.
	Message string

	// Fields contains only fields the sending registry explicitly marked
	// as exported. Values decode as the usual encoding/json types
	// (string, float64, bool, nil, []any, map[string]any).
	Fields map[string]any

	// Known reports whether TypeID was registered locally.
	Known bool

	causes []error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Unwrap exposes the causes so errors.Is / errors.As traverse the tree,
// and marks unknown types without nesting an extra Error value.
func (e *Error) Unwrap() []error {
	out := append([]error(nil), e.causes...)
	if !e.Known {
		out = append(out, ErrUnknownType)
	}
	return out
}

// Is defines equality by stable TypeID: two generic representations of the
// same agreed type compare equal even if only one side knows the concrete
// Go type. Targets carrying [ErrUnknownType] only match unknown errors.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	if e.TypeID == "" || t.TypeID == "" {
		return false
	}
	return e.TypeID == t.TypeID
}

// Field returns the exported value of name and whether it was present.
func (e *Error) Field(name string) (any, bool) {
	v, ok := e.Fields[name]
	return v, ok
}

// CauseCount reports how many direct causes the node was reconstructed with.
func (e *Error) CauseCount() int { return len(e.causes) }

// placeholder is a terminal sentinel-bearing error used while decoding.
type placeholder struct{ cause error }

func (p *placeholder) Error() string { return p.cause.Error() }
func (p *placeholder) Unwrap() error { return p.cause }
