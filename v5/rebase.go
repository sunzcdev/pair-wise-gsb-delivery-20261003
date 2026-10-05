package jsonpatch

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"strconv"
	"strings"

	json "github.com/evanphx/json-patch/v5/internal/json"
)

// RebaseConflict describes one B operation that cannot be rebased safely.
// Operation indexes are zero-based and refer to the positions in the input patches.
type RebaseConflict struct {
	Reason          string
	Path            string
	From            string
	AOperationIndex int
	BOperationIndex int
}

// ConflictError is returned when patchB contains operations that require manual resolution.
type ConflictError struct {
	Conflicts []RebaseConflict
}

func (e *ConflictError) Error() string {
	if len(e.Conflicts) == 0 {
		return "patch rebase conflict"
	}

	c := e.Conflicts[0]
	if c.From != "" {
		return fmt.Sprintf("patch rebase conflict at B operation %d (%s from %s to %s): %s", c.BOperationIndex, c.Reason, c.From, c.Path, c.Reason)
	}

	return fmt.Sprintf("patch rebase conflict at B operation %d (%s %s): %s", c.BOperationIndex, c.Reason, c.Path, c.Reason)
}

// RebasePatch transforms patchB so that it applies to the document produced by
// applying patchA to originalJSON. Array indexes are followed by element
// identity, so unrelated inserts, deletes and moves made by patchA do not make
// B point at the wrong element.
func RebasePatch(originalJSON, patchAJSON, patchBJSON []byte) (Patch, error) {
	patchA, err := DecodePatch(patchAJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid patch A: %w", err)
	}

	patchB, err := DecodePatch(patchBJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid patch B: %w", err)
	}

	return patchB.Rebase(originalJSON, patchA)
}

// Rebase transforms the receiver from the common original document to the
// document produced by patchA.
func (patchB Patch) Rebase(originalJSON []byte, patchA Patch) (Patch, error) {
	if !json.Valid(originalJSON) {
		return nil, ErrInvalid
	}

	if _, err := patchA.Apply(originalJSON); err != nil {
		return nil, fmt.Errorf("patch A does not apply to original: %w", err)
	}

	if _, err := patchB.Apply(originalJSON); err != nil {
		return nil, fmt.Errorf("patch B does not apply to original: %w", err)
	}

	var original any
	originalDecoder := stdjson.NewDecoder(bytes.NewReader(originalJSON))
	originalDecoder.UseNumber()
	if err := originalDecoder.Decode(&original); err != nil {
		return nil, err
	}

	state := newRebaseState()
	state.b.root, state.a.root = decodePairedIDValue(original, state, nil, nil)
	state.survey = true

	for i, op := range patchA {
		if err := state.applyA(i, op); err != nil {
			return nil, fmt.Errorf("patch A does not apply to original: %w", err)
		}
	}
	for i, op := range patchB {
		_, conflict := state.applyB(i, op)
		if conflict != nil {
			state.conflicts = append(state.conflicts, *conflict)
		}
	}
	if len(state.conflicts) > 0 {
		return nil, &ConflictError{Conflicts: state.conflicts}
	}

	state = newRebaseState()
	state.b.root, state.a.root = decodePairedIDValue(original, state, nil, nil)
	for i, op := range patchA {
		if err := state.applyA(i, op); err != nil {
			return nil, fmt.Errorf("patch A does not apply to original: %w", err)
		}
	}
	rebased := Patch{}

	for i, op := range patchB {
		out, conflict := state.applyB(i, op)
		if conflict != nil {
			return nil, &ConflictError{Conflicts: []RebaseConflict{*conflict}}
		}

		if out != nil {
			rebased = append(rebased, out)
		}
	}

	return rebased, nil
}

func newRebaseState() *rebaseState {
	return &rebaseState{
		a:       &rebaseWorld{nodesByID: map[int]*idNode{}, removed: map[int]*removedNode{}},
		b:       &rebaseWorld{nodesByID: map[int]*idNode{}, removed: map[int]*removedNode{}},
		aliases: map[int]int{},
	}
}

type rebaseState struct {
	a         *rebaseWorld
	b         *rebaseWorld
	nextID    int
	aliases   map[int]int
	survey    bool
	conflicts []RebaseConflict
}

type rebaseWorld struct {
	root      *idNode
	nodesByID map[int]*idNode
	removed   map[int]*removedNode
}

type idNode struct {
	id          int
	parent      *idNode
	object      map[string]*idNode
	objectKey   []string
	array       []*idNode
	raw         json.RawMessage
	isValue     bool
	addedA      bool
	replacedA   bool
	movedA      bool
	structuralA bool
	aOp         int
}

type removedNode struct {
	node      *idNode
	opIndex   int
	exact     bool
	replacing bool
}

type nodeLocation struct {
	parent *idNode
	key    string
	index  int
	node   *idNode
	exists bool
}

func decodeIDTree(data []byte, nextID *int, nodes map[int]*idNode, parent *idNode) (*idNode, error) {
	dec := stdjson.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}

	node := decodeIDValue(value, nextID, nodes, parent)
	return node, nil
}

