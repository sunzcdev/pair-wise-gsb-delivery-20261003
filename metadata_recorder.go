package toml

import (
	"reflect"

	"github.com/pelletier/go-toml/v2/unstable"
)

// metaRecorder collects the locations of the values actually assigned while
// decoding. It is nil unless Decoder.WithMetadata was used, so that the hot
// decode paths remain a single nil check away from the metadata-free
// behavior.
//
// The recorder walks the document together with the decoder: goSeg/tomlSeg
// hold the path to the value currently being assigned. The slices are
// scratch stacks restored to their previous length after every recursive
// descent, so they never need to be copied per field.
type metaRecorder struct {
	meta *unstable.Metadata

	goSeg   []unstable.MetaSegment
	tomlSeg []string
}

func newMetaRecorder(m *unstable.Metadata) *metaRecorder {
	if m == nil {
		return nil
	}
	return &metaRecorder{meta: m}
}

func (r *metaRecorder) reset() {
	if r == nil {
		return
	}
	r.goSeg = r.goSeg[:0]
	r.tomlSeg = r.tomlSeg[:0]
	r.meta.Reset()
}

// save returns the current stack depths, for use with restore.
func (r *metaRecorder) save() (int, int) {
	return len(r.goSeg), len(r.tomlSeg)
}

func (r *metaRecorder) restore(g, t int) {
	r.goSeg = r.goSeg[:g]
	r.tomlSeg = r.tomlSeg[:t]
}

func (r *metaRecorder) pushName(goName, tomlName string) {
	r.goSeg = append(r.goSeg, unstable.MetaSegment{Name: goName, Index: -1})
	r.tomlSeg = append(r.tomlSeg, tomlName)
}

func (r *metaRecorder) pushIndex(i int) {
	r.goSeg = append(r.goSeg, unstable.MetaSegment{Index: i})
}

// pushToml adds a TOML name without a Go segment (e.g. a header part that
// traverses an embedded struct).
func (r *metaRecorder) pushToml(name string) {
	r.tomlSeg = append(r.tomlSeg, name)
}

// consumeMetaLastName pushes the deferred array-table last-part map key, if
// any.
func (d *decoder) consumeMetaLastName() {
	if d.metaLastName != "" {
		d.meta.pushName(d.metaLastName, d.metaLastName)
		d.metaLastName = ""
	}
}

// recordCaptureMeta emits the location of a custom-Unmarshaler destination:
// every source span accumulated into the capture becomes one (possibly
// discontiguous) location.
func (d *decoder) recordCaptureMeta(c *rawCapture) {
	if d.meta == nil || len(c.ranges) == 0 {
		return
	}
	g := make([]unstable.MetaSegment, len(c.goSeg))
	copy(g, c.goSeg)
	t := make([]string, len(c.tomlSeg))
	copy(t, c.tomlSeg)
	var extra []unstable.Range
	if len(c.ranges) > 1 {
		extra = append(extra, c.ranges[1:]...)
	}
	d.meta.meta.Add(unstable.Location{
		Field: g,
		Key:   t,
		Range: c.ranges[0],
		Extra: extra,
	})
}

// appendFieldChain pushes the Go field names of a (possibly embedded) struct
// field index path, in traversal order.
func (d *decoder) appendFieldChain(t reflect.Type, index []int) {
	for _, i := range index {
		d.meta.goSeg = append(d.meta.goSeg, unstable.MetaSegment{Name: t.Field(i).Name, Index: -1})
		t = t.Field(i).Type
		for t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
	}
}

// metaValueRange returns the byte span of the value of a key-value,
// including the whole inline table or array (whose nodes only carry the span
// of their opening bracket). Scalars use the node's own range.
func (d *decoder) metaValueRange(expr *unstable.Node, value *unstable.Node) unstable.Range {
	if value.Kind == unstable.Array || value.Kind == unstable.InlineTable {
		if rng, ok := d.metaContainerRange(expr, value); ok {
			return rng
		}
	}
	raw := d.rawValue(expr, value)
	return d.p.Range(raw)
}

