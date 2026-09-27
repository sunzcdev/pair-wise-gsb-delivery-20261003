package chi

import (
	"net/url"
	"strings"
)

// PathStrategy controls how a Mux derives the routing path of a request
// and how captured URL parameter values are exposed to handlers.
//
// The strategy of a request is decided by the root Mux that serves it
// (see Mux.SetPathStrategy) and is carried by the request's routing
// context, so it applies consistently across the whole request
// lifecycle, including routers composed with Mount.
type PathStrategy int

const (
	// PathStrategyEscaped is the default strategy and chi's historical
	// behavior. The router matches against r.URL.RawPath when it is set,
	// falling back to r.URL.Path, and URL parameter values are exposed
	// exactly as captured. As a result, a parameter captured from a
	// request to "/objects/a%2Fb" is returned by URLParam in its escaped
	// form "a%2Fb".
	PathStrategyEscaped PathStrategy = iota

	// PathStrategyUnescaped preserves the raw segment boundaries of the
	// request path while routing, then percent-decodes each captured URL
	// parameter exactly once before exposing it.
	//
	// The router always matches against r.URL.EscapedPath(): the escaped
	// form of the path, whose segment boundaries are the literal '/'
	// bytes sent by the client. An escaped slash (%2F) therefore stays
	// inside a single path segment while matching and is decoded into
	// '/' only in the parameter value delivered to handlers:
	//
	//	GET /objects/a%2Fb    => URLParam(r, "key") == "a/b"
	//	GET /objects/a%252Fb  => URLParam(r, "key") == "a%2Fb"
	//
	// Use URLParamRaw to access the captured value before decoding.
	//
	// All routing layers follow the same rule: static segments, regexp
	// and wildcard parameters, sub-routers mounted with Mount, path
	// cleaning (middleware.CleanPath) and trailing-slash handling
	// (middleware.StripSlashes, middleware.RedirectSlashes) all operate
	// on the escaped path, so a preserved segment boundary is never
	// re-split by another layer.
	//
	// Notes:
	//   - Regexp parameters are matched against the escaped segment
	//     text, e.g. the pattern {n:[0-9]+} does not match "/%31%32".
	//   - Static segments are matched byte-wise against the escaped
	//     path, so a route registered as "/objects/café" does not match
	//     a request for "/objects/caf%C3%A9" (same as the default
	//     strategy).
	//   - If r.URL.RawPath is an invalid encoding hint it is ignored,
	//     and the router falls back to the escaped form of r.URL.Path.
	//   - Middleware that sets Context.RoutePath must assign the
	//     escaped form of the path under this strategy.
	PathStrategyUnescaped
)

// SetPathStrategy sets the path strategy used by the mux to derive the
// routing path of each request and to expose captured URL parameter
// values. See PathStrategy for the available strategies. The default
// strategy is PathStrategyEscaped.
//
// The strategy is carried by the request's routing context, so it
// applies consistently to the whole request lifecycle, including
// sub-routers mounted with Mount, regardless of which mux in the
// stack this method is called on, as long as it is the mux that
// serves the request.
func (mx *Mux) SetPathStrategy(strategy PathStrategy) {
	mx.pathStrategy = strategy
}

// unescapePathParam percent-decodes a captured URL parameter value
// exactly once. Values that contain no escapes are returned as-is,
// and values that fail to decode (which cannot happen for paths
// derived from r.URL.EscapedPath) are returned undecoded.
func unescapePathParam(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}
