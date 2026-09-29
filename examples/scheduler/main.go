// This example demonstrates the scheduler package: it builds a renewal
// plan for a fleet of certificates using a simulated clock, saves and
// restores it (showing the plan is stable across restarts), then
// simulates a CA shrinking a renewal window (e.g. a mass revocation)
// and an issuance failure, showing how the plan adapts.
//
// Run with: go run ./examples/scheduler
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mholt/acmez/v3/scheduler"
)

// clock is a simulated clock so the whole demo is deterministic.
type clock struct{ t time.Time }

func (c *clock) Now() time.Time      { return c.t }
func (c *clock) Add(d time.Duration) { c.t = c.t.Add(d) }

func main() {
	sim := &clock{t: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}

	s := &scheduler.Scheduler{
		SafetyMargin:       24 * time.Hour, // renew at least a day before expiry
		Slot:               10 * time.Minute,
		MaxConcurrentPerCA: 2, // at most 2 renewals per CA per 10 minutes
		RetryBaseDelay:     time.Hour,
		RetryMaxDelay:      12 * time.Hour,
		Now:                sim.Now,
	}

	// A fleet of 45 certificates spread over 3 CAs. Expiries are
	// staggered over ~2 months; each CA suggests a 3-day window
	// roughly 2 weeks before expiry.
	cas := []string{"letsencrypt", "zerossl", "google"}
	var certs []scheduler.Cert
	for i := 0; i < 45; i++ {
		ca := cas[i%len(cas)]
		notAfter := sim.Now().Add(time.Duration(30+i) * 24 * time.Hour)
		certs = append(certs, scheduler.Cert{
			ID:       fmt.Sprintf("cert-%02d.example.com", i),
			CA:       ca,
			NotAfter: notAfter,
			Window: scheduler.Window{
				Start: notAfter.Add(-17 * 24 * time.Hour),
				End:   notAfter.Add(-14 * 24 * time.Hour),
			},
		})
	}
	for _, cert := range certs {
		if _, err := s.Upsert(cert); err != nil {
			fmt.Println("upsert:", err)
			os.Exit(1)
		}
	}

	fmt.Printf("=== Renewal plan for %d certificates (now = %s) ===\n",
		len(certs), sim.Now().Format(time.DateOnly))
	printPlan(s, -1)

	// Save the plan, "restart" into a fresh scheduler, and confirm
	// nothing moves: no re-randomization on process start.
	planFile := filepath.Join(os.TempDir(), "renewal-plan.json")
	if err := s.SaveFile(planFile); err != nil {
		fmt.Println("save:", err)
		os.Exit(1)
	}
	restarted := &scheduler.Scheduler{
		SafetyMargin:       s.SafetyMargin,
		Slot:               s.Slot,
		MaxConcurrentPerCA: s.MaxConcurrentPerCA,
		Now:                sim.Now,
	}
	if err := restarted.LoadFile(planFile); err != nil {
		fmt.Println("load:", err)
		os.Exit(1)
	}
	moved := 0
	for _, cert := range certs {
		before, _ := s.Entry(cert.ID)
		after, err := restarted.Upsert(cert)
		if err != nil {
			fmt.Println("re-upsert:", err)
			os.Exit(1)
		}
		if !after.Time.Equal(before.Time) {
			moved++
		}
	}
	fmt.Printf("\n=== Restart from %s: %d of %d entries moved ===\n", planFile, moved, len(certs))

	// The CA shrinks the window for 8 certs on letsencrypt (e.g. a
	// mass revocation): renew within the next 12 hours instead of
	// "in a few weeks".
	fmt.Println("\n=== Window shrink: 8 letsencrypt certs must renew within 12h ===")
	shrunk := scheduler.Window{Start: sim.Now(), End: sim.Now().Add(12 * time.Hour)}
	for i := 0; i < 45; i += 6 {
		certs[i].Window = shrunk
	}
	for _, cert := range certs {
		before, _ := restarted.Entry(cert.ID)
		after, err := restarted.Upsert(cert)
		if err != nil {
			fmt.Println("upsert:", err)
			os.Exit(1)
		}
		if !after.Time.Equal(before.Time) {
			fmt.Printf("  moved  %-24s %s -> %s\n", cert.ID,
				before.Time.Format("01-02 15:04"), after.Time.Format("01-02 15:04"))
		}
	}
	fmt.Println("\nFirst 12 entries after the shrink:")
	printPlan(restarted, 12)

	// One renewal fails at runtime; backoff grows but stays ahead of
	// the safety margin as the remaining lifetime shrinks.
	fmt.Println("\n=== Issuance failure backoff for cert-00.example.com ===")
	victim := certs[0]
	victim.NotAfter = sim.Now().Add(30 * time.Hour) // little time left
	victim.Window = scheduler.Window{Start: sim.Now(), End: victim.NotAfter.Add(-25 * time.Hour)}
	if _, err := restarted.Upsert(victim); err != nil {
		fmt.Println("upsert:", err)
		os.Exit(1)
	}
	for attempt := 1; attempt <= 4; attempt++ {
		entry, err := restarted.RescheduleAfterFailure(victim.ID)
		if err != nil {
			fmt.Println("reschedule:", err)
			os.Exit(1)
		}
		remaining := victim.NotAfter.Sub(entry.Time)
		fmt.Printf("  attempt %d failed; retry at %s (%.1f h before expiry)\n",
			attempt, entry.Time.Format("01-02 15:04"), remaining.Hours())
		sim.t = entry.Time // pretend we waited until the retry ran
	}
}

// printPlan prints the scheduled entries in time order, grouped with a
// per-CA slot-load summary; limit < 0 prints all entries.
func printPlan(s *scheduler.Scheduler, limit int) {
	entries := s.Entries()
	shown := 0
	for _, entry := range entries {
		if limit >= 0 && shown >= limit {
			fmt.Printf("  ... and %d more\n", len(entries)-shown)
			break
		}
		fmt.Printf("  %s  %-24s attempts=%d\n",
			entry.Time.Format("01-02 15:04"), entry.CertID, entry.Attempts)
		shown++
	}
}
