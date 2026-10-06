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

import "strings"

// caaEvaluation carries the inputs to a CAA policy evaluation.
type caaEvaluation struct {
	// caIdentities are the issuer domain names the requesting CA
	// recognizes as itself (directory meta.caaIdentities).
	caIdentities []string

	// accountURL is the ACME account URL the order will use.
	accountURL string

	// methods are the validation methods the client is prepared to use.
	methods map[string]bool

	// methodNames maps a method to the DNS name on which validation will
	// be performed (RFC 8657 §3.2.2 name scoping).
	methodNames map[string]string

	// wildcard is true for a "*.example.com" identifier.
	wildcard bool
}

// evaluateCAA applies the CAA issuance policy from RFC 8659 §4.5 and the
// account/method binding rules from RFC 8657 to a single controlling record
// set.
//
// It returns the verdict and a non-empty reason for any verdict other than
// CAAAllowed.
func evaluateCAA(records []CAARecord, in caaEvaluation) (CAAVerdict, string) {
	// RFC 8659 §4.7: a critical property the CA does not recognize
	// forbids issuance outright.
	for _, rec := range records {
		if rec.Critical() && !knownCAATags[rec.Tag] {
			return CAABlocked, "critical CAA property not recognized: " + rec.Tag
		}
	}

	eligible := selectIssuerRecords(records, in.wildcard)

	// RFC 8659 §4.3: if an "issue" property is marked critical, then it
	// also applies to wildcard requests and a matching critical "issuewild"
	// must be present; otherwise a CA MUST NOT issue the wildcard
	// certificate (even when a non-critical "issuewild" would allow it).
	if in.wildcard {
		criticalIssue := false
		criticalIssueWild := false
		for _, rec := range records {
			switch rec.Tag {
			case CAATagIssue:
				criticalIssue = criticalIssue || rec.Critical()
			case CAATagIssueWild:
				criticalIssueWild = criticalIssueWild || rec.Critical()
			}
		}
		if criticalIssue && !criticalIssueWild {
			return CAABlocked, "critical issue property exists but no critical issuewild property; wildcard issuance forbidden"
		}
	}

	// RFC 8659 §4.5: if no issue/issuewild properties exist for this
	// request, the holder of the DNS name makes no assertion and the
	// CA may issue.
	if len(eligible) == 0 {
		return CAAAllowed, ""
	}

	matched := matchingIssuerRecords(eligible, in.caIdentities)
	if len(matched) == 0 {
		return CAABlocked, "no issue" + issuerTagName(in.wildcard) +
			" record names the target CA (" + strings.Join(in.caIdentities, ", ") + ")"
	}

	// RFC 8657 constraints are evaluated globally across every matching
	// issuer record: satisfying ANY record is sufficient.
	if verdict, reason := evaluateAccountURI(matched, in); verdict != CAAAllowed {
		return verdict, reason
	}
	if verdict, reason := evaluateValidationMethods(matched, in); verdict != CAAAllowed {
		return verdict, reason
	}

	return CAAAllowed, ""
}

// selectIssuerRecords returns the "issue"/"issuewild" records that govern the
// request. For a wildcard, "issuewild" is preferred and "issue" is only used
// when no "issuewild" record is present (RFC 8659 §4.3). For a non-wildcard
// name, only "issue" records apply and "issuewild" is ignored.
func selectIssuerRecords(records []CAARecord, wildcard bool) []CAARecord {
	if wildcard {
		var wild []CAARecord
		for _, rec := range records {
			if rec.Tag == CAATagIssueWild {
				wild = append(wild, rec)
			}
		}
		if len(wild) > 0 {
			return wild
		}
	}

	var plain []CAARecord
	for _, rec := range records {
		if rec.Tag == CAATagIssue {
			plain = append(plain, rec)
		}
	}
	return plain
}

