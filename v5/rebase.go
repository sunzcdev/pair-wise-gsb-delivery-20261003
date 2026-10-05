package jsonpatch

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/evanphx/json-patch/v5/internal/json"
)

// RebaseConflict describes why patchB cannot be rebased onto the result of
// patchA automatically. Operation indexes are 1-based and refer to the input
// patch arrays.
type RebaseConflict struct {
	Reason     string `json:"reason"`
	Path       string `json:"path,omitempty"`
	From       string `json:"from,omitempty"`
	AOperation int    `json:"a_operation"`
	BOperation int    `json:"b_operation"`
}

func (c *RebaseConflict) Error() string {
	if c.From != "" {
		return fmt.Sprintf("rebase conflict at operation %d: %s (path %q, from %q, conflicts with A operation %d)",
			c.BOperation, c.Reason, c.Path, c.From, c.AOperation)
	}
	return fmt.Sprintf("rebase conflict at operation %d: %s at %q (conflicts with A operation %d)",
		c.BOperation, c.Reason, c.Path, c.AOperation)
}

const (
	rebaseAncestorDeleted = "ancestor deleted by A"
	rebaseTargetDeleted   = "target deleted or replaced by A"
	rebaseTargetChanged   = "target changed by A"
	rebaseAddCollision    = "both patches add a different value at the same path"
	rebaseInvalidMove     = "move cannot be applied after A's changes"
)

// RebasePatch transforms patchB so that it applies to the document produced by
// applying patchA to original. Both input patches must individually apply to
// original. Array elements are tracked by identity, so inserts, removals, and
// moves made by patchA adjust the indexes used by patchB without retargeting
// B's operations to unrelated elements.
//
// If the patches cannot be combined safely, the returned error is
// *RebaseConflict.
func RebasePatch(original, patchAData, patchBData []byte) ([]byte, error) {
	patchA, err := DecodePatch(patchAData)
	if err != nil {
		return nil, err
	}
	patchB, err := DecodePatch(patchBData)
	if err != nil {
		return nil, err
	}
	if err := validatePatch(patchA); err != nil {
		return nil, err
	}
	if err := validatePatch(patchB); err != nil {
		return nil, err
	}

	aDoc, err := patchA.Apply(original)
	if err != nil {
		return nil, err
	}
	if _, err := patchB.Apply(original); err != nil {
		return nil, err
	}

	rb, err := newRebaser(original)
	if err != nil {
		return nil, err
	}
	if err := rb.applyA(patchA); err != nil {
		return nil, err
	}

	rebased, err := rb.rebaseB(patchB)
	if err != nil {
		return nil, err
	}

	_, err = rebased.Apply(aDoc)
	if err != nil {
		return nil, err
	}

	return json.Marshal(rebased)
}

type rbKind int

const (
	rbNull rbKind = iota
	rbScalar
	rbObject
	rbArray
)

type rbNode struct {
	id     int64
	parent int64
	kind   rbKind
	raw    json.RawMessage
	object map[string]*rbNode
	array  []*rbNode
}

func (n *rbNode) MarshalJSON() ([]byte, error) {
	switch n.kind {
	case rbObject:
		return json.Marshal(n.object)
	case rbArray:
		return json.Marshal(n.array)
	default:
		return append([]byte(nil), n.raw...), nil
	}
}

type rbDoc struct {
	root        *rbNode
	allNodes    map[int64]*rbNode
	tombstones  map[int64]bool
	deletedRoot map[int64]int64
	deletedBy   map[int64]int
	replacedBy  map[int64]int
	counter     *int64
}

type rbRef struct {
	parent  *rbNode
	node    *rbNode
	key     string
	index   int
	isArray bool
	isRoot  bool
	shift   bool
}

type rebaser struct {
	r         *rbDoc
	b         *rbDoc
	counter   *int64
	createdBy map[int64]int
	movedBy   map[int64]int
	deletedBy map[int64]int
}

func newRebaser(original []byte) (*rebaser, error) {
	if len(original) == 0 {
		return nil, ErrBadJSONDoc
	}

	counter := int64(0)
	root, err := buildRBNode(original, &counter)
	if err != nil {
		return nil, err
	}

	r := &rbDoc{
		allNodes:    map[int64]*rbNode{},
		tombstones:  map[int64]bool{},
		deletedRoot: map[int64]int64{},
		deletedBy:   map[int64]int{},
		replacedBy:  map[int64]int{},
		counter:     &counter,
	}
	r.root = root
	r.register(root, 0)

	b := &rbDoc{
		allNodes:    map[int64]*rbNode{},
		tombstones:  map[int64]bool{},
		deletedRoot: map[int64]int64{},
		deletedBy:   map[int64]int{},
		replacedBy:  map[int64]int{},
		counter:     &counter,
	}
	bRoot := cloneRBNode(root, false, &counter)
	b.root = bRoot
	b.register(bRoot, 0)

	return &rebaser{
		r:         r,
		b:         b,
		counter:   &counter,
		createdBy: map[int64]int{},
		movedBy:   map[int64]int{},
		deletedBy: map[int64]int{},
	}, nil
}

func buildRBNode(data []byte, counter *int64) (*rbNode, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value interface{}
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return buildRBValue(value, counter), nil
}

func buildRBValue(value interface{}, counter *int64) *rbNode {
	*counter++
	node := &rbNode{id: *counter}
	switch v := value.(type) {
	case nil:
		node.kind = rbNull
		node.raw = rawJSONNull
	case map[string]interface{}:
		node.kind = rbObject
		node.object = make(map[string]*rbNode, len(v))
		for key, child := range v {
			childNode := buildRBValue(child, counter)
			childNode.parent = node.id
			node.object[key] = childNode
		}
	case []interface{}:
		node.kind = rbArray
		node.array = make([]*rbNode, len(v))
		for i, child := range v {
			childNode := buildRBValue(child, counter)
			childNode.parent = node.id
			node.array[i] = childNode
		}
	default:
		raw, _ := json.Marshal(v)
		node.kind = rbScalar
		node.raw = raw
	}
	return node
}

