package unstable

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2/internal/assert"
)

func TestSnapshot_NilWithoutExpression(t *testing.T) {
	var p Parser
	assert.True(t, p.Snapshot() == nil)

	var s Snapshot
	assert.True(t, s.Expression() == nil)
}

func TestSnapshot_SurvivesReset(t *testing.T) {
	var p Parser
	p.KeepComments = true
	p.Reset([]byte("greeting = \"hello\" # a comment\n"))
	assert.True(t, p.NextExpression())

	snap := p.Snapshot()
	assert.True(t, snap != nil)

	// Reuse the parser on a completely different document.
	p.Reset([]byte("[other]\nx = [1, 2, 3]\n"))
	for p.NextExpression() {
	}
	assert.NoError(t, p.Error())

	root := snap.Expression()
	assert.Equal(t, KeyValue, root.Kind)

	key := root.Key()
	assert.True(t, key.Next())
	assert.Equal(t, Key, key.Node().Kind)
	assert.Equal(t, "greeting", string(key.Node().Data))
	assert.Equal(t, "greeting", string(snap.Raw(key.Node().Raw)))

	value := root.Value()
	assert.Equal(t, String, value.Kind)
	assert.Equal(t, "hello", string(value.Data))
	assert.Equal(t, `"hello"`, string(snap.Raw(value.Raw)))

	comment := root.Next()
	assert.True(t, comment.Valid())
	assert.Equal(t, Comment, comment.Kind)
	assert.Equal(t, "# a comment", string(comment.Data))
	assert.Equal(t, "# a comment", string(snap.Raw(comment.Raw)))
}

func TestSnapshot_NestedValuesTraversable(t *testing.T) {
	var p Parser
	p.Reset([]byte("point = {x = 1, y = [2, 3]}\n"))
	assert.True(t, p.NextExpression())
	snap := p.Snapshot()

	p.Reset([]byte("unrelated = true\n"))
	for p.NextExpression() {
	}

	table := snap.Expression().Value()
	assert.Equal(t, InlineTable, table.Kind)

	it := table.Children()
	assert.True(t, it.Next())
	x := it.Node()
	assert.Equal(t, KeyValue, x.Kind)
	xk := x.Key()
	assert.True(t, xk.Next())
	assert.Equal(t, "x", string(xk.Node().Data))
	assert.Equal(t, "1", string(x.Value().Data))

	assert.True(t, it.Next())
	y := it.Node()
	array := y.Value()
	assert.Equal(t, Array, array.Kind)

	var elements []string
	eit := array.Children()
	for eit.Next() {
		elements = append(elements, string(eit.Node().Data))
	}
	assert.Equal(t, []string{"2", "3"}, elements)
	assert.False(t, it.Next())
}

func TestSnapshot_AllocatedValueData(t *testing.T) {
	// Escaped strings are unescaped into freshly allocated Data that does
	// not reference the parser input.
	var p Parser
	p.Reset([]byte(`key = "a\nb"` + "\n"))
	assert.True(t, p.NextExpression())
	snap := p.Snapshot()

	p.Reset([]byte("other = 1\n"))
	for p.NextExpression() {
	}

	value := snap.Expression().Value()
	assert.Equal(t, "a\nb", string(value.Data))
	assert.Equal(t, `"a\nb"`, string(snap.Raw(value.Raw)))
}

func TestSnapshot_SnapshotsAreIndependent(t *testing.T) {
	var p Parser

	p.Reset([]byte("first = 1\n"))
	assert.True(t, p.NextExpression())
	snap1 := p.Snapshot()

	p.Reset([]byte("second = 2\n"))
	assert.True(t, p.NextExpression())
	snap2 := p.Snapshot()

	// Reading the second snapshot must not disturb the first, and their
	// buffers must not alias.
	assert.Equal(t, "2", string(snap2.Expression().Value().Data))
	assert.Equal(t, "1", string(snap1.Expression().Value().Data))
	k1 := snap1.Expression().Key()
	assert.True(t, k1.Next())
	assert.Equal(t, "first", string(k1.Node().Data))
	k2 := snap2.Expression().Key()
	assert.True(t, k2.Next())
	assert.Equal(t, "second", string(k2.Node().Data))
}

func TestSnapshot_DoesNotRetainWholeInput(t *testing.T) {
	doc := "wanted = 1\npadding = \"" + strings.Repeat("x", 100000) + "\"\n"
	var p Parser
	p.Reset([]byte(doc))
	assert.True(t, p.NextExpression())
	snap := p.Snapshot()

	key := snap.Expression().Key()
	assert.True(t, key.Next())
	assert.Equal(t, "wanted", string(key.Node().Data))
	assert.True(t, len(snap.Data()) < 100,
		"snapshot should only own the bytes of its expression, got %d", len(snap.Data()))
}

func TestSnapshot_RangesRelativeToSnapshotData(t *testing.T) {
	input := "prefix = 1\ntarget = [1, 2]\n"
	var p Parser
	p.Reset([]byte(input))
	assert.True(t, p.NextExpression()) // prefix = 1
	assert.True(t, p.NextExpression()) // target = [1, 2]
	snap := p.Snapshot()

	value := snap.Expression().Value()
	child := value.Child()
	assert.Equal(t, Integer, child.Kind)
	r := child.Raw
	// The range is rebased on the snapshot data, not on the original input.
	assert.Equal(t, "1", string(snap.Raw(child.Raw)))
	assert.Equal(t, "1", string(snap.Data()[r.Offset:r.Offset+r.Length]))

	shape := snap.Shape(r)
	assert.Equal(t, 1, shape.Start.Line)
	assert.Equal(t, int(r.Offset), shape.Start.Offset)
}
