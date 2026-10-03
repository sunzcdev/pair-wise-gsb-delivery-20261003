package ber

import (
	"errors"
	"io"
	"math"
	"slices"
)

// WriteTo writes the BER encoding of the packet and, for a constructed
// packet, its children to w. Unlike Bytes, it does not materialize the
// encoded form of any subtree: lengths are computed in a first pass, then
// headers and leaf content are written sequentially, so encoding a deep tree
// costs O(tree size) time and O(depth) stack instead of the
// O(depth x subtree) copying of Bytes.
//
// WriteTo returns the number of bytes written. If the underlying writer
// fails or reports a short write, WriteTo stops immediately (it does not
// continue with sibling packets) and returns the bytes written so far
// together with the error; a short write without an error is reported as
// io.ErrShortWrite.
//
// The tree is validated before anything is written: a nil packet, a nil
// child, a nil Data buffer on a leaf, a cycle, or an encoded length that
// overflows int is reported as an error. A subtree shared between several
// parents is legal and is encoded once per occurrence.
func (p *Packet) WriteTo(w io.Writer) (int64, error) {
	lengths, err := p.encodedLengths()
	if err != nil {
		return 0, err
	}

	cw := &countingWriter{w: w}
	p.writeBER(cw, lengths)
	return cw.n, cw.err
}

// AppendTo appends the BER encoding of the packet to dst and returns the
// extended slice. It is the append-style counterpart of WriteTo: the output
// buffer is grown at most once, up front, so no subtree is copied per level
// of nesting. On error dst is returned unmodified; see WriteTo for the
// validation performed.
func (p *Packet) AppendTo(dst []byte) ([]byte, error) {
	lengths, err := p.encodedLengths()
	if err != nil {
		return dst, err
	}

	total := len(encodeIdentifier(p.Identifier)) + len(encodeLength(lengths[p])) + lengths[p]
	dst = slices.Grow(dst, total)
	return p.appendBER(dst, lengths), nil
}

// encodedLengths computes the BER content length of every node in the tree
// rooted at p, keyed by packet pointer. It validates the tree as it walks:
// nil nodes, nil leaf Data buffers, cycles, and length overflow are errors.
// A shared subtree is measured once (its length is context-independent) but
// is still encoded once per occurrence by the callers.
func (p *Packet) encodedLengths() (map[*Packet]int, error) {
	lengths := make(map[*Packet]int)
	if _, err := p.computeContentLength(make(map[*Packet]struct{}), lengths); err != nil {
		return nil, err
	}
	return lengths, nil
}

// computeContentLength returns the content length of p, recording it (and
// every descendant's) in lengths. visiting holds the nodes on the current
// path from the root and detects cycles; a node reachable along several
// paths is not itself a cycle and is resolved from lengths after its first
// computation.
func (p *Packet) computeContentLength(visiting map[*Packet]struct{}, lengths map[*Packet]int) (int, error) {
	if p == nil {
		return 0, errors.New("cannot encode nil packet")
	}
	if _, ok := visiting[p]; ok {
		return 0, errors.New("cannot encode packet tree containing a cycle")
	}
	if content, ok := lengths[p]; ok {
		return content, nil
	}

	var content int
	if len(p.Children) == 0 {
		if p.Data == nil {
			return 0, errors.New("cannot encode packet with nil Data")
		}
		content = p.Data.Len()
	} else {
		visiting[p] = struct{}{}
		defer delete(visiting, p)

		for _, child := range p.Children {
			childContent, err := child.computeContentLength(visiting, lengths)
			if err != nil {
				return 0, err
			}

			header := len(encodeIdentifier(child.Identifier)) + len(encodeLength(childContent))
			if childContent > math.MaxInt-header {
				return 0, errors.New("encoded length overflow")
			}
			if content > math.MaxInt-header-childContent {
				return 0, errors.New("encoded length overflow")
			}
			content += header + childContent
		}
	}

	lengths[p] = content
	return content, nil
}

// countingWriter counts the bytes accepted by the underlying writer and
// latches the first error (including a short write, converted to
// io.ErrShortWrite), refusing to write anything further afterwards.
type countingWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	if cw.err != nil {
		return 0, cw.err
	}
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	cw.err = err
	return n, err
}

// writeBER writes p's identifier, length, and content to w, recursing into
// children for constructed packets. lengths must come from encodedLengths.
func (p *Packet) writeBER(w *countingWriter, lengths map[*Packet]int) {
	if w.err != nil {
		return
	}

	_, _ = w.Write(encodeIdentifier(p.Identifier))
	_, _ = w.Write(encodeLength(lengths[p]))

	if len(p.Children) == 0 {
		_, _ = w.Write(p.Data.Bytes())
		return
	}

	for _, child := range p.Children {
		child.writeBER(w, lengths)
		if w.err != nil {
			return
		}
	}
}

// appendBER appends p's encoding to dst, recursing into children for
// constructed packets. lengths must come from encodedLengths; callers are
// expected to have grown dst to the full encoded size so the appends do not
// reallocate.
func (p *Packet) appendBER(dst []byte, lengths map[*Packet]int) []byte {
	dst = append(dst, encodeIdentifier(p.Identifier)...)
	dst = append(dst, encodeLength(lengths[p])...)

	if len(p.Children) == 0 {
		return append(dst, p.Data.Bytes()...)
	}

	for _, child := range p.Children {
		dst = child.appendBER(dst, lengths)
	}
	return dst
}
