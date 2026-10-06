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

// newCAAClient builds a high-level client whose CAA pre-check is
// answered entirely in memory.
func newCAAClient(queryer CAAQueryer, policy string) *Client {
	return &Client{
		Client: &acme.Client{Directory: "http://127.0.0.1:0/dir"},
		CAAChecker: &CAAChecker{
			Queryer:        queryer,
			CAAIdentities:  []string{"letsencrypt.org"},
			OnUndetermined: policy,
		},
	}
}

func TestObtainCertificateBlockedByCAA(t *testing.T) {
	queryer := &fakeCAAQueryer{
		records: map[string][]CAARecord{
			"example.com": {issue("other-ca.example")},
		},
	}
	client := newCAAClient(queryer, CAAPolicyBlockOnUndetermined)

	params := OrderParameters{
		Account:     acme.Account{Status: acme.StatusValid, Location: "https://ca/acct/1"},
		Identifiers: []acme.Identifier{{Type: "dns", Value: "example.com"}},
		CSR:         StaticCSR(nil),
	}
	_, err := client.ObtainCertificate(context.Background(), params)
	var caaErr *CAAError
	if !errors.As(err, &caaErr) {
		t.Fatalf("expected *CAAError before any network call, got %v", err)
	}
}

func TestObtainCertificateCAAWarnProceeds(t *testing.T) {
	queryer := &fakeCAAQueryer{
		err: map[string]error{"example.com": errors.New("dns unavailable")},
	}
	client := newCAAClient(queryer, CAAPolicyWarnOnUndetermined)

	params := OrderParameters{
		Account:     acme.Account{Status: acme.StatusValid, Location: "https://ca/acct/1"},
		Identifiers: []acme.Identifier{{Type: "dns", Value: "example.com"}},
		CSR:         StaticCSR(nil),
	}
	// The pre-check is undetermined but warn policy proceeds; the next
	// failure must NOT be a CAA error (the unreachable directory is).
	_, err := client.ObtainCertificate(context.Background(), params)
	var caaErr *CAAError
	if errors.As(err, &caaErr) {
		t.Fatalf("undetermined result should not block in warn mode: %v", err)
	}
	if err == nil {
		t.Fatal("expected a non-CAA error when reaching the (unreachable) directory")
	}
}

func TestObtainCertificateCAABlocksUndeterminedByDefault(t *testing.T) {
	queryer := &fakeCAAQueryer{
		err: map[string]error{"example.com": errors.New("dns unavailable")},
	}
	client := newCAAClient(queryer, CAAPolicyBlockOnUndetermined)

	params := OrderParameters{
		Account:     acme.Account{Status: acme.StatusValid, Location: "https://ca/acct/1"},
		Identifiers: []acme.Identifier{{Type: "dns", Value: "example.com"}},
		CSR:         StaticCSR(nil),
	}
	_, err := client.ObtainCertificate(context.Background(), params)
	var caaErr *CAAError
	if !errors.As(err, &caaErr) {
		t.Fatalf("expected *CAAError for undetermined result, got %v", err)
	}
}
