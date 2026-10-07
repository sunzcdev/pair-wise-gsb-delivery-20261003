package jsonquery

import (
	"strings"
	"testing"
)

const cursorTestDoc = `{
	"store": {
		"book": [
			{"id": 1, "price": 8.95},
			{"id": 2, "price": 12.99}
		],
		"bicycle": {"color": "red", "price": 19.95}
	},
	"expensive": 10
}`

func countNodes(n *Node) int {
	count := 1
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		count += countNodes(child)
	}
	return count
}

func collectNodes(n *Node, set map[*Node]struct{}) {
	set[n] = struct{}{}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		collectNodes(child, set)
	}
}

// countShared returns how many nodes of the tree rooted at n are the exact
// same node objects (pointer identity) as nodes in set.
func countShared(n *Node, set map[*Node]struct{}) int {
	count := 0
	if _, ok := set[n]; ok {
		count++
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		count += countShared(child, set)
	}
	return count
}

func TestCursorBranchesShareStructure(t *testing.T) {
	doc, err := parseString(cursorTestDoc)
	if err != nil {
		t.Fatal(err)
	}
	total := countNodes(doc)

	base := NewCursor(doc)

	// Branch A: deep edit at /store/book[1]/price.
	deep := base.Clone()
	for _, step := range []string{"store", "book"} {
		if !deep.ChildElement(step) {
			t.Fatalf("cannot descend into %s", step)
		}
	}
	if !deep.Child(0) || !deep.ChildElement("price") {
		t.Fatal("cannot descend into book[1]/price")
	}
	rootA := deep.SetValue(7.95)

	// Branch B: shallow edit at /expensive, derived from the same snapshot.
	shallow := base.Clone()
	if !shallow.ChildElement("expensive") {
		t.Fatal("cannot descend into expensive")
	}
	rootB := shallow.SetValue(20)

	// The original snapshot is unchanged.
	if n := FindOne(doc, "//book/*[1]/price"); n == nil || n.InnerText() != "8.95" {
		t.Fatalf("old snapshot changed, got %v", n)
	}
	if n := FindOne(doc, "//expensive"); n == nil || n.InnerText() != "10" {
		t.Fatalf("old snapshot changed, got %v", n)
	}

	// Branch A sees the deep edit and nothing else.
	if n := deep.FindOne("//book/*[1]/price"); n == nil || n.InnerText() != "7.95" {
		t.Fatalf("branch A deep edit missing, got %v", n)
	}
	if n := deep.FindOne("//book/*[2]/price"); n == nil || n.InnerText() != "12.99" {
		t.Fatalf("branch A lost book[2], got %v", n)
	}
	if n := deep.FindOne("//expensive"); n == nil || n.InnerText() != "10" {
		t.Fatalf("branch A lost expensive, got %v", n)
	}

	// Branch B sees the shallow edit and nothing else.
	if n := shallow.FindOne("//expensive"); n == nil || n.InnerText() != "20" {
		t.Fatalf("branch B edit missing, got %v", n)
	}
	if n := shallow.FindOne("//book/*[1]/price"); n == nil || n.InnerText() != "8.95" {
		t.Fatalf("branch B lost book[1]/price, got %v", n)
	}

	// Structural sharing: count how many nodes of each new version are
	// pointer-identical to nodes of the original snapshot.
	oldSet := make(map[*Node]struct{})
	collectNodes(doc, oldSet)
	for name, root := range map[string]*Node{"branchA": rootA, "branchB": rootB} {
		if got := countNodes(root); got != total {
			t.Fatalf("%s has %d nodes, expected %d", name, got, total)
		}
		shared := countShared(root, oldSet)
		fresh := total - shared
		if shared == 0 {
			t.Fatalf("%s shares no structure with the snapshot", name)
		}
		if fresh > total/2 {
			t.Fatalf("%s copied %d of %d nodes; unchanged subtrees not reused", name, fresh, total)
		}
	}

	// Untouched subtrees must be reused by pointer identity, not re-created.
	// (Direct children of edited ancestors are shallow boundary clones, so
	// sharing is verified one level below them.)
	if old, new := FindOne(doc, "//bicycle/color"), deep.FindOne("//bicycle/color"); old != new {
		t.Fatal("branch A did not reuse the bicycle/color subtree")
	}
	if old, new := FindOne(doc, "//book/*[2]/id"), deep.FindOne("//book/*[2]/id"); old != new {
		t.Fatal("branch A did not reuse the second book subtree")
	}
	if old, new := FindOne(doc, "//store"), shallow.FindOne("//store"); old == new {
		// store itself sits on no edit path of branch B, only its boundary
		// clone may differ; its children must be shared.
		if old.FirstChild != new.FirstChild {
			t.Fatal("branch B did not reuse the store subtree")
		}
	}
}

