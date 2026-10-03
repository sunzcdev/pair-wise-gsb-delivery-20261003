package nosurf

import (
	"errors"
	"net/http"
	"net/url"
)

// OriginPolicy selects how strictly nosurf validates the origin of
// non-safe requests, by combining the evidence found in the
// Sec-Fetch-Site, Origin and Referer headers.
//
// The CSRF token is always validated independently of this policy:
// a request that passes the origin checks can still be rejected
// because of a missing or incorrect token, and vice versa.
type OriginPolicy int

const (
	// OriginPolicyCompat (the default) preserves the historical behavior:
	//
	//   - Sec-Fetch-Site: same-origin is always trusted on its own;
	//   - otherwise the Origin header is checked, and, when it is absent,
	//     the Referer header is used as a fallback;
	//   - contradictory evidence (e.g. a same-origin Sec-Fetch-Site value
	//     together with a cross-origin Origin) is not rejected.
	OriginPolicyCompat OriginPolicy = iota

	// OriginPolicyStrict enforces a stricter, table-driven combination of
	// the Sec-Fetch-Site, Origin and Referer headers.
	//
	// Every request is classified into a row of the strictOriginRules
	// decision table; requests whose evidence contradicts itself
	// (e.g. Sec-Fetch-Site: same-origin together with a cross-origin
	// Origin, or repeated header values) are rejected.
	OriginPolicyStrict
)

// Additional reasons for origin-check failures, reported in strict mode.
var (
	// ErrDuplicateOrigin is reported when the request contains more than
	// one Origin header, or a single Origin header with several values.
	ErrDuplicateOrigin = errors.New("Request contained multiple Origin header values")
	// ErrDuplicateReferer is reported when the request contains more than
	// one Referer header.
	ErrDuplicateReferer = errors.New("Request contained multiple Referer header values")
	// ErrDuplicateFetchSite is reported when the request contains more
	// than one Sec-Fetch-Site header value.
	ErrDuplicateFetchSite = errors.New("Request contained multiple Sec-Fetch-Site header values")
	// ErrNullOrigin is reported when the Origin header has the value
	// "null" (e.g. a request issued from a sandboxed document).
	ErrNullOrigin = errors.New("Request was made with an opaque (\"null\") origin")
	// ErrMalformedOrigin is reported when the Origin header value
	// can not be parsed.
	ErrMalformedOrigin = errors.New("Request contained a malformed Origin header")
	// ErrMalformedReferer is reported when the Referer header value
	// can not be parsed.
	ErrMalformedReferer = errors.New("Request contained a malformed Referer header")
	// ErrContradictoryOrigin is reported when the available origin
	// evidence contradicts itself, e.g. Sec-Fetch-Site: cross-site
	// together with an Origin equal to the website's own origin.
	ErrContradictoryOrigin = errors.New("Request contained contradictory origin evidence")
	// ErrCrossSite is reported when Sec-Fetch-Site explicitly classifies
	// the request as cross-site (or as "none", e.g. a request triggered
	// by an opaque origin) and no explicitly allowed origin matches.
	ErrCrossSite = errors.New("Request was explicitly made from a cross-site context")
	// ErrUnknownFetchSite is reported when the Sec-Fetch-Site header
	// contains a value that is not one of the standardized values
	// (same-origin, same-site, cross-site, none).
	ErrUnknownFetchSite = errors.New("Request contained an unknown Sec-Fetch-Site value")
)

// fetchSiteClass is the normalized value of the Sec-Fetch-Site header.
type fetchSiteClass int

const (
	fsSameOrigin fetchSiteClass = iota
	fsSameSite
	fsCrossSite
	fsNone
	fsMissing
	fsUnknown
	fsDuplicate
)

// evidenceClass describes what a single Origin or Referer header proves.
type evidenceClass int

