from collections import defaultdict
from dataclasses import dataclass
import errno
import glob
import json
import os
from typing import Callable, Dict, List, Optional, Tuple
import warnings

from .metrics import Gauge
from .metrics_core import Metric
from .mmap_dict import MmapedDict
from .samples import Sample
from .utils import floatToGoString

try:  # Python3
    FileNotFoundError
except NameError:  # Python >= 2.5
    FileNotFoundError = IOError

__all__ = (
    'MultiProcessCollector',
    'mark_process_dead',
    'build_cleanup_plan',
    'execute_cleanup_plan',
    'default_process_is_alive',
    'CleanupPlan',
    'CleanupEntry',
    'MetricFile',
    'FileFingerprint',
    'UnrecognizedFile',
    'CleanupResult',
    'SkippedFile',
)

# File names produced by MultiProcessValue follow one of two protocols:
#   gauge_<multiprocess_mode>_<pid>.db
#   <metric_type>_<pid>.db
# where metric_type is one of the cumulative types below.
_DB_SUFFIX = '.db'
_CUMULATIVE_METRIC_TYPES = frozenset(('counter', 'histogram', 'summary'))
_LIVE_GAUGE_MULTIPROCESS_MODES = frozenset(
    mode for mode in Gauge._MULTIPROC_MODES if mode.startswith('live'))

# Human-readable, stable reasons attached to every entry of a plan so the
# output can be audited before anything is removed.
REASON_LIVE_GAUGE_DEAD_PROCESS = 'live gauge of a dead process'
REASON_LIVE_GAUGE_PROCESS_ALIVE = 'live gauge of a still running process'
REASON_CUMULATIVE_METRIC = 'cumulative metric must be retained for aggregation'
REASON_NONLIVE_GAUGE = 'non-live gauge keeps its last value for a dead process'
REASON_PID_NOT_REQUESTED = 'belongs to a pid not requested for cleanup'