// matchingIssuerRecords returns the issuer records that name the requesting
// CA. An empty issuer domain ("issue" ";") is an explicit deny-all and never
// matches (RFC 8659 §4.5).
func matchingIssuerRecords(records []CAARecord, caIdentities []string) []CAARecord {
	wanted := make(map[string]bool, len(caIdentities))
	for _, identity := range caIdentities {
		if identity = normalizeIssuerDomain(identity); identity != "" {
			wanted[identity] = true
		}
	}

	var matched []CAARecord
	for _, rec := range records {
		if rec.IssuerDomain != "" && wanted[rec.IssuerDomain] {
			matched = append(matched, rec)
		}
	}
	return matched
}

// evaluateAccountURI enforces the RFC 8657 §4.1 account binding. If no
// matching issuer record carries an accounturi parameter there is no
// constraint. Otherwise the requested account URL must be named by one of
// them.
func evaluateAccountURI(matched []CAARecord, in caaEvaluation) (CAAVerdict, string) {
	constrained := false
	for _, rec := range matched {
		uri, ok := rec.Parameters[CAAParamAccountURI]
		if !ok {
			continue
		}
		constrained = true
		if in.accountURL != "" && accountURIMatches(uri, in.accountURL) {
			return CAAAllowed, ""
		}
	}

	if !constrained {
		return CAAAllowed, ""
	}
	if in.accountURL == "" {
		return CAAInconclusive, "CAA record constrains accounturi but no account URL is configured"
	}
	return CAABlocked, "account " + in.accountURL + " is not named by an accounturi parameter"
}

// accountURIMatches compares a CAA accounturi value against the account URL
// in use. The account-uri parameter carries exactly one URI per RFC 8657
// §4.1 (additional accounts are expressed with separate CAA records), so the
// comparison is an exact match after trimming surrounding whitespace; a
// comma-separated value is deliberately not split, to avoid matching an
// account that a multi-URI value never actually authorized.
func accountURIMatches(parameterValue, accountURL string) bool {
	return strings.TrimSpace(parameterValue) == accountURL
}

// evaluateValidationMethods enforces the RFC 8657 §4.2 method binding. If no
// matching issuer record carries a validationmethods parameter there is no
// constraint. Otherwise at least one method the client is prepared to use
// must be permitted. A "name"-scoped method (RFC 8657 §3.2.2) for which the
// validation name is unknown makes the result inconclusive rather than
// guessing.
func evaluateValidationMethods(matched []CAARecord, in caaEvaluation) (CAAVerdict, string) {
	constrained := false
	sawUnknownScope := false

	for _, rec := range matched {
		raw, ok := rec.Parameters[CAAParamValidationMethods]
		if !ok {
			continue
		}
		constrained = true
		scopeName := normalizeIssuerDomain(rec.Parameters["name"])

		for _, method := range splitCSV(raw) {
			if !in.methods[method] {
				continue
			}
			if scopeName == "" {
				// Unscoped permit: this record authorizes the method.
				return CAAAllowed, ""
			}
			expected, known := in.methodNames[method]
			if !known || expected == "" {
				sawUnknownScope = true
				continue
			}
			if normalizeIssuerDomain(expected) == scopeName {
				return CAAAllowed, ""
			}
		}
	}

	if !constrained {
		return CAAAllowed, ""
	}
	if len(in.methods) == 0 {
		return CAAInconclusive, "validationmethods is constrained but no validation methods are configured"
	}
	if sawUnknownScope {
		return CAAInconclusive, "validationmethods record scopes a method with a name, but the validation DNS name is unknown"
	}
	return CAABlocked, "none of the configured validation methods are permitted by a validationmethods parameter"
}

// splitCSV splits a comma-separated parameter value, trimming whitespace and
// stripping surrounding quotes from each element.
func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
			if decoded, err := decodeCAAQuotedString(part); err == nil {
				part = decoded
			}
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// issuerTagName returns the tag name used in human-readable messages.
func issuerTagName(wildcard bool) string {
	if wildcard {
		return "wild"
	}
	return ""
}
