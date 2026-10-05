package unstable

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2/internal/assert"
)

func TestSnapshot_NoExpression(t *testing.T) {
	p := &Parser{}
	assert.Equal(t, true, p.Snapshot() == nil)
	p.Reset([]byte("\n\n"))
	assert.Equal(t, false, p.NextExpression())
	assert.Equal(t, true, p.Snapshot() == nil)

	var zero Snapshot
	assert.Equal(t, true, zero.Expression() == nil)
}

func TestSnapshot_SurvivesReset(t *testing.T) {
	doc := "first = 1\ntarget = \"hello\" # trailing\nthird = 3\n"
	p := &Parser{}
	p.Reset([]byte(doc))
	assert.Equal(t, true, p.NextExpression())
	assert.Equal(t, true, p.NextExpression())

	expr := p.Expression()
	assert.Equal(t, KeyValue, expr.Kind)
	key := expr.Key()
	assert.Equal(t, true, key.Next())
	assert.Equal(t, "target", string(key.Node().Data))
	rawBefore := expr.Value().Raw
	rawBytesBefore := string(p.Raw(rawBefore))

	snap := p.Snapshot()

	// Reuse the parser for a completely different document.
	p.Reset([]byte("other = [1, 2, 3]"))
	assert.Equal(t, true, p.NextExpression())

	// The snapshot still reads the original key, value and raw range.
	root := snap.Expression()
	assert.Equal(t, KeyValue, root.Kind)
	k := root.Key()
	assert.Equal(t, true, k.Next())
	assert.Equal(t, "target", string(k.Node().Data))
	assert.Equal(t, "hello", string(root.Value().Data))
	assert.Equal(t, rawBefore, root.Value().Raw)
	assert.Equal(t, rawBytesBefore, string(snap.Raw(root.Value().Raw)))
}

func TestSnapshot_NestedChildren(t *testing.T) {
	p := &Parser{}
	p.Reset([]byte(`x = { a = [1, 2], b = { c = "s" } }`))
	assert.Equal(t, true, p.NextExpression())

	snap := p.Snapshot()
	p.Reset([]byte("y = 0"))
	assert.Equal(t, true, p.NextExpression())

	root := snap.Expression()
	table := root.Value()
	assert.Equal(t, InlineTable, table.Kind)

	it := table.Children()
	assert.Equal(t, true, it.Next())
	kvA := it.Node()
	assert.Equal(t, KeyValue, kvA.Kind)
	arr := kvA.Value()
	assert.Equal(t, Array, arr.Kind)
	var elems []string
	for c := arr.Children(); c.Next(); {
		elems = append(elems, string(c.Node().Data))
	}
	assert.Equal(t, []string{"1", "2"}, elems)

	assert.Equal(t, true, it.Next())
	kvB := it.Node()
	inner := kvB.Value()
	assert.Equal(t, InlineTable, inner.Kind)
	cit := inner.Children()
	assert.Equal(t, true, cit.Next())
	assert.Equal(t, "s", string(cit.Node().Value().Data))
	assert.Equal(t, false, it.Next())
}

func TestSnapshot_Comment(t *testing.T) {
	p := &Parser{KeepComments: true}
	p.Reset([]byte("# hello world\nx = 1 # inline\n"))

	assert.Equal(t, true, p.NextExpression())
	snapComment := p.Snapshot()
	assert.Equal(t, true, p.NextExpression())
	snapKV := p.Snapshot()

	p.Reset([]byte("z = 2"))

	c := snapComment.Expression()
	assert.Equal(t, Comment, c.Kind)
	assert.Equal(t, "# hello world", string(c.Data))
	assert.Equal(t, "# hello world", string(snapComment.Raw(c.Raw)))

	// The inline comment is attached as the key-value's next sibling and
	// is captured as part of its expression.
	kv := snapKV.Expression()
	assert.Equal(t, KeyValue, kv.Kind)
	comment := kv.Next()
	assert.Equal(t, Comment, comment.Kind)
	assert.Equal(t, "# inline", string(comment.Data))
}

func TestSnapshot_DoesNotRetainWholeInput(t *testing.T) {
	padding := strings.Repeat("pad = 1\n", 1000)
	doc := padding + "kept = \"value\"\n" + padding
	p := &Parser{}
	p.Reset([]byte(doc))

	var snap *Snapshot
	for p.NextExpression() {
		n := p.Expression()
		if n.Kind == KeyValue {
			k := n.Key()
			if k.Next() && string(k.Node().Data) == "kept" {
				snap = p.Snapshot()
			}
		}
	}
	assert.Equal(t, false, snap == nil)

	// The snapshot owns the expression's bytes, not the whole document.
	if len(snap.Data()) >= len(doc)/10 {
		t.Fatalf("snapshot retains %d bytes out of a %d bytes document", len(snap.Data()), len(doc))
	}
	assert.Equal(t, `kept = "value"`, strings.TrimSpace(string(snap.Data())))
	assert.Equal(t, "value", string(snap.Expression().Value().Data))
}

func TestSnapshot_IndependentBuffers(t *testing.T) {
	p := &Parser{}
	p.Reset([]byte(`k = "esc\taped"`))
	assert.Equal(t, true, p.NextExpression())
	snap1 := p.Snapshot()
	snap2 := p.Snapshot()

	// Mutating one snapshot's buffers must not affect the other.
	d1 := snap1.Expression().Value().Data
	for i := range d1 {
		d1[i] = 'X'
	}
	assert.Equal(t, "esc\taped", string(snap2.Expression().Value().Data))

	raw1 := snap1.Data()
	for i := range raw1 {
		raw1[i] = 'X'
	}
	assert.Equal(t, `"esc\taped"`, string(snap2.Raw(snap2.Expression().Value().Raw)))
}

func TestSnapshot_RangeRoundTrip(t *testing.T) {
	doc := "prefix = 1\nmid = [10, 20]\nsuffix = 2\n"
	p := &Parser{}
	p.Reset([]byte(doc))
	assert.Equal(t, true, p.NextExpression())
	assert.Equal(t, true, p.NextExpression())

	want := p.Expression().Raw
	snap := p.Snapshot()
	p.Reset([]byte("other = true"))

	got := snap.Expression().Raw
	assert.Equal(t, want, got)

	// Ranges stay in original-document coordinates and round-trip
	// through the snapshot's own bytes.
	assert.Equal(t, "mid = [10, 20]", string(snap.Raw(got)))
	assert.Equal(t, got, snap.Range(snap.Raw(got)))
	assert.Equal(t, uint32(len("prefix = 1\n")), got.Offset)

	// Nodes without a recorded Raw range (like arrays) resolve to nothing.
	assert.Equal(t, 0, len(snap.Raw(snap.Expression().Value().Raw)))
}

func TestSnapshot_RawOutOfRangePanics(t *testing.T) {
	p := &Parser{}
	p.Reset([]byte("x = 1"))
	assert.Equal(t, true, p.NextExpression())
	snap := p.Snapshot()

	assert.Panics(t, func() {
		snap.Raw(Range{Offset: 10000, Length: 4})
	})
}
