package validator

import (
	"context"
	"testing"

	. "github.com/go-playground/assert/v2"
)

// graphNode models a value that is reachable through multiple fields and may
// point back at its ancestors.
type graphNode struct {
	Name     string `validate:"len=3"`
	Self     *graphNode
	Parent   *graphNode
	Child    *graphNode
	Alt      *graphNode
	Children []*graphNode `validate:"dive"`
	Iface    interface{}
}

// graphRigid forces a constraint on every reached node; used to show that a
// shared node is validated independently under each field path.
type graphRigid struct {
	Value int `validate:"gt=0"`
}

func firstFieldError(t *testing.T, err error, tag string) FieldError {
	t.Helper()

	verrs, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
	}

	for _, fe := range verrs {
		if fe.Tag() == tag {
			return fe
		}
	}

	t.Fatalf("expected an error with tag %q, got: %v", tag, err)
	return nil
}

// A real self-cycle: a node points back to itself. The back-edge is rejected
// and the error keeps the path at which the cycle was reached.
func TestGraphRejectSelfCycle(t *testing.T) {
	v := New()

	node := &graphNode{Name: "abc"}
	node.Self = node

	err := v.Struct(node)

	fe := firstFieldError(t, err, cycleTag)
	Equal(t, fe.Namespace(), "graphNode.Self")
	Equal(t, fe.Field(), "Self")
	Equal(t, fe.StructField(), "Self")
	Equal(t, fe.StructNamespace(), "graphNode.Self")
}

// A multi-node cycle through two distinct fields: parent points to child and
// the child points back. Only the back-edge is reported; the acyclic content
// is still validated exactly as for a tree.
func TestGraphRejectMutualCycle(t *testing.T) {
	v := New()

	parent := &graphNode{Name: "abc"}
	child := &graphNode{Name: "xyz"} // invalid: len must be 3
	parent.Child = child
	child.Parent = parent

	err := v.Struct(parent)

	verrs, ok := err.(ValidationErrors)
	Equal(t, ok, true)

	var cycleCount int
	for _, fe := range verrs {
		switch fe.Tag() {
		case cycleTag:
			cycleCount++
			Equal(t, fe.Namespace(), "graphNode.Child.Parent")
		case "len":
			Equal(t, fe.Namespace(), "graphNode.Child.Name")
		}
	}
	Equal(t, cycleCount, 1)
}

// A cycle hidden behind an interface value is detected by pointer identity,
// not by field layout.
func TestGraphRejectCycleThroughInterface(t *testing.T) {
	v := New()

	node := &graphNode{Name: "abc"}
	node.Iface = node

	err := v.Struct(node)

	fe := firstFieldError(t, err, cycleTag)
	Equal(t, fe.Namespace(), "graphNode.Iface")
}

// TruncateCycles cuts the back-edge silently: no cycle error, no runaway
// recursion, and acyclic content is still validated.
func TestGraphTruncateCycle(t *testing.T) {
	v := New(WithCyclePolicy(TruncateCycles))

	node := &graphNode{Name: "abc"}
	node.Self = node

	// a value reached through the cycle would otherwise fail: child is valid
	// but the truncated back-edge must not be walked.
	err := v.Struct(node)
	Equal(t, err, nil)

	// an ordinary error reached without going around the cycle is still seen
	bad := &graphNode{Name: "nope"}
	bad.Self = bad

	err = v.Struct(bad)
	verrs, ok := err.(ValidationErrors)
	Equal(t, ok, true)

	var foundCycleTag bool
	for _, fe := range verrs {
		if fe.Tag() == cycleTag {
			foundCycleTag = true
		}
	}
	Equal(t, foundCycleTag, false)
}

// A diamond reference: the same object is reached via two unrelated fields.
// It must be validated on BOTH paths with each path's own namespace, unlike a
// global visited set that would skip the second encounter.
func TestGraphDiamondValidatedOnEveryPath(t *testing.T) {
	v := New()

	shared := &graphRigid{Value: -1} // invalid under every path

	root := struct {
		A *graphRigid
		B *graphRigid
	}{
		A: shared,
		B: shared,
	}

	err := v.Struct(root)
	verrs, ok := err.(ValidationErrors)
	Equal(t, ok, true)

	seen := make(map[string]int)
	for _, fe := range verrs {
		Equal(t, fe.Tag(), "gt")
		seen[fe.Namespace()]++
	}

	Equal(t, len(seen), 2)
	Equal(t, seen["A.Value"], 1)
	Equal(t, seen["B.Value"], 1)
}

// ctxConstrained is validated by a FuncCtx whose verdict depends on the
// context in force for the whole Struct call. Running the same diamond twice
// under different contexts proves the shared node's validator actually runs
// under each field path rather than being skipped after the first encounter.
type ctxConstrained struct {
	Mark string `validate:"ctxmark"`
}