func decodeIDValue(value any, nextID *int, nodes map[int]*idNode, parent *idNode) *idNode {
	node := &idNode{id: *nextID, parent: parent}
	*nextID++
	nodes[node.id] = node

	switch v := value.(type) {
	case map[string]any:
		node.object = map[string]*idNode{}
		for key, childValue := range v {
			child := decodeIDValue(childValue, nextID, nodes, node)
			node.object[key] = child
			node.objectKey = append(node.objectKey, key)
		}
	case []any:
		node.array = make([]*idNode, len(v))
		for i, childValue := range v {
			node.array[i] = decodeIDValue(childValue, nextID, nodes, node)
		}
	default:
		raw, _ := stdjson.Marshal(v)
		node.raw = raw
		node.isValue = true
	}

	return node
}

func cloneIDTree(node *idNode, nextID *int, nodes map[int]*idNode, parent *idNode) *idNode {
	clone := &idNode{
		id:      *nextID,
		parent:  parent,
		raw:     append(json.RawMessage(nil), node.raw...),
		isValue: node.isValue,
		addedA:  node.addedA,
		movedA:  node.movedA,
		aOp:     node.aOp,
	}
	*nextID++
	nodes[clone.id] = clone

	if node.object != nil {
		clone.object = map[string]*idNode{}
		for _, key := range node.objectKey {
			child := cloneIDTree(node.object[key], nextID, nodes, clone)
			clone.object[key] = child
			clone.objectKey = append(clone.objectKey, key)
		}
	}

	if node.array != nil {
		clone.array = make([]*idNode, len(node.array))
		for i, child := range node.array {
			clone.array[i] = cloneIDTree(child, nextID, nodes, clone)
		}
	}

	return clone
}

func decodePairedValue(raw json.RawMessage, state *rebaseState) (*idNode, *idNode, error) {
	var value any
	dec := stdjson.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, nil, err
	}

	bNode, aNode := decodePairedIDValue(value, state, nil, nil)
	return bNode, aNode, nil
}

func decodePairedIDValue(value any, state *rebaseState, bParent, aParent *idNode) (*idNode, *idNode) {
	bNode := &idNode{id: state.nextID, parent: bParent}
	state.nextID++
	state.b.nodesByID[bNode.id] = bNode

	aNode := &idNode{id: state.nextID, parent: aParent}
	state.nextID++
	state.a.nodesByID[aNode.id] = aNode
	state.aliases[bNode.id] = aNode.id

	switch v := value.(type) {
	case map[string]any:
		bNode.object = map[string]*idNode{}
		aNode.object = map[string]*idNode{}
		for key, childValue := range v {
			bChild, aChild := decodePairedIDValue(childValue, state, bNode, aNode)
			bNode.object[key] = bChild
			bNode.objectKey = append(bNode.objectKey, key)
			aNode.object[key] = aChild
			aNode.objectKey = append(aNode.objectKey, key)
		}
	case []any:
		bNode.array = make([]*idNode, len(v))
		aNode.array = make([]*idNode, len(v))
		for i, childValue := range v {
			bChild, aChild := decodePairedIDValue(childValue, state, bNode, aNode)
			bNode.array[i] = bChild
			aNode.array[i] = aChild
		}
	default:
		raw, _ := stdjson.Marshal(v)
		bNode.raw = raw
		bNode.isValue = true
		aNode.raw = append(json.RawMessage(nil), raw...)
		aNode.isValue = true
	}

	return bNode, aNode
}

func (s *rebaseState) applyA(index int, op Operation) error {
	switch op.Kind() {
	case "add":
		return s.aAdd(index, op)
	case "remove":
		return s.aRemove(index, op)
	case "replace":
		return s.aReplace(index, op)
	case "move":
		return s.aMove(index, op)
	case "copy":
		return s.aCopy(index, op)
	case "test":
		return nil
	default:
		return fmt.Errorf("unsupported operation")
	}
}

func (s *rebaseState) aAdd(index int, op Operation) error {
	path, _ := op.Path()

	if path == "" {
		root, err := decodeIDTree(*op.value().raw, &s.nextID, s.a.nodesByID, nil)
		if err != nil {
			return err
		}
		if s.a.root != nil {
			s.markRemoved(s.a.root, index, true, false)
		}
		markAddedSubtree(root, index)
		s.a.root = root
		return nil
	}

	loc, err := locate(s.a, path)
	if err != nil {
		return err
	}

	node, err := decodeIDTree(*op.value().raw, &s.nextID, s.a.nodesByID, nil)
	if err != nil {
		return err
	}
	markAddedSubtree(node, index)

	if loc.parent.object != nil {
		if _, exists := loc.parent.object[loc.key]; exists {
			s.markRemoved(loc.parent.object[loc.key], index, false, false)
		}
		setObjectChild(loc.parent, loc.key, node)
		return nil
	}

	if loc.key == "-" {
		markContainerStructural(loc.parent, index)
		loc.parent.array = append(loc.parent.array, node)
		node.parent = loc.parent
		return nil
	}

	markContainerStructural(loc.parent, index)
	insertArrayChild(loc.parent, loc.index, node)
	return nil
}

func (s *rebaseState) aRemove(index int, op Operation) error {
	path, _ := op.Path()
	loc, err := locateExisting(s.a, path)
	if err != nil {
		return err
	}

	s.markRemoved(loc.node, index, true, false)
	if loc.parent != nil {
		markContainerStructural(loc.parent, index)
	}
	removeLocation(loc)
	return nil
}