class MultiProcessCollector:
    """Collector for files for multi-process mode."""

    def __init__(self, registry, path=None):
        if path is None:
            # This deprecation warning can go away in a few releases when removing the compatibility
            if 'prometheus_multiproc_dir' in os.environ and 'PROMETHEUS_MULTIPROC_DIR' not in os.environ:
                os.environ['PROMETHEUS_MULTIPROC_DIR'] = os.environ['prometheus_multiproc_dir']
                warnings.warn("prometheus_multiproc_dir variable has been deprecated in favor of the upper case naming PROMETHEUS_MULTIPROC_DIR", DeprecationWarning)
            path = os.environ.get('PROMETHEUS_MULTIPROC_DIR')
        if not path or not os.path.isdir(path):
            raise ValueError('env PROMETHEUS_MULTIPROC_DIR is not set or not a directory')
        self._path = path
        if registry:
            registry.register(self)

    @staticmethod
    def merge(files, accumulate=True):
        """Merge metrics from given mmap files.

        By default, histograms are accumulated, as per prometheus wire format.
        But if writing the merged data back to mmap files, use
        accumulate=False to avoid compound accumulation.
        """
        metrics = MultiProcessCollector._read_metrics(files)
        return MultiProcessCollector._accumulate_metrics(metrics, accumulate)

    @staticmethod
    def _read_metrics(files):
        metrics = {}
        key_cache = {}

        def _parse_key(key):
            val = key_cache.get(key)
            if not val:
                metric_name, name, labels, help_text = json.loads(key)
                labels_key = tuple(sorted(labels.items()))
                val = key_cache[key] = (metric_name, name, labels, labels_key, help_text)
            return val

        for f in files:
            parts = os.path.basename(f).split('_')
            typ = parts[0]
            try:
                file_values = MmapedDict.read_all_values_from_file(f)
            except FileNotFoundError:
                if typ == 'gauge' and parts[1].startswith('live'):
                    # Files for 'live*' gauges can be deleted between the glob of collect
                    # and now (via a mark_process_dead call) so don't fail if
                    # the file is missing
                    continue
                raise
            for key, value, timestamp, _ in file_values:
                metric_name, name, labels, labels_key, help_text = _parse_key(key)

                metric = metrics.get(metric_name)
                if metric is None:
                    metric = Metric(metric_name, help_text, typ)
                    metrics[metric_name] = metric

                if typ == 'gauge':
                    pid = parts[2][:-3]
                    metric._multiprocess_mode = parts[1]
                    metric.add_sample(name, labels_key + (('pid', pid),), value, timestamp)
                else:
                    # The duplicates and labels are fixed in the next for.
                    metric.add_sample(name, labels_key, value)
        return metrics

    @staticmethod
    def _accumulate_metrics(metrics, accumulate):
        for metric in metrics.values():
            samples = defaultdict(lambda: defaultdict(float))
            sample_timestamps = defaultdict(lambda: defaultdict(float))
            buckets = defaultdict(lambda: defaultdict(float))
            for s in metric.samples:
                name, labels, value, timestamp, exemplar, native_histogram_value = s

                if (
                    metric.type == 'gauge'
                    and metric._multiprocess_mode in (
                        'min', 'livemin',
                        'max', 'livemax',
                        'sum', 'livesum',
                        'mostrecent', 'livemostrecent',
                    )
                ):
                    labels = tuple(l for l in labels if l[0] != 'pid')

                if metric.type == 'gauge':
                    if metric._multiprocess_mode in ('min', 'livemin'):
                        current = samples[labels].setdefault((name, labels), value)
                        if value < current:
                            samples[labels][(name, labels)] = value
                    elif metric._multiprocess_mode in ('max', 'livemax'):
                        current = samples[labels].setdefault((name, labels), value)
                        if value > current:
                            samples[labels][(name, labels)] = value
                    elif metric._multiprocess_mode in ('sum', 'livesum'):
                        samples[labels][(name, labels)] += value
                    elif metric._multiprocess_mode in ('mostrecent', 'livemostrecent'):
                        current_timestamp = sample_timestamps[labels][name]
                        timestamp = float(timestamp or 0)
                        if current_timestamp < timestamp:
                            samples[labels][(name, labels)] = value
                            sample_timestamps[labels][name] = timestamp
                    else:  # all/liveall
                        samples[labels][(name, labels)] = value

                elif metric.type == 'histogram':
                    # A for loop with early exit is faster than a genexpr
                    # or a listcomp that ends up building unnecessary things
                    for l in labels:
                        if l[0] == 'le':
                            bucket_value = float(l[1])
                            # _bucket
                            without_le = tuple(l for l in labels if l[0] != 'le')
                            buckets[without_le][bucket_value] += value
                            break
                    else:  # did not find the `le` key
                        # _sum/_count
                        samples[labels][(name, labels)] += value
                else:
                    # Counter and Summary.
                    samples[labels][(name, labels)] += value

            # Accumulate bucket values.
            if metric.type == 'histogram':
                for labels, values in buckets.items():
                    acc = 0.0
                    for bucket, value in sorted(values.items()):
                        sample_key = (
                            metric.name + '_bucket',
                            labels + (('le', floatToGoString(bucket)),),
                        )
                        if accumulate:
                            acc += value
                            samples[labels][sample_key] = acc
                        else:
                            samples[labels][sample_key] = value
                    if accumulate:
                        samples[labels][(metric.name + '_count', labels)] = acc

            # Convert to correct sample format.
            metric.samples = []
            for _, samples_by_labels in samples.items():
                for (name_, labels), value in samples_by_labels.items():
                    metric.samples.append(Sample(name_, dict(labels), value))
        return metrics.values()

    def collect(self):
        files = glob.glob(os.path.join(self._path, '*.db'))
        return self.merge(files, accumulate=True)


def _resolve_multiproc_dir(path):
    """Return the multiprocess directory, falling back to the environment."""
    if path is None:
        path = os.environ.get(
            'PROMETHEUS_MULTIPROC_DIR',
            os.environ.get('prometheus_multiproc_dir'))
    return path


