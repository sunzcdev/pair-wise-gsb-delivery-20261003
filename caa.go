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
	"errors"
	"strings"

	"github.com/mholt/acmez/v3/acme"
)

// CAA-related property tags and parameter names.
//
// The property tags are defined by RFC 8659 ("DNS Certification Authority
// Authorization (CAA) Resource Record") and the optional parameters accepted
// by the "issue" and "issuewild" properties are defined by RFC 8657 ("CAA
// Record Extension for Account and Method Binding").
const (
	// CAA tag "issue": authorizes the named CA to issue non-wildcard
	// (and, in the absence of "issuewild", wildcard) certificates.
	CAATagIssue = "issue"

	// CAA tag "issuewild": only applies when the requested certificate
	// is for a wildcard identifier.
	CAATagIssueWild = "issuewild"

	// CAA "issue"/"issuewild" parameter which constrains issuance to a
	// specific ACME account URL. RFC 8657 §4.1.
	CAAParamAccountURI = "accounturi"

	// CAA "issue"/"issuewild" parameter which constrains issuance to a
	// set of permitted domain validation methods. RFC 8657 §4.2.
	CAAParamValidationMethods = "validationmethods"
)

// CAAVerdict is the result of evaluating the CAA records for one identifier
// against a target CA, account URL, and set of validation methods.
type CAAVerdict int

const (
	// CAAInconclusive means the authoritative CAA policy could not be
	// determined (for example because the DNS response could not be
	// obtained or was ambiguous). It MUST NOT be treated as "allowed".
	CAAInconclusive CAAVerdict = iota

	// CAAAllowed means the CAA records permit issuance by the target CA
	// for the requested account and validation methods, or that the
	// domain publishes no CAA records at all (which, per RFC 8659,
	// means any CA may issue).
	CAAAllowed

	// CAABlocked means an authoritative CAA policy exists and forbids
	// the requested issuance.
	CAABlocked
)

// String returns a human-readable representation of the verdict.
func (v CAAVerdict) String() string {
	switch v {
	case CAAAllowed:
		return "allowed"
	case CAABlocked:
		return "blocked"
	default:
		return "inconclusive"
	}
}

// CAAInconclusivePolicy decides what happens when a CAA precheck cannot
// reach a definitive conclusion for an identifier.
//
// The zero value is CAABlockOnInconclusive: when in doubt, do not proceed.
// This deliberately fails closed, because an ambiguous CAA answer must never
// be silently assumed to permit issuance.
type CAAInconclusivePolicy int

const (
	// CAABlockOnInconclusive stops the issuance attempt and reports an
	// error for any identifier whose CAA policy cannot be determined.
	CAABlockOnInconclusive CAAInconclusivePolicy = iota

	// CAAContinueOnInconclusive reports the identifier as inconclusive but
	// still proceeds with the ACME transaction. This is useful when the
	// precheck is purely advisory, but it removes the guarantee that a
	// failed DNS lookup surfaces early.
	CAAContinueOnInconclusive
)

// CAAPrecheck configures an optional, advisory pre-issuance check of DNS
// Certification Authority Authorization (CAA) records.
//
// The precheck is evaluated locally, before any challenges are provisioned,
// to surface CAA misconfiguration early. It is NOT an authorization decision:
// the CA performs its own authoritative CAA validation (RFC 8659 §4) when the
// order is finalized and remains the sole source of truth for whether a
// certificate will be issued.
type CAAPrecheck struct {
	// Enabled turns on the precheck. The zero value disables it, which
	// preserves the default ACME behavior of contacting no DNS beyond
	// what the configured solvers require.
	Enabled bool

	// CAIdentities lists the issuer domain names to look for in "issue"
	// and "issuewild" records. It should be derived from the CA's
	// directory "meta.caaIdentities" (RFC 8555 §7.1.1). If empty, the
	// CA identities advertised by the configured directory are used.
	// If none are known, the result is inconclusive rather than guessing.
	CAIdentities []string

	// AccountURL is the full ACME account URL (the account object's
	// "kid"/Location) that will be used for the order. It is compared
	// against any "accounturi" parameters. If empty, the preorder's
	// account Location is used.
	AccountURL string

	// ValidationMethods are the ACME challenge types the client is
	// prepared to use (for example acme.ChallengeTypeDNS01,
	// acme.ChallengeTypeHTTP01, acme.ChallengeTypeTLSALPN01). They are
	// compared against any "validationmethods" parameters. If empty,
	// the keys of Client.ChallengeSolvers that name a domain-validation
	// challenge are used. If none are available, the result is
	// inconclusive rather than assuming a method the client cannot use.
	ValidationMethods []string

	// ValidationMethodNames optionally maps a validation method to the
	// DNS name on which that method's validation will be performed.
	// RFC 8657 §3.2.2 allows an entry in a "validationmethods" list to
	// be scoped with a "name" variable (relevant for dns-01, including
	// CNAME-delegated validation). If a record scopes a method with a
	// "name" and no matching value is provided here for that method,
	// the result is inconclusive, since the correct zone is unknown
	// before the challenge.
	ValidationMethodNames map[string]string

	// OnInconclusive controls behavior when an identifier's CAA policy
	// cannot be determined. The zero value blocks (fails closed).
	OnInconclusive CAAInconclusivePolicy

	// Querier performs the CAA lookups. If nil, a DefaultDNSCAAQuerier
	// is used. Replace it to route queries through a custom resolver,
	// to inject test data, or to avoid contacting the public DNS.
	Querier CAAQuerier
}

