package jsonquery

import (
	"sync"
	"testing"

	"github.com/antchfx/xpath"
	"github.com/stretchr/testify/require"
)

const viewTestJSON = `{
	"expensive": 10,
	"store": {
		"bicycle": { "color": "red", "price": 19.95 },
		"book": [
			{ "category": "reference", "price": 8.95 },
			{ "category": "fiction", "price": 12.99 }
		]
	}
}`

func parseViewTestDoc(t *testing.T) (doc, store, bicycle *Node) {
	t.Helper()
	doc, err := parseString(viewTestJSON)
	require.NoError(t, err)
	store = FindOne(doc, "//store")
	require.NotNil(t, store)
	bicycle = FindOne(doc, "//bicycle")
	require.NotNil(t, bicycle)
	return doc, store, bicycle
}

func TestViewRootNodeType(t *testing.T) {
	doc, store, _ := parseViewTestDoc(t)
	view := NewView(store)

	nav := view.Navigator()
	// The virtual root uses the same NodeType convention as a real
	// document root.
	require.Equal(t, xpath.RootNode, nav.NodeType())
	docNav := CreateXPathNavigator(doc)
	require.Equal(t, docNav.NodeType(), nav.NodeType())

	// Children of the virtual root are regular elements.
	require.True(t, nav.MoveToChild())
	require.Equal(t, xpath.ElementNode, nav.NodeType())
	require.Equal(t, "bicycle", nav.Current().Data)

	// MoveToRoot returns to the virtual root, not the document root.
	nav.MoveToRoot()
	require.Equal(t, xpath.RootNode, nav.NodeType())
	require.Same(t, store, nav.Current())
}

func TestViewParentAxis(t *testing.T) {
	doc, store, bicycle := parseViewTestDoc(t)
	view := NewView(store)

	// Parent axis inside the view still works and returns original nodes.
	n := view.FindOne("//color/..")
	require.Same(t, bicycle, n)

	// The parent of the virtual root is not reachable.
	require.Empty(t, view.Find("//color/../../.."))
	require.Empty(t, view.Find("/.."))

	// The unscoped document still sees the real parent chain.
	require.Same(t, store, FindOne(doc, "//bicycle/.."))
}

func TestViewAncestorAxis(t *testing.T) {
	doc, store, _ := parseViewTestDoc(t)
	view := NewView(store)

	// Inside the view, ancestors of color stop below the virtual root.
	nodes := view.Find("//color/ancestor::*")
	require.Len(t, nodes, 1)
	require.Equal(t, "bicycle", nodes[0].Data)

	// The same node in the unscoped document sees store as an ancestor.
	require.Len(t, Find(doc, "//color/ancestor::*"), 2)

	// The virtual root itself is not an element, so a named ancestor
	// query cannot escape the view either.
	require.Empty(t, view.Find("//color/ancestor::store"))
}

func TestViewSiblingAxis(t *testing.T) {
	_, store, bicycle := parseViewTestDoc(t)
	view := NewView(store)

	// The virtual root has no siblings inside the view.
	nav := view.Navigator()
	require.False(t, nav.MoveToNext())
	require.False(t, nav.MoveToPrevious())
	require.False(t, nav.MoveToFirst())
	require.Same(t, store, nav.Current())

	// Sibling axes between nodes inside the view keep working.
	nodes := view.Find("/bicycle/following-sibling::*")
	require.Len(t, nodes, 1)
	require.Equal(t, "book", nodes[0].Data)

	// A view rooted at bicycle hides its sibling book.
	sub := NewView(bicycle)
	require.Empty(t, sub.Find("//book"))
	nodes = sub.Find("/color/following-sibling::*")
	require.Len(t, nodes, 1)
	require.Equal(t, "price", nodes[0].Data)
}