func (s *rebaseState) aReplace(index int, op Operation) error {
	path, _ := op.Path()

	if path == "" {
		root, err := decodeIDTree(*op.value().raw, &s.nextID, s.a.nodesByID, nil)
		if err != nil {
			return err
		}
		if s.a.root != nil {
			s.markRemoved(s.a.root, index, true, true)
		}
		markReplacedSubtree(root, index)
		s.a.root = root
		s.a.removed = map[int]*removedNode{}
		return nil
	}

	loc, err := locateExisting(s.a, path)
	if err != nil {
		return err
	}

	node, err := decodeIDTree(*op.value().raw, &s.nextID, s.a.nodesByID, nil)
	if err != nil {
		return err
	}
	markReplacedSubtree(node, index)
	s.markRemoved(loc.node, index, true, true)
	replaceLocation(loc, node)
	clearRemovedSubtree(s.a.removed, node)
	return nil
}

func (s *rebaseState) aMove(index int, op Operation) error {
	from, _ := op.From()
	path, _ := op.Path()

	if path == "" {
		loc, err := locateExisting(s.a, from)
		if err != nil {
			return err
		}
		root := loc.node
		if loc.parent != nil {
			markContainerStructural(loc.parent, index)
		}
		removeLocation(loc)
		root.parent = nil
		markMovedSubtree(root, index)
		s.a.root = root
		return nil
	}

	source, err := locateExisting(s.a, from)
	if err != nil {
		return err
	}
	node := source.node
	if source.parent != nil {
		markContainerStructural(source.parent, index)
	}
	removeLocation(source)

	destination, err := locate(s.a, path)
	if err != nil {
		return err
	}

	node.parent = nil
	markMovedSubtree(node, index)

	if destination.parent.object != nil {
		if old, exists := destination.parent.object[destination.key]; exists {
			s.markRemoved(old, index, false, false)
		}
		setObjectChild(destination.parent, destination.key, node)
		return nil
	}

	if destination.key == "-" {
		markContainerStructural(destination.parent, index)
		destination.parent.array = append(destination.parent.array, node)
		node.parent = destination.parent
		return nil
	}

	markContainerStructural(destination.parent, index)
	insertArrayChild(destination.parent, destination.index, node)
	return nil
}

func (s *rebaseState) aCopy(index int, op Operation) error {
	from, _ := op.From()
	path, _ := op.Path()

	source, err := locateExisting(s.a, from)
	if err != nil {
		return err
	}
	node := cloneIDTree(source.node, &s.nextID, s.a.nodesByID, nil)
	markAddedSubtree(node, index)

	if path == "" {
		if s.a.root != nil {
			s.markRemoved(s.a.root, index, true, false)
		}
		s.a.root = node
		return nil
	}

	destination, err := locate(s.a, path)
	if err != nil {
		return err
	}

	if destination.parent.object != nil {
		if old, exists := destination.parent.object[destination.key]; exists {
			s.markRemoved(old, index, false, false)
		}
		setObjectChild(destination.parent, destination.key, node)
		return nil
	}

	if destination.key == "-" {
		markContainerStructural(destination.parent, index)
		destination.parent.array = append(destination.parent.array, node)
		node.parent = destination.parent
		return nil
	}

	markContainerStructural(destination.parent, index)
	insertArrayChild(destination.parent, destination.index, node)
	return nil
}

func (s *rebaseState) markRemoved(node *idNode, opIndex int, exact, replacing bool) {
	walkSubtree(node, func(n *idNode) {
		s.a.removed[n.id] = &removedNode{node: node, opIndex: opIndex, exact: exact, replacing: replacing}
		delete(s.a.nodesByID, n.id)
	})
}

func clearRemovedSubtree(removed map[int]*removedNode, node *idNode) {
	delete(removed, node.id)
	if node.object != nil {
		for _, key := range node.objectKey {
			clearRemovedSubtree(removed, node.object[key])
		}
	}
	for _, child := range node.array {
		clearRemovedSubtree(removed, child)
	}
}

func markAddedSubtree(node *idNode, opIndex int) {
	walkSubtree(node, func(n *idNode) {
		n.addedA = true
		n.aOp = opIndex
	})
}

func markReplacedSubtree(node *idNode, opIndex int) {
	walkSubtree(node, func(n *idNode) {
		n.replacedA = true
		n.addedA = true
		n.aOp = opIndex
	})
}

func markMovedSubtree(node *idNode, opIndex int) {
	node.movedA = true
	node.aOp = opIndex
}

func markContainerStructural(node *idNode, opIndex int) {
	if node == nil {
		return
	}
	node.structuralA = true
	node.aOp = opIndex
}

func walkSubtree(node *idNode, fn func(*idNode)) {
	if node == nil {
		return
	}

	fn(node)
	if node.object != nil {
		for _, key := range node.objectKey {
			walkSubtree(node.object[key], fn)
		}
	}
	for _, child := range node.array {
		walkSubtree(child, fn)
	}
}

