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
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/mholt/acmez/v3/acme"
)

// CAA status values reported for an individual identifier.
const (
	// CAAStatusAllowed means the CAA records explicitly permit the
	// configured CA (with the given account and validation methods)
	// to issue for the identifier, or no CAA records are published
	// for it. An empty CAA policy is an authorization to issue per
	// RFC 8659 §4.
	CAAStatusAllowed = "allowed"

	// CAAStatusDenied means a published CAA policy forbids the
	// issuance, for example an "issue"/"issuewild" record naming a
	// different CA or an empty "issue;" record.
	CAAStatusDenied = "denied"

	// CAAStatusUndetermined means the checker could not reach a
	// definitive answer, for example because the DNS query failed,
	// the CAA policy uses parameters that cannot be evaluated with
	// the provided input, or the CA identity is unknown. An
	// undetermined result is never treated as an authorization to
	// issue; callers decide whether to block or proceed using
	// CAAPolicyOnUndetermined.
	CAAStatusUndetermined = "undetermined"
)

// Policies controlling what happens when a CAA check cannot reach a
// definitive answer.
const (
	// CAAPolicyBlockOnUndetermined aborts issuance when a CAA check
	// is inconclusive. This is the safe default: an undetermined
	// result is not taken as permission.
	CAAPolicyBlockOnUndetermined = "block"

	// CAAPolicyWarnOnUndetermined proceeds with issuance when a CAA
	// check is inconclusive, returning the undetermined result so the
	// caller can log it. The ACME server still performs its own,
	// authoritative CAA evaluation.
	CAAPolicyWarnOnUndetermined = "warn"
)

// CAAQueryer retrieves CAA resource records for a DNS name.
//
// Implementations return the CAA records published exactly at fqdn.
// A successful response with no CAA records MUST return an empty (or
// nil) slice and a nil error. Lookup failures (timeouts, DNS errors
// such as SERVFAIL, malformed responses) MUST return a non-nil error;
// the checker reports them as "undetermined" rather than assuming
// permission. NXDOMAIN is a successful empty response for the
// purposes of this interface.
//
// If fqdn is a DNS alias, CNAME points at the canonical name (the
// target of the last CNAME, if several are chained); otherwise it is
// equal to fqdn. The interface does not perform the RFC 8659 §4 tree
// climb itself; the checker does that.
type CAAQueryer interface {
	LookupCAA(ctx context.Context, fqdn string) (records []CAARecord, cname string, err error)
}

// CAARecord is a single CAA resource record (RFC 8659 §4.1) with the
// CAA parameters relevant to ACME issuance (RFC 8657) parsed out.
type CAARecord struct {
	// Tag is the property tag: "issue", "issuewild", "iodef", or an
	// extension tag. Tags are case-insensitive and are normalized to
	// lowercase.
	Tag string

	// Value is the raw property value (without surrounding quotes, if
	// any). For issue/issuewild records it holds the CA issuer domain
	// name, which may be empty for an explicit deny.
	Value string

	// Critical is the issuer critical flag (RFC 8659 §4.2). An unknown
	// tag marked critical forbids issuance.
	Critical bool

	// AccountURI is the value of the "accounturi" parameter of an
	// issue/issuewild record (RFC 8657 §4), empty when absent.
	AccountURI string

	// ValidationMethods are the entries of the space-separated
	// "validationmethods" parameter (RFC 8657 §3), empty when absent.
	// Method names are ACME challenge identifiers such as "http-01"
	// and "dns-01".
	ValidationMethods []string
}

// IsIssue reports whether the record is an "issue" or "issuewild"
// property.
func (r CAARecord) IsIssue() bool {
	return r.Tag == "issue" || r.Tag == "issuewild"
}

// CAAChecker performs an optional, advisory CAA pre-check before an
// ACME order is created. Its purpose is to surface obvious
// misconfiguration (a domain that does not list the target CA, an
// accounturi that does not match the current account, validation
// methods that cannot be used) before challenges are set up.
//
// The check is advisory only: CAA is evaluated authoritatively by the
// CA when it issues, and a "allowed" result from this checker does
// not bind the server.
type CAAChecker struct {
	// Queryer retrieves CAA records. If nil, a DNSCAAQueryer is used.
	// Tests and custom resolution strategies should supply their own
	// implementation.
	Queryer CAAQueryer

	// CAAIdentities lists the CAA issuer domain names that represent
	// the target CA, as a CA would publish them in the "caaIdentities"
	// field of its ACME directory (acme.DirectoryMeta.CAAIdentities).
	// If empty, ObtainCertificate populates it from the directory
	// before checking.
	CAAIdentities []string

	// OnUndetermined controls what happens when a check is
	// inconclusive. Use CAAPolicyBlockOnUndetermined (the default) to
	// stop, or CAAPolicyWarnOnUndetermined to log and continue.
	OnUndetermined string
}