func cloneRBNode(node *rbNode, newIDs bool, counter *int64) *rbNode {
	if node == nil {
		return nil
	}

	id := node.id
	if newIDs {
		*counter++
		id = *counter
	}

	clone := &rbNode{
		id:     id,
		parent: node.parent,
		kind:   node.kind,
		raw:    append(json.RawMessage(nil), node.raw...),
	}

	switch node.kind {
	case rbObject:
		clone.object = make(map[string]*rbNode, len(node.object))
		for key, child := range node.object {
			childClone := cloneRBNode(child, newIDs, counter)
			childClone.parent = clone.id
			clone.object[key] = childClone
		}
	case rbArray:
		clone.array = make([]*rbNode, len(node.array))
		for i, child := range node.array {
			childClone := cloneRBNode(child, newIDs, counter)
			childClone.parent = clone.id
			clone.array[i] = childClone
		}
	}

	return clone
}

func (d *rbDoc) register(node *rbNode, parent int64) {
	if node == nil {
		return
	}
	node.parent = parent
	d.allNodes[node.id] = node
	d.tombstones[node.id] = false
	d.deletedRoot[node.id] = 0
	switch node.kind {
	case rbObject:
		for _, child := range node.object {
			d.register(child, node.id)
		}
	case rbArray:
		for _, child := range node.array {
			d.register(child, node.id)
		}
	}
}

func (d *rbDoc) unregister(node *rbNode) {
	if node == nil {
		return
	}
	d.tombstones[node.id] = true
	switch node.kind {
	case rbObject:
		for _, child := range node.object {
			d.unregister(child)
		}
	case rbArray:
		for _, child := range node.array {
			d.unregister(child)
		}
	}
}

func (d *rbDoc) markDeleted(node *rbNode, root *rbNode, op int) {
	if node == nil {
		return
	}
	if root == nil {
		root = node
	}
	d.deletedRoot[node.id] = root.id
	d.deletedBy[node.id] = op
	d.unregister(node)
	switch node.kind {
	case rbObject:
		for _, child := range node.object {
			d.markDeleted(child, root, op)
		}
	case rbArray:
		for _, child := range node.array {
			d.markDeleted(child, root, op)
		}
	}
}

func splitPointer(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("invalid JSON pointer: %q", path)
	}
	parts := strings.Split(path, "/")[1:]
	for i := range parts {
		parts[i] = decodePatchKey(parts[i])
	}
	return parts, nil
}

func parseArrayIndex(token string, length int, forInsert bool) (int, error) {
	if token == "-" {
		return length, nil
	}
	idx, err := strconv.Atoi(token)
	if err != nil {
		return 0, err
	}
	if idx < 0 {
		if !SupportNegativeIndices {
			return 0, ErrInvalidIndex
		}
		limit := length
		if forInsert {
			limit++
		}
		if idx < -limit {
			return 0, ErrInvalidIndex
		}
		idx += length
		if forInsert {
			idx++
		}
	}
	if idx < 0 {
		return 0, ErrInvalidIndex
	}
	if forInsert {
		if idx > length {
			return 0, ErrInvalidIndex
		}
	} else if idx >= length {
		return 0, ErrInvalidIndex
	}
	return idx, nil
}

func (d *rbDoc) resolveExisting(path string) (*rbRef, error) {
	parts, err := splitPointer(path)
	if err != nil {
		return nil, err
	}
	ref := &rbRef{node: d.root, isRoot: true}
	for _, token := range parts {
		node := ref.node
		if node == nil || d.tombstones[node.id] {
			return nil, fmt.Errorf("%w: %s", ErrMissing, path)
		}
		switch node.kind {
		case rbObject:
			child, ok := node.object[token]
			if !ok || child == nil || d.tombstones[child.id] {
				return nil, fmt.Errorf("%w: %s", ErrMissing, path)
			}
			ref = &rbRef{parent: node, node: child, key: token}
		case rbArray:
			idx, err := parseArrayIndex(token, len(node.array), false)
			if err != nil {
				return nil, err
			}
			child := node.array[idx]
			ref = &rbRef{parent: node, node: child, key: token, index: idx, isArray: true}
		default:
			return nil, fmt.Errorf("%w: %s", ErrMissing, path)
		}
	}
	return ref, nil
}

func (d *rbDoc) resolveParent(path string, forInsert bool) (*rbRef, error) {
	parts, err := splitPointer(path)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return &rbRef{node: d.root, isRoot: true}, nil
	}
	ref := &rbRef{node: d.root, isRoot: true}
	for i, token := range parts {
		last := i == len(parts)-1
		node := ref.node
		if node == nil || d.tombstones[node.id] {
			return nil, fmt.Errorf("%w: %s", ErrMissing, path)
		}
		if last {
			switch node.kind {
			case rbObject:
				child := node.object[token]
				return &rbRef{parent: node, node: child, key: token}, nil
			case rbArray:
				idx, err := parseArrayIndex(token, len(node.array), forInsert)
				if err != nil {
					return nil, err
				}
				var child *rbNode
				if !forInsert && idx < len(node.array) {
					child = node.array[idx]
				}
				return &rbRef{parent: node, node: child, key: token, index: idx, isArray: true, shift: forInsert}, nil
			default:
				return nil, fmt.Errorf("%w: %s", ErrMissing, path)
			}
		}

		switch node.kind {
		case rbObject:
			child, ok := node.object[token]
			if !ok || child == nil || d.tombstones[child.id] {
				return nil, fmt.Errorf("%w: %s", ErrMissing, path)
			}
			ref = &rbRef{parent: node, node: child, key: token}
		case rbArray:
			idx, err := parseArrayIndex(token, len(node.array), false)
			if err != nil {
				return nil, err
			}
			ref = &rbRef{parent: node, node: node.array[idx], key: token, index: idx, isArray: true}
		default:
			return nil, fmt.Errorf("%w: %s", ErrMissing, path)
		}
	}
	return ref, nil
}

func (d *rbDoc) setRoot(node *rbNode) {
	d.root = node
}

func (d *rbDoc) removeAt(ref *rbRef) *rbNode {
	if ref.isRoot {
		node := d.root
		d.root = nil
		return node
	}
	if ref.isArray {
		node := ref.parent.array[ref.index]
		ref.parent.array = append(ref.parent.array[:ref.index], ref.parent.array[ref.index+1:]...)
		return node
	}
	node := ref.parent.object[ref.key]
	delete(ref.parent.object, ref.key)
	return node
}

