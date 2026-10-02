// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type sharedRefInner struct {
	V int `cbor:"v"`
}

type sharedRefOuter struct {
	A *sharedRefInner `cbor:"a"`
	B *sharedRefInner `cbor:"b"`
}

type sharedRefListNode struct {
	Val  int                `cbor:"val"`
	Next *sharedRefListNode `cbor:"next"`
}

func sharedRefTestEncMode(t *testing.T) EncMode {
	t.Helper()
	em, err := EncOptions{SharedRefs: SharedRefAuto}.EncMode()
	if err != nil {
		t.Fatalf("failed to create EncMode: %v", err)
	}
	return em
}

func sharedRefTestDecMode(t *testing.T) DecMode {
	t.Helper()
	dm, err := DecOptions{SharedRefs: SharedRefAuto}.DecMode()
	if err != nil {
		t.Fatalf("failed to create DecMode: %v", err)
	}
	return dm
}

// TestSharedRefDiamond verifies that a diamond-shaped object graph (two
// fields pointing to the same object) round-trips through CBOR tag 28
// (shareable) and tag 29 (sharedref) with pointer identity restored.
// Field-value equality alone would not prove this: the decoded fields must
// hold the same pointer.
func TestSharedRefDiamond(t *testing.T) {
	em := sharedRefTestEncMode(t)
	dm := sharedRefTestDecMode(t)

	shared := &sharedRefInner{V: 1}
	in := sharedRefOuter{A: shared, B: shared}

	data, err := em.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	// Expected encoding:
	//   a2                  -- map(2)
	//   61 61               --   "a"
	//   d8 1c               --   tag 28 (shareable)
	//   a1 61 76 01         --     {"v": 1}
	//   61 62               --   "b"
	//   d8 1d 00            --   tag 29 (sharedref) 0
	want := []byte{
		0xa2,
		0x61, 0x61,
		0xd8, 0x1c,
		0xa1, 0x61, 0x76, 0x01,
		0x61, 0x62,
		0xd8, 0x1d, 0x00,
	}
	if !bytes.Equal(data, want) {
		t.Errorf("Marshal() = 0x%x, want 0x%x", data, want)
	}

	var out sharedRefOuter
	if err := dm.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}

	// Pointer identity must be restored: A and B must be the same object.
	if out.A == nil || out.B == nil {
		t.Fatalf("Unmarshal() = %+v, want non-nil A and B", out)
	}
	if out.A != out.B {
		t.Errorf("Unmarshal() A (%p) and B (%p) are different objects, want shared", out.A, out.B)
	}
	if out.A.V != 1 {
		t.Errorf("Unmarshal() A.V = %d, want 1", out.A.V)
	}

	// Mutating through one field must be visible through the other.
	out.A.V = 42
	if out.B.V != 42 {
		t.Errorf("mutation through A not visible through B: B.V = %d, want 42", out.B.V)
	}
}

// TestSharedRefCycle verifies that a cyclic object graph encodes without
// infinite recursion and decodes with the cycle's pointer identity restored.
func TestSharedRefCycle(t *testing.T) {
	em := sharedRefTestEncMode(t)
	dm := sharedRefTestDecMode(t)

	a := &sharedRefListNode{Val: 1}
	b := &sharedRefListNode{Val: 2}
	a.Next = b
	b.Next = a

	data, err := em.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	// Expected encoding:
	//   d8 1c               -- tag 28 (shareable)
	//   a2                  --   map(2)
	//   63 76 61 6c         --     "val"
	//   01                  --     1
	//   64 6e 65 78 74      --     "next"
	//   a2 ...              --     {"val": 2, "next": ...} (b, encoded once)
	//   d8 1d 00            --       tag 29 (sharedref) 0
	want := []byte{
		0xd8, 0x1c,
		0xa2,
		0x63, 0x76, 0x61, 0x6c,
		0x01,
		0x64, 0x6e, 0x65, 0x78, 0x74,
		0xa2,
		0x63, 0x76, 0x61, 0x6c,
		0x02,
		0x64, 0x6e, 0x65, 0x78, 0x74,
		0xd8, 0x1d, 0x00,
	}
	if !bytes.Equal(data, want) {
		t.Errorf("Marshal() = 0x%x, want 0x%x", data, want)
	}

	var out *sharedRefListNode
	if err := dm.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}

	// The cycle must be restored: out.Next.Next is out itself.
	if out == nil || out.Next == nil || out.Next.Next == nil {
		t.Fatalf("Unmarshal() = %+v, want two-node cycle", out)
	}
	if out.Next.Next != out {
		t.Errorf("cycle not restored: out.Next.Next (%p) != out (%p)", out.Next.Next, out)
	}
	if out.Val != 1 || out.Next.Val != 2 {
		t.Errorf("Unmarshal() values = %d, %d, want 1, 2", out.Val, out.Next.Val)
	}
}

