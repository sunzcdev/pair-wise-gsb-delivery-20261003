package jsonquery

import (
	"testing"
)

const cursorTestDoc = `{
  "store": {
    "book": [
      {"category":"reference","author":"Nigel Rees","title":"Sayings of the Century","price":8.95},
      {"category":"fiction","author":"Evelyn Waugh","title":"Sword of Honour","price":12.99},
      {"category":"fiction","author":"Herman Melville","title":"Moby Dick","isbn":"0-553-21311-3","price":8.99}
    ],
    "bicycle": {"color":"red","price":19.95}
  },
  "expensive": 10
}`

func nodeSet(root *Node) map[*Node]struct{} {
	set := make(map[*Node]struct{})
	var walk func(*Node)
	walk = func(n *Node) {
		set[n] = struct{}{}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return set
}

// countVersions returns how many nodes of root are shared with (present in)
// old and how many are freshly allocated.
func countVersions(root *Node, old map[*Node]struct{}) (shared, fresh int) {
	var walk func(*Node)
	walk = func(n *Node) {
		if _, ok := old[n]; ok {
			shared++
		} else {
			fresh++
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return shared, fresh
}

func countNodes(root *Node) int {
	n := 1
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		n += countNodes(c)
	}
	return n
}

func TestCursorNavigation(t *testing.T) {
	doc, err := parseString(cursorTestDoc)
	if err != nil {
		t.Fatal(err)
	}
	cur := NewCursor(doc)
	if cur.Path() != "/" {
		t.Fatalf("root path = %q", cur.Path())
	}
	cur, ok := cur.DownName("store")
	if !ok {
		t.Fatal("no store member")
	}
	if cur, ok = cur.DownName("book"); !ok {
		t.Fatal("no book member")
	}
	if cur, ok = cur.DownIndex(1); !ok {
		t.Fatal("no second book")
	}
	if cur, ok = cur.DownName("price"); !ok {
		t.Fatal("no price member")
	}
	if expected := "/store/book/*[2]/price"; cur.Path() != expected {
		t.Fatalf("path = %q, want %q", cur.Path(), expected)
	}
	if cur.Value() != 12.99 {
		t.Fatalf("value = %v, want 12.99", cur.Value())
	}
	// Walking back up returns to the root; an unmodified cursor reproduces
	// the original root pointer.
	up, ok := cur.Up()
	if !ok || up.Path() != "/store/book/*[2]" {
		t.Fatalf("up path = %q", up.Path())
	}
	if root := cur.Root(); root != doc {
		t.Fatal("unmodified cursor should return the original root")
	}
	// Sibling moves.
	prev, ok := cur.Prev()
	if !ok || prev.Node().Data != "category" {
		t.Fatalf("prev = %v", prev.Node())
	}
	if next, ok := cur.Next(); !ok || next.Node().Data != "title" {
		t.Fatalf("next = %v", next.Node())
	}
}

func TestCursorBranchesShareUntouchedSubtrees(t *testing.T) {
	doc, err := parseString(cursorTestDoc)
	if err != nil {
		t.Fatal(err)
	}
	base := NewCursor(doc)

	// Branch A: deep edit, replace the price of the second book.
	curA, _ := base.DownName("store")
	curA, _ = curA.DownName("book")
	curA, _ = curA.DownIndex(1)
	curA, _ = curA.DownName("price")
	rootA := curA.SetValue(5.99).Root()

	// Branch B: shallow edit, rename the top-level "expensive" member.
	curB, _ := base.DownName("expensive")
	curB = curB.Rename("costly")
	if expected := "/costly"; curB.Path() != expected {
		t.Fatalf("renamed path = %q, want %q", curB.Path(), expected)
	}
	rootB := curB.Root()

	// XPath queries against each branch see their own version.
	if n := FindOne(rootA, "//book/*[2]/price"); n == nil || n.InnerText() != "5.99" {
		t.Fatalf("branch A price = %v", n)
	}
	if n := FindOne(rootA, "//bicycle/price"); n == nil || n.InnerText() != "19.95" {
		t.Fatalf("branch A bicycle price = %v", n)
	}
	if n := FindOne(rootB, "/costly"); n == nil || n.InnerText() != "10" {
		t.Fatalf("branch B costly = %v", n)
	}
	if n := FindOne(rootB, "/expensive"); n != nil {
		t.Fatal("branch B should not contain /expensive")
	}
	if n := FindOne(rootB, "//book/*[2]/price"); n == nil || n.InnerText() != "12.99" {
		t.Fatalf("branch B book price = %v", n)
	}

	// The original snapshot is untouched and still queryable.
	if n := FindOne(doc, "//book/*[2]/price"); n == nil || n.InnerText() != "12.99" {
		t.Fatalf("original price changed: %v", n)
	}
	if n := FindOne(doc, "/expensive"); n == nil {
		t.Fatal("original /expensive missing")
	}
	if got, want := countNodes(doc), countNodes(rootA); got != want {
		t.Fatalf("branch A node count = %d, want %d", want, got)
	}

	// Untouched subtrees are shared, not copied. Nodes along the edited path
	// and their siblings are re-linked as fresh copies (their navigation
	// relations belong to the new version), but everything below them is the
	// very same nodes as in the original snapshot.
	oldNodes := nodeSet(doc)
	if FindOne(rootA, "//bicycle/color") != FindOne(doc, "//bicycle/color") {
		t.Fatal("branch A should share the bicycle/color node with the original")
	}
	if FindOne(rootB, "//store/book") != FindOne(doc, "//store/book") {
		t.Fatal("branch B should share the store/book subtree with the original")
	}
	sharedA, freshA := countVersions(rootA, oldNodes)
	sharedB, freshB := countVersions(rootB, oldNodes)
	total := len(oldNodes)
	if sharedA == 0 || sharedA >= total {
		t.Fatalf("branch A shared %d of %d nodes", sharedA, total)
	}
	if freshA > total/2 {
		t.Fatalf("branch A copied %d of %d nodes; deep edit should copy only the path", freshA, total)
	}
	if sharedB <= sharedA {
		t.Fatalf("shallow edit (shared %d) should share more than deep edit (shared %d)", sharedB, sharedA)
	}
	if freshB != 3 { // new document node + renamed member + re-linked store sibling
		t.Fatalf("branch B allocated %d nodes, want 3", freshB)
	}
	t.Logf("branch A: shared=%d fresh=%d; branch B: shared=%d fresh=%d; total=%d",
		sharedA, freshA, sharedB, freshB, total)
}

func TestCursorParentAxisStaysInVersion(t *testing.T) {
	doc, err := parseString(cursorTestDoc)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := NewCursor(doc).DownName("expensive")
	root := cur.SetValue(100.0).Root()

	// The bicycle/color node is shared with the original tree; navigating
	// up from it must stay inside the new version.
	n := FindOne(root, "//bicycle/color/../price")
	if n == nil || n.InnerText() != "19.95" {
		t.Fatalf("//bicycle/color/../price = %v", n)
	}
	if n = FindOne(root, "//bicycle/../../expensive"); n == nil || n.InnerText() != "100" {
		t.Fatalf("ancestor traversal leaked versions: %v", n)
	}
	// The same queries on the original snapshot are unaffected.
	if n = FindOne(doc, "//bicycle/../../expensive"); n == nil || n.InnerText() != "10" {
		t.Fatalf("original snapshot changed: %v", n)
	}
}

func TestCursorBranchFromVersionedRoot(t *testing.T) {
	doc, err := parseString(cursorTestDoc)
	if err != nil {
		t.Fatal(err)
	}
	// First generation: rename the top-level member.
	cur, _ := NewCursor(doc).DownName("expensive")
	rootA := cur.Rename("costly").Root()

	// Second generation: derive a deeper edit from the versioned root.
	deep, _ := NewCursor(rootA).DownName("store")
	deep, _ = deep.DownName("bicycle")
	deep, _ = deep.DownName("color")
	rootB := deep.SetValue("blue").Root()

	if n := FindOne(rootB, "//bicycle/color"); n == nil || n.InnerText() != "blue" {
		t.Fatalf("second generation color = %v", n)
	}
	if n := FindOne(rootB, "/costly"); n == nil {
		t.Fatal("second generation lost the first generation rename")
	}
	// Both earlier versions remain intact and queryable.
	if n := FindOne(rootA, "//bicycle/color"); n == nil || n.InnerText() != "red" {
		t.Fatalf("first generation color changed: %v", n)
	}
	if n := FindOne(rootA, "/costly"); n == nil {
		t.Fatal("first generation lost its rename")
	}
	if n := FindOne(doc, "/expensive"); n == nil || n.InnerText() != "10" {
		t.Fatalf("original snapshot changed: %v", n)
	}
	// The second generation still shares untouched subtrees with the
	// original snapshot.
	if FindOne(rootB, "//book/*[1]") != FindOne(doc, "//book/*[1]") {
		t.Fatal("second generation should share the book subtree")
	}
}

func TestCursorOldCursorsRemainValid(t *testing.T) {
	doc, err := parseString(cursorTestDoc)
	if err != nil {
		t.Fatal(err)
	}
	// Position a cursor before any edit happens.
	cur, _ := NewCursor(doc).DownName("store")
	cur, _ = cur.DownName("bicycle")
	cur, _ = cur.DownName("color")

	// Derive and materialize an edited branch.
	other, _ := NewCursor(doc).DownName("expensive")
	root2 := other.SetValue(100.0).Root()

	// The pre-existing cursor still sees the old snapshot.
	if got := cur.Root(); got != doc {
		t.Fatal("old cursor root changed")
	}
	if cur.Value() != "red" || cur.Path() != "/store/bicycle/color" {
		t.Fatalf("old cursor = %v at %s", cur.Value(), cur.Path())
	}
	if n := FindOne(doc, "//bicycle/color"); n == nil || n.InnerText() != "red" {
		t.Fatal("old snapshot corrupted by edit")
	}
	if n := FindOne(root2, "/expensive"); n == nil || n.InnerText() != "100" {
		t.Fatalf("edited root /expensive = %v", n)
	}
}

func TestCursorArrayInsertAndRemove(t *testing.T) {
	doc, err := parseString(`{"name":"John","hobbies":["coding","eating","football"]}`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert into the middle of the array.
	hobbies, _ := NewCursor(doc).DownName("hobbies")
	inserted, ok := hobbies.InsertChildAt(1, NewElement("", "reading"))
	if !ok {
		t.Fatal("insert failed")
	}
	root := inserted.Root()
	if got := len(Find(root, "//hobbies/*")); got != 4 {
		t.Fatalf("after insert got %d items, want 4", got)
	}
	if n := FindOne(root, "//hobbies/*[2]"); n == nil || n.InnerText() != "reading" {
		t.Fatalf("item 2 = %v", n)
	}
	if n := FindOne(root, "//hobbies/*[3]"); n == nil || n.InnerText() != "eating" {
		t.Fatalf("item 3 = %v", n)
	}
	// Original array is unchanged.
	if got := len(Find(doc, "//hobbies/*")); got != 3 {
		t.Fatalf("original got %d items, want 3", got)
	}

	// Delete the first item: the cursor falls back to the next sibling,
	// which now occupies the deleted position.
	first, _ := hobbies.DownIndex(0)
	after, ok := first.Remove()
	if !ok {
		t.Fatal("remove failed")
	}
	if after.Value() != "eating" || after.Path() != "/hobbies/*[1]" {
		t.Fatalf("after remove: value=%v path=%s", after.Value(), after.Path())
	}
	root = after.Root()
	if got := len(Find(root, "//hobbies/*")); got != 2 {
		t.Fatalf("after remove got %d items, want 2", got)
	}
	if n := FindOne(root, "//hobbies/*[1]"); n == nil || n.InnerText() != "eating" {
		t.Fatalf("item 1 = %v", n)
	}

	// Delete the last item: the cursor falls back to the previous sibling.
	last, _ := hobbies.DownIndex(2)
	after, ok = last.Remove()
	if !ok {
		t.Fatal("remove last failed")
	}
	if after.Value() != "eating" || after.Path() != "/hobbies/*[2]" {
		t.Fatalf("after removing last: value=%v path=%s", after.Value(), after.Path())
	}

	// Delete the only child: the cursor falls back to the parent.
	solo, _ := NewCursor(doc).DownName("name")
	solo, ok = solo.Down() // the text node
	if !ok {
		t.Fatal("no text child")
	}
	after, ok = solo.Remove()
	if !ok {
		t.Fatal("remove only child failed")
	}
	if after.Path() != "/name" || len(after.Node().ChildNodes()) != 0 {
		t.Fatalf("after removing only child: path=%s children=%d",
			after.Path(), len(after.Node().ChildNodes()))
	}
	if n := FindOne(after.Root(), "/name"); n == nil {
		t.Fatal("/name element should remain after its text was removed")
	}
}

func TestCursorSetValueRebuildsContainer(t *testing.T) {
	doc, err := parseString(`{"a":{"x":1},"b":[1,2]}`)
	if err != nil {
		t.Fatal(err)
	}
	// Replace an object with an array in one branch.
	cur, _ := NewCursor(doc).DownName("a")
	root := cur.SetValue([]interface{}{"u", "v"}).Root()
	if got := len(Find(root, "/a/*")); got != 2 {
		t.Fatalf("got %d items, want 2", got)
	}
	if n := FindOne(root, "/a/*[2]"); n == nil || n.InnerText() != "v" {
		t.Fatalf("/a/*[2] = %v", n)
	}
	if n := FindOne(root, "/a/x"); n != nil {
		t.Fatal("old member /a/x should be gone")
	}
	// Values propagate to the recomputed containers.
	if arr, ok := FindOne(root, "/a").Value().([]interface{}); !ok || len(arr) != 2 {
		t.Fatalf("/a value = %#v", FindOne(root, "/a").Value())
	}
	// The sibling branch of the old tree is shared and intact.
	if FindOne(root, "/b/*[1]") != FindOne(doc, "/b/*[1]") {
		t.Fatal("/b subtree should be shared")
	}
	if n := FindOne(doc, "/a/x"); n == nil || n.InnerText() != "1" {
		t.Fatal("original /a/x corrupted")
	}
}