func locate(world *rebaseWorld, path string) (*nodeLocation, error) {
	if path == "" {
		return &nodeLocation{node: world.root, exists: world.root != nil}, nil
	}

	parts, err := splitPointer(path)
	if err != nil {
		return nil, err
	}

	parent := world.root
	for _, part := range parts[:len(parts)-1] {
		if parent == nil || parent.array == nil && parent.object == nil {
			return nil, ErrMissing
		}

		if parent.object != nil {
			parent = parent.object[part]
		} else {
			index, err := arrayExistingIndex(part, len(parent.array))
			if err != nil {
				return nil, err
			}
			if index < 0 || index >= len(parent.array) {
				return nil, ErrMissing
			}
			parent = parent.array[index]
		}

		if parent == nil {
			return nil, ErrMissing
		}
	}

	key := parts[len(parts)-1]
	loc := &nodeLocation{parent: parent, key: key, exists: false}

	if parent.object != nil {
		loc.node, loc.exists = parent.object[key]
		return loc, nil
	}

	if parent.array == nil {
		return nil, ErrMissing
	}

	if key == "-" {
		loc.index = len(parent.array)
		return loc, nil
	}

	index, err := arrayLeafIndex(key, len(parent.array), true)
	if err != nil {
		return nil, err
	}
	loc.index = index
	if index >= 0 && index < len(parent.array) {
		loc.node = parent.array[index]
		loc.exists = true
	}
	return loc, nil
}

func arrayExistingIndex(token string, length int) (int, error) {
	return arrayIndex(token, length, false)
}

func arrayInsertIndexFromToken(token string, length int) (int, error) {
	return arrayIndex(token, length+1, true)
}

func locateExisting(world *rebaseWorld, path string) (*nodeLocation, error) {
	loc, err := locate(world, path)
	if err != nil {
		return nil, err
	}
	if !loc.exists {
		return nil, ErrMissing
	}
	return loc, nil
}

func arrayLeafIndex(token string, length int, resolveExisting bool) (int, error) {
	parsed, err := strconv.Atoi(token)
	if err != nil {
		return 0, ErrInvalidIndex
	}

	if parsed < 0 {
		if !SupportNegativeIndices || parsed < -(length+1) {
			return 0, ErrInvalidIndex
		}
		adjustedLength := length
		if !resolveExisting {
			adjustedLength = length + 1
		}
		return parsed + adjustedLength, nil
	}

	allowEnd := !resolveExisting
	return arrayIndex(token, length, allowEnd)
}

func splitPointer(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("invalid JSON pointer %q", path)
	}

	rawParts := strings.Split(path, "/")[1:]
	parts := make([]string, len(rawParts))
	for i, part := range rawParts {
		parts[i] = decodePointerToken(part)
	}
	return parts, nil
}

func decodePointerToken(token string) string {
	token = strings.ReplaceAll(token, "~1", "/")
	token = strings.ReplaceAll(token, "~0", "~")
	return token
}

func encodePointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	return token
}

func arrayIndex(token string, length int, allowEnd bool) (int, error) {
	index, err := strconv.Atoi(token)
	if err != nil {
		return 0, ErrInvalidIndex
	}

	if index < 0 {
		if !SupportNegativeIndices {
			return 0, ErrInvalidIndex
		}
		minimum := -length
		if allowEnd {
			minimum = -(length + 1)
		}
		if index < minimum {
			return 0, ErrInvalidIndex
		}
		index += length
	}

	max := length - 1
	if allowEnd {
		max = length
	}
	if index < 0 || index > max {
		return 0, ErrInvalidIndex
	}

	return index, nil
}

func setObjectChild(parent *idNode, key string, node *idNode) {
	if _, exists := parent.object[key]; !exists {
		parent.objectKey = append(parent.objectKey, key)
	}
	parent.object[key] = node
	node.parent = parent
}

func insertArrayChild(parent *idNode, index int, node *idNode) {
	parent.array = append(parent.array, nil)
	copy(parent.array[index+1:], parent.array[index:])
	parent.array[index] = node
	node.parent = parent
}

func replaceLocation(loc *nodeLocation, node *idNode) {
	if loc.parent == nil {
		node.parent = nil
		return
	}

	if loc.parent.object != nil {
		setObjectChild(loc.parent, loc.key, node)
		return
	}

	loc.parent.array[loc.index] = node
	node.parent = loc.parent
}

func removeLocation(loc *nodeLocation) {
	if loc.parent == nil {
		return
	}

	if loc.parent.object != nil {
		delete(loc.parent.object, loc.key)
		keys := make([]string, 0, len(loc.parent.objectKey))
		for _, key := range loc.parent.objectKey {
			if key != loc.key {
				keys = append(keys, key)
			}
		}
		loc.parent.objectKey = keys
		return
	}

	loc.parent.array = append(loc.parent.array[:loc.index], loc.parent.array[loc.index+1:]...)
}

func (s *rebaseState) applyB(index int, op Operation) (Operation, *RebaseConflict) {
	switch op.Kind() {
	case "add":
		return s.bAdd(index, op)
	case "remove":
		return s.bRemove(index, op)
	case "replace":
		return s.bReplace(index, op)
	case "move":
		return s.bMove(index, op)
	case "copy":
		return s.bCopy(index, op)
	case "test":
		return s.bTest(index, op)
	default:
		return s.conflict(&RebaseConflict{Reason: "unsupported operation", BOperationIndex: index})
	}
}

func (s *rebaseState) conflict(conflict *RebaseConflict) (Operation, *RebaseConflict) {
	if s.survey {
		s.conflicts = append(s.conflicts, *conflict)
		return nil, nil
	}
	return nil, conflict
}

