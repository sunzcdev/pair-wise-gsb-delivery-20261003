---
title: Histogram
weight: 4
---

A Histogram samples observations and counts them in configurable buckets. Use it
when you want to track distributions — request latency, response sizes — and need
to calculate quantiles (p50, p95, p99) in your queries.

```python
from prometheus_client import Histogram
h = Histogram('request_latency_seconds', 'Description of histogram')
h.observe(4.7)    # Observe 4.7 (seconds in this case)
```

A Histogram exposes three time series per metric:
- `<name>_bucket{le="<bound>"}` — count of observations with value ≤ le (cumulative)
- `<name>_sum` — sum of all observed values
- `<name>_count` — total number of observations

If the lowest bucket bound is negative, OpenMetrics 1.0 output leaves out `_sum`
and `_count`, because OpenMetrics 1.0 does not allow a sum for histograms with
negative buckets and only allows `_count` alongside a sum. The `+Inf` bucket
still has the count. Other formats, including OpenMetrics 2.0, keep both.

## Constructor

```python
Histogram(name, documentation, labelnames=(), namespace='', subsystem='', unit='', registry=REGISTRY, buckets=DEFAULT_BUCKETS)
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `name` | `str` | required | Metric name. |
| `documentation` | `str` | required | Help text shown in the `/metrics` output and Prometheus UI. |
| `labelnames` | `Iterable[str]` | `()` | Names of labels for this metric. See [Labels](../labels/). Note: `le` is reserved and cannot be used as a label name. |
| `namespace` | `str` | `''` | Optional prefix. |
| `subsystem` | `str` | `''` | Optional middle component. |
| `unit` | `str` | `''` | Optional unit suffix appended to the metric name. |
| `registry` | `CollectorRegistry` | `REGISTRY` | Registry to register with. Pass `None` to skip registration, which is useful in tests where you create metrics without wanting them in the global registry. |
| `buckets` | `Sequence[float]` | `DEFAULT_BUCKETS` | Upper bounds of the histogram buckets. Must be in ascending order. `+Inf` is always appended automatically. |

`namespace`, `subsystem`, and `name` are joined with underscores to form the full metric name:

```python
# namespace='myapp', subsystem='http', name='request_duration_seconds'
# produces: myapp_http_request_duration_seconds
Histogram('request_duration_seconds', 'Latency', namespace='myapp', subsystem='http')
```

Default buckets are intended to cover typical web/RPC request latency in seconds and are
accessible as `Histogram.DEFAULT_BUCKETS`:

```
.005, .01, .025, .05, .075, .1, .25, .5, .75, 1.0, 2.5, 5.0, 7.5, 10.0, +Inf
```

To override with buckets tuned to your workload:

```python
h = Histogram('request_latency_seconds', 'Latency', buckets=[.1, .5, 1, 2, 5])
```

## Methods

### `observe(amount, exemplar=None)`

Record a single observation. The amount is typically positive or zero.

```python
h.observe(0.43)   # observe 430ms
```

To attach trace context to an observation, pass an `exemplar` dict. Exemplars are
only rendered in OpenMetrics format. See [Exemplars](../exemplars/) for details.

```python
h.observe(0.43, exemplar={'trace_id': 'abc123'})
```

### `time()`

Observe the duration in seconds of a block of code or function and add it to the
histogram. Every call accumulates — unlike `Gauge.time()`, which only keeps the
most recent duration. Can be used as a decorator or context manager.

```python
@h.time()
def process():
    pass

with h.time():
    pass

with h.time() as t:
    pass
print(t.duration) # observed time in seconds.
```

## Labels

See [Labels](../labels/) for how to use `.labels()`, `.remove()`, `.remove_by_labels()`, and `.clear()`.

## Real-world example

Tracking HTTP request latency with custom buckets tuned to the workload:

```python
from prometheus_client import Histogram, start_http_server

REQUEST_LATENCY = Histogram(
    'request_duration_seconds',
    'HTTP request latency',
    labelnames=['method', 'endpoint'],
    namespace='myapp',
    buckets=[.01, .05, .1, .25, .5, 1, 2.5, 5],
)

def handle_request(method, endpoint):
    with REQUEST_LATENCY.labels(method=method, endpoint=endpoint).time():
        # ... handle the request ...
        pass

if __name__ == '__main__':
    start_http_server(8000)  # exposes metrics at http://localhost:8000/metrics
    # ... start your application ...