// metaContainerRange returns the span of an inline table or array value.
// When the value belongs to a key-value expression, its end is the end of
// the expression; when it is nested in another container, the closing
// bracket is located by scanning from the opening one. The boolean is false
// if the span cannot be determined.
func (d *decoder) metaContainerRange(expr, value *unstable.Node) (unstable.Range, bool) {
	if expr != nil && expr.Kind == unstable.KeyValue {
		raw := d.rawValue(expr, value)
		return d.p.Range(raw), true
	}
	return d.scanContainerRange(value)
}

// scanContainerRange finds the span of a nested array/inline table by
// matching its closing bracket, honoring TOML strings.
func (d *decoder) scanContainerRange(value *unstable.Node) (unstable.Range, bool) {
	doc := d.p.Data()
	start := d.nodeStart(value)
	if start >= len(doc) {
		return unstable.Range{}, false
	}
	open := doc[start]
	close := byte(']')
	if open == '{' {
		close = '}'
	}
	// Track both bracket kinds independently: the target closes when its
	// own depth returns to zero, regardless of nested opposite brackets.
	sq, cu := 0, 0
	for j := start; j < len(doc); j++ {
		switch doc[j] {
		case '"', '\'':
			end, ok := skipTomlString(doc, j)
			if !ok {
				return unstable.Range{}, false
			}
			j = end - 1
		case '[':
			sq++
		case '{':
			cu++
		case ']', '}':
			if doc[j] == ']' {
				sq--
			} else {
				cu--
			}
			if doc[j] == close && ((close == ']' && sq == 0) || (close == '}' && cu == 0)) {
				return d.p.Range(doc[start : j+1]), true
			}
		}
	}
	return unstable.Range{}, false
}

// nodeStart returns the offset of the opening bracket of an array or inline
// table node. Array nodes do not carry their own range, so the position of
// the first child (minus leading whitespace) is used.
func (d *decoder) nodeStart(n *unstable.Node) int {
	if n.Raw.Length > 0 {
		return int(n.Raw.Offset)
	}
	doc := d.p.Data()
	child := n.Child()
	for child != nil {
		if child.Kind != unstable.Comment {
			off := int(child.Raw.Offset)
			for off > 0 && (doc[off-1] == ' ' || doc[off-1] == '\t' ||
				doc[off-1] == '\n' || doc[off-1] == '\r') {
				off--
			}
			if off > 0 && (doc[off-1] == '[' || doc[off-1] == '{') {
				return off - 1
			}
			return int(child.Raw.Offset)
		}
		child = child.Next()
	}
	return -1
}

// skipTomlString returns the index just past the TOML string literal
// starting at i, or false if it is unterminated.
func skipTomlString(doc []byte, i int) (int, bool) {
	q := doc[i]
	if i+2 < len(doc) && doc[i+1] == q && doc[i+2] == q {
		for j := i + 3; j+2 < len(doc); j++ {
			if q == '"' && doc[j] == '\\' {
				j++
				continue
			}
			if doc[j] == q && doc[j+1] == q && doc[j+2] == q {
				return j + 3, true
			}
		}
		return 0, false
	}
	for j := i + 1; j < len(doc); j++ {
		if q == '"' && doc[j] == '\\' {
			j++
			continue
		}
		if doc[j] == q {
			return j + 1, true
		}
	}
	return 0, false
}

