package goproxy

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseCacheControl parses a Cache-Control header into a map of directives.
// Token-only directives map to "true"; delta-seconds directives map to
// their value.
func parseCacheControl(header string) map[string]string {
	directives := make(map[string]string)
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if !ok {
			directives[name] = "true"
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, "\"")
		directives[name] = value
	}
	return directives
}

// headerToken reports whether header name contains token (case-insensitive,
// comma-separated). An empty token with an empty value checks presence.
func headerToken(h http.Header, name, token string) bool {
	value := h.Get(name)
	if value == "" {
		return false
	}
	if token == "" {
		return true
	}
	for _, item := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(item), token) {
			return true
		}
	}
	return false
}

// currentAge implements the age_value/current_age calculation of RFC 9111
// section 4.2.3.
func (c *CachedResponse) currentAge(now time.Time) time.Duration {
	// apparent_age = max(0, response_time - date_value)
	apparentAge := c.ResponseTime.Sub(c.Date)
	if apparentAge < 0 {
		apparentAge = 0
	}
	// corrected_received_age = max(apparent_age, age_value)
	correctedReceivedAge := c.AgeValue
	if apparentAge > correctedReceivedAge {
		correctedReceivedAge = apparentAge
	}
	residentTime := now.Sub(c.ResponseTime)
	if residentTime < 0 {
		residentTime = 0
	}
	// response_delay is unknown for a proxy response, treated as zero;
	// corrected_initial_age = corrected_received_age + response_delay.
	// current_age = corrected_initial_age + resident_time.
	return correctedReceivedAge + residentTime
}

// mustRevalidate reports whether the stored response requires
// revalidation before every use, regardless of freshness. must-revalidate
// and proxy-revalidate only change stale behavior (which the cache always
// treats as "revalidate, never serve stale"), so they need no special case
// here; no-cache is the always-revalidate directive.
func (c *CachedResponse) mustRevalidate() bool {
	cc := parseCacheControl(c.Header.Get("Cache-Control"))
	return cc["no-cache"] != ""
}

// toResponse reconstructs an http.Response for a client, setting Age.
func (c *CachedResponse) toResponse(r *http.Request, age time.Duration) *http.Response {
	headers := c.Header.Clone()
	headers.Set("Age", strconv.FormatInt(int64(age.Seconds()), 10))
	return &http.Response{
		StatusCode:    c.StatusCode,
		Status:        http.StatusText(c.StatusCode),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        headers,
		Trailer:       cloneHeader(c.Trailer),
		ContentLength: int64(len(c.Body)),
		Body:          io.NopCloser(bytes.NewReader(c.Body)),
		Request:       r,
	}
}

func cloneHeader(h http.Header) http.Header {
	if len(h) == 0 {
		return nil
	}
	return h.Clone()
}

// varyNames parses a Vary response header. A wildcard is reported
// separately and disables caching.
func varyNames(header http.Header) (names []string, wildcard bool) {
	for _, field := range header.Values("Vary") {
		for _, name := range strings.Split(field, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if name == "*" {
				return nil, true
			}
			names = append(names, http.CanonicalHeaderKey(name))
		}
	}
	return names, false
}

// varySelector builds the per-variant key from request headers and returns
// the restricted request header snapshot for storage.
func varySelector(names []string, r *http.Request) (string, http.Header) {
	if len(names) == 0 {
		return "", nil
	}
	var b strings.Builder
	snapshot := make(http.Header)
	for _, name := range names {
		values := r.Header.Values(name)
		snapshot[name] = append([]string(nil), values...)
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.Join(values, ","))
		b.WriteByte('\n')
	}
	return b.String(), snapshot
}

// freshnessLifetime computes the shared-cache freshness lifetime per
// RFC 9111 section 4.2.1 (s-maxage, then max-age, then Expires). A missing
// lifetime is zero: the response is immediately stale but can still be
// stored for revalidation if it carries a validator.
func freshnessLifetime(respHeader http.Header, date time.Time) (time.Duration, bool) {
	cc := parseCacheControl(respHeader.Get("Cache-Control"))
	if v, ok := cc["s-maxage"]; ok {
		if seconds, err := strconv.Atoi(v); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second, true
		}
	}
	if v, ok := cc["max-age"]; ok {
		if seconds, err := strconv.Atoi(v); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second, true
		}
	}
	if expires := respHeader.Get("Expires"); expires != "" {
		if t, err := http.ParseTime(expires); err == nil {
			d := t.Sub(date)
			if d < 0 {
				d = 0
			}
			return d, true
		}
	}
	return 0, false
}

// parseDate returns the origin Date, falling back to responseTime.
func parseDate(header http.Header, responseTime time.Time) time.Time {
	if v := header.Get("Date"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			return t
		}
	}
	return responseTime
}

