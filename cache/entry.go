package cache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// primaryKey identifies a target URL independent of Vary-selected headers.
func primaryKey(req *http.Request) string {
	return req.Method + " " + req.URL.Scheme + "://" + req.URL.Host + req.URL.RequestURI()
}

// selectVariant finds the stored variant matching the request's Vary header
// values. It returns nil when no representation matches.
func selectVariant(entry *Entry, reqHeader http.Header) *Variant {
	if entry == nil {
		return nil
	}
	for _, v := range entry.Variants {
		if v.VaryKey == varyKey(v.VaryNames, reqHeader) {
			return v
		}
	}
	return nil
}

// buildResponse reconstructs a client-facing response from a stored variant.
func (c *Cache) buildResponse(v *Variant, req *http.Request, now time.Time) (*http.Response, bool) {
	body, ok := c.store.GetBody(v.BodyID)
	if !ok {
		return nil, false
	}
	header := v.Header.Clone()
	header.Set(ageHeader, strconv.FormatInt(int64(v.age(now).Seconds()), 10))

	statusCode := v.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	return &http.Response{
		StatusCode:    statusCode,
		Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, true
}

// writeEntry stores one representation, adding or replacing its variant in
// the entry. Bodies are stored first and are content-addressed.
func (c *Cache) writeEntry(key string, req *http.Request, header http.Header, statusCode int, body []byte, requestTime, responseTime time.Time) {
	varyNames, wildcard := varyHeaderNames(header)
	if wildcard {
		return
	}

	id := BodyID(body)
	c.store.PutBody(id, body)

	decision := responseStorePolicy(req.Header, header, statusCode)

	date := parseHTTPDate(header.Get(dateHeader))
	if date.IsZero() {
		date = responseTime
	}
	hAge := headerAge(header)

	variant := &Variant{
		PrimaryKey:   key,
		VaryNames:    varyNames,
		VaryKey:      varyKey(varyNames, req.Header),
		BodyID:       id,
		Header:       sanitizeStoredHeader(header),
		StatusCode:   statusCode,
		Date:         date,
		RequestTime:  requestTime,
		ResponseTime: responseTime,
		HeaderAge:    hAge,
		FreshnessTTL: decision.lifetime,
		MustValidate: decision.mustRevalidate,
		ExpiresAt:    expiry(responseTime, date, requestTime, hAge, decision.lifetime),
	}

	entry, ok := c.store.GetEntry(key)
	if !ok {
		entry = &Entry{PrimaryKey: key}
	} else {
		// Copy so the stored object is never mutated in place; the store
		// treats PutEntry as a replacement and reconciles body refs using
		// the old and new variant sets.
		entry = cloneEntry(entry)
	}
	c.store.PutEntry(mergeVariant(entry, variant))
}

func cloneEntry(e *Entry) *Entry {
	cp := &Entry{PrimaryKey: e.PrimaryKey, Variants: make([]*Variant, 0, len(e.Variants))}
	for _, v := range e.Variants {
		cp.Variants = append(cp.Variants, v.Clone())
	}
	return cp
}

// expiry computes absolute expiry from the corrected initial age.
func expiry(responseTime, date, requestTime time.Time, hAge, lifetime time.Duration) time.Time {
	initialAge := currentAge(responseTime, date, requestTime, responseTime, hAge)
	return responseTime.Add(lifetime - initialAge)
}

// mergeVariant inserts variant, replacing only the matching Vary variant.
// Other variants (e.g. the Chinese representation of the same URL) are
// retained untouched, including their metadata and body references.
func mergeVariant(entry *Entry, variant *Variant) *Entry {
	for i, existing := range entry.Variants {
		if existing.VaryKey == variant.VaryKey {
			entry.Variants[i] = variant
			return entry
		}
	}
	entry.Variants = append(entry.Variants, variant)
	return entry
}

// sanitizeStoredHeader removes hop-by-hop headers and Age, which is
// recomputed whenever a representation is served.
func sanitizeStoredHeader(h http.Header) http.Header {
	out := h.Clone()
	for _, name := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Transfer-Encoding",
		"Upgrade",
		ageHeader,
	} {
		out.Del(name)
	}
	return out
}

// cloneRequestForValidation builds a conditional GET from a stale variant,
// preserving the selecting headers and adding the stored validators.
func cloneRequestForValidation(req *http.Request, stale *Variant) *http.Request {
	cond := req.Clone(context.Background())
	cond.Header = req.Header.Clone()
	cond.Header.Del("If-None-Match")
	cond.Header.Del("If-Modified-Since")
	if etag := stale.Header.Get(etagHeader); etag != "" {
		cond.Header.Set("If-None-Match", etag)
	}
	if lm := stale.Header.Get("Last-Modified"); lm != "" {
		cond.Header.Set("If-Modified-Since", lm)
	}
	return cond
}

// applyNotModified merges a 304 into exactly one stale variant. It updates
// only that variant's metadata (and body relationship); every other variant
// (e.g. the Chinese entry for the same URL) is left intact.
func (c *Cache) applyNotModified(key string, stale *Variant, resp *http.Response, now time.Time) error {
	entry, ok := c.store.GetEntry(key)
	if !ok {
		return fmt.Errorf("cache: entry %q vanished during revalidation", key)
	}

	var target *Variant
	for _, v := range entry.Variants {
		if v.VaryKey == stale.VaryKey {
			target = v
			break
		}
	}
	if target == nil {
		return fmt.Errorf("cache: variant vanished during revalidation")
	}

	updated := target.Clone()
	updated.Header = merge304Headers(updated.Header, resp.Header)

	lifetime, mustRevalidate, hasFreshness := freshness(updated.Header)
	if hasFreshness {
		updated.FreshnessTTL = lifetime
		updated.MustValidate = mustRevalidate
		// A successful 304 re-anchors age at the response's Age header,
		// measured now; resident time since the original fetch must not be
		// double counted (RFC 9111 §4.2.3 updates on revalidation).
		hAge := headerAge(resp.Header)
		updated.HeaderAge = hAge
		updated.RequestTime = now
		updated.ResponseTime = now
		updated.ExpiresAt = now.Add(lifetime - hAge)
	}

	// A 304 carries no body: the existing BodyID relationship is retained.

	c.store.PutEntry(mergeVariant(cloneEntry(entry), updated))
	return nil
}

// merge304Headers overlays a 304's metadata onto stored headers per RFC 9111
// without replacing the body or dropping headers the 304 omits.
func merge304Headers(stored, fresh http.Header) http.Header {
	out := stored.Clone()
	for name, values := range fresh {
		switch http.CanonicalHeaderKey(name) {
		case "Connection", "Keep-Alive", "Proxy-Connection",
			"Transfer-Encoding", "Upgrade", "Warning", ageHeader:
			continue
		}
		out[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	return out
}
