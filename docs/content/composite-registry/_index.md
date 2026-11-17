---
title: Composite registry
weight: 9
---

A `CompositeRegistry` publishes several independent registries through one
read-only view, giving each a stable namespace. This is useful when a service
aggregates metrics from third-party libraries or sub-systems that already
maintain their own `CollectorRegistry`.

```python
from prometheus_client import CollectorRegistry, CompositeRegistry, Counter

billing = CollectorRegistry()
Counter('invoices_total', 'Invoices issued', registry=billing).inc(3)

search = CollectorRegistry()
Counter('queries_total', 'Queries served', registry=search).inc(11)

composite = CompositeRegistry([
    (billing, 'billing'),
    (search, 'search'),
])
```

Collecting the composite yields `billing_invoices_total` and
`search_queries_total`. The wrapped registries are never modified: collecting
`billing` still yields `invoices_total`.

## What gets renamed

Only metric names are rewritten. For every family the view:

* prefixes the family name, e.g. `latency` becomes `svc_latency`;
* prefixes every derived sample name, so `_bucket`, `_sum`, `_count`,
  `_created`, `_total`, `_info`, `_gsum` and `_gcount` stay consistent with
  their family;
* keeps `documentation`, `type` and `unit` unchanged, so HELP, TYPE and UNIT
  lines describe the same family under its new name.

Label names and label values are data, not metric names, and are copied
verbatim. This includes exemplar label values: an exemplar value that happens
to look like a metric name is never rewritten.

## Conflicts

Namespaces are checked against each other before any scrape has to fail. A
clash raises `NamespaceConflict`, a subclass of `DuplicateTimeseries` (and
therefore of `ValueError`):

```python
from prometheus_client import CompositeRegistry, NamespaceConflict

# 'a_b' + 'c' and 'a' + 'b_c' both publish a_b_c
try:
    CompositeRegistry([(left, 'a_b'), (right, 'a')])
except NamespaceConflict as exc:
    print(exc.duplicates)     # {'a_b_c'}
    print(exc.namespaces)     # {'a_b_c': ('a', 'a_b')}
```

Conflicts are detected on family names and on every derived sample name. A
histogram in namespace `x` named `m` reserves `x_m_bucket`, so a gauge named
`bucket` in namespace `x_m` collides even though no HELP line clashes.

Sources that implement `describe()` are checked when the registry is added.
Sources that only implement `collect()` can only be inspected by collecting;
in that case `collect()` and `check()` raise before yielding the first metric,
so a scrape never observes a half-valid view.

## Restricted view

`CompositeRegistry.restricted_registry(names)` selects samples by their
**public view names** - the namespaced names a scrape would expose - not by
the names of the wrapped registries:

```python
from prometheus_client import generate_latest

generate_latest(composite.restricted_registry(['svc_latency_sum']))
```

A family from which no sample is selected is dropped together with its
metadata. Namespace conflicts are still reported for restricted views.

## Repeated collection

Each `collect()` builds fresh copies. Collecting the view repeatedly therefore
publishes the same names every time instead of accumulating prefixes, and it
never mutates the wrapped registries or the labels of their samples.
