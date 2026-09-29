// Package renewal schedules renewal times for a fleet of certificates.
//
// It complements the low-level ARI support in the parent acme package: ARI
// tells the client each certificate's individually suggested renewal window,
// and this package turns a set of those windows into a stable plan that also
// respects per-CA concurrency limits and a safety margin before expiry.
//
// The plan is persisted through a Store, so restarting the process does not
// re-randomize scheduled times or stampede a CA. When ARI windows move,
// [Planner.Reconcile] only reschedules entries whose selected time no longer
// fits; failed issuances are retried with backoff that is clamped to the
// remaining time before the safety-margin deadline.
package renewal

import "time"

// Window is a half-open interval [Start, End) in which a renewal may run.
type Window struct {
	Start time.Time
	End   time.Time
}

// Contains reports whether t falls inside the window.
func (w Window) Contains(t time.Time) bool {
	return !t.Before(w.Start) && t.Before(w.End)
}

// Cert describes one deployed certificate whose renewal is managed.
type Cert struct {
	// ID is the stable identifier of the certificate, typically its ARI
	// unique identifier. It is used to correlate plans across restarts.
	ID string

	// CA identifies the issuing ACME server (for example its directory URL).
	// Concurrency limits are applied per CA.
	CA string

	// Names is informational and used in logs.
	Names []string

	// NotAfter is the current certificate's expiry. Renewals are scheduled
	// to complete SafetyMargin before this time.
	NotAfter time.Time

	// Window is the ARI suggested renewal window. If its zero value, the
	// planner falls back to an expiry-derived window.
	Window Window
}

// HasWindow reports whether the certificate carried an ARI window.
func (c Cert) HasWindow() bool {
	return !c.Window.Start.IsZero() && !c.Window.End.IsZero()
}

// Status is the lifecycle state of a planned renewal.
type Status string

const (
	// StatusScheduled means a renewal time has been chosen but not started.
	StatusScheduled Status = "scheduled"
	// StatusIssuing means the renewal is currently in flight.
	StatusIssuing Status = "issuing"
	// StatusRenewed means the certificate was successfully renewed.
	StatusRenewed Status = "renewed"
)

// Entry is one scheduled renewal in a persisted [Plan].
type Entry struct {
	CertID string   `json:"cert_id"`
	CA     string   `json:"ca"`
	Names  []string `json:"names,omitempty"`

	// NotAfter is the expiry of the certificate being renewed.
	NotAfter time.Time `json:"not_after"`

	// WindowStart and WindowEnd bound the effective window the entry was
	// placed in. WindowEnd already incorporates the safety margin.
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`

	// ScheduledAt is the chosen renewal time. It is persisted verbatim: a
	// restart reloads it instead of drawing a fresh random time.
	ScheduledAt time.Time `json:"scheduled_at"`
	RenewedAt   time.Time `json:"renewed_at,omitempty"`

	Status    Status `json:"status"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`

	// Sig is a signature of the inputs ScheduledAt was chosen from
	// (CA, expiry, raw ARI window), used to detect relevant changes.
	Sig string `json:"sig"`
}

// Clock yields the current time. Production code uses [RealClock]; the
// simulator and tests use a controllable fake clock.
type Clock interface {
	Now() time.Time
}

// RealClock reports the wall-clock time.
type RealClock struct{}

// Now implements [Clock].
func (RealClock) Now() time.Time { return time.Now().UTC() }
