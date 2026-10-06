// Copyright 2020 Matthew Holt
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
	"errors"
	"testing"

	"github.com/mholt/acmez/v3/acme"
)

// fakeCAAQueryer answers CAA queries from an in-memory table, so tests
// never touch the real DNS. Names are matched without a trailing dot
// and case-insensitively.
type fakeCAAQueryer struct {
	records map[string][]CAARecord
	cname   map[string]string
	err     map[string]error

	calls []string
}

func (f *fakeCAAQueryer) LookupCAA(_ context.Context, fqdn string) ([]CAARecord, string, error) {
	name := normalizeName(fqdn)
	f.calls = append(f.calls, name)
	if err := f.err[name]; err != nil {
		return nil, "", err
	}
	cname := f.cname[name]
	if cname == "" {
		cname = name
	}
	return f.records[name], cname, nil
}

func issue(ca string) CAARecord {
	return parseCAARecord("issue", ca, false)
}

func issuewild(ca string) CAARecord {
	return parseCAARecord("issuewild", ca, false)
}

func TestCAAChecker(t *testing.T) {
	const account = "https://example-acme.ca/acct/1"

	tests := []struct {
		name       string
		records    map[string][]CAARecord
		cname      map[string]string
		queryErr   map[string]error
		identifier string
		account    string
		methods    []string
		wantStatus string
		wantCalls  []string
	}{
		{
			name:       "no records allows",
			records:    map[string][]CAARecord{},
			identifier: "example.com",
			wantStatus: CAAStatusAllowed,
			wantCalls:  []string{"example.com", "com"},
		},
		{
			name:       "issuer matches",
			records:    map[string][]CAARecord{"example.com": {issue("letsencrypt.org")}},
			identifier: "example.com",
			wantStatus: CAAStatusAllowed,
			wantCalls:  []string{"example.com", "com"},
		},
		{
			name:       "issuer at parent matches",
			records:    map[string][]CAARecord{"com": {issue("letsencrypt.org")}},
			identifier: "www.example.com",
			wantStatus: CAAStatusAllowed,
			wantCalls:  []string{"www.example.com", "example.com", "com"},
		},
		{
			name:       "other issuer denied",
			records:    map[string][]CAARecord{"example.com": {issue("other-ca.example")}},
			identifier: "example.com",
			wantStatus: CAAStatusDenied,
			wantCalls:  []string{"example.com", "com"},
		},
		{
			name:       "empty issuer explicit deny",
			records:    map[string][]CAARecord{"example.com": {issue(";")}},
			identifier: "example.com",
			wantStatus: CAAStatusDenied,
		},
		{
			name: "iodef only is no issue property",
			records: map[string][]CAARecord{
				"example.com": {parseCAARecord("iodef", "mailto:ops@example.com", false)},
			},
			identifier: "example.com",
			wantStatus: CAAStatusDenied,
		},
		{
			name: "wildcard prefers issuewild over issue",
			records: map[string][]CAARecord{
				"example.com": {
					issue("other-ca.example"),
					issuewild("letsencrypt.org"),
				},
			},
			identifier: "*.example.com",
			wantStatus: CAAStatusAllowed,
		},
		{
			name: "wildcard denied by issuewild, issue ignored",
			records: map[string][]CAARecord{
				"example.com": {
					issue("letsencrypt.org"),
					issuewild("other-ca.example"),
				},
			},
			identifier: "*.example.com",
			wantStatus: CAAStatusDenied,
		},
		{
			name: "issuewild at ancestor overrides issue below",
			records: map[string][]CAARecord{
				"www.example.com": {issue("letsencrypt.org")},
				"example.com":     {issuewild("other-ca.example")},
			},
			identifier: "*.www.example.com",
			wantStatus: CAAStatusDenied,
		},
		{
			name:       "non-wildcard ignores issuewild",
			records:    map[string][]CAARecord{"example.com": {issuewild("letsencrypt.org")}},
			identifier: "www.example.com",
			wantStatus: CAAStatusDenied,
		},
		{
			name:       "accounturi matches",
			records:    map[string][]CAARecord{"example.com": {issue("letsencrypt.org; accounturi=" + account)}},
			identifier: "example.com",
			account:    account,
			wantStatus: CAAStatusAllowed,
		},
		{
			name:       "accounturi mismatch denied",
			records:    map[string][]CAARecord{"example.com": {issue("letsencrypt.org; accounturi=" + account)}},
			identifier: "example.com",
			account:    "https://example-acme.ca/acct/2",
			wantStatus: CAAStatusDenied,
		},
		{
			name:       "accounturi without account URL undetermined",
			records:    map[string][]CAARecord{"example.com": {issue("letsencrypt.org; accounturi=" + account)}},
			identifier: "example.com",
			wantStatus: CAAStatusUndetermined,
		},
		{
			name:       "validationmethods allows permitted method",
			records:    map[string][]CAARecord{"example.com": {issue(`letsencrypt.org; validationmethods="dns-01 http-01"`)}},
			identifier: "example.com",
			methods:    []string{"http-01"},
			wantStatus: CAAStatusAllowed,
		},
		{
			name:       "validationmethods denies disallowed method",
			records:    map[string][]CAARecord{"example.com": {issue(`letsencrypt.org; validationmethods="dns-01"`)}},
			identifier: "example.com",
			methods:    []string{"http-01"},
			wantStatus: CAAStatusDenied,
		},
		{
			name: "validationmethods union across records",
			records: map[string][]CAARecord{"example.com": {
				issue(`letsencrypt.org; validationmethods="http-01"`),
				issue(`letsencrypt.org; validationmethods="dns-01"`),
			}},
			identifier: "example.com",
			methods:    []string{"http-01", "dns-01"},
			wantStatus: CAAStatusAllowed,
		},
		{
			name: "record without validationmethods permits all",
			records: map[string][]CAARecord{"example.com": {
				issue(`letsencrypt.org; validationmethods="dns-01"`),
				issue("letsencrypt.org"),
			}},
			identifier: "example.com",
			methods:    []string{"http-01"},
			wantStatus: CAAStatusAllowed,
		},
		{
			name:       "unknown critical tag denied",
			records:    map[string][]CAARecord{"example.com": {parseCAARecord("tbs", "something", true)}},
			identifier: "example.com",
			wantStatus: CAAStatusDenied,
		},
		{
			name:       "query error undetermined",
			queryErr:   map[string]error{"example.com": errors.New("SERVFAIL")},
			identifier: "example.com",
			wantStatus: CAAStatusUndetermined,
			wantCalls:  []string{"example.com"},
		},
		{
			name:       "cname restarts climb",
			cname:      map[string]string{"example.com": "alias.target.net"},
			records:    map[string][]CAARecord{"alias.target.net": {issue("letsencrypt.org")}},
			identifier: "example.com",
			wantStatus: CAAStatusAllowed,
			wantCalls:  []string{"example.com", "alias.target.net", "target.net", "net"},
		},
		{
			name:       "cname loop undetermined",
			cname:      map[string]string{"a.example.com": "b.example.com", "b.example.com": "a.example.com"},
			identifier: "a.example.com",
			wantStatus: CAAStatusUndetermined,
		},
		{
			name:       "non-dns identifier skipped",
			identifier: "192.0.2.10",
			wantStatus: CAAStatusAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queryer := &fakeCAAQueryer{
				records: tc.records,
				cname:   tc.cname,
				err:     tc.queryErr,
			}
			checker := &CAAChecker{
				Queryer:        queryer,
				CAAIdentities:  []string{"letsencrypt.org"},
				OnUndetermined: CAAPolicyBlockOnUndetermined,
			}
			req := CAACheckRequest{
				Identifier:        acme.Identifier{Type: "dns", Value: tc.identifier},
				AccountURI:        tc.account,
				ValidationMethods: tc.methods,
			}
			if tc.name == "non-dns identifier skipped" {
				req.Identifier = acme.Identifier{Type: "ip", Value: tc.identifier}
			}

			results := checker.CheckResults(context.Background(), []CAACheckRequest{req})
			if len(results) != 1 {
				t.Fatalf("expected 1 result, got %d", len(results))
			}
			if got := results[0].Status; got != tc.wantStatus {
				t.Fatalf("status = %q (%s), want %q", got, results[0].Reason, tc.wantStatus)
			}
			if tc.wantCalls != nil && !equalStringSlices(queryer.calls, tc.wantCalls) {
				t.Fatalf("queries = %v, want %v", queryer.calls, tc.wantCalls)
			}
		})
	}
}

func TestCAACheckerUnknownIdentityUndetermined(t *testing.T) {
	queryer := &fakeCAAQueryer{
		records: map[string][]CAARecord{"example.com": {issue("letsencrypt.org")}},
	}
	checker := &CAAChecker{Queryer: queryer} // no CAAIdentities
	results := checker.CheckResults(context.Background(), []CAACheckRequest{{
		Identifier: acme.Identifier{Type: "dns", Value: "example.com"},
	}})
	if results[0].Status != CAAStatusUndetermined {
		t.Fatalf("status = %q, want undetermined", results[0].Status)
	}
}

func TestCAAPolicy(t *testing.T) {
	block := &CAAChecker{OnUndetermined: CAAPolicyBlockOnUndetermined}
	warn := &CAAChecker{OnUndetermined: CAAPolicyWarnOnUndetermined}

	denied := []CAAResult{{Status: CAAStatusDenied}}
	if err := warn.ApplyPolicy(denied); err == nil {
		t.Fatal("denied result must block even in warn mode")
	}

	undetermined := []CAAResult{{Status: CAAStatusUndetermined}}
	if err := block.ApplyPolicy(undetermined); err == nil {
		t.Fatal("undetermined result must block by default")
	}
	if err := warn.ApplyPolicy(undetermined); err != nil {
		t.Fatalf("undetermined result should not block in warn mode: %v", err)
	}

	allowed := []CAAResult{{Status: CAAStatusAllowed}}
	if err := block.ApplyPolicy(allowed); err != nil {
		t.Fatalf("allowed result must not block: %v", err)
	}

	var caaErr *CAAError
	if err := block.ApplyPolicy(undetermined); !errors.As(err, &caaErr) {
		t.Fatalf("expected *CAAError, got %T", err)
	}
}

func TestCAACheckCombinedError(t *testing.T) {
	queryer := &fakeCAAQueryer{
		records: map[string][]CAARecord{
			"bad.example": {issue("other-ca.example")},
		},
	}
	checker := &CAAChecker{Queryer: queryer, CAAIdentities: []string{"letsencrypt.org"}}
	_, err := checker.Check(context.Background(), []CAACheckRequest{
		{Identifier: acme.Identifier{Type: "dns", Value: "good.example"}},
		{Identifier: acme.Identifier{Type: "dns", Value: "bad.example"}},
	})
	if err == nil {
		t.Fatal("expected error for one denied identifier")
	}
	var caaErr *CAAError
	if !errors.As(err, &caaErr) {
		t.Fatalf("expected *CAAError, got %T", err)
	}
	if len(caaErr.Results) != 2 {
		t.Fatalf("expected 2 results in error, got %d", len(caaErr.Results))
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