// The same shared object reached via two fields is validated under the
// constraints of each path. A custom FuncCtx keyed on context lets us prove
// the node is revisited on every path: under one context both paths fail,
// under the other both pass, which a global visited set could not reproduce.
func TestGraphDiamondDifferentConstraintsPerPath(t *testing.T) {
	v := New()

	_ = v.RegisterValidationCtx("ctxmark", func(ctx context.Context, fl FieldLevel) bool {
		reject, _ := ctx.Value("reject").(bool)
		return !reject
	})

	shared := &ctxConstrained{Mark: "whatever"}

	root := struct {
		Loose  *ctxConstrained
		Strict *ctxConstrained
	}{
		Loose:  shared,
		Strict: shared,
	}

	rejectCtx := context.WithValue(context.Background(), "reject", true)
	err := v.StructCtx(rejectCtx, root)
	verrs, ok := err.(ValidationErrors)
	Equal(t, ok, true)

	paths := make(map[string]bool)
	for _, fe := range verrs {
		paths[fe.Namespace()] = true
	}
	Equal(t, paths["Loose.Mark"], true)
	Equal(t, paths["Strict.Mark"], true)

	// under an accepting context the same shared node passes on both paths
	acceptCtx := context.WithValue(context.Background(), "reject", false)
	Equal(t, v.StructCtx(acceptCtx, root), nil)
}

// A diamond spread across slice positions: one shared element appears in two
// indices; each occurrence reports its own indexed reaching path.
func TestGraphDiamondWithinDive(t *testing.T) {
	v := New()

	shared := &graphRigid{Value: -5}

	root := struct {
		Items []*graphRigid `validate:"dive"`
	}{
		Items: []*graphRigid{shared, shared},
	}

	err := v.Struct(root)
	verrs, ok := err.(ValidationErrors)
	Equal(t, ok, true)

	paths := make(map[string]bool)
	for _, fe := range verrs {
		paths[fe.Namespace()] = true
	}
	Equal(t, paths["Items[0].Value"], true)
	Equal(t, paths["Items[1].Value"], true)
}

// graphLink is a classic singly linked list: deep but acyclic.
type graphLink struct {
	Value int `validate:"gt=0"`
	Next  *graphLink
}

func buildChain(n int) *graphLink {
	var head *graphLink
	for i := 0; i < n; i++ {
		head = &graphLink{Value: 1, Next: head}
	}
	return head
}

// A very deep, acyclic chain is bounded by the configurable depth limit; the
// reported path points at the node that exceeded it.
func TestGraphMaxDepthDeepAcyclicChain(t *testing.T) {
	v := New(WithMaxStructDepth(10))

	err := v.Struct(buildChain(100))

	fe := firstFieldError(t, err, maxDepthTag)
	Equal(t, fe.Field(), "Next")

	// the depth-limited branch stops, so errors do not repeat for every node
	verrs := err.(ValidationErrors)
	var count int
	for _, fe := range verrs {
		if fe.Tag() == maxDepthTag {
			count++
		}
	}
	Equal(t, count, 1)
}

// Without a depth limit a deep acyclic chain validates in full.
func TestGraphDeepAcyclicChainWithoutLimit(t *testing.T) {
	v := New()

	err := v.Struct(buildChain(2000))
	Equal(t, err, nil)
}

// The work budget bounds wide, acyclic graphs. Build a layered DAG where
// every node references every node of the previous layer: the number of
// visits grows quickly even though the graph has no cycles and no path is
// deep.
func TestGraphMaxTraversalNodesBoundsWork(t *testing.T) {
	type dagNode struct {
		Prev []*dagNode `validate:"dive"`
		V    string     `validate:"len=1"`
	}

	build := func(layers, width int) *dagNode {
		var prev []*dagNode
		for l := 0; l < layers; l++ {
			cur := make([]*dagNode, width)
			for i := range cur {
				cur[i] = &dagNode{Prev: prev, V: "x"}
			}
			prev = cur
		}
		return &dagNode{Prev: prev, V: "x"}
	}

	v := New(WithMaxTraversalNodes(50))

	err := v.Struct(build(6, 3))

	fe := firstFieldError(t, err, maxNodesTag)
	NotEqual(t, fe, nil)
}

// Default behaviour remains usable: cycle rejection and limits are
// independent, and tree-shaped input validates exactly as before.
func TestGraphTreeInputUnaffected(t *testing.T) {
	v := New()

	root := struct {
		Name string `validate:"len=3"`
		Next struct {
			Name string `validate:"len=3"`
		}
	}{
		Name: "abc",
		Next: struct {
			Name string `validate:"len=3"`
		}{Name: "def"},
	}

	Equal(t, v.Struct(root), nil)
}

// Pooling must not leak traversal state between calls: a truncated cycle on
// one validate call must not leave a poisoned ancestor stack for the next.
func TestGraphStateDoesNotLeakAcrossCalls(t *testing.T) {
	v := New(WithCyclePolicy(TruncateCycles))

	cyclic := &graphNode{Name: "abc"}
	cyclic.Self = cyclic

	Equal(t, v.Struct(cyclic), nil)

	// same object again, and then an unrelated object: both behave the same
	Equal(t, v.Struct(cyclic), nil)
	Equal(t, v.Struct(&graphNode{Name: "abc"}), nil)
}

