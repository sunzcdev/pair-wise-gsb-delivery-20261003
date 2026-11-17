---
title: Bucket projection
weight: 8
---

Different collection endpoints sometimes need a coarser view of the same
Histogram - for example, an edge aggregator that only retains a few wide
buckets. Bucket projection produces that view **without modifying the source
metric**: it creates a new histogram metric family that keeps only a subset of
the original cumulative bucket boundaries.

The retained boundaries must be an **ordered subset** of the boundaries already
present on the source histogram. Projection never invents data:

- Each retained bucket keeps its original *cumulative* count; counts are never
  recomputed or interpolated to fabricate the contents of a merged interval.
- The `_count`, `_sum` and `+Inf` samples are preserved, as is the cumulative
  meaning of every bucket.
- Labels are preserved per labelled series.
- Exemplars are kept only when the bucket they are attached to is itself
  retained, so an exemplar is only ever emitted for a bucket with an
  unambiguous membership.
- NaN boundaries, duplicate boundaries and boundaries that are missing from the
  source are rejected with a `ValueError` before any output is produced.

## Projecting a single metric family

`project_histogram_buckets(metric, buckets)` takes a histogram
`Metric` (for example one returned by `collect()`) and returns a new, projected
metric. The input metric and its samples are left untouched.

```python
from prometheus_client import Histogram, CollectorRegistry
from prometheus_client import project_histogram_buckets

registry = CollectorRegistry()
h = Histogram('request_latency_seconds', 'RPC latency',
              buckets=(0.1, 0.5, 1.0, 2.5, 5.0), registry=registry)
for value in (0.05, 0.4, 0.9, 2.0, 4.5, 8.0):
    h.observe(value)

source = next(registry.collect())
# Keep only the 0.5 and 2.5 boundaries; +Inf is always retained.
projected = project_histogram_buckets(source, (0.5, 2.5))
```

The projected family only contains the retained buckets, while `_count`,
`_sum` and `+Inf` are unchanged:

```
request_latency_seconds_bucket{le="0.5"} 2.0
request_latency_seconds_bucket{le="2.5"} 4.0
request_latency_seconds_bucket{le="+Inf"} 6.0
request_latency_seconds_count 6.0
request_latency_seconds_sum 15.85
```

`+Inf` may be included in `buckets` and is appended automatically when omitted,
so the total count (carried by the `+Inf` bucket) is always available. Asking
for a boundary that does not exist on the source raises an error instead of
guessing an interval count:

```python
project_histogram_buckets(source, (0.5, 0.75))  # ValueError: 0.75 is not a source boundary
```

## Serving different endpoints from one set of instruments

`BucketProjectionCollector` wraps another collector (such as a registry) and
projects every histogram it produces while passing all other metric types
through unchanged. This lets several endpoints share one underlying registry,
each exposing a different bucket granularity.

```python
from prometheus_client import (
    CollectorRegistry, Histogram, BucketProjectionCollector,
)
from prometheus_client.exposition import generate_latest

registry = CollectorRegistry()
Histogram('request_latency_seconds', 'RPC latency',
          buckets=(0.1, 0.5, 1.0, 2.5, 5.0), registry=registry)

# A coarse-grained endpoint that only retains the 0.5 and 2.5 boundaries.
coarse_registry = CollectorRegistry()
coarse_registry.register(BucketProjectionCollector(registry, (0.5, 2.5)))

print(generate_latest(coarse_registry).decode())
```

The wrapped registry is never modified: scraping `coarse_registry` has no effect
on the buckets exposed by the original `registry`. Both the Prometheus text
format and the OpenMetrics text format render the projected family, and the
output can be parsed back into a valid histogram metric family.
