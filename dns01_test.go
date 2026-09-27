package acmez

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mholt/acmez/v3/acme"
)

// fakeDNSProvider is a controllable DNS provider that records every
// call in order, so tests can assert exactly when each TXT value was
// added and when it was allowed to be removed.
type fakeDNSProvider struct {
	mu   sync.Mutex
	log  []string
	txts map[string]map[string]int // name -> value -> count
}

func newFakeDNSProvider() *fakeDNSProvider {
	return &fakeDNSProvider{txts: make(map[string]map[string]int)}
}

func (f *fakeDNSProvider) AddTXTRecord(_ context.Context, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "ADD "+name+" "+value)
	if f.txts[name] == nil {
		f.txts[name] = make(map[string]int)
	}
	f.txts[name][value]++
	return nil
}

func (f *fakeDNSProvider) RemoveTXTRecord(_ context.Context, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.txts[name][value] == 0 {
		return fmt.Errorf("removing TXT %q %q that is not present", name, value)
	}
	f.log = append(f.log, "REMOVE "+name+" "+value)
	f.txts[name][value]--
	if f.txts[name][value] == 0 {
		delete(f.txts[name], value)
	}
	if len(f.txts[name]) == 0 {
		delete(f.txts, name)
	}
	return nil
}

func (f *fakeDNSProvider) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeDNSProvider) has(name, value string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.txts[name][value] > 0
}

func countCalls(log []string, want string) int {
	n := 0
	for _, entry := range log {
		if entry == want {
			n++
		}
	}
	return n
}

func indexOfCall(log []string, want string) int {
	for i, entry := range log {
		if entry == want {
			return i
		}
	}
	return -1
}