func (d *rbDoc) insertAt(ref *rbRef, node *rbNode, overwrite bool) {
	if ref.isRoot {
		d.root = node
		d.register(node, 0)
		return
	}
	if ref.isArray {
		if ref.shift {
			ref.parent.array = append(ref.parent.array, nil)
			copy(ref.parent.array[ref.index+1:], ref.parent.array[ref.index:])
			ref.parent.array[ref.index] = node
		} else if ref.index < len(ref.parent.array) {
			old := ref.parent.array[ref.index]
			if overwrite {
				d.unregister(old)
			}
			ref.parent.array = append(ref.parent.array, nil)
			ref.parent.array[ref.index] = node
		} else {
			ref.parent.array = append(ref.parent.array, node)
		}
		d.register(node, ref.parent.id)
		return
	}

	old, exists := ref.parent.object[ref.key]
	if exists && overwrite {
		d.unregister(old)
	}
	ref.parent.object[ref.key] = node
	d.register(node, ref.parent.id)
}

func rbNodeFromValue(raw json.RawMessage, counter *int64) (*rbNode, error) {
	return buildRBNode(raw, counter)
}

func nodesEqual(a, b *rbNode) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func (rb *rebaser) markRDeleted(node *rbNode, op int) {
	rb.r.markDeleted(node, nil, op)
}

func (rb *rebaser) markRReplaced(node *rbNode, op int) {
	if node == nil {
		return
	}
	var mark func(*rbNode, int64)
	mark = func(n *rbNode, rootID int64) {
		if n == nil {
			return
		}
		rb.r.tombstones[n.id] = true
		rb.r.deletedRoot[n.id] = rootID
		rb.r.deletedBy[n.id] = op
		if n.id == node.id {
			rb.r.replacedBy[n.id] = op
		}
		switch n.kind {
		case rbObject:
			for _, child := range n.object {
				mark(child, rootID)
			}
		case rbArray:
			for _, child := range n.array {
				mark(child, rootID)
			}
		}
	}
	mark(node, node.id)
}

func (rb *rebaser) markSubtreeIDs(node *rbNode, target map[int64]int, op int) {
	if node == nil {
		return
	}
	target[node.id] = op
	switch node.kind {
	case rbObject:
		for _, child := range node.object {
			rb.markSubtreeIDs(child, target, op)
		}
	case rbArray:
		for _, child := range node.array {
			rb.markSubtreeIDs(child, target, op)
		}
	}
}

func (rb *rebaser) isDescendant(id, root int64) bool {
	for id != 0 {
		if id == root {
			return true
		}
		node, ok := rb.r.allNodes[id]
		if !ok {
			return false
		}
		id = node.parent
	}
	return false
}

func (rb *rebaser) latestInSubtree(root int64, sources ...map[int64]int) int {
	latest := 0
	for _, source := range sources {
		for id, op := range source {
			if id != root && rb.isDescendant(id, root) && op > latest {
				latest = op
			}
		}
	}
	return latest
}

func (rb *rebaser) latestAtOrInSubtree(root int64, sources ...map[int64]int) int {
	latest := 0
	for _, source := range sources {
		for id, op := range source {
			if (id == root || rb.isDescendant(id, root)) && op > latest {
				latest = op
			}
		}
	}
	return latest
}

func (rb *rebaser) applyA(patch Patch) error {
	for i, op := range patch {
		opNum := i + 1
		switch op.Kind() {
		case "test":
			continue
		case "add":
			if err := rb.applyAAdd(op, opNum); err != nil {
				return err
			}
		case "replace":
			if err := rb.applyAReplace(op, opNum); err != nil {
				return err
			}
		case "remove":
			path, _ := op.Path()
			ref, err := rb.r.resolveExisting(path)
			if err != nil {
				return err
			}
			node := rb.r.removeAt(ref)
			rb.markRDeleted(node, opNum)
		case "move":
			from, _ := op.From()
			path, _ := op.Path()
			sourceRef, err := rb.r.resolveExisting(from)
			if err != nil {
				return err
			}
			if isAncestorPath(from, path) {
				return fmt.Errorf("%w: cannot move path into itself", ErrInvalid)
			}
			destRef, err := rb.r.resolveParent(path, true)
			if err != nil {
				return err
			}
			source := rb.r.removeAt(sourceRef)
			if !destRef.isArray && destRef.node != nil {
				rb.markRDeleted(destRef.node, opNum)
			}
			rb.r.insertAt(destRef, source, true)
			rb.movedBy[source.id] = opNum
		case "copy":
			from, _ := op.From()
			path, _ := op.Path()
			sourceRef, err := rb.r.resolveExisting(from)
			if err != nil {
				return err
			}
			destRef, err := rb.r.resolveParent(path, true)
			if err != nil {
				return err
			}
			clone := cloneRBNode(sourceRef.node, true, rb.counter)
			if !destRef.isArray && destRef.node != nil {
				rb.markRDeleted(destRef.node, opNum)
			}
			rb.r.insertAt(destRef, clone, true)
			rb.markSubtreeIDs(clone, rb.createdBy, opNum)
		}
	}
	return nil
}

func (rb *rebaser) applyAAdd(op Operation, opNum int) error {
	path, _ := op.Path()
	value, err := rbNodeFromValue(*op.value().raw, rb.counter)
	if err != nil {
		return err
	}
	if path == "" {
		if rb.r.root != nil {
			rb.markRDeleted(rb.r.root, opNum)
		}
		rb.r.setRoot(value)
		rb.r.register(value, 0)
		rb.markSubtreeIDs(value, rb.createdBy, opNum)
		return nil
	}

	ref, err := rb.r.resolveParent(path, true)
	if err != nil {
		return err
	}
	if ref.node != nil && !ref.isArray {
		rb.markRDeleted(ref.node, opNum)
	}
	rb.r.insertAt(ref, value, ref.node != nil && !ref.isArray)
	rb.markSubtreeIDs(value, rb.createdBy, opNum)
	return nil
}

func (rb *rebaser) applyAReplace(op Operation, opNum int) error {
	path, _ := op.Path()
	value, err := rbNodeFromValue(*op.value().raw, rb.counter)
	if err != nil {
		return err
	}

	if path == "" {
		if rb.r.root != nil && !nodesEqual(rb.r.root, value) {
			rb.markRDeleted(rb.r.root, opNum)
			rb.r.replacedBy[rb.r.root.id] = opNum
		}
		rb.r.setRoot(value)
		rb.r.register(value, 0)
		rb.markSubtreeIDs(value, rb.createdBy, opNum)
		return nil
	}

	ref, err := rb.r.resolveParent(path, false)
	if err != nil {
		return err
	}
	if ref.node == nil {
		return fmt.Errorf("replace operation does not apply: doc is missing key: %s: %w", path, ErrMissing)
	}
	if !nodesEqual(ref.node, value) {
		rb.markRReplaced(ref.node, opNum)
	}
	if ref.isArray {
		rb.r.unregister(ref.node)
		ref.parent.array[ref.index] = value
		rb.r.register(value, ref.parent.id)
		rb.markSubtreeIDs(value, rb.createdBy, opNum)
		return nil
	}
	rb.r.insertAt(&rbRef{
		parent:  ref.parent,
		node:    ref.node,
		key:     ref.key,
		index:   ref.index,
		isArray: ref.isArray,
		isRoot:  ref.isRoot,
	}, value, !nodesEqual(ref.node, value))
	rb.markSubtreeIDs(value, rb.createdBy, opNum)
	return nil
}

func isAncestorPath(from, path string) bool {
	return path == from || strings.HasPrefix(path, from+"/")
}

func encodePointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	return token
}

func buildPointer(parts []string) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteByte('/')
		b.WriteString(encodePointerToken(part))
	}
	return b.String()
}

func copyOperation(op Operation) Operation {
	out := Operation{}
	for key, value := range op {
		out[key] = value
	}
	return out
}

func opWithPath(op Operation, path string) Operation {
	out := copyOperation(op)
	raw := json.RawMessage(mustMarshalString(path))
	out["path"] = &raw
	return out
}

func opWithPathAndFrom(op Operation, path, from string) Operation {
	out := opWithPath(op, path)
	raw := json.RawMessage(mustMarshalString(from))
	out["from"] = &raw
	return out
}

func mustMarshalString(value string) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

type pairRef struct {
	parts       []string
	last        string
	bNode       *rbNode
	rNode       *rbNode
	bParent     *rbNode
	rParent     *rbNode
	bIndex      int
	isArray     bool
	parentArray bool
	exact       bool
}

func (rb *rebaser) conflict(reason, path string, aop, bop int) *RebaseConflict {
	return &RebaseConflict{Reason: reason, Path: path, AOperation: aop, BOperation: bop}
}

func (rb *rebaser) resolveTargetPair(path string, bop int) (*pairRef, *RebaseConflict) {
	parts, err := splitPointer(path)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}

	bNode := rb.b.root
	rNode := rb.r.root
	var bParent, rParent *rbNode
	lastIndex := -1
	parentArray := false
	exactDeleted := false

	for _, token := range parts {
		if bNode == nil || rb.b.tombstones[bNode.id] {
			return nil, rb.conflict(rebaseTargetDeleted, path, 0, bop)
		}
		if rNode == nil || rb.r.tombstones[rNode.id] {
			return nil, rb.conflict(rebaseAncestorDeleted, path, rb.deletedOp(rNode), bop)
		}

		switch bNode.kind {
		case rbObject:
			bChild, ok := bNode.object[token]
			if !ok || bChild == nil {
				return nil, rb.conflict(rebaseTargetDeleted, path, 0, bop)
			}
			rChild := rb.r.allNodes[bChild.id]
			if rChild == nil {
				return nil, rb.conflict(rebaseTargetDeleted, path, 0, bop)
			}
			if rb.r.tombstones[rChild.id] {
				if token == parts[len(parts)-1] {
					exactDeleted = true
					rParent = rNode
					bParent = bNode
					bNode, rNode = bChild, rChild
					continue
				}
				return nil, rb.conflict(rebaseAncestorDeleted, path, rb.r.deletedBy[rChild.id], bop)
			}
			bParent, rParent = bNode, rNode
			bNode, rNode = bChild, rChild
			exactDeleted = false
		case rbArray:
			idx, err := parseArrayIndex(token, len(bNode.array), false)
			if err != nil {
				return nil, rb.conflict(err.Error(), path, 0, bop)
			}
			if idx >= len(bNode.array) {
				return nil, rb.conflict(rebaseTargetDeleted, path, 0, bop)
			}
			bChild := bNode.array[idx]
			rChild := rb.r.allNodes[bChild.id]
			if rChild == nil {
				return nil, rb.conflict(rebaseTargetDeleted, path, 0, bop)
			}
			rMovedParent := rNode
			if actualParent := rb.r.allNodes[rChild.parent]; actualParent != nil {
				rMovedParent = actualParent
			}
			if rb.r.tombstones[bChild.id] {
				if token == parts[len(parts)-1] {
					exactDeleted = true
					rParent = rMovedParent
					bParent = bNode
					bNode, rNode = bChild, rChild
					lastIndex = idx
					parentArray = true
					continue
				}
				return nil, rb.conflict(rebaseAncestorDeleted, path, rb.r.deletedBy[bChild.id], bop)
			}
			bParent, rParent = bNode, rMovedParent
			bNode, rNode = bChild, rChild
			lastIndex = idx
			parentArray = true
			exactDeleted = false
		default:
			return nil, rb.conflict(rebaseTargetDeleted, path, 0, bop)
		}
	}

	last := ""
	if len(parts) > 0 {
		last = parts[len(parts)-1]
	}
	finalParentArray := false
	if bParent != nil {
		finalParentArray = bParent.kind == rbArray
	}
	return &pairRef{
		parts:       parts,
		last:        last,
		bNode:       bNode,
		rNode:       rNode,
		bParent:     bParent,
		rParent:     rParent,
		bIndex:      lastIndex,
		isArray:     parentArray,
		parentArray: finalParentArray,
		exact:       exactDeleted,
	}, nil
}