@dataclass(frozen=True)
class FileFingerprint:
    """Identity snapshot of a file, used to detect replacement.

    A file that disappears and is recreated (e.g. because its pid was reused
    by a new process) keeps the same name but gets a different fingerprint.
    """

    device: int
    inode: int
    size: int
    mtime_ns: int

    @classmethod
    def from_path(cls, path):
        """Build a fingerprint from an existing file path."""
        stat_result = os.stat(path)
        return cls(
            stat_result.st_dev,
            stat_result.st_ino,
            stat_result.st_size,
            stat_result.st_mtime_ns)

    def matches(self, path):
        """Return whether the file at path still has this fingerprint."""
        try:
            return self == FileFingerprint.from_path(path)
        except FileNotFoundError:
            return False


@dataclass(frozen=True)
class MetricFile:
    """A file whose name follows the multi-process metric file protocol."""

    path: str
    metric_type: str
    pid: int
    fingerprint: FileFingerprint
    gauge_mode: Optional[str] = None

    @property
    def is_live_gauge(self):
        """Whether this is a gauge file for a live* multiprocess mode."""
        return self.gauge_mode in _LIVE_GAUGE_MULTIPROCESS_MODES

    @property
    def is_cumulative(self):
        """Whether this file holds a monotonic, accumulable metric."""
        return self.metric_type in _CUMULATIVE_METRIC_TYPES


def parse_metric_filename(path):
    """Parse a multi-process metric file name per the naming protocol.

    Returns a MetricFile, or None if the name does not match the protocol
    (wrong suffix, unknown metric type/mode or non-numeric pid).
    """
    name = os.path.basename(path)
    if not name.endswith(_DB_SUFFIX):
        return None
    stem = name[:-len(_DB_SUFFIX)]
    parts = stem.split('_')
    if len(parts) < 2:
        return None
    metric_type = parts[0]
    pid_text = parts[-1]
    if not pid_text or not pid_text.isascii() or not pid_text.isdigit():
        return None
    pid = int(pid_text)
    if metric_type == 'gauge':
        if len(parts) != 3:
            return None
        gauge_mode = parts[1]
        if gauge_mode not in Gauge._MULTIPROC_MODES:
            return None
        return MetricFile(
            path, metric_type, pid,
            FileFingerprint.from_path(path), gauge_mode)
    if metric_type not in _CUMULATIVE_METRIC_TYPES or len(parts) != 2:
        return None
    return MetricFile(
        path, metric_type, pid, FileFingerprint.from_path(path))


@dataclass(frozen=True)
class UnrecognizedFile:
    """A directory entry that does not follow the metric file protocol.

    Such entries are never deleted; they are reported so an operator can
    investigate where they came from.
    """

    path: str
    reason: str


@dataclass(frozen=True)
class CleanupEntry:
    """A protocol-conforming file together with its cleanup disposition."""

    file: MetricFile
    reason: str

    @property
    def delete(self):
        """Whether this entry is planned for removal."""
        return self.reason == REASON_LIVE_GAUGE_DEAD_PROCESS


@dataclass(frozen=True)
class SkippedFile:
    """A planned deletion that was not carried out, with the reason why."""

    entry: CleanupEntry
    reason: str


@dataclass(frozen=True)
class CleanupResult:
    """Outcome of executing a cleanup plan."""

    deleted: Tuple[CleanupEntry, ...] = ()
    skipped: Tuple[SkippedFile, ...] = ()


