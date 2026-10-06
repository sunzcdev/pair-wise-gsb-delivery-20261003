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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/mholt/acmez/v3/acme"
)

// fakeCAAQuerier is a scripted CAAQuerier; it never touches real DNS.
type fakeCAAQuerier struct {
	responses map[string]struct {
		records []*CAARecord
		status  CAALookupStatus
		err     error
	}
	queried []string
}

func (f *fakeCAAQuerier) LookupCAA(_ context.Context, name string) ([]*CAARecord, CAALookupStatus, error) {
	f.queried = append(f.queried, name)
	if res, ok := f.responses[name]; ok {
		return res.records, res.status, res.err
	}
	// By default, act as an authoritative empty answer to allow the walk
	// to continue toward the parent zone.
	return nil, CAALookupAuthoritativeEmpty, nil
}

func ptr(rec CAARecord) *CAARecord { return &rec }

func newCAAClient() *Client {
	return &Client{
		Client: &acme.Client{},
		ChallengeSolvers: map[string]Solver{
			acme.ChallengeTypeDNS01: nopSolver{},
		},
	}
}

type nopSolver struct{}

func (nopSolver) Present(context.Context, acme.Challenge) error { return nil }
func (nopSolver) CleanUp(context.Context, acme.Challenge) error { return nil }

func defaultPre() CAAPrecheck {
	return CAAPrecheck{
		Enabled:           true,
		CAIdentities:      []string{"letsencrypt.org"},
		AccountURL:        "https://acme.example/acct/1",
		ValidationMethods: []string{acme.ChallengeTypeDNS01},
	}
}

func TestCheckCAABlock(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"example.com": {
				records: []*CAARecord{ptr(issueRec("other-ca.example"))},
				status:  CAALookupFound,
			},
		},
	}
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	reports, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "example.com")})

	if !errors.Is(err, ErrCAABlocked) {
		t.Fatalf("expected ErrCAABlocked, got %v", err)
	}
	if len(reports) != 1 || reports[0].Verdict != CAABlocked {
		t.Fatalf("unexpected reports: %+v", reports)
	}
}

func TestCheckCAAAllowNoRecords(t *testing.T) {
	q := &fakeCAAQuerier{} // every answer authoritative empty
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	reports, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "www.example.com")})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reports[0].Verdict != CAAAllowed {
		t.Fatalf("verdict = %s, want allowed", reports[0].Verdict)
	}
	// Walk: www.example.com -> example.com -> com (then stop).
	want := []string{"www.example.com", "example.com", "com"}
	if len(q.queried) != len(want) {
		t.Fatalf("queried %v, want %v", q.queried, want)
	}
	for i := range want {
		if q.queried[i] != want[i] {
			t.Fatalf("queried %v, want %v", q.queried, want)
		}
	}
}

func TestCheckCAATreeWalkFindsRecordsAtParent(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"example.com": {
				records: []*CAARecord{ptr(issueRec("letsencrypt.org"))},
				status:  CAALookupFound,
			},
		},
	}
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	reports, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "deep.sub.example.com")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reports[0].Verdict != CAAAllowed {
		t.Fatalf("verdict = %s, want allowed", reports[0].Verdict)
	}
	want := []string{"deep.sub.example.com", "sub.example.com", "example.com"}
	if len(q.queried) != len(want) {
		t.Fatalf("queried %v, want %v", q.queried, want)
	}
}

func TestCheckCAANXDOMAINFallsBackToParent(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"www.example.com": {status: CAALookupNameError},
			"example.com": {
				records: []*CAARecord{ptr(issueRec("letsencrypt.org"))},
				status:  CAALookupFound,
			},
		},
	}
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	reports, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "www.example.com")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reports[0].Verdict != CAAAllowed {
		t.Fatalf("verdict = %s, want allowed", reports[0].Verdict)
	}
}

func TestCheckCAAInconclusiveBlocksByDefault(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"example.com": {status: CAALookupIndeterminate},
		},
	}
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	_, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "example.com")})
	if !errors.Is(err, ErrCAAInconclusive) {
		t.Fatalf("expected ErrCAAInconclusive (default fail closed), got %v", err)
	}
}

func TestCheckCAAInconclusiveCanContinue(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"example.com": {status: CAALookupIndeterminate},
		},
	}
	pre := defaultPre()
	pre.Querier = q
	pre.OnInconclusive = CAAContinueOnInconclusive

	client := newCAAClient()
	reports, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "example.com")})
	if err != nil {
		t.Fatalf("expected no error when continuing on inconclusive, got %v", err)
	}
	if reports[0].Verdict != CAAInconclusive {
		t.Fatalf("verdict = %s, want inconclusive", reports[0].Verdict)
	}
}

func TestCheckCAAQueryErrorIsInconclusive(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"example.com": {err: errors.New("network down")},
		},
	}
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	_, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "example.com")})
	if !errors.Is(err, ErrCAAInconclusive) {
		t.Fatalf("expected a lookup error to be inconclusive, got %v", err)
	}
}

func TestCheckCAAWildcard(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"example.com": {
				records: []*CAARecord{ptr(wildRec("letsencrypt.org"))},
				status:  CAALookupFound,
			},
		},
	}
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	reports, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "*.example.com")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reports[0].LookedUpName != "example.com" || reports[0].Verdict != CAAAllowed {
		t.Fatalf("unexpected report: %+v", reports[0])
	}
}

func TestCheckCAASkipsNonDNS(t *testing.T) {
	q := &fakeCAAQuerier{}
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	reports, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("ip", "192.0.2.1")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reports) != 0 || len(q.queried) != 0 {
		t.Fatalf("non-DNS identifiers must be skipped, got reports=%v queried=%v", reports, q.queried)
	}
}

func TestCheckCAAUnknownCAIdentitiesIsInconclusive(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"example.com": {
				records: []*CAARecord{ptr(issueRec("letsencrypt.org"))},
				status:  CAALookupFound,
			},
		},
	}
	pre := defaultPre()
	pre.Querier = q
	pre.CAIdentities = nil // and no directory available

	client := newCAAClient() // embedded acme.Client has no Directory -> provision fails
	_, err := client.CheckCAA(context.Background(), pre, acme.Account{},
		[]acme.Identifier{id("dns", "example.com")})
	if !errors.Is(err, ErrCAAInconclusive) {
		t.Fatalf("expected unknown CA identity to be inconclusive, got %v", err)
	}
}

func TestObtainCertificatePreflightBlocksBeforeOrder(t *testing.T) {
	q := &fakeCAAQuerier{
		responses: map[string]struct {
			records []*CAARecord
			status  CAALookupStatus
			err     error
		}{
			"example.com": {
				records: []*CAARecord{ptr(issueRec("other-ca.example"))},
				status:  CAALookupFound,
			},
		},
	}
	pre := defaultPre()
	pre.Querier = q

	client := newCAAClient()
	client.CAAPrecheck = &pre

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewCSR(key, []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	params, err := OrderParametersFromCSR(acme.Account{Status: acme.StatusValid}, csr)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.ObtainCertificate(context.Background(), params)
	if !errors.Is(err, ErrCAABlocked) {
		t.Fatalf("expected ObtainCertificate to fail with ErrCAABlocked before any order, got %v", err)
	}
}
