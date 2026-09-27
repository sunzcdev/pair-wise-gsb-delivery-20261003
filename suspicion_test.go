// Copyright IBM Corp. 2013, 2025
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"fmt"
	"testing"
	"time"
)

func TestSuspicion_remainingSuspicionTime(t *testing.T) {
	cases := []struct {
		n        int32
		k        int32
		elapsed  time.Duration
		min      time.Duration
		max      time.Duration
		expected time.Duration
	}{
		{0, 3, 0, 2 * time.Second, 30 * time.Second, 30 * time.Second},
		{1, 3, 2 * time.Second, 2 * time.Second, 30 * time.Second, 14 * time.Second},
		{2, 3, 3 * time.Second, 2 * time.Second, 30 * time.Second, 4810 * time.Millisecond},
		{3, 3, 4 * time.Second, 2 * time.Second, 30 * time.Second, -2 * time.Second},
		{4, 3, 5 * time.Second, 2 * time.Second, 30 * time.Second, -3 * time.Second},
		{5, 3, 10 * time.Second, 2 * time.Second, 30 * time.Second, -8 * time.Second},
	}
	for i, c := range cases {
		remaining := remainingSuspicionTime(c.n, c.k, c.elapsed, c.min, c.max)
		if remaining != c.expected {
			t.Errorf("case %d: remaining %9.6f != expected %9.6f", i, remaining.Seconds(), c.expected.Seconds())
		}
	}
}

func TestSuspicion_Timer(t *testing.T) {
	const k = 3
	const min = 500 * time.Millisecond
	const max = 2 * time.Second

	type pair struct {
		from    string
		newInfo bool
	}
	cases := []struct {
		numConfirmations int
		from             string
		confirmations    []pair
		expected         time.Duration
	}{
		{
			0,
			"me",
			[]pair{},
			max,
		},
		{
			1,
			"me",
			[]pair{
				pair{"me", false},
				pair{"foo", true},
			},
			1250 * time.Millisecond,
		},
		{
			1,
			"me",
			[]pair{
				pair{"me", false},
				pair{"foo", true},
				pair{"foo", false},
				pair{"foo", false},
			},
			1250 * time.Millisecond,
		},
		{
			2,
			"me",
			[]pair{
				pair{"me", false},
				pair{"foo", true},
				pair{"bar", true},
			},
			810 * time.Millisecond,
		},
		{
			3,
			"me",
			[]pair{
				pair{"me", false},
				pair{"foo", true},
				pair{"bar", true},
				pair{"baz", true},
			},
			min,
		},
		{
			3,
			"me",
			[]pair{
				pair{"me", false},
				pair{"foo", true},
				pair{"bar", true},
				pair{"baz", true},
				pair{"zoo", false},
			},
			min,
		},
	}
	for i, c := range cases {
		ch := make(chan time.Duration, 1)
		start := time.Now()
		f := func(numConfirmations int) {
			if numConfirmations != c.numConfirmations {
				t.Errorf("case %d: bad %d != %d", i, numConfirmations, c.numConfirmations)
			}

			ch <- time.Since(start)
		}

		// Create the timer and add the requested confirmations. Wait
		// the fudge amount to help make sure we calculate the timeout
		// overall, and don't accumulate extra time.
		s := newSuspicion(c.from, k, min, max, f)
		fudge := 25 * time.Millisecond
		for _, p := range c.confirmations {
			time.Sleep(fudge)
			if s.Confirm(p.from) != p.newInfo {
				t.Fatalf("case %d: newInfo mismatch for %s", i, p.from)
			}
		}

		// Wait until right before the timeout and make sure the
		// timer hasn't fired.
		already := time.Duration(len(c.confirmations)) * fudge
		time.Sleep(c.expected - already - fudge)
		select {
		case d := <-ch:
			t.Fatalf("case %d: should not have fired (%9.6f)", i, d.Seconds())
		default:
		}

		// Wait through the timeout and a little after and make sure it
		// fires.
		time.Sleep(2 * fudge)
		select {
		case <-ch:
		default:
			t.Fatalf("case %d: should have fired", i)
		}

		// Confirm after to make sure it handles a negative remaining
		// time correctly and doesn't fire again.
		s.Confirm("late")
		time.Sleep(c.expected + 2*fudge)
		select {
		case d := <-ch:
			t.Fatalf("case %d: should not have fired (%9.6f)", i, d.Seconds())
		default:
		}
	}
}

func TestSuspicion_Timer_ZeroK(t *testing.T) {
	ch := make(chan struct{}, 1)
	f := func(int) {
		ch <- struct{}{}
	}

	// This should select the min time since there are no expected
	// confirmations to accelerate the timer.
	s := newSuspicion("me", 0, 25*time.Millisecond, 30*time.Second, f)
	if s.Confirm("foo") {
		t.Fatalf("should not provide new information")
	}

	select {
	case <-ch:
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("should have fired")
	}
}