```

This produces time series like:
```
myapp_request_duration_seconds_bucket{method="GET",endpoint="/api/users",le="0.1"} 42
myapp_request_duration_seconds_sum{method="GET",endpoint="/api/users"} 3.7
myapp_request_duration_seconds_count{method="GET",endpoint="/api/users"} 50
```


## Native histograms

A `Histogram` with `native_histogram=True` (or a `NativeHistogram` sample you
build yourself) stores its buckets as spans and deltas: compact run-length
descriptions of which buckets are populated and how their populations change.
Combining such histograms from several sources requires them to agree on a
schema and a zero bucket threshold, which is what
`prometheus_client.samples.merge_native_histograms` does.

A native histogram's `schema` sets the bucket layout. Schema `n` splits every
power of two into `2 ** n` buckets, so bigger schemas are finer; the supported
range is `-4` to `8`. Two histograms with different schemas can be merged by
reducing the finer one to the coarser schema, which only widens buckets and
therefore never loses an observation.

```python
from prometheus_client.samples import (
    BucketSpan, NativeHistogram, merge_native_histograms,
    native_histogram_bucket_total,
)

# Two observations in schema 2, in buckets 1 and 2.
fine = NativeHistogram(
    count_value=2, sum_value=3.0, schema=2,
    zero_threshold=0.0, zero_count=0.0,
    pos_spans=(BucketSpan(1, 2),), pos_deltas=(1, 0),
)
# One observation in schema 0, also in bucket 1.
coarse = NativeHistogram(
    count_value=1, sum_value=1.5, schema=0,
    zero_threshold=0.0, zero_count=0.0,
    pos_spans=(BucketSpan(1, 1),), pos_deltas=(1,),
)

merged = merge_native_histograms(fine, coarse)
assert merged.schema == 0
assert merged.count_value == 3
assert merged.sum_value == 4.5
# Every observation is still accounted for.
assert native_histogram_bucket_total(merged) == 3.0
```

`merge_native_histograms(histograms, schema=..., zero_threshold=...)` accepts a
single histogram, a list of histograms, or the histograms as separate
arguments. The result is a new `NativeHistogram` with canonical spans and
deltas:

- buckets are ordered by index and consecutive populated buckets share a span;
- gaps between populated buckets stay span offsets, and are never expanded into
  empty buckets, so very sparse histograms stay compact;
- buckets whose population is zero are trimmed;
- every delta list starts from zero relative to the first bucket of its side;
- empty spans and negative deltas in the inputs are accepted and re-encoded.

Pass `schema` to choose the result's schema. It must not be finer than the
coarsest input, because resolution can only be reduced; the default is that
coarsest input schema. Use `reduce_native_histogram_resolution(histogram,
schema)` to downscale a single histogram without merging it.

### Zero bucket thresholds

The zero bucket counts the observations within `[-zero_threshold,
+zero_threshold]`. Merging uses the largest threshold of the inputs, because a
zero bucket cannot be split back into regular buckets. If that threshold falls
inside a *populated* bucket of any input, the bucket cannot be split either, so
the threshold is raised to that bucket's upper bound and the check repeats until
the threshold is a bucket boundary or is not inside a populated bucket at all.
Every bucket that ends up inside the new threshold has its population added to
the zero bucket, so still no observation is lost.

Passing a larger `zero_threshold` is allowed and folds more buckets into the
zero bucket. Passing a smaller one raises `NativeHistogramError`, as does
requesting a schema finer than the coarsest input.

### Checking counts

`count_value` and `sum_value` of the result are the sums of the inputs' values.
`native_histogram_bucket_total(histogram)` adds up the zero count and the
absolute population of all regular buckets; comparing it with `count_value`
shows whether a histogram accounts for every observation. Merging only moves
populations between buckets, so the bucket total is preserved.

### Unsupported schemas

Only the standard exponential schemas `-4` to `8` can be merged. Other schemas
raise `UnsupportedSchemaError`, which carries the offending `schema` and a
`reason`:

- `-53` is the custom bucket schema (NHCB). Those histograms describe their
  buckets with custom bounds and have no spans or deltas to combine.
- `-9` to `-5` and `9` to `52` are reserved for future resolutions. Reduce them
  to a supported schema before merging.
- Anything else is unknown.

The inputs are never modified: spans and deltas you pass in are left exactly as
they were, including when a merge fails.