const (
	// evMissing: the header is absent.
	evMissing evidenceClass = iota
	// evDuplicate: several values were supplied for the header.
	evDuplicate
	// evNull: the Origin header has the value "null".
	evNull
	// evMalformed: the header value can not be parsed.
	evMalformed
	// evSelf: the header origin equals the website's own origin.
	evSelf
	// evAllowed: the header origin is accepted by isAllowedOrigin.
	evAllowed
	// evForeign: the header origin is neither self nor explicitly allowed.
	evForeign
)

// anyClass is a wildcard matching every evidenceClass or fetchSiteClass.
const anyClass = -1

// originDecision is a single row of the strict-mode decision table:
// for a given Sec-Fetch-Site class and Origin/Referer evidence classes,
// allow the request or reject it with the given reason.
type originDecision struct {
	fetchSite fetchSiteClass
	origin    evidenceClass
	referer   evidenceClass
	reason    error // nil means the request is allowed
}

// strictOriginRules is the readable decision table used by
// OriginPolicyStrict. The first matching row wins; each block ends with
// a catch-all row so that every combination is accounted for.
var strictOriginRules = []originDecision{
	// Sec-Fetch-Site: same-origin. The browser vouches for the request,
	// but the header is cross-checked against Origin/Referer when present.
	{fsSameOrigin, evDuplicate, anyClass, ErrDuplicateOrigin},
	{fsSameOrigin, evNull, anyClass, ErrNullOrigin},
	{fsSameOrigin, evMalformed, anyClass, ErrMalformedOrigin},
	{fsSameOrigin, anyClass, evDuplicate, ErrDuplicateReferer},
	{fsSameOrigin, anyClass, evMalformed, ErrMalformedReferer},
	// Only our own origin is consistent with a same-origin claim;
	// foreign or merely allow-listed origins contradict it.
	{fsSameOrigin, evForeign, anyClass, ErrContradictoryOrigin},
	{fsSameOrigin, evAllowed, anyClass, ErrContradictoryOrigin},
	{fsSameOrigin, anyClass, evForeign, ErrContradictoryOrigin},
	{fsSameOrigin, anyClass, evAllowed, ErrContradictoryOrigin},
	// Self evidence is consistent, provided the other header does not
	// contradict it (contradiction rows above take precedence).
	{fsSameOrigin, anyClass, evSelf, nil},
	{fsSameOrigin, evSelf, anyClass, nil},
	// No contradicting evidence: trust the Sec-Fetch-Site header.
	{fsSameOrigin, evMissing, evMissing, nil},
	{fsSameOrigin, anyClass, anyClass, ErrContradictoryOrigin},

	// Sec-Fetch-Site: same-site. A sibling subdomain is plausible, but it
	// has to be explicitly allow-listed; the self origin always passes.
	{fsSameSite, evDuplicate, anyClass, ErrDuplicateOrigin},
	{fsSameSite, evNull, anyClass, ErrNullOrigin},
	{fsSameSite, evMalformed, anyClass, ErrMalformedOrigin},
	{fsSameSite, anyClass, evDuplicate, ErrDuplicateReferer},
	{fsSameSite, anyClass, evMalformed, ErrMalformedReferer},
	// One trusted header combined with a foreign counterpart is a conflict.
	{fsSameSite, evForeign, evSelf, ErrBadOrigin},
	{fsSameSite, evForeign, evAllowed, ErrBadOrigin},
	{fsSameSite, evSelf, evForeign, ErrBadReferer},
	{fsSameSite, evAllowed, evForeign, ErrBadReferer},
	{fsSameSite, evSelf, anyClass, nil},
	{fsSameSite, evAllowed, anyClass, nil},
	{fsSameSite, anyClass, evSelf, nil},
	{fsSameSite, anyClass, evAllowed, nil},
	{fsSameSite, evForeign, anyClass, ErrBadOrigin},
	{fsSameSite, anyClass, evForeign, ErrBadReferer},
	{fsSameSite, evMissing, evMissing, ErrNoReferer},
	{fsSameSite, anyClass, anyClass, ErrBadOrigin},

	// Sec-Fetch-Site: cross-site. Only explicitly allow-listed origins
	// pass; an Origin matching our own origin is a contradiction.
	{fsCrossSite, evDuplicate, anyClass, ErrDuplicateOrigin},
	{fsCrossSite, evNull, anyClass, ErrNullOrigin},
	{fsCrossSite, evMalformed, anyClass, ErrMalformedOrigin},
	{fsCrossSite, anyClass, evDuplicate, ErrDuplicateReferer},
	{fsCrossSite, anyClass, evMalformed, ErrMalformedReferer},
	{fsCrossSite, evSelf, anyClass, ErrContradictoryOrigin},
	{fsCrossSite, anyClass, evSelf, ErrContradictoryOrigin},
	{fsCrossSite, evAllowed, evSelf, ErrContradictoryOrigin},
	{fsCrossSite, evSelf, evAllowed, ErrContradictoryOrigin},
	{fsCrossSite, evAllowed, anyClass, nil},
	{fsCrossSite, anyClass, evAllowed, nil},
	{fsCrossSite, evForeign, anyClass, ErrBadOrigin},
	{fsCrossSite, anyClass, evForeign, ErrBadReferer},
	{fsCrossSite, evMissing, evMissing, ErrCrossSite},
	{fsCrossSite, anyClass, anyClass, ErrCrossSite},

	// Sec-Fetch-Site: none (user-initiated navigation, an opaque origin,
	// etc.). No Origin value is trustworthy in this context.
	{fsNone, evDuplicate, anyClass, ErrDuplicateOrigin},
	{fsNone, evNull, anyClass, ErrNullOrigin},
	{fsNone, evMalformed, anyClass, ErrMalformedOrigin},
	{fsNone, anyClass, evDuplicate, ErrDuplicateReferer},
	{fsNone, anyClass, evMalformed, ErrMalformedReferer},
	{fsNone, anyClass, anyClass, ErrCrossSite},

	// Sec-Fetch-Site is absent. Fall back to Origin, then Referer.
	{fsMissing, evDuplicate, anyClass, ErrDuplicateOrigin},
	{fsMissing, evNull, anyClass, ErrNullOrigin},
	{fsMissing, evMalformed, anyClass, ErrMalformedOrigin},
	{fsMissing, anyClass, evDuplicate, ErrDuplicateReferer},
	{fsMissing, anyClass, evMalformed, ErrMalformedReferer},
	{fsMissing, evForeign, evSelf, ErrBadOrigin},
	{fsMissing, evForeign, evAllowed, ErrBadOrigin},
	{fsMissing, evSelf, evForeign, ErrBadReferer},
	{fsMissing, evAllowed, evForeign, ErrBadReferer},
	{fsMissing, evSelf, anyClass, nil},
	{fsMissing, evAllowed, anyClass, nil},
	{fsMissing, evForeign, anyClass, ErrBadOrigin},
	{fsMissing, anyClass, evSelf, nil},
	{fsMissing, anyClass, evAllowed, nil},
	{fsMissing, anyClass, evForeign, ErrBadReferer},
	{fsMissing, evMissing, evMissing, ErrNoReferer},
	{fsMissing, anyClass, anyClass, ErrNoReferer},

	// An unknown or repeated Sec-Fetch-Site value is never trusted.
	{fsUnknown, anyClass, anyClass, ErrUnknownFetchSite},
	{fsDuplicate, anyClass, anyClass, ErrDuplicateFetchSite},
}

