package cache

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	cacheControlHeader = "Cache-Control"
	ageHeader          = "Age"
	etagHeader         = "Etag"
	varyHeader         = "Vary"
	dateHeader         = "Date"
	expiresHeader      = "Expires"
	pragmaHeader       = "Pragma"
)

// directives holds the normalized directives of a Cache-Control header.
// Token directives are recorded with the empty string; delta-seconds
// directives carry their parsed value.
type directives struct {
	tokens map[string]string
}

func parseCacheControl(h http.Header) directives {
	d := directives{tokens: make(map[string]string)}
	for _, hdr := range h[http.CanonicalHeaderKey(cacheControlHeader)] {
		for _, part := range splitComma(hdr) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name, value, hasValue := strings.Cut(part, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if hasValue {
				value = strings.Trim(strings.TrimSpace(value), `"`)
				d.tokens[name] = value
			} else {
				d.tokens[name] = ""
			}
		}
	}
	return d
}

func (d directives) has(name string) bool {
	_, ok := d.tokens[name]
	return ok
}

func (d directives) seconds(name string) (time.Duration, bool) {
	v, ok := d.tokens[name]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func splitComma(s string) []string {
	// Cache-Control tokens never legitimately contain quoted commas for
	// the directives we understand, so a plain split is sufficient.
	return strings.Split(s, ",")
}

// freshness computes the freshness lifetime for a stored response per
// RFC 9111 §4.2.1 (Cache-Control wins over Expires). A zero result with
// ok=false means no heuristic lifetime is permitted.
func freshness(respHeader http.Header) (lifetime time.Duration, mustRevalidate bool, ok bool) {
	d := parseCacheControl(respHeader)
	if d.has("no-cache") || d.has("no-store") {
		// no-cache stores the response but requires every-use validation;
		// no-store is handled (and rejected) by the caller.
		return 0, d.has("no-cache"), d.has("no-cache")
	}
	if maxAge, present := d.seconds("max-age"); present {
		return maxAge, d.has("must-revalidate"), true
	}
	if exp := parseHTTPDate(respHeader.Get(expiresHeader)); !exp.IsZero() {
		if date := parseHTTPDate(respHeader.Get(dateHeader)); !date.IsZero() {
			if delta := exp.Sub(date); delta > 0 {
				return delta, d.has("must-revalidate"), true
			}
			return 0, d.has("must-revalidate"), true
		}
	}
	return 0, d.has("must-revalidate"), false
}

func parseHTTPDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC1123, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// currentAge implements RFC 9111 §4.2.3. headerAge is the Age header value
// at storage time; date is the response Date (or storage time fallback).
func currentAge(now, date, requestTime, responseTime time.Time, headerAge time.Duration) time.Duration {
	apparentAge := responseTime.Sub(date)
	if apparentAge < 0 {
		apparentAge = 0
	}
	correctedReceivedAge := maxDuration(headerAge, apparentAge)
	responseDelay := responseTime.Sub(requestTime)
	if responseDelay < 0 {
		responseDelay = 0
	}
	correctedInitialAge := correctedReceivedAge + responseDelay
	residentTime := now.Sub(responseTime)
	if residentTime < 0 {
		residentTime = 0
	}
	return correctedInitialAge + residentTime
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// headerAge reads a syntactically valid Age delta-seconds value.
func headerAge(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get(ageHeader))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// requestRevalidate reports whether the client demands validation
// (Cache-Control: no-cache or Pragma: no-cache).
func requestRevalidate(h http.Header) bool {
	d := parseCacheControl(h)
	if d.has("no-cache") {
		return true
	}
	for _, p := range h[http.CanonicalHeaderKey(pragmaHeader)] {
		for _, part := range splitComma(p) {
			if strings.EqualFold(strings.TrimSpace(part), "no-cache") {
				return true
			}
		}
	}
	return false
}

// varyHeaderNames returns the request header names named by Vary.
// A wildcard ("Vary: *") is reported separately.
func varyHeaderNames(respHeader http.Header) (names []string, wildcard bool) {
	seen := make(map[string]bool)
	for _, v := range respHeader[http.CanonicalHeaderKey(varyHeader)] {
		if strings.TrimSpace(v) == "*" {
			return nil, true
		}
		for _, name := range splitComma(v) {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names, false
}

// varyKey builds the discriminating key for a request given the Vary names.
func varyKey(names []string, reqHeader http.Header) string {
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.Join(reqHeader[http.CanonicalHeaderKey(name)], ","))
	}
	return b.String()
}