func (rb *rebaser) resolveInsertParent(path string, bop int) (*rbNode, *rbNode, *RebaseConflict) {
	parts, err := splitPointer(path)
	if err != nil {
		return nil, nil, rb.conflict(err.Error(), path, 0, bop)
	}
	if len(parts) == 0 {
		return nil, nil, nil
	}

	bNode := rb.b.root
	rNode := rb.r.root
	for _, token := range parts[:len(parts)-1] {
		if bNode == nil || rb.b.tombstones[bNode.id] {
			return nil, nil, rb.conflict(rebaseTargetDeleted, path, 0, bop)
		}
		if rNode == nil || rb.r.tombstones[rNode.id] {
			return nil, nil, rb.conflict(rebaseAncestorDeleted, path, rb.deletedOp(rNode), bop)
		}
		switch bNode.kind {
		case rbObject:
			bChild := bNode.object[token]
			rChild := rb.r.allNodes[bChild.id]
			if bChild == nil || rChild == nil {
				return nil, nil, rb.conflict(rebaseAncestorDeleted, path, 0, bop)
			}
			if rb.r.tombstones[rChild.id] {
				return nil, nil, rb.conflict(rebaseAncestorDeleted, path, rb.r.deletedBy[rChild.id], bop)
			}
			bNode, rNode = bChild, rChild
		case rbArray:
			idx, err := parseArrayIndex(token, len(bNode.array), false)
			if err != nil {
				return nil, nil, rb.conflict(err.Error(), path, 0, bop)
			}
			if idx >= len(bNode.array) {
				return nil, nil, rb.conflict(rebaseAncestorDeleted, path, 0, bop)
			}
			bChild := bNode.array[idx]
			rChild := rb.r.allNodes[bChild.id]
			if rChild == nil || rb.r.tombstones[bChild.id] {
				return nil, nil, rb.conflict(rebaseAncestorDeleted, path, rb.r.deletedBy[bChild.id], bop)
			}
			bNode, rNode = bChild, rChild
		default:
			return nil, nil, rb.conflict(rebaseAncestorDeleted, path, 0, bop)
		}
	}

	if bNode == nil || rb.b.tombstones[bNode.id] {
		return nil, nil, rb.conflict(rebaseTargetDeleted, path, 0, bop)
	}
	if rNode == nil || rb.r.tombstones[rNode.id] {
		return nil, nil, rb.conflict(rebaseAncestorDeleted, path, rb.deletedOp(rNode), bop)
	}
	return bNode, rNode, nil
}

func (rb *rebaser) changedWithin(node *rbNode) (int, bool) {
	op := rb.latestInSubtree(node.id, rb.createdBy, rb.movedBy, rb.r.deletedBy)
	return op, op > 0
}

func (rb *rebaser) changedAtOrWithin(node *rbNode) (int, bool) {
	op := rb.latestAtOrInSubtree(node.id, rb.createdBy, rb.r.deletedBy)
	return op, op > 0
}

func mappedArrayIndex(bArray, rArray []*rbNode, desired int, movedID int64) int {
	if desired < 0 {
		desired = 0
	}
	if desired >= len(bArray) {
		return len(rArray)
	}
	rIndex := func(id int64) int {
		for i, node := range rArray {
			if node != nil && node.id == id {
				return i
			}
		}
		return -1
	}
	for i := desired; i < len(bArray); i++ {
		node := bArray[i]
		if node == nil || node.id == movedID {
			continue
		}
		if idx := rIndex(node.id); idx >= 0 {
			return idx
		}
	}
	for i := desired - 1; i >= 0; i-- {
		node := bArray[i]
		if node == nil || node.id == movedID {
			continue
		}
		if idx := rIndex(node.id); idx >= 0 {
			return idx + 1
		}
	}
	return len(rArray)
}

func (rb *rebaser) rebaseB(patch Patch) (Patch, error) {
	out := Patch{}

	for i, op := range patch {
		bop := i + 1
		path, _ := op.Path()

		switch op.Kind() {
		case "test":
			pair, conflict := rb.resolveTargetPair(path, bop)
			if conflict != nil {
				return nil, conflict
			}
			if pair.exact {
				return nil, rb.conflict(rebaseTargetChanged, path, rb.r.deletedBy[pair.rNode.id], bop)
			}
			value, err := rbNodeFromValue(*op.value().raw, rb.counter)
			if err != nil {
				return nil, err
			}
			if !nodesEqual(pair.rNode, value) {
				return nil, rb.conflict(rebaseTargetChanged, path, rb.changedOp(pair.rNode), bop)
			}
		case "remove":
			transformed, conflict := rb.rebaseRemove(path, bop)
			if conflict != nil {
				return nil, conflict
			}
			if transformed != nil {
				out = append(out, transformed)
			}
		case "replace":
			transformed, conflict := rb.rebaseReplace(op, path, bop)
			if conflict != nil {
				return nil, conflict
			}
			if transformed != nil {
				out = append(out, transformed)
			}
		case "add":
			transformed, conflict := rb.rebaseAdd(op, path, bop)
			if conflict != nil {
				return nil, conflict
			}
			if transformed != nil {
				out = append(out, transformed)
			}
		case "move":
			from, _ := op.From()
			transformed, conflict := rb.rebaseMove(from, path, bop)
			if conflict != nil {
				return nil, conflict
			}
			if transformed != nil {
				out = append(out, transformed)
			}
		case "copy":
			from, _ := op.From()
			transformed, conflict := rb.rebaseCopy(from, path, bop)
			if conflict != nil {
				return nil, conflict
			}
			if transformed != nil {
				out = append(out, transformed)
			}
		}
	}

	return out, nil
}

func (rb *rebaser) changedOp(node *rbNode) int {
	if node == nil {
		return 0
	}
	op := rb.createdBy[node.id]
	if rb.movedBy[node.id] > op {
		op = rb.movedBy[node.id]
	}
	if rb.r.deletedBy[node.id] > op {
		op = rb.r.deletedBy[node.id]
	}
	return op
}

func (rb *rebaser) deletedOp(node *rbNode) int {
	if node == nil {
		return 0
	}
	return rb.r.deletedBy[node.id]
}

func (rb *rebaser) pathOfNode(node *rbNode) string {
	if node == nil || node == rb.r.root {
		return ""
	}

	var parts []string
	current := node
	for current != nil && current != rb.r.root {
		parent := rb.r.allNodes[current.parent]
		if parent == nil {
			break
		}
		if parent.kind == rbObject {
			for key, child := range parent.object {
				if child == current {
					parts = append([]string{key}, parts...)
					break
				}
			}
		} else {
			for i, child := range parent.array {
				if child == current {
					parts = append([]string{strconv.Itoa(i)}, parts...)
					break
				}
			}
		}
		current = parent
	}
	return buildPointer(parts)
}

