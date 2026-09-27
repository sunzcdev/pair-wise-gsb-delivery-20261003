package acmez

import (
	"context"
	"sync"
	"time"

	"github.com/mholt/acmez/v3/acme"
)

// DNSProvider is the interface a DNS provider must satisfy so that the
// dns-01 challenge can be solved. Implementations must treat TXT records
// as a multi-value resource: appending a value must not replace or remove
// other values already present under the same name, and removing a value
// must leave any other values under that name untouched.
type DNSProvider interface {
	// AppendTXTRecord adds a single TXT record with the given name and
	// value, without disturbing any other records under that name.
	AppendTXTRecord(ctx context.Context, name, value string, ttl time.Duration) error

	// RemoveTXTRecord removes a single TXT record matching both the
	// given name and value, leaving any other values under that name
	// in place.
	RemoveTXTRecord(ctx context.Context, name, value string) error
}

// DNS01Solver is a Solver that solves the dns-01 challenge by
// provisioning TXT records through a DNSProvider.
//
// If multiple certificates are being obtained concurrently (for example,
// a wildcard certificate and a regular certificate for the same domain),
// their challenges can share the same _acme-challenge name, and even the
// same TXT value. Set Coordinator to a DNS01Coordinator shared by all the
// solvers that may run concurrently so that records are published and
// removed according to actual ownership: values still needed by another
// in-flight order are never removed, and identical values are only
// published once.
type DNS01Solver struct {
	// The DNS provider used to publish and remove TXT records. Required.
	Provider DNSProvider

	// An optional shared coordination layer. If nil, every Present and
	// CleanUp call is passed straight through to the provider, which is
	// only safe if no two challenges can share a TXT record name.
	Coordinator *DNS01Coordinator

	// The TTL to use when creating TXT records. Optional.
	TTL time.Duration
}

// Present publishes the TXT record for the challenge.
func (s *DNS01Solver) Present(ctx context.Context, challenge acme.Challenge) error {
	name, value := challenge.DNS01TXTRecordName(), challenge.DNS01KeyAuthorization()
	if s.Coordinator != nil {
		return s.Coordinator.Present(ctx, s.Provider, challenge.URL, name, value, s.TTL)
	}
	return s.Provider.AppendTXTRecord(ctx, name, value, s.TTL)
}

// CleanUp removes the TXT record for the challenge, unless it is still
// owned by another in-flight challenge (only when a Coordinator is used).
func (s *DNS01Solver) CleanUp(ctx context.Context, challenge acme.Challenge) error {
	name, value := challenge.DNS01TXTRecordName(), challenge.DNS01KeyAuthorization()
	if s.Coordinator != nil {
		return s.Coordinator.CleanUp(ctx, s.Provider, challenge.URL, name, value)
	}
	return s.Provider.RemoveTXTRecord(ctx, name, value)
}

// DNS01Coordinator coordinates concurrent dns-01 challenges that publish
// TXT records to the same names, so that concurrent orders (for example,
// a wildcard and a regular certificate for the same domain) don't
// interfere with each other's records.
//
// It tracks ownership per (name, value) pair, which keeps two cases
// strictly apart:
//
//   - Same name, different values: each value is an independent record;
//     all of them coexist under the name and each is removed only when
//     its own last owner cleans up.
//   - Same name, same value (e.g. two orders that received the same
//     token): the value is appended to the provider only once, reference
//     counted by owner, and removed only after every owner has cleaned
//     up.
//
// Owners are identified by an opaque token that must uniquely identify
// the in-flight challenge; DNS01Solver uses the challenge URL, which the
// ACME server makes unique per authorization.
//
// The zero value is ready to use. A single DNS01Coordinator should be
// shared by all solvers that may publish to the same zone concurrently.
type DNS01Coordinator struct {
	mu      sync.Mutex
	records map[dns01TXTRecord]*dns01TXTRecordState
}

type dns01TXTRecord struct {
	name, value string
}

type dns01TXTRecordState struct {
	owners map[string]struct{}
}

// Present registers owner's interest in the TXT record (name, value) and
// ensures the record exists at the provider. The provider is only asked
// to append the record if no other owner has already published the same
// value under the same name; otherwise the existing record is simply
// shared. If the provider call fails, the ownership registration is
// rolled back.
func (c *DNS01Coordinator) Present(ctx context.Context, provider DNSProvider, owner, name, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := dns01TXTRecord{name: name, value: value}
	state, ok := c.records[key]
	if ok {
		// the record is already published (or being published) for
		// another owner; just register our interest in it
		state.owners[owner] = struct{}{}
		return nil
	}

	if c.records == nil {
		c.records = make(map[dns01TXTRecord]*dns01TXTRecordState)
	}
	state = &dns01TXTRecordState{owners: map[string]struct{}{owner: {}}}
	c.records[key] = state

	// the mutex is held during the provider call so that the record is
	// guaranteed to exist at the provider before any other owner can
	// register its interest and report the challenge as presented
	if err := provider.AppendTXTRecord(ctx, name, value, ttl); err != nil {
		delete(c.records, key)
		return err
	}
	return nil
}

// CleanUp releases owner's interest in the TXT record (name, value). The
// record is only removed from the provider once its last remaining owner
// has been released; values still needed by other in-flight challenges
// are left untouched. It is safe to call CleanUp for a record that was
// never presented; it is a no-op in that case.
func (c *DNS01Coordinator) CleanUp(ctx context.Context, provider DNSProvider, owner, name, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := dns01TXTRecord{name: name, value: value}
	state, ok := c.records[key]
	if !ok {
		return nil
	}
	delete(state.owners, owner)
	if len(state.owners) > 0 {
		// other in-flight challenges still need this record
		return nil
	}

	delete(c.records, key)
	return provider.RemoveTXTRecord(ctx, name, value)
}
