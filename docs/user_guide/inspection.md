(inspection)=
# {fas}`search` Cache Inspection
Here are some ways to get additional information out of the cache session, backend, and responses:

(cache-plan)=
## Planning Cache Behavior
Use {py:meth}`.CachedSession.cache_plan` to find out what a request **would** do, without sending
it. This is a side-effect-free decision entry point: it never sends a request, never modifies the
given {py:class}`~requests.PreparedRequest`, and never adds, updates, or deletes cached responses.

It returns a {py:class}`.CachePlan` that indicates whether each cache operation is allowed:
- `read_cache`: Reading a response from the cache is allowed
- `contact_origin`: Contacting the origin server is allowed (including for redirects and
  background refreshes)
- `write_cache`: Writing a response to the cache is allowed

It also describes what will happen given the current cache contents (`send_request`,
`resend_request`, `resend_async`, `error_504`), and includes human-readable `reasons` for each
decision. See {ref}`option-precedence` for the rules used to combine `only_if_cached`, `refresh`,
`force_refresh`, and `Cache-Control` request headers.

```python
>>> from requests import Request
>>> from requests_cache import CachedSession

>>> session = CachedSession()
>>> request = session.prepare_request(Request('GET', 'https://httpbin.org/get'))

>>> plan = session.cache_plan(request, only_if_cached=True)
>>> print(plan.read_cache, plan.contact_origin, plan.write_cache)
True False True
>>> print(plan.error_504)  # Not cached, and contacting the origin server is forbidden
True
>>> print(*plan.reasons, sep='\n')
```

Contradictory option combinations (for example `only_if_cached=True` with `force_refresh=True`)
are rejected with {py:class}`.CachePolicyError`, which is distinct from
{py:exc}`requests.RequestException` so decision errors can be told apart from real network
failures:
```python
>>> from requests_cache import CachePolicyError
>>> try:
...     plan = session.cache_plan(request, only_if_cached=True, force_refresh=True)
... except CachePolicyError as e:
...     print(e)
Conflicting cache options: 'only-if-cached' and 'no-cache' (force_refresh). ...
```

## Response Details
The following attributes are available on responses:
- `from_cache`: indicates if the response came from the cache
- `has_content_changed`: `True` for a changed synchronous refresh, `False` for an unchanged
  synchronous refresh or HTTP 304 revalidation, and `None` when no comparison was made. See
  {py:meth}`.OriginalResponse.update_content_changed` for comparison rules and limitations.
- `cache_key`: The unique identifier used to match the request to the response (see {ref}`matching`
  for details)
- `created_at`: {py:class}`~datetime.datetime` of when the cached response was created or last updated
- `expires`: {py:class}`~datetime.datetime` after which the cached response will expire (see
  {ref}`expiration` for details)
- `is_expired`: indicates if the cached response is expired (if, for example, an old response was returned due to a request error)

:::{dropdown} Examples
:animate: fade-in-slide-down
:color: primary
:icon: file-code

```python
>>> from requests_cache import CachedSession
>>> session = CachedSession(expire_after=timedelta(days=1))

>>> # Placeholder attributes are added for non-cached responses
>>> response = session.get('https://httpbin.org/get')
>>> print(response.from_cache, response.created_at, response.expires, response.is_expired)
False None None None

>>> # These attributes will be populated for cached responses
>>> response = session.get('https://httpbin.org/get')
>>> print(response.from_cache, response.created_at, response.expires, response.is_expired)
True 2021-01-01 18:00:00 2021-01-02 18:00:00 False

>>> # Print a response object to get general information about it
>>> print(response)
'request: GET https://httpbin.org/get, response: 200 (308 bytes), created: 2024-01-01 22:45:00 EST, expires: 2024-01-02 18:45:00 EST (fresh)'
```
:::

## Cache Contents

### Checking for responses
Use {py:meth}`.BaseCache.contains` to check if a given request is cached.

Check if a specific URL is cached:
```python
>>> print(session.cache.contains(url='https://httpbin.org/get'))
```

To match additional request values (parameters, headers, etc), you can pass a
{py:class}`~requests.models.Request` object instead:
```python
>>> from requests import Request

>>> request = Request('GET', 'https://httpbin.org/get', params={'k': 'v'})
>>> print(session.cache.contains(request=request))
```

You can also check for a specific cache key:
```python
>>> print(session.cache.contains('d1e666e9fdfb3f86'))
```

### Filtering responses
Use {py:meth}`.BaseCache.filter` to get responses with optional filters. By default, it returns all
responses except any invalid ones that would raise an exception:
```python
>>> for response in session.cache.filter():
>>>     print(response)
```

Get unexpired responses:
```python
>>> for response in session.cache.filter(expired=False):
>>>     print(response)
```

Get keys for **only** expired responses:
```python
>>> expired_responses = session.cache.filter(valid=False, expired=True)
>>> keys = [response.cache_key for response in expired_responses]
```

### Deleting responses
Use {py:meth}`.BaseCache.delete` to manually delete responses. See {ref}`manual_removal` for
examples.

### Response URLs
You can use {py:meth}`.BaseCache.urls` to see all URLs currently in the cache:
```python
>>> session = CachedSession()
>>> print(session.cache.urls())
['https://httpbin.org/get', 'https://httpbin.org/stream/100']
```

### Other response details
If needed, you can access all responses via `CachedSession.cache.responses`, which is a dict-like
interface to the cache backend, where:
* Keys are cache keys (a hash of matched request information)
* Values are {py:class}`.CachedResponse` objects

For example, if you wanted to see URLs only for `POST` requests:
```python
>>> post_urls = [
...     response.url for response in session.cache.responses.values()
...     if response.request.method == 'POST'
... ]
```