// CAAQuerier retrieves the CAA records for a DNS name.
//
// Implementations return the CAA records published at name, following the
// CNAME chain as needed (RFC 8659 §4.4), along with a status describing how
// to interpret an empty answer set. The tree-walk toward parent zones is
// performed by the precheck itself, not by the Querier, so implementations
// only answer the single question "what is at this exact name?".
type CAAQuerier interface {
	LookupCAA(ctx context.Context, name string) (records []*CAARecord, status CAALookupStatus, err error)
}

// CAAQuerierFunc adapts a function into a CAAQuerier.
type CAAQuerierFunc func(ctx context.Context, name string) ([]*CAARecord, CAALookupStatus, error)

// LookupCAA calls f.
func (f CAAQuerierFunc) LookupCAA(ctx context.Context, name string) ([]*CAARecord, CAALookupStatus, error) {
	return f(ctx, name)
}

// CAALookupStatus describes how to interpret a CAA lookup that returned no
// records. It maps the DNS response classes from RFC 8659 §4.7.
type CAALookupStatus int

const (
	// CAALookupFound means records are non-empty and should be evaluated.
	CAALookupFound CAALookupStatus = iota

	// CAALookupAuthoritativeEmpty means the response is an authoritative
	// negative answer (NODATA with an SOA in the authority section): the
	// queried zone confirms that no CAA records exist at this name, so
	// the precheck may continue walking toward the parent zone.
	CAALookupAuthoritativeEmpty

	// CAALookupNameError means the response is an authoritative NXDOMAIN.
	// Per RFC 8659 §4.7, the precheck retries at the parent zone.
	CAALookupNameError

	// CAALookupIndeterminate means the response could not establish whether
	// CAA records exist (a non-authoritative negative answer, a referral, a
	// malformed response, etc.). The identifier is treated as inconclusive.
	CAALookupIndeterminate
)

// CAAReport summarizes the CAA precheck for a single identifier.
type CAAReport struct {
	// Identifier is the ACME identifier that was evaluated. Only "dns"
	// identifiers are examined; other types are skipped and not reported.
	Identifier acme.Identifier

	// LookedUpName is the name actually queried for the controlling CAA
	// record set after stripping a wildcard label ("*.example.com" is
	// looked up as "example.com").
	LookedUpName string

	// Records are the controlling CAA records (empty when no policy was
	// published or when the verdict is inconclusive).
	Records []CAARecord

	// Verdict is the evaluated result.
	Verdict CAAVerdict

	// Reason is a short, human-readable explanation; it is set for
	// blocked and inconclusive verdicts.
	Reason string
}

// IsWildcard reports whether the DNS identifier requests a wildcard
// certificate (its value begins with "*.").
func IsWildcard(identifier acme.Identifier) bool {
	return identifier.Type == "dns" && strings.HasPrefix(identifier.Value, "*.")
}

// Sentinel errors returned for precheck failures. They are wrapped with
// per-identifier detail; use errors.Is to classify them.
var (
	// ErrCAABlocked indicates that at least one identifier's CAA records
	// forbid the requested CA, account, or validation method.
	ErrCAABlocked = errors.New("CAA records block issuance")

	// ErrCAAInconclusive indicates that the CAA policy for at least one
	// identifier could not be determined and the caller chose to block.
	ErrCAAInconclusive = errors.New("CAA policy could not be determined")
)

// caaError is the per-identifier error wrapped around the sentinels above.
type caaError struct {
	sentinel     error
	identifier   acme.Identifier
	lookedUpName string
	reason       string
}

func (e *caaError) Error() string {
	label := "CAA"
	if e.lookedUpName != "" && e.lookedUpName != e.identifier.Value {
		label = "CAA for " + e.identifier.Value + " (queried " + e.lookedUpName + ")"
	} else {
		label = "CAA for " + e.identifier.Value
	}
	return label + ": " + e.reason
}

func (e *caaError) Unwrap() error { return e.sentinel }
