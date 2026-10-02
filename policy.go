package secure

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
)

// PolicySelector selects the name of the security policy that must be used
// for a request. It is invoked exactly once per request: host checks, HTTPS
// redirects and security headers all use the policy snapshot resolved from
// the returned name.
type PolicySelector func(r *http.Request) string

// ErrPolicyNotFound is returned (and the request is rejected through the bad
// request handler) when a PolicySelector returns a name that the active
// PolicySet does not contain. Unknown names never fall back to a permissive
// policy.
var ErrPolicyNotFound = errors.New("secure: named security policy not found")

// compiledPolicy is an immutable, validated policy. Regexps are compiled once
// and slices/maps are private copies, so publishing or using a policy never
// touches caller-owned data.
type compiledPolicy struct {
	opt Options

	nonceEnabled bool

	// cRegexAllowedHosts holds the compiled regular expressions of the
	// AllowedHosts option, ready for use during request processing.
	cRegexAllowedHosts []*regexp.Regexp
}

// policySnapshot is an immutable version of a PolicySet. A request pins one
// snapshot when its policy name is selected, giving the whole request a single
// consistent policy even while the set is republished concurrently.
type policySnapshot struct {
	policies map[string]*compiledPolicy
	version  uint64
}

// PolicySet is a published collection of pre-compiled, named security
// policies.
//
// A PolicySet is safe for concurrent use: lookups are lock-free and a new
// collection is only published after every policy has been compiled and
// validated. An invalid update is rejected and the previously published
// version keeps serving requests. The maps and slices passed to NewPolicySet
// and Replace are copied, so later mutation by the caller cannot affect the
// policies in use.
type PolicySet struct {
	active atomic.Value // *policySnapshot
}

// NewPolicySet compiles and publishes the first version of a PolicySet. Every
// policy is validated (all regexps compile and every header value is legal)
// before the set is published; no PolicySet is returned when validation fails.
func NewPolicySet(named map[string]Options) (*PolicySet, error) {
	snapshot, err := compileSnapshot(named)
	if err != nil {
		return nil, err
	}

	snapshot.version = 1

	set := &PolicySet{}
	set.active.Store(snapshot)

	return set, nil
}

// Replace validates and atomically publishes a new collection of policies. If
// any policy is invalid, the update is rejected and the currently published
// version is preserved. The provided map and the slices/maps inside it are
// copied, so mutating them after Replace returns cannot change the published
// set.
func (p *PolicySet) Replace(named map[string]Options) error {
	snapshot, err := compileSnapshot(named)
	if err != nil {
		return err
	}

	if previous, ok := p.active.Load().(*policySnapshot); ok && previous != nil {
		snapshot.version = previous.version + 1
	} else {
		snapshot.version = 1
	}

	p.active.Store(snapshot)

	return nil
}

// Version returns the monotonically increasing version number of the
// currently published collection, or 0 when no collection has been
// published.
func (p *PolicySet) Version() uint64 {
	if snapshot, ok := p.active.Load().(*policySnapshot); ok && snapshot != nil {
		return snapshot.version
	}

	return 0
}

// lookup pins the active snapshot and resolves the named policy from it.
func (p *PolicySet) lookup(name string) (*compiledPolicy, bool) {
	if p == nil {
		return nil, false
	}

	snapshot, ok := p.active.Load().(*policySnapshot)
	if !ok || snapshot == nil {
		return nil, false
	}

	policy, ok := snapshot.policies[name]

	return policy, ok
}

// compileSnapshot deep-copies, validates and compiles every named policy
// without publishing anything.
func compileSnapshot(named map[string]Options) (*policySnapshot, error) {
	if len(named) == 0 {
		return nil, errors.New("secure: policy set must contain at least one named policy")
	}

	policies := make(map[string]*compiledPolicy, len(named))

	for name, options := range named {
		if name == "" {
			return nil, errors.New("secure: policy set contains a policy with an empty name")
		}

		policy, err := compilePolicy(options, true)
		if err != nil {
			return nil, fmt.Errorf("secure: policy %q is invalid: %w", name, err)
		}

		policies[name] = policy
	}

	return &policySnapshot{policies: policies}, nil
}

// compilePolicy produces an immutable compiled policy from a private copy of
// the supplied options. When strict is true, all regexps and header values are
// validated and compilation errors are returned instead of panicking.
func compilePolicy(options Options, strict bool) (*compiledPolicy, error) {
	options = cloneOptions(options)

	if strict {
		if err := validatePolicy(options); err != nil {
			return nil, err
		}
	}

	options.ContentSecurityPolicy = strings.ReplaceAll(options.ContentSecurityPolicy, "$NONCE", "'nonce-%[1]s'")
	options.ContentSecurityPolicyReportOnly = strings.ReplaceAll(options.ContentSecurityPolicyReportOnly, "$NONCE", "'nonce-%[1]s'")

	policy := &compiledPolicy{opt: options}
	policy.nonceEnabled = strings.Contains(options.ContentSecurityPolicy, "%[1]s") ||
		strings.Contains(options.ContentSecurityPolicyReportOnly, "%[1]s")

	if err := validateCSPTemplate(options.ContentSecurityPolicy); err != nil {
		return nil, err
	}

	if err := validateCSPTemplate(options.ContentSecurityPolicyReportOnly); err != nil {
		return nil, err
	}

	if options.AllowedHostsAreRegex {
		for _, allowedHost := range options.AllowedHosts {
			regex, err := regexp.Compile(fmt.Sprintf("^%s$", allowedHost))
			if err != nil {
				return nil, fmt.Errorf("error parsing AllowedHost %q: %w", allowedHost, err)
			}

			policy.cRegexAllowedHosts = append(policy.cRegexAllowedHosts, regex)
		}
	}

	return policy, nil
}