// classifyFetchSite normalizes the Sec-Fetch-Site header.
func classifyFetchSite(r *http.Request) fetchSiteClass {
	values := r.Header[http.CanonicalHeaderKey("Sec-Fetch-Site")]
	switch {
	case len(values) == 0:
		return fsMissing
	case len(values) > 1 || countHeaderValues(values[0]) > 1:
		return fsDuplicate
	}
	switch values[0] {
	case "same-origin":
		return fsSameOrigin
	case "same-site":
		return fsSameSite
	case "cross-site":
		return fsCrossSite
	case "none":
		return fsNone
	default:
		return fsUnknown
	}
}

// classifyOrigin normalizes the Origin header and returns its class
// together with the parsed origin (nil when the header is absent or
// structurally invalid).
func (h *CSRFHandler) classifyOrigin(selfOrigin *url.URL, r *http.Request) (evidenceClass, *url.URL) {
	values := r.Header[http.CanonicalHeaderKey("Origin")]
	switch {
	case len(values) == 0:
		return evMissing, nil
	case len(values) > 1 || countHeaderValues(values[0]) > 1:
		return evDuplicate, nil
	}
	value := values[0]
	if value == "null" {
		return evNull, nil
	}
	origin, err := url.Parse(value)
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return evMalformed, nil
	}
	switch {
	case sameOrigin(selfOrigin, origin):
		return evSelf, origin
	case h.isAllowedOrigin != nil && h.isAllowedOrigin(origin):
		return evAllowed, origin
	default:
		return evForeign, origin
	}
}