// hopByHopHeaders are removed before storage and never replayed.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// sanitizeStoredHeader strips hop-by-hop headers, including the connection
// tokens named by the Connection header itself.
func sanitizeStoredHeader(h http.Header) http.Header {
	out := h.Clone()
	var tokens []string
	for _, v := range out.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tokens = append(tokens, http.CanonicalHeaderKey(t))
			}
		}
	}
	names := make([]string, 0, len(hopByHopHeaders)+len(tokens))
	names = append(names, hopByHopHeaders...)
	names = append(names, tokens...)
	for _, name := range names {
		out.Del(name)
	}
	return out
}

// sharedExplicitly marks Cache-Control directives that allow caching a
// response to an authenticated request in a shared cache.
func sharedExplicitly(cc map[string]string) bool {
	return cc["public"] != "" || cc["s-maxage"] != "" || cc["must-revalidate"] != ""
}

// responseCacheable applies the conservative shared-cache storage policy.
// It returns the normalized Vary names and whether the response may be
// stored.
func (hc *httpCache) responseCacheable(req *http.Request, resp *http.Response) ([]string, bool) {
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	cc := parseCacheControl(resp.Header.Get("Cache-Control"))
	if cc["no-store"] != "" || cc["private"] != "" {
		return nil, false
	}
	names, wildcard := varyNames(resp.Header)
	if wildcard {
		return nil, false
	}
	if req.Header.Get("Authorization") != "" && !sharedExplicitly(cc) {
		// Authenticated response without an explicit shared-cache marker.
		return nil, false
	}
	if resp.Header.Get("Set-Cookie") != "" {
		// User state attached to the response: cache only when the origin
		// explicitly makes it public, or cookies are part of the key.
		if cc["public"] == "" && !containsHeader(names, "Cookie") {
			return nil, false
		}
	}
	if req.Header.Get("Cookie") != "" && !containsHeader(names, "Cookie") {
		// Request carried user state not accounted for by Vary: Cookie.
		return nil, false
	}
	return names, true
}

func containsHeader(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// headersUpdatedOn304 are end-to-end headers that a 304 may refresh; the
// remaining stored headers are retained (notably Content-* describing the
// unchanged body, and Date).
var headersUpdatedOn304 = []string{
	"Cache-Control",
	"Expires",
	"ETag",
	"Last-Modified",
	"Age",
	"Vary",
	"Warning",
}

// merge304 refreshes only the metadata of a stored variant after a 304,
// keeping its Body and body-describing headers. It returns a new variant;
// the input is not mutated.
func merge304(stored *CachedResponse, notModified http.Header, now time.Time) *CachedResponse {
	header := stored.Header.Clone()
	for _, name := range headersUpdatedOn304 {
		header.Del(name)
	}
	// Date is the validation epoch: prefer the 304 Date, drop the stored
	// one so parseDate falls back to the revalidation time.
	header.Del("Date")
	for name, values := range notModified {
		canonical := http.CanonicalHeaderKey(name)
		if isHopByHop(canonical) {
			continue
		}
		header[canonical] = append([]string(nil), values...)
	}

	// The 304 response defines the new validation epoch. Date is taken
	// from the 304 when present, otherwise the revalidation time.
	responseTime := now
	date := parseDate(header, responseTime)
	lifetime, _ := freshnessLifetime(header, date)

	age := time.Duration(0)
	if v := header.Get("Age"); v != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && seconds >= 0 {
			age = time.Duration(seconds) * time.Second
		}
	}

	return &CachedResponse{
		StatusCode:        stored.StatusCode,
		Header:            sanitizeStoredHeader(header),
		Trailer:           cloneHeader(stored.Trailer),
		Body:              stored.Body,
		RequestHeaders:    stored.RequestHeaders,
		Date:              date,
		ResponseTime:      responseTime,
		AgeValue:          age,
		FreshnessLifetime: lifetime,
	}
}

func isHopByHop(name string) bool {
	for _, h := range hopByHopHeaders {
		if h == name {
			return true
		}
	}
	return false
}

// readLimitedBody buffers at most limit bytes. A body exceeding the limit
// returns errBodyTooLarge without caching it.
func readLimitedBody(r io.Reader, limit int64) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	written, err := io.Copy(buf, io.LimitReader(r, limit+1))
	if err != nil {
		return buf.Bytes(), err
	}
	if written > limit {
		return buf.Bytes(), errBodyTooLarge
	}
	return buf.Bytes(), nil
}

var errBodyTooLarge = &cacheError{"response body exceeds cache MaxBodyBytes"}

type cacheError struct{ msg string }

func (e *cacheError) Error() string { return e.msg }