// cloneOptions returns a deep copy of every slice and map held by options, so
// a compiled policy can never alias caller-owned data.
func cloneOptions(options Options) Options {
	if options.AllowedHosts != nil {
		options.AllowedHosts = append([]string(nil), options.AllowedHosts...)
	}

	if options.HostsProxyHeaders != nil {
		options.HostsProxyHeaders = append([]string(nil), options.HostsProxyHeaders...)
	}

	if options.SSLProxyHeaders != nil {
		cloned := make(map[string]string, len(options.SSLProxyHeaders))
		for key, value := range options.SSLProxyHeaders {
			cloned[key] = value
		}

		options.SSLProxyHeaders = cloned
	}

	return options
}

// validatePolicy verifies every regexp-adjacent and header-bound value before a
// named policy is published.
func validatePolicy(options Options) error {
	if options.STSSeconds < 0 {
		return fmt.Errorf("STSSeconds must not be negative, got %d", options.STSSeconds)
	}

	if options.XDNSPrefetchControl != "" {
		switch strings.ToLower(options.XDNSPrefetchControl) {
		case "on", "off":
		default:
			return fmt.Errorf("XDNSPrefetchControl must be \"on\" or \"off\", got %q", options.XDNSPrefetchControl)
		}
	}

	headerChecks := []struct {
		name  string
		value string
	}{
		{frameOptionsHeader, options.CustomFrameOptionsValue},
		{xssProtectionHeader, options.CustomBrowserXssValue},
		{cspHeader, options.ContentSecurityPolicy},
		{cspReportOnlyHeader, options.ContentSecurityPolicyReportOnly},
		{referrerPolicyHeader, options.ReferrerPolicy},
		{featurePolicyHeader, options.FeaturePolicy},
		{permissionsPolicyHeader, options.PermissionsPolicy},
		{coopHeader, options.CrossOriginOpenerPolicy},
		{corpHeader, options.CrossOriginResourcePolicy},
		{coepHeader, options.CrossOriginEmbedderPolicy},
		{permittedCrossDomainPolicies, options.XPermittedCrossDomainPolicies},
		{"SSLHost", options.SSLHost},
	}

	for _, check := range headerChecks {
		if check.value != "" && !validHeaderFieldValue(check.value) {
			return fmt.Errorf("%s contains illegal header value characters", check.name)
		}
	}

	for index, header := range options.HostsProxyHeaders {
		if !validHeaderFieldName(header) {
			return fmt.Errorf("HostsProxyHeaders[%d] %q is not a valid header name", index, header)
		}
	}

	for header, value := range options.SSLProxyHeaders {
		if !validHeaderFieldName(header) {
			return fmt.Errorf("SSLProxyHeaders contains invalid header name %q", header)
		}

		if !validHeaderFieldValue(value) {
			return fmt.Errorf("SSLProxyHeaders[%q] contains illegal header value characters", header)
		}
	}

	// Literal allowed hosts are compared against request hosts, which must be
	// valid hosts themselves; reject control characters (CR/LF in particular)
	// so a crafted entry can never smuggle output.
	if !options.AllowedHostsAreRegex {
		for _, allowedHost := range options.AllowedHosts {
			if !validHeaderFieldValue(allowedHost) {
				return fmt.Errorf("AllowedHost %q contains illegal host characters", allowedHost)
			}
		}
	}

	return nil
}

// validHeaderFieldName reports whether value is an RFC 7230 token, i.e. a legal
// HTTP header field name.
func validHeaderFieldName(value string) bool {
	if value == "" {
		return false
	}

	for i := 0; i < len(value); i++ {
		if !isTokenByte(value[i]) {
			return false
		}
	}

	return true
}

// validHeaderFieldValue reports whether value is a legal RFC 7230 header field
// value: tabs, spaces, visible ASCII and obs-text bytes are allowed, but
// carriage returns, line feeds and other control characters are rejected.
func validHeaderFieldValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]

		switch {
		case c == '\t' || c == ' ':
			continue
		case c >= 0x21 && c <= 0x7e:
			continue
		case c >= 0x80:
			continue
		default:
			return false
		}
	}

	return true
}

// isTokenByte reports whether c is an RFC 7230 tchar.
func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	}

	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}

	return false
}

// validateCSPTemplate ensures a nonce-enabled Content-Security-Policy value
// only uses the supported %[1]s conversion (or an escaped %%); a stray verb
// would corrupt the generated header.
func validateCSPTemplate(value string) error {
	for i := 0; i < len(value); i++ {
		if value[i] != '%' {
			continue
		}

		if strings.HasPrefix(value[i:], "%[1]s") {
			i += len("%[1]s") - 1

			continue
		}

		if strings.HasPrefix(value[i:], "%%") {
			i++

			continue
		}

		return errors.New("Content-Security-Policy contains an unsupported format verb; use $NONCE")
	}

	return nil
}
