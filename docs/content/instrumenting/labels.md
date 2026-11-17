---
title: Labels
weight: 7
---

All metrics can have labels, allowing grouping of related time series.

See the best practices on [naming](https://prometheus.io/docs/practices/naming/)
and [labels](https://prometheus.io/docs/practices/instrumentation/#use-labels).

Taking a counter as an example:

```python
from prometheus_client import Counter
c = Counter('my_requests_total', 'HTTP Failures', ['method', 'endpoint'])
c.labels('get', '/').inc()
c.labels('post', '/submit').inc()
```

Labels can also be passed as keyword-arguments:

```python
from prometheus_client import Counter
c = Counter('my_requests_total', 'HTTP Failures', ['method', 'endpoint'])
c.labels(method='get', endpoint='/').inc()
c.labels(method='post', endpoint='/submit').inc()
```

Metrics with labels are not initialized when declared, because the client can't
know what values the label can have. It is recommended to initialize the label
values by calling the `.labels()` method alone:

```python
from prometheus_client import Counter
c = Counter('my_requests_total', 'HTTP Failures', ['method', 'endpoint'])
c.labels('get', '/')
c.labels('post', '/submit')
```

## Removing labelsets

### `remove(*labelvalues)`

Remove a specific labelset from the metric. Values must be passed in the same
order as `labelnames` were declared.

```python
c = Counter('my_requests_total', 'HTTP Failures', ['method', 'endpoint'])
c.labels('get', '/').inc()
c.remove('get', '/')
```

### `remove_by_labels(labels)`

Remove all labelsets that partially match the given dict of label names and values.

```python
c.remove_by_labels({'method': 'get'})  # removes all labelsets where method='get'
```

### `clear()`

Remove all labelsets from the metric at once.

```python
c.clear()
```

## Limiting label cardinality

User-supplied label values (for example a user ID or URL) can make a metric
create an unbounded number of children. Pass `label_limit` to cap how many
distinct ordinary labelsets a metric keeps. The limit is optional and applies
per metric in the current process.

```python
from prometheus_client import Counter

# Keep at most 100 distinct (method, endpoint) combinations.
c = Counter(
    'my_requests_total', 'HTTP requests',
    ['method', 'endpoint'], label_limit=100,
)
```

### What happens when the limit is reached

Labelsets that already exist always remain usable — the limit only affects
*new* combinations. When another distinct labelset would exceed the limit:

- without `overflow_labels`, the `.labels()` call raises
  `LabelLimitExceededError`;
- with `overflow_labels`, the call instead returns a single fixed "overflow"
  child shared by every combination that does not fit.

```python
c = Counter(
    'my_requests_total', 'HTTP requests', ['user'],
    label_limit=100,
    # A sequence gives values in labelname order; a mapping is also accepted.
    overflow_labels=('overflow-user',),
)

c.labels('alice')   # ordinary child, counts against the 100 slots
c.labels('bob')     # ordinary child, counts against the 100 slots
# ... once 100 ordinary combinations exist:
c.labels('carol')   # returns the shared overflow child instead
```

The overflow labelset is created lazily and does **not** consume one of the
`label_limit` slots, so a metric exposes at most `label_limit + 1` children
when overflow is in use. Calling `.labels()` with the overflow values directly
returns that same child.

### Quota lifecycle

Concurrent first-time calls for the same labelset create exactly one child and
consume one slot; concurrent calls for different labelsets never push the
number of ordinary children past `label_limit`.

`remove()`, `remove_by_labels()` and `clear()` release the slots held by the
deleted labelsets, so new combinations can be created again. If the overflow
child itself is removed, it is recreated on the next overflow.

A child object obtained before its labelset is removed keeps working as an
independent object, but it is no longer exported and is never returned by
later `.labels()` calls; the next `.labels()` call for that labelset returns a
fresh child (which is itself subject to the limit).

### Derived samples share one identity

The limit counts label combinations, not exposed time series. A Counter's
`_total`/`_created` samples, a Histogram's bucket/`_count`/`_sum` samples and a
Gauge's sample all derive from a single child, so each labelset consumes
exactly one slot regardless of how many samples it produces.

`label_limit` and `overflow_labels` are available on `Counter`, `Gauge`,
`Summary`, `Histogram`, `Info` and `Enum`.