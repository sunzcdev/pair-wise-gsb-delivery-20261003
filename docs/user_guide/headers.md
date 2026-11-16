(headers)=
# {fas}`file-code` Cache Headers
Requests-cache supports most common HTTP caching headers, including
[ETags](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/ETag),
[Cache-Control](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control),
and several extensions.

(conditional-requests)=
## Conditional Requests
[Conditional requests](https://developer.mozilla.org/en-US/docs/Web/HTTP/Conditional_requests) are
automatically sent for any servers that support them. Once a cached response expires, it will only
be updated if the remote content has changed.

Here's an example using the [GitHub API](https://docs.github.com/en/rest) to get info about the
requests-cache repo:
```python
>>> # Cache a response that will expire immediately
>>> url = 'https://api.github.com/repos/requests-cache/requests-cache'
>>> session = CachedSession(expire_after=1)
>>> session.get(url)
>>> time.sleep(1)

>>> # The cached response will still be used until the remote content actually changes
>>> response = session.get(url)
>>> print(response.from_cache)
True
```

```{note}
Also see {ref}`stale-while-revalidate` for a variation of this behavior.
```

(bulk-revalidation)=
## Bulk revalidation
In addition to revalidating responses one at a time as they are requested, a whole cache (or a
subset of it) can be proactively revalidated with bounded concurrency via
{py:meth:`.CachedSession.revalidate`. A conditional request is sent for each cached entity that
has a validator:

```python
>>> from threading import Event
>>> report = session.revalidate(max_workers=4)
>>> report.counts()
{'not_modified': 8, 'updated': 1, 'invalidated': 0, 'skipped': 2, 'failed': 0}
```

Each entity is identified by its cache key, so distinct `Vary` variants of the same URL are
revalidated separately. Every entity gets an independent result, so one failing request never
discards the others; the previously cached responses also remain readable after a failure.
Entities that could not be confirmed with the origin (failures and skips) are available through
{py:attr}`.RevalidationReport.unverified`:

```python
>>> for result in report.unverified:
...     print(result.cache_key, result.status, result.reason or result.error)
```

Revalidation does not dispatch normal response hooks. It can be cancelled cooperatively by
passing a {py:class}`threading.Event`; after cancellation no new entities are admitted, while
requests already in flight are allowed to finish and the remaining entities are reported as
skipped:

```python
>>> cancel_event = Event()
>>> report = session.revalidate(cancel_event=cancel_event)
>>> # ...from another thread, to stop admitting new entities:
>>> cancel_event.set()
```

(cache-control)=
## Cache-Control
`Cache-Control` **request** headers will always be used if present. This is mainly useful if you are
adding requests-cache to an existing application or library that already sends requests with cache
headers.

`Cache-Control` **response** headers are an opt-in feature. If enabled, these will take priority over
any other `expire_after` values. See {ref}`precedence` for the full order of precedence.
To enable this behavior, use the `cache_control` option:
```python
>>> session = CachedSession(cache_control=True)
```

## Supported Headers
Requests-cache implements the majority of private cache behaviors specified by the following RFCs,
with some minor variations:
* [RFC 2616](https://datatracker.ietf.org/doc/html/rfc2616)
* [RFC 5861](https://datatracker.ietf.org/doc/html/rfc5861)
* [RFC 7234](https://datatracker.ietf.org/doc/html/rfc7234)
* [RFC 8246](https://datatracker.ietf.org/doc/html/rfc8246)

The following headers are currently supported:

**Request headers:**
- `Cache-Control: max-age`: Used as the expiration time in seconds
- `Cache-Control: max-stale`: Accept responses that have been expired for up to this many seconds
- `Cache-Control: min-fresh`: Don't accept responses if they will expire within this many seconds
- `Cache-Control: no-cache`: Revalidate with the server before using a cached response
- `Cache-Control: no-store`: Skip reading from and writing to the cache
- `Cache-Control: only-if-cached`: Only return results from the cache. If not cached, return a 504
  response instead of sending a new request. Note that this may return a stale response.
- `Cache-Control: stale-if-error`: If an error occurs while refreshing a cached response, use it
  if it expired by no more than this many seconds ago
- `If-None-Match`: Automatically added for revalidation, if an `ETag` is available
- `If-Modified-Since`: Automatically added for revalidation, if `Last-Modified` is available

**Response headers:**
- `Cache-Control: immutable`: Cache the response with no expiration
- `Cache-Control: max-age`: Used as the expiration time in seconds
- `Cache-Control: must-revalidate`: When used in combination with `max-age=0`, revalidate immediately.
- `Cache-Control: no-cache`: Revalidate with the server before using a cached response
- `Cache-Control: no-store` Skip writing to the cache
- `Cache-Control: stale-if-error`: Same behavior as request header
- `Cache-Control: stale-while-revalidate`: If expired by less than this many seconds, return the stale response immediately and send an asynchronous revalidation request
- `Expires`: Used as an absolute expiration datetime
- `ETag`: Validator used for conditional requests
- `Last-Modified`: Validator used for conditional requests
- `Vary`: Used to indicate which request headers to match. See {ref}`matching-headers` for details.