func (s *rebaseState) bAdd(index int, op Operation) (Operation, *RebaseConflict) {
	path, _ := op.Path()

	if path == "" {
		bValue, rValue, err := decodePairedValue(*op.value().raw, s)
		if err != nil {
			return s.conflict(invalidBOperation(index, path, "", err))
		}

		if s.a.root != nil {
			if nodesEqual(s.a.root, rValue) {
				s.aliasSubtree(bValue, s.a.root)
				s.b.root = bValue
				return nil, nil
			}
			if conflict := s.bulkConflict(index, path, "", s.a.root); conflict != nil {
				return s.conflict(conflict)
			}
		}

		s.b.root = bValue
		s.a.root = rValue
		return transformValueOperation(op, "", "", rValue), nil
	}

	loc, err := locate(s.b, path)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, "", err))
	}

	bValue, rValue, err := decodePairedValue(*op.value().raw, s)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, "", err))
	}

	rParent := s.mapNode(loc.parent)
	if rParent == nil {
		return s.conflict(s.ancestorConflict(index, path, "", loc.parent))
	}

	if rParent.object != nil {
		current, exists := rParent.object[loc.key]
		if exists {
			if nodesEqual(current, rValue) {
				s.aliasSubtree(bValue, current)
				setObjectChild(loc.parent, loc.key, bValue)
				return nil, nil
			}
			if conflict := s.bulkConflict(index, path, "", current); conflict != nil {
				return s.conflict(conflict)
			}
		}

		setObjectChild(rParent, loc.key, rValue)
		setObjectChild(loc.parent, loc.key, bValue)
		return transformValueOperation(op, path, "", rValue), nil
	}

	if loc.key == "-" {
		rParent.array = append(rParent.array, rValue)
		rValue.parent = rParent
		loc.parent.array = append(loc.parent.array, bValue)
		bValue.parent = loc.parent
		return op, nil
	}

	rBoundary := s.mapArrayBoundary(loc.parent, loc.index, nil, nil, rParent)
	targetIndex := arrayInsertIndex(rParent, rBoundary)

	insertArrayChild(rParent, targetIndex, rValue)
	insertArrayChild(loc.parent, loc.index, bValue)

	if strconv.Itoa(targetIndex) == loc.key {
		return op, nil
	}
	return transformValueOperation(op, joinPointer(parentPointer(rParent), strconv.Itoa(targetIndex)), "", rValue), nil
}

func (s *rebaseState) bRemove(index int, op Operation) (Operation, *RebaseConflict) {
	path, _ := op.Path()
	loc, err := locateExisting(s.b, path)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, "", err))
	}

	rNode := s.mapNode(loc.node)
	if rNode == nil {
		removed := s.removed(loc.node.id)
		if removed != nil && removed.exact && !removed.replacing {
			removeLocation(loc)
			return nil, nil
		}
		if removed != nil && removed.exact && removed.replacing {
			return s.conflict(&RebaseConflict{
				Reason:          "A modified a value that B removes",
				Path:            path,
				AOperationIndex: removed.opIndex,
				BOperationIndex: index,
			})
		}
		return s.conflict(s.ancestorConflict(index, path, "", loc.node))
	}

	if conflict := s.modifyDeleteConflict(index, path, "", rNode); conflict != nil {
		return s.conflict(conflict)
	}

	newPath := nodePath(rNode)
	rParent := rNode.parent
	var rKey string
	var rIndex int
	if rNode.parent != nil {
		if rNode.parent.object != nil {
			rKey = objectChildKey(rNode.parent, rNode)
		} else {
			rIndex = arrayChildIndex(rNode.parent, rNode)
		}
	}

	removeLocation(&nodeLocation{parent: rParent, key: rKey, index: rIndex, node: rNode, exists: true})
	removeLocation(loc)

	if newPath == path {
		return op, nil
	}
	return transformPathOperation(op, newPath), nil
}

func (s *rebaseState) bReplace(index int, op Operation) (Operation, *RebaseConflict) {
	path, _ := op.Path()

	if path == "" {
		bValue, rValue, err := decodePairedValue(*op.value().raw, s)
		if err != nil {
			return s.conflict(invalidBOperation(index, path, "", err))
		}

		if s.a.root != nil && nodesEqual(s.a.root, rValue) {
			s.aliasSubtree(bValue, s.a.root)
			s.b.root = bValue
			return nil, nil
		}
		if conflict := s.bulkConflict(index, path, "", s.a.root); conflict != nil {
			return s.conflict(conflict)
		}

		s.a.root = rValue
		s.b.root = bValue
		return transformValueOperation(op, "", "", rValue), nil
	}

	loc, err := locateExisting(s.b, path)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, "", err))
	}
	bValue, rValue, err := decodePairedValue(*op.value().raw, s)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, "", err))
	}

	rNode := s.mapNode(loc.node)
	if rNode == nil {
		if removed := s.removed(loc.node.id); removed != nil && removed.exact && removed.replacing {
			replacement := s.a.root
			if path != "" {
				if replacementLocation, locateErr := locateExisting(s.a, path); locateErr == nil {
					replacement = replacementLocation.node
				}
			}
			if replacement != nil && nodesEqual(replacement, rValue) {
				s.aliasSubtree(bValue, replacement)
				replaceLocation(loc, bValue)
				return nil, nil
			}
		}
		return s.conflict(s.ancestorConflict(index, path, "", loc.node))
	}

	if nodesEqual(rNode, rValue) {
		s.aliasSubtree(bValue, rNode)
		replaceLocation(loc, bValue)
		return nil, nil
	}

	if conflict := s.bulkConflict(index, path, "", rNode); conflict != nil {
		return s.conflict(conflict)
	}

	newPath := nodePath(rNode)
	rParent := rNode.parent
	var rKey string
	var rIndex int
	if rParent != nil {
		if rParent.object != nil {
			rKey = objectChildKey(rParent, rNode)
		} else {
			rIndex = arrayChildIndex(rParent, rNode)
		}
	}

	replaceLocation(&nodeLocation{parent: rParent, key: rKey, index: rIndex, node: rNode, exists: true}, rValue)
	replaceLocation(loc, bValue)

	if newPath == path {
		return op, nil
	}
	return transformValueOperation(op, newPath, "", rValue), nil
}

