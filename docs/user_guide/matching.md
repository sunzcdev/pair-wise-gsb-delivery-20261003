(matching)=
# {fas}`equals` Request Matching
Requests are matched according to the request method, URL, parameters and body. All of these values
are normalized to account for any variations that do not modify response content.

There are some additional options to configure how you want requests to be matched.

(filter-params)=
## Selective Parameter Matching
By default, all normalized request parameters are matched. In some cases, there may be request
parameters that you don't want to match. For example, an authentication token will change frequently
but not change response content.

Use the `ignored_parameters` option if you want to ignore specific parameters.

```{note}
Many common authentication parameters are already ignored by default.
See {ref}`default-filter-params` for details.
```

**Request Parameters:**

In this example, only the first request will be sent, and the second request will be a cache hit
due to the ignored parameters:
```python
>>> session = CachedSession(ignored_parameters=['auth-token'])
>>> session.get('https://httpbin.org/get', params={'auth-token': '2F63E5DF4F44'})
>>> r = session.get('https://httpbin.org/get', params={'auth-token': 'D9FAEB3449D3'})
>>> assert r.from_cache is True
```

**Request Body Parameters:**

This also applies to parameters in a JSON-formatted request body:
```python
>>> session = CachedSession(allowable_methods=('GET', 'POST'), ignored_parameters=['auth-token'])
>>> session.post('https://httpbin.org/post', json={'auth-token': '2F63E5DF4F44'})
>>> r = session.post('https://httpbin.org/post', json={'auth-token': 'D9FAEB3449D3'})
>>> assert r.from_cache is True
```

**Request Headers:**

As well as headers, if `match_headers=True` is used:
```python
>>> session = CachedSession(ignored_parameters=['auth-token'], match_headers=True)
>>> session.get('https://httpbin.org/get', headers={'auth-token': '2F63E5DF4F44'})
>>> r = session.get('https://httpbin.org/get', headers={'auth-token': 'D9FAEB3449D3'})
>>> assert r.from_cache is True
```
```{note}
Since `ignored_parameters` is most often used for sensitive info like credentials, these values will also be removed from the cached request parameters, body, and headers.
```

