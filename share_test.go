// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type sharedNode struct {
	Name  string         `cbor:"name"`
	Left  *sharedNode    `cbor:"left,omitempty"`
	Right *sharedNode    `cbor:"right,omitempty"`
	Items *[]*sharedNode `cbor:"items,omitempty"`
	Dyn   any            `cbor:"dyn,omitempty"`
}

type sharedInt struct {
	Value int `cbor:"value"`
}

func sharedTestModes(t *testing.T) (EncMode, DecMode) {
	t.Helper()
	em, err := EncOptions{SharedRef: SharedRefAllowed}.EncMode()
	if err != nil {
		t.Fatal(err)
	}
	dm, err := DecOptions{SharedRef: SharedRefAllowed}.DecMode()
	if err != nil {
		t.Fatal(err)
	}
	return em, dm
}

func TestSharedRefDiamondRestoresPointerIdentity(t *testing.T) {
	em, dm := sharedTestModes(t)

	bottom := &sharedNode{Name: "bottom"}
	root := &sharedNode{
		Name:  "root",
		Left:  &sharedNode{Name: "left", Right: bottom},
		Right: &sharedNode{Name: "right", Right: bottom},
	}

	data, err := em.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(data, []byte{0xd8, 0x1d}); count != 1 {
		t.Fatalf("encoded %d shared reference tags, want 1: %x", count, data)
	}

	var got *sharedNode
	if err := dm.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Left.Right != got.Right.Right {
		t.Fatal("diamond pointers were not restored to the same node")
	}
	if got.Left.Right.Name != "bottom" {
		t.Fatalf("shared node name = %q, want bottom", got.Left.Right.Name)
	}
}

func TestSharedRefCycleRestoresPointerIdentity(t *testing.T) {
	em, dm := sharedTestModes(t)

	a := &sharedNode{Name: "a"}
	b := &sharedNode{Name: "b"}
	a.Right = b
	b.Right = a

	data, err := em.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}

	var got *sharedNode
	if err := dm.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Right.Right != got {
		t.Fatal("cycle pointer was not restored")
	}
	if got.Right.Name != "b" {
		t.Fatalf("cycle node name = %q, want b", got.Right.Name)
	}
}

func TestSharedRefDoesNotMergeEqualDistinctPointers(t *testing.T) {
	em, dm := sharedTestModes(t)

	first := &sharedNode{Name: "equal"}
	second := &sharedNode{Name: "equal"}
	root := &sharedNode{Name: "root", Left: first, Right: second}

	data, err := em.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(data, []byte{0xd8, 0x1d}); count != 0 {
		t.Fatalf("encoded %d shared references for distinct equal pointers, want 0: %x", count, data)
	}

	var got *sharedNode
	if err := dm.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Left == got.Right {
		t.Fatal("equal but distinct pointers were incorrectly merged")
	}
}

func TestSharedRefDoesNotEncodePointersThroughInterfaces(t *testing.T) {
	em, dm := sharedTestModes(t)

	shared := &sharedNode{Name: "shared"}
	root := &sharedNode{Name: "root", Left: shared, Dyn: shared}
	data, err := em.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}

	var got *sharedNode
	if err := dm.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	dyn, ok := got.Dyn.(map[any]any)
	if !ok {
		t.Fatalf("dynamic value = %T, want map[any]any", got.Dyn)
	}
	if got.Left == nil || dyn["name"] != "shared" {
		t.Fatal("pointer field and interface value were not decoded")
	}
}

func TestSharedRefNumbersAreScopedToTopLevelDataItem(t *testing.T) {
	em, dm := sharedTestModes(t)

	bottom := &sharedNode{Name: "bottom"}
	root := &sharedNode{Name: "root", Left: bottom, Right: bottom}
	data, err := em.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var first *sharedNode
	if err := dm.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}

	var second *sharedNode
	err = dm.Unmarshal([]byte{0xd8, 0x1d, 0x00}, &second)
	var refErr *InvalidSharedRefError
	if !errors.As(err, &refErr) {
		t.Fatalf("second top-level item got error %v, want *InvalidSharedRefError", err)
	}
}