func (s *rebaseState) bMove(index int, op Operation) (Operation, *RebaseConflict) {
	from, _ := op.From()
	path, _ := op.Path()

	source, err := locateExisting(s.b, from)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, from, err))
	}

	rSource := s.mapNode(source.node)
	if rSource == nil {
		return s.conflict(s.ancestorConflict(index, path, from, source.node))
	}

	if conflict := s.bulkMoveConflict(index, path, from, rSource); conflict != nil {
		return s.conflict(conflict)
	}
	newFrom := nodePath(rSource)

	if path == "" {
		if s.a.root != nil {
			if rSource == s.a.root {
				removeLocation(source)
				s.b.root = source.node
				source.node.parent = nil
				return nil, nil
			}
			if conflict := s.bulkConflict(index, path, from, s.a.root); conflict != nil {
				return s.conflict(conflict)
			}
		}

		rSourceParent := rSource.parent
		removeLocation(source)
		removeLocation(&nodeLocation{
			parent: rSourceParent,
			key:    objectChildKey(rSourceParent, rSource),
			index:  arrayChildIndex(rSourceParent, rSource),
			node:   rSource,
			exists: true,
		})
		s.b.root = source.node
		source.node.parent = nil
		rSource.parent = nil
		s.a.root = rSource
		return transformMoveOperation(op, "", newFrom), nil
	}

	destination, err := locate(s.b, path)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, from, err))
	}

	rDestParent := s.mapNode(destination.parent)
	if rDestParent == nil {
		return s.conflict(s.ancestorConflict(index, path, from, destination.parent))
	}

	var rBoundary arraySlot
	if destination.parent.array != nil && destination.key != "-" {
		var excludeB, excludeR *idNode
		if source.parent == destination.parent {
			excludeB = source.node
			excludeR = rSource
		}
		rBoundary = s.mapArrayBoundary(destination.parent, destination.index, excludeB, excludeR, rDestParent)
	}

	if rDestParent.object != nil {
		current, exists := rDestParent.object[destination.key]
		if exists && current != rSource {
			if conflict := s.bulkConflict(index, path, from, current); conflict != nil {
				return s.conflict(conflict)
			}
		}
	}

	rSourceParent := rSource.parent
	rSourceKey := objectChildKey(rSourceParent, rSource)
	rSourceIndex := arrayChildIndex(rSourceParent, rSource)

	removeLocation(source)
	removeLocation(&nodeLocation{parent: rSourceParent, key: rSourceKey, index: rSourceIndex, node: rSource, exists: true})
	rSource.parent = nil

	var out Operation
	if rDestParent.object != nil {
		setObjectChild(rDestParent, destination.key, rSource)
		setObjectChild(destination.parent, destination.key, source.node)
		out = transformMoveOperation(op, nodePath(rSource), newFrom)
	} else if destination.key == "-" {
		rDestParent.array = append(rDestParent.array, rSource)
		rSource.parent = rDestParent
		destination.parent.array = append(destination.parent.array, source.node)
		source.node.parent = destination.parent
		out = transformMoveOperation(op, nodePath(rSource), newFrom)
	} else {
		targetIndex := arrayInsertIndex(rDestParent, rBoundary)
		insertArrayChild(rDestParent, targetIndex, rSource)
		insertArrayChild(destination.parent, destination.index, source.node)
		out = transformMoveOperation(op, nodePath(rSource), newFrom)
	}

	return out, nil
}

func (s *rebaseState) bCopy(index int, op Operation) (Operation, *RebaseConflict) {
	from, _ := op.From()
	path, _ := op.Path()

	source, err := locateExisting(s.b, from)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, from, err))
	}
	rSource := s.mapNode(source.node)
	if rSource == nil {
		return s.conflict(s.ancestorConflict(index, path, from, source.node))
	}
	newFrom := nodePath(rSource)

	bCopy := cloneIDTree(source.node, &s.nextID, s.b.nodesByID, nil)
	rCopy := cloneIDTree(rSource, &s.nextID, s.a.nodesByID, nil)
	s.aliasSubtree(bCopy, rCopy)

	if path == "" {
		if s.a.root != nil {
			if nodesEqual(s.a.root, rCopy) {
				s.aliasSubtree(bCopy, s.a.root)
				s.b.root = bCopy
				return nil, nil
			}
			if conflict := s.bulkConflict(index, path, from, s.a.root); conflict != nil {
				return s.conflict(conflict)
			}
		}
		s.b.root = bCopy
		s.a.root = rCopy
		return transformMoveOperation(op, "", newFrom), nil
	}

	destination, err := locate(s.b, path)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, from, err))
	}
	rDestParent := s.mapNode(destination.parent)
	if rDestParent == nil {
		return s.conflict(s.ancestorConflict(index, path, from, destination.parent))
	}

	if rDestParent.object != nil {
		current, exists := rDestParent.object[destination.key]
		if exists {
			if nodesEqual(current, rCopy) {
				s.aliasSubtree(bCopy, current)
				setObjectChild(destination.parent, destination.key, bCopy)
				return nil, nil
			}
			if conflict := s.bulkConflict(index, path, from, current); conflict != nil {
				return s.conflict(conflict)
			}
		}
		setObjectChild(rDestParent, destination.key, rCopy)
		setObjectChild(destination.parent, destination.key, bCopy)
		return transformMoveOperation(op, path, newFrom), nil
	}

	if destination.key == "-" {
		rDestParent.array = append(rDestParent.array, rCopy)
		rCopy.parent = rDestParent
		destination.parent.array = append(destination.parent.array, bCopy)
		bCopy.parent = destination.parent
		return transformMoveOperation(op, path, newFrom), nil
	}

	rBoundary := s.mapArrayBoundary(destination.parent, destination.index, nil, nil, rDestParent)
	targetIndex := arrayInsertIndex(rDestParent, rBoundary)
	insertArrayChild(rDestParent, targetIndex, rCopy)
	insertArrayChild(destination.parent, destination.index, bCopy)

	if targetIndex == destination.index {
		return transformMoveOperation(op, path, newFrom), nil
	}
	return transformMoveOperation(op, joinPointer(parentPointer(rDestParent), strconv.Itoa(targetIndex)), newFrom), nil
}