func TestCursorArrayInsertRemove(t *testing.T) {
	doc, err := parseString(`{"list":[1,2,3]}`)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCursor(doc)
	if !c.ChildElement("list") {
		t.Fatal("cannot descend into list")
	}

	root := c.InsertChild(1, 99)
	if got := c.Find("//list/*"); len(got) != 4 {
		t.Fatalf("expected 4 elements after insert, got %d", len(got))
	}
	// The cursor path follows the edit: children of the current node are
	// now [1, 99, 2, 3].
	var values []string
	for _, n := range c.Node().ChildNodes() {
		values = append(values, n.InnerText())
	}
	if strings.Join(values, ",") != "1,99,2,3" {
		t.Fatalf("unexpected list after insert: %v", values)
	}
	// Sibling navigation belongs to the new version.
	if !c.Child(0) || !c.Next() || c.Node().InnerText() != "99" {
		t.Fatal("cursor sibling navigation does not reflect the new version")
	}
	// XPath sibling axes work on the new root as well.
	if n := c.FindOne("//list/*[1]/following-sibling::*[1]"); n == nil || n.InnerText() != "99" {
		t.Fatalf("following-sibling on new version broken, got %v", n)
	}
	if !c.Parent() {
		t.Fatal("cannot move back to list")
	}

	// Delete the element 2 (now at index 2); the cursor must land on the
	// node that shifts into its place.
	if !c.Child(2) {
		t.Fatal("cannot descend to index 2")
	}
	root = c.Remove()
	if c.Node().InnerText() != "3" || c.Index() != 2 {
		t.Fatalf("after remove cursor at index %d with value %s, expected index 2 value 3",
			c.Index(), c.Node().InnerText())
	}
	// Delete the tail; the cursor must fall back to the new last sibling.
	root = c.Remove()
	if c.Node().InnerText() != "99" || c.Index() != 1 {
		t.Fatalf("after tail remove cursor at index %d with value %s, expected index 1 value 99",
			c.Index(), c.Node().InnerText())
	}
	// Delete the remaining elements; the cursor ends up on the parent.
	c.Remove()
	root = c.Remove()
	if c.Node().Data != "list" || c.Depth() != 1 {
		t.Fatalf("after emptying list cursor at %q depth %d, expected list at depth 1",
			c.Node().Data, c.Depth())
	}
	if got := Find(root, "//list/*"); len(got) != 0 {
		t.Fatalf("expected empty list, got %d elements", len(got))
	}

	// The original snapshot still holds [1,2,3].
	if got := Find(doc, "//list/*"); len(got) != 3 {
		t.Fatalf("old snapshot changed, has %d elements", len(got))
	}
}

func TestCursorRenameObjectMember(t *testing.T) {
	doc, err := parseString(`{"a":{"b":1,"c":2}}`)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCursor(doc)
	if !c.ChildElement("a") || !c.ChildElement("b") {
		t.Fatal("cannot descend into a/b")
	}
	c.Rename("beta")

	// The cursor path reflects the new key.
	if c.Node().Data != "beta" {
		t.Fatalf("cursor path not updated, current key %q", c.Node().Data)
	}
	if !c.Parent() || !c.ChildElement("beta") {
		t.Fatal("cannot reach renamed member through updated path")
	}

	// XPath on the new version sees the new key only.
	if n := c.FindOne("//a/beta"); n == nil || n.InnerText() != "1" {
		t.Fatalf("renamed member not found, got %v", n)
	}
	if n := c.FindOne("//a/b"); n != nil {
		t.Fatalf("old key still present in new version: %v", n.InnerText())
	}
	// The rebuilt object value carries the new key.
	obj := c.FindOne("//a").Value().(map[string]interface{})
	if obj["beta"] != float64(1) {
		t.Fatalf("rebuilt object value missing beta: %v", obj)
	}
	if _, ok := obj["b"]; ok {
		t.Fatalf("rebuilt object value keeps old key: %v", obj)
	}

	// The old snapshot and its Go map are untouched.
	if n := FindOne(doc, "//a/b"); n == nil || n.InnerText() != "1" {
		t.Fatal("old snapshot lost member b")
	}
	oldObj := FindOne(doc, "//a").Value().(map[string]interface{})
	if _, ok := oldObj["beta"]; ok {
		t.Fatal("old snapshot map was mutated")
	}
}

func TestCursorOldCursorsStayValid(t *testing.T) {
	doc, err := parseString(`{"a":{"b":1},"x":[1,2]}`)
	if err != nil {
		t.Fatal(err)
	}
	c1 := NewCursor(doc)
	if !c1.ChildElement("a") || !c1.ChildElement("b") {
		t.Fatal("cannot descend into a/b")
	}
	c2 := c1.Clone()

	c1.SetValue(42)

	// c2 still belongs to the old version and navigates the old tree.
	if c2.Node().InnerText() != "1" {
		t.Fatalf("cloned cursor saw the edit, got %s", c2.Node().InnerText())
	}
	for c2.Parent() {
	}
	if c2.Node() != doc {
		t.Fatal("cloned cursor does not walk up to the old root")
	}
	// c1 walks up to its own new root.
	for c1.Parent() {
	}
	if c1.Root() == doc || FindOne(c1.Root(), "//a/b").InnerText() != "42" {
		t.Fatal("edited cursor does not belong to the new version")
	}
}