func (rb *rebaser) partsOfNode(node *rbNode) []string {
	if node == nil || node == rb.r.root {
		return nil
	}

	var parts []string
	current := node
	for current != nil && current != rb.r.root {
		parent := rb.r.allNodes[current.parent]
		if parent == nil {
			break
		}
		if parent.kind == rbObject {
			for key, child := range parent.object {
				if child == current {
					parts = append([]string{key}, parts...)
					break
				}
			}
		} else {
			for i, child := range parent.array {
				if child == current {
					parts = append([]string{strconv.Itoa(i)}, parts...)
					break
				}
			}
		}
		current = parent
	}
	return parts
}

func (rb *rebaser) detachNode(doc *rbDoc, node *rbNode) {
	if node == doc.root {
		doc.root = nil
		return
	}
	parent := doc.allNodes[node.parent]
	if parent == nil {
		return
	}
	switch parent.kind {
	case rbObject:
		for key, child := range parent.object {
			if child == node {
				delete(parent.object, key)
				return
			}
		}
	case rbArray:
		for i, child := range parent.array {
			if child == node {
				parent.array = append(parent.array[:i], parent.array[i+1:]...)
				return
			}
		}
	}
}

func (rb *rebaser) replaceNodePair(pair *pairRef, value *rbNode) *rbNode {
	rb.b.unregister(pair.bNode)
	rb.r.unregister(pair.rNode)
	if len(pair.parts) == 0 {
		rb.b.setRoot(value)
		rb.b.register(value, 0)
		rValue := cloneRBNode(value, false, rb.counter)
		rb.r.setRoot(rValue)
		rb.r.register(rValue, 0)
		return rValue
	}

	rValue := cloneRBNode(value, false, rb.counter)
	if pair.parentArray {
		pair.bParent.array[pair.bIndex] = value
		rIndex := indexOfNode(pair.rParent.array, pair.rNode.id)
		pair.rParent.array[rIndex] = rValue
	} else {
		pair.bParent.object[pair.last] = value
		pair.rParent.object[pair.last] = rValue
	}
	rb.b.register(value, pair.bParent.id)
	rb.r.register(rValue, pair.rParent.id)
	return rValue
}

func indexOfNode(array []*rbNode, id int64) int {
	for i, node := range array {
		if node != nil && node.id == id {
			return i
		}
	}
	return len(array)
}

func (rb *rebaser) replaceBWithExisting(pair *pairRef, existing *rbNode) {
	value := cloneRBNode(existing, false, rb.counter)
	rb.b.unregister(pair.bNode)
	if len(pair.parts) == 0 {
		rb.b.setRoot(value)
		rb.b.register(value, 0)
		return
	}
	if pair.parentArray {
		pair.bParent.array[pair.bIndex] = value
	} else {
		pair.bParent.object[pair.last] = value
	}
	rb.b.register(value, pair.bParent.id)
}

func (rb *rebaser) rebaseRemove(path string, bop int) (Operation, *RebaseConflict) {
	pair, conflict := rb.resolveTargetPair(path, bop)
	if conflict != nil {
		return nil, conflict
	}
	if pair.exact {
		rb.detachNode(rb.b, pair.bNode)
		rb.b.unregister(pair.bNode)
		return nil, nil
	}

	if op, changed := rb.changedWithin(pair.rNode); changed {
		return nil, rb.conflict(rebaseTargetChanged, path, op, bop)
	}

	transformedPath := rb.pathOfNode(pair.rNode)
	rb.detachNode(rb.b, pair.bNode)
	rb.b.unregister(pair.bNode)
	rb.detachNode(rb.r, pair.rNode)
	rb.r.unregister(pair.rNode)
	return opWithPath(Operation{"op": stringRaw("remove")}, transformedPath), nil
}

func stringRaw(value string) *json.RawMessage {
	raw := json.RawMessage(mustMarshalString(value))
	return &raw
}

func (rb *rebaser) rebaseReplace(op Operation, path string, bop int) (Operation, *RebaseConflict) {
	pair, conflict := rb.resolveTargetPair(path, bop)
	if conflict != nil {
		return nil, conflict
	}
	if pair.exact {
		if aop := rb.r.replacedBy[pair.rNode.id]; aop > 0 {
			return nil, rb.conflict(rebaseTargetChanged, path, aop, bop)
		}
		return nil, rb.conflict(rebaseTargetDeleted, path, rb.r.deletedBy[pair.rNode.id], bop)
	}
	value, err := rbNodeFromValue(*op.value().raw, rb.counter)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}

	if nodesEqual(pair.rNode, value) {
		if !nodesEqual(pair.bNode, value) {
			rb.replaceBWithExisting(pair, pair.rNode)
		}
		return nil, nil
	}

	if op, changed := rb.changedWithin(pair.rNode); changed {
		return nil, rb.conflict(rebaseTargetChanged, path, op, bop)
	}

	if nodesEqual(pair.bNode, value) {
		return nil, nil
	}

	rValue := rb.replaceNodePair(pair, value)
	return opWithPath(op, rb.pathOfNode(rValue)), nil
}

func (rb *rebaser) rebaseAdd(op Operation, path string, bop int) (Operation, *RebaseConflict) {
	value, err := rbNodeFromValue(*op.value().raw, rb.counter)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}

	if path == "" {
		return rb.rebaseRootValue(op, path, value, bop)
	}

	parts, err := splitPointer(path)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}
	bParent, rParent, conflict := rb.resolveInsertParent(path, bop)
	if conflict != nil {
		return nil, conflict
	}

	key := parts[len(parts)-1]
	if bParent.kind == rbObject {
		return rb.addObjectValue(op, path, bParent, rParent, key, value, bop)
	}

	desired, err := parseArrayIndex(key, len(bParent.array), true)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}
	rDesired := mappedArrayIndex(bParent.array, rParent.array, desired, 0)
	bParent.array = append(bParent.array, nil)
	copy(bParent.array[desired+1:], bParent.array[desired:])
	bParent.array[desired] = value
	rb.b.register(value, bParent.id)

	rValue := cloneRBNode(value, false, rb.counter)
	rParent.array = append(rParent.array, nil)
	copy(rParent.array[rDesired+1:], rParent.array[rDesired:])
	rParent.array[rDesired] = rValue
	rb.r.register(rValue, rParent.id)

	return opWithPath(op, rb.pathOfNode(rValue)), nil
}