@dataclass(frozen=True)
class CleanupPlan:
    """Reviewable description of what a cleanup would do.

    to_delete/kept cover every protocol-conforming file in the directory and
    unrecognized covers everything else, so the plan accounts for the whole
    directory without touching any file on its own.
    """

    path: str
    pid: Optional[int]
    to_delete: Tuple[CleanupEntry, ...] = ()
    kept: Tuple[CleanupEntry, ...] = ()
    unrecognized: Tuple[UnrecognizedFile, ...] = ()
    process_is_alive: Optional[Callable[[int], bool]] = None

    def summary(self):
        """Return a human-readable description of the plan for review."""
        scope = 'all pids' if self.pid is None else 'pid {}'.format(self.pid)
        lines = [
            'Cleanup plan for {} ({}):'.format(self.path, scope),
            '  delete ({}):'.format(len(self.to_delete)),
        ]
        lines.extend(
            '    - {} [{}]'.format(entry.file.path, entry.reason)
            for entry in self.to_delete)
        lines.append('  keep ({}):'.format(len(self.kept)))
        lines.extend(
            '    - {} [{}]'.format(entry.file.path, entry.reason)
            for entry in self.kept)
        lines.append('  unrecognized ({}):'.format(len(self.unrecognized)))
        lines.extend(
            '    - {} [{}]'.format(entry.path, entry.reason)
            for entry in self.unrecognized)
        return '\n'.join(lines)


def _windows_process_is_alive(pid):
    import ctypes

    kernel32 = ctypes.WinDLL('kernel32', use_last_error=True)
    kernel32.OpenProcess.restype = ctypes.c_void_p
    kernel32.OpenProcess.argtypes = (ctypes.c_uint32, ctypes.c_int, ctypes.c_uint32)
    kernel32.CloseHandle.argtypes = (ctypes.c_void_p,)
    PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
    ERROR_INVALID_PARAMETER = 87
    ERROR_ACCESS_DENIED = 5
    handle = kernel32.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, False, pid)
    if not handle:
        # ERROR_INVALID_PARAMETER means no such process; access denied means
        # the process exists but we are not allowed to open it.
        return ctypes.get_last_error() == ERROR_ACCESS_DENIED
    kernel32.CloseHandle(handle)
    return True


def default_process_is_alive(pid):
    """Return whether a process with the given pid appears to be running.

    The signal-0 probe is used on POSIX. Windows has no equivalent, so the
    process is opened with limited query rights instead. Callers that already
    track their workers (e.g. a process manager) can pass their own liveness
    callback to build_cleanup_plan instead.
    """
    if pid <= 0:
        return False
    if os.name == 'nt':
        return _windows_process_is_alive(pid)
    try:
        os.kill(pid, 0)
    except OSError as err:
        if err.errno == errno.ESRCH:
            return False
        # EPERM means the process exists but cannot be signalled.
        return True
    return True


def build_cleanup_plan(pid=None, path=None, process_is_alive=default_process_is_alive):
    """Scan the multiprocess directory and build a reviewable cleanup plan.

    No file is removed by this function. Every protocol-conforming file is
    classified as either to_delete or kept, with a stable reason, while files
    that do not follow the naming protocol are listed as unrecognized and
    left alone.

    Only live* gauge files of a dead process are eligible for deletion:
    counters, histograms, summaries and non-live gauges must be retained so
    cumulative values and last gauge values keep aggregating correctly.

    Pass pid=None to consider every process in the directory.
    process_is_alive is a callback taking a pid and returning True when the
    process still runs, which keeps this testable without real processes.
    """
    path = _resolve_multiproc_dir(path)
    if not path or not os.path.isdir(path):
        raise ValueError(
            'env PROMETHEUS_MULTIPROC_DIR is not set or not a directory')

    alive_cache: Dict[int, bool] = {}

    def is_alive(checked_pid):
        if checked_pid not in alive_cache:
            alive_cache[checked_pid] = bool(process_is_alive(checked_pid))
        return alive_cache[checked_pid]

    to_delete: List[CleanupEntry] = []
    kept: List[CleanupEntry] = []
    unrecognized: List[UnrecognizedFile] = []

    with os.scandir(path) as entries:
        for entry in entries:
            entry_path = os.path.join(path, entry.name)
            if not entry.is_file():
                unrecognized.append(UnrecognizedFile(
                    entry_path, 'not a regular file'))
                continue
            metric_file = parse_metric_filename(entry_path)
            if metric_file is None:
                unrecognized.append(UnrecognizedFile(
                    entry_path,
                    'file name does not match the multi-process metric protocol'))
                continue
            if pid is not None and metric_file.pid != pid:
                kept.append(CleanupEntry(
                    metric_file, REASON_PID_NOT_REQUESTED))
            elif metric_file.is_cumulative:
                kept.append(CleanupEntry(
                    metric_file, REASON_CUMULATIVE_METRIC))
            elif not metric_file.is_live_gauge:
                kept.append(CleanupEntry(
                    metric_file, REASON_NONLIVE_GAUGE))
            elif is_alive(metric_file.pid):
                kept.append(CleanupEntry(
                    metric_file, REASON_LIVE_GAUGE_PROCESS_ALIVE))
            else:
                to_delete.append(CleanupEntry(
                    metric_file, REASON_LIVE_GAUGE_DEAD_PROCESS))

    return CleanupPlan(
        path=os.path.abspath(path),
        pid=pid,
        to_delete=tuple(to_delete),
        kept=tuple(kept),
        unrecognized=tuple(unrecognized),
        process_is_alive=process_is_alive)


