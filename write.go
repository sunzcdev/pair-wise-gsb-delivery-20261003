package ber

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// Encoding errors returned by [Packet.WriteTo] and [Packet.AppendTo].
var (
	// ErrNilPacket indicates the packet being encoded, or one of its
	// descendants, is a nil pointer.
	ErrNilPacket = errors.New("ber: cannot encode a nil packet")
	// ErrPacketCycle indicates the packet tree contains a cycle: the same
	// packet is reachable from itself through Children.
	ErrPacketCycle = errors.New("ber: packet tree contains a cycle")
	// ErrPacketTooLarge indicates the encoded packet exceeds the largest
	// length the package can represent in a definite length (2^63-1 bytes).
	ErrPacketTooLarge = errors.New("ber: encoded packet length overflow")
)

// Compile-time assertion that Packet implements io.WriterTo.
var _ io.WriterTo = (*Packet)(nil)

// WriteTo writes the BER encoding of the packet to w. Unlike [Packet.Bytes],
// which rebuilds and concatenates each subtree in turn, WriteTo first computes
// every node's length in one walk, then streams identifier, length, and child
// bytes straight to w. A packet whose subtree is shared (the same child
// reachable through several parents) is encoded once per occurrence; a cycle
// is rejected with [ErrPacketCycle].
//
// If w writes fewer bytes than requested, WriteTo stops immediately -- no
// sibling nodes are written -- and returns the number of bytes actually
// written together with the short-write error.
func (p *Packet) WriteTo(w io.Writer) (int64, error) {
	if p == nil {
		return 0, ErrNilPacket
	}

	enc := packetEncoder{
		sink:  &streamSink{w: w},
		plans: make(map[*Packet]packetPlan),
	}

	if err := enc.plan(p); err != nil {
		return enc.written, err
	}
	if err := enc.writePacket(p); err != nil {
		return enc.written, err
	}
	return enc.written, nil
}

// AppendTo appends the BER encoding of the packet to dst and returns the
// extended slice. It uses the same single length pass as [Packet.WriteTo] and
// grows dst at most once, so a deeply nested tree does not copy its leaf
// content once per depth.
//
// On error the returned slice is dst unchanged.
func (p *Packet) AppendTo(dst []byte) ([]byte, error) {
	if p == nil {
		return dst, ErrNilPacket
	}

	plans := make(map[*Packet]packetPlan)
	enc := packetEncoder{plans: plans}
	if err := enc.plan(p); err != nil {
		return dst, err
	}

	total := plans[p].total
	if uint64(len(dst))+uint64(total) > math.MaxInt64 {
		return dst, ErrPacketTooLarge
	}
	if uint64(cap(dst)-len(dst)) < uint64(total) {
		grown := make([]byte, len(dst), len(dst)+total)
		copy(grown, dst)
		dst = grown
	}

	start := len(dst)
	dst = dst[:start+total]

	enc.sink = &sliceSink{buf: dst, off: start}
	if err := enc.writePacket(p); err != nil {
		return dst[:start], err
	}
	return dst, nil
}

// packetPlan caches the lengths computed during the planning walk so the
// writing walk emits each node's definite length without recomputation.
type packetPlan struct {
	content int
	total   int
	active  bool
}

// packetEncoder drives both walks. plans maps packets reached by pointer to
// their computed sizes.
type packetEncoder struct {
	sink    sink
	written int64
	plans   map[*Packet]packetPlan
	header  [maxHeaderLength]byte
}

// plan computes the encoded size of p and every descendant, rejecting nil
// children, cycles, and lengths that cannot be represented in BER.
func (e *packetEncoder) plan(p *Packet) error {
	if p == nil {
		return ErrNilPacket
	}
	if plan, seen := e.plans[p]; seen {
		if plan.active {
			return ErrPacketCycle
		}
		return nil
	}

	if len(p.Children) == 0 {
		dataLen := 0
		if p.Data != nil {
			dataLen = p.Data.Len()
		}
		total, err := addPacketLengths(identifierLength(p.Identifier), lenOctets(dataLen), dataLen)
		if err != nil {
			return err
		}
		e.plans[p] = packetPlan{content: dataLen, total: total}
		return nil
	}

	e.plans[p] = packetPlan{active: true}

	content := 0
	for i, child := range p.Children {
		if child == nil {
			return fmt.Errorf("%w: child at index %d", ErrNilPacket, i)
		}
		if err := e.plan(child); err != nil {
			return err
		}
		sum, err := addInt(content, e.plans[child].total)
		if err != nil {
			return err
		}
		content = sum
	}

	total, err := addPacketLengths(identifierLength(p.Identifier), lenOctets(content), content)
	if err != nil {
		return err
	}
	e.plans[p] = packetPlan{content: content, total: total}
	return nil
}