// TestSharedRefSelfCycle verifies a node pointing to itself.
func TestSharedRefSelfCycle(t *testing.T) {
	em := sharedRefTestEncMode(t)
	dm := sharedRefTestDecMode(t)

	a := &sharedRefListNode{Val: 7}
	a.Next = a

	data, err := em.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	var out *sharedRefListNode
	if err := dm.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	if out.Next != out {
		t.Errorf("self-cycle not restored: out.Next (%p) != out (%p)", out.Next, out)
	}
}

// TestSharedRefDistinctEqualObjects verifies that equal but distinct objects
// are never merged: they are encoded without any shareable/sharedref tags
// and decode to distinct pointers.
func TestSharedRefDistinctEqualObjects(t *testing.T) {
	em := sharedRefTestEncMode(t)
	dm := sharedRefTestDecMode(t)

	x := &sharedRefInner{V: 1}
	y := &sharedRefInner{V: 1}
	in := [2]*sharedRefInner{x, y}

	data, err := em.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	// No tag 28 (0xd8 0x1c) or tag 29 (0xd8 0x1d) may appear: the two
	// objects are distinct, so nothing is shared.
	if bytes.Contains(data, []byte{0xd8, 0x1c}) || bytes.Contains(data, []byte{0xd8, 0x1d}) {
		t.Errorf("Marshal() = 0x%x contains shareable/sharedref tag for distinct objects", data)
	}

	// Encoding must be identical to default encoding without shared refs.
	defaultData, err := Marshal(in)
	if err != nil {
		t.Fatalf("default Marshal() error: %v", err)
	}
	if !bytes.Equal(data, defaultData) {
		t.Errorf("Marshal() = 0x%x, want default encoding 0x%x", data, defaultData)
	}

	var out [2]*sharedRefInner
	if err := dm.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	if out[0] == out[1] {
		t.Errorf("Unmarshal() merged distinct objects: out[0] == out[1] (%p)", out[0])
	}
	if out[0].V != 1 || out[1].V != 1 {
		t.Errorf("Unmarshal() values = %d, %d, want 1, 1", out[0].V, out[1].V)
	}
}