// TestDNS01SolverCoordinatedInterleavedOrders runs two interleaved
// orders through solvers sharing one TXTRecordCoordinator. Order 1 is
// for example.com and *.example.com, so both of its challenges use the
// same _acme-challenge name with different TXT values. Order 2 (a
// duplicate order for example.com, e.g. authorization reuse) ends up
// with the same TXT value as order 1's first challenge. Order 2 is
// canceled mid-flight; order 1 completes normally.
func TestDNS01SolverCoordinatedInterleavedOrders(t *testing.T) {
	provider := newFakeDNSProvider()
	coordinator := new(TXTRecordCoordinator)
	solver := DNS01Solver{Provider: provider, Coordinator: coordinator}
	ctx := context.Background()

	order1Apex := acme.Challenge{
		Type:             "dns-01",
		URL:              "https://ca.example/acme/chal/order1-apex",
		Identifier:       acme.Identifier{Type: "dns", Value: "example.com"},
		KeyAuthorization: "order1.apex",
	}
	order1Wild := acme.Challenge{
		Type:             "dns-01",
		URL:              "https://ca.example/acme/chal/order1-wild",
		Identifier:       acme.Identifier{Type: "dns", Value: "example.com"},
		KeyAuthorization: "order1.wild",
	}
	order2Apex := acme.Challenge{
		Type:             "dns-01",
		URL:              "https://ca.example/acme/chal/order2-apex",
		Identifier:       acme.Identifier{Type: "dns", Value: "example.com"},
		KeyAuthorization: "order1.apex", // same key authorization => same TXT value
	}

	name := order1Apex.DNS01TXTRecordName()
	sharedVal := order1Apex.DNS01KeyAuthorization()
	wildVal := order1Wild.DNS01KeyAuthorization()
	if name != order1Wild.DNS01TXTRecordName() || name != order2Apex.DNS01TXTRecordName() {
		t.Fatalf("test setup: expected all challenges to share record name, got %q %q %q",
			name, order1Wild.DNS01TXTRecordName(), order2Apex.DNS01TXTRecordName())
	}
	if sharedVal != order2Apex.DNS01KeyAuthorization() {
		t.Fatal("test setup: expected order 2 to reuse order 1's TXT value")
	}
	if sharedVal == wildVal {
		t.Fatal("test setup: expected distinct TXT values for the two order-1 challenges")
	}

	// Order 1 presents both of its challenges: two distinct values under
	// the same name, so both must be added at the provider.
	if err := solver.Present(ctx, order1Apex); err != nil {
		t.Fatalf("order1 apex present: %v", err)
	}
	if err := solver.Present(ctx, order1Wild); err != nil {
		t.Fatalf("order1 wild present: %v", err)
	}

	// Order 2 presents the same value order 1 already published: the
	// coordinator must NOT add it again.
	if err := solver.Present(ctx, order2Apex); err != nil {
		t.Fatalf("order2 apex present: %v", err)
	}
	if n := countCalls(provider.calls(), "ADD "+name+" "+sharedVal); n != 1 {
		t.Fatalf("expected shared value to be added exactly once, got %d: %v", n, provider.calls())
	}

	// Order 2 is canceled: it cleans up, but order 1 still owns the
	// shared value, so nothing may be removed yet.
	if err := solver.CleanUp(ctx, order2Apex); err != nil {
		t.Fatalf("order2 cleanup: %v", err)
	}
	if !provider.has(name, sharedVal) {
		t.Fatalf("shared value was removed while order 1 still needs it: %v", provider.calls())
	}
	if !provider.has(name, wildVal) {
		t.Fatalf("wildcard value was removed while order 1 still needs it: %v", provider.calls())
	}
	for _, entry := range provider.calls() {
		if strings.HasPrefix(entry, "REMOVE") {
			t.Fatalf("no record may be removed while order 1 is in flight, saw %q: %v", entry, provider.calls())
		}
	}

	// Order 1 completes and cleans up both challenges. Only now may the
	// shared value be removed, exactly once, and the wildcard value too.
	if err := solver.CleanUp(ctx, order1Apex); err != nil {
		t.Fatalf("order1 apex cleanup: %v", err)
	}
	if err := solver.CleanUp(ctx, order1Wild); err != nil {
		t.Fatalf("order1 wild cleanup: %v", err)
	}

	log := provider.calls()
	if n := countCalls(log, "ADD "+name+" "+sharedVal); n != 1 {
		t.Errorf("expected 1 ADD of shared value, got %d: %v", n, log)
	}
	if n := countCalls(log, "REMOVE "+name+" "+sharedVal); n != 1 {
		t.Errorf("expected 1 REMOVE of shared value, got %d: %v", n, log)
	}
	if n := countCalls(log, "ADD "+name+" "+wildVal); n != 1 {
		t.Errorf("expected 1 ADD of wildcard value, got %d: %v", n, log)
	}
	if n := countCalls(log, "REMOVE "+name+" "+wildVal); n != 1 {
		t.Errorf("expected 1 REMOVE of wildcard value, got %d: %v", n, log)
	}
	if add, rem := indexOfCall(log, "ADD "+name+" "+sharedVal), indexOfCall(log, "REMOVE "+name+" "+sharedVal); add == -1 || rem <= add {
		t.Errorf("shared value must be removed only after it was added: %v", log)
	}
	if provider.has(name, sharedVal) || provider.has(name, wildVal) {
		t.Errorf("expected all records gone after both orders finished: %v", log)
	}
	t.Logf("provider call log: %v", log)
}