func (rb *rebaser) rebaseRootValue(op Operation, path string, value *rbNode, bop int) (Operation, *RebaseConflict) {
	pair, conflict := rb.resolveTargetPair("", bop)
	if conflict != nil {
		return nil, conflict
	}
	if nodesEqual(pair.rNode, value) {
		if !nodesEqual(pair.bNode, value) {
			rb.replaceBWithExisting(pair, pair.rNode)
		}
		return nil, nil
	}
	if aop, changed := rb.changedWithin(pair.rNode); changed {
		return nil, rb.conflict(rebaseTargetChanged, path, aop, bop)
	}
	rValue := rb.replaceNodePair(pair, value)
	_ = rValue
	return opWithPath(op, ""), nil
}

func (rb *rebaser) addObjectValue(op Operation, path string, bParent, rParent *rbNode, key string, value *rbNode, bop int) (Operation, *RebaseConflict) {
	bOld := bParent.object[key]
	rOld := rParent.object[key]

	if bOld != nil && rOld != nil {
		if nodesEqual(bOld, rOld) {
			rb.applyObjectOverwrite(bParent, rParent, key, value)
			rDest := rParent.object[key]
			return opWithPath(op, rb.pathOfNode(rDest)), nil
		}
		if nodesEqual(rOld, value) {
			rb.overwriteBObjectWithExisting(bParent, key, bOld, rOld)
			return nil, nil
		}
		return nil, rb.conflict(rebaseTargetChanged, path, rb.changedOp(rOld), bop)
	}

	if bOld != nil && rOld == nil {
		return nil, rb.conflict(rebaseTargetDeleted, path, rb.r.deletedBy[bOld.id], bop)
	}

	if bOld == nil && rOld != nil {
		if nodesEqual(rOld, value) {
			bParent.object[key] = cloneRBNode(rOld, false, rb.counter)
			rb.b.register(bParent.object[key], bParent.id)
			return nil, nil
		}
		return nil, rb.conflict(rebaseAddCollision, path, rb.changedOp(rOld), bop)
	}

	bParent.object[key] = value
	rb.b.register(value, bParent.id)
	rValue := cloneRBNode(value, false, rb.counter)
	rParent.object[key] = rValue
	rb.r.register(rValue, rParent.id)
	return opWithPath(op, rb.pathOfNode(rValue)), nil
}

func (rb *rebaser) applyObjectOverwrite(bParent, rParent *rbNode, key string, value *rbNode) {
	if old := bParent.object[key]; old != nil {
		rb.b.unregister(old)
	}
	if old := rParent.object[key]; old != nil {
		rb.r.unregister(old)
	}
	bParent.object[key] = value
	rb.b.register(value, bParent.id)
	rValue := cloneRBNode(value, false, rb.counter)
	rParent.object[key] = rValue
	rb.r.register(rValue, rParent.id)
}

func (rb *rebaser) overwriteBObjectWithExisting(bParent *rbNode, key string, bOld, existing *rbNode) {
	value := cloneRBNode(existing, false, rb.counter)
	if bOld != nil {
		rb.b.unregister(bOld)
	}
	bParent.object[key] = value
	rb.b.register(value, bParent.id)
}

func (rb *rebaser) rebaseMove(from, path string, bop int) (Operation, *RebaseConflict) {
	pair, conflict := rb.resolveTargetPair(from, bop)
	if conflict != nil {
		return nil, conflict
	}
	if pair.exact {
		return nil, &RebaseConflict{Reason: rebaseTargetDeleted, Path: path, From: from, AOperation: rb.r.deletedBy[pair.rNode.id], BOperation: bop}
	}
	if path == "" || isAncestorPath(from, path) {
		return nil, rb.conflict(rebaseInvalidMove, path, 0, bop)
	}
	if aop, changed := rb.changedWithin(pair.rNode); changed {
		return nil, &RebaseConflict{Reason: rebaseTargetChanged, Path: path, From: from, AOperation: aop, BOperation: bop}
	}

	parts, err := splitPointer(path)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}
	bDestParent, rDestParent, conflict := rb.resolveInsertParent(path, bop)
	if conflict != nil {
		return nil, conflict
	}
	if rDestParent.kind != bDestParent.kind {
		return nil, rb.conflict(rebaseInvalidMove, path, 0, bop)
	}

	bSource := pair.bNode
	rSource := pair.rNode
	transformedFrom := rb.pathOfNode(rSource)
	key := parts[len(parts)-1]
	if bDestParent.kind == rbObject {
		return rb.moveObject(transformedFrom, path, bSource, rSource, bDestParent, rDestParent, key, bop)
	}

	desiredLength := len(bDestParent.array)
	desired, err := parseArrayIndex(key, desiredLength, true)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}

	parentParts := rb.parentPointerParts(rDestParent)
	rInsertionIndex := desired
	if desired == len(bDestParent.array) {
		rInsertionIndex = len(rDestParent.array)
	} else if desired < len(bDestParent.array) {
		anchor := bDestParent.array[desired]
		if mapped := indexOfNode(rDestParent.array, anchor.id); mapped < len(rDestParent.array) {
			rInsertionIndex = mapped
		} else {
			rInsertionIndex = mappedArrayIndex(bDestParent.array, rDestParent.array, desired, rSource.id)
		}
	}

	rb.detachNode(rb.b, bSource)
	rb.detachNode(rb.r, rSource)

	bDestParent.array = insertRBArrayNode(bDestParent.array, desired, bSource)
	rDestParent.array = insertRBArrayNode(rDestParent.array, rInsertionIndex, rSource)
	rb.b.register(bSource, bDestParent.id)
	rb.r.register(rSource, rDestParent.id)

	transformedPath := buildPointer(append(append([]string{}, parentParts...), strconv.Itoa(rInsertionIndex)))
	return opWithPathAndFrom(Operation{"op": stringRaw("move")}, transformedPath, transformedFrom), nil
}

