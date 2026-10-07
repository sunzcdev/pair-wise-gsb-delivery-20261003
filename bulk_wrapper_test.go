package btree

import (
	"errors"
	"testing"
)

func TestBTreeBulkLoadWrapper(t *testing.T) {
	tr := New(func(a, b any) bool { return a.(int) < b.(int) })
	stats, err := tr.BulkLoad(func(yield func(item any) bool) {
		for i := 0; i < 1000; i++ {
			if !yield(i) {
				return
			}
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Loaded != 1000 {
		t.Fatalf("loaded %d", stats.Loaded)
	}

	_, err = tr.BulkLoad(func(yield func(item any) bool) {
		yield(1)
		yield(0)
	}, nil)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("got %v", err)
	}

	_, err = tr.BulkLoad(nil, nil)
	if !errors.Is(err, ErrNilIterator) {
		t.Fatalf("got %v", err)
	}
}
