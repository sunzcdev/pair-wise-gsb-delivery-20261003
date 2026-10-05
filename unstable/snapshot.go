package unstable

// Snapshot is a self-contained copy of a single expression parsed by a
// Parser. It owns every byte its nodes refer to, so it stays valid and
// readable after the Parser has been reset to a different document, and it
// does not keep the original input alive.
//
// All Range values carried by the snapshot's nodes are relative to the
// snapshot's own Data(), not to the document originally given to the
// Parser.
type Snapshot struct {
	arena nodeArena
	data  []byte
}

// Snapshot returns a detached copy of the last expression parsed by
// NextExpression (or of the value parsed by parseValue), including its
// attached comments and, for arrays and inline tables, all of their
// children. It returns nil if no expression has been parsed yet.
//
// The snapshot copies only the bytes its nodes refer to (keys, values,
// comments and their raw source ranges); it does not retain the rest of
// the input document. Two snapshots never share buffers, and neither
// aliases the parser's internal storage, so the parser can be Reset and
// reused immediately.
//
// The zero-copy iteration entry points (NextExpression, Expression) are
// unaffected: taking a snapshot is an explicit, opt-in copy.
func (p *Parser) Snapshot() *Snapshot {
	if len(p.nodes) == 0 {
		return nil
	}

	nodes := make([]Node, len(p.nodes))
	size := 0
	for i := range p.nodes {
		n := &p.nodes[i]
		size += int(n.Raw.Length) + len(n.Data)
		nodes[i] = Node{
			Kind:  n.Kind,
			Raw:   n.Raw,
			Data:  n.Data,
			next:  n.next,
			child: n.child,
		}
	}

	// data is allocated with its final capacity up front, so the appends
	// below never reallocate and slices taken into it stay valid.
	data := make([]byte, 0, size)
	for i := range nodes {
		n := &nodes[i]
		raw := p.data[n.Raw.Offset : n.Raw.Offset+n.Raw.Length]
		offset := len(data)
		data = append(data, raw...)
		n.Raw.Offset = uint32(offset) //nolint:gosec // bounded by the expression size
		if len(n.Data) > 0 {
			offset = len(data)
			data = append(data, n.Data...)
			n.Data = data[offset : offset+len(n.Data)]
		}
	}

	s := &Snapshot{data: data}
	s.arena.nodes = nodes
	for i := range nodes {
		nodes[i].arena = &s.arena
	}
	return s
}

// Expression returns the root node of the snapshotted expression, or nil if
// the snapshot is empty. Children of the root (array elements, inline table
// entries, keys, values, attached comments) are traversable with the usual
// Node methods.
func (s *Snapshot) Expression() *Node {
	if len(s.arena.nodes) == 0 {
		return nil
	}
	return &s.arena.nodes[0]
}

// Data returns the snapshot's own copy of the source bytes referenced by
// its nodes. All Range values in the snapshot's nodes are relative to this
// slice, not to the document originally given to the Parser.
func (s *Snapshot) Data() []byte {
	return s.data
}

// Raw returns the slice corresponding to the bytes in the given range,
// relative to Data().
func (s *Snapshot) Raw(raw Range) []byte {
	return s.data[raw.Offset : raw.Offset+raw.Length]
}

// Shape returns the shape of the given range, relative to Data().
func (s *Snapshot) Shape(r Range) Shape {
	return Shape{
		Start: position(s.data, int(r.Offset)),
		End:   position(s.data, int(r.Offset)+int(r.Length)),
	}
}
