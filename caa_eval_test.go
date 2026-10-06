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

	"github.com/mholt/acmez/v3/acme"
)

func id(typ, value string) acme.Identifier {
	return acme.Identifier{Type: typ, Value: value}
}

func wildRec(issuer string, params ...string) CAARecord {
	value := issuer
	for i := 0; i+1 < len(params); i += 2 {
		value += "; " + params[i] + "=" + params[i+1]
	}
	rec, _ := parseCAARecord(0, CAATagIssueWild, value)
	return rec
}

func recWith(flag uint8, tag, value string) CAARecord {
	rec, _ := parseCAARecord(flag, tag, value)
	return rec
}

func TestEvaluateCAA(t *testing.T) {
	const acct = "https://acme.example/acct/1"

	base := func() caaEvaluation {
		return caaEvaluation{
			caIdentities: []string{"letsencrypt.org"},
			accountURL:   acct,
			methods: map[string]bool{
				acme.ChallengeTypeHTTP01:    true,
				acme.ChallengeTypeDNS01:     true,
				acme.ChallengeTypeTLSALPN01: true,
			},
		}
	}

	tests := []struct {
		name     string
		records  []CAARecord
		mutate   func(*caaEvaluation)
		wildcard bool
		want     CAAVerdict
	}{
		{
			name:    "no records",
			records: nil,
			want:    CAAAllowed,
		},
		{
			name:    "matching plain issue",
			records: []CAARecord{issueRec("letsencrypt.org")},
			want:    CAAAllowed,
		},
		{
			name:    "matching issue case insensitive and trailing dot",
			records: []CAARecord{issueRec("LetsEncrypt.ORG.")},
			want:    CAAAllowed,
		},
		{
			name:    "non-matching issuer blocks",
			records: []CAARecord{issueRec("other-ca.example")},
			want:    CAABlocked,
		},
		{
			name:    "explicit deny all blocks",
			records: []CAARecord{recWith(0, CAATagIssue, ";")},
			want:    CAABlocked,
		},
		{
			name: "one of several records names the CA",
			records: []CAARecord{
				issueRec("other-ca.example"),
				issueRec("letsencrypt.org"),
			},
			want: CAAAllowed,
		},
		{
			name: "deny plus authorizing record still allows",
			records: []CAARecord{
				recWith(0, CAATagIssue, ";"),
				issueRec("letsencrypt.org"),
			},
			want: CAAAllowed,
		},
		{
			name: "accounturi matches",
			records: []CAARecord{
				issueRec("letsencrypt.org", CAAParamAccountURI, `"`+acct+`"`),
			},
			want: CAAAllowed,
		},
		{
			name: "accounturi mismatch blocks",
			records: []CAARecord{
				issueRec("letsencrypt.org", CAAParamAccountURI, `"https://acme.example/acct/9"`),
			},
			want: CAABlocked,
		},
		{
			name: "account constraint but no account URL configured is inconclusive",
			records: []CAARecord{
				issueRec("letsencrypt.org", CAAParamAccountURI, `"`+acct+`"`),
			},
			mutate: func(e *caaEvaluation) { e.accountURL = "" },
			want:   CAAInconclusive,
		},
		{
			name: "validationmethods includes a configured method",
			records: []CAARecord{
				issueRec("letsencrypt.org", CAAParamValidationMethods, `"dns-01,http-01"`),
			},
			want: CAAAllowed,
		},
		{
			name: "validationmethods excludes configured methods",
			records: []CAARecord{
				issueRec("letsencrypt.org", CAAParamValidationMethods, `"dns-01"`),
			},
			mutate: func(e *caaEvaluation) {
				e.methods = map[string]bool{acme.ChallengeTypeHTTP01: true}
			},
			want: CAABlocked,
		},
		{
			name: "method constraint but no methods known is inconclusive",
			records: []CAARecord{
				issueRec("letsencrypt.org", CAAParamValidationMethods, `"dns-01"`),
			},
			mutate: func(e *caaEvaluation) { e.methods = map[string]bool{} },
			want:   CAAInconclusive,
		},
		{
			name: "name scoped method with matching name",
			records: []CAARecord{
				issueRec("letsencrypt.org",
					CAAParamValidationMethods, `"dns-01"`,
					"name", "_acme-challenge.example.com"),
			},
			mutate: func(e *caaEvaluation) {
				e.methodNames = map[string]string{
					acme.ChallengeTypeDNS01: "_acme-challenge.example.com",
				}
			},
			want: CAAAllowed,
		},
		{
			name: "name scoped method without provided name is inconclusive",
			records: []CAARecord{
				issueRec("letsencrypt.org",
					CAAParamValidationMethods, `"dns-01"`,
					"name", "_acme-challenge.example.com"),
			},
			want: CAAInconclusive,
		},
		{
			name: "name scoped with mismatched name blocks",
			records: []CAARecord{
				issueRec("letsencrypt.org",
					CAAParamValidationMethods, `"dns-01"`,
					"name", "_acme-challenge.example.com"),
			},
			mutate: func(e *caaEvaluation) {
				e.methodNames = map[string]string{
					acme.ChallengeTypeDNS01: "_acme-challenge.other.com",
				}
			},
			want: CAABlocked,
		},
		{
			name:     "non-wildcard ignores issuewild",
			records:  []CAARecord{wildRec("letsencrypt.org")},
			wildcard: false,
			want:     CAAAllowed,
		},
		{
			name:     "wildcard uses issuewild",
			records:  []CAARecord{wildRec("letsencrypt.org")},
			wildcard: true,
			want:     CAAAllowed,
		},
		{
			name: "wildcard prefers issuewild over issue",
			records: []CAARecord{
				issueRec("letsencrypt.org"),
				wildRec("other-ca.example"),
			},
			wildcard: true,
			want:     CAABlocked,
		},
		{
			name: "wildcard falls back to issue when no issuewild",
			records: []CAARecord{
				issueRec("letsencrypt.org"),
			},
			wildcard: true,
			want:     CAAAllowed,
		},
		{
			name: "wildcard blocked by critical issue without critical issuewild",
			records: []CAARecord{
				recWith(0x80, CAATagIssue, "letsencrypt.org"),
				wildRec("letsencrypt.org"),
			},
			wildcard: true,
			want:     CAABlocked,
		},
		{
			name: "wildcard allowed with matching critical issuewild",
			records: []CAARecord{
				recWith(0x80, CAATagIssue, "letsencrypt.org"),
				recWith(0x80, CAATagIssueWild, "letsencrypt.org"),
			},
			wildcard: true,
			want:     CAAAllowed,
		},
		{
			name: "unknown critical tag blocks",
			records: []CAARecord{
				issueRec("letsencrypt.org"),
				recWith(0x80, "future-critical", "something"),
			},
			want: CAABlocked,
		},
		{
			name: "unknown non-critical tag ignored",
			records: []CAARecord{
				issueRec("letsencrypt.org"),
				recWith(0, "unknown-x", "something"),
			},
			want: CAAAllowed,
		},
		{
			name: "iodef alone means no assertion",
			records: []CAARecord{
				recWith(0, "iodef", "mailto:security@example.com"),
			},
			want: CAAAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			in.wildcard = tc.wildcard
			got, reason := evaluateCAA(tc.records, in)
			if got != tc.want {
				t.Fatalf("verdict = %s (%s), want %s", got, reason, tc.want)
			}
		})
	}
}