// TestSharedRefNumberingReset verifies that shared reference numbers start
// at 0 for each top-level data item and never leak into the next one.
func TestSharedRefNumberingReset(t *testing.T) {
	em := sharedRefTestEncMode(t)
	dm := sharedRefTestDecMode(t)

	newDiamond := func() sharedRefOuter {
		shared := &sharedRefInner{V: 1}
		return sharedRefOuter{A: shared, B: shared}
	}

	data1, err := em.Marshal(newDiamond())
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	data2, err := em.Marshal(newDiamond())
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatalf("second top-level item encoding 0x%x differs from first 0x%x: reference numbers leaked", data2, data1)
	}

	// Decode both items from one stream. The second item's sharedref 0 must
	// resolve to its own shareable value, not the first item's.
	combined := append(data1, data2...)
	var out1, out2 sharedRefOuter
	rest, err := dm.UnmarshalFirst(combined, &out1)
	if err != nil {
		t.Fatalf("UnmarshalFirst() error: %v", err)
	}
	if _, err := dm.UnmarshalFirst(rest, &out2); err != nil {
		t.Fatalf("UnmarshalFirst() error: %v", err)
	}
	if out1.A != out1.B {
		t.Errorf("first item: A (%p) != B (%p), want shared", out1.A, out1.B)
	}
	if out2.A != out2.B {
		t.Errorf("second item: A (%p) != B (%p), want shared", out2.A, out2.B)
	}
	if out1.A == out2.A {
		t.Errorf("items share object %p: reference numbers leaked across top-level items", out1.A)
	}

	// Streaming Decoder must also reset reference numbers per item.
	dec := dm.NewDecoder(bytes.NewReader(combined))
	var s1, s2 sharedRefOuter
	if err := dec.Decode(&s1); err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if err := dec.Decode(&s2); err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if s1.A != s1.B || s2.A != s2.B {
		t.Errorf("streaming decode broke sharing: s1.A==s1.B is %v, s2.A==s2.B is %v", s1.A == s1.B, s2.A == s2.B)
	}
	if s1.A == s2.A {
		t.Errorf("streaming decode leaked reference across items: s1.A == s2.A (%p)", s1.A)
	}
}

// TestSharedRefStreamEncoder verifies shared references through the
// streaming Encoder, with numbering reset per Encode call.
func TestSharedRefStreamEncoder(t *testing.T) {
	em := sharedRefTestEncMode(t)

	newDiamond := func() sharedRefOuter {
		shared := &sharedRefInner{V: 1}
		return sharedRefOuter{A: shared, B: shared}
	}

	var buf bytes.Buffer
	enc := em.NewEncoder(&buf)
	if err := enc.Encode(newDiamond()); err != nil {
		t.Fatalf("Encode() error: %v", err)
	}
	firstLen := buf.Len()
	if err := enc.Encode(newDiamond()); err != nil {
		t.Fatalf("Encode() error: %v", err)
	}

	// Both items must be byte-identical: numbering restarts per item.
	b := buf.Bytes()
	if !bytes.Equal(b[:firstLen], b[firstLen:]) {
		t.Errorf("second streamed item 0x%x differs from first 0x%x", b[firstLen:], b[:firstLen])
	}
}

// TestSharedRefDecodeToAny verifies that shareable/sharedref tags decode to
// values of type any with identity of shared containers preserved.
func TestSharedRefDecodeToAny(t *testing.T) {
	dm := sharedRefTestDecMode(t)

	// [28({1: 2}), 29(0)]
	data := []byte{
		0x82,
		0xd8, 0x1c,
		0xa1, 0x01, 0x02,
		0xd8, 0x1d, 0x00,
	}

	var v any
	if err := dm.Unmarshal(data, &v); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}

	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("Unmarshal() = %#v, want []any of length 2", v)
	}
	m0, ok0 := arr[0].(map[any]any)
	m1, ok1 := arr[1].(map[any]any)
	if !ok0 || !ok1 {
		t.Fatalf("Unmarshal() = %#v, want two maps", v)
	}

	// Both elements must be the same map: mutation through one is visible
	// through the other.
	m0["shared"] = true
	if m1["shared"] != true {
		t.Errorf("shared map identity not restored when decoding to any")
	}
}

// TestSharedRefDefaultModeUnchanged verifies that with the default
// SharedRefNone mode, tags 28 and 29 keep their previous behavior
// (unrecognized tags decoded to cbor.Tag).
func TestSharedRefDefaultModeUnchanged(t *testing.T) {
	data := []byte{0xd8, 0x1c, 0x01} // 28(1)

	var v any
	if err := Unmarshal(data, &v); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	tag, ok := v.(Tag)
	if !ok {
		t.Fatalf("Unmarshal() = %#v, want cbor.Tag", v)
	}
	if tag.Number != 28 || tag.Content != uint64(1) {
		t.Errorf("Unmarshal() = %+v, want Tag{28, 1}", tag)
	}
}