// assignArrayElem assigns one element of an array value, recording its
// location with the element index on the current path.
func (d *decoder) assignArrayElem(elem reflect.Value, n *unstable.Node, i int) (reflect.Value, error) {
	if d.meta == nil {
		return d.assignValue(elem, nil, n)
	}
	elemGo, elemToml := d.meta.save()
	d.meta.pushIndex(i)
	container := n.Kind == unstable.Array || n.Kind == unstable.InlineTable
	if !container {
		// Record scalars once with their index on the path. This also covers
		// elements handed to a custom Unmarshaler (generic recording is
		// suppressed via metaSkip below).
		d.meta.record(n.Raw)
	}
	skip := d.metaSkip
	// The element is recorded here with its index on the path; suppress the
	// generic recording inside assignValue to avoid duplicates.
	d.metaSkip = true
	nv, err := d.assignValue(elem, nil, n)
	d.metaSkip = skip
	d.lastElemGo = elemGo
	d.lastElemToml = elemToml
	d.lastElemContainer = container
	d.lastElemNode = n
	return nv, err
}

// recordLastArrayElem records the location of the container element last
// processed by assignArrayElem (a no-op for scalar elements). It must be
// called right after assignArrayElem, before the stack is popped for the
// next element.
func (d *decoder) recordLastArrayElem() {
	if d.meta == nil || !d.lastElemContainer {
		return
	}
	// Record the container at the depth of its index (a Go segment with no
	// TOML name), independently of the inner-key segments still on the
	// stack from nested descents.
	g := d.meta.goSeg[:d.lastElemGo+1]
	t := d.meta.tomlSeg[:d.lastElemToml]
	if rng, ok := d.scanContainerRange(d.lastElemNode); ok {
		d.meta.recordAt(g, t, rng)
	} else {
		d.meta.recordAt(g, t, d.lastElemNode.Raw)
	}
}

// metaRecord records the location of the value currently being assigned,
// unless assignValue was entered from assignArrayElem (which records the
// element with its index) or the value is a container (whose contents record
// themselves). unmarshaler is true when the value is consumed by a custom
// Unmarshaler: containers count as a single recorded location in that case.
func (d *decoder) metaRecord(expr, value *unstable.Node, unmarshaler bool) {
	if d.meta == nil || d.metaSkip {
		return
	}
	if !unmarshaler && (value.Kind == unstable.Array || value.Kind == unstable.InlineTable) {
		return
	}
	d.meta.record(d.metaValueRange(expr, value))
}

// prefixes returns copies of the current stacks, to be replayed when the
// cached target of a table is later populated.
func (r *metaRecorder) prefixes() ([]unstable.MetaSegment, []string) {
	g := make([]unstable.MetaSegment, len(r.goSeg))
	copy(g, r.goSeg)
	t := make([]string, len(r.tomlSeg))
	copy(t, r.tomlSeg)
	return g, t
}

// setPrefixes replaces the stacks with the given prefix (used when starting a
// key-value under a cached table target).
func (r *metaRecorder) setPrefixes(g []unstable.MetaSegment, t []string) {
	r.goSeg = append(r.goSeg[:0], g...)
	r.tomlSeg = append(r.tomlSeg[:0], t...)
}

// record stores a location for the current stacks.
func (r *metaRecorder) record(rng unstable.Range, extra ...unstable.Range) {
	r.recordAt(r.goSeg, r.tomlSeg, rng, extra...)
}

// recordAt stores a location for explicit paths, without mutating or
// depending on the current stacks.
func (r *metaRecorder) recordAt(g0 []unstable.MetaSegment, t0 []string, rng unstable.Range, extra ...unstable.Range) {
	g := make([]unstable.MetaSegment, len(g0))
	copy(g, g0)
	t := make([]string, len(t0))
	copy(t, t0)
	r.meta.Add(unstable.Location{
		Field: g,
		Key:   t,
		Range: rng,
		Extra: extra,
	})
}

// mapKeyName renders the Go-side name of a map entry. For string keys this is
// the TOML key itself; other key kinds are rendered in their TOML spelling so
// that struct and map targets decoded from the same document agree on the
// named path for common keys.
func mapKeyName(k reflect.Value, tomlName string) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return tomlName
}