def _plan_entry_still_valid(plan, entry, liveness_cache, process_is_alive):
    """Re-check a planned deletion against the current state of the disk.

    Guards against a stale plan: the file must still exist, be byte-for-byte
    the same object fingerprinted at planning time, still match the protocol
    with the same pid/type, and its pid must still be dead.
    """
    metric_file = entry.file
    if not metric_file.fingerprint.matches(metric_file.path):
        return False, 'file was replaced or removed after the plan was built'
    if not os.path.abspath(metric_file.path).startswith(plan.path + os.sep):
        return False, 'file is no longer inside the planned directory'
    reparsed = parse_metric_filename(metric_file.path)
    if reparsed is None or reparsed != metric_file:
        return False, 'file no longer matches the multi-process metric protocol'
    checked_pid = metric_file.pid
    if checked_pid not in liveness_cache:
        liveness_cache[checked_pid] = bool(process_is_alive(checked_pid))
    if liveness_cache[checked_pid]:
        return False, 'pid {} is alive again (pid reuse)'.format(checked_pid)
    return True, ''


def execute_cleanup_plan(plan, process_is_alive=None):
    """Execute a previously built CleanupPlan in a controlled way.

    Each planned deletion is re-validated against the current disk state, so
    a plan cannot delete a file that was replaced after planning or one whose
    pid was reused by a new process. Entries that fail re-validation are
    reported in the result as skipped rather than removed. Files that were
    only ever classified as kept or unrecognized are never touched.
    """
    if process_is_alive is None:
        process_is_alive = plan.process_is_alive or default_process_is_alive

    deleted: List[CleanupEntry] = []
    skipped: List[SkippedFile] = []
    liveness_cache: Dict[int, bool] = {}

    for entry in plan.to_delete:
        valid, reason = _plan_entry_still_valid(
            plan, entry, liveness_cache, process_is_alive)
        if not valid:
            skipped.append(SkippedFile(entry, reason))
            continue
        try:
            os.remove(entry.file.path)
        except FileNotFoundError:
            skipped.append(SkippedFile(
                entry, 'file was replaced or removed after the plan was built'))
        else:
            deleted.append(entry)

    return CleanupResult(
        deleted=tuple(deleted),
        skipped=tuple(skipped))


def mark_process_dead(pid, path=None):
    """Do bookkeeping for when one process dies in a multi-process setup.

    Only the live* gauge files of the given pid are removed. Counters,
    histograms, summaries and non-live gauges stay in place, and files that
    do not follow the metric file protocol are ignored. The deletion goes
    through build_cleanup_plan/execute_cleanup_plan, so a file replaced after
    planning (e.g. via pid reuse) is skipped rather than deleted.

    The caller is expected to know the process has exited (e.g. a process
    manager's child-exit hook); use build_cleanup_plan directly when you want
    a liveness check before deleting.
    """
    path = _resolve_multiproc_dir(path)
    if not path:
        return
    plan = build_cleanup_plan(pid, path, process_is_alive=lambda _: False)
    execute_cleanup_plan(plan, process_is_alive=lambda _: False)
