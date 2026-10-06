package ratelimit_test

import (
	"errors"
	"testing"
	"time"

	. "github.com/bsm/ratelimit/v3"
)

func ExampleRateLimiter_Reserve() {
	rl := New(2, time.Second)

	r, err := rl.Reserve()
	if err != nil {
		return
	}

	time.Sleep(r.Delay())
	if err := r.Consume(); err != nil {
		// work was no longer needed or the reservation was canceled
		_ = err
	}
}

func TestReservation_publicLifecycle(t *testing.T) {
	rl := New(1000, time.Second)

	r1, err := rl.Reserve()
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if r1.Cost() != 1 {
		t.Fatalf("cost = %d, want 1", r1.Cost())
	}
	if d := r1.Delay(); d > 10*time.Millisecond {
		t.Fatalf("initial delay = %v, want ~0", d)
	}
	if r1.TimeToAct().IsZero() {
		t.Fatal("expected a concrete TimeToAct")
	}
	if err := r1.Consume(); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !errors.Is(r1.Cancel(), ErrReservationFinal) {
		t.Fatal("cancel after consume must report ErrReservationFinal")
	}

	// A cost above burst is rejected rather than scheduled infinitely far.
	if _, err := rl.ReserveN(1001); !errors.Is(err, ErrCostExceedsBurst) {
		t.Fatalf("ReserveN(1001) = %v, want ErrCostExceedsBurst", err)
	}

	// A future reservation can be canceled before it is due.
	queued := New(1000, time.Second)
	var rs []*Reservation
	for i := 0; i < 2000; i++ {
		r, err := queued.Reserve()
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		rs = append(rs, r)
	}
	future, err := queued.Reserve()
	if err != nil {
		t.Fatalf("queue reservation: %v", err)
	}
	if future.Delay() < 900*time.Millisecond {
		t.Fatalf("queued delay = %v, want ~1s", future.Delay())
	}
	if err := future.Cancel(); err != nil {
		t.Fatalf("cancel future reservation: %v", err)
	}
	if !errors.Is(future.Cancel(), ErrReservationFinal) {
		t.Fatal("double cancel must report ErrReservationFinal")
	}
}
