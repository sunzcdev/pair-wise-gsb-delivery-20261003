---
title: Multiprocess Mode
weight: 5
---

Prometheus client libraries presume a threaded model, where metrics are shared
across workers. This doesn't work so well for languages such as Python where
it's common to have processes rather than threads to handle large workloads.

To handle this the client library can be put in multiprocess mode.
This comes with a number of limitations:

- Registries can not be used as normal:
  - all instantiated metrics are collected
  - Registering metrics to a registry later used by a `MultiProcessCollector`
    may cause duplicate metrics to be exported
  - Filtering on metrics works if and only if the constructor was called with
    `support_collectors_without_names=True` and it but might be inefficient.
- Custom collectors do not work (e.g. cpu and memory metrics)
- Gauges cannot use `set_function`
- Info and Enum metrics do not work
- The pushgateway cannot be used
- Gauges cannot use the `pid` label
- Exemplars are not supported
- Remove and Clear of labels are currently not supported in multiprocess mode.

There's several steps to getting this working:

**1. Deployment**:

The `PROMETHEUS_MULTIPROC_DIR` environment variable must be set to a directory
that the client library can use for metrics. This directory must be wiped
between process/Gunicorn runs (before startup is recommended).

This environment variable should be set from a start-up shell script,
and not directly from Python (otherwise it may not propagate to child processes).

Note: on Windows Subsystem for Linux (WSL), set `PROMETHEUS_MULTIPROC_DIR` to a
Linux-native filesystem path (e.g. `/tmp` or `/home/<user>`) rather than a
Windows-mounted path (e.g. `/mnt/c/...`). On Windows-mounted filesystems the
per-process metric files can be written with an incorrect internal offset,
causing the collector to silently read no data.

**2. Metrics collector**:

The application must initialize a new `CollectorRegistry`, and store the
multi-process collector inside. It is a best practice to create this registry
inside the context of a request to avoid metrics registering themselves to a
collector used by a `MultiProcessCollector`. If a registry with metrics
registered is used by a `MultiProcessCollector` duplicate metrics may be
exported, one for multiprocess, and one for the process serving the request.

```python
from prometheus_client import multiprocess
from prometheus_client import generate_latest, CollectorRegistry, CONTENT_TYPE_LATEST, Counter

MY_COUNTER = Counter('my_counter', 'Description of my counter')

# Expose metrics.
def app(environ, start_response):
    registry = CollectorRegistry(support_collectors_without_names=True)
    multiprocess.MultiProcessCollector(registry)
    data = generate_latest(registry)
    status = '200 OK'
    response_headers = [
        ('Content-type', CONTENT_TYPE_LATEST),
        ('Content-Length', str(len(data)))
    ]
    start_response(status, response_headers)
    return iter([data])
```

**3. Gunicorn configuration**:

The `gunicorn` configuration file needs to include the following function:

```python
from prometheus_client import multiprocess

def child_exit(server, worker):
    multiprocess.mark_process_dead(worker.pid)
```

**4. Metrics tuning (Gauge)**:

When `Gauge`s are used in multiprocess applications,
you must decide how to handle the metrics reported by each process.
Gauges have several modes they can run in, which can be selected with the `multiprocess_mode` parameter.

- 'all': Default. Return a timeseries per process (alive or dead), labelled by the process's `pid` (the label is added internally).
- 'min': Return a single timeseries that is the minimum of the values of all processes (alive or dead).
- 'max': Return a single timeseries that is the maximum of the values of all processes (alive or dead).
- 'sum': Return a single timeseries that is the sum of the values of all processes (alive or dead).
- 'mostrecent': Return a single timeseries that is the most recent value among all processes (alive or dead).

