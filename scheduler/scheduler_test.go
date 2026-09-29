package scheduler

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time      { return c.t }
func (c *fakeClock) Add(d time.Duration) { c.t = c.t.Add(d) }

var testEpoch = time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)

func newTestScheduler(clock *fakeClock) *Scheduler {
	return &Scheduler{
		SafetyMargin:       time.Hour,
		Slot:               time.Minute,
		MaxConcurrentPerCA: 2,
		RetryBaseDelay:     30 * time.Minute,
		RetryMaxDelay:      4 * time.Hour,
		Now:                clock.Now,
	}
}

func makeCert(id, ca string, notAfter time.Time, window Window) Cert {
	return Cert{ID: id, CA: ca, NotAfter: notAfter, Window: window}
}

// Every entry must land inside its effective window: within the
// suggested window, not in the past, and at least SafetyMargin before
// expiration.
func TestPlacementRespectsWindowAndSafetyMargin(t *testing.T) {
	clock := &fakeClock{t: testEpoch}
	s := newTestScheduler(clock)

	for i := 0; i < 50; i++ {
		notAfter := testEpoch.Add(time.Duration(24+i) * time.Hour)
		window := Window{
			Start: testEpoch.Add(time.Duration(i%12) * time.Hour),
			End:   notAfter.Add(-30 * time.Minute), // window end beyond margin on purpose
		}
		cert := makeCert(fmt.Sprintf("cert-%02d", i), "ca-a", notAfter, window)
		entry, err := s.Upsert(cert)
		if err != nil {
			t.Fatal(err)
		}
		deadline := notAfter.Add(-s.SafetyMargin)
		if entry.Time.Before(clock.Now()) {
			t.Errorf("%s scheduled in the past: %s", cert.ID, entry.Time)
		}
		if entry.Time.Before(window.Start) {
			t.Errorf("%s scheduled before window start: %s < %s", cert.ID, entry.Time, window.Start)
		}
		if entry.Time.After(deadline) {
			t.Errorf("%s scheduled past safety margin: %s > %s", cert.ID, entry.Time, deadline)
		}
	}
}

// Renewals of one CA must not pile up beyond the per-slot limit.
func TestConcurrencyLimitPerCA(t *testing.T) {
	clock := &fakeClock{t: testEpoch}
	s := newTestScheduler(clock)

	// All certs share the same 2-hour window, so 20 certs of ca-a
	// cannot fit without spreading across slots.
	window := Window{Start: testEpoch, End: testEpoch.Add(2 * time.Hour)}
	for i := 0; i < 20; i++ {
		cert := makeCert(fmt.Sprintf("a-%02d", i), "ca-a", testEpoch.Add(48*time.Hour), window)
		if _, err := s.Upsert(cert); err != nil {
			t.Fatal(err)
		}
	}
	// Same window, different CA: independent limit.
	for i := 0; i < 20; i++ {
		cert := makeCert(fmt.Sprintf("b-%02d", i), "ca-b", testEpoch.Add(48*time.Hour), window)
		if _, err := s.Upsert(cert); err != nil {
			t.Fatal(err)
		}
	}

	loads := map[string]map[int64]int{}
	for _, entry := range s.Entries() {
		cert := s.certs[entry.CertID]
		slot := entry.Time.Unix() / int64(s.Slot.Seconds())
		if loads[cert.CA] == nil {
			loads[cert.CA] = map[int64]int{}
		}
		loads[cert.CA][slot]++
	}
	for ca, slots := range loads {
		for slot, n := range slots {
			if n > s.MaxConcurrentPerCA {
				t.Errorf("CA %s slot %d has %d renewals, limit is %d", ca, slot, n, s.MaxConcurrentPerCA)
			}
		}
	}
}

// A saved plan must survive a restart unchanged: no re-randomization
// that would bunch certificates together.
func TestSaveLoadRoundTrip(t *testing.T) {
	clock := &fakeClock{t: testEpoch}
	s := newTestScheduler(clock)
	for i := 0; i < 30; i++ {
		window := Window{Start: testEpoch, End: testEpoch.Add(6 * time.Hour)}
		cert := makeCert(fmt.Sprintf("cert-%02d", i), "ca-a", testEpoch.Add(72*time.Hour), window)
		if _, err := s.Upsert(cert); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	if err := s.Save(&buf); err != nil {
		t.Fatal(err)
	}

	restored := newTestScheduler(clock)
	if err := restored.Load(&buf); err != nil {
		t.Fatal(err)
	}

	before, after := s.Entries(), restored.Entries()
	if len(before) != len(after) {
		t.Fatalf("entry count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("entry %d changed across save/load: %+v -> %+v", i, before[i], after[i])
		}
	}

	// Re-upserting the same certs after a restart must keep every time.
	for id, cert := range restored.certs {
		entry, err := restored.Upsert(cert)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := restored.Entry(id)
		if !entry.Time.Equal(want.Time) {
			t.Errorf("%s moved after restart: %s -> %s", id, want.Time, entry.Time)
		}
	}
}

// Even without a saved plan, placement is deterministic per
// certificate and window, so a crash before the first save does not
// reshuffle the fleet.
func TestDeterministicPlacement(t *testing.T) {
	window := Window{Start: testEpoch, End: testEpoch.Add(24 * time.Hour)}

	var first []Entry
	for run := 0; run < 2; run++ {
		clock := &fakeClock{t: testEpoch}
		s := newTestScheduler(clock)
		var entries []Entry
		for i := 0; i < 20; i++ {
			cert := makeCert(fmt.Sprintf("cert-%02d", i), "ca-a", testEpoch.Add(72*time.Hour), window)
			entry, err := s.Upsert(cert)
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, entry)
		}
		if run == 0 {
			first = entries
			continue
		}
		for i := range entries {
			if !entries[i].Time.Equal(first[i].Time) {
				t.Errorf("cert %s moved between identical runs: %s -> %s",
					entries[i].CertID, first[i].Time, entries[i].Time)
			}
		}
	}
}

