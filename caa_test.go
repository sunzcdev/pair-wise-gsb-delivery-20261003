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
	"testing"
)

func mustParseCAA(t *testing.T, flag uint8, tag, value string) CAARecord {
	t.Helper()
	rec, err := parseCAARecord(flag, tag, value)
	if err != nil {
		t.Fatalf("parseCAARecord(%q, %q): %v", tag, value, err)
	}
	return rec
}

func issueRec(issuer string, params ...string) CAARecord {
	value := issuer
	for i := 0; i+1 < len(params); i += 2 {
		value += "; " + params[i] + "=" + params[i+1]
	}
	rec, _ := parseCAARecord(0, CAATagIssue, value)
	return rec
}

func TestParseCAARecord(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		issuer     string
		accountURI string
		methods    string
		scope      string
		wantErr    bool
	}{
		{
			name:   "plain issuer",
			value:  "letsencrypt.org",
			issuer: "letsencrypt.org",
		},
		{
			name:   "issuer with spaces and trailing dot",
			value:  "  LetsEncrypt.ORG.  ",
			issuer: "letsencrypt.org",
		},
		{
			name:   "deny all",
			value:  ";",
			issuer: "",
		},
		{
			name:       "account binding quoted",
			value:      `letsencrypt.org; accounturi="https://example/acct/1"`,
			issuer:     "letsencrypt.org",
			accountURI: "https://example/acct/1",
		},
		{
			name:    "validation methods quoted list",
			value:   `letsencrypt.org; validationmethods="http-01,dns-01"`,
			issuer:  "letsencrypt.org",
			methods: "http-01,dns-01",
		},
		{
			name:    "methods plus RFC 8657 name scope",
			value:   `letsencrypt.org; validationmethods="dns-01"; name="_acme-challenge.example.com"`,
			issuer:  "letsencrypt.org",
			methods: "dns-01",
			scope:   "_acme-challenge.example.com",
		},
		{
			name:    "semicolon inside quotes is literal",
			value:   `pki.example.net; validationmethods="http-01;name=foo"`,
			issuer:  "pki.example.net",
			methods: "http-01;name=foo",
		},
		{
			name:       "quoted with escaped characters",
			value:      `ca.example; accounturi="https://exa\057mple/a;b"`,
			issuer:     "ca.example",
			accountURI: "https://exa/mple/a;b",
		},
		{
			name:    "unterminated quote",
			value:   `letsencrypt.org; accounturi="oops`,
			wantErr: true,
		},
		{
			name:    "parameter without equals",
			value:   `letsencrypt.org; accounturi`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := parseCAARecord(0, "issue", tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", rec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.IssuerDomain != tc.issuer {
				t.Errorf("issuer = %q, want %q", rec.IssuerDomain, tc.issuer)
			}
			if got := rec.Parameters[CAAParamAccountURI]; got != tc.accountURI {
				t.Errorf("accounturi = %q, want %q", got, tc.accountURI)
			}
			if got := rec.Parameters[CAAParamValidationMethods]; got != tc.methods {
				t.Errorf("validationmethods = %q, want %q", got, tc.methods)
			}
			if got := rec.Parameters["name"]; got != tc.scope {
				t.Errorf("name = %q, want %q", got, tc.scope)
			}
		})
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(` http-01 , "dns-01" ,tls-alpn-01 `)
	want := []string{"http-01", "dns-01", "tls-alpn-01"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("element %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParentDomain(t *testing.T) {
	tests := map[string]string{
		"www.example.com": "example.com",
		"example.com":     "com",
		"com":             "",
	}
	for in, want := range tests {
		if got := parentDomain(in); got != want {
			t.Errorf("parentDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsWildcard(t *testing.T) {
	if !IsWildcard(id("dns", "*.example.com")) {
		t.Error("expected *.example.com to be wildcard")
	}
	if IsWildcard(id("dns", "example.com")) {
		t.Error("expected example.com not to be wildcard")
	}
	if IsWildcard(id("ip", "*.example.com")) {
		t.Error("non-dns identifier must not be treated as wildcard")
	}
}