func TestSuspicion_Timer_Immediate(t *testing.T) {
	ch := make(chan struct{}, 1)
	f := func(int) {
		ch <- struct{}{}
	}

	// This should underflow the timeout and fire immediately.
	s := newSuspicion("me", 1, 100*time.Millisecond, 30*time.Second, f)
	time.Sleep(200 * time.Millisecond)
	s.Confirm("foo")

	// Wait a little while since the function gets called in a goroutine.
	select {
	case <-ch:
	case <-time.After(25 * time.Millisecond):
		t.Fatalf("should have fired")
	}
}

func TestSuspicion_Timer_FailureDomains(t *testing.T) {
	const k = 3
	const min = 500 * time.Millisecond
	const max = 2 * time.Second

	// Ten peers that all live in a single rack, simulating a rack-level
	// partition where they all lose sight of the suspect node together.
	oneRack := make(map[string]string)
	var oneRackPeers []string
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("node-%d", i)
		oneRack[name] = "rack-a"
		oneRackPeers = append(oneRackPeers, name)
	}
	threeRacks := map[string]string{
		"node-0": "rack-a",
		"node-1": "rack-b",
		"node-2": "rack-c",
	}

	cases := []struct {
		name     string
		peers    []string
		domains  map[string]string
		wantN    int
		expected time.Duration
	}{
		// Ten confirmations from a single failure domain only count as
		// one independent confirmation, so the timer stays well above
		// the minimum. It still fires even though k domains never
		// confirm, just later.
		{"ten confirmations, one domain", oneRackPeers, oneRack, 1, 1250 * time.Millisecond},
		// Three confirmations from three independent domains drive the
		// timer all the way to the minimum.
		{"three confirmations, three domains", []string{"node-0", "node-1", "node-2"}, threeRacks, 3, min},
	}
	for i, c := range cases {
		ch := make(chan time.Duration, 1)
		start := time.Now()
		f := func(numConfirmations int) {
			if numConfirmations != c.wantN {
				t.Errorf("case %d (%s): bad %d != %d", i, c.name, numConfirmations, c.wantN)
			}

			ch <- time.Since(start)
		}

		domainFn := func(node string) string { return c.domains[node] }
		s := newSuspicionWithDomains("me", k, min, max, domainFn, f)
		fudge := 25 * time.Millisecond
		for _, p := range c.peers {
			time.Sleep(fudge)
			if !s.Confirm(p) {
				t.Fatalf("case %d (%s): confirmation from %s should be new information", i, c.name, p)
			}
		}

		// Wait until right before the timeout and make sure the
		// timer hasn't fired.
		already := time.Duration(len(c.peers)) * fudge
		time.Sleep(c.expected - already - fudge)
		select {
		case d := <-ch:
			t.Fatalf("case %d (%s): should not have fired (%9.6f)", i, c.name, d.Seconds())
		default:
		}

		// Wait through the timeout and a little after and make sure it
		// fires.
		time.Sleep(2 * fudge)
		select {
		case <-ch:
		default:
			t.Fatalf("case %d (%s): should have fired", i, c.name)
		}
	}
}

func TestSuspicion_FailureDomainDedup(t *testing.T) {
	domains := map[string]string{
		"foo": "rack-a",
		"bar": "rack-a",
		"baz": "rack-b",
	}
	domainFn := func(node string) string { return domains[node] }
	s := newSuspicionWithDomains("me", 5, 25*time.Millisecond, 30*time.Second, domainFn, func(int) {})

	if !s.Confirm("foo") {
		t.Fatalf("foo should be new information")
	}

	// A node must not be able to sneak in a second confirmation by
	// changing its domain mid-stream.
	domains["foo"] = "rack-b"
	if s.Confirm("foo") {
		t.Fatalf("foo should not confirm twice after changing domains")
	}

	// A second node in an already-counted domain is new information
	// (it gets re-gossiped) but must not accelerate the timer.
	if !s.Confirm("bar") {
		t.Fatalf("bar should be new information")
	}
	if got := s.n.Load(); got != 1 {
		t.Fatalf("same-domain confirmations should count once, got n=%d", got)
	}

	// A node in a new domain counts as another independent confirmation.
	if !s.Confirm("baz") {
		t.Fatalf("baz should be new information")
	}
	if got := s.n.Load(); got != 2 {
		t.Fatalf("new domain should count, got n=%d", got)
	}
}

func TestSuspicion_FailureDomainMissing(t *testing.T) {
	// Nodes with no known domain must each count individually so that
	// missing domain information can't stall the timer.
	domainFn := func(string) string { return "" }
	s := newSuspicionWithDomains("me", 5, 25*time.Millisecond, 30*time.Second, domainFn, func(int) {})

	if !s.Confirm("foo") || !s.Confirm("bar") {
		t.Fatalf("confirmations should be new information")
	}
	if got := s.n.Load(); got != 2 {
		t.Fatalf("nodes without domains should count individually, got n=%d", got)
	}
}