// When a window shrinks, only entries that no longer fit may move.
func TestWindowShrinkMovesOnlyMisfits(t *testing.T) {
	clock := &fakeClock{t: testEpoch}
	s := newTestScheduler(clock)

	window := Window{Start: testEpoch, End: testEpoch.Add(24 * time.Hour)}
	original := map[string]time.Time{}
	for i := 0; i < 30; i++ {
		cert := makeCert(fmt.Sprintf("cert-%02d", i), "ca-a", testEpoch.Add(72*time.Hour), window)
		entry, err := s.Upsert(cert)
		if err != nil {
			t.Fatal(err)
		}
		original[cert.ID] = entry.Time
	}

	// The CA tightens the window to the first 6 hours.
	shrunk := Window{Start: testEpoch, End: testEpoch.Add(6 * time.Hour)}
	moved, kept := 0, 0
	for id, t0 := range original {
		cert := s.certs[id]
		cert.Window = shrunk
		entry, err := s.Upsert(cert)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Time.Equal(t0) {
			kept++
			if entry.Time.After(shrunk.End) {
				t.Errorf("%s kept a time outside the shrunk window: %s", id, entry.Time)
			}
		} else {
			moved++
			if !t0.After(shrunk.End) {
				t.Errorf("%s moved even though %s still fits the shrunk window", id, t0)
			}
			if entry.Time.Before(shrunk.Start) || entry.Time.After(shrunk.End) {
				t.Errorf("%s moved outside the shrunk window: %s", id, entry.Time)
			}
		}
	}
	if moved == 0 {
		t.Error("expected some entries to move after the window shrank")
	}
	if kept == 0 {
		t.Error("expected entries inside the shrunk window to keep their times")
	}
	t.Logf("window shrink: %d moved, %d kept", moved, kept)
}

// Backoff after failed issuance must grow, but collapse toward the
// safety-margin deadline as the remaining lifetime runs out, so the
// certificate is still renewed before it expires.
func TestRescheduleAfterFailureRespectsRemainingTime(t *testing.T) {
	clock := &fakeClock{t: testEpoch}
	s := newTestScheduler(clock)

	notAfter := testEpoch.Add(3 * time.Hour)
	cert := makeCert("cert-01", "ca-a", notAfter,
		Window{Start: testEpoch, End: notAfter.Add(-time.Hour)})
	if _, err := s.Upsert(cert); err != nil {
		t.Fatal(err)
	}

	// First failure at t0: retry in RetryBaseDelay (30m).
	entry, err := s.RescheduleAfterFailure("cert-01")
	if err != nil {
		t.Fatal(err)
	}
	if want := testEpoch.Add(30 * time.Minute); !entry.Time.Equal(want) {
		t.Errorf("first retry: got %s, want %s", entry.Time, want)
	}

	// Second failure: backoff doubles to 1h.
	clock.t = entry.Time
	entry, err = s.RescheduleAfterFailure("cert-01")
	if err != nil {
		t.Fatal(err)
	}
	if want := testEpoch.Add(90 * time.Minute); !entry.Time.Equal(want) {
		t.Errorf("second retry: got %s, want %s", entry.Time, want)
	}

	// The deadline (NotAfter - margin = t0+2h) is closer than the next
	// backoff (2h), so the retry clamps to the deadline.
	clock.t = entry.Time
	entry, err = s.RescheduleAfterFailure("cert-01")
	if err != nil {
		t.Fatal(err)
	}
	deadline := notAfter.Add(-s.SafetyMargin)
	if !entry.Time.Equal(deadline) {
		t.Errorf("retry past deadline: got %s, want clamped to %s", entry.Time, deadline)
	}

	// Once the deadline has passed, retry immediately rather than never.
	clock.t = deadline.Add(time.Minute)
	entry, err = s.RescheduleAfterFailure("cert-01")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Time.Equal(clock.Now()) {
		t.Errorf("overdue retry: got %s, want now (%s)", entry.Time, clock.Now())
	}
}

// Certificates without ARI still get spread over their safe lifetime.
func TestNoWindowFallback(t *testing.T) {
	clock := &fakeClock{t: testEpoch}
	s := newTestScheduler(clock)

	cert := makeCert("cert-01", "ca-a", testEpoch.Add(30*24*time.Hour), Window{})
	entry, err := s.Upsert(cert)
	if err != nil {
		t.Fatal(err)
	}
	deadline := cert.NotAfter.Add(-s.SafetyMargin)
	if entry.Time.Before(clock.Now()) || entry.Time.After(deadline) {
		t.Errorf("fallback placement %s outside [%s, %s]", entry.Time, clock.Now(), deadline)
	}
}