func TestViewAbsolutePath(t *testing.T) {
	_, store, _ := parseViewTestDoc(t)
	view := NewView(store)

	// Absolute paths resolve against the virtual root.
	n := view.FindOne("/bicycle/color")
	require.NotNil(t, n)
	require.Equal(t, "red", n.InnerText())

	// Nodes outside the boundary are invisible to absolute and
	// descendant queries.
	require.Empty(t, view.Find("/expensive"))
	require.Empty(t, view.Find("//expensive"))
	require.Nil(t, view.FindOne("//expensive"))
	// The virtual root itself is not its own descendant.
	require.Empty(t, view.Find("//store"))
}

func TestViewResultsPointToOriginalNodes(t *testing.T) {
	doc, store, _ := parseViewTestDoc(t)
	view := NewView(store)

	// Query results are the same *Node values as in the original tree.
	require.Same(t, FindOne(doc, "//bicycle"), view.FindOne("//bicycle"))

	got := view.Find("//book/*/price")
	want := Find(doc, "//book/*/price")
	require.Len(t, got, 2)
	require.Equal(t, len(want), len(got))
	for i := range want {
		require.Same(t, want[i], got[i])
	}

	// Navigating from a result still reaches the original tree parents
	// up to the boundary.
	n := view.FindOne("//color")
	require.NotNil(t, n)
	require.Same(t, FindOne(doc, "//bicycle"), n.Parent)
	require.Same(t, store, n.Parent.Parent)
}

func TestViewDoesNotModifyTree(t *testing.T) {
	doc, store, bicycle := parseViewTestDoc(t)
	parent, prev := store.Parent, store.PrevSibling
	first, last := store.FirstChild, store.LastChild

	view := NewView(store)
	_ = view.Find("//*")
	nav := view.Navigator()
	nav.MoveToChild()
	nav.MoveToRoot()

	require.Same(t, doc, parent)
	require.Same(t, parent, store.Parent)
	require.Same(t, prev, store.PrevSibling)
	require.Same(t, first, store.FirstChild)
	require.Same(t, last, store.LastChild)
	require.Same(t, store, bicycle.Parent)
	require.Equal(t, 2, len(doc.ChildNodes()))
}

func TestViewMoveToAcrossViews(t *testing.T) {
	doc, store, bicycle := parseViewTestDoc(t)
	storeView := NewView(store)
	bicycleView := NewView(bicycle)

	storeNav := storeView.Navigator()
	bicycleNav := bicycleView.Navigator()

	// MoveTo cannot pull nodes from another view in.
	require.False(t, storeNav.MoveTo(bicycleNav))
	require.Same(t, store, storeNav.Current())
	require.False(t, bicycleNav.MoveTo(storeNav))
	require.Same(t, bicycle, bicycleNav.Current())

	// An unscoped navigator is yet another view, even on the same node.
	require.False(t, CreateXPathNavigator(store).MoveTo(storeNav))
	require.False(t, storeNav.MoveTo(CreateXPathNavigator(store)))
	require.False(t, storeNav.MoveTo(CreateXPathNavigator(doc)))

	// Copy stays in the same view, so MoveTo within a view works.
	c := storeNav.Copy()
	require.True(t, c.MoveToChild())
	require.True(t, storeNav.MoveTo(c))
	require.Same(t, store.FirstChild, storeNav.Current())
}

func TestMultipleViewsUsedInParallel(t *testing.T) {
	doc, store, bicycle := parseViewTestDoc(t)
	storeView := NewView(store)
	bicycleView := NewView(bicycle)
	docView := Find(doc, "//price") // unscoped baseline: 3 prices
	require.Len(t, docView, 3)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			require.Len(t, storeView.Find("//price"), 3)
			require.Empty(t, storeView.Find("//expensive"))
		}()
		go func() {
			defer wg.Done()
			nodes := bicycleView.Find("//price")
			require.Len(t, nodes, 1)
			require.Equal(t, "19.95", nodes[0].InnerText())
			require.Empty(t, bicycleView.Find("//book"))
		}()
		go func() {
			defer wg.Done()
			require.Len(t, Find(doc, "//price"), 3)
			require.NotNil(t, FindOne(doc, "//expensive"))
		}()
	}
	wg.Wait()
}
