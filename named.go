package chi

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// namedRoute is a route registered under a name, keeping the parsed
// pattern segments for reverse URL generation.
type namedRoute struct {
	pattern  string
	segments []routeSegment
}

// routeSegment is one piece of a parsed routing pattern: a static chunk of
// text, a {param}, a {param:regexp} or a trailing * wildcard.
type routeSegment struct {
	typ nodeTyp        // static, param, regexp or catch-all segment
	raw string         // static text (ntStatic only)
	key string         // param key, "*" for ntCatchAll
	rex *regexp.Regexp // anchored matcher (ntRegexp only)
}

// parsePattern splits a routing pattern into static and parameter segments.
// The pattern must already be valid, i.e. accepted by InsertRoute.
func parsePattern(pattern string) []routeSegment {
	segs := []routeSegment{}
	rest := pattern
	for len(rest) > 0 {
		typ, key, rexpat, _, start, end := patNextSegment(rest)
		if start > 0 {
			segs = append(segs, routeSegment{typ: ntStatic, raw: rest[:start]})
		}
		if typ == ntStatic {
			segs = append(segs, routeSegment{typ: ntStatic, raw: rest})
			break
		}
		seg := routeSegment{typ: typ, key: key}
		if typ == ntRegexp {
			seg.rex = regexp.MustCompile(rexpat)
		}
		segs = append(segs, seg)
		rest = rest[end:]
	}
	return segs
}

// namedMux returns the mux holding the named-route registry for mx. Inline
// muxes created by With()/Group() share the routing tree of their parent,
// so their named routes are registered on the parent as well.
func (mx *Mux) namedMux() *Mux {
	if mx.inline && mx.parent != nil {
		return mx.parent.namedMux()
	}
	return mx
}

// namedHandle registers a handler for method+pattern and records the route
// under name for reverse URL generation with URL.
func (mx *Mux) namedHandle(method methodTyp, name, pattern string, handler http.Handler) {
	nm := mx.namedMux()
	if existing, ok := nm.named[name]; ok && existing.pattern != pattern {
		panic(fmt.Sprintf("chi: duplicate route name '%s' for patterns '%s' and '%s'", name, existing.pattern, pattern))
	}
	mx.handle(method, pattern, handler)
	if nm.named == nil {
		nm.named = map[string]*namedRoute{}
	}
	if _, ok := nm.named[name]; !ok {
		nm.named[name] = &namedRoute{pattern: pattern, segments: parsePattern(pattern)}
	}
}

// NamedHandle adds the route `pattern` that matches any http method to
// execute the `handler` http.Handler, and registers it under `name` for
// reverse URL generation with URL.
func (mx *Mux) NamedHandle(name, pattern string, handler http.Handler) {
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		method, rest := pattern[:i], strings.TrimLeft(pattern[i+1:], " \t")
		mx.NamedMethod(name, method, rest, handler)
		return
	}
	mx.namedHandle(mALL, name, pattern, handler)
}

// NamedHandleFunc adds the route `pattern` that matches any http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedHandleFunc(name, pattern string, handlerFn http.HandlerFunc) {
	mx.NamedHandle(name, pattern, handlerFn)
}

// NamedMethod adds the route `pattern` that matches `method` http method to
// execute the `handler` http.Handler, and registers it under `name`.
func (mx *Mux) NamedMethod(name, method, pattern string, handler http.Handler) {
	m, ok := methodMap[strings.ToUpper(method)]
	if !ok {
		panic(fmt.Sprintf("chi: '%s' http method is not supported.", method))
	}
	mx.namedHandle(m, name, pattern, handler)
}

// NamedMethodFunc adds the route `pattern` that matches `method` http method
// to execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedMethodFunc(name, method, pattern string, handlerFn http.HandlerFunc) {
	mx.NamedMethod(name, method, pattern, handlerFn)
}

// NamedConnect adds the route `pattern` that matches a CONNECT http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedConnect(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mCONNECT, name, pattern, handlerFn)
}

// NamedDelete adds the route `pattern` that matches a DELETE http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedDelete(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mDELETE, name, pattern, handlerFn)
}

// NamedGet adds the route `pattern` that matches a GET http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedGet(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mGET, name, pattern, handlerFn)
}

// NamedHead adds the route `pattern` that matches a HEAD http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedHead(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mHEAD, name, pattern, handlerFn)
}

// NamedOptions adds the route `pattern` that matches an OPTIONS http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedOptions(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mOPTIONS, name, pattern, handlerFn)
}

// NamedPatch adds the route `pattern` that matches a PATCH http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedPatch(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mPATCH, name, pattern, handlerFn)
}

// NamedPost adds the route `pattern` that matches a POST http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedPost(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mPOST, name, pattern, handlerFn)
}

// NamedPut adds the route `pattern` that matches a PUT http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedPut(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mPUT, name, pattern, handlerFn)
}

// NamedQuery adds the route `pattern` that matches a QUERY http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedQuery(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mQUERY, name, pattern, handlerFn)
}

