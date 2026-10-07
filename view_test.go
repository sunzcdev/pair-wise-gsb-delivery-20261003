package jsonquery

import (
	"strings"
	"testing"

	"github.com/antchfx/xpath"
	"github.com/stretchr/testify/require"
)

func newViewTestDoc(t *testing.T) *Node {
	t.Helper()
	doc, err := parseString(`{
		"store": {
			"book": [
				{
					"category": "reference",
					"author": "Nigel Rees",
					"title": "Sayings of the Century",
					"price": 8.95
				},
				{
					"category": "fiction",
					"author": "Evelyn Waugh",
					"title": "Sword of Honour",
					"price": 12.99
				},
				{
					"category": "fiction",
					"author": "Herman Melville",
					"title": "Moby Dick",
					"isbn": "0-553-21311-3",
					"price": 8.99
				},
				{
					"category": "fiction",
					"author": "J. R. R. Tolkien",
					"title": "The Lord of the Rings",
					"isbn": "0-395-19395-8",
					"price": 22.99
				}
			],
			"bicycle": {
				"color": "red",
				"price": 19.95
			}
		},
		"expensive": 10
	}`)
	require.NoError(t, err)
	return doc
}

func TestNewViewNilRoot(t *testing.T) {
	var v *View
	require.Nil(t, NewView(nil))
	require.Nil(t, v.Root())
	require.Nil(t, v.CreateXPathNavigator())
	require.Nil(t, v.QuerySelector(nil))
	require.Nil(t, v.QuerySelectorAll(nil))
}

func TestViewRootedOnTextAndDocument(t *testing.T) {
	doc := newViewTestDoc(t)

	// A scalar text node can also serve as a virtual root: it is presented as
	// a document root, has no visible relatives and still exposes its value.
	colorText := FindOne(doc, "/store/bicycle/color/text()")
	require.NotNil(t, colorText)
	tv := NewView(colorText)
	require.Equal(t, xpath.RootNode, tv.CreateXPathNavigator().NodeType())
	require.Empty(t, tv.FindAll(".."))
	require.Empty(t, tv.FindAll("preceding-sibling::*"))
	require.Empty(t, tv.FindAll("following-sibling::*"))
	require.Empty(t, tv.FindAll("/*"))
	require.Equal(t, "red", tv.CreateXPathNavigator().Value())

	// A view rooted on the document node covers the whole document.
	dv := NewView(doc)
	require.NotNil(t, dv.FindOne("/store/bicycle/color"))
	require.Len(t, dv.FindAll("/store/book/*"), 4)
}

func TestViewRootNodeType(t *testing.T) {
	doc := newViewTestDoc(t)
	bicycle := FindOne(doc, "/store/bicycle")
	require.NotNil(t, bicycle)
	require.Equal(t, ElementNode, bicycle.Type, "the original node keeps its own type")

	v := NewView(bicycle)
	nav := v.CreateXPathNavigator()
	require.Equal(t, bicycle, nav.Current(), "the virtual root is the original node")
	require.Equal(t, xpath.RootNode, nav.NodeType(),
		"the virtual root follows the same NodeType convention as a document root")

	// A full-document navigator on the same node still sees it as an element.
	full := CreateXPathNavigator(bicycle)
	require.Equal(t, xpath.ElementNode, full.NodeType())
}

func TestViewAbsolutePath(t *testing.T) {
	doc := newViewTestDoc(t)
	bicycle := FindOne(doc, "/store/bicycle")
	v := NewView(bicycle)

	// An absolute path resolves from the virtual root, not the document root.
	color := v.FindOne("/color")
	require.NotNil(t, color)
	require.Equal(t, "red", color.InnerText())
	require.Equal(t, FindOne(doc, "/store/bicycle/color"), color,
		"results point back at the original node")

	// Nothing outside the boundary is reachable through an absolute path.
	require.Nil(t, v.FindOne("/store"))
	require.Nil(t, v.FindOne("/expensive"))
	require.Nil(t, v.FindOne("/book"))
	require.Len(t, v.FindAll("/store//*"), 0)

	// Descendant queries are scoped to the view as well.
	prices := v.FindAll("//price")
	require.Len(t, prices, 1)
	require.Equal(t, "19.95", prices[0].InnerText())
}

