package acmez_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mholt/acmez/v3"
	"github.com/mholt/acmez/v3/acme"
)

// fakeDNSProvider is a controllable DNS provider for tests. It keeps a
// multiset of TXT records per name and logs every mutation so tests can
// verify exactly when each value was added and when it was removed.
// Optional hooks allow tests to trigger interleaved actions at precise
// points during provider calls.
type fakeDNSProvider struct {
	mu      sync.Mutex
	records map[string]map[string]int // name -> value -> count
	log     []string

	onAppend func(name, value string)
	onRemove func(name, value string)
}

func newFakeDNSProvider() *fakeDNSProvider {
	return &fakeDNSProvider{records: make(map[string]map[string]int)}
}

func (f *fakeDNSProvider) AppendTXTRecord(ctx context.Context, name, value string, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	if f.records[name] == nil {
		f.records[name] = make(map[string]int)
	}
	f.records[name][value]++
	f.log = append(f.log, fmt.Sprintf("add %s %q", name, value))
	f.mu.Unlock()
	if f.onAppend != nil {
		f.onAppend(name, value)
	}
	return nil
}

func (f *fakeDNSProvider) RemoveTXTRecord(ctx context.Context, name, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	if f.records[name][value] <= 1 {
		delete(f.records[name], value)
	} else {
		f.records[name][value]--
	}
	f.log = append(f.log, fmt.Sprintf("remove %s %q", name, value))
	f.mu.Unlock()
	if f.onRemove != nil {
		f.onRemove(name, value)
	}
	return nil
}

func (f *fakeDNSProvider) values(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var values []string
	for value := range f.records[name] {
		values = append(values, value)
	}
	slices.Sort(values)
	return values
}

