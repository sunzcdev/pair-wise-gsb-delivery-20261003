package dns

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func testZone(t *testing.T, serial uint32, records ...string) []RR {
	t.Helper()
	soa, err := NewRR(fmt.Sprintf("example.com. 3600 IN SOA ns1.example.com. admin.example.com. %d 7200 3600 1209600 3600", serial))
	if err != nil {
		t.Fatal(err)
	}
	zone := []RR{soa}
	for _, s := range records {
		rr, err := NewRR(s)
		if err != nil {
			t.Fatal(err)
		}
		zone = append(zone, rr)
	}
	return zone
}

func rrStrings(rrs []RR) []string {
	s := make([]string, len(rrs))
	for i, rr := range rrs {
		s[i] = rr.String()
	}
	return s
}

func equalRRSet(a, b []RR) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, rr := range a {
		set[rr.String()]++
	}
	for _, rr := range b {
		set[rr.String()]--
		if set[rr.String()] < 0 {
			return false
		}
	}
	return true
}

func ixfrChannel(envs ...*Envelope) chan *Envelope {
	ch := make(chan *Envelope, len(envs))
	for _, e := range envs {
		ch <- e
	}
	close(ch)
	return ch
}

func TestGenerateIXFR(t *testing.T) {
	old := testZone(t, 1,
		"a.example.com. 3600 IN A 1.2.3.4",
		"b.example.com. 3600 IN A 5.6.7.8",
		"c.example.com. 300 IN A 9.9.9.9",
	)
	new := testZone(t, 2,
		"a.example.com. 3600 IN A 1.2.3.4",
		"c.example.com. 600 IN A 9.9.9.9", // TTL change: must show up as delete+add
		"d.example.com. 3600 IN A 10.0.0.1",
	)

	ixfr, err := GenerateIXFR(old, new)
	if err != nil {
		t.Fatal(err)
	}
	want := testZone(t, 2)[:1]
	want = append(want, testZone(t, 1)[:1]...)
	want = append(want,
		mustRR(t, "b.example.com. 3600 IN A 5.6.7.8"),
		mustRR(t, "c.example.com. 300 IN A 9.9.9.9"),
	)
	want = append(want, testZone(t, 2)[:1]...)
	want = append(want,
		mustRR(t, "c.example.com. 600 IN A 9.9.9.9"),
		mustRR(t, "d.example.com. 3600 IN A 10.0.0.1"),
	)
	want = append(want, testZone(t, 2)[:1]...)

	got, wantStr := rrStrings(ixfr), rrStrings(want)
	if len(got) != len(wantStr) {
		t.Fatalf("got %d RRs, want %d:\n%q", len(got), len(wantStr), got)
	}
	for i := range got {
		if got[i] != wantStr[i] {
			t.Errorf("RR %d: got %q, want %q", i, got[i], wantStr[i])
		}
	}
}

func mustRR(t *testing.T, s string) RR {
	t.Helper()
	rr, err := NewRR(s)
	if err != nil {
		t.Fatal(err)
	}
	return rr
}

func TestGenerateIXFRSerial(t *testing.T) {
	// Equal serials: no changes, a single SOA is returned.
	ixfr, err := GenerateIXFR(testZone(t, 7), testZone(t, 7))
	if err != nil {
		t.Fatal(err)
	}
	if len(ixfr) != 1 || ixfr[0].(*SOA).Serial != 7 {
		t.Fatalf("expected a single SOA, got %v", rrStrings(ixfr))
	}

	// Wraparound: 4294967295 -> 1 is a valid increment (RFC 1982).
	old := testZone(t, 4294967295, "a.example.com. 3600 IN A 1.2.3.4")
	new := testZone(t, 1, "b.example.com. 3600 IN A 5.6.7.8")
	ixfr, err = GenerateIXFR(old, new)
	if err != nil {
		t.Fatal(err)
	}
	if len(ixfr) != 6 {
		t.Fatalf("expected 6 RRs, got %v", rrStrings(ixfr))
	}

	// Incomparable: exactly 2^31 apart.
	if _, err := GenerateIXFR(testZone(t, 0), testZone(t, 1<<31)); !errors.Is(err, ErrSerialCompare) {
		t.Fatalf("expected ErrSerialCompare, got %v", err)
	}

	// New serial before old serial.
	if _, err := GenerateIXFR(testZone(t, 10), testZone(t, 5)); !errors.Is(err, ErrSerialOrder) {
		t.Fatalf("expected ErrSerialOrder, got %v", err)
	}
}

