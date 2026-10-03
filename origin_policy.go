package nosurf

import (
	"errors"
	"net/http"
	"net/url"
)

// OriginPolicy determines how CSRFHandler evaluates the origin evidence
// (the Sec-Fetch-Site, Origin and Referer headers) of non-safe requests.
type OriginPolicy int

const (
	// OriginPolicyCompat is the default, backwards-compatible policy.
	// A `Sec-Fetch-Site: same-origin` header short-circuits the check.
	// Otherwise, the Origin header, and then the Referer header,
	// are compared against the self-origin.
	// Contradictions between the headers are not detected.
	OriginPolicyCompat OriginPolicy = iota

	// OriginPolicyStrict cross-checks all the origin evidence present
	// on the request and rejects requests whose headers contradict
	// each other. See strictDecisions for the exact decision table.
	OriginPolicyStrict
)

// Reasons for CSRF check failures specific to OriginPolicyStrict.
var (
	ErrAmbiguousOriginHeaders = errors.New("Request carries duplicate Sec-Fetch-Site, Origin or Referer headers")
	ErrNullOrigin             = errors.New("Request was made from an opaque origin (Origin: null)")
	ErrConflictingOrigin      = errors.New("Request carries contradictory origin evidence")
	ErrCrossSiteFetch         = errors.New("Sec-Fetch-Site indicates a cross-origin request," +
		" but no allowed Origin or Referer corroborates it")
	ErrUnknownSecFetchSite = errors.New("Sec-Fetch-Site header carries an unrecognized value")
)

// fetchSiteClass is the normalized classification
// of the Sec-Fetch-Site header of a request.
type fetchSiteClass int

const (
	fetchSiteAbsent fetchSiteClass = iota
	fetchSiteSameOrigin
	fetchSiteSameSite
	fetchSiteCrossSite
	fetchSiteNone
	fetchSiteUnknown
)

// originEvidence is the normalized verdict
// of a single origin hint (Origin or Referer header),
// after duplicate, null and malformed values have been rejected.
type originEvidence int

const (
	evidenceAbsent originEvidence = iota
	evidenceSameOrigin
	evidenceAllowed
	evidenceCross
)

// strictDecisions is the decision table applied by OriginPolicyStrict.
// It combines the Sec-Fetch-Site classification with the merged
// Origin/Referer evidence (see mergeEvidence). A nil reason allows
// the request to proceed to the (independent) CSRF token check,
// a non-nil reason rejects the request with that reason.
var strictDecisions = []struct {
	fetchSite fetchSiteClass
	evidence  originEvidence
	reason    error
}{
	// The browser asserts a same-origin request:
	// allow, unless the Origin/Referer evidence contradicts it.
	{fetchSiteSameOrigin, evidenceAbsent, nil},
	{fetchSiteSameOrigin, evidenceSameOrigin, nil},
	{fetchSiteSameOrigin, evidenceAllowed, ErrConflictingOrigin},

	// The browser asserts a same-site (but cross-origin) request:
	// only allow origins explicitly allowed via SetIsAllowedOriginFunc.
	{fetchSiteSameSite, evidenceAbsent, ErrCrossSiteFetch},
	{fetchSiteSameSite, evidenceSameOrigin, ErrConflictingOrigin},
	{fetchSiteSameSite, evidenceAllowed, nil},

	// The browser asserts a cross-site request:
	// only allow origins explicitly allowed via SetIsAllowedOriginFunc.
	{fetchSiteCrossSite, evidenceAbsent, ErrCrossSiteFetch},
	{fetchSiteCrossSite, evidenceSameOrigin, ErrConflictingOrigin},
	{fetchSiteCrossSite, evidenceAllowed, nil},

	// The browser asserts a user-initiated request (e.g. typed URL):
	// allow, unless the Origin/Referer evidence contradicts it.
	{fetchSiteNone, evidenceAbsent, nil},
	{fetchSiteNone, evidenceSameOrigin, nil},
	{fetchSiteNone, evidenceAllowed, ErrConflictingOrigin},

	// No Sec-Fetch-Site header (e.g. an outdated browser):
	// rely on the Origin/Referer evidence alone.
	{fetchSiteAbsent, evidenceAbsent, ErrNoReferer},
	{fetchSiteAbsent, evidenceSameOrigin, nil},
	{fetchSiteAbsent, evidenceAllowed, nil},

	// An unrecognized Sec-Fetch-Site value: fail closed.
	{fetchSiteUnknown, evidenceAbsent, ErrUnknownSecFetchSite},
	{fetchSiteUnknown, evidenceSameOrigin, ErrUnknownSecFetchSite},
	{fetchSiteUnknown, evidenceAllowed, ErrUnknownSecFetchSite},
}