// CAACheckRequest describes one identifier to check.
type CAACheckRequest struct {
	// Identifier is the ACME identifier. Only "dns" identifiers are
	// checked; other types are skipped with status "allowed".
	Identifier acme.Identifier

	// AccountURI is the current account's URL (acme.Account.Location).
	// It is matched against "accounturi" CAA parameters.
	AccountURI string

	// ValidationMethods are the ACME challenge types the client is
	// prepared to use for this identifier (e.g. "http-01", "dns-01"),
	// matched against "validationmethods" CAA parameters. Empty means
	// no method information is available.
	ValidationMethods []string
}

// CAAResult is the pre-check outcome for a single identifier.
type CAAResult struct {
	// Identifier is the identifier that was checked.
	Identifier acme.Identifier

	// Status is one of the CAAStatus* constants.
	Status string

	// Reason explains a "denied" or "undetermined" result.
	Reason string

	// Records are the CAA records the decision was based on, when
	// available.
	Records []CAARecord
}

// CAAError is returned by Check when at least one identifier was
// denied, or when a check was undetermined and the policy is
// CAAPolicyBlockOnUndetermined.
type CAAError struct {
	// Results holds the outcome for every requested identifier.
	Results []CAAResult
}

func (e *CAAError) Error() string {
	var denied, undetermined int
	for _, r := range e.Results {
		switch r.Status {
		case CAAStatusDenied:
			denied++
		case CAAStatusUndetermined:
			undetermined++
		}
	}
	return fmt.Sprintf("CAA pre-check failed: %d identifier(s) denied, %d undetermined", denied, undetermined)
}

// Check evaluates the CAA policy for all the requested identifiers and
// applies OnUndetermined. It returns nil when every identifier is
// allowed or (with CAAPolicyWarnOnUndetermined) merely undetermined;
// callers can inspect the returned results to log undetermined or
// allowed identifiers. A *CAAError is returned when issuance must stop.
func (c *CAAChecker) Check(ctx context.Context, reqs []CAACheckRequest) ([]CAAResult, error) {
	results := c.CheckResults(ctx, reqs)
	return results, c.applyPolicy(results)
}

// CheckResults evaluates the CAA policy for all the requested
// identifiers without applying OnUndetermined. No result is collapsed
// into an error; this is useful when the caller wants to apply its own
// policy to the undetermined results.
func (c *CAAChecker) CheckResults(ctx context.Context, reqs []CAACheckRequest) []CAAResult {
	queryer := c.Queryer
	if queryer == nil {
		queryer = &DNSCAAQueryer{}
	}

	identities := make(map[string]struct{}, len(c.CAAIdentities))
	for _, id := range c.CAAIdentities {
		id = normalizeName(id)
		if id != "" {
			identities[id] = struct{}{}
		}
	}

	results := make([]CAAResult, 0, len(reqs))
	for _, req := range reqs {
		results = append(results, c.checkIdentifier(ctx, queryer, identities, req))
	}
	return results
}

// ApplyPolicy returns a *CAAError if the results require blocking
// issuance according to OnUndetermined: a denied identifier always
// blocks, and an undetermined identifier blocks unless the policy is
// CAAPolicyWarnOnUndetermined.
func (c *CAAChecker) ApplyPolicy(results []CAAResult) error {
	return c.applyPolicy(results)
}

func (c *CAAChecker) applyPolicy(results []CAAResult) error {
	blocked := false
	for _, r := range results {
		if r.Status == CAAStatusDenied {
			blocked = true
		}
		if r.Status == CAAStatusUndetermined && c.OnUndetermined != CAAPolicyWarnOnUndetermined {
			blocked = true
		}
	}
	if blocked {
		return &CAAError{Results: results}
	}
	return nil
}