// writePacket emits p's identifier and length octets followed by its content:
// either the primitive data octets or each child in order. Planning has
// validated the tree and fixed every length.
func (e *packetEncoder) writePacket(p *Packet) error {
	plan := e.plans[p]

	if err := e.emit(appendIdentifier(e.header[:0], p.Identifier)); err != nil {
		return err
	}
	if err := e.emit(appendLength(e.header[:0], plan.content)); err != nil {
		return err
	}

	if len(p.Children) == 0 {
		if p.Data != nil {
			if err := e.emit(p.Data.Bytes()); err != nil {
				return err
			}
		}
		return nil
	}

	for _, child := range p.Children {
		if err := e.writePacket(child); err != nil {
			return err
		}
	}
	return nil
}

// emit writes b to the sink in a single call, tracking bytes written and
// converting a short write into io.ErrShortWrite.
func (e *packetEncoder) emit(b []byte) error {
	n, err := e.sink.write(b)
	e.written += int64(n)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	return nil
}

// addInt returns a+b or ErrPacketTooLarge if it overflows int64.
func addInt(a, b int) (int, error) {
	sum := int64(a) + int64(b)
	if sum > math.MaxInt64 || sum < int64(a) || int64(int(sum)) != sum {
		return 0, ErrPacketTooLarge
	}
	return int(sum), nil
}

// addPacketLengths sums the identifier, length, and content octet counts.
func addPacketLengths(parts ...int) (int, error) {
	total := 0
	for _, part := range parts {
		sum, err := addInt(total, part)
		if err != nil {
			return 0, err
		}
		total = sum
	}
	return total, nil
}

// maxHeaderLength bounds identifier and length octets: a Tag is uint64 (up to
// 10 high-tag octets), and a definite long-form length spans at most 9 octets
// (one lead byte plus eight value bytes).
const maxHeaderLength = 1 + 10 + 9

// identifierLength reports len(encodeIdentifier(id)) without allocating.
func identifierLength(id Identifier) int {
	if id.Tag < HighTag {
		return 1
	}
	return 1 + highTagLength(id.Tag)
}

// highTagLength is the number of base-128 octets used for a high-tag value.
func highTagLength(tag Tag) int {
	n := 0
	for tag != 0 {
		n++
		tag >>= 7
	}
	return n
}

// lenOctets reports how many length octets encode contentLen, mirroring
// encodeLength: one short-form octet for 0..127, otherwise a lead byte plus
// the minimal big-endian value bytes.
func lenOctets(contentLen int) int {
	if contentLen <= 127 {
		return 1
	}
	n := 1
	for v := contentLen; v > 0; v >>= 8 {
		n++
	}
	return n
}

// appendIdentifier appends the BER identifier octets of id to dst.
func appendIdentifier(dst []byte, id Identifier) []byte {
	b := byte(id.ClassType) | byte(id.TagType)
	if id.Tag < HighTag {
		return append(dst, b|byte(id.Tag))
	}

	b |= byte(HighTag)
	dst = append(dst, b)

	tag := id.Tag
	var value [10]byte
	n := highTagLength(tag)
	for i := n - 1; i >= 0; i-- {
		o := byte(tag) & byte(HighTagValueBitmask)
		if i != n-1 {
			o |= byte(HighTagContinueBitmask)
		}
		value[i] = o
		tag >>= 7
	}
	return append(dst, value[:n]...)
}

// appendLength appends the BER definite-length octets of contentLen to dst.
func appendLength(dst []byte, contentLen int) []byte {
	if contentLen <= 127 {
		return append(dst, byte(contentLen))
	}

	var value [8]byte
	n := 0
	for v := contentLen; v > 0; v >>= 8 {
		value[7-n] = byte(v)
		n++
	}
	dst = append(dst, byte(LengthLongFormBitmask|n))
	return append(dst, value[8-n:]...)
}

// sink is the write target shared by streaming and in-slice encoding.
type sink interface {
	write(b []byte) (int, error)
}

// streamSink writes through to an io.Writer.
type streamSink struct {
	w io.Writer
}

func (s *streamSink) write(b []byte) (int, error) {
	return s.w.Write(b)
}

// sliceSink writes into a pre-sized buffer at off.
type sliceSink struct {
	buf []byte
	off int
}

func (s *sliceSink) write(b []byte) (int, error) {
	n := copy(s.buf[s.off:], b)
	s.off += n
	return n, nil
}
