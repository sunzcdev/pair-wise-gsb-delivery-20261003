package secure

import (
	"fmt"
	"net/http"
	"regexp"
	"sync/atomic"
)

// PolicySelector is a custom function type that returns the name of the
// policy that should be applied to the request. It is called exactly once
// per request; the host checks, HTTPS redirects and security headers for
// that request all use the single selected policy.
type PolicySelector func(r *http.Request) string

// defaultBadPolicyHandler fails closed when the selector returns an unknown
// policy name. It deliberately does not fall back to a permissive default.
func defaultBadPolicyHandler(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "Unknown Security Policy", http.StatusInternalServerError)
}

// policySet is an immutable, pre-compiled collection of named policies.
// Once published it is never modified, so requests can use it concurrently
// without locks.
type policySet struct {
	policies map[string]*Secure
}

// PolicyMux selects one of the named, pre-compiled security policies for
// each request, based on the name returned by its PolicySelector. It is
// safe for concurrent use, and the active policy set can be swapped
// atomically with Update.
type PolicyMux struct {
	selector PolicySelector

	// current holds the active *policySet. It is swapped atomically so
	// in-flight requests always see a complete, consistent version.
	current atomic.Value

	// badPolicyHandler is used when the selector returns an unknown name.
	badPolicyHandler http.Handler
}

// NewPolicyMux constructs a PolicyMux with the supplied selector and named
// policies. Every policy is validated (regular expressions and header
// values) and pre-compiled before the mux is returned; if any policy is
// invalid an error is returned and no mux is created.
func NewPolicyMux(selector PolicySelector, policies map[string]Options) (*PolicyMux, error) {
	if selector == nil {
		return nil, fmt.Errorf("secure: PolicySelector must not be nil")
	}

	set, err := compilePolicySet(policies)
	if err != nil {
		return nil, err
	}

	m := &PolicyMux{
		selector:         selector,
		badPolicyHandler: http.HandlerFunc(defaultBadPolicyHandler),
	}
	m.current.Store(set)

	return m, nil
}

// Update validates and compiles the supplied policies and, only if every
// policy is valid, atomically publishes them as the new active set. If any
// policy is invalid an error is returned and the previous set remains
// active. Requests that already started keep using the version they
// captured; new requests use the new version.
func (m *PolicyMux) Update(policies map[string]Options) error {
	set, err := compilePolicySet(policies)
	if err != nil {
		return err
	}

	m.current.Store(set)

	return nil
}

// SetBadPolicyHandler sets the handler to call when the selector returns an
// unknown policy name.
func (m *PolicyMux) SetBadPolicyHandler(handler http.Handler) {
	m.badPolicyHandler = handler
}

// Handler implements the http.HandlerFunc for integration with the standard net/http lib.
func (m *PolicyMux) Handler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy := m.selectPolicy(r)
		if policy == nil {
			m.badPolicyHandler.ServeHTTP(w, r)

			return
		}

		// Let secure process the request. If it returns an error,
		// that indicates the request should not continue.
		responseHeader, r, err := policy.processRequest(w, r)
		addResponseHeaders(responseHeader, w)

		// If there was an error, do not continue.
		if err != nil {
			return
		}

		h.ServeHTTP(w, r)
	})
}

// HandlerFuncWithNext is a special implementation for Negroni, but could be used elsewhere.
func (m *PolicyMux) HandlerFuncWithNext(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	policy := m.selectPolicy(r)
	if policy == nil {
		m.badPolicyHandler.ServeHTTP(w, r)

		return
	}

	// Let secure process the request. If it returns an error,
	// that indicates the request should not continue.
	responseHeader, r, err := policy.processRequest(w, r)
	addResponseHeaders(responseHeader, w)

	// If there was an error, do not call next.
	if err == nil && next != nil {
		next(w, r)
	}
}

// selectPolicy captures the active policy set exactly once and resolves the
// selector's name within that version. It returns nil for unknown names so
// the caller can fail closed.
func (m *PolicyMux) selectPolicy(r *http.Request) *Secure {
	set, _ := m.current.Load().(*policySet)
	if set == nil {
		return nil
	}

	policy, ok := set.policies[m.selector(r)]
	if !ok {
		return nil
	}

	return policy
}

// compilePolicySet validates and pre-compiles every named policy. The
// supplied options are deep-copied, so later mutation of the caller's maps
// or slices cannot alter the compiled set.
func compilePolicySet(policies map[string]Options) (*policySet, error) {
	set := &policySet{policies: make(map[string]*Secure, len(policies))}

	for name, opt := range policies {
		if err := validatePolicyOptions(name, opt); err != nil {
			return nil, err
		}

		set.policies[name] = New(copyOptions(opt))
	}

	return set, nil
}

// validatePolicyOptions checks that all regular expressions compile and all
// header values are legal before a policy set is published.
func validatePolicyOptions(name string, opt Options) error {
	if opt.AllowedHostsAreRegex {
		for _, allowedHost := range opt.AllowedHosts {
			if _, err := regexp.Compile(fmt.Sprintf("^%s$", allowedHost)); err != nil {
				return fmt.Errorf("secure: policy %q: error parsing AllowedHost: %w", name, err)
			}
		}
	}

	headerValues := map[string]string{
		cspHeader:                    opt.ContentSecurityPolicy,
		cspReportOnlyHeader:          opt.ContentSecurityPolicyReportOnly,
		xssProtectionHeader:          opt.CustomBrowserXssValue,
		frameOptionsHeader:           opt.CustomFrameOptionsValue,
		referrerPolicyHeader:         opt.ReferrerPolicy,
		featurePolicyHeader:          opt.FeaturePolicy,
		permissionsPolicyHeader:      opt.PermissionsPolicy,
		coopHeader:                   opt.CrossOriginOpenerPolicy,
		coepHeader:                   opt.CrossOriginEmbedderPolicy,
		corpHeader:                   opt.CrossOriginResourcePolicy,
		dnsPreFetchControlHeader:     opt.XDNSPrefetchControl,
		permittedCrossDomainPolicies: opt.XPermittedCrossDomainPolicies,
	}

	for header, value := range headerValues {
		if !validHeaderValue(value) {
			return fmt.Errorf("secure: policy %q: invalid %s header value", name, header)
		}
	}

	return nil
}

// validHeaderValue reports whether value can be used as an HTTP header
// value, rejecting control characters such as CR and LF.
func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}

	return true
}

// copyOptions returns a copy of opt that shares no mutable maps or slices
// with the original.
func copyOptions(opt Options) Options {
	copied := opt

	if opt.AllowedHosts != nil {
		copied.AllowedHosts = append([]string(nil), opt.AllowedHosts...)
	}

	if opt.HostsProxyHeaders != nil {
		copied.HostsProxyHeaders = append([]string(nil), opt.HostsProxyHeaders...)
	}

	if opt.SSLProxyHeaders != nil {
		copied.SSLProxyHeaders = make(map[string]string, len(opt.SSLProxyHeaders))
		for key, value := range opt.SSLProxyHeaders {
			copied.SSLProxyHeaders[key] = value
		}
	}

	return copied
}
