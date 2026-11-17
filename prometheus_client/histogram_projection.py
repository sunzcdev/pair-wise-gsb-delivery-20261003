"""Non-destructive bucket projection for classic histograms.

Different collection endpoints sometimes need a coarser view of the same
Histogram without the instrumented metric itself being changed. This module
implements *bucket projection*: given a histogram metric family it produces a
new metric family that only retains a subset of the original cumulative
bucket boundaries.

Projection never invents data. The retained boundaries must be an ordered
subset of the boundaries already present on the source metric, and each
retained bucket keeps its original cumulative count. Interpolation is never
used to fabricate counts for intervals that the source does not define. The
``_count``, ``_sum`` and ``+Inf`` samples (and their cumulative meaning) are
preserved, as are labels. Exemplars are kept only when the bucket they are
attached to is itself retained, so an exemplar is only ever emitted for a
bucket whose membership is unambiguous.
"""

import math
from typing import Dict, Iterable, List, Sequence, Tuple, Union

from .metrics_core import Metric
from .registry import Collector
from .samples import Sample
from .utils import INF

_BUCKET_SUFFIX = '_bucket'
_HISTOGRAM_TYPES = ('histogram', 'gaugehistogram')


def _prepare_target_buckets(buckets: Sequence[Union[float, str]]) -> List[float]:
    """Validate the requested projection boundaries and normalise them.

    Returns the boundaries as strictly increasing floats, with ``+Inf``
    appended when it was not given explicitly. NaN, non-finite negative
    values, duplicates and out-of-order boundaries are rejected.
    """
    prepared: List[float] = []
    for bucket in buckets:
        bound = float(bucket)
        if math.isnan(bound):
            raise ValueError('Bucket projection boundaries must not contain NaN')
        if math.isinf(bound) and bound < 0:
            raise ValueError('Bucket projection boundaries must not contain -Inf')
        prepared.append(bound)

    if not prepared:
        raise ValueError('At least one bucket boundary is required for a bucket projection')

    for lower, upper in zip(prepared, prepared[1:]):
        if not lower < upper:
            raise ValueError(
                'Bucket projection boundaries must be strictly increasing '
                'and must not contain duplicates')

    if not math.isinf(prepared[-1]):
        # +Inf is always part of a valid histogram and is always retained.
        prepared.append(INF)
    return prepared


def _series_key(sample: Sample) -> Tuple[Tuple[str, str], ...]:
    """Identify the label set a bucket belongs to (all labels but ``le``)."""
    return tuple(sorted(
        (name, value) for name, value in sample.labels.items() if name != 'le'))


def project_histogram_buckets(
        metric: Metric,
        buckets: Sequence[Union[float, str]]) -> Metric:
    """Project a histogram metric family onto a coarser set of buckets.

    Args:
        metric: A histogram (or gauge histogram) metric family. The metric
            and its samples are not modified.
        buckets: The upper bounds to retain. They must be finite, strictly
            increasing and every bound must already exist on the source
            metric. ``+Inf`` may be included and is always retained; it is
            appended automatically when omitted.

    Returns:
        A new ``Metric`` of the same name, type, documentation and unit that
        contains only the retained bucket samples alongside the original
        non-bucket samples (``_count``, ``_sum`` and friends).

    Raises:
        ValueError: If the metric is not a histogram, a requested boundary is
            missing from the source, or the requested boundaries contain NaN,
            duplicates or are not strictly increasing.
    """
    if metric.type not in _HISTOGRAM_TYPES:
        raise ValueError(
            f'Bucket projection is only supported for histograms, got {metric.type}')

    targets = _prepare_target_buckets(buckets)
    finite_targets = [bound for bound in targets if not math.isinf(bound)]

    # Collect the bucket boundaries that exist for each labelled series so
    # that every requested boundary can be checked against the source.
    series_bounds: Dict[Tuple[Tuple[str, str], ...], Dict[float, Sample]] = {}
    series_order: List[Tuple[Tuple[str, str], ...]] = []
    for sample in metric.samples:
        if sample.name != metric.name + _BUCKET_SUFFIX or 'le' not in sample.labels:
            continue
        key = _series_key(sample)
        try:
            bound = float(sample.labels['le'])
        except (TypeError, ValueError) as cause:
            raise ValueError(
                f"Histogram bucket has a non-numeric le label: {sample.labels['le']!r}") from cause
        if math.isnan(bound):
            raise ValueError('Histogram bucket has a NaN le label')
        group = series_bounds.setdefault(key, {})
        if key not in series_order:
            series_order.append(key)
        if bound in group:
            raise ValueError(f'Duplicate bucket boundary {bound!r} in source histogram')
        group[bound] = sample

    if not series_bounds:
        raise ValueError('Histogram metric has no bucket samples to project')

    retained: Dict[Tuple[Tuple[str, str], ...], set] = {}
    for key in series_order:
        available = series_bounds[key]
        if INF not in available:
            raise ValueError('Source histogram is missing the required +Inf bucket')
        missing = [bound for bound in finite_targets if bound not in available]
        if missing:
            raise ValueError(
                'Bucket projection boundaries must be an ordered subset of the '
                'source boundaries; missing boundaries: '
                + ', '.join(repr(bound) for bound in missing))
        retained[key] = set(targets)

    # Rebuild the family in source order, dropping only the bucket samples
    # whose boundaries were not retained. Everything else passes through.
    projected = Metric(metric.name, metric.documentation, metric.type, metric.unit)
    for sample in metric.samples:
        if sample.name == metric.name + _BUCKET_SUFFIX and 'le' in sample.labels:
            key = _series_key(sample)
            bound = float(sample.labels['le'])
            if bound not in retained[key]:
                continue
        projected.samples.append(sample)
    return projected


class BucketProjectionCollector:
    """Collector wrapper that exposes a coarse-grained bucket view.

    The wrapped collector is left untouched. On every scrape, histogram
    metric families it produces are projected onto ``buckets`` while every
    other metric family is passed through unchanged. This makes it possible
    to serve different collection endpoints from one underlying set of
    instruments, each endpoint retaining only the bucket boundaries it needs.

    Args:
        collector: The collector (for example a ``CollectorRegistry``) whose
            histogram metrics should be projected.
        buckets: The upper bounds to retain. See
            :func:`project_histogram_buckets` for the validation rules.
    """

    def __init__(
            self,
            collector: Collector,
            buckets: Sequence[Union[float, str]]):
        # Validate eagerly so that a bad configuration fails at construction
        # time rather than on the first scrape.
        self._targets = _prepare_target_buckets(buckets)
        self._collector = collector

    def describe(self) -> List[Metric]:
        """Return the underlying collector's description, when available.

        Projection only removes samples, so the metric names are unchanged.
        A registry has no ``describe`` method, in which case the collected
        families are used instead.
        """
        describe = getattr(self._collector, 'describe', None)
        if describe is not None:
            return describe()
        return list(self._collector.collect())

    def collect(self) -> Iterable[Metric]:
        """Yield projected histograms and all other metric families as-is."""
        for metric in self._collector.collect():
            if metric.type in _HISTOGRAM_TYPES:
                yield project_histogram_buckets(metric, self._targets)
            else:
                yield metric