// NamedTrace adds the route `pattern` that matches a TRACE http method to
// execute the `handlerFn` http.HandlerFunc, and registers it under `name`.
func (mx *Mux) NamedTrace(name, pattern string, handlerFn http.HandlerFunc) {
	mx.namedHandle(mTRACE, name, pattern, handlerFn)
}

// composedRoute is a named route found during lookup, with the mount
// prefixes of any parent routers prepended.
type composedRoute struct {
	pattern  string
	segments []routeSegment
}

// collectNamed appends every route registered under name that is reachable
// from mx, composing mount patterns of nested sub-routers into the result.
func (mx *Mux) collectNamed(name string, prefixPat string, prefix []routeSegment, out *[]composedRoute) {
	if nr, ok := mx.named[name]; ok {
		segments := make([]routeSegment, 0, len(prefix)+len(nr.segments))
		segments = append(segments, prefix...)
		segments = append(segments, nr.segments...)
		*out = append(*out, composedRoute{pattern: prefixPat + nr.pattern, segments: segments})
	}
	for _, rt := range mx.tree.routes() {
		sub, ok := rt.SubRoutes.(*Mux)
		if !ok {
			continue
		}
		mountPat := strings.TrimSuffix(rt.Pattern, "/*")
		sub.collectNamed(name, prefixPat+mountPat, append(prefix, parsePattern(mountPat)...), out)
	}
}

// URL generates a URL for the route registered under name, substituting the
// path placeholders with params and appending query as the query string.
//
// The lookup covers the receiver and every chi router mounted below it, so
// it is typically called on the root mux. Mount patterns of sub-routers are
// prepended to the route pattern, including any {param} placeholders they
// declare; a placeholder name may appear at multiple levels and is filled
// from the same params entry. The trailing "*" wildcard is addressed with
// the params key "*", and its value may contain '/' path separators.
//
// params must contain exactly the values consumed by the path: a missing
// placeholder, an unused params entry, an empty value for a non-wildcard
// placeholder, or a value rejected by a {param:regexp} constraint is
// reported as an error instead of silently producing a broken link. Query
// parameters are passed separately via query and are never consumed as
// path parameters.
//
// The returned *url.URL carries the decoded path in Path and, when
// escaping is needed, its escaped form in RawPath, such that unescaping
// RawPath exactly once yields Path (url.PathUnescape(u.EscapedPath()) ==
// u.Path). Placeholder values containing '/', '%', ' ' or non-ASCII
// characters are percent-encoded once in RawPath; a '*' wildcard value
// keeps its '/' separators. Note that chi matches incoming requests
// against the escaped path, so a handler reading such a placeholder with
// URLParam observes the escaped value and must apply url.PathUnescape once
// to recover the original.
func (mx *Mux) URL(name string, params map[string]string, query url.Values) (*url.URL, error) {
	var matches []composedRoute
	mx.collectNamed(name, "", nil, &matches)
	if len(matches) == 0 {
		return nil, fmt.Errorf("chi: no route registered with name '%s'", name)
	}
	route := matches[0]
	for _, m := range matches[1:] {
		if m.pattern != route.pattern {
			return nil, fmt.Errorf("chi: name '%s' matches multiple routes '%s' and '%s'", name, route.pattern, m.pattern)
		}
	}

	var path, raw strings.Builder
	used := make(map[string]struct{}, len(params))
	for _, seg := range route.segments {
		switch seg.typ {
		case ntStatic:
			path.WriteString(seg.raw)
			raw.WriteString(escapePath(seg.raw, true))

		case ntCatchAll:
			value, ok := params["*"]
			if !ok {
				return nil, fmt.Errorf("chi: route '%s' is missing a value for the '*' wildcard", name)
			}
			used["*"] = struct{}{}
			path.WriteString(value)
			raw.WriteString(escapePath(value, true))

		default: // ntParam, ntRegexp
			value, ok := params[seg.key]
			if !ok {
				return nil, fmt.Errorf("chi: route '%s' is missing a value for parameter '%s'", name, seg.key)
			}
			used[seg.key] = struct{}{}
			if value == "" {
				return nil, fmt.Errorf("chi: route '%s' got an empty value for parameter '%s'", name, seg.key)
			}
			if seg.typ == ntRegexp && !seg.rex.MatchString(value) {
				return nil, fmt.Errorf("chi: route '%s' parameter '%s' value '%s' does not match '%s'", name, seg.key, value, seg.rex.String())
			}
			path.WriteString(value)
			raw.WriteString(escapePath(value, false))
		}
	}
	for key := range params {
		if _, ok := used[key]; !ok {
			return nil, fmt.Errorf("chi: route '%s' got an unexpected parameter '%s'", name, key)
		}
	}

	u := &url.URL{Path: path.String()}
	if escaped := raw.String(); escaped != u.Path {
		u.RawPath = escaped
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return u, nil
}

// escapePath percent-encodes s for use in the escaped form of a URL path.
// When allowSlash is set, '/' is kept as a path separator; otherwise it is
// encoded as %2F so the value stays within a single path segment.
func escapePath(s string, allowSlash bool) string {
	if !allowSlash {
		return url.PathEscape(s)
	}
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