func TestGenerateIXFRDuplicates(t *testing.T) {
	old := testZone(t, 1,
		"a.example.com. 3600 IN A 1.2.3.4",
		"a.example.com. 3600 IN A 1.2.3.4", // duplicate in the input
	)
	new := testZone(t, 2)

	ixfr, err := GenerateIXFR(old, new)
	if err != nil {
		t.Fatal(err)
	}
	// SOA(2) SOA(1) A SOA(2) SOA(2): the duplicate must be deleted only once.
	if len(ixfr) != 5 {
		t.Fatalf("expected 5 RRs, got %v", rrStrings(ixfr))
	}
}

func TestApplyIXFR(t *testing.T) {
	old := testZone(t, 1,
		"a.example.com. 3600 IN A 1.2.3.4",
		"b.example.com. 3600 IN A 5.6.7.8",
	)
	new := testZone(t, 2,
		"a.example.com. 3600 IN A 1.2.3.4",
		"c.example.com. 3600 IN A 9.9.9.9",
	)
	ixfr, err := GenerateIXFR(old, new)
	if err != nil {
		t.Fatal(err)
	}

	store := NewMemZoneStore(old)
	if err := ApplyIXFR(context.Background(), store, ixfrChannel(&Envelope{RR: ixfr})); err != nil {
		t.Fatal(err)
	}
	if !equalRRSet(store.All(), new) {
		t.Fatalf("got %v, want %v", rrStrings(store.All()), rrStrings(new))
	}
}

