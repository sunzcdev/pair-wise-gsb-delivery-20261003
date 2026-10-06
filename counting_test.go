package bloom

import (
	"errors"
	"strconv"
	"testing"
)

func TestCountingReceiptRemovesOnlyThatAdd(t *testing.T) {
	f := NewCounting(1000, 4)

	first, err := f.AddString("same")
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	second, err := f.AddString("same")
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if first == second {
		t.Fatal("repeated adds must return independent receipts")
	}

	if err := f.Remove(first); err != nil {
		t.Fatalf("remove first add: %v", err)
	}
	if !f.TestString("same") {
		t.Fatal("surviving second add must remain positive")
	}

	if err := f.Remove(second); err != nil {
		t.Fatalf("remove second add: %v", err)
	}
	if f.TestString("same") {
		t.Fatal("key must be negative after all receipts are removed")
	}
}

func TestCountingRejectsReusedWrongAndExpiredReceipts(t *testing.T) {
	f := NewCounting(1000, 4)
	other := NewCounting(1000, 4)

	receipt, err := f.AddString("key")
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	if err := f.Remove(receipt); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := f.Remove(receipt); !errors.Is(err, ErrReceiptUsed) {
		t.Fatalf("reused receipt error = %v, want %v", err, ErrReceiptUsed)
	}
	if err := other.Remove(receipt); !errors.Is(err, ErrWrongFilter) {
		t.Fatalf("wrong filter error = %v, want %v", err, ErrWrongFilter)
	}

	receipt, err = f.AddString("old")
	if err != nil {
		t.Fatalf("add before clear: %v", err)
	}
	f.ClearAll()
	_, err = f.AddString("new")
	if err != nil {
		t.Fatalf("add after clear: %v", err)
	}
	if err := f.Remove(receipt); !errors.Is(err, ErrReceiptExpired) {
		t.Fatalf("expired receipt error = %v, want %v", err, ErrReceiptExpired)
	}
	if !f.TestString("new") {
		t.Fatal("new data must survive a remove attempt by an expired receipt")
	}
}

type fakeReceipt struct{}

func (*fakeReceipt) isCountingReceipt() {}

func TestCountingRejectsInvalidReceipt(t *testing.T) {
	f := NewCounting(1000, 4)
	if err := f.Remove(&fakeReceipt{}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("invalid receipt error = %v, want %v", err, ErrInvalidReceipt)
	}
	var nilReceipt *countingReceipt
	if err := f.Remove(nilReceipt); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("nil receipt error = %v, want %v", err, ErrInvalidReceipt)
	}
}

func TestCountingDuplicateHashLocations(t *testing.T) {
	f := NewCounting(1, 3)

	first, err := f.AddString("survivor")
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	second, err := f.AddString("colliding")
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if f.counters[0] != 6 {
		t.Fatalf("counter = %d, want 6 for two adds with three duplicate locations", f.counters[0])
	}

	if err := f.Remove(second); err != nil {
		t.Fatalf("remove colliding add: %v", err)
	}
	if f.counters[0] != 3 {
		t.Fatalf("counter = %d, want 3 after one of two colliding adds", f.counters[0])
	}
	if !f.TestString("survivor") {
		t.Fatal("surviving key must not become negative after duplicate-location accounting")
	}

	if err := f.Remove(first); err != nil {
		t.Fatalf("remove surviving add: %v", err)
	}
	if f.counters[0] != 0 {
		t.Fatalf("counter = %d, want 0", f.counters[0])
	}
}

func TestCountingCounterLimitRejectsEntireAdd(t *testing.T) {
	f := NewCounting(2, 3)
	var overflowKey []byte
	for i := 0; i < 10000; i++ {
		data := []byte("overflow-" + strconv.Itoa(i))
		locations := f.locations(baseHashes(data))
		hitsFirst := false
		hitsLast := false
		for _, location := range locations {
			hitsFirst = hitsFirst || location == 0
			hitsLast = hitsLast || location == 1
		}
		if hitsFirst && hitsLast {
			overflowKey = data
			break
		}
	}
	if overflowKey == nil {
		t.Fatal("could not construct a key hashing to both counter locations")
	}

	f.counters[1] = maxCounterValueAsUint32()
	before := []uint32{f.counters[0], f.counters[1]}

	_, err := f.Add(overflowKey)
	if !errors.Is(err, ErrCounterLimit) {
		t.Fatalf("add error = %v, want %v", err, ErrCounterLimit)
	}
	if f.counters[0] != before[0] || f.counters[1] != before[1] {
		t.Fatalf("counters changed from %v to %v after rejected add", before, f.counters)
	}
}

func TestCountingReceiptStorageReclaimed(t *testing.T) {
	f := NewCounting(1000, 4)

	receipt, err := f.AddString("temporary")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	r := receipt.(*countingReceipt)
	if r.used {
		t.Fatal("new receipt must be unused")
	}

	if err := f.Remove(receipt); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !r.used {
		t.Fatal("receipt must be marked used after remove")
	}
	if r.h != ([4]uint64{}) {
		t.Fatal("receipt hash storage must be released after remove")
	}

	if _, err := f.AddString("cleared"); err != nil {
		t.Fatalf("add before clear: %v", err)
	}
	f.ClearAll()
	if f.generation != 2 {
		t.Fatalf("generation = %d, want 2 after clear", f.generation)
	}
}

func maxCounterValueAsUint32() uint32 {
	return ^uint32(0)
}
