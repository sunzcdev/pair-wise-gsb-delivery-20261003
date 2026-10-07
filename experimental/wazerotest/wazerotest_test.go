package wazerotest

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

func TestNewFunction(t *testing.T) {
	tests := []struct {
		in  any
		out *Function
	}{
		{
			in: func(ctx context.Context, mod api.Module) {},
			out: &Function{
				ParamTypes:  []api.ValueType{},
				ResultTypes: []api.ValueType{},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module, v uint32) {},
			out: &Function{
				ParamTypes:  []api.ValueType{api.ValueTypeI32},
				ResultTypes: []api.ValueType{},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module, v uint64) {},
			out: &Function{
				ParamTypes:  []api.ValueType{api.ValueTypeI64},
				ResultTypes: []api.ValueType{},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module, v int32) {},
			out: &Function{
				ParamTypes:  []api.ValueType{api.ValueTypeI32},
				ResultTypes: []api.ValueType{},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module, v int64) {},
			out: &Function{
				ParamTypes:  []api.ValueType{api.ValueTypeI64},
				ResultTypes: []api.ValueType{},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module, v float32) {},
			out: &Function{
				ParamTypes:  []api.ValueType{api.ValueTypeF32},
				ResultTypes: []api.ValueType{},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module, v float64) {},
			out: &Function{
				ParamTypes:  []api.ValueType{api.ValueTypeF64},
				ResultTypes: []api.ValueType{},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module) uint32 { return 0 },
			out: &Function{
				ParamTypes:  []api.ValueType{},
				ResultTypes: []api.ValueType{api.ValueTypeI32},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module) uint64 { return 0 },
			out: &Function{
				ParamTypes:  []api.ValueType{},
				ResultTypes: []api.ValueType{api.ValueTypeI64},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module) int32 { return 0 },
			out: &Function{
				ParamTypes:  []api.ValueType{},
				ResultTypes: []api.ValueType{api.ValueTypeI32},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module) int64 { return 0 },
			out: &Function{
				ParamTypes:  []api.ValueType{},
				ResultTypes: []api.ValueType{api.ValueTypeI64},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module) float32 { return 0 },
			out: &Function{
				ParamTypes:  []api.ValueType{},
				ResultTypes: []api.ValueType{api.ValueTypeF32},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module) float64 { return 0 },
			out: &Function{
				ParamTypes:  []api.ValueType{},
				ResultTypes: []api.ValueType{api.ValueTypeF64},
			},
		},

		{
			in: func(ctx context.Context, mod api.Module, _ uint32, _ uint64, _ int32, _ int64, _ float32, _ float64) (_ uint32, _ uint64, _ int32, _ int64, _ float32, _ float64) {
				return
			},
			out: &Function{
				ParamTypes: []api.ValueType{
					api.ValueTypeI32, api.ValueTypeI64,
					api.ValueTypeI32, api.ValueTypeI64,
					api.ValueTypeF32, api.ValueTypeF64,
				},
				ResultTypes: []api.ValueType{
					api.ValueTypeI32, api.ValueTypeI64,
					api.ValueTypeI32, api.ValueTypeI64,
					api.ValueTypeF32, api.ValueTypeF64,
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(reflect.TypeOf(test.in).String(), func(t *testing.T) {
			f := NewFunction(test.in)
			if !bytes.Equal(test.out.ParamTypes, f.ParamTypes) {
				t.Errorf("invalid parameter types: want=%v got=%v", test.out.ParamTypes, f.ParamTypes)
			}
			if !bytes.Equal(test.out.ResultTypes, f.ResultTypes) {
				t.Errorf("invalid result types: want=%v got=%v", test.out.ResultTypes, f.ResultTypes)
			}
		})
	}
}

func TestNewMemory(t *testing.T) {
	if memory := NewMemory(1); len(memory.Bytes) != PageSize {
		t.Error("memory buffer not aligned on page size")
	}
}

func TestNewFixedMemory(t *testing.T) {
	if memory := NewFixedMemory(1); len(memory.Bytes) != PageSize {
		t.Error("memory buffer not aligned on page size")
	} else if memory.Min != 1 {
		t.Error("invalid min memory size:", memory.Min)
	} else if memory.Max != 1 {
		t.Error("invalid max memory size:", memory.Max)
	}
}

func TestMemoryReadCopy(t *testing.T) {
	mem := NewFixedMemory(1)
	mem.Bytes[0] = 1
	mem.Bytes[1] = 2

	cp, ok := mem.ReadCopy(0, 2)
	if !ok || !bytes.Equal(cp, []byte{1, 2}) {
		t.Fatalf("unexpected copy: %v ok=%v", cp, ok)
	}

	// Owned data: writes neither go through nor get affected by the memory.
	cp[0] = 9
	if mem.Bytes[0] != 1 {
		t.Fatal("ReadCopy wrote through to the memory")
	}
	mem.Bytes[1] = 8
	if cp[1] != 2 {
		t.Fatal("memory mutation affected the copy")
	}

	// A grow replaces Bytes, but the copy remains stable.
	growable := NewMemory(1)
	growable.Bytes[0] = 3
	cp, ok = growable.ReadCopy(0, 1)
	if !ok {
		t.Fatal("ReadCopy failed")
	}
	if _, ok := growable.Grow(1); !ok {
		t.Fatal("Grow failed")
	}
	if cp[0] != 3 {
		t.Fatal("copy changed after grow")
	}

	// Out of range and zero-length end semantics.
	if _, ok := mem.ReadCopy(PageSize, 1); ok {
		t.Fatal("expected out of range")
	}
	empty, ok := mem.ReadCopy(PageSize, 0)
	if !ok || len(empty) != 0 || empty == nil {
		t.Fatalf("zero-length end read: %v ok=%v", empty, ok)
	}
	if _, ok := mem.ReadCopy(PageSize+1, 0); ok {
		t.Fatal("zero-length read past the end must fail")
	}
	if _, ok := mem.ReadCopy(^uint32(0), 2); ok {
		t.Fatal("offset+length overflow must fail")
	}
}

func TestMemoryReadBorrow(t *testing.T) {
	mem := NewMemory(1)
	mem.Max = 2
	mem.Bytes[0] = 1

	buf, gen, ok := mem.ReadBorrow(0, 1)
	if !ok || buf[0] != 1 {
		t.Fatalf("unexpected borrow: %v ok=%v", buf, ok)
	}
	if !mem.ReadBorrowStable(0, 1, gen) {
		t.Fatal("fresh borrow must be stable")
	}

	// Borrow keeps Read's write-through semantics.
	buf[0] = 4
	if mem.Bytes[0] != 4 {
		t.Fatal("borrow lost write-through semantics")
	}

	// After Grow (which always replaces Bytes), old borrows are invalidated.
	if _, ok := mem.Grow(1); !ok {
		t.Fatal("Grow failed")
	}
	if mem.ReadBorrowStable(0, 1, gen) {
		t.Fatal("borrow must be invalidated after grow")
	}

	// A fresh borrow sees the copied data and a new generation.
	buf2, gen2, ok := mem.ReadBorrow(0, 1)
	if !ok || buf2[0] != 4 {
		t.Fatalf("unexpected borrow after grow: %v ok=%v", buf2, ok)
	}
	if gen == gen2 {
		t.Fatal("generation must advance on grow")
	}
	if !mem.ReadBorrowStable(0, 1, gen2) {
		t.Fatal("fresh borrow must be stable")
	}

	// A failed grow leaves the generation untouched.
	if _, ok := mem.Grow(10); ok {
		t.Fatal("expected grow beyond max to fail")
	}
	if !mem.ReadBorrowStable(0, 1, gen2) {
		t.Fatal("failed grow must not invalidate borrows")
	}

	// Grow(0) reports the current size and must not invalidate borrows.
	if prev, ok := mem.Grow(0); !ok || prev != 2 {
		t.Fatalf("Grow(0): prev=%d ok=%v", prev, ok)
	}
	if !mem.ReadBorrowStable(0, 1, gen2) {
		t.Fatal("Grow(0) must not invalidate borrows")
	}

	// Range and zero-length end semantics.
	if _, _, ok := mem.ReadBorrow(uint32(len(mem.Bytes)), 1); ok {
		t.Fatal("expected out of range")
	}
	empty, zeroGen, ok := mem.ReadBorrow(uint32(len(mem.Bytes)), 0)
	if !ok || len(empty) != 0 || empty == nil {
		t.Fatalf("zero-length end read: %v ok=%v", empty, ok)
	}
	if !mem.ReadBorrowStable(uint32(len(mem.Bytes)), 0, zeroGen) {
		t.Fatal("fresh zero-length borrow must be stable")
	}
	if mem.ReadBorrowStable(^uint32(0), 2, gen2) {
		t.Fatal("overflowing range must not be stable")
	}
}