func (s *rebaseState) bTest(index int, op Operation) (Operation, *RebaseConflict) {
	path, _ := op.Path()

	if path == "" {
		if s.a.root == nil {
			return s.conflict(&RebaseConflict{Reason: "test target was deleted", Path: path, BOperationIndex: index})
		}
		return s.transformTest(op, "", s.a.root), nil
	}

	loc, err := locateExisting(s.b, path)
	if err != nil {
		return s.conflict(invalidBOperation(index, path, "", err))
	}

	rNode := s.mapNode(loc.node)
	if rNode == nil {
		return s.conflict(s.ancestorConflict(index, path, "", loc.node))
	}

	newPath := nodePath(rNode)
	return s.transformTest(op, newPath, rNode), nil
}

type arraySlot struct {
	prevID  int
	nextID  int
	hasPrev bool
	hasNext bool
}

func (s *rebaseState) mapNode(node *idNode) *idNode {
	if node == nil {
		return nil
	}
	id := node.id
	if mapped, ok := s.aliases[id]; ok {
		id = mapped
	}
	return s.a.nodesByID[id]
}

func (s *rebaseState) removed(id int) *removedNode {
	if mapped, ok := s.aliases[id]; ok {
		id = mapped
	}
	return s.a.removed[id]
}

func (s *rebaseState) aliasSubtree(bNode, aNode *idNode) {
	if bNode == nil || aNode == nil {
		return
	}

	s.aliases[bNode.id] = aNode.id
	if bNode.object != nil && aNode.object != nil {
		for _, key := range bNode.objectKey {
			s.aliasSubtree(bNode.object[key], aNode.object[key])
		}
	}
	if bNode.array != nil && aNode.array != nil && len(bNode.array) == len(aNode.array) {
		for i := range bNode.array {
			s.aliasSubtree(bNode.array[i], aNode.array[i])
		}
	}
}

func (s *rebaseState) mapArrayBoundary(bArray *idNode, index int, excludeB, excludeR, rArray *idNode) arraySlot {
	filtered := make([]*idNode, 0, len(bArray.array))
	for _, node := range bArray.array {
		if node != excludeB {
			filtered = append(filtered, node)
		}
	}

	slot := arraySlot{}
	for i := index - 1; i >= 0; i-- {
		if mapped := s.mapNode(filtered[i]); mapped != nil && mapped != excludeR && containsIDNode(rArray.array, mapped) {
			slot.prevID = mapped.id
			slot.hasPrev = true
			break
		}
	}
	for i := index; i < len(filtered); i++ {
		if mapped := s.mapNode(filtered[i]); mapped != nil && mapped != excludeR && containsIDNode(rArray.array, mapped) {
			slot.nextID = mapped.id
			slot.hasNext = true
			break
		}
	}
	return slot
}

func containsIDNode(array []*idNode, target *idNode) bool {
	for _, node := range array {
		if node == target {
			return true
		}
	}
	return false
}

func arrayInsertIndex(array *idNode, slot arraySlot) int {
	if slot.hasPrev {
		for i, node := range array.array {
			if node.id == slot.prevID {
				return i + 1
			}
		}
	}
	if slot.hasNext {
		for i, node := range array.array {
			if node.id == slot.nextID {
				return i
			}
		}
	}
	return len(array.array)
}

func objectChildKey(parent, child *idNode) string {
	if parent == nil || parent.object == nil || child == nil {
		return ""
	}
	for _, key := range parent.objectKey {
		if parent.object[key] == child {
			return key
		}
	}
	return ""
}

func arrayChildIndex(parent, child *idNode) int {
	if parent == nil || parent.array == nil || child == nil {
		return -1
	}
	for i, node := range parent.array {
		if node == child {
			return i
		}
	}
	return -1
}

func nodePath(node *idNode) string {
	if node == nil || node.parent == nil {
		return ""
	}

	parent := node.parent
	if parent.object != nil {
		return joinPointer(parentPointer(parent), encodePointerToken(objectChildKey(parent, node)))
	}

	return joinPointer(parentPointer(parent), strconv.Itoa(arrayChildIndex(parent, node)))
}

