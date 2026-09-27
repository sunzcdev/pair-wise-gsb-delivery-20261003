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

package acmez

import (
	"context"
	"sync"

	"github.com/mholt/acmez/v3/acme"
)

// DNSProvider is the interface required of a DNS provider so that a
// DNS01Solver can provision and deprovision TXT records for dns-01
// challenges. Implementations MUST treat records as (name, value)
// pairs: adding a value that already exists for a name MUST NOT
// disturb other values under the same name, and removing a value
// MUST NOT remove any other value, even under the same name.
type DNSProvider interface {
	// AddTXTRecord ensures a TXT record with the given name and value
	// exists. It MUST be safe to call with a (name, value) pair that
	// is already present.
	AddTXTRecord(ctx context.Context, name, value string) error

	// RemoveTXTRecord removes the TXT record with the exact given
	// name and value. Other values under the same name MUST be left
	// untouched.
	RemoveTXTRecord(ctx context.Context, name, value string) error
}

// DNS01Solver solves dns-01 challenges by provisioning TXT records
// through a DNSProvider.
type DNS01Solver struct {
	// Provider is the DNS provider used to manage TXT records. Required.
	Provider DNSProvider

	// Coordinator is an optional shared coordination layer. When set,
	// records are reference-counted by ownership so that concurrent
	// orders (e.g. a wildcard certificate and a regular certificate for
	// the same domain, which both use the same _acme-challenge name)
	// do not delete TXT values that other in-flight orders still need.
	// Share one Coordinator among all solvers that use the same DNS
	// zone(s). When nil, Present and CleanUp call the provider directly.
	Coordinator *TXTRecordCoordinator
}

// Present provisions the TXT record for the challenge.
func (s DNS01Solver) Present(ctx context.Context, challenge acme.Challenge) error {
	name := challenge.DNS01TXTRecordName()
	value := challenge.DNS01KeyAuthorization()
	if s.Coordinator != nil {
		return s.Coordinator.present(ctx, s.Provider, dns01Owner(challenge), name, value)
	}
	return s.Provider.AddTXTRecord(ctx, name, value)
}

// CleanUp removes the TXT record for the challenge, unless a shared
// Coordinator is configured and other orders still own the same value.
func (s DNS01Solver) CleanUp(ctx context.Context, challenge acme.Challenge) error {
	name := challenge.DNS01TXTRecordName()
	value := challenge.DNS01KeyAuthorization()
	if s.Coordinator != nil {
		return s.Coordinator.cleanUp(ctx, s.Provider, dns01Owner(challenge), name, value)
	}
	return s.Provider.RemoveTXTRecord(ctx, name, value)
}

// dns01Owner returns a stable identity for the ACME authorization that
// owns a challenge, so the coordinator can tell "two orders sharing one
// TXT value" apart from "one order presented twice". The challenge URL
// uniquely identifies a challenge object on the server; the identifier
// and token are a fallback for challenges constructed offline.
func dns01Owner(challenge acme.Challenge) string {
	if challenge.URL != "" {
		return challenge.URL
	}
	return challenge.Identifier.Value + "|" + challenge.Token
}

// TXTRecordCoordinator coordinates TXT record lifecycles across
// concurrent ACME orders that share a DNS provider. It distinguishes
// two cases that naive solvers conflate:
//
//   - Same name, different values: each value is an independent record;
//     cleaning up one must not remove the others.
//   - Same name AND same value used by multiple orders: the provider is
//     told to add the record only once, and told to remove it only after
//     the last owning order has cleaned up.
//
// The zero value is ready to use, but a coordinator only helps if the
// same instance is shared by all solvers touching the same zone.
type TXTRecordCoordinator struct {
	mu      sync.Mutex
	records map[txtRecordKey]*txtRecordState
}

type txtRecordKey struct {
	name  string
	value string
}

// txtRecordState tracks which ACME authorizations currently rely on a
// given TXT record. The record exists at the provider iff it has at
// least one owner.
type txtRecordState struct {
	owners map[string]struct{}
}

// present registers owner's interest in the (name, value) record,
// adding it at the provider only if this is the first owner.
func (c *TXTRecordCoordinator) present(ctx context.Context, provider DNSProvider, owner, name, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.records == nil {
		c.records = make(map[txtRecordKey]*txtRecordState)
	}
	key := txtRecordKey{name: name, value: value}
	state, ok := c.records[key]
	if !ok {
		state = &txtRecordState{owners: make(map[string]struct{})}
		c.records[key] = state
	}
	if _, dup := state.owners[owner]; dup {
		return nil // this owner already presented; idempotent
	}
	first := len(state.owners) == 0
	state.owners[owner] = struct{}{}
	if first {
		if err := provider.AddTXTRecord(ctx, name, value); err != nil {
			// roll back so a retry can attempt the add again
			delete(state.owners, owner)
			if len(state.owners) == 0 {
				delete(c.records, key)
			}
			return err
		}
	}
	return nil
}

// cleanUp releases owner's interest in the (name, value) record,
// removing it at the provider only when the last owner releases it.
// It is a no-op if the owner never presented the record.
func (c *TXTRecordCoordinator) cleanUp(ctx context.Context, provider DNSProvider, owner, name, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := txtRecordKey{name: name, value: value}
	state, ok := c.records[key]
	if !ok {
		return nil
	}
	if _, owned := state.owners[owner]; !owned {
		return nil
	}
	delete(state.owners, owner)
	if len(state.owners) > 0 {
		return nil // other orders still need this exact value
	}
	delete(c.records, key)
	return provider.RemoveTXTRecord(ctx, name, value)
}

// Interface guard.
var _ Solver = DNS01Solver{}