// caaLevel holds the CAA records found at one name during the
// RFC 8659 §4 tree climb.
type caaLevel struct {
	name    string
	records []CAARecord
}

func (c *CAAChecker) checkIdentifier(ctx context.Context, queryer CAAQueryer, identities map[string]struct{}, req CAACheckRequest) CAAResult {
	result := CAAResult{Identifier: req.Identifier}

	if req.Identifier.Type != "dns" {
		result.Status = CAAStatusAllowed
		result.Reason = "not a DNS identifier; CAA does not apply"
		return result
	}

	domain := normalizeName(req.Identifier.Value)
	wildcard := false
	if strings.HasPrefix(domain, "*.") {
		wildcard = true
		domain = normalizeName(strings.TrimPrefix(domain, "*."))
	}
	if domain == "" || strings.Contains(domain, "*") {
		result.Status = CAAStatusDenied
		result.Reason = "invalid DNS identifier"
		return result
	}

	levels, err := climbCAA(ctx, queryer, domain)
	if err != nil {
		return undetermined(result, err.Error())
	}

	// RFC 8659 §4: absent any CAA records, issuance is permitted.
	if len(levels) == 0 {
		result.Status = CAAStatusAllowed
		return result
	}

	records := selectIssuerRecords(levels, wildcard)
	result.Records = records

	// RFC 8659 §4.2: an unrecognized property with the critical flag
	// set in the relevant RRset forbids issuance.
	for _, level := range levelsForCritical(levels, wildcard) {
		for _, rec := range level.records {
			if rec.Critical && !knownCAATag(rec.Tag) {
				result.Status = CAAStatusDenied
				result.Reason = fmt.Sprintf("unknown critical CAA property %q at %s", rec.Tag, level.name)
				return result
			}
		}
	}

	if len(records) == 0 {
		result.Status = CAAStatusDenied
		result.Reason = "no applicable issue/issuewild property authorizes issuance"
		return result
	}

	if len(identities) == 0 {
		return undetermined(result, "target CA identity is unknown; cannot match issue/issuewild property")
	}

	return evaluateIssuerRecords(result, records, identities, req)
}

// climbCAA walks the labels from the fully-qualified name up to the
// TLD, querying CAA at each name. Per RFC 8659 §4 the climb starts at
// the requested name; if that name is a DNS alias it restarts at the
// CNAME target. Every name at which one or more CAA records exist is
// returned, most specific first. A lookup error aborts the walk.
func climbCAA(ctx context.Context, queryer CAAQueryer, domain string) ([]caaLevel, error) {
	visited := make(map[string]bool)
	current := domain
	const maxRestarts = 16

	for restart := 0; restart < maxRestarts; restart++ {
		var levels []caaLevel
		restarted := false
		for name := current; name != ""; name = parentDomain(name) {
			if visited[name] {
				return nil, fmt.Errorf("CNAME loop detected at %s", name)
			}
			visited[name] = true

			records, cname, err := queryer.LookupCAA(ctx, name)
			if err != nil {
				return nil, fmt.Errorf("looking up CAA for %s: %w", name, err)
			}
			if target := normalizeName(cname); target != "" && target != name {
				// Alias: RFC 8659 §4 restarts the algorithm from the
				// canonical name.
				current = target
				restarted = true
				break
			}
			if len(records) > 0 {
				levels = append(levels, caaLevel{name: name, records: records})
			}
		}
		if !restarted {
			return levels, nil
		}
	}
	return nil, fmt.Errorf("CNAME chain exceeded %d restarts", maxRestarts)
}

// selectIssuerRecords picks the issuer CAA properties that govern
// issuance per RFC 8659 §4:
//
// For a wildcard name, any "issuewild" property found during the climb
// (levels, most specific first) exclusively governs; the most specific
// issuewild RRset wins and "issue" properties are ignored. Without an
// issuewild property, the first (most specific) RRset's "issue"
// properties govern. For non-wildcard names, only the first RRset's
// "issue" properties are relevant.
func selectIssuerRecords(levels []caaLevel, wildcard bool) []CAARecord {
	if wildcard {
		for _, level := range levels {
			records := recordsWithTag(level.records, "issuewild")
			if len(records) > 0 {
				return records
			}
		}
	}
	if len(levels) == 0 {
		return nil
	}
	return recordsWithTag(levels[0].records, "issue")
}

