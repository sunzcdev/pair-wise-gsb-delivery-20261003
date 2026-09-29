// Package scheduler maintains renewal plans for a set of certificates
// based on their ACME Renewal Information (ARI) suggested windows.
//
// It is an independent component: it does not talk to any ACME server.
// Callers feed it certificates with their current ARI windows (see
// acme.Client.GetRenewalInfo), and it decides when each certificate
// should be renewed, subject to three constraints:
//
//   - each certificate is scheduled at a random time inside its own
//     suggested window (RFC 9773 §4.2);
//   - renewals for the same CA are spread out so that no more than
//     MaxConcurrentPerCA of them fall into the same time slot;
//   - no renewal is scheduled later than SafetyMargin before the
//     certificate expires.
//
// Plans are stable: selected times are derived deterministically from
// the certificate ID and its window, and the whole plan can be saved
// and restored, so restarting the process does not reshuffle every
// certificate into a pile. When a window changes, only entries that no
// longer fit are moved; everything else keeps its selected time.
package scheduler

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"os"
	"sort"
	"time"
)

// Default values used when the corresponding Scheduler field is zero.
const (
	DefaultSafetyMargin       = 24 * time.Hour
	DefaultSlot               = time.Minute
	DefaultMaxConcurrentPerCA = 1
	DefaultRetryBaseDelay     = time.Hour
	DefaultRetryMaxDelay      = 24 * time.Hour
)

// Window is a suggested renewal window from ARI.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// valid reports whether the window is usable.
func (w Window) valid() bool {
	return !w.Start.IsZero() && !w.End.IsZero() && w.End.After(w.Start)
}

// Cert describes one managed certificate.
type Cert struct {
	// ID uniquely identifies the certificate (e.g. its ARI unique
	// identifier, or any stable name chosen by the caller).
	ID string `json:"id"`

	// CA groups certificates by issuing CA; the concurrency limit is
	// applied per CA. Any stable label works (e.g. the directory URL).
	CA string `json:"ca"`

	// NotAfter is the certificate's expiration time.
	NotAfter time.Time `json:"not_after"`

	// Window is the certificate's current ARI suggested window. A zero
	// window means no ARI is available; the scheduler then falls back
	// to spreading renewals between now and the safety margin.
	Window Window `json:"window,omitempty"`
}

// Entry is one scheduled renewal.
type Entry struct {
	CertID   string    `json:"cert_id"`
	Time     time.Time `json:"time"`
	Attempts int       `json:"attempts,omitempty"`
}

// Scheduler computes and remembers renewal times for a set of
// certificates. It is not safe for concurrent use.
type Scheduler struct {
	// SafetyMargin is how long before expiration a renewal must happen
	// at the latest. Default: DefaultSafetyMargin.
	SafetyMargin time.Duration

	// Slot is the granularity used for the per-CA concurrency limit:
	// at most MaxConcurrentPerCA renewals per CA may land in the same
	// slot. Default: DefaultSlot.
	Slot time.Duration

	// MaxConcurrentPerCA limits how many renewals of one CA may share
	// a slot. Default: DefaultMaxConcurrentPerCA.
	MaxConcurrentPerCA int

	// RetryBaseDelay and RetryMaxDelay control the exponential backoff
	// used when rescheduling after a failed issuance. Defaults:
	// DefaultRetryBaseDelay and DefaultRetryMaxDelay.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration

	// Now returns the current time; if nil, time.Now is used. Tests
	// and simulations can inject a fake clock.
	Now func() time.Time

	certs   map[string]Cert
	entries map[string]*Entry
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) safetyMargin() time.Duration {
	if s.SafetyMargin > 0 {
		return s.SafetyMargin
	}
	return DefaultSafetyMargin
}

func (s *Scheduler) slot() time.Duration {
	if s.Slot > 0 {
		return s.Slot
	}
	return DefaultSlot
}

func (s *Scheduler) maxConcurrentPerCA() int {
	if s.MaxConcurrentPerCA > 0 {
		return s.MaxConcurrentPerCA
	}
	return DefaultMaxConcurrentPerCA
}

