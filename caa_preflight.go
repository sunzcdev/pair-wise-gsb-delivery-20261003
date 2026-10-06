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
	"log/slog"
	"strings"

	"github.com/mholt/acmez/v3/acme"
)

// domainValidationChallengeTypes are the ACME challenge types that demonstrate
// control over a DNS name and therefore make sense for CAA validationmethods
// (RFC 8657). Other solver keys (for example "email-reply-00") are not domain
// validation methods and are never offered as a CAA method.
var domainValidationChallengeTypes = map[string]bool{
	acme.ChallengeTypeHTTP01:    true,
	acme.ChallengeTypeDNS01:     true,
	acme.ChallengeTypeTLSALPN01: true,
}

// CheckCAA runs the configured CAA precheck against the given identifiers and
// returns one report per DNS identifier examined. Non-DNS identifiers are
// skipped.
//
// A nil report slice entry is never returned. When the precheck blocks, the
// returned error wraps ErrCAABlocked or ErrCAAInconclusive (errors.Is); the
// reports are still returned so callers can inspect which identifiers failed.
//
// This is a local, advisory check. The CA performs its own authoritative CAA
// evaluation and remains the final authority on issuance.
func (c *Client) CheckCAA(ctx context.Context, pre CAAPrecheck, account acme.Account, identifiers []acme.Identifier) ([]CAAReport, error) {
	querier := pre.Querier
	if querier == nil {
		querier = NewDefaultDNSCAAQuerier()
	}

	accountURL := pre.AccountURL
	if accountURL == "" {
		accountURL = account.Location
	}

	methods := make(map[string]bool)
	for _, method := range pre.ValidationMethods {
		methods[method] = true
	}
	if len(methods) == 0 {
		for challengeType, solver := range c.ChallengeSolvers {
			if solver != nil && domainValidationChallengeTypes[challengeType] {
				methods[challengeType] = true
			}
		}
	}

	caIdentities := pre.CAIdentities
	if len(caIdentities) == 0 {
		caIdentities = c.directoryCAAIdentities(ctx)
	}

	in := caaEvaluation{
		caIdentities: caIdentities,
		accountURL:   accountURL,
		methods:      methods,
		methodNames:  pre.ValidationMethodNames,
	}
	caIdentitiesKnown := len(caIdentities) > 0

	var reports []CAAReport
	var errs []error

	for _, identifier := range identifiers {
		if identifier.Type != "dns" {
			continue
		}

		wildcard := IsWildcard(identifier)
		name := identifier.Value
		if wildcard {
			name = strings.TrimPrefix(name, "*.")
		}
		name = strings.ToLower(strings.TrimSuffix(name, "."))

		report := CAAReport{
			Identifier:   identifier,
			LookedUpName: name,
		}

		if !caIdentitiesKnown {
			// We cannot identify the requesting CA, so we cannot tell
			// whether any published issue record authorizes it; guessing
			// would risk silently proceeding past a real restriction.
			report.Verdict = CAAInconclusive
			report.Reason = "target CA identity is unknown (directory advertises no caaIdentities)"
			reports = append(reports, report)
			if c.Logger != nil {
				c.Logger.Warn("CAA precheck inconclusive",
					slog.String("identifier", identifier.Value),
					slog.String("reason", report.Reason))
			}
			if pre.OnInconclusive == CAABlockOnInconclusive {
				errs = append(errs, &caaError{
					sentinel:     ErrCAAInconclusive,
					identifier:   identifier,
					lookedUpName: name,
					reason:       report.Reason,
				})
			}
			continue
		}

		records, verdict, reason := c.checkCAAName(ctx, querier, in, name, wildcard)
		report.Records = records
		report.Verdict = verdict
		report.Reason = reason
		reports = append(reports, report)

		switch verdict {
		case CAABlocked:
			errs = append(errs, &caaError{
				sentinel:     ErrCAABlocked,
				identifier:   identifier,
				lookedUpName: name,
				reason:       reason,
			})
		case CAAInconclusive:
			if c.Logger != nil {
				c.Logger.Warn("CAA precheck inconclusive",
					slog.String("identifier", identifier.Value),
					slog.String("reason", reason))
			}
			if pre.OnInconclusive == CAABlockOnInconclusive {
				errs = append(errs, &caaError{
					sentinel:     ErrCAAInconclusive,
					identifier:   identifier,
					lookedUpName: name,
					reason:       reason,
				})
			}
		}
	}

	return reports, errors.Join(errs...)
}

// directoryCAAIdentities returns the CAA identities advertised by the CA
// directory. It returns nil if the directory is unavailable or advertises no
// identities; callers treat that as "cannot determine" rather than guessing.
func (c *Client) directoryCAAIdentities(ctx context.Context) []string {
	directory, err := c.GetDirectory(ctx)
	if err != nil {
		return nil
	}
	if directory.Meta == nil {
		return nil
	}
	return directory.Meta.CAAIdentities
}

// checkCAAName walks the DNS tree for name's controlling CAA record set and
// evaluates it. The first non-empty set encountered is authoritative (RFC
// 8659 §4.4); an authoritative negative answer causes the walk to continue at
// the parent zone; any ambiguous answer is inconclusive.
func (c *Client) checkCAAName(ctx context.Context, querier CAAQuerier, in caaEvaluation, name string, wildcard bool) ([]CAARecord, CAAVerdict, string) {
	current := name
	for {
		rawRecords, status, err := querier.LookupCAA(ctx, current)
		if err != nil {
			return nil, CAAInconclusive, "CAA lookup failed: " + err.Error()
		}

		switch status {
		case CAALookupFound:
			records := make([]CAARecord, 0, len(rawRecords))
			for _, raw := range rawRecords {
				if raw == nil {
					continue
				}
				records = append(records, *raw)
			}
			verdict, reason := evaluateCAA(records, caaEvaluationWithWildcard(in, wildcard))
			return records, verdict, reason

		case CAALookupIndeterminate:
			return nil, CAAInconclusive, "indeterminate CAA response for " + current

		case CAALookupAuthoritativeEmpty, CAALookupNameError:
			parent := parentDomain(current)
			if parent == current || parent == "" {
				// Reached the top of the tree with no CAA records: the
				// domain publishes no policy, so issuance is allowed.
				return nil, CAAAllowed, ""
			}
			current = parent
		}
	}
}

// caaEvaluationWithWildcard returns a copy of in with the wildcard flag set.
func caaEvaluationWithWildcard(in caaEvaluation, wildcard bool) caaEvaluation {
	in.wildcard = wildcard
	return in
}

// parentDomain strips the leftmost label from a fully-qualified (but without
// trailing dot) domain name. parentDomain("example.com") == "com" and
// parentDomain("com") == "".
func parentDomain(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return ""
}

// preflightCAA runs the order's CAA precheck, if enabled, before any ACME
// resources or challenges are created.
func (c *Client) preflightCAA(ctx context.Context, pre *CAAPrecheck, account acme.Account, identifiers []acme.Identifier) error {
	if pre == nil || !pre.Enabled {
		return nil
	}
	_, err := c.CheckCAA(ctx, *pre, account, identifiers)
	return err
}