func parentPointer(node *idNode) string {
	return nodePath(node)
}

func joinPointer(parent, token string) string {
	return parent + "/" + token
}

func nodesEqual(a, b *idNode) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	aRaw, err := marshalIDNode(a)
	if err != nil {
		return false
	}
	bRaw, err := marshalIDNode(b)
	if err != nil {
		return false
	}

	var aValue, bValue any
	if err := stdjson.Unmarshal(aRaw, &aValue); err != nil {
		return false
	}
	if err := stdjson.Unmarshal(bRaw, &bValue); err != nil {
		return false
	}

	return deepEqual(aValue, bValue)
}

func deepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for key, value := range av {
			other, exists := bv[key]
			if !exists || !deepEqual(value, other) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

func marshalIDNode(node *idNode) ([]byte, error) {
	if node.isValue {
		return append([]byte(nil), node.raw...), nil
	}
	if node.object != nil {
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, key := range node.objectKey {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyRaw, _ := stdjson.Marshal(key)
			buf.Write(keyRaw)
			buf.WriteByte(':')
			value, err := marshalIDNode(node.object[key])
			if err != nil {
				return nil, err
			}
			buf.Write(value)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, child := range node.array {
		if i > 0 {
			buf.WriteByte(',')
		}
		value, err := marshalIDNode(child)
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

func transformValueOperation(op Operation, path, from string, value *idNode) Operation {
	out := cloneOperation(op)
	if path != "" || op["path"] != nil {
		raw, _ := stdjson.Marshal(path)
		out["path"] = newRawMessage(raw)
	}
	if from != "" {
		raw, _ := stdjson.Marshal(from)
		out["from"] = newRawMessage(raw)
	}
	return out
}

func transformPathOperation(op Operation, path string) Operation {
	out := cloneOperation(op)
	raw, _ := stdjson.Marshal(path)
	out["path"] = newRawMessage(raw)
	return out
}

func transformMoveOperation(op Operation, path, from string) Operation {
	out := transformPathOperation(op, path)
	if from != "" {
		raw, _ := stdjson.Marshal(from)
		out["from"] = newRawMessage(raw)
	}
	return out
}

func (s *rebaseState) transformTest(op Operation, path string, node *idNode) Operation {
	out := cloneOperation(op)
	rawPath, _ := stdjson.Marshal(path)
	out["path"] = newRawMessage(rawPath)
	return out
}

func cloneOperation(op Operation) Operation {
	out := Operation{}
	for key, value := range op {
		out[key] = value
	}
	return out
}

func invalidBOperation(index int, path, from string, err error) *RebaseConflict {
	return &RebaseConflict{
		Reason:          "invalid B operation after rebase: " + err.Error(),
		Path:            path,
		From:            from,
		BOperationIndex: index,
	}
}

func (s *rebaseState) ancestorConflict(index int, path, from string, node *idNode) *RebaseConflict {
	reason := "target ancestor was deleted"
	aIndex := -1

	if removed := s.removed(node.id); removed != nil {
		aIndex = removed.opIndex
		if removed.replacing {
			reason = "target ancestor was replaced"
		}
	}

	return &RebaseConflict{
		Reason:          reason,
		Path:            path,
		From:            from,
		AOperationIndex: aIndex,
		BOperationIndex: index,
	}
}

func (s *rebaseState) bulkConflict(index int, path, from string, node *idNode) *RebaseConflict {
	changed := s.findAChange(node, false)
	if changed == nil {
		return nil
	}
	return &RebaseConflict{
		Reason:          "both patches modify the same value",
		Path:            path,
		From:            from,
		AOperationIndex: changed.aOp,
		BOperationIndex: index,
	}
}

func (s *rebaseState) bulkMoveConflict(index int, path, from string, node *idNode) *RebaseConflict {
	changed := s.findAChange(node, true)
	if changed == nil {
		return nil
	}
	return &RebaseConflict{
		Reason:          "both patches modify the same value",
		Path:            path,
		From:            from,
		AOperationIndex: changed.aOp,
		BOperationIndex: index,
	}
}

func (s *rebaseState) modifyDeleteConflict(index int, path, from string, node *idNode) *RebaseConflict {
	changed := s.findAChange(node, false)
	if changed == nil {
		return nil
	}
	return &RebaseConflict{
		Reason:          "A modified a value that B removes",
		Path:            path,
		From:            from,
		AOperationIndex: changed.aOp,
		BOperationIndex: index,
	}
}

func (s *rebaseState) findAChange(node *idNode, ignoreDirectMove bool) *idNode {
	if node == nil {
		return nil
	}

	if node.addedA || node.replacedA || node.structuralA || (node.movedA && !ignoreDirectMove) {
		return node
	}

	var changed *idNode
	walkSubtreeChildrenFirst(node, func(child *idNode) bool {
		if child == node {
			return false
		}
		if child.addedA || child.replacedA || child.structuralA || child.movedA {
			changed = child
			return true
		}
		return false
	})
	return changed
}

func walkSubtreeChildrenFirst(node *idNode, fn func(*idNode) bool) bool {
	if node.object != nil {
		for _, key := range node.objectKey {
			if walkSubtreeChildrenFirst(node.object[key], fn) {
				return true
			}
		}
	}
	for _, child := range node.array {
		if walkSubtreeChildrenFirst(child, fn) {
			return true
		}
	}
	return fn(node)
}