// SetOriginPolicy sets the policy used to evaluate the origin evidence
// of non-safe requests. The default is OriginPolicyCompat,
// which preserves the historical behavior of nosurf.
// The CSRF token check is unaffected by this setting:
// it always runs as an independent step after the origin check.
func (h *CSRFHandler) SetOriginPolicy(policy OriginPolicy) {
	h.originPolicy = policy
}

// ensureSameOriginStrict implements OriginPolicyStrict:
// it normalizes all the origin evidence on the request,
// rejects ambiguous or contradictory evidence,
// and then applies the strictDecisions table.
func (h *CSRFHandler) ensureSameOriginStrict(selfOrigin *url.URL, r *http.Request) error {
	fetchSite, err := classifySecFetchSite(r)
	if err != nil {
		return err
	}

	origin, err := h.originEvidence(selfOrigin, r)
	if err != nil {
		return err
	}

	referer, err := h.refererEvidence(selfOrigin, r)
	if err != nil {
		return err
	}

	evidence, err := mergeEvidence(origin, referer)
	if err != nil {
		return err
	}

	for _, row := range strictDecisions {
		if row.fetchSite == fetchSite && row.evidence == evidence {
			return row.reason
		}
	}

	// Unreachable: the table covers every fetchSite/evidence combination.
	return ErrUnknownSecFetchSite
}

// classifySecFetchSite normalizes the Sec-Fetch-Site header of the request.
// Duplicate headers are rejected as ambiguous evidence.
func classifySecFetchSite(r *http.Request) (fetchSiteClass, error) {
	values := r.Header[http.CanonicalHeaderKey("Sec-Fetch-Site")]
	if len(values) > 1 {
		return fetchSiteAbsent, ErrAmbiguousOriginHeaders
	}
	if len(values) == 0 {
		return fetchSiteAbsent, nil
	}

	switch values[0] {
	case "":
		return fetchSiteAbsent, nil
	case "same-origin":
		return fetchSiteSameOrigin, nil
	case "same-site":
		return fetchSiteSameSite, nil
	case "cross-site":
		return fetchSiteCrossSite, nil
	case "none":
		return fetchSiteNone, nil
	default:
		return fetchSiteUnknown, nil
	}
}

// originEvidence evaluates the Origin header of the request.
// Duplicate headers, the opaque "null" origin and malformed values
// are rejected outright.
func (h *CSRFHandler) originEvidence(selfOrigin *url.URL, r *http.Request) (originEvidence, error) {
	values := r.Header[http.CanonicalHeaderKey("Origin")]
	if len(values) > 1 {
		return evidenceAbsent, ErrAmbiguousOriginHeaders
	}
	if len(values) == 0 || values[0] == "" {
		return evidenceAbsent, nil
	}
	if values[0] == "null" {
		return evidenceAbsent, ErrNullOrigin
	}

	origin, err := url.Parse(values[0])
	if err != nil {
		return evidenceAbsent, ErrBadOrigin
	}

	return h.classifyOrigin(selfOrigin, origin), nil
}

// refererEvidence evaluates the Referer header of the request.
func (h *CSRFHandler) refererEvidence(selfOrigin *url.URL, r *http.Request) (originEvidence, error) {
	values := r.Header[http.CanonicalHeaderKey("Referer")]
	if len(values) > 1 {
		return evidenceAbsent, ErrAmbiguousOriginHeaders
	}
	if len(values) == 0 || values[0] == "" {
		return evidenceAbsent, nil
	}

	referer, err := url.Parse(values[0])
	if err != nil {
		return evidenceAbsent, ErrNoReferer
	}

	return h.classifyOrigin(selfOrigin, referer), nil
}

func (h *CSRFHandler) classifyOrigin(selfOrigin, candidate *url.URL) originEvidence {
	if sameOrigin(selfOrigin, candidate) {
		return evidenceSameOrigin
	}
	if h.isAllowedOrigin != nil && h.isAllowedOrigin(candidate) {
		return evidenceAllowed
	}
	return evidenceCross
}

// mergeEvidence combines the Origin and Referer evidence into a single
// verdict, rejecting requests whose headers contradict each other.
// Origin takes precedence over Referer, mirroring the compat policy.
func mergeEvidence(origin, referer originEvidence) (originEvidence, error) {
	if origin == evidenceCross {
		return evidenceAbsent, ErrBadOrigin
	}
	if origin != evidenceAbsent {
		// The Origin checks out on its own,
		// but a cross-origin Referer contradicts it.
		if referer == evidenceCross {
			return evidenceAbsent, ErrConflictingOrigin
		}
		return origin, nil
	}
	if referer == evidenceCross {
		return evidenceAbsent, ErrBadReferer
	}
	return referer, nil
}
