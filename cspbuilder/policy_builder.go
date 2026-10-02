package cspbuilder

import (
	"fmt"
	"strings"
	"unicode"
)

// Nonce is a source expression placeholder for the per-request CSP nonce.
//
// When the built policy is passed to the secure middleware
// (via secure.Options.ContentSecurityPolicy or secure.Options.ContentSecurityPolicyReportOnly),
// each occurrence is expanded to the nonce generated for that request,
// e.g. 'nonce-a2ZobGFoZg=='. The same nonce is shared by the enforce and
// report-only policies of a request, and it is only expanded in the
// directive slots where it was explicitly declared.
const Nonce = "$NONCE"

// PolicyBuilder builds a Content Security Policy by appending source
// expressions to directives. Unlike Builder, declarations are validated
// when the policy is built: duplicate directives, illegal separator
// characters, and mutually exclusive values (such as 'none' combined with
// other source expressions) are reported as errors. Duplicate source
// expressions within a directive are removed, and directives are emitted
// in a stable (sorted) order.
//
// PolicyBuilder does not attempt to parse or repair existing policy
// strings; use secure.Options.ContentSecurityPolicy directly for those.
type PolicyBuilder struct {
	names   []string
	sources map[string][]string
}

// NewPolicyBuilder returns an empty PolicyBuilder.
func NewPolicyBuilder() *PolicyBuilder {
	return &PolicyBuilder{
		sources: make(map[string][]string),
	}
}

// Add appends source expressions to the given directive. Each directive may
// only be declared once; declaring it again is reported as an error by Build.
// Use the Nonce constant as a source expression to reference the per-request
// nonce in directives such as ScriptSrc or StyleSrc.
func (builder *PolicyBuilder) Add(directive string, sources ...string) *PolicyBuilder {
	if builder.sources == nil {
		builder.sources = make(map[string][]string)
	}

	builder.names = append(builder.names, directive)
	builder.sources[directive] = append(builder.sources[directive], sources...)

	return builder
}

// MustBuild is like Build but panics if an error occurs.
func (builder *PolicyBuilder) MustBuild() string {
	policy, err := builder.Build()
	if err != nil {
		panic(err)
	}

	return policy
}

// Build validates the declared directives and creates a content security
// policy string. Directives are emitted in sorted order so the output is
// stable. If any declaration is invalid, an error is returned instead.
func (builder *PolicyBuilder) Build() (string, error) {
	directives := make(map[string][]string, len(builder.names))

	for _, name := range builder.names {
		if _, exists := directives[name]; exists {
			return "", fmt.Errorf("duplicate directive %s", name)
		}

		if err := validateToken("directive", name); err != nil {
			return "", err
		}

		sources, err := sanitizeSources(name, builder.sources[name])
		if err != nil {
			return "", err
		}

		directives[name] = sources
	}

	inner := &Builder{Directives: directives}

	return inner.Build()
}

// sanitizeSources validates the source expressions of a directive, removes
// duplicates while preserving the declaration order, and ensures mutually
// exclusive values are not combined.
func sanitizeSources(directive string, sources []string) ([]string, error) {
	deduped := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))

	for _, source := range sources {
		if err := validateToken("source expression", source); err != nil {
			return nil, fmt.Errorf("invalid value for directive %s: %w", directive, err)
		}

		if _, ok := seen[source]; ok {
			continue
		}

		seen[source] = struct{}{}

		deduped = append(deduped, source)
	}

	if _, ok := seen["'none'"]; ok && len(deduped) > 1 {
		return nil, fmt.Errorf("'none' must not be combined with other source expressions for directive %s", directive)
	}

	return deduped, nil
}

// validateToken ensures a directive name or source expression does not
// contain characters that would break the policy serialization.
func validateToken(kind, token string) error {
	if token == "" {
		return fmt.Errorf("empty %s", kind)
	}

	if strings.ContainsAny(token, ";,") || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return fmt.Errorf("illegal separator character in %s %q", kind, token)
	}

	return nil
}