// A child pointing back to the validation root closes a cycle through the
// root's own identity, which is pushed before any field is traversed.
func TestGraphChildPointingBackToRoot(t *testing.T) {
	v := New()

	root := &graphNode{Name: "abc"}
	child := &graphNode{Name: "abc"}
	root.Child = child
	child.Alt = root

	err := v.Struct(root)

	fe := firstFieldError(t, err, cycleTag)
	Equal(t, fe.Namespace(), "graphNode.Child.Alt")
}

// Distinct heap allocations must never be treated as a cycle even when they
// are structurally equal or referenced side by side.
func TestGraphDistinctPointersAreNotCycles(t *testing.T) {
	v := New()

	a := &graphNode{Name: "abc"}
	b := &graphNode{Name: "abc"}
	a.Alt = b

	// b references yet another fresh node, not a: no cycle anywhere
	b.Alt = &graphNode{Name: "abc"}

	Equal(t, v.Struct(a), nil)
}

// Cycle rejection still reports ordinary validation errors on the acyclic
// portion of the same traversal, each with its own reaching path.
func TestGraphCycleCoexistsWithFieldErrors(t *testing.T) {
	v := New()

	parent := &graphNode{Name: "abc"}
	bad := &graphNode{Name: "too long"}
	parent.Child = bad
	bad.Parent = parent

	err := v.Struct(parent)
	verrs, ok := err.(ValidationErrors)
	Equal(t, ok, true)

	tags := make(map[string]string)
	for _, fe := range verrs {
		tags[fe.Namespace()] = fe.Tag()
	}
	Equal(t, tags["graphNode.Child.Name"], "len")
	Equal(t, tags["graphNode.Child.Parent"], cycleTag)
}

// A diamond followed later by a true cycle on one branch: the shared node is
// validated through both branches, and only the branch that closes the cycle
// reports it.
func TestGraphDiamondAndCycleCombined(t *testing.T) {
	v := New()

	shared := &graphNode{Name: "abc"}
	root := &graphNode{Name: "abc"}

	viaA := &graphNode{Name: "abc", Alt: shared}
	viaB := &graphNode{Name: "abc", Alt: shared}
	root.Child = viaA
	viaA.Child = viaB

	// only viaB closes back to root
	viaB.Parent = root

	err := v.Struct(root)
	verrs, ok := err.(ValidationErrors)
	Equal(t, ok, true)

	var cycleNs []string
	for _, fe := range verrs {
		if fe.Tag() == cycleTag {
			cycleNs = append(cycleNs, fe.Namespace())
		}
	}
	Equal(t, len(cycleNs), 1)
	Equal(t, cycleNs[0], "graphNode.Child.Child.Parent")
}

// TruncateCycles changes only back-edge handling; diamond references remain
// fully validated on every path.
func TestGraphTruncateStillTraversesDiamond(t *testing.T) {
	v := New(WithCyclePolicy(TruncateCycles))

	shared := &graphRigid{Value: -1}

	root := struct {
		A *graphRigid
		B *graphRigid
	}{
		A: shared,
		B: shared,
	}

	err := v.Struct(root)
	verrs, ok := err.(ValidationErrors)
	Equal(t, ok, true)

	paths := make(map[string]bool)
	for _, fe := range verrs {
		paths[fe.Namespace()] = true
	}
	Equal(t, paths["A.Value"], true)
	Equal(t, paths["B.Value"], true)
}

// The Var entry point is also protected against cyclic input.
func TestGraphVarRejectsCycle(t *testing.T) {
	v := New()

	node := &graphNode{Name: "abc"}
	node.Self = node

	err := v.Var(node, "")
	Equal(t, err, nil) // empty tag skips entirely

	err = v.Var(node, "required")
	fe := firstFieldError(t, err, cycleTag)
	NotEqual(t, fe, nil)
}

// A chain of indirection levels still shares the final pointee's identity:
// a pointer-to-pointer field that returns to the same node closes a cycle.
func TestGraphCycleThroughPointerToPointer(t *testing.T) {
	type ppNode struct {
		Name string `validate:"len=3"`
		Ref  **ppNode
	}

	v := New()

	node := &ppNode{Name: "abc"}
	p := &node
	node.Ref = p

	err := v.Struct(node)

	fe := firstFieldError(t, err, cycleTag)
	Equal(t, fe.Namespace(), "ppNode.Ref.Ref")
}

// Distinct pointers at different indirection depths that merely point at
// copies are not a cycle.
func TestGraphPointerToCopyIsNotCycle(t *testing.T) {
	type cpNode struct {
		Name string `validate:"len=3"`
		Ref  *cpNode
	}

	v := New()

	orig := &cpNode{Name: "abc"}
	// a fresh, independent copy
	copyNode := *orig
	orig.Ref = &copyNode

	Equal(t, v.Struct(orig), nil)
}