// levelsForCritical returns the RRsets whose critical flags must be
// evaluated: the relevant RRset for plain names, and for wildcards
// every level between the requested name and the chosen issuewild
// RRset (since an issuewild RRset may sit at an ancestor of the first
// RRset that contains records).
func levelsForCritical(levels []caaLevel, wildcard bool) []caaLevel {
	if !wildcard {
		return levels[:1]
	}
	for i, level := range levels {
		if len(recordsWithTag(level.records, "issuewild")) > 0 {
			return levels[:i+1]
		}
	}
	return levels[:1]
}

func recordsWithTag(records []CAARecord, tag string) []CAARecord {
	var out []CAARecord
	for _, rec := range records {
		if rec.Tag == tag {
			out = append(out, rec)
		}
	}
	return out
}

func knownCAATag(tag string) bool {
	switch tag {
	case "issue", "issuewild", "iodef", "contactemail", "contactphone":
		return true
	}
	return false
}

// evaluateIssuerRecords applies the RFC 8657 accounturi and
// validationmethods parameters to the governing issuer properties.
//
// An empty issuer value (e.g. "issue ;") is an explicit deny. Any
// property naming the target CA may authorize: account-scoped
// properties apply only when accounturi matches, and a property
// carrying validationmethods authorizes a given challenge only when
// that challenge is listed. A property without validationmethods
// permits every challenge. Per the permissive reading of RFC 8657,
// the union of the applicable properties must cover every challenge
// the client intends to use.
func evaluateIssuerRecords(result CAAResult, records []CAARecord, identities map[string]struct{}, req CAACheckRequest) CAAResult {
	applicable := make([]CAARecord, 0, len(records))
	var sawAccountScoped bool

	// If the policy is account-scoped but the caller cannot tell us
	// which account is making the request, the result is inconclusive
	// rather than a denial.
	for _, rec := range records {
		if rec.AccountURI != "" && req.AccountURI == "" {
			return undetermined(result, "CAA uses accounturi but no account URL is available")
		}
	}

	for _, rec := range records {
		issuer, _ := parseCAAValue(rec.Value)
		if issuer == "" {
			result.Status = CAAStatusDenied
			result.Reason = "CAA explicitly forbids issuance (empty issuer value)"
			return result
		}
		if _, ok := identities[issuer]; !ok {
			continue
		}
		if rec.AccountURI != "" {
			sawAccountScoped = true
			if rec.AccountURI != req.AccountURI {
				continue
			}
		}
		applicable = append(applicable, rec)
	}

	if len(applicable) == 0 {
		result.Status = CAAStatusDenied
		if sawAccountScoped {
			result.Reason = fmt.Sprintf("CAA limits issuance to a different accounturi (current account %q)", req.AccountURI)
			return result
		}
		result.Reason = "CAA does not name the target CA"
		return result
	}

	for _, method := range req.ValidationMethods {
		if !methodPermitted(method, applicable) {
			result.Status = CAAStatusDenied
			result.Reason = fmt.Sprintf("CAA does not permit validation method %q", method)
			return result
		}
	}

	result.Status = CAAStatusAllowed
	return result
}

func methodPermitted(method string, records []CAARecord) bool {
	for _, rec := range records {
		if len(rec.ValidationMethods) == 0 {
			return true
		}
		for _, m := range rec.ValidationMethods {
			if m == method {
				return true
			}
		}
	}
	return false
}

// normalizeName lower-cases, strips a trailing dot and surrounding
// whitespace, and strips a leading wildcard label.
func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ToLower(name)
	name = strings.TrimSuffix(name, ".")
	return name
}

// parentDomain returns the parent of name, or "" when name is a TLD
// (the root label itself is never queried).
func parentDomain(name string) string {
	name = normalizeName(name)
	idx := strings.IndexByte(name, '.')
	if idx < 0 {
		return ""
	}
	return name[idx+1:]
}

