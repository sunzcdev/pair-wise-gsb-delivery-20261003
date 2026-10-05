package unstable

import "errors"

// Snapshot is an independent, owning copy of a single expression parsed by a
// Parser.
//
// A Parser reuses its internal node arena on every call to NextExpression and
// Reset, so nodes obtained from Parser.Expression are only valid until the
// parser moves on. Snapshot captures the current expression — its nodes, the
// bytes of the input document those nodes refer to, and any decoded node
// values — into storage owned by the Snapshot itself. The result stays valid
// after the originating Parser is reset and reused for another document, and
// after the original input bytes are gone.
//
// A Snapshot only retains the slice of the input document that covers the
// captured expression, not the whole document. Two snapshots never share
// mutable buffers: each one owns its copies outright.
//
// Nodes reachable from a Snapshot keep their original Raw ranges: offsets are
// expressed relative to the beginning of the original document given to
// Parser.Reset, exactly as they were while parsing. Use Raw and Range to
// convert between those document-relative ranges and the snapshot's own
// bytes, exposed by Data.
type Snapshot struct {
	// parser backs the snapshot's nodes so that Node.Child, Node.Next and
	// Iterator keep working unchanged. Its data holds the snapshot's copy of
	// the input bytes. It is not usable for parsing.
	parser *Parser

	// base is the offset of parser.data[0] in the original document.
	base uint32
}

// Snapshot captures the last successfully parsed expression (the one returned
// by Expression) into an independent Snapshot. It returns nil if no
// expression has been parsed yet.
//
// The Snapshot owns copies of everything the expression refers to. It does
// not keep the parser's input document alive beyond the bytes of the captured
// expression, and it is not affected by subsequent NextExpression or Reset
// calls on the parser.
func (p *Parser) Snapshot() *Snapshot {
	if len(p.nodes) == 0 {
		return nil
	}

	// Find the span of the input document the expression refers to through
	// its nodes' Raw ranges.
	lo := len(p.data)
	hi := 0
	extend := func(offset, length int) {
		if length == 0 {
			return
		}
		if offset < lo {
			lo = offset
		}
		if offset+length > hi {
			hi = offset + length
		}
	}
	for i := range p.nodes {
		n := &p.nodes[i]
		extend(int(n.Raw.Offset), int(n.Raw.Length))
	}

	data := make([]byte, hi-lo)
	copy(data, p.data[lo:hi])

	s := &Snapshot{
		parser: &Parser{data: data},
		base:   uint32(lo), //nolint:gosec // TOML documents are small
	}

	// Copy the whole arena: node handles (next/child) are indexes into it,
	// so copying it verbatim keeps the expression's structure intact.
	s.parser.nodes = make([]Node, len(p.nodes))
	for i := range p.nodes {
		n := p.nodes[i]
		if len(n.Data) > 0 {
			// Data either references the input document or a buffer
			// allocated during parsing (e.g. an unescaped string). Copy
			// it unconditionally so that the snapshot never keeps the
			// input alive and two snapshots never share a writable
			// buffer.
			c := make([]byte, len(n.Data))
			copy(c, n.Data)
			n.Data = c
		} else {
			// Empty slices may still point into the input's backing
			// array and keep it alive.
			n.Data = nil
		}
		n.parser = s.parser
		s.parser.nodes[i] = n
	}

	return s
}

// Expression returns the root node of the captured expression, mirroring
// Parser.Expression. Its children — including the elements of arrays and
// inline tables — are fully traversable with the usual Node and Iterator
// methods. It returns nil on a zero Snapshot.
func (s *Snapshot) Expression() *Node {
	if s.parser == nil || len(s.parser.nodes) == 0 {
		return nil
	}
	return &s.parser.nodes[0]
}

// Data returns the bytes of the original document that the captured
// expression refers to. The snapshot owns this slice; it is not shared with
// the parser's input nor with any other snapshot.
//
// Note that node Raw ranges are not offsets into Data: they remain relative
// to the original document. Use Raw to resolve a range against Data.
func (s *Snapshot) Data() []byte {
	return s.parser.data
}

// Raw returns the bytes covered by r. The range must be expressed in
// original-document offsets — the same coordinates used by Node.Raw — and
// must fall within the expression's span, otherwise Raw panics.
//
// A zero-length range resolves to an empty slice: some node kinds (such as
// Array or Table) do not record a Raw range at all.
func (s *Snapshot) Raw(r Range) []byte {
	if r.Length == 0 {
		return nil
	}
	start := int64(r.Offset) - int64(s.base)
	if start < 0 || start+int64(r.Length) > int64(len(s.parser.data)) {
		panic(errors.New("range is outside the snapshot's data"))
	}
	return s.parser.data[int(start) : int(start)+int(r.Length)]
}

// Range returns the range, in original-document offsets, of a subslice of
// Data. It panics if b is not a subslice of Data.
func (s *Snapshot) Range(b []byte) Range {
	r := s.parser.Range(b)
	r.Offset += s.base
	return r
}