// TestSharedRefMaxSharedRefs verifies that the number of shared references
// per top-level data item is limited by MaxSharedRefs.
func TestSharedRefMaxSharedRefs(t *testing.T) {
	dm, err := DecOptions{SharedRefs: SharedRefAuto, MaxSharedRefs: 1}.DecMode()
	if err != nil {
		t.Fatalf("failed to create DecMode: %v", err)
	}

	// [28({}), 28({})]: two shareable values, limit is 1.
	data := []byte{
		0x82,
		0xd8, 0x1c, 0xa0,
		0xd8, 0x1c, 0xa0,
	}

	var v any
	err = dm.Unmarshal(data, &v)
	var maxErr *MaxSharedRefsError
	if !errors.As(err, &maxErr) {
		t.Errorf("Unmarshal() error = %v, want MaxSharedRefsError", err)
	}
}

// TestSharedRefInvalidRef verifies that invalid shared reference numbers
// produce explicit errors.
func TestSharedRefInvalidRef(t *testing.T) {
	dm := sharedRefTestDecMode(t)

	testCases := []struct {
		name string
		data []byte
	}{
		{"sharedref without shareable", []byte{0xd8, 0x1d, 0x00}},
		{"sharedref out of range", []byte{0xd8, 0x1d, 0x05}},
		{"forward sharedref", []byte{0x82, 0xd8, 0x1d, 0x00, 0xd8, 0x1c, 0xa0}},
		{"sharedref to later number", []byte{0x82, 0xd8, 0x1c, 0xa0, 0xd8, 0x1d, 0x01}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			err := dm.Unmarshal(tc.data, &v)
			var invalidErr *InvalidSharedRefError
			if !errors.As(err, &invalidErr) {
				t.Errorf("Unmarshal() error = %v, want InvalidSharedRefError", err)
			}
		})
	}
}

// TestSharedRefNonIntegerRef verifies that a sharedref tag enclosing a
// non-integer produces an explicit error.
func TestSharedRefNonIntegerRef(t *testing.T) {
	dm := sharedRefTestDecMode(t)

	// 29(-1)
	data := []byte{0xd8, 0x1d, 0x20}

	var v any
	err := dm.Unmarshal(data, &v)
	if err == nil || !strings.Contains(err.Error(), "unsigned integer") {
		t.Errorf("Unmarshal() error = %v, want error about unsigned integer content", err)
	}
}

// TestSharedRefMaxNestedLevels verifies that nesting depth of shareable
// values is limited by MaxNestedLevels.
func TestSharedRefMaxNestedLevels(t *testing.T) {
	dm := sharedRefTestDecMode(t)

	// Nested 28(28(...28({})...)) exceeding the default max nested levels.
	data := []byte{0xa0}
	for range defaultMaxNestedLevels + 1 {
		data = append([]byte{0xd8, 0x1c}, data...)
	}

	var v any
	err := dm.Unmarshal(data, &v)
	var nestErr *MaxNestedLevelError
	if !errors.As(err, &nestErr) {
		t.Errorf("Unmarshal() error = %v, want MaxNestedLevelError", err)
	}
}

// TestSharedRefTypeMismatch verifies that a sharedref decoded to an
// incompatible Go type produces an UnmarshalTypeError.
func TestSharedRefTypeMismatch(t *testing.T) {
	dm := sharedRefTestDecMode(t)

	// {"a": 28({"v": 1}), "b": 29(0)} decoded to a struct whose field b is
	// not a pointer.
	data := []byte{
		0xa2,
		0x61, 0x61,
		0xd8, 0x1c,
		0xa1, 0x61, 0x76, 0x01,
		0x61, 0x62,
		0xd8, 0x1d, 0x00,
	}

	var out struct {
		A *sharedRefInner `cbor:"a"`
		B sharedRefInner  `cbor:"b"`
	}
	err := dm.Unmarshal(data, &out)
	var typeErr *UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Errorf("Unmarshal() error = %v, want UnmarshalTypeError", err)
	}
}

