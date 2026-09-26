package chi

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Name assigns a name to the most recently registered route on the mux,
// so its URL can later be generated with URL(). It is optional; routes
// registered without a name behave exactly as before.
//
//	r.Get("/users/{id}", showUser)
//	r.Name("user.show")
//
// Names registered on an inline mux (With/Group) are stored on the root
// mux of the shared routing tree. Names must be unique per mux; registering
// a duplicate name panics. Note that a name may also become ambiguous if
// sub-routers mounted at different prefixes each register the same name -
// URL() reports an error in that case instead of guessing.
func (mx *Mux) Name(name string) {
	if mx.lastPattern == "" {
		panic("chi: Name() must be called after registering a route")
	}

	// Inline muxes share the routing tree with their parent, so store
	// the name on the root mux of the tree.
	root := mx
	for root.inline && root.parent != nil {
		root = root.parent
	}

	if root.namedRoutes == nil {
		root.namedRoutes = map[string]string{}
	}
	if _, ok := root.namedRoutes[name]; ok {
		panic(fmt.Sprintf("chi: duplicate route name '%s'", name))
	}
	root.namedRoutes[name] = mx.lastPattern
}

// URL generates a URL for the route registered with the given name.
//
// Path parameters are taken from params, keyed by the placeholder names in
// the routing pattern ("*" for a wildcard). Query parameters are passed
// separately via query and are never consumed as path parameters. Every
// placeholder in the pattern must have a corresponding entry in params, and
// every entry in params must be consumed by the pattern; values must also
// satisfy the regexp of a {name:regexp} placeholder. Any violation results
// in an error rather than a silently broken link.
//
// Routes named on sub-routers mounted with Mount (possibly multiple levels
// deep, possibly with placeholders in the mount patterns) are resolved from
// the root mux with the full prefix applied.
//
// The returned URL always carries a fully decoded Path. When any parameter
// value or static segment requires percent-encoding (e.g. it contains '/',
// '%', ' ' or non-ASCII characters), RawPath holds the encoded form, such
// that decoding RawPath exactly once yields Path - no more, no less. When
// no encoding is needed RawPath is left empty. A wildcard parameter keeps
// its '/' separators literal while each segment is encoded individually.
func (mx *Mux) URL(name string, params map[string]string, query url.Values) (*url.URL, error) {
	patterns := mx.findNamedPatterns(name)
	if len(patterns) == 0 {
		return nil, fmt.Errorf("chi: route named '%s' not found", name)
	}
	if len(patterns) > 1 {
		return nil, fmt.Errorf("chi: route name '%s' is ambiguous, found %d routes: %s", name, len(patterns), strings.Join(patterns, ", "))
	}
	return buildNamedURL(name, patterns[0], params, query)
}

// findNamedPatterns returns the full patterns (including mount prefixes) of
// every route registered under name, searching mounted sub-routers too.
func (mx *Mux) findNamedPatterns(name string) []string {
	var patterns []string
	mx.collectNamedPatterns(name, "", &patterns)
	return patterns
}

func (mx *Mux) collectNamedPatterns(name, prefix string, patterns *[]string) {
	if pattern, ok := mx.namedRoutes[name]; ok {
		*patterns = append(*patterns, prefix+pattern)
	}
	for _, route := range mx.tree.routes() {
		subMux, ok := route.SubRoutes.(*Mux)
		if !ok {
			continue
		}
		// The subroutes node pattern always ends in "/*"; the mount
		// prefix is everything before it, e.g. "/api/*" -> "/api".
		mountPrefix := strings.TrimSuffix(strings.TrimSuffix(route.Pattern, "*"), "/")
		subMux.collectNamedPatterns(name, prefix+mountPrefix, patterns)
	}
}

// routeSegment is one piece of a compiled routing pattern: either a static
// string, or a {param} / {param:regexp} / * placeholder.
type routeSegment struct {
	static   bool
	raw      string         // static text
	key      string         // param key, "*" for wildcard
	rex      *regexp.Regexp // set for {key:regexp} params
	wildcard bool           // true for the trailing * placeholder
}

// compileRoutePattern parses a routing pattern into static and parameter
// segments, reusing the same segment parser as the routing tree.
func compileRoutePattern(pattern string) []routeSegment {
	var segments []routeSegment
	pat := pattern
	for {
		ptyp, key, rexpat, _, start, end := patNextSegment(pat)
		if ptyp == ntStatic {
			if pat != "" {
				segments = append(segments, routeSegment{static: true, raw: pat})
			}
			return segments
		}
		if start > 0 {
			segments = append(segments, routeSegment{static: true, raw: pat[:start]})
		}
		seg := routeSegment{key: key, wildcard: ptyp == ntCatchAll}
		if ptyp == ntRegexp {
			// rexpat was already validated when the route was inserted.
			seg.rex = regexp.MustCompile(rexpat)
		}
		segments = append(segments, seg)
		pat = pat[end:]
	}
}

// buildNamedURL substitutes params into the compiled pattern and assembles
// the decoded Path and, when needed, the singly-encoded RawPath.
func buildNamedURL(name, pattern string, params map[string]string, query url.Values) (*url.URL, error) {
	segments := compileRoutePattern(pattern)

	var path, rawPath strings.Builder
	used := make(map[string]struct{}, len(segments))

	for _, seg := range segments {
		if seg.static {
			path.WriteString(seg.raw)
			rawPath.WriteString(escapePathSegments(seg.raw))
			continue
		}

		value, ok := params[seg.key]
		if !ok {
			return nil, fmt.Errorf("chi: route '%s' (%s) is missing a value for param '%s'", name, pattern, seg.key)
		}
		used[seg.key] = struct{}{}

		if seg.rex != nil && !seg.rex.MatchString(value) {
			return nil, fmt.Errorf("chi: route '%s' (%s) param '%s' value '%s' does not match the route regexp", name, pattern, seg.key, value)
		}

		path.WriteString(value)
		if seg.wildcard {
			// A wildcard value may span multiple path segments; keep the
			// '/' separators literal and encode each segment on its own.
			rawPath.WriteString(escapePathSegments(value))
		} else {
			rawPath.WriteString(url.PathEscape(value))
		}
	}

	for key := range params {
		if _, ok := used[key]; !ok {
			return nil, fmt.Errorf("chi: route '%s' (%s) got unexpected param '%s'", name, pattern, key)
		}
	}

	u := &url.URL{Path: path.String()}
	if escaped := rawPath.String(); escaped != u.Path {
		u.RawPath = escaped
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u, nil
}

// escapePathSegments percent-encodes s one path segment at a time, leaving
// the '/' separators between segments intact.
func escapePathSegments(s string) string {
	parts := strings.Split(s, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