// TestDNS01SolverCoordinatedSameNameDifferentValues ensures that
// cleaning up one value under a shared name never touches the other.
func TestDNS01SolverCoordinatedSameNameDifferentValues(t *testing.T) {
	provider := newFakeDNSProvider()
	solver := DNS01Solver{Provider: provider, Coordinator: new(TXTRecordCoordinator)}
	ctx := context.Background()

	chal1 := acme.Challenge{
		URL:              "https://ca.example/acme/chal/1",
		Identifier:       acme.Identifier{Type: "dns", Value: "example.com"},
		KeyAuthorization: "one",
	}
	chal2 := acme.Challenge{
		URL:              "https://ca.example/acme/chal/2",
		Identifier:       acme.Identifier{Type: "dns", Value: "example.com"},
		KeyAuthorization: "two",
	}

	if err := solver.Present(ctx, chal1); err != nil {
		t.Fatal(err)
	}
	if err := solver.Present(ctx, chal2); err != nil {
		t.Fatal(err)
	}
	if err := solver.CleanUp(ctx, chal1); err != nil {
		t.Fatal(err)
	}
	if !provider.has(chal2.DNS01TXTRecordName(), chal2.DNS01KeyAuthorization()) {
		t.Fatalf("cleaning up challenge 1 removed challenge 2's record: %v", provider.calls())
	}
	if err := solver.CleanUp(ctx, chal2); err != nil {
		t.Fatal(err)
	}
	if provider.has(chal2.DNS01TXTRecordName(), chal2.DNS01KeyAuthorization()) {
		t.Fatalf("challenge 2's record was not cleaned up: %v", provider.calls())
	}
}

// TestDNS01SolverUncoordinated documents the pass-through behavior when
// no Coordinator is configured: every Present/CleanUp goes straight to
// the provider, so sharing a solver across orders without coordination
// can remove a value another order still needs.
func TestDNS01SolverUncoordinated(t *testing.T) {
	provider := newFakeDNSProvider()
	solver := DNS01Solver{Provider: provider}
	ctx := context.Background()

	chal := acme.Challenge{
		URL:              "https://ca.example/acme/chal/1",
		Identifier:       acme.Identifier{Type: "dns", Value: "example.com"},
		KeyAuthorization: "one",
	}
	if err := solver.Present(ctx, chal); err != nil {
		t.Fatal(err)
	}
	if err := solver.CleanUp(ctx, chal); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ADD " + chal.DNS01TXTRecordName() + " " + chal.DNS01KeyAuthorization(),
		"REMOVE " + chal.DNS01TXTRecordName() + " " + chal.DNS01KeyAuthorization(),
	}
	got := provider.calls()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// TestTXTRecordCoordinatorConcurrent hammers the coordinator from
// concurrent goroutines to ensure the reference counting holds up and
// every value ends up removed exactly once.
func TestTXTRecordCoordinatorConcurrent(t *testing.T) {
	provider := newFakeDNSProvider()
	solver := DNS01Solver{Provider: provider, Coordinator: new(TXTRecordCoordinator)}
	ctx := context.Background()

	const orders = 8
	const challengesPerOrder = 4
	var wg sync.WaitGroup
	for order := 0; order < orders; order++ {
		for chal := 0; chal < challengesPerOrder; chal++ {
			challenge := acme.Challenge{
				URL:        fmt.Sprintf("https://ca.example/acme/chal/%d-%d", order, chal),
				Identifier: acme.Identifier{Type: "dns", Value: "example.com"},
				// Half the challenges across all orders share one value.
				KeyAuthorization: fmt.Sprintf("keyauth-%d", chal%2),
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := solver.Present(ctx, challenge); err != nil {
					t.Error(err)
					return
				}
				if err := solver.CleanUp(ctx, challenge); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()

	log := provider.calls()
	// A value may be added and removed several times as owners come and
	// go, but a REMOVE is only legal while the value is present, and
	// everything must be gone at the end.
	balance := make(map[string]int)
	for _, entry := range log {
		switch {
		case strings.HasPrefix(entry, "ADD "):
			balance[strings.TrimPrefix(entry, "ADD ")]++
		case strings.HasPrefix(entry, "REMOVE "):
			value := strings.TrimPrefix(entry, "REMOVE ")
			balance[value]--
			if balance[value] < 0 {
				t.Fatalf("value %q removed while not present: %v", value, log)
			}
		}
	}
	for value, n := range balance {
		if n != 0 {
			t.Errorf("value %q: %d more adds than removes: %v", value, n, log)
		}
	}
	if len(provider.txts) != 0 {
		t.Errorf("expected no leftover records, got %v", provider.txts)
	}
}
