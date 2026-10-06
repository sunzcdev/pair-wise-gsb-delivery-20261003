package bloom

import (
	"errors"
	"fmt"
	"testing"
)

func TestCountingBloomCollisionsAndIndependentReceipts(t *testing.T) {
	f := NewCountingBloomFilter(1, 3)
	key := []byte("same-key")

	first, err := f.Add(key)
	if err != nil {
		t.Fatal(err)
	}
	if f.counters[0] != 3 {
		t.Fatalf("duplicate hash locations should add 3, got %d", f.counters[0])
	}

	second, err := f.Add(key)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("repeated additions should return independent receipts")
	}
	if f.counters[0] != 6 {
		t.Fatalf("second addition should add another 3, got %d", f.counters[0])
	}

	if err := f.Remove(first); err != nil {
		t.Fatal(err)
	}
	if f.counters[0] != 3 || !f.Test(key) {
		t.Fatal("surviving duplicate addition must remain positive and test positive")
	}

	if err := f.Remove(second); err != nil {
		t.Fatal(err)
	}
	if f.counters[0] != 0 || f.Test(key) {
		t.Fatal("removing all receipts should return the counter to zero")
	}
	if err := f.Remove(second); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("duplicate removal should fail with ErrInvalidReceipt, got %v", err)
	}
}

func TestCountingBloomAddRejectsAtCounterLimitAtomically(t *testing.T) {
	f := NewCountingBloomFilter(1, 3)
	key := []byte("limited")

	for i := 0; i < maxCounterValue/3; i++ {
		if _, err := f.Add(key); err != nil {
			t.Fatal(err)
		}
	}
	if f.counters[0] != maxCounterValue {
		t.Fatalf("setup should saturate the counter, got %d", f.counters[0])
	}

	receipt, err := f.Add(key)
	if !errors.Is(err, ErrCounterLimit) {
		t.Fatalf("expected ErrCounterLimit, got %v", err)
	}
	if receipt != (Receipt{}) {
		t.Fatal("rejected addition should not return a receipt")
	}
	if f.counters[0] != maxCounterValue || len(f.receipts) != maxCounterValue/3 {
		t.Fatal("rejected addition should leave counters and receipt records unchanged")
	}
}

func TestCountingBloomRejectedAddDoesNotPartiallySaturate(t *testing.T) {
	f := NewCountingBloomFilter(4, 3)

	var key []byte
	var increments []counterIncrement
	for i := 0; ; i++ {
		if i == 100000 {
			t.Fatal("could not find a deterministic test key with distinct counters")
		}
		key = []byte(fmt.Sprintf("partial-%d", i))
		increments, _ = f.increments(key)
		if len(increments) >= 2 {
			break
		}
	}

	f.counters[increments[0].index] = maxCounterValue
	before := make([]byte, len(f.counters))
	copy(before, f.counters)

	receipt, err := f.Add(key)
	if !errors.Is(err, ErrCounterLimit) {
		t.Fatalf("expected ErrCounterLimit, got %v", err)
	}
	if receipt != (Receipt{}) {
		t.Fatal("rejected addition should not return a receipt")
	}
	for i := range before {
		if f.counters[i] != before[i] {
			t.Fatalf("counter %d changed: %d -> %d", i, before[i], f.counters[i])
		}
	}
}

func TestCountingBloomReceiptValidation(t *testing.T) {
	f := NewCountingBloomFilter(100, 3)
	g := NewCountingBloomFilter(100, 3)

	fReceipt, err := f.AddString("from-f")
	if err != nil {
		t.Fatal(err)
	}
	gReceipt, err := g.AddString("from-g")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Remove(gReceipt); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("cross-filter receipt should be rejected, got %v", err)
	}
	if !f.TestString("from-f") {
		t.Fatal("cross-filter removal changed f")
	}
	if err := g.Remove(gReceipt); err != nil {
		t.Fatalf("rejection by another filter should not consume the receipt: %v", err)
	}

	first, err := f.AddString("twice")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.AddString("twice")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Remove(first); err != nil {
		t.Fatal(err)
	}
	if !f.TestString("twice") {
		t.Fatal("the second receipt should keep the key present")
	}
	if err := f.Remove(second); err != nil {
		t.Fatal(err)
	}
	if f.TestString("twice") {
		t.Fatal("the key should be absent after both receipts are removed")
	}
	if err := f.Remove(fReceipt); err != nil {
		t.Fatalf("cross-filter rejection should not consume the original receipt: %v", err)
	}
	if err := f.Remove(first); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("duplicate receipt should be rejected, got %v", err)
	}
}

func TestCountingBloomClearAllInvalidatesOldReceipts(t *testing.T) {
	f := NewCountingBloomFilter(100, 3)
	oldFirst, err := f.AddString("first")
	if err != nil {
		t.Fatal(err)
	}
	oldSecond, err := f.AddString("second")
	if err != nil {
		t.Fatal(err)
	}

	f.ClearAll()
	newSecond, err := f.AddString("second")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Remove(oldFirst); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("pre-clear receipt should be rejected, got %v", err)
	}
	if err := f.Remove(oldSecond); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("pre-clear receipt should be rejected, got %v", err)
	}
	if f.TestString("first") {
		t.Fatal("ClearAll should remove the first key")
	}
	if !f.TestString("second") {
		t.Fatal("new data with the same key must remain present")
	}
	if err := f.Remove(newSecond); err != nil {
		t.Fatal(err)
	}
	if f.TestString("second") {
		t.Fatal("a valid new receipt should remove only the new addition")
	}
}

func TestCountingBloomReclaimsReceiptRecords(t *testing.T) {
	f := NewCountingBloomFilter(100, 3)
	receipt, err := f.AddString("temporary")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.receipts) != 1 {
		t.Fatalf("expected one active receipt record, got %d", len(f.receipts))
	}

	if err := f.Remove(receipt); err != nil {
		t.Fatal(err)
	}
	if len(f.receipts) != 0 {
		t.Fatalf("Remove should delete the receipt record, got %d", len(f.receipts))
	}

	if _, err := f.AddString("cleared"); err != nil {
		t.Fatal(err)
	}
	f.ClearAll()
	if len(f.receipts) != 0 {
		t.Fatalf("ClearAll should replace the receipt table, got %d", len(f.receipts))
	}
}