// classifyReferer normalizes the Referer header.
func (h *CSRFHandler) classifyReferer(selfOrigin *url.URL, r *http.Request) evidenceClass {
	values := r.Header[http.CanonicalHeaderKey("Referer")]
	switch {
	case len(values) == 0:
		return evMissing
	case len(values) > 1:
		return evDuplicate
	}
	referer, err := url.Parse(values[0])
	if err != nil || referer.String() == "" || referer.Scheme == "" || referer.Host == "" {
		return evMalformed
	}
	switch {
	case sameOrigin(selfOrigin, referer):
		return evSelf
	case h.isAllowedOrigin != nil && h.isAllowedOrigin(referer):
		return evAllowed
	default:
		return evForeign
	}
}

// countHeaderValues returns the number of comma-separated, non-empty
// values in a single header line. Sec-Fetch-Site and Origin are
// single-value headers.
func countHeaderValues(line string) int {
	count := 0
	for _, part := range splitHeader(line) {
		if part != "" {
			count++
		}
	}
	return count
}

func splitHeader(line string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] == ',' {
			parts = append(parts, trimSpace(line[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, trimSpace(line[start:]))
	return parts
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && isSpace(s[start]) {
		start++
	}
	end := len(s)
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t'
}

// evaluateStrictOrigin applies the strict decision table to the request.
func (h *CSRFHandler) evaluateStrictOrigin(selfOrigin *url.URL, r *http.Request) error {
	fetchSite := classifyFetchSite(r)
	originClass, _ := h.classifyOrigin(selfOrigin, r)
	refererClass := h.classifyReferer(selfOrigin, r)
	err := h.lookupStrictOrigin(fetchSite, originClass, refererClass)
	if errors.Is(err, errTableMiss) {
		// Fail closed if a table row is ever missing.
		return ErrBadOrigin
	}
	return err
}

// errTableMiss is the internal sentinel returned by lookupStrictOrigin
// when no table row covers a classification combination.
var errTableMiss = errors.New("no origin decision-table row")

// lookupStrictOrigin finds the first decision-table row matching the
// given classifications. It returns errTableMiss if none matches.
func (h *CSRFHandler) lookupStrictOrigin(fetchSite fetchSiteClass, origin, referer evidenceClass) error {
	for _, rule := range strictOriginRules {
		if rule.matches(fetchSite, origin, referer) {
			return rule.reason
		}
	}
	// The table is exhaustive; fail closed if a row is ever missing.
	return errTableMiss
}

func (d originDecision) matches(fetchSite fetchSiteClass, origin, referer evidenceClass) bool {
	return (d.fetchSite == anyClass || d.fetchSite == fetchSite) &&
		(d.origin == anyClass || d.origin == origin) &&
		(d.referer == anyClass || d.referer == referer)
}
