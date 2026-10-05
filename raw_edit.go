package message

import (
	"errors"
	"fmt"
)

// ByteRange is a half-open interval [Start, End) of byte offsets inside the
// original message.
type ByteRange struct {
	Start int
	End   int
}

// Len returns the length in bytes of the range.
func (r ByteRange) Len() int {
	return r.End - r.Start
}

// Contains reports whether [start, end) is fully contained in r.
func (r ByteRange) Contains(start, end int) bool {
	return start >= r.Start && end <= r.End && start <= end
}

// Intersects reports whether r and [start, end) share at least one byte.
func (r ByteRange) Intersects(start, end int) bool {
	return start < r.End && end > r.Start
}

// LocateError is returned when the input message or an edit cannot be located
// on the raw byte stream. In that case a byte-exact local edit cannot be
// performed safely.
type LocateError struct {
	Reason string
}

func (err *LocateError) Error() string {
	return "message: cannot locate raw byte range: " + err.Reason
}

// IsLocateError reports whether err is a LocateError.
func IsLocateError(err error) bool {
	var locErr *LocateError
	return errors.As(err, &locErr)
}

// Protection describes a byte region whose integrity is cryptographically
// protected by a security multipart such as multipart/signed (RFC 1847).
type Protection struct {
	// MediaType is the media type of the protected container, e.g.
	// "multipart/signed".
	MediaType string
	// Path is the multipart path of the protected container, using the same
	// indexing scheme as WalkFunc.
	Path []int
	// Range covers the protected subtree bytes verbatim.
	Range ByteRange
}

// ProtectedSubtreeError is returned when an edit would modify bytes inside a
// cryptographically protected subtree (e.g. multipart/signed) without an
// explicit acknowledgement that the existing signature becomes invalid.
type ProtectedSubtreeError struct {
	// Protection describes the protected subtree that the edit intersects.
	Protection Protection
}

func (err *ProtectedSubtreeError) Error() string {
	return fmt.Sprintf("message: edit intersects protected subtree %s at path %v; the existing signature will be invalid, pass WithInvalidateSignatures to proceed",
		err.Protection.MediaType, err.Protection.Path)
}

// IsProtectedSubtree reports whether err is a ProtectedSubtreeError.
func IsProtectedSubtree(err error) bool {
	var protectedErr *ProtectedSubtreeError
	return errors.As(err, &protectedErr)
}

// RawEntity is an entity located inside a RawMessage, together with the exact
// byte offsets of its header block, body and whole content inside the original
// message.
//
// All ranges are relative to the original, unedited message. After an edit,
// offsets must be re-computed using ParseRawMessage on the result.
type RawEntity struct {
	// Path is the multipart path of the entity, using the same indexing scheme
	// as WalkFunc. The root entity has a nil path.
	Path []int
	// Header is the parsed header of the entity.
	Header Header
	// MediaType is the parsed Content-Type media type.
	MediaType string
	// MediaParams are the parsed Content-Type parameters.
	MediaParams map[string]string

	// HeaderRange covers the raw header fields and the terminating blank line.
	HeaderRange ByteRange
	// BodyRange covers the raw, transfer-encoded body. For a part in a
	// multipart container this includes the CRLF immediately preceding the
	// next boundary delimiter, matching both the wire format (RFC 2046) and
	// the bytes a part writer is responsible for emitting.
	BodyRange ByteRange
	// WholeRange covers the entity content (header and body). It does not
	// include the surrounding multipart boundary delimiter lines.
	WholeRange ByteRange

	// Children are the parts of a multipart entity, in order.
	Children []*RawEntity

	raw         []byte
	protectedBy []*Protection
}

// Multipart reports whether the entity is a multipart entity.
func (e *RawEntity) Multipart() bool {
	return e.MediaParams["boundary"] != ""
}

// IsLeaf reports whether the entity is not a multipart entity.
func (e *RawEntity) IsLeaf() bool {
	return !e.Multipart()
}

// Raw returns the raw bytes of the whole entity.
//
// The returned slice must not be modified.
func (e *RawEntity) Raw() []byte {
	return e.raw[e.WholeRange.Start:e.WholeRange.End]
}

// RawHeader returns the raw bytes of the entity's header block, including the
// terminating blank line. Header folding, capitalization and whitespace are
// preserved exactly.
//
// The returned slice must not be modified.
func (e *RawEntity) RawHeader() []byte {
	return e.raw[e.HeaderRange.Start:e.HeaderRange.End]
}

// RawBody returns the raw, transfer-encoded bytes of the entity's body.
//
// The returned slice must not be modified.
func (e *RawEntity) RawBody() []byte {
	return e.raw[e.BodyRange.Start:e.BodyRange.End]
}

// ProtectedBy returns the protections (e.g. enclosing multipart/signed
// subtrees) that cover this entity's bytes.
func (e *RawEntity) ProtectedBy() []Protection {
	out := make([]Protection, len(e.protectedBy))
	for i, p := range e.protectedBy {
		out[i] = *p
	}
	return out
}

// IsProtected reports whether the entity's bytes are part of a
// cryptographically protected subtree.
func (e *RawEntity) IsProtected() bool {
	return len(e.protectedBy) > 0
}

// RawMessage is a complete, parseable MIME message stored as a single byte
// slice, with each entity mapped to its exact raw byte offsets.
//
// A RawMessage is required for byte-exact local edits: the API never
// re-serializes the message, it splices replacement bytes into the original
// buffer. All bytes outside edited ranges are copied verbatim, which preserves
// header folding, field ordering, multipart boundaries, transfer encodings and
// any unparsed content.
type RawMessage struct {
	// Root is the root entity (the message itself).
	Root *RawEntity
	// Entities lists all entities in depth-first order, including Root.
	Entities []*RawEntity
	// Protections lists the cryptographically protected subtrees found in the
	// message.
	Protections []Protection

	raw []byte
}
