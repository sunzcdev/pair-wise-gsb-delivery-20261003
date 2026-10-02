package cache

import (
	"net/http"
	"strings"
	"time"
)

// cacheableStatus is the conservative v1 set: only complete successful
// representations are stored. Revalidation failures and partial responses
// are never written.
func cacheableStatus(status int) bool {
	return status == http.StatusOK
}

// requestMayUseCache decides whether a request may touch the shared cache.
// The policy is deliberately conservative; when in doubt, the request is
// forwarded straight to the origin and its response is not stored.
func requestMayUseCache(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	// Range and conditional requests need end-to-end semantics in v1.
	if r.Header.Get("Range") != "" ||
		r.Header.Get("If-Range") != "" ||
		r.Header.Get("If-None-Match") != "" ||
		r.Header.Get("If-Modified-Since") != "" ||
		r.Header.Get("If-Match") != "" ||
		r.Header.Get("If-Unmodified-Since") != "" {
		return false
	}
	// Upgrades (including WebSockets) are tunnelled, never cached.
	if connectionUpgrades(r.Header) {
		return false
	}
	d := parseCacheControl(r.Header)
	if d.has("no-store") {
		return false
	}
	return true
}

// responseDecision records whether and how a response may be stored in the
// shared cache.
type responseDecision struct {
	store          bool
	mustRevalidate bool
	lifetime       time.Duration
	hasLifetime    bool
}

// responseStorePolicy applies the conservative shared-cache rules of
// RFC 9111. It is called only for cache-eligible GET traffic.
func responseStorePolicy(reqHeader, respHeader http.Header, status int) responseDecision {
	dec := responseDecision{}
	if !cacheableStatus(status) {
		return dec
	}

	rd := parseCacheControl(respHeader)
	if rd.has("no-store") {
		return dec
	}
	// hop-by-hop / streaming features we do not reconstruct in v1.
	if respHeader.Get("Transfer-Encoding") != "" {
		return dec
	}

	// Vary: * forbids selecting a single stored variant.
	if _, wildcard := varyHeaderNames(respHeader); wildcard {
		return dec
	}

	// Responses to an Authorization request are private to the client
	// unless the origin explicitly marks them shared-cacheable.
	if reqHeader.Get("Authorization") != "" {
		if !(rd.has("must-revalidate") || rd.has("public") || rd.has("s-maxage")) {
			return dec
		}
	}

	// Private responses must not be written to a shared cache.
	if rd.has("private") {
		return dec
	}

	// Set-Cookie carries user state. Without an explicit public marker it
	// is conservatively treated as private.
	if respHeader.Get("Set-Cookie") != "" && !rd.has("public") {
		return dec
	}

	dec.store = true
	if rd.has("no-cache") {
		dec.mustRevalidate = true
		dec.hasLifetime = true
		return dec
	}

	lifetime, mustRevalidate, ok := freshness(respHeader)
	if !ok {
		// No explicit freshness and no usable Expires: do not cache rather
		// than applying a heuristic, per the conservative v1 policy.
		dec.store = false
		return dec
	}
	dec.mustRevalidate = mustRevalidate
	dec.lifetime = lifetime
	dec.hasLifetime = true
	return dec
}

// connectionUpgrades reports whether a request or response is negotiating
// an upgraded protocol (WebSocket, h2c, ...), which must not be cached.
func connectionUpgrades(h http.Header) bool {
	if h.Get("Upgrade") != "" {
		return true
	}
	for _, token := range strings.Split(h.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}