Prepend 'live' to the beginning of the mode to return the same result but only considering living processes
(e.g., 'liveall, 'livesum', 'livemax', 'livemin', 'livemostrecent').

```python
from prometheus_client import Gauge

# Example gauge
IN_PROGRESS = Gauge("inprogress_requests", "help", multiprocess_mode='livesum')
```

## API Reference

### `MultiProcessCollector(registry, path=None)`

Collector that aggregates metrics written by all processes in the multiprocess directory.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `registry` | `CollectorRegistry` | required | Registry to register with. Pass a registry created inside the request context to avoid duplicate metrics. |
| `path` | `Optional[str]` | `None` | Path to the directory containing the per-process metric files. Defaults to the `PROMETHEUS_MULTIPROC_DIR` environment variable. |

Raises `ValueError` if `path` is not set or does not point to an existing directory.

```python
from prometheus_client import multiprocess, CollectorRegistry

def app(environ, start_response):
    registry = CollectorRegistry(support_collectors_without_names=True)
    multiprocess.MultiProcessCollector(registry)
    ...
```

To use a custom path instead of the environment variable:

```python
collector = multiprocess.MultiProcessCollector(registry, path='/var/run/prom')
```

### `mark_process_dead(pid, path=None)`

Removes the per-process metric files for a dead process. Call this from your process manager
when a worker exits to prevent stale `live*` gauge values from accumulating.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `pid` | `int` | required | PID of the process that has exited. |
| `path` | `Optional[str]` | `None` | Path to the multiprocess directory. Defaults to the `PROMETHEUS_MULTIPROC_DIR` environment variable. |

Returns `None`. Only removes files for `live*` gauge modes (e.g. `livesum`, `liveall`); files
for non-live modes are left in place so their last values remain visible until the directory is
wiped on restart.

Counters, histograms and summaries are never removed by this function: their values are
cumulative and a dead process's contribution must keep being added to the aggregated totals.
Files whose names do not follow the multi-process metric file protocol are ignored as well.
Deletions are executed through a cleanup plan (see below), so a file that was replaced between
planning and execution — for example because its pid was reused — is skipped rather than
deleted.

```python
# Gunicorn config
from prometheus_client import multiprocess

def child_exit(server, worker):
    multiprocess.mark_process_dead(worker.pid)
```

### Reviewable cleanup: `build_cleanup_plan` / `execute_cleanup_plan`

When you want to inspect what would be removed before removing it, build a `CleanupPlan`
first. Building a plan never touches the filesystem; the plan lists every file in the
directory together with its disposition and reason:

- `to_delete`: `live*` gauge files whose process is no longer running.
- `kept`: everything that must remain, with the reason:
  - cumulative metrics (`counter`, `histogram`, `summary`) are retained for aggregation,
  - non-live gauges retain their last value for a dead process,
  - files belonging to a pid outside the requested cleanup are left alone,
  - `live*` gauges of a still running process are left alone.
- `unrecognized`: entries that do not follow the metric file protocol (unknown metric type or
  gauge mode, non-numeric pid, missing `.db` suffix, non-regular files). They are reported for
  review and never deleted.

`plan.summary()` renders the plan as human-readable text for auditing.

```python
from prometheus_client import multiprocess

# Review first.
plan = multiprocess.build_cleanup_plan(pid=worker.pid)
print(plan.summary())

# Then execute, or hand the plan to whoever approves the cleanup.
result = multiprocess.execute_cleanup_plan(plan)
print('deleted:', len(result.deleted), 'skipped:', len(result.skipped))
```

| Function / parameter | Type | Default | Description |
|----------------------|------|---------|-------------|
| `build_cleanup_plan(pid, path, process_is_alive)` | | | Returns a `CleanupPlan` without deleting anything. |
| `pid` | `Optional[int]` | `None` | Only consider this pid; `None` considers every process in the directory. |
| `path` | `Optional[str]` | `None` | Multiprocess directory; defaults to `PROMETHEUS_MULTIPROC_DIR`. |
| `process_is_alive` | `Callable[[int], bool]` | `default_process_is_alive` | Liveness callback taking a pid. Inject a fake callback in tests, or your process manager's own liveness knowledge in production. |
| `execute_cleanup_plan(plan, process_is_alive)` | | | Performs the planned deletions and returns a `CleanupResult` of `deleted` and `skipped` entries. |

Every deletion is re-validated at execution time against the state captured in the plan:
the file must still exist, must have the same fingerprint (device/inode/size/mtime), must still
parse to the same protocol entry, and the pid must still be dead. If anything changed — the
file was replaced or the pid was reused — the entry is reported in `result.skipped` and kept
on disk instead of being removed. A stale plan can therefore never delete a file belonging to
a new process.

`default_process_is_alive(pid)` probes liveness portably: signal zero on POSIX and a limited
`OpenProcess` query on Windows. It is safe to call natively on Windows without spawning
subprocesses.

```python
# Clean up every dead worker in one auditable pass.
plan = multiprocess.build_cleanup_plan()  # pid=None considers all pids
for entry in plan.to_delete:
    ...  # custom approval/logging before execution
multiprocess.execute_cleanup_plan(plan)
```
