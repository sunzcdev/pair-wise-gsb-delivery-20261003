---
title: Restricted registry
weight: 7
---

Registries support restriction to only return specific metrics.
If you’re using the built-in HTTP server, you can use the GET parameter "name[]", since it’s an array it can be used multiple times.
If you’re directly using `generate_latest`, you can use the function `restricted_registry()`.

```python
curl --get --data-urlencode "name[]=python_gc_objects_collected_total" --data-urlencode "name[]=python_info" http://127.0.0.1:9200/metrics
```

```python
from prometheus_client import generate_latest

generate_latest(REGISTRY.restricted_registry(['python_gc_objects_collected_total', 'python_info']))
```

```python
# HELP python_info Python platform information
# TYPE python_info gauge
python_info{implementation="CPython",major="3",minor="9",patchlevel="3",version="3.9.3"} 1.0
# HELP python_gc_objects_collected_total Objects collected during gc
# TYPE python_gc_objects_collected_total counter
python_gc_objects_collected_total{generation="0"} 73129.0
python_gc_objects_collected_total{generation="1"} 8594.0
python_gc_objects_collected_total{generation="2"} 296.0
```

## Sample selection and family selection

There are two selection modes, and they are declared separately:

* `names` selects **public sample names**. A sample is kept only when its own
  name matches exactly, so selecting `hh_sum` never pulls in a different
  metric such as `hh_summary`.
* `families` selects **whole metric families** by family name. Every sample
  the family currently exposes is kept, so a composite family such as a
  Histogram stays complete and interpretable. The family name is the base
  name of the metric, i.e. without any derived sample suffix (`_bucket`,
  `_sum`, `_count`, `_total`, `_created`, ...).

```python
from prometheus_client import generate_latest

# Only the _sum sample of the histogram, with its family metadata.
generate_latest(REGISTRY.restricted_registry(['hh_sum']))

# The whole histogram: buckets, count, sum and any _created sample.
generate_latest(REGISTRY.restricted_registry(families=['hh']))
```

Both modes can be combined. Over HTTP the same distinction is available as
the `name[]` and `family[]` query parameters, and the selection is applied
before encoding, so the text and OpenMetrics outputs always obey the same
choice:

```python
curl --get --data-urlencode "family[]=hh" http://127.0.0.1:9200/metrics
```

The `HELP`, `TYPE` and `UNIT` metadata of a selected family is preserved even
when only a single sample is selected. A family from which no sample is
selected is dropped entirely, so exposition never contains metadata without
any sample behind it.

Each collector runs at most once per `collect()`, no matter how many
selectors resolve to it.

## target_info

`target_info` is not produced by a collector, so its acceptance rule is
declared explicitly. The metadata family is exposed only while target info is
configured and only when it is selected by its public sample name as a sample
selector (`names=['target_info']`) or by its family name as a family selector
(`families=['target']`). The two modes remain distinct: `names=['target']`
and `families=['target_info']` do not expose it.