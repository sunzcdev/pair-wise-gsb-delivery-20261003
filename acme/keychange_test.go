// Copyright 2026 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package acme

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

// droppingTransport serves requests through the fake CA but hides the
// response of requests matching drop, returning errDropped instead. The
// request still reaches the CA (which performs the key change) — the
// client just never sees the answer, exactly the §7.3.5 edge case.
type droppingTransport struct {
	ca   *fakeCA
	drop func(req *http.Request) bool

	mu sync.Mutex
}

var errDropped = errors.New("simulated connection reset after request was delivered")

func newDroppingTransport(ca *fakeCA, drop func(req *http.Request) bool) *droppingTransport {
	return &droppingTransport{ca: ca, drop: drop}
}

func (d *droppingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	d.ca.handle(rec, req)
	if d.drop != nil && d.drop(req) {
		_ = rec.Result().Body.Close()
		return nil, errDropped
	}
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// holdOrdersTransport blocks the first hold /new-order requests until
// release is closed, keeping genuine issuance requests in flight (each
// holding a real httpPostJWS key lease) so a rollover can be interleaved
// deterministically.
type holdOrdersTransport struct {
	ca      *fakeCA
	hold    int
	release chan struct{}
	started chan struct{}

	mu       sync.Mutex
	inFlight int
}

func (h *holdOrdersTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/new-order" {
		h.mu.Lock()
		h.inFlight++
		n := h.inFlight
		h.mu.Unlock()
		if n <= h.hold {
			if n == h.hold {
				close(h.started)
			}
			select {
			case <-h.release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
	}
	return h.ca.RoundTrip(req)
}

// TestRolloverLostResponseThenReconcile is the hard §7.3.5 case: the CA
// applies the key change but the keyChange response is lost. The client
// must not replay the keyChange; it reconciles by probing the account
// with the new key, finds it committed, and switches to the new key.
func TestRolloverLostResponseThenReconcile(t *testing.T) {
	ca := newFakeCA(t)

	// Drop only the keyChange response. Probes (POST-as-GET to /account/...)
	// get through, so reconciliation resolves immediately.
	transport := newDroppingTransport(ca, func(req *http.Request) bool {
		return req.URL.Path == "/key-change"
	})
	c := ca.client(transport)

	account, err := c.NewAccount(context.Background(), Account{
		Status:     StatusValid,
		PrivateKey: newTestECKey(t),
	})
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	manager := NewAccountKeyManager(account)

	oldLabel := thumb(t, manager.CurrentKey().Public())
	newKey := newTestECKey(t)
	res, err := c.Rollover(context.Background(), manager, newKey)
	if err != nil {
		t.Fatalf("Rollover should reconcile to committed, got err: %v", err)
	}
	if res.Phase != RolloverCommitted {
		t.Fatalf("Rollover phase = %s, want committed", res.Phase)
	}
	if manager.CurrentKey() != newKey {
		t.Fatal("manager did not switch to the new key")
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.keyChanges != 1 {
		t.Fatalf("processed keyChanges = %d, want exactly 1 (no automatic replay)", ca.keyChanges)
	}

	// The dropped keyChange was followed by a new-key account probe, not a
	// second keyChange. Find the keyChange use and the probes around it.
	var sawNewKeyProbe, sawOldKeyProbe bool
	for _, use := range ca.uses {
		if use.endpoint == "/key-change" {
			continue
		}
		if strings.HasPrefix(use.endpoint, "/account/") {
			if use.keyLabel == thumb(t, newKey.Public()) {
				sawNewKeyProbe = true
			}
			if use.keyLabel == oldLabel {
				sawOldKeyProbe = true
			}
		}
	}
	if !sawNewKeyProbe {
		t.Error("reconciliation did not probe the account with the new key")
	}
	if sawOldKeyProbe {
		t.Error("unexpected old-key probe; the new-key probe should have resolved it")
	}
}

// TestRolloverUndeterminableReturnsUncertain covers the case where both
// the keyChange response and the subsequent probes are lost. The client
// must report an explicit uncertain state, keep both keys, refuse further
// signing, and never send a second keyChange on its own. A later
// Resolve with connectivity restored commits the rollover.
func TestRolloverUndeterminableReturnsUncertain(t *testing.T) {
	ca := newFakeCA(t)

	// Drop the keyChange and every account probe: there is no way to tell
	// whether the CA applied the change.
	transport := newDroppingTransport(ca, func(req *http.Request) bool {
		return req.URL.Path == "/key-change" || strings.HasPrefix(req.URL.Path, "/account/")
	})
	c := ca.client(transport)

	account, err := c.NewAccount(context.Background(), Account{
		Status:     StatusValid,
		PrivateKey: newTestECKey(t),
	})
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	manager := NewAccountKeyManager(account)
	oldKey := manager.CurrentKey()
	newKey := newTestECKey(t)

	res, err := c.Rollover(context.Background(), manager, newKey)
	if err == nil {
		t.Fatal("Rollover err = nil, want an uncertain error")
	}
	if !errors.Is(err, ErrRolloverUncertain) {
		t.Fatalf("Rollover err = %v, want it to wrap ErrRolloverUncertain", err)
	}
	if res.Phase != RolloverUncertain {
		t.Fatalf("Rollover phase = %s, want uncertain", res.Phase)
	}

	var uncertain *UncertainRollover
	if !errors.As(err, &uncertain) {
		t.Fatalf("err is not *UncertainRollover: %v", err)
	}
	if uncertain.OldKey != oldKey || uncertain.NewKey != newKey {
		t.Fatal("uncertain error must retain both the old and new keys")
	}

	ca.mu.Lock()
	if ca.keyChanges != 1 {
		t.Fatalf("processed keyChanges = %d, want 1 (must not auto-cycle the key)", ca.keyChanges)
	}
	ca.mu.Unlock()

	// While uncertain, signing is refused so issuance cannot use a key
	// whose status is unknown.
	if _, err := c.NewOrder(context.Background(), manager.Account, Order{Identifiers: []Identifier{{Type: "dns", Value: "x.example"}}}); !errors.Is(err, ErrRolloverUncertain) {
		t.Fatalf("NewOrder while uncertain err = %v, want ErrRolloverUncertain", err)
	}

	// Restore connectivity: probes now get through and the CA does hold
	// the new key. An explicit Resolve commits the rollover; it still does
	// not send another keyChange.
	transport.mu.Lock()
	transport.drop = nil
	transport.mu.Unlock()

	resolved, err := c.Resolve(context.Background(), manager)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Phase != RolloverCommitted {
		t.Fatalf("Resolve phase = %s, want committed", resolved.Phase)
	}
	if manager.CurrentKey() != newKey {
		t.Fatal("manager did not adopt the new key after resolve")
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.keyChanges != 1 {
		t.Fatalf("processed keyChanges after resolve = %d, still want 1", ca.keyChanges)
	}
}

func registerManagedAccount(t *testing.T, ca *fakeCA) (*Client, *AccountKeyManager) {
	t.Helper()
	c := ca.client(nil)
	account, err := c.NewAccount(context.Background(), Account{
		Status:     StatusValid,
		PrivateKey: newTestECKey(t),
	})
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	return c, NewAccountKeyManager(account)
}

// TestRolloverDefinitiveRejection: a CA-answered rejection (an ACME
// Problem) means the old key stays in effect. The client must not treat
// this as uncertain and must not reconcile or replay the keyChange.
func TestRolloverDefinitiveRejection(t *testing.T) {
	ca := newFakeCA(t)
	c := ca.client(nil)

	account, err := c.NewAccount(context.Background(), Account{
		Status:     StatusValid,
		PrivateKey: newTestECKey(t),
	})
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	manager := NewAccountKeyManager(account)
	keyA := manager.CurrentKey()
	keyB := newTestECKey(t)
	keyC := newTestECKey(t)

	// First rollover succeeds: the CA now holds key B.
	if res, err := c.Rollover(context.Background(), manager, keyB); err != nil || res.Phase != RolloverCommitted {
		t.Fatalf("first Rollover: phase=%s err=%v", res.Phase, err)
	}

	// Forge a second keyChange "from the past": an unmanaged account copy
	// that still presents A as its key, asking to switch to C. The outer
	// signature is by A, which the CA no longer holds, so it rejects with
	// a definitive Problem.
	stale := NewAccountKeyManager(Account{
		Status:     StatusValid,
		Location:   manager.Location,
		PrivateKey: keyA,
	})
	res, err := c.Rollover(context.Background(), stale, keyC)
	if err == nil {
		t.Fatal("second Rollover err = nil, want a definitive rejection")
	}
	var problem Problem
	if !errors.As(err, &problem) {
		t.Fatalf("err = %v, want an ACME Problem", err)
	}
	if res.Phase != RolloverRejected {
		t.Fatalf("phase = %s, want rejected", res.Phase)
	}

	// The stale manager keeps A; the real manager is untouched on B.
	if manager.CurrentKey() != keyB {
		t.Fatal("committed manager moved off key B")
	}
	if stale.CurrentKey() != keyA {
		t.Fatal("rejected manager moved off key A")
	}

	// B still works, proving no reconciliation/replay disturbed the CA.
	if _, err := c.NewOrder(context.Background(), manager.Account, Order{Identifiers: []Identifier{{Type: "dns", Value: "still-b.example"}}}); err != nil {
		t.Fatalf("NewOrder with key B after rejection: %v", err)
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.keyChanges != 1 {
		t.Fatalf("processed keyChanges = %d, want 1 (the rejected attempt must not replay)", ca.keyChanges)
	}
}

// TestRolloverHappyPath verifies the plain boundary: issuance requests
// before the rollover authenticate with the old key, the keyChange is
// signed outside by the old key and inside by the new key, and requests
// after the rollover authenticate with the new key.
func TestRolloverHappyPath(t *testing.T) {
	ca := newFakeCA(t)
	c, account := registerManagedAccount(t, ca)
	ctx := context.Background()

	oldLabel := thumb(t, account.CurrentKey().Public())
	newKey := newTestECKey(t)
	newLabel := thumb(t, newKey.Public())

	if _, err := c.NewOrder(ctx, account.Account, Order{Identifiers: []Identifier{{Type: "dns", Value: "example.com"}}}); err != nil {
		t.Fatalf("pre-rollover NewOrder: %v", err)
	}

	res, err := c.Rollover(ctx, account, newKey)
	if err != nil || res.Phase != RolloverCommitted {
		t.Fatalf("Rollover: phase=%s err=%v", res.Phase, err)
	}

	if _, err := c.NewOrder(ctx, account.Account, Order{Identifiers: []Identifier{{Type: "dns", Value: "example.org"}}}); err != nil {
		t.Fatalf("post-rollover NewOrder: %v", err)
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.keyChanges != 1 {
		t.Fatalf("processed keyChanges = %d, want 1", ca.keyChanges)
	}

	// Expect, in order: newAccount(old), order(old), keyChange outer(old)/
	// inner(new), order(new).
	want := []jwsUse{
		{endpoint: "/new-account", keyLabel: oldLabel},
		{endpoint: "/new-order", keyLabel: oldLabel},
		{endpoint: "/key-change", keyLabel: oldLabel, innerLabel: newLabel},
		{endpoint: "/new-order", keyLabel: newLabel},
	}
	if len(ca.uses) != len(want) {
		t.Fatalf("recorded %d JWS uses, want %d: %+v", len(ca.uses), len(want), ca.uses)
	}
	for i, got := range ca.uses {
		if got != want[i] {
			t.Errorf("use %d = %+v, want %+v", i, got, want[i])
		}
	}
}

// TestRolloverDrainsConcurrentIssuance defines the concurrent boundary:
// a rollover waits for old-key requests already in flight, requests that
// arrive once it is queued block behind it, and everything after the
// boundary uses the new key. The fake CA records the signer key per
// request, so the resulting timeline is asserted directly.
func TestRolloverDrainsConcurrentIssuance(t *testing.T) {
	ca := newFakeCA(t)
	c, account := registerManagedAccount(t, ca)
	ctx := context.Background()

	oldLabel := thumb(t, account.CurrentKey().Public())
	newKey := newTestECKey(t)
	newLabel := thumb(t, newKey.Public())

	// Two in-flight old-key requests, represented by held read leases
	// (exactly what httpPostJWS holds while a request is outstanding).
	releaseInflight := make(chan struct{})
	var holders sync.WaitGroup
	for range 2 {
		holders.Add(1)
		if err := account.gate.beginRead(ctx); err != nil {
			t.Fatalf("beginRead: %v", err)
		}
		go func() {
			defer holders.Done()
			<-releaseInflight
			account.gate.endRead()
		}()
	}

	// Queue the rollover first and wait until it is parked behind the two
	// held old-key leases. This pins the ordering before any late request.
	rolloverDone := make(chan RolloverResult, 1)
	go func() {
		res, _ := c.Rollover(ctx, account, newKey)
		rolloverDone <- res
	}()

	waitFor(t, account.gate.writerQueued)

	// A "late" issuance request, submitted only once the rollover is
	// queued. It must block until the boundary is crossed and then use the
	// new key (writer-preference parks it behind the rollover).
	lateDone := make(chan error, 1)
	go func() {
		_, err := c.NewOrder(ctx, account.Account, Order{Identifiers: []Identifier{{Type: "dns", Value: "late.example"}}})
		lateDone <- err
	}()

	select {
	case err := <-lateDone:
		t.Fatalf("late order ran before the boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseInflight)

	select {
	case res := <-rolloverDone:
		if res.Phase != RolloverCommitted {
			t.Fatalf("Rollover phase = %s, want committed", res.Phase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rollover did not complete after old-key requests drained")
	}
	holders.Wait()

	select {
	case err := <-lateDone:
		if err != nil {
			t.Fatalf("late order after boundary: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late order stayed blocked after rollover")
	}

	// Only the late order actually hit /new-order, and with the new key.
	ca.mu.Lock()
	defer ca.mu.Unlock()
	var orderKeys []string
	for _, use := range ca.uses {
		if use.endpoint == "/new-order" {
			orderKeys = append(orderKeys, use.keyLabel)
		}
		if use.endpoint == "/key-change" {
			if use.keyLabel != oldLabel || use.innerLabel != newLabel {
				t.Fatalf("keyChange keys = outer %s inner %s, want %s/%s",
					use.keyLabel, use.innerLabel, oldLabel, newLabel)
			}
		}
	}
	if len(orderKeys) != 1 || orderKeys[0] != newLabel {
		t.Fatalf("order keys = %v, want exactly [%s]", orderKeys, newLabel)
	}
	if ca.keyChanges != 1 {
		t.Fatalf("processed keyChanges = %d, want 1", ca.keyChanges)
	}
}

// TestConcurrentIssuanceInterleavedWithRollover drives real issuance
// requests (full httpPostJWS lease lifecycle) concurrently with a
// rollover, using the fake CA's per-JWS key log to assert the boundary:
// every order that was in flight before the keyChange used the old key,
// the keyChange is outer-old/inner-new exactly once, and every order
// after the boundary used the new key.
func TestConcurrentIssuanceInterleavedWithRollover(t *testing.T) {
	ca := newFakeCA(t)
	ctx := context.Background()

	hold := &holdOrdersTransport{
		ca:      ca,
		hold:    3,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	c := ca.client(hold)

	account, err := c.NewAccount(ctx, Account{Status: StatusValid, PrivateKey: newTestECKey(t)})
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	manager := NewAccountKeyManager(account)
	oldLabel := thumb(t, manager.CurrentKey().Public())
	newKey := newTestECKey(t)
	newLabel := thumb(t, newKey.Public())

	issue := func(sem chan<- error) {
		_, err := c.NewOrder(ctx, manager.Account, Order{
			Identifiers: []Identifier{{Type: "dns", Value: "issuing.example"}},
		})
		sem <- err
	}

	// Three in-flight orders that will park inside the transport while
	// holding their old-key lease.
	inFlight := make(chan error, hold.hold)
	for i := 0; i < hold.hold; i++ {
		go issue(inFlight)
	}
	select {
	case <-hold.started:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight orders never reached the transport")
	}

	// Rollover must wait for all three leases.
	rolloverDone := make(chan RolloverResult, 1)
	go func() {
		res, _ := c.Rollover(ctx, manager, newKey)
		rolloverDone <- res
	}()
	waitFor(t, manager.gate.writerQueued)

	// Orders issued once the rollover is queued must not pass before it.
	afterDone := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go issue(afterDone)
	}
	select {
	case err := <-afterDone:
		t.Fatalf("post-boundary order ran early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Drain the in-flight orders; rollover completes, then the rest run.
	close(hold.release)
	select {
	case res := <-rolloverDone:
		if res.Phase != RolloverCommitted {
			t.Fatalf("Rollover phase = %s, want committed", res.Phase)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rollover did not complete after in-flight orders drained")
	}
	for i := 0; i < hold.hold; i++ {
		if err := <-inFlight; err != nil {
			t.Fatalf("in-flight order: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := <-afterDone; err != nil {
			t.Fatalf("post-boundary order: %v", err)
		}
	}

	// Assert the recorded timeline: the three in-flight orders precede the
	// single keyChange and used the old key; the two later orders used the
	// new key.
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.keyChanges != 1 {
		t.Fatalf("processed keyChanges = %d, want 1", ca.keyChanges)
	}

	var orderLabels []string
	keyChangeSeen := false
	for _, use := range ca.uses {
		switch {
		case use.endpoint == "/new-order":
			if !keyChangeSeen {
				if use.keyLabel != oldLabel {
					t.Fatalf("in-flight order used key %s, want old %s", use.keyLabel, oldLabel)
				}
			} else if use.keyLabel != newLabel {
				t.Fatalf("post-boundary order used key %s, want new %s", use.keyLabel, newLabel)
			}
			orderLabels = append(orderLabels, use.keyLabel)
		case use.endpoint == "/key-change":
			if keyChangeSeen {
				t.Fatal("more than one keyChange in timeline")
			}
			if use.keyLabel != oldLabel || use.innerLabel != newLabel {
				t.Fatalf("keyChange keys outer=%s inner=%s, want %s/%s",
					use.keyLabel, use.innerLabel, oldLabel, newLabel)
			}
			keyChangeSeen = true
		}
	}
	if len(orderLabels) != 5 {
		t.Fatalf("recorded %d orders, want 5", len(orderLabels))
	}
}
