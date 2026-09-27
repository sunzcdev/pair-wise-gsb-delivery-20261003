// Copyright IBM Corp. 2013, 2025
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
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
	const min = 400 * time.Millisecond
	const max = 4400 * time.Millisecond

	// With k = 3 a single independent confirmation drives the timeout to
	// the midpoint: 4400ms - log(2)/log(4) * (4400ms - 400ms) = 2400ms,
	// while k independent confirmations drive it all the way to min.
	const singleDomainExpected = 2400 * time.Millisecond

	run := func(t *testing.T, confirmations []string, domainOf func(string) (string, bool)) (time.Duration, int) {
		ch := make(chan time.Duration, 1)
		var numConfirmations int
		start := time.Now()
		f := func(n int) {
			numConfirmations = n
			ch <- time.Since(start)
		}
		s := newSuspicionWithDomain("me", k, min, max, f, domainOf)
		for _, from := range confirmations {
			s.Confirm(from)
		}
		select {
		case d := <-ch:
			return d, numConfirmations
		case <-time.After(max + 2*time.Second):
			t.Fatalf("should have fired")
			return 0, 0
		}
	}

	// Ten confirmations that all come from a single failure domain (for
	// example, one rack that lost sight of the target) must count as only
	// one independent confirmation.
	oneRack := func(string) (string, bool) { return "rack-a", true }
	singleDomainNodes := []string{"n0", "n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8", "n9"}
	singleElapsed, singleN := run(t, singleDomainNodes, oneRack)
	if singleN != 1 {
		t.Fatalf("single domain: bad independent confirmations %d != 1", singleN)
	}
	if singleElapsed < singleDomainExpected || singleElapsed > singleDomainExpected+500*time.Millisecond {
		t.Fatalf("single domain: fired after %9.6f, expected around %9.6f",
			singleElapsed.Seconds(), singleDomainExpected.Seconds())
	}

	// Three confirmations from three independent failure domains must
	// drive the timer to the minimum.
	racks := map[string]string{"n0": "rack-a", "n1": "rack-b", "n2": "rack-c"}
	threeRacks := func(node string) (string, bool) {
		rack, ok := racks[node]
		return rack, ok
	}
	multiElapsed, multiN := run(t, []string{"n0", "n1", "n2"}, threeRacks)
	if multiN != 3 {
		t.Fatalf("three domains: bad independent confirmations %d != 3", multiN)
	}
	if multiElapsed > min+500*time.Millisecond {
		t.Fatalf("three domains: fired after %9.6f, expected around %9.6f",
			multiElapsed.Seconds(), min.Seconds())
	}

	// The correlated confirmations must result in a strictly later
	// decision than the independent ones.
	if multiElapsed >= singleElapsed {
		t.Fatalf("three domains (%9.6f) should decide faster than one domain (%9.6f)",
			multiElapsed.Seconds(), singleElapsed.Seconds())
	}
}

func TestSuspicion_FailureDomain_NodeSwitchingDomains(t *testing.T) {
	domains := map[string]string{"n1": "rack-a", "n2": "rack-b"}
	domainOf := func(node string) (string, bool) {
		domain, ok := domains[node]
		return domain, ok
	}
	s := newSuspicionWithDomain("me", 3, 100*time.Millisecond, 30*time.Second, func(int) {}, domainOf)

	if !s.Confirm("n1") {
		t.Fatalf("first confirmation should be new info")
	}
	if n := s.n.Load(); n != 1 {
		t.Fatalf("bad independent confirmations %d != 1", n)
	}

	// A node that switches domains mid-way must not bypass the dedup and
	// count a second time.
	domains["n1"] = "rack-c"
	if s.Confirm("n1") {
		t.Fatalf("confirmation from the same node should not be new info, even after a domain change")
	}
	if n := s.n.Load(); n != 1 {
		t.Fatalf("domain switch must not add an independent confirmation, got %d", n)
	}

	// A different node in a new domain counts as independent.
	if !s.Confirm("n2") {
		t.Fatalf("confirmation from a new domain should be new info")
	}
	if n := s.n.Load(); n != 2 {
		t.Fatalf("bad independent confirmations %d != 2", n)
	}

	// A different node in an already-seen domain is new info worth
	// gossiping, but must not accelerate the timer.
	domains["n3"] = "rack-a"
	if !s.Confirm("n3") {
		t.Fatalf("confirmation from a new node should be new info")
	}
	if n := s.n.Load(); n != 2 {
		t.Fatalf("correlated confirmation must not add an independent confirmation, got %d", n)
	}
}

func TestSuspicion_FailureDomain_MissingDomain(t *testing.T) {
	// Nodes without any domain information fall back to plain node
	// counting so missing data can never stall failure detection.
	domainOf := func(string) (string, bool) { return "", false }

	ch := make(chan int, 1)
	start := time.Now()
	s := newSuspicionWithDomain("me", 3, 400*time.Millisecond, 30*time.Second, func(n int) {
		ch <- n
	}, domainOf)
	s.Confirm("n1")
	s.Confirm("n2")
	s.Confirm("n3")

	select {
	case n := <-ch:
		if n != 3 {
			t.Fatalf("bad independent confirmations %d != 3", n)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("should have fired near the minimum timeout, took %9.6f", elapsed.Seconds())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("should have fired")
	}
}
