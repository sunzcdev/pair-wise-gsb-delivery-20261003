package cache

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func policyFetch(mutateReq func(*http.Request), mutateResp func(http.Header)) (Fetcher, *int) {
	hits := new(int)
	fetch := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		*hits++
		h := http.Header{}
		h.Set("Cache-Control", "max-age=600")
		if mutateResp != nil {
			mutateResp(h)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body:       io.NopCloser(stringReader("data")),
		}, nil
	}
	_ = mutateReq
	return fetch, hits
}

func twice(t *testing.T, c *Cache, req *http.Request, fetch Fetcher) {
	t.Helper()
	readBody(t, mustFetch(t, c, req, fetch))
	readBody(t, mustFetch(t, c, req, fetch))
}

func TestPrivateNotCached(t *testing.T) {
	c := New(Options{})
	req := newTestRequest("http://example.com/private")
	fetch, hits := policyFetch(nil, func(h http.Header) { h.Set("Cache-Control", "private, max-age=600") })
	twice(t, c, req, fetch)
	if *hits != 2 {
		t.Fatalf("private hits=%d want 2", *hits)
	}
}

func TestNoStoreNotCached(t *testing.T) {
	c := New(Options{})
	req := newTestRequest("http://example.com/nostore")
	fetch, hits := policyFetch(nil, func(h http.Header) { h.Set("Cache-Control", "no-store") })
	twice(t, c, req, fetch)
	if *hits != 2 {
		t.Fatalf("no-store hits=%d want 2", *hits)
	}
}

func TestSetCookieNotCachedUnlessPublic(t *testing.T) {
	c := New(Options{})
	req := newTestRequest("http://example.com/cookie")
	fetch, hits := policyFetch(nil, func(h http.Header) { h.Set("Set-Cookie", "sid=1") })
	twice(t, c, req, fetch)
	if *hits != 2 {
		t.Fatalf("set-cookie hits=%d want 2 (conservative)", *hits)
	}

	// Explicit public makes it cacheable despite the cookie.
	c2 := New(Options{})
	req2 := newTestRequest("http://example.com/cookie2")
	fetch2, hits2 := policyFetch(nil, func(h http.Header) {
		h.Set("Cache-Control", "public, max-age=600")
		h.Set("Set-Cookie", "sid=1")
	})
	twice(t, c2, req2, fetch2)
	if *hits2 != 1 {
		t.Fatalf("public set-cookie hits=%d want 1", *hits2)
	}
}

func TestAuthorizationNotCachedUnlessExplicit(t *testing.T) {
	c := New(Options{})
	req := newTestRequest("http://example.com/auth")
	req.Header.Set("Authorization", "Bearer x")
	fetch, hits := policyFetch(nil, nil)
	twice(t, c, req, fetch)
	if *hits != 2 {
		t.Fatalf("authorization hits=%d want 2", *hits)
	}

	// must-revalidate marks the response shared-cacheable.
	c2 := New(Options{})
	req2 := newTestRequest("http://example.com/auth2")
	req2.Header.Set("Authorization", "Bearer x")
	fetch2, hits2 := policyFetch(nil, func(h http.Header) {
		h.Set("Cache-Control", "max-age=600, must-revalidate")
	})
	twice(t, c2, req2, fetch2)
	if *hits2 != 1 {
		t.Fatalf("must-revalidate auth hits=%d want 1", *hits2)
	}
}

func TestRequestNoStoreBypasses(t *testing.T) {
	c := New(Options{})
	req := newTestRequest("http://example.com/reqnostore")
	req.Header.Set("Cache-Control", "no-store")
	fetch, hits := policyFetch(nil, nil)
	twice(t, c, req, fetch)
	if *hits != 2 {
		t.Fatalf("request no-store hits=%d want 2", *hits)
	}
}

func TestNoCacheRequiresRevalidation(t *testing.T) {
	c := New(Options{})
	var hits int
	prime := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		hits++
		h := http.Header{}
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", `"x"`)
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(stringReader("nc"))}, nil
	}
	readBody(t, mustFetch(t, c, newTestRequest("http://example.com/nc"), prime))

	rev := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: http.StatusNotModified, Header: http.Header{}, Body: io.NopCloser(emptyReader{})}, nil
	}
	readBody(t, mustFetch(t, c, newTestRequest("http://example.com/nc"), rev))
	if hits != 2 {
		t.Fatalf("no-cache hits=%d want 2 (must revalidate)", hits)
	}
}