func insertRBArrayNode(array []*rbNode, index int, node *rbNode) []*rbNode {
	if index == len(array) {
		return append(array, node)
	}
	array = append(array, nil)
	copy(array[index+1:], array[index:])
	array[index] = node
	return array
}

func (rb *rebaser) parentPointerParts(parent *rbNode) []string {
	return rb.partsOfNode(parent)
}

func (rb *rebaser) moveObject(from, path string, bSource, rSource, bParent, rParent *rbNode, key string, bop int) (Operation, *RebaseConflict) {
	bOld := bParent.object[key]
	rOld := rParent.object[key]

	if bOld == bSource && rOld == rSource {
		return nil, nil
	}
	if bOld != nil && rOld != nil {
		if nodesEqual(bOld, rOld) {
			if nodesEqual(rOld, rSource) {
				rb.detachNode(rb.b, bSource)
				rb.b.unregister(bSource)
				rb.overwriteBObjectWithExisting(bParent, key, bOld, rOld)
				return nil, nil
			}
			return nil, rb.conflict(rebaseTargetChanged, path, rb.changedOp(rOld), bop)
		}
		return nil, rb.conflict(rebaseTargetChanged, path, rb.changedOp(rOld), bop)
	}
	if bOld != nil && rOld == nil {
		return nil, rb.conflict(rebaseTargetDeleted, path, rb.r.deletedBy[bOld.id], bop)
	}
	if bOld == nil && rOld != nil {
		if nodesEqual(rOld, rSource) {
			rb.detachNode(rb.b, bSource)
			rb.b.unregister(bSource)
			bParent.object[key] = cloneRBNode(rOld, false, rb.counter)
			rb.b.register(bParent.object[key], bParent.id)
			return nil, nil
		}
		return nil, rb.conflict(rebaseAddCollision, path, rb.changedOp(rOld), bop)
	}

	sourcePath := from
	rb.detachNode(rb.b, bSource)
	rb.detachNode(rb.r, rSource)
	bParent.object[key] = bSource
	rParent.object[key] = rSource
	rb.b.register(bSource, bParent.id)
	rb.r.register(rSource, rParent.id)
	destinationPath := buildPointer(append(append([]string{}, rb.partsOfNode(rParent)...), key))
	return opWithPathAndFrom(Operation{"op": stringRaw("move")}, destinationPath, sourcePath), nil
}

func (rb *rebaser) rebaseCopy(from, path string, bop int) (Operation, *RebaseConflict) {
	pair, conflict := rb.resolveTargetPair(from, bop)
	if conflict != nil {
		return nil, conflict
	}
	if pair.exact {
		return nil, &RebaseConflict{Reason: rebaseTargetDeleted, Path: path, From: from, AOperation: rb.r.deletedBy[pair.rNode.id], BOperation: bop}
	}
	if aop, changed := rb.changedWithin(pair.rNode); changed {
		return nil, &RebaseConflict{Reason: rebaseTargetChanged, Path: path, From: from, AOperation: aop, BOperation: bop}
	}

	parts, err := splitPointer(path)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}
	bParent, rParent, conflict := rb.resolveInsertParent(path, bop)
	if conflict != nil {
		return nil, conflict
	}
	key := parts[len(parts)-1]
	transformedFrom := rb.pathOfNode(pair.rNode)
	parentParts := rb.partsOfNode(rParent)
	transformedPath := buildPointer(append(append([]string{}, parentParts...), key))

	if bParent.kind == rbObject {
		bOld := bParent.object[key]
		rOld := rParent.object[key]
		if bOld != nil || rOld != nil {
			if bOld != nil && rOld != nil && nodesEqual(bOld, rOld) {
				clone := cloneRBNode(pair.bNode, true, rb.counter)
				rClone := cloneRBNode(pair.rNode, true, rb.counter)
				rb.b.unregister(bOld)
				rb.r.unregister(rOld)
				bParent.object[key] = clone
				rParent.object[key] = rClone
				rb.b.register(clone, bParent.id)
				rb.r.register(rClone, rParent.id)
				return opWithPathAndFrom(copyOperation(Operation{"op": stringRaw("copy")}), transformedPath, transformedFrom), nil
			}
			if rOld != nil && nodesEqual(rOld, pair.rNode) {
				bParent.object[key] = cloneRBNode(rOld, false, rb.counter)
				rb.b.register(bParent.object[key], bParent.id)
				return nil, nil
			}
			aop := rb.changedOp(rOld)
			if aop == 0 && rOld != nil {
				aop = 1
			}
			reason := rebaseAddCollision
			if bOld != nil && rOld == nil {
				reason = rebaseTargetDeleted
				aop = rb.r.deletedBy[bOld.id]
			}
			return nil, rb.conflict(reason, path, aop, bop)
		}

		clone := cloneRBNode(pair.bNode, true, rb.counter)
		rClone := cloneRBNode(pair.rNode, true, rb.counter)
		bParent.object[key] = clone
		rParent.object[key] = rClone
		rb.b.register(clone, bParent.id)
		rb.r.register(rClone, rParent.id)
		return opWithPathAndFrom(copyOperation(Operation{"op": stringRaw("copy")}), transformedPath, transformedFrom), nil
	}

	desired, err := parseArrayIndex(key, len(bParent.array), true)
	if err != nil {
		return nil, rb.conflict(err.Error(), path, 0, bop)
	}
	rDesired := mappedArrayIndex(bParent.array, rParent.array, desired, 0)
	arrayPath := buildPointer(append(append([]string{}, parentParts...), strconv.Itoa(rDesired)))
	clone := cloneRBNode(pair.bNode, true, rb.counter)
	rClone := cloneRBNode(pair.rNode, true, rb.counter)
	bParent.array = append(bParent.array, nil)
	copy(bParent.array[desired+1:], bParent.array[desired:])
	bParent.array[desired] = clone
	rb.b.register(clone, bParent.id)
	rParent.array = append(rParent.array, nil)
	copy(rParent.array[rDesired+1:], rParent.array[rDesired:])
	rParent.array[rDesired] = rClone
	rb.r.register(rClone, rParent.id)
	return opWithPathAndFrom(copyOperation(Operation{"op": stringRaw("copy")}), arrayPath, transformedFrom), nil
}