// splitFields splits a CAA property value on whitespace, honoring
// double-quoted parameter values (RFC 8659 §4.1).
func splitFields(value string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			fields = append(fields, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
		case (ch == ' ' || ch == '\t') && !inQuote:
			flush()
		case ch == ';' && !inQuote:
			// semicolons are tolerated as informal separators
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return fields
}

// parseCAAValue parses an issue/issuewild property value into the
// issuer domain name and its RFC 8657 parameters.
func parseCAAValue(value string) (issuer string, params map[string]string) {
	fields := splitFields(value)
	if len(fields) == 0 {
		return "", nil
	}
	issuer = normalizeName(fields[0])
	params = make(map[string]string)
	for _, field := range fields[1:] {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		params[strings.ToLower(key)] = strings.Trim(val, "\"")
	}
	return issuer, params
}

// parseCAARecord turns a raw tag/value pair into a CAARecord, parsing
// the accounturi and validationmethods parameters of issuer records.
func parseCAARecord(tag string, value string, critical bool) CAARecord {
	rec := CAARecord{Tag: strings.ToLower(strings.TrimSpace(tag)), Critical: critical}
	raw := strings.TrimSpace(value)
	raw = strings.TrimPrefix(raw, "\"")
	raw = strings.TrimSuffix(raw, "\"")
	rec.Value = raw

	if rec.IsIssue() {
		_, params := parseCAAValue(raw)
		rec.AccountURI = params["accounturi"]
		if methods, ok := params["validationmethods"]; ok {
			for _, m := range strings.FieldsFunc(methods, func(r rune) bool {
				return r == ' ' || r == ','
			}) {
				rec.ValidationMethods = append(rec.ValidationMethods, m)
			}
		}
	}
	return rec
}

func undetermined(result CAAResult, reason string) CAAResult {
	result.Status = CAAStatusUndetermined
	result.Reason = reason
	return result
}

// preCheckCAA runs the optional advisory CAA pre-check for the DNS
// identifiers of an order. The target CA identity is taken from the
// checker configuration or, failing that, from the server directory's
// caaIdentities metadata. Inconclusive checks never default to
// "allowed": they are either blocked or, under
// CAAPolicyWarnOnUndetermined, logged and left for the CA to decide.
func (c *Client) preCheckCAA(ctx context.Context, account acme.Account, identifiers []acme.Identifier) error {
	checker := c.CAAChecker
	if checker == nil {
		return nil
	}

	dnsIdentifiers := make([]acme.Identifier, 0, len(identifiers))
	for _, id := range identifiers {
		if id.Type == "dns" {
			dnsIdentifiers = append(dnsIdentifiers, id)
		}
	}
	if len(dnsIdentifiers) == 0 {
		return nil
	}

	if len(checker.CAAIdentities) == 0 {
		dir, err := c.Client.GetDirectory(ctx)
		if err != nil {
			return checker.applyPolicy([]CAAResult{{
				Identifier: dnsIdentifiers[0],
				Status:     CAAStatusUndetermined,
				Reason:     "unable to retrieve ACME directory for caaIdentities: " + err.Error(),
			}})
		}
		if dir.Meta != nil {
			checker.CAAIdentities = dir.Meta.CAAIdentities
		}
	}

	methods := c.caaValidationMethods()
	reqs := make([]CAACheckRequest, 0, len(dnsIdentifiers))
	for _, id := range dnsIdentifiers {
		reqs = append(reqs, CAACheckRequest{
			Identifier:        id,
			AccountURI:        account.Location,
			ValidationMethods: methods,
		})
	}

	results := checker.CheckResults(ctx, reqs)
	for _, result := range results {
		if c.Logger == nil {
			continue
		}
		switch result.Status {
		case CAAStatusUndetermined:
			c.Logger.Warn("CAA pre-check undetermined; CA will make the authoritative decision",
				slog.String("identifier", result.Identifier.Value),
				slog.String("reason", result.Reason))
		case CAAStatusDenied:
			c.Logger.Error("CAA pre-check denied",
				slog.String("identifier", result.Identifier.Value),
				slog.String("reason", result.Reason))
		}
	}
	return checker.applyPolicy(results)
}

// caaValidationMethods returns the challenge types configured with a
// solver that are also meaningful in a CAA validationmethods
// parameter (RFC 8657).
func (c *Client) caaValidationMethods() []string {
	supported := map[string]bool{
		acme.ChallengeTypeHTTP01:    true,
		acme.ChallengeTypeDNS01:     true,
		acme.ChallengeTypeTLSALPN01: true,
	}
	var methods []string
	for challengeType, solver := range c.ChallengeSolvers {
		if solver != nil && supported[challengeType] {
			methods = append(methods, challengeType)
		}
	}
	sort.Strings(methods)
	return methods
}
