# Response cache example

A minimal caching proxy. The shared GET response cache is disabled by
default and enabled by assigning a `goproxy.CacheConfig`.

```sh
go run main.go -addr :8080
```

Then point a client at it:

```sh
curl -x http://127.0.0.1:8080 -v http://example.com/
```

The first request is fetched from the origin; repeat requests within the
response freshness lifetime are served from cache with an updated `Age`
header. Stale responses carrying an `ETag` or `Last-Modified` are
revalidated with a conditional request, and concurrent callers share a
single revalidation per `Vary` variant.