func TestViewParentAndAncestorAxes(t *testing.T) {
	doc := newViewTestDoc(t)
	color := FindOne(doc, "/store/bicycle/color")
	v := NewView(FindOne(doc, "/store/bicycle"))

	// ancestor::* from a descendant stops at the virtual root and must not
	// leak store or the document node.
	anchor := NewView(color)
	nav := anchor.CreateXPathNavigator()
	require.Equal(t, xpath.RootNode, nav.NodeType())
	require.False(t, nav.MoveToParent(), "the virtual root has no parent in the view")

	colorNav := v.CreateXPathNavigator()
	require.True(t, colorNav.MoveToChild()) // first child: color
	require.Equal(t, "color", colorNav.Current().Data)
	require.True(t, colorNav.MoveToParent())
	require.Equal(t, v.Root(), colorNav.Current(), "parent walks only up to the boundary")
	require.False(t, colorNav.MoveToParent(), "parent cannot leave the view")

	// ancestor::* matches element nodes only; the virtual root is presented
	// as a document root, so use node() to select it on the ancestor axis.
	ancestors := v.FindAll("color/ancestor::node()")
	require.Len(t, ancestors, 1)
	require.Equal(t, v.Root(), ancestors[0])

	// ancestor-or-self from the boundary node returns only the boundary node.
	selfAncestors := v.FindAll("ancestor-or-self::node()")
	require.Len(t, selfAncestors, 1)
	require.Equal(t, v.Root(), selfAncestors[0])

	// parent::* from the virtual root itself is empty.
	require.Len(t, v.FindAll("../*"), 0)

	// From a deeply nested node, ancestors never escape.
	firstAuthor := FindOne(doc, "/store/book/*[1]/author")
	bookView := NewView(FindOne(doc, "/store/book/*[1]"))
	require.NotNil(t, firstAuthor)
	got := bookView.FindAll("author/ancestor::node()")
	require.Len(t, got, 1)
	require.Equal(t, bookView.Root(), got[0])
}

func TestViewSiblingAxes(t *testing.T) {
	doc := newViewTestDoc(t)

	// Boundary on one book element: the other books (its real siblings in the
	// document) must not be visible.
	book := FindOne(doc, "/store/book/*[2]")
	v := NewView(book)

	require.Len(t, v.FindAll("preceding-sibling::*"), 0)
	require.Len(t, v.FindAll("following-sibling::*"), 0)

	nav := v.CreateXPathNavigator()
	require.False(t, nav.MoveToPrevious())
	require.False(t, nav.MoveToNext())
	require.True(t, nav.MoveToFirst())
	require.Equal(t, v.Root(), nav.Current())

	// Inner siblings remain visible and stay inside the view.
	require.NotNil(t, v.FindOne("/author"))
	require.NotNil(t, v.FindOne("/price"))
	authors := v.FindAll("preceding-sibling::*")
	require.Empty(t, authors)

	price := v.FindOne("price")
	priceView := NewView(price)
	// price has author, category, title as preceding siblings in the document,
	// but in a view rooted at price none of them exist.
	require.Empty(t, priceView.FindAll("preceding-sibling::*"))
	require.Empty(t, priceView.FindAll("following-sibling::*"))

	// A view rooted at the whole book still sees sibling fields normally.
	bookFull := NewView(FindOne(doc, "/store/book/*[2]"))
	require.Equal(t, "Evelyn Waugh",
		bookFull.FindOne("title/preceding-sibling::author").InnerText())
}

func TestViewFollowingPrecedingAxes(t *testing.T) {
	doc := newViewTestDoc(t)
	v := NewView(FindOne(doc, "/store/bicycle"))

	// following/preceding axes climb with MoveToParent internally; they must
	// stop at the virtual root instead of reaching store/book/expensive.
	color := v.CreateXPathNavigator()
	require.True(t, color.MoveToChild()) // color

	// price follows color within the view.
	following := v.FindAll("color/following::*")
	require.NotEmpty(t, following)
	for _, n := range following {
		require.NotEqual(t, "book", n.Data)
		require.NotEqual(t, "expensive", n.Data)
	}

	preceding := v.FindAll("price/preceding::*")
	for _, n := range preceding {
		require.NotEqual(t, "store", n.Data)
	}

	// From the virtual root there is nothing before or after the document.
	require.Empty(t, v.FindAll("preceding::*"))
	require.Empty(t, v.FindAll("following::*"))
}