func TestApplyIXFRMultiSegment(t *testing.T) {
	// Two deltas: 1 -> 2 -> 3.
	stream := []RR{
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 3 7200 3600 1209600 3600"),
		// delta 1 -> 2
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 3600"),
		mustRR(t, "gone.example.com. 3600 IN A 1.1.1.1"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 2 7200 3600 1209600 3600"),
		mustRR(t, "x.example.com. 3600 IN A 2.2.2.2"),
		// delta 2 -> 3
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 2 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 3 7200 3600 1209600 3600"),
		mustRR(t, "y.example.com. 3600 IN A 3.3.3.3"),
		// final SOA
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 3 7200 3600 1209600 3600"),
	}
	store := NewMemZoneStore(testZone(t, 1,
		"gone.example.com. 3600 IN A 1.1.1.1",
		"stay.example.com. 3600 IN A 4.4.4.4",
	))
	// Split the stream over multiple envelopes to mimic multi-message transfers.
	ch := ixfrChannel(&Envelope{RR: stream[:4]}, &Envelope{RR: stream[4:]})
	if err := ApplyIXFR(context.Background(), store, ch); err != nil {
		t.Fatal(err)
	}
	want := testZone(t, 3,
		"stay.example.com. 3600 IN A 4.4.4.4",
		"x.example.com. 3600 IN A 2.2.2.2",
		"y.example.com. 3600 IN A 3.3.3.3",
	)
	if !equalRRSet(store.All(), want) {
		t.Fatalf("got %v, want %v", rrStrings(store.All()), rrStrings(want))
	}
}

func TestApplyIXFRAXFRFallback(t *testing.T) {
	// Server falls back to AXFR: SOA, all records, SOA.
	stream := testZone(t, 9,
		"only.example.com. 3600 IN A 8.8.8.8",
	)
	stream = append(stream, stream[0])

	before := testZone(t, 1, "old.example.com. 3600 IN A 1.1.1.1")
	store := NewMemZoneStore(before)
	if err := ApplyIXFR(context.Background(), store, ixfrChannel(&Envelope{RR: stream})); err != nil {
		t.Fatal(err)
	}
	want := testZone(t, 9, "only.example.com. 3600 IN A 8.8.8.8")
	if !equalRRSet(store.All(), want) {
		t.Fatalf("got %v, want %v", rrStrings(store.All()), rrStrings(want))
	}
}

func TestApplyIXFRUpToDate(t *testing.T) {
	before := testZone(t, 5, "a.example.com. 3600 IN A 1.2.3.4")
	store := NewMemZoneStore(before)
	ch := ixfrChannel(&Envelope{RR: testZone(t, 5)[:1]})
	if err := ApplyIXFR(context.Background(), store, ch); err != nil {
		t.Fatal(err)
	}
	if !equalRRSet(store.All(), before) {
		t.Fatalf("zone changed: %v", rrStrings(store.All()))
	}
}

func TestApplyIXFRMissingFinal(t *testing.T) {
	before := testZone(t, 1, "a.example.com. 3600 IN A 1.2.3.4")

	// The final SOA is missing.
	stream := []RR{
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 2 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 2 7200 3600 1209600 3600"),
		mustRR(t, "b.example.com. 3600 IN A 5.6.7.8"),
	}
	store := NewMemZoneStore(before)
	if err := ApplyIXFR(context.Background(), store, ixfrChannel(&Envelope{RR: stream})); !errors.Is(err, ErrIXFRFormat) {
		t.Fatalf("expected ErrIXFRFormat, got %v", err)
	}
	if !equalRRSet(store.All(), before) {
		t.Fatalf("store modified on failed apply: %v", rrStrings(store.All()))
	}

	// The last delta never reaches the final serial: the stream just ends.
	stream = []RR{
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 3 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 2 7200 3600 1209600 3600"),
	}
	store = NewMemZoneStore(before)
	if err := ApplyIXFR(context.Background(), store, ixfrChannel(&Envelope{RR: stream})); !errors.Is(err, ErrIXFRFormat) {
		t.Fatalf("expected ErrIXFRFormat, got %v", err)
	}
	if !equalRRSet(store.All(), before) {
		t.Fatalf("store modified on failed apply: %v", rrStrings(store.All()))
	}
}

func TestApplyIXFRBrokenChain(t *testing.T) {
	before := testZone(t, 1, "a.example.com. 3600 IN A 1.2.3.4")
	// Second delta starts at serial 5 instead of 2.
	stream := []RR{
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 3 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 2 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 5 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 3 7200 3600 1209600 3600"),
		mustRR(t, "example.com. 3600 IN SOA ns1.example.com. admin.example.com. 3 7200 3600 1209600 3600"),
	}
	store := NewMemZoneStore(before)
	if err := ApplyIXFR(context.Background(), store, ixfrChannel(&Envelope{RR: stream})); !errors.Is(err, ErrIXFRChain) {
		t.Fatalf("expected ErrIXFRChain, got %v", err)
	}
	if !equalRRSet(store.All(), before) {
		t.Fatalf("store modified on failed apply: %v", rrStrings(store.All()))
	}
}

func TestApplyIXFRCancel(t *testing.T) {
	before := testZone(t, 1, "a.example.com. 3600 IN A 1.2.3.4")
	store := NewMemZoneStore(before)

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan *Envelope, 1)
	ch <- &Envelope{RR: testZone(t, 2)[:1]} // first envelope, then the stream stalls
	cancel()

	if err := ApplyIXFR(ctx, store, ch); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !equalRRSet(store.All(), before) {
		t.Fatalf("store modified on cancelled apply: %v", rrStrings(store.All()))
	}
}