func (s *Scheduler) retryBaseDelay() time.Duration {
	if s.RetryBaseDelay > 0 {
		return s.RetryBaseDelay
	}
	return DefaultRetryBaseDelay
}

func (s *Scheduler) retryMaxDelay() time.Duration {
	if s.RetryMaxDelay > 0 {
		return s.RetryMaxDelay
	}
	return DefaultRetryMaxDelay
}

// Upsert adds or updates a certificate and returns its scheduled time.
// An existing entry is kept as-is if it still fits the (possibly new)
// effective window and the per-CA concurrency limit; only entries that
// no longer fit are moved.
func (s *Scheduler) Upsert(cert Cert) (Entry, error) {
	if cert.ID == "" {
		return Entry{}, fmt.Errorf("certificate ID is required")
	}
	if cert.NotAfter.IsZero() {
		return Entry{}, fmt.Errorf("certificate %s: NotAfter is required", cert.ID)
	}
	s.init()
	s.certs[cert.ID] = cert

	start, end := s.effectiveWindow(cert)
	if entry, ok := s.entries[cert.ID]; ok && s.fits(cert, entry, start, end) {
		return *entry, nil
	}

	t := s.place(cert, start, end)
	entry := &Entry{CertID: cert.ID, Time: t}
	if old, ok := s.entries[cert.ID]; ok {
		entry.Attempts = old.Attempts
	}
	s.entries[cert.ID] = entry
	return *entry, nil
}

// Remove deletes a certificate and its scheduled entry.
func (s *Scheduler) Remove(id string) {
	delete(s.certs, id)
	delete(s.entries, id)
}

// Entry returns the scheduled entry for a certificate.
func (s *Scheduler) Entry(id string) (Entry, bool) {
	entry, ok := s.entries[id]
	if !ok {
		return Entry{}, false
	}
	return *entry, true
}

// Entries returns all scheduled entries sorted by time.
func (s *Scheduler) Entries() []Entry {
	entries := make([]Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Time.Equal(entries[j].Time) {
			return entries[i].CertID < entries[j].CertID
		}
		return entries[i].Time.Before(entries[j].Time)
	})
	return entries
}

// Due returns the entries scheduled at or before now, sorted by time.
func (s *Scheduler) Due() []Entry {
	now := s.now()
	var due []Entry
	for _, entry := range s.Entries() {
		if !entry.Time.After(now) {
			due = append(due, entry)
		}
	}
	return due
}

// RescheduleAfterFailure moves a certificate's renewal to a backoff
// time in the future, doubling the delay with each consecutive failure
// (up to RetryMaxDelay). The retry is never scheduled past the safety
// margin before expiration: as the remaining lifetime runs out, the
// backoff collapses toward "as soon as possible" so the certificate
// still gets renewed before it expires.
func (s *Scheduler) RescheduleAfterFailure(id string) (Entry, error) {
	cert, ok := s.certs[id]
	if !ok {
		return Entry{}, fmt.Errorf("unknown certificate %q", id)
	}
	entry, ok := s.entries[id]
	if !ok {
		return Entry{}, fmt.Errorf("no entry for certificate %q", id)
	}

	entry.Attempts++
	delay := s.retryBaseDelay()
	for i := 1; i < entry.Attempts && delay < s.retryMaxDelay(); i++ {
		delay *= 2
	}
	if delay > s.retryMaxDelay() {
		delay = s.retryMaxDelay()
	}

	now := s.now()
	t := now.Add(delay)
	if deadline := cert.NotAfter.Add(-s.safetyMargin()); t.After(deadline) {
		t = deadline
	}
	if t.Before(now) {
		t = now
	}
	entry.Time = t
	return *entry, nil
}