func TestViewResultsAreOriginalNodes(t *testing.T) {
	doc := newViewTestDoc(t)
	book := FindOne(doc, "/store/book/*[3]")
	v := NewView(book)

	found := v.FindOne("author")
	require.NotNil(t, found)
	require.Equal(t, FindOne(doc, "/store/book/*[3]/author"), found)
	require.Equal(t, "Herman Melville", found.InnerText())

	// The original tree is untouched: the boundary node still links outward.
	require.Equal(t, FindOne(doc, "/store/book"), book.Parent)
	require.Equal(t, FindOne(doc, "/store"), book.Parent.Parent)
	require.NotNil(t, book.PrevSibling, "outer siblings still exist in the original tree")
}

func TestViewCopyStaysInView(t *testing.T) {
	doc := newViewTestDoc(t)
	v := NewView(FindOne(doc, "/store/bicycle"))
	nav := v.CreateXPathNavigator()
	require.True(t, nav.MoveToChild()) // color

	cp := nav.Copy().(*NodeNavigator)
	require.Equal(t, nav.Current(), cp.Current())
	require.True(t, cp.MoveToParent())
	require.Equal(t, v.Root(), cp.Current(), "a copied navigator cannot leave the view")
	require.False(t, cp.MoveToParent())

	// Copy preserves the absolute-root semantics of the view.
	rootCopy := v.CreateXPathNavigator().Copy().(*NodeNavigator)
	require.True(t, rootCopy.MoveToChild())
	rootCopy.MoveToRoot()
	require.Equal(t, v.Root(), rootCopy.Current())
	require.Equal(t, xpath.RootNode, rootCopy.NodeType())
}

func TestViewMoveToRejectsForeignNavigators(t *testing.T) {
	doc := newViewTestDoc(t)
	bicycle := FindOne(doc, "/store/bicycle")

	v1 := NewView(bicycle)
	v2 := NewView(bicycle) // same boundary node, different view
	full := CreateXPathNavigator(bicycle)

	n1 := v1.CreateXPathNavigator()
	n2 := v2.CreateXPathNavigator()

	// Two distinct views over the identical node must not share positions.
	require.False(t, n1.MoveTo(n2), "a foreign view cannot be moved into")
	require.Equal(t, v1.Root(), n1.Current(), "a rejected MoveTo leaves the navigator unchanged")

	// A restricted navigator cannot move to a full-document navigator either.
	require.False(t, n1.MoveTo(full))
	require.False(t, full.MoveTo(n1))

	// MoveTo within the same view still works.
	inner := v1.CreateXPathNavigator()
	require.True(t, inner.MoveToChild()) // color
	target := v1.CreateXPathNavigator()
	require.True(t, target.MoveTo(inner))
	require.Equal(t, "color", target.Current().Data)
}

func TestMultipleViewsAreIndependent(t *testing.T) {
	doc := newViewTestDoc(t)

	bicycleView := NewView(FindOne(doc, "/store/bicycle"))
	bookView := NewView(FindOne(doc, "/store/book/*[1]"))

	// Run both views in parallel and assert each only sees its own subtree.
	done := make(chan string, 2)
	query := func(v *View, want, forbidden string) {
		got := v.FindOne("//" + want)
		if got == nil {
			done <- "missing " + want
			return
		}
		if v.FindOne("//"+forbidden) != nil {
			done <- "leaked " + forbidden
			return
		}
		done <- "ok"
	}
	go query(bicycleView, "color", "author")
	go query(bookView, "author", "color")

	for i := 0; i < 2; i++ {
		require.Equal(t, "ok", <-done)
	}

	// Each view keeps its own root and external values stay hidden.
	require.Equal(t, "red", bicycleView.FindOne("/color").InnerText())
	require.Nil(t, bicycleView.FindOne("/title"))
	require.Equal(t, "Nigel Rees", bookView.FindOne("/author").InnerText())
	require.Nil(t, bookView.FindOne("/color"))
}

func TestViewSelectorAPI(t *testing.T) {
	doc := newViewTestDoc(t)
	v := NewView(FindOne(doc, "/store/bicycle"))

	exp, err := xpath.Compile("price")
	require.NoError(t, err)

	one := v.QuerySelector(exp)
	require.NotNil(t, one)
	require.Equal(t, "19.95", one.InnerText())

	all := v.QuerySelectorAll(exp)
	require.Len(t, all, 1)
	require.Equal(t, one, all[0])

	// Ensure the helper builds against an io.Reader-backed doc too.
	other, err := Parse(strings.NewReader(`{"a":{"b":1},"c":2}`))
	require.NoError(t, err)
	ov := NewView(FindOne(other, "/a"))
	require.Equal(t, "1", ov.FindOne("b").InnerText())
	require.Nil(t, ov.FindOne("/c"))
}