func TestSharedRefNumbersAreScopedAcrossStreamedTopLevelDataItems(t *testing.T) {
	em, dm := sharedTestModes(t)

	bottom := &sharedNode{Name: "bottom"}
	root := &sharedNode{Name: "root", Left: bottom, Right: bottom}
	data, err := em.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, 0xd8, 0x1d, 0x00)

	dec := dm.NewDecoder(bytes.NewReader(data))
	var first *sharedNode
	if err := dec.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.Left != first.Right {
		t.Fatal("streamed diamond pointer was not restored")
	}

	var second *sharedNode
	if err := dec.Decode(&second); !errors.As(err, new(*InvalidSharedRefError)) {
		t.Fatalf("second streamed item got error %v, want *InvalidSharedRefError", err)
	}

	emptyDecoder := dm.NewDecoder(bytes.NewReader(nil))
	var third *sharedNode
	if err := emptyDecoder.Decode(&third); err != io.EOF {
		t.Fatalf("empty stream error = %v, want io.EOF", err)
	}
}

func TestSharedRefRejectsInvalidReferenceAndEnforcesLimit(t *testing.T) {
	dm, err := DecOptions{
		SharedRef:     SharedRefAllowed,
		MaxSharedRefs: 16,
	}.DecMode()
	if err != nil {
		t.Fatal(err)
	}

	var node *sharedNode
	if err := dm.Unmarshal([]byte{0xd8, 0x1d, 0x00}, &node); err == nil {
		t.Fatal("forward reference was accepted")
	}

	if err := dm.Unmarshal([]byte{0xd8, 0x1d, 0x20}, &node); err == nil {
		t.Fatal("negative-integer shared reference was accepted")
	}

	em, err := EncOptions{SharedRef: SharedRefAllowed}.EncMode()
	if err != nil {
		t.Fatal(err)
	}
	root := &sharedNode{Name: "root"}
	nodes := make([]*sharedNode, 16)
	root.Left = root
	nodes[0] = root
	for i := 1; i < len(nodes); i++ {
		nodes[i] = root
	}
	root.Items = &nodes
	data, err := em.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var got *sharedNode
	limitDm, limitErrMode := DecOptions{
		SharedRef:     SharedRefAllowed,
		MaxSharedRefs: 16,
	}.DecMode()
	if limitErrMode != nil {
		t.Fatal(limitErrMode)
	}
	err = limitDm.Unmarshal(data, &got)
	var limitErr *MaxSharedRefsError
	if !errors.As(err, &limitErr) {
		t.Fatalf("two references got error %v, want *MaxSharedRefsError", err)
	}
}

func TestSharedRefRespectsNestedDepthAndValueLimit(t *testing.T) {
	var node *sharedNode
	depthDM, err := DecOptions{
		SharedRef:       SharedRefAllowed,
		MaxNestedLevels: 4,
	}.DecMode()
	if err != nil {
		t.Fatal(err)
	}
	err = depthDM.Unmarshal([]byte{
		0xd8, 0x1c, 0xd8, 0x1c, 0xd8, 0x1c, 0xd8, 0x1c, 0xd8, 0x1c,
		0xa1, 0x64, 0x6e, 0x61, 0x6d, 0x65, 0x61,
	}, &node)
	if !errors.As(err, new(*MaxNestedLevelError)) {
		t.Fatalf("nested shared tags got error %v, want *MaxNestedLevelError", err)
	}

	valueDM, err := DecOptions{
		SharedRef:       SharedRefAllowed,
		MaxSharedValues: 16,
	}.DecMode()
	if err != nil {
		t.Fatal(err)
	}
	var values []*sharedInt
	data := []byte{0x91}
	for range 17 {
		data = append(data, 0xd8, 0x1c, 0xa1, 0x65, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x01)
	}
	err = valueDM.Unmarshal(data, &values)
	if !errors.As(err, new(*MaxSharedValuesError)) {
		t.Fatalf("two shareable values got error %v, want *MaxSharedValuesError", err)
	}
}

func TestSharedRefDisabledPreservesUnknownTagBehavior(t *testing.T) {
	var value any
	if err := Unmarshal([]byte{0xd8, 0x1d, 0x00}, &value); err != nil {
		t.Fatal(err)
	}
	tag, ok := value.(Tag)
	if !ok || tag.Number != 29 || tag.Content != uint64(0) {
		t.Fatalf("disabled shared reference decoded as %#v", value)
	}
}