// effectiveWindow intersects the suggested window with [now, NotAfter
// - SafetyMargin]. Without a valid window it spreads renewals over the
// whole remaining safe lifetime. The result always has end >= start;
// a collapsed result means the margin leaves no room and the renewal
// is effectively due immediately.
func (s *Scheduler) effectiveWindow(cert Cert) (start, end time.Time) {
	start, end = cert.Window.Start, cert.Window.End
	if !cert.Window.valid() {
		start = s.now()
		end = cert.NotAfter
	}
	if deadline := cert.NotAfter.Add(-s.safetyMargin()); end.After(deadline) {
		end = deadline
	}
	if now := s.now(); start.Before(now) {
		start = now
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

// fits reports whether an existing entry may be kept for the given
// effective window: it must lie inside the window and its slot must
// not exceed the per-CA concurrency limit (not counting itself).
func (s *Scheduler) fits(cert Cert, entry *Entry, start, end time.Time) bool {
	if entry.Time.Before(start) || entry.Time.After(end) {
		return false
	}
	return s.slotLoad(cert.CA, entry.Time, cert.ID) < s.maxConcurrentPerCA()
}

// slotLoad counts how many entries of the same CA share the slot
// containing t, optionally ignoring one certificate.
func (s *Scheduler) slotLoad(ca string, t time.Time, ignoreID string) int {
	slot := t.Unix() / int64(s.slot().Seconds())
	load := 0
	for id, entry := range s.entries {
		if id == ignoreID {
			continue
		}
		if cert, ok := s.certs[id]; ok && cert.CA == ca &&
			entry.Time.Unix()/int64(s.slot().Seconds()) == slot {
			load++
		}
	}
	return load
}

// place selects a time within [start, end]. Slots that are already at
// the per-CA concurrency limit are avoided when possible. The choice
// is deterministic for a given certificate ID and window, so a restart
// without a saved plan still reproduces the same schedule instead of
// re-randomizing everything into a cluster.
func (s *Scheduler) place(cert Cert, start, end time.Time) time.Time {
	if !end.After(start) {
		return start
	}

	slotNanos := int64(s.slot())
	nSlots := int(end.Sub(start).Nanoseconds()/slotNanos) + 1

	available := make([]int, 0, nSlots)
	for i := 0; i < nSlots; i++ {
		t := start.Add(time.Duration(i) * s.slot())
		if s.slotLoad(cert.CA, t, cert.ID) < s.maxConcurrentPerCA() {
			available = append(available, i)
		}
	}
	if len(available) == 0 {
		// The window is too crowded to honor the limit; better to
		// overflow a slot than to renew late.
		available = make([]int, nSlots)
		for i := range available {
			available[i] = i
		}
	}

	rng := rand.New(rand.NewSource(seed(cert.ID, start, end)))
	i := available[rng.Intn(len(available))]
	t := start.Add(time.Duration(i) * s.slot())
	if max := end.Sub(t); max > 0 {
		offset := time.Duration(rng.Int63n(int64(min(s.slot(), max))))
		t = t.Add(offset)
	}
	return t
}

// seed derives a deterministic random seed from the certificate ID and
// its effective window.
func seed(id string, start, end time.Time) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%d|%d", id, start.Unix(), end.Unix())
	return int64(h.Sum64())
}

func (s *Scheduler) init() {
	if s.certs == nil {
		s.certs = make(map[string]Cert)
	}
	if s.entries == nil {
		s.entries = make(map[string]*Entry)
	}
}

// plan is the serializable form of a Scheduler.
type plan struct {
	Certs   []Cert  `json:"certs"`
	Entries []Entry `json:"entries"`
}

// Save writes the plan (certificates and scheduled times) as JSON.
func (s *Scheduler) Save(w io.Writer) error {
	p := plan{Entries: s.Entries()}
	for _, cert := range s.certs {
		p.Certs = append(p.Certs, cert)
	}
	sort.Slice(p.Certs, func(i, j int) bool { return p.Certs[i].ID < p.Certs[j].ID })
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

// Load restores a plan previously written with Save. Restored entries
// are kept verbatim; the next Upsert only moves the ones whose windows
// or limits no longer fit.
func (s *Scheduler) Load(r io.Reader) error {
	var p plan
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return err
	}
	s.init()
	for _, cert := range p.Certs {
		s.certs[cert.ID] = cert
	}
	for _, entry := range p.Entries {
		entry := entry
		s.entries[entry.CertID] = &entry
	}
	return nil
}

// SaveFile writes the plan to a JSON file.
func (s *Scheduler) SaveFile(name string) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.Save(f)
}

// LoadFile restores a plan from a JSON file written by SaveFile.
func (s *Scheduler) LoadFile(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.Load(f)
}