func (f *fakeDNSProvider) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// TestDNS01CoordinatorInterleavedOrders runs two interleaved orders
// against one shared coordinator: each order is for a wildcard and its
// base domain, so both orders publish to the same _acme-challenge name.
// One value is identical across the two orders (same key authorization),
// the other values differ. Order A is cancelled mid-way; order B must
// complete normally, and no record may be removed while another order
// still owns it.
func TestDNS01CoordinatorInterleavedOrders(t *testing.T) {
	provider := newFakeDNSProvider()
	solver := &acmez.DNS01Solver{
		Provider:    provider,
		Coordinator: new(acmez.DNS01Coordinator),
		TTL:         time.Minute,
	}

	const recordName = "_acme-challenge.example.com"
	dnsChallenge := func(url, keyAuth string) acme.Challenge {
		return acme.Challenge{
			Type:             acme.ChallengeTypeDNS01,
			URL:              "https://ca.example.com/acme/" + url,
			Identifier:       acme.Identifier{Type: "dns", Value: "example.com"},
			KeyAuthorization: keyAuth,
		}
	}

	// order A (will be cancelled): wildcard + base domain
	chalA1 := dnsChallenge("orderA/chal1", "shared-token.account-thumb") // same value as chalB1
	chalA2 := dnsChallenge("orderA/chal2", "token-a2.account-thumb")
	// order B (completes normally): wildcard + base domain
	chalB1 := dnsChallenge("orderB/chal1", "shared-token.account-thumb")
	chalB2 := dnsChallenge("orderB/chal2", "token-b2.account-thumb")

	valShared := chalA1.DNS01KeyAuthorization()
	valA := chalA2.DNS01KeyAuthorization()
	valB := chalB2.DNS01KeyAuthorization()
	if valShared != chalB1.DNS01KeyAuthorization() {
		t.Fatal("test setup: expected chalA1 and chalB1 to share the same TXT value")
	}

	// the provider signals when the shared value is appended; it must
	// happen exactly once even though two orders present it
	sharedAppended := make(chan struct{})
	provider.onAppend = func(name, value string) {
		if value == valShared {
			close(sharedAppended)
		}
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB := context.Background()
	// cleanup must still be possible after order A's context is cancelled,
	// so order A cleans up with a context detached from the cancellation
	cleanupCtxA := context.WithoutCancel(ctxA)

	orderAPresented := make(chan struct{})
	orderACleaned := make(chan struct{})
	orderBPresented := make(chan struct{})
	orderBProceed := make(chan struct{})
	orderBCleaned := make(chan struct{})
	errs := make(chan error, 8)

	// order A: present both challenges, get cancelled, then clean up
	go func() {
		if err := solver.Present(ctxA, chalA1); err != nil {
			errs <- fmt.Errorf("order A present chalA1: %w", err)
		}
		if err := solver.Present(ctxA, chalA2); err != nil {
			errs <- fmt.Errorf("order A present chalA2: %w", err)
		}
		close(orderAPresented)
		<-ctxA.Done() // cancelled mid-way
		if err := solver.CleanUp(cleanupCtxA, chalA1); err != nil {
			errs <- fmt.Errorf("order A cleanup chalA1: %w", err)
		}
		if err := solver.CleanUp(cleanupCtxA, chalA2); err != nil {
			errs <- fmt.Errorf("order A cleanup chalA2: %w", err)
		}
		close(orderACleaned)
	}()

	// order B: present both challenges once the shared value is up,
	// complete normally, then clean up
	go func() {
		<-sharedAppended
		<-orderAPresented
		if err := solver.Present(ctxB, chalB1); err != nil {
			errs <- fmt.Errorf("order B present chalB1: %w", err)
		}
		if err := solver.Present(ctxB, chalB2); err != nil {
			errs <- fmt.Errorf("order B present chalB2: %w", err)
		}
		close(orderBPresented)
		<-orderBProceed // order A was cancelled and cleaned up meanwhile
		if err := solver.CleanUp(ctxB, chalB1); err != nil {
			errs <- fmt.Errorf("order B cleanup chalB1: %w", err)
		}
		if err := solver.CleanUp(ctxB, chalB2); err != nil {
			errs <- fmt.Errorf("order B cleanup chalB2: %w", err)
		}
		close(orderBCleaned)
	}()

	<-orderBPresented

	// all three values must coexist under the same name before cleanup
	if got, want := provider.values(recordName), slices.Sorted(slices.Values([]string{valA, valB, valShared})); !slices.Equal(got, want) {
		t.Errorf("after both orders presented: records = %v, want %v", got, want)
	}

	// cancel order A mid-way; its cleanup runs with a detached context
	cancelA()
	<-orderACleaned

	// order A's own value is gone, but the shared value (still owned by
	// order B) and order B's own value must remain
	if got, want := provider.values(recordName), slices.Sorted(slices.Values([]string{valB, valShared})); !slices.Equal(got, want) {
		t.Errorf("after order A cleanup: records = %v, want %v", got, want)
	}

	// let order B finish and clean up
	close(orderBProceed)
	<-orderBCleaned

	// everything must be gone once the last owners cleaned up
	if got := provider.values(recordName); len(got) != 0 {
		t.Errorf("after order B cleanup: records = %v, want none", got)
	}

	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// the provider log must show that the shared value was added exactly
	// once, that each distinct value was added independently, and that a
	// value was only removed once its last owner had cleaned up
	wantLog := []string{
		fmt.Sprintf("add %s %q", recordName, valShared),    // order A presents shared value
		fmt.Sprintf("add %s %q", recordName, valA),         // order A presents its own value
		fmt.Sprintf("add %s %q", recordName, valB),         // order B presents its own value (shared value NOT re-added)
		fmt.Sprintf("remove %s %q", recordName, valA),      // order A cancelled: only its own value removed
		fmt.Sprintf("remove %s %q", recordName, valShared), // order B done: shared value finally removed
		fmt.Sprintf("remove %s %q", recordName, valB),      // order B done: its own value removed
	}
	if got := provider.callLog(); !slices.Equal(got, wantLog) {
		t.Errorf("provider call log:\n got: %v\nwant: %v", got, wantLog)
	}
}

// TestDNS01CoordinatorEdgeCases verifies idempotency and no-op behavior
// of the coordination layer.
func TestDNS01CoordinatorEdgeCases(t *testing.T) {
	provider := newFakeDNSProvider()
	solver := &acmez.DNS01Solver{
		Provider:    provider,
		Coordinator: new(acmez.DNS01Coordinator),
	}
	ctx := context.Background()
	chal := acme.Challenge{
		Type:             acme.ChallengeTypeDNS01,
		URL:              "https://ca.example.com/acme/chal1",
		Identifier:       acme.Identifier{Type: "dns", Value: "example.com"},
		KeyAuthorization: "token.thumb",
	}

	// cleanup without present must be a no-op
	if err := solver.CleanUp(ctx, chal); err != nil {
		t.Fatalf("cleanup without present: %v", err)
	}
	if got := provider.callLog(); len(got) != 0 {
		t.Errorf("cleanup without present made provider calls: %v", got)
	}

	// double present by the same owner appends only once
	if err := solver.Present(ctx, chal); err != nil {
		t.Fatalf("present: %v", err)
	}
	if err := solver.Present(ctx, chal); err != nil {
		t.Fatalf("present again: %v", err)
	}
	if got := provider.values("_acme-challenge.example.com"); len(got) != 1 {
		t.Errorf("after double present: records = %v, want 1 value", got)
	}

	// double cleanup removes only once and stays a no-op afterwards
	if err := solver.CleanUp(ctx, chal); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := solver.CleanUp(ctx, chal); err != nil {
		t.Fatalf("cleanup again: %v", err)
	}
	if got := provider.values("_acme-challenge.example.com"); len(got) != 0 {
		t.Errorf("after double cleanup: records = %v, want none", got)
	}
	wantLog := []string{
		fmt.Sprintf("add %s %q", "_acme-challenge.example.com", chal.DNS01KeyAuthorization()),
		fmt.Sprintf("remove %s %q", "_acme-challenge.example.com", chal.DNS01KeyAuthorization()),
	}
	if got := provider.callLog(); !slices.Equal(got, wantLog) {
		t.Errorf("provider call log:\n got: %v\nwant: %v", got, wantLog)
	}
}