// TestSharedRefOptionsRoundTrip verifies options are validated and
// round-trip through EncOptions()/DecOptions().
func TestSharedRefOptionsRoundTrip(t *testing.T) {
	if _, err := (EncOptions{SharedRefs: SharedRefMode(-1)}).EncMode(); err == nil {
		t.Error("EncMode() with invalid SharedRefs didn't return error")
	}
	if _, err := (DecOptions{SharedRefs: maxSharedRefMode}).DecMode(); err == nil {
		t.Error("DecMode() with invalid SharedRefs didn't return error")
	}
	if _, err := (DecOptions{MaxSharedRefs: -1}).DecMode(); err == nil {
		t.Error("DecMode() with invalid MaxSharedRefs didn't return error")
	}

	em, err := (EncOptions{SharedRefs: SharedRefAuto}).EncMode()
	if err != nil {
		t.Fatalf("EncMode() error: %v", err)
	}
	if got := em.EncOptions().SharedRefs; got != SharedRefAuto {
		t.Errorf("EncOptions().SharedRefs = %v, want SharedRefAuto", got)
	}

	dm, err := (DecOptions{SharedRefs: SharedRefAuto, MaxSharedRefs: 10}).DecMode()
	if err != nil {
		t.Fatalf("DecMode() error: %v", err)
	}
	if got := dm.DecOptions().SharedRefs; got != SharedRefAuto {
		t.Errorf("DecOptions().SharedRefs = %v, want SharedRefAuto", got)
	}
	if got := dm.DecOptions().MaxSharedRefs; got != 10 {
		t.Errorf("DecOptions().MaxSharedRefs = %v, want 10", got)
	}

	// Default MaxSharedRefs applies when unset.
	dmDefault, err := (DecOptions{}).DecMode()
	if err != nil {
		t.Fatalf("DecMode() error: %v", err)
	}
	if got := dmDefault.DecOptions().MaxSharedRefs; got != defaultMaxSharedRefs {
		t.Errorf("DecOptions().MaxSharedRefs = %v, want %v", got, defaultMaxSharedRefs)
	}
}

// TestSharedRefDecodeDiamondToInterface verifies pointer identity is
// restored when shared structures are decoded through interface fields.
func TestSharedRefDecodeDiamondToInterface(t *testing.T) {
	dm := sharedRefTestDecMode(t)

	// [28({"v": 1}), 29(0)]
	data := []byte{
		0x82,
		0xd8, 0x1c,
		0xa1, 0x61, 0x76, 0x01,
		0xd8, 0x1d, 0x00,
	}

	var out [2]any
	if err := dm.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	if !reflect.DeepEqual(out[0], out[1]) {
		t.Errorf("Unmarshal() = %#v, want two equal values", out)
	}
}

// TestSharedRefThroughSliceAndMap verifies pointer identity is restored for
// shared objects nested in slices and maps.
func TestSharedRefThroughSliceAndMap(t *testing.T) {
	em := sharedRefTestEncMode(t)
	dm := sharedRefTestDecMode(t)

	shared := &sharedRefInner{V: 9}
	in := map[string][]*sharedRefInner{
		"first":  {shared},
		"second": {shared},
	}

	data, err := em.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	var out map[string][]*sharedRefInner
	if err := dm.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	if out["first"][0] != out["second"][0] {
		t.Errorf("shared object not restored: first (%p) != second (%p)", out["first"][0], out["second"][0])
	}
	if out["first"][0].V != 9 {
		t.Errorf("Unmarshal() V = %d, want 9", out["first"][0].V)
	}
}