```{tip}
Variations in headers can be introduced by the libraries used by CacheSession.
These variations can be eliminated by calling CachedSession request functions
with a fixed header value. For example, always passing `"Accept-Encoding": "gzip,
deflate"` ensures that additional compression encodings are not added to the request.
```

### JSON Request Bodies
The default body normalization is a best-effort transformation intended to increase cache hits.
For JSON bodies it sorts both object keys and array values, which can conflate requests that are
not actually equivalent. For example, `[1, 2]` and `[2, 1]` would match under that behavior.

If you need strict, unambiguous matching for bodies sent as JSON, use the `json_body_match`
option. When enabled, a body is only treated as JSON when its `Content-Type` declares it and the
body can be fully parsed. Matching then follows these rules:

* Object key order is ignored (the only reordering that is semantically irrelevant)
* Value types are kept distinct, so `1`, `"1"` and `true` never match each other
* Array order is preserved, so `[1, 2]` and `[2, 1]` do not match
* Large integers keep their full precision and are never converted to floating point
* Objects containing duplicate keys are rejected as ambiguous
* The body is decoded using a definite charset (UTF-8 by default, or the declared `charset`)

The normalization is used only to build the cache key; the actual request body sent over the
wire is never modified.

`json_body_match` accepts one of three values:

* `None` (default): keep the legacy best-effort behavior unchanged
* `'fallback'`: use strict JSON matching when the body is fully parseable; otherwise fall back to
  matching the raw body bytes exactly
* `'reject'`: use strict JSON matching, and bypass the cache entirely (neither read nor write)
  when a body declared as JSON cannot be normalized unambiguously

```python
>>> session = CachedSession(
...     allowable_methods=('GET', 'POST'), json_body_match='reject'
... )
>>> # These two requests match because object key order is irrelevant
>>> session.post('https://example.com/items', json={'a': 1, 'b': 2})
>>> session.post('https://example.com/items', json={'b': 2, 'a': 1})
>>> # These do not match, because value types and array order are significant
>>> session.post('https://example.com/items', json={'v': 1})
>>> session.post('https://example.com/items', json={'v': '1'})
```

(matching-headers)=
## Matching Request Headers
```{note}
In some cases, request header values can affect response content. For example, sites that support
i18n and [content negotiation](https://developer.mozilla.org/en-US/docs/Web/HTTP/Content_negotiation) may use the `Accept-Language` header to determine which language to serve content in.

The server will ideally also send a `Vary` header in the response, which informs caches about
which request headers to match. By default, requests-cache respects this: each unique combination
of Vary-specified header values is cached separately, so alternating between variants (e.g.,
different `Accept` values for content negotiation) works correctly without extra configuration.
Not all servers send `Vary`, however.
```

Use the `match_headers` option if you want to specify which headers you want to match when `Vary`
isn't available:
```python
>>> session = CachedSession(match_headers=['Accept'])
>>> # These two requests will be sent and cached separately
>>> session.get('https://httpbin.org/headers', {'Accept': 'text/plain'})
>>> session.get('https://httpbin.org/headers', {'Accept': 'application/json'})
```

If you want to match _all_ request headers, you can use `match_headers=True`.


(custom-matching)=
## Custom Request Matching
If you need more advanced behavior, you can implement your own custom request matching.

### Cache Keys
Request matching is accomplished using a **cache key**, which uniquely identifies a response in the
cache based on request info. For example, the option `ignored_parameters=['foo']` works by excluding
the `foo` request parameter from the cache key, meaning these three requests will all use the same
cached response:
```python
>>> session = CachedSession(ignored_parameters=['foo'])
>>> response_1 = session.get('https://example.com')          # cache miss
>>> response_2 = session.get('https://example.com?foo=bar')  # cache hit
>>> response_3 = session.get('https://example.com?foo=qux')  # cache hit
>>> assert response_2.cache_key == response_3.cache_key
```

### Recreating Cache Keys
There are some situations where request matching behavior may change, which causes previously cached
responses to become obsolete:
* You start using a custom cache key, or change other settings that affect request matching
* A new version of requests-cache is released that includes new or changed request matching behavior
  (typically, most non-patch releases)

In these cases, if you want to keep using your existing cache data, you can use the
`recreate_keys` method:
```python
>>> session = CachedSession()
>>> session.cache.recreate_keys()
```

### Cache Key Functions
If you want to implement your own request matching, you can provide a cache key function which will
take a {py:class}`~requests.PreparedRequest` plus optional keyword args for
{py:func}`~requests.request`, and return a string:
```python
def create_key(request: requests.PreparedRequest, **kwargs) -> str:
    """Generate a custom cache key for the given request"""
```

You can then pass this function via the `key_fn` param:
```python
session = CachedSession(key_fn=create_key)
```

`**kwargs` includes relevant {py:class}`.BaseCache` settings and any other keyword args passed to
{py:meth}`.CachedSession.send`. If you want use a custom matching function _and_ the existing
options `ignored_parameters` and `match_headers`, you can implement them in `key_fn`:
```python
def create_key(
    request: requests.PreparedRequest,
    ignored_parameters: list[str] = None,
    match_headers: list[str] = None,
    **kwargs,
) -> str:
    """Generate a custom cache key for the given request"""
```

Reference:
* See {py:func}`.create_key` for the reference implementation.
* See the rest of the {py:mod}`.cache_keys` module for some useful helper functions.
* See {ref}`Examples<custom_keys>` for a complete example of custom request matching.


```{tip}
As a general rule, if you include less information in your cache keys, you will have more cache hits
and use less storage space, but risk getting incorrect response data back.
```
```{warning}
If you provide a custom key function for a non-empty cache, any responses previously cached with a
different key function will be unused, so it's recommended to clear the cache first.
```

### Custom Header Normalization
When matching request headers (using `match_headers` or `Vary`), requests-cache will normalize minor
header variations like order, casing, whitespace, etc. In some cases, you may be able to further
optimize your requests with some additional header normalization.

For example, let's say you're working with a site that supports content negotiation using the
`Accept-Encoding` header, and the only variation you care about is whether you requested gzip
encoding. This example will increase cache hits by ignoring variations you don't care about:
```python
from requests import PreparedRequest
from requests_cache import CachedSession, create_key


def create_custom_key(request: PreparedRequest, **kwargs) -> str:
    # Don't modify the original request that's about to be sent
    request = request.copy()

    # Simplify values like `Accept-Encoding: gzip, compress, br` to just `Accept-Encoding: gzip`
    if 'gzip' in request.headers.get('Accept-Encoding', ''):
        request.headers['Accept-Encoding'] = 'gzip'
    else:
        request.headers['Accept-Encoding'] = None

    # Use the default key function to do the rest of the work
    return create_key(request, **kwargs)


# Provide your custom request matcher when creating the session
session = CachedSession(key_fn=create_custom_key)
```
