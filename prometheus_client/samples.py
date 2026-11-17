"""Metric sample records, plus helpers for merging native histograms.

The sample types at the top describe single samples as they appear in the
exposition formats.  :class:`NativeHistogram` stores bucket populations as
spans and deltas, which is compact but awkward to combine; the helpers below
implement the Prometheus rules for merging such histograms when their schemas
are compatible.
"""

import math
import sys
from typing import (
    Dict, Iterable, List, NamedTuple, Optional, Sequence, Tuple, Union,
)


class Timestamp:
    """A nanosecond-resolution timestamp."""

    def __init__(self, sec: float, nsec: float) -> None:
        if nsec < 0 or nsec >= 1e9:
            raise ValueError(f"Invalid value for nanoseconds in Timestamp: {nsec}")
        if sec < 0:
            nsec = -nsec
        self.sec: int = int(sec)
        self.nsec: int = int(nsec)

    def __str__(self) -> str:
        sign = '-' if self.sec < 0 or self.nsec < 0 else ''
        return f"{sign}{abs(self.sec)}.{abs(self.nsec):09d}"

    def __repr__(self) -> str:
        return f"Timestamp({self.sec}, {self.nsec})"

    def __float__(self) -> float:
        return float(self.sec) + float(self.nsec) / 1e9

    def __eq__(self, other: object) -> bool:
        return isinstance(other, Timestamp) and self.sec == other.sec and self.nsec == other.nsec

    def __ne__(self, other: object) -> bool:
        return not self == other

    def __gt__(self, other: "Timestamp") -> bool:
        return self.nsec > other.nsec if self.sec == other.sec else self.sec > other.sec

    def __lt__(self, other: "Timestamp") -> bool:
        return self.nsec < other.nsec if self.sec == other.sec else self.sec < other.sec


# BucketSpan is experimental and subject to change at any time.
class BucketSpan(NamedTuple):
    offset: int
    length: int


# Timestamp and exemplar are optional.
# Value can be an int or a float.
# Timestamp can be a float containing a unixtime in seconds,
# a Timestamp object, or None.
# Exemplar can be an Exemplar object, or None.
class Exemplar(NamedTuple):
    labels: Dict[str, str]
    value: float
    timestamp: Optional[Union[float, Timestamp]] = None


# NativeHistogram is experimental and subject to change at any time.
class NativeHistogram(NamedTuple):
    count_value: float
    sum_value: float
    schema: int
    zero_threshold: float
    zero_count: float
    pos_spans: Optional[Sequence[BucketSpan]] = None
    neg_spans: Optional[Sequence[BucketSpan]] = None
    pos_deltas: Optional[Sequence[int]] = None
    neg_deltas: Optional[Sequence[int]] = None
    nh_exemplars: Optional[Sequence[Exemplar]] = None


class Sample(NamedTuple):
    name: str
    labels: Dict[str, str]
    value: float
    timestamp: Optional[Union[float, Timestamp]] = None
    exemplar: Optional[Exemplar] = None
    native_histogram: Optional[NativeHistogram] = None


# Finest and coarsest standard exponential schema supported here.  A schema of
# n divides every power of two into 2**n buckets, so a smaller schema means
# wider buckets, i.e. a coarser resolution.
EXPONENTIAL_SCHEMA_MIN = -4
EXPONENTIAL_SCHEMA_MAX = 8

# Schema Prometheus uses for custom bucket histograms ("NHCB").  Those have no
# spans or deltas at all, so they cannot be merged by the helpers below.
CUSTOM_BUCKETS_SCHEMA = -53

# Prometheus reserves -9..-5 and 9..52 for future resolutions.  They are valid
# on the wire, but are not resolutions this client can compute with.
RESERVED_SCHEMA_MIN = -9
RESERVED_SCHEMA_MAX = 52

# The largest finite float64.  The bucket before the +Inf bucket ends here
# rather than at the +Inf its boundary formula would produce.
_MAX_FLOAT = sys.float_info.max


class NativeHistogramError(ValueError):
    """Raised when a native histogram is malformed or cannot be merged."""


class UnsupportedSchemaError(NativeHistogramError):
    """Raised for a native histogram schema that cannot be handled.

    ``schema`` holds the offending value and ``reason`` states why it is not
    supported, so that callers can report the problem without parsing the
    message.
    """

    def __init__(self, schema: object, reason: str) -> None:
        super().__init__(f"unsupported native histogram schema {schema!r}: {reason}")
        self.schema = schema
        self.reason = reason


class _Layout(NamedTuple):
    """Validated view of one native histogram.

    ``positives`` and ``negatives`` map a bucket index in ``schema`` to the
    absolute (not delta encoded) population of that bucket.
    """

    count_value: Union[int, float]
    sum_value: Union[int, float]
    schema: int
    zero_threshold: float
    zero_count: Union[int, float]
    positives: Dict[int, int]
    negatives: Dict[int, int]


def merge_native_histograms(
    histograms: Union[NativeHistogram, Iterable[NativeHistogram]],
    *more_histograms: NativeHistogram,
    schema: Optional[int] = None,
    zero_threshold: Optional[float] = None,
) -> NativeHistogram:
    """Merge compatible native histograms into a new native histogram.

    Inputs with a finer schema than ``schema`` are first reduced to it: bucket
    index ``i`` of schema ``s`` becomes bucket ``((i - 1) >> (s - t)) + 1`` of
    schema ``t``, which is precisely the bucket of the coarser schema that
    contains it.  Populations are then summed, so no observation is dropped and
    bucket boundaries only ever get wider.

    The result is canonical: buckets are ordered by index, consecutive
    populated buckets share a span, gaps stay encoded as span offsets (they are
    never expanded into empty buckets), empty buckets are trimmed and every
    delta list is relative to zero for the first bucket of its side.  Empty
    spans, negative deltas and very sparse populations are therefore preserved
    rather than flattened.  Observations only ever move from a regular bucket
    into the zero bucket (see below); their number is unchanged.

    Zero buckets follow the Prometheus merge rules.  The largest zero threshold
    of the inputs is used.  If that threshold falls inside a populated bucket of
    any input, the threshold is raised to that bucket's inclusive upper bound
    and the check is repeated, so that the final threshold either coincides with
    a bucket boundary or is not inside a populated bucket at all.  Every bucket
    whose upper bound is at most the threshold is then folded into the zero
    bucket, which keeps its observations.

    Arguments:
        histograms: A native histogram, or an iterable of native histograms.
        *more_histograms: Further histograms, so that both
            ``merge_native_histograms(a, b)`` and
            ``merge_native_histograms([a, b])`` work.
        schema: Exponential schema of the result, between
            :data:`EXPONENTIAL_SCHEMA_MIN` and :data:`EXPONENTIAL_SCHEMA_MAX`.
            It must not be finer than the coarsest input schema, because
            resolution can only be reduced.  Defaults to that coarsest schema.
        zero_threshold: Zero bucket threshold of the result.  It must not be
            smaller than the largest threshold of the inputs, because the
            observations in a zero bucket cannot be split again; it may be
            larger, in which case more buckets are folded into the zero bucket.
            Defaults to the largest threshold of the inputs.

    Returns:
        A new ``NativeHistogram``.  Neither the inputs nor any sequence they
        reference are modified.  ``count_value`` and ``sum_value`` are the sums
        of the inputs' values, and :func:`native_histogram_bucket_total` still
        accounts for every observation, which lets callers cross-check the
        merge against ``count_value``.

    Raises:
        UnsupportedSchemaError: If an input or the requested schema is not a
            standard exponential schema.  Custom bucket, reserved and unknown
            schemas are reported together with the reason.
        NativeHistogramError: If an input is malformed (a negative absolute
            bucket population, spans that disagree with the deltas, a negative
            offset on a later span, overlapping spans, ...), if ``schema`` is
            finer than an input schema, or if ``zero_threshold`` is smaller
            than the threshold of an input.
        TypeError: If an argument is not a native histogram.
    """
    inputs = _collect_histograms(histograms, more_histograms)
    layouts = [_layout_of(histogram, position) for position, histogram in enumerate(inputs, 1)]
    target_schema = _target_schema(layouts, schema)
    reduced = [_at_schema(layout, target_schema) for layout in layouts]
    # Resolution is reduced before the zero bucket is settled, so the threshold
    # is raised against the buckets that actually survive the reduction.
    threshold = _target_zero_threshold(reduced, zero_threshold)

    positive: Dict[int, int] = {}
    negative: Dict[int, int] = {}
    zero_counts: List[Union[int, float]] = []
    for layout in reduced:
        zero_counts.append(layout.zero_count)
        for buckets, merged in ((layout.positives, positive), (layout.negatives, negative)):
            remaining, absorbed = _split_off_zero_bucket(buckets, target_schema, threshold)
            _add_buckets(merged, remaining)
            zero_counts.append(absorbed)
    zero_count = _sum_numbers(zero_counts)
    _check_populations_preserved(reduced, positive, negative, zero_count)

    pos_spans, pos_deltas = _encode_side(positive)
    neg_spans, neg_deltas = _encode_side(negative)
    exemplars = tuple(
        exemplar for histogram in inputs for exemplar in (histogram.nh_exemplars or ()))
    return NativeHistogram(
        count_value=_sum_numbers([layout.count_value for layout in reduced]),
        sum_value=_sum_values([layout.sum_value for layout in reduced]),
        schema=target_schema,
        zero_threshold=threshold,
        zero_count=zero_count,
        pos_spans=pos_spans,
        neg_spans=neg_spans,
        pos_deltas=pos_deltas,
        neg_deltas=neg_deltas,
        nh_exemplars=exemplars or None,
    )


def reduce_native_histogram_resolution(
    histogram: NativeHistogram,
    schema: int,
    *,
    zero_threshold: Optional[float] = None,
) -> NativeHistogram:
    """Return ``histogram`` expressed in a coarser ``schema``.

    This is :func:`merge_native_histograms` for a single histogram, and follows
    the same zero bucket rules.  Use it to downscale a histogram before storing
    or comparing it with histograms of the coarser schema.

    Arguments:
        histogram: The native histogram to downscale.
        schema: Target exponential schema, not finer than ``histogram.schema``.
        zero_threshold: Optional zero bucket threshold for the result.

    Returns:
        A new ``NativeHistogram``; the input is left untouched.

    Raises:
        UnsupportedSchemaError: If either schema is not a standard exponential
            schema, with the reason.
        NativeHistogramError: If the histogram is malformed or the target
            schema is finer than the current one.
    """
    return merge_native_histograms(histogram, schema=schema, zero_threshold=zero_threshold)


def native_histogram_bucket_total(histogram: NativeHistogram) -> float:
    """Return the population of the zero bucket plus all regular buckets.

    This is ``zero_count`` plus the absolute population of every positive and
    negative bucket.  Comparing it with ``count_value`` shows whether a
    histogram accounts for all of its observations, and since merging only
    moves populations between buckets, :func:`merge_native_histograms` keeps it
    unchanged.

    Arguments:
        histogram: The native histogram to inspect.

    Returns:
        The summed bucket population as a float.

    Raises:
        UnsupportedSchemaError: If its schema is not supported.
        NativeHistogramError: If the histogram is malformed.
    """
    layout = _layout_of(histogram, 1)
    return _flat_population(layout)


def _collect_histograms(
    histograms: Union[NativeHistogram, Iterable[NativeHistogram]],
    more_histograms: Tuple[NativeHistogram, ...],
) -> List[NativeHistogram]:
    """Normalize the accepted calling conventions into a list of histograms."""
    if more_histograms:
        if not isinstance(histograms, NativeHistogram):
            raise TypeError(
                'merge_native_histograms() takes either an iterable of native histograms '
                'or the native histograms themselves')
        return [histograms, *more_histograms]
    if isinstance(histograms, NativeHistogram):
        return [histograms]
    inputs: List[NativeHistogram] = list(histograms)
    if not inputs:
        raise NativeHistogramError('at least one native histogram is required')
    return inputs


def _layout_of(histogram: object, position: int) -> _Layout:
    """Validate one histogram and decode its deltas into absolute populations."""
    label = f'histogram {position}'
    if not isinstance(histogram, NativeHistogram):
        raise TypeError(f'{label} is a {type(histogram).__name__}, not a NativeHistogram')
    checked_schema = _check_schema(histogram.schema, f'{label} schema')
    positives = _bucket_populations(
        histogram.pos_spans, histogram.pos_deltas, f'{label} positive')
    negatives = _bucket_populations(
        histogram.neg_spans, histogram.neg_deltas, f'{label} negative')
    return _Layout(
        count_value=_check_count(histogram.count_value, f'{label} count_value'),
        sum_value=_check_sum(histogram.sum_value, f'{label} sum_value'),
        schema=checked_schema,
        zero_threshold=_check_zero_threshold(
            histogram.zero_threshold, f'{label} zero_threshold'),
        zero_count=_check_count(histogram.zero_count, f'{label} zero_count'),
        positives=positives,
        negatives=negatives,
    )


def _check_schema(schema: object, label: str) -> int:
    """Return ``schema`` if it is a supported exponential schema, else raise."""
    if isinstance(schema, bool) or not isinstance(schema, int):
        raise UnsupportedSchemaError(schema, f'{label} is not an integer')
    if schema == CUSTOM_BUCKETS_SCHEMA:
        raise UnsupportedSchemaError(
            schema,
            'custom bucket schema (NHCB); such histograms describe their buckets with '
            'custom bounds instead of spans and deltas, so they cannot be merged here')
    if EXPONENTIAL_SCHEMA_MIN <= schema <= EXPONENTIAL_SCHEMA_MAX:
        return schema
    if RESERVED_SCHEMA_MIN <= schema <= RESERVED_SCHEMA_MAX:
        raise UnsupportedSchemaError(
            schema,
            'reserved exponential schema; reduce it to a schema between '
            f'{EXPONENTIAL_SCHEMA_MIN} and {EXPONENTIAL_SCHEMA_MAX} before merging')
    raise UnsupportedSchemaError(
        schema,
        'unknown schema; only the exponential schemas '
        f'{EXPONENTIAL_SCHEMA_MIN} to {EXPONENTIAL_SCHEMA_MAX} are supported')


def _check_count(value: object, label: str) -> Union[int, float]:
    """Validate an observation count."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise NativeHistogramError(f'{label} must be a number, got {value!r}')
    if isinstance(value, float) and not math.isfinite(value):
        raise NativeHistogramError(f'{label} must be finite, got {value!r}')
    if value < 0:
        raise NativeHistogramError(f'{label} must not be negative, got {value!r}')
    return value


def _check_sum(value: object, label: str) -> Union[int, float]:
    """Validate the sum of the observations.  NaN and infinities are allowed."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise NativeHistogramError(f'{label} must be a number, got {value!r}')
    return value


def _check_zero_threshold(value: object, label: str) -> float:
    """Validate a zero bucket threshold and return it as a float."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise NativeHistogramError(f'{label} must be a number, got {value!r}')
    threshold = float(value)
    if not math.isfinite(threshold):
        raise NativeHistogramError(f'{label} must be finite, got {value!r}')
    if threshold < 0:
        raise NativeHistogramError(f'{label} must not be negative, got {value!r}')
    return threshold


def _bucket_populations(
    spans: Optional[Sequence[BucketSpan]],
    deltas: Optional[Sequence[int]],
    label: str,
) -> Dict[int, int]:
    """Decode delta encoded buckets into a bucket index to population mapping.

    Empty spans (length zero) yield no bucket but still shift the index by their
    offset, and gaps between spans stay gaps, so neither loses observations.
    Deltas are relative to the previous bucket of the same side and may be
    negative; only the resulting absolute population has to stay non-negative.
    """
    span_list = tuple(spans or ())
    delta_list = tuple(deltas or ())
    populations: Dict[int, int] = {}
    bucket_index = 0
    delta_index = 0
    population = 0
    previous_index: Optional[int] = None
    for number, span in enumerate(span_list, 1):
        offset, length = _check_span(span, number, label)
        if number > 1 and offset < 0:
            raise NativeHistogramError(
                f'{label} span {number} has the negative offset {offset}; only the first '
                'span of a side may start below bucket index zero')
        bucket_index += offset
        for _ in range(length):
            if delta_index >= len(delta_list):
                raise NativeHistogramError(
                    f'{label} spans cover more buckets than the {len(delta_list)} deltas '
                    'provided')
            delta = _check_delta(delta_list[delta_index], delta_index, label)
            population += delta
            if population < 0:
                raise NativeHistogramError(
                    f'{label} bucket at index {bucket_index} has the negative absolute '
                    f'population {population}; deltas must not evaluate to a negative count')
            if previous_index is not None and bucket_index <= previous_index:
                raise NativeHistogramError(
                    f'{label} bucket at index {bucket_index} overlaps the previous bucket '
                    f'at index {previous_index}')
            populations[bucket_index] = population
            previous_index = bucket_index
            delta_index += 1
            bucket_index += 1
    if delta_index != len(delta_list):
        raise NativeHistogramError(
            f'{label} spans cover {delta_index} buckets but {len(delta_list)} deltas were '
            'provided')
    return populations


def _check_span(span: object, number: int, label: str) -> Tuple[int, int]:
    """Validate one span and return its offset and length."""
    if not isinstance(span, (tuple, list)) or len(span) != 2:
        raise NativeHistogramError(
            f'{label} span {number} must be a BucketSpan or an (offset, length) pair, '
            f'got {span!r}')
    offset = _check_span_int(span[0], 'offset', number, label)
    length = _check_span_int(span[1], 'length', number, label)
    if length < 0:
        raise NativeHistogramError(f'{label} span {number} has the negative length {length}')
    return offset, length


def _check_span_int(value: object, name: str, number: int, label: str) -> int:
    """Validate one span field and return it as an int."""
    if isinstance(value, bool) or not isinstance(value, int):
        raise NativeHistogramError(
            f'{label} span {number} has the non-integer {name} {value!r}')
    return value


def _check_delta(delta: object, position: int, label: str) -> int:
    """Validate one delta."""
    if isinstance(delta, bool) or not isinstance(delta, int):
        raise NativeHistogramError(
            f'{label} delta {position + 1} must be an integer, got {delta!r}')
    return delta


def _target_schema(layouts: List[_Layout], schema: Optional[int]) -> int:
    """Pick and validate the schema that all inputs are merged at."""
    coarsest = min(layout.schema for layout in layouts)
    if schema is None:
        return coarsest
    if not isinstance(schema, int) or isinstance(schema, bool):
        raise UnsupportedSchemaError(schema, 'target schema is not an integer')
    target = _check_schema(schema, 'target schema')
    if target > coarsest:
        raise NativeHistogramError(
            f'cannot merge at schema {target}: it is finer than the coarsest input schema '
            f'{coarsest}, and resolution can only be reduced')
    return target


def _at_schema(layout: _Layout, target_schema: int) -> _Layout:
    """Return ``layout`` re-expressed in ``target_schema``."""
    if layout.schema == target_schema:
        return layout
    shift = layout.schema - target_schema
    return layout._replace(
        schema=target_schema,
        positives=_reindex(layout.positives, shift),
        negatives=_reindex(layout.negatives, shift),
    )


def _reindex(buckets: Dict[int, int], shift: int) -> Dict[int, int]:
    """Map bucket indices ``shift`` schemas coarser, summing shared buckets.

    ``((index - 1) >> shift) + 1`` is the index of the coarser bucket that
    contains the bucket at ``index``, so source buckets that fall into the same
    target bucket have their populations added.
    """
    reindexed: Dict[int, int] = {}
    for index, population in buckets.items():
        target = ((index - 1) >> shift) + 1
        reindexed[target] = reindexed.get(target, 0) + population
    return reindexed


def _target_zero_threshold(layouts: List[_Layout], requested: Optional[float]) -> float:
    """Resolve the zero bucket threshold following the Prometheus rules."""
    largest = max(layout.zero_threshold for layout in layouts)
    if requested is None:
        threshold = largest
    else:
        threshold = _check_zero_threshold(requested, 'zero_threshold')
        if threshold < largest:
            raise NativeHistogramError(
                f'zero_threshold {threshold!r} is smaller than the largest input zero '
                f'threshold {largest!r}; observations in a zero bucket cannot be split '
                'back into regular buckets')
    return _raise_threshold_above_populated_buckets(threshold, layouts)


def _raise_threshold_above_populated_buckets(
    threshold: float,
    layouts: List[_Layout],
) -> float:
    """Raise ``threshold`` until it does not cut through a populated bucket.

    A bucket with index ``index`` spans the absolute values
    ``(bound(index - 1), bound(index)]`` on either side of zero and cannot be
    split, so a threshold inside it is raised to its inclusive upper bound.  The
    larger threshold may cut through a bucket further out, hence the loop.
    """
    schema = layouts[0].schema
    while True:
        raised = threshold
        for layout in layouts:
            for buckets in (layout.positives, layout.negatives):
                for index, population in buckets.items():
                    if population == 0 or _upper_bound(index, schema) <= threshold:
                        continue
                    if _lower_bound(index, schema) < threshold:
                        raised = max(raised, _upper_bound(index, schema))
        if raised == threshold:
            return threshold
        threshold = raised


def _split_off_zero_bucket(
    buckets: Dict[int, int],
    schema: int,
    threshold: float,
) -> Tuple[Dict[int, int], int]:
    """Separate the buckets fully inside the zero bucket from the rest."""
    remaining: Dict[int, int] = {}
    absorbed = 0
    for index, population in buckets.items():
        if _upper_bound(index, schema) <= threshold:
            absorbed += population
        else:
            remaining[index] = population
    return remaining, absorbed


def _add_buckets(target: Dict[int, int], extra: Dict[int, int]) -> None:
    """Add the populations of ``extra`` into ``target``."""
    for index, population in extra.items():
        target[index] = target.get(index, 0) + population


def _encode_side(
    buckets: Dict[int, int],
) -> Tuple[Optional[Tuple[BucketSpan, ...]], Optional[Tuple[int, ...]]]:
    """Encode absolute bucket populations as canonical spans and deltas.

    A side without populated buckets is reported as ``None`` for both the spans
    and the deltas, matching how ``NativeHistogram`` and the parsers represent
    an absent side.
    """
    spans: List[BucketSpan] = []
    deltas: List[int] = []
    previous_index = 0
    previous_population = 0
    for index in sorted(buckets):
        population = buckets[index]
        if population == 0:
            # An empty bucket adds nothing and stays part of the gap between
            # spans, which is exactly how offsets encode it.
            continue
        if not spans:
            spans.append(BucketSpan(index, 1))
        elif index == previous_index + 1:
            last = spans[-1]
            spans[-1] = BucketSpan(last.offset, last.length + 1)
        else:
            spans.append(BucketSpan(index - previous_index - 1, 1))
        deltas.append(population - previous_population)
        previous_index = index
        previous_population = population
    if not spans:
        return None, None
    return tuple(spans), tuple(deltas)


def _sum_numbers(values: Iterable[Union[int, float]]) -> Union[int, float]:
    """Sum non-negative finite numbers, staying exact when all are integers."""
    numbers = list(values)
    if all(isinstance(value, int) and not isinstance(value, bool) for value in numbers):
        return sum(numbers)
    return math.fsum(numbers)


def _sum_values(values: Iterable[Union[int, float]]) -> Union[int, float]:
    """Sum observation sums, propagating NaN and infinities like float addition."""
    numbers = list(values)
    if any(isinstance(value, float) and math.isnan(value) for value in numbers):
        return math.nan
    positive_infinity = any(value == math.inf for value in numbers)
    negative_infinity = any(value == -math.inf for value in numbers)
    if positive_infinity and negative_infinity:
        return math.nan
    if positive_infinity:
        return math.inf
    if negative_infinity:
        return -math.inf
    return _sum_numbers(numbers)


def _check_populations_preserved(
    layouts: List[_Layout],
    positive: Dict[int, int],
    negative: Dict[int, int],
    zero_count: Union[int, float],
) -> None:
    """Verify that the merge only moved populations between buckets.

    ``zero_count`` already contains both the input zero buckets and the
    populations absorbed into the zero bucket, so the total has to match the
    input total.  Both totals use ``math.fsum``, which is order independent, so
    float counts cannot trip the check merely because the summation order
    changed.
    """
    before = math.fsum(
        _flat_population(layout) for layout in layouts)
    after = math.fsum(
        [float(zero_count)]
        + [float(count) for count in positive.values()]
        + [float(count) for count in negative.values()])
    if before != after:
        raise NativeHistogramError(
            f'merging lost observations: {before!r} bucket observations before, '
            f'{after!r} after')


def _flat_population(layout: _Layout) -> float:
    """Return the total population stored in one layout."""
    return math.fsum(
        [float(layout.zero_count)]
        + [float(count) for count in layout.positives.values()]
        + [float(count) for count in layout.negatives.values()])


def _lower_bound(index: int, schema: int) -> float:
    """Return the exclusive lower bound of bucket ``index``."""
    return _bound(index - 1, schema)


def _upper_bound(index: int, schema: int) -> float:
    """Return the inclusive upper bound of bucket ``index``."""
    return _bound(index, schema)


def _bound(index: int, schema: int) -> float:
    """Return the bucket boundary at ``index`` for the exponential ``schema``.

    This is the layout Prometheus uses: for schema ``n >= 0`` index ``i`` ends at
    ``2 ** (i / 2 ** n)``, and for negative schemas every boundary is a power of
    two.  The bucket before the ``+Inf`` bucket is capped at the largest finite
    float so that observations of ``+Inf`` keep their own bucket.
    """
    if schema < 0:
        exponent = index << -schema
        if exponent == 1024:
            return _MAX_FLOAT
        return _ldexp(1.0, exponent)
    steps = 1 << schema
    quotient, remainder = divmod(index, steps)
    exponent = quotient + 1
    if remainder == 0:
        if exponent == 1025:
            return _MAX_FLOAT
        return _ldexp(0.5, exponent)
    return _ldexp(_EXPONENTIAL_BOUNDS[schema][remainder], exponent)


def _ldexp(value: float, exponent: int) -> float:
    """``value * 2 ** exponent``, saturating to infinity on overflow."""
    try:
        return math.ldexp(value, exponent)
    except OverflowError:
        return math.inf


# Bucket boundaries in [0.5, 1) for schemas 0 to 8, as published by Prometheus
# (model/histogram/generic.go).  They are spelled out because computing them
# with 2 ** (i / 2 ** n) is off by one unit in the last place for many entries.
_EXPONENTIAL_BOUNDS: Tuple[Tuple[float, ...], ...] = (
    # Schema 0, 1 boundaries.
    (0.5,),
    # Schema 1, 2 boundaries.
    (
        0.5, 0.7071067811865475,
    ),
    # Schema 2, 4 boundaries.
    (
        0.5, 0.5946035575013605, 0.7071067811865475, 0.8408964152537144,
    ),
    # Schema 3, 8 boundaries.
    (
        0.5, 0.5452538663326288, 0.5946035575013605, 0.6484197773255048,
        0.7071067811865475, 0.7711054127039704, 0.8408964152537144, 0.9170040432046711,
    ),
    # Schema 4, 16 boundaries.
    (
        0.5, 0.5221368912137069, 0.5452538663326288, 0.5693943173783458,
        0.5946035575013605, 0.620928906036742, 0.6484197773255048, 0.6771277734684463,
        0.7071067811865475, 0.7384130729697496, 0.7711054127039704, 0.805245165974627,
        0.8408964152537144, 0.8781260801866495, 0.9170040432046711, 0.9576032806985735,
    ),
    # Schema 5, 32 boundaries.
    (
        0.5, 0.5109485743270583, 0.5221368912137069, 0.5335702003384117,
        0.5452538663326288, 0.5571933712979462, 0.5693943173783458, 0.5818624293887887,
        0.5946035575013605, 0.6076236799902344, 0.620928906036742, 0.6345254785958666,
        0.6484197773255048, 0.6626183215798706, 0.6771277734684463, 0.6919549409819159,
        0.7071067811865475, 0.7225904034885232, 0.7384130729697496, 0.7545822137967112,
        0.7711054127039704, 0.7879904225539431, 0.805245165974627, 0.8228777390769823,
        0.8408964152537144, 0.8593096490612387, 0.8781260801866495, 0.8973545375015533,
        0.9170040432046711, 0.9370838170551498, 0.9576032806985735, 0.9785720620876999,
    ),
    # Schema 6, 64 boundaries.
    (
        0.5, 0.5054446430258502, 0.5109485743270583, 0.5165124395106142,
        0.5221368912137069, 0.5278225891802786, 0.5335702003384117, 0.5393803988785598,
        0.5452538663326288, 0.5511912916539204, 0.5571933712979462, 0.5632608093041209,
        0.5693943173783458, 0.5755946149764913, 0.5818624293887887, 0.5881984958251406,
        0.5946035575013605, 0.6010783657263515, 0.6076236799902344, 0.6142402680534349,
        0.620928906036742, 0.6276903785123455, 0.6345254785958666, 0.6414350080393891,
        0.6484197773255048, 0.6554806057623822, 0.6626183215798706, 0.6698337620266515,
        0.6771277734684463, 0.6845012114872953, 0.6919549409819159, 0.6994898362691555,
        0.7071067811865475, 0.7148066691959849, 0.7225904034885232, 0.7304588970903234,
        0.7384130729697496, 0.7464538641456323, 0.7545822137967112, 0.762799075372269,
        0.7711054127039704, 0.7795022001189185, 0.7879904225539431, 0.7965710756711334,
        0.805245165974627, 0.8140137109286738, 0.8228777390769823, 0.8318382901633681,
        0.8408964152537144, 0.8500531768592616, 0.8593096490612387, 0.8686669176368529,
        0.8781260801866495, 0.8876882462632604, 0.8973545375015533, 0.9071260877501991,
        0.9170040432046711, 0.9269895625416926, 0.9370838170551498, 0.9472879907934827,
        0.9576032806985735, 0.9680308967461471, 0.9785720620876999, 0.9892280131939752,
    ),
    # Schema 7, 128 boundaries.
    (
        0.5, 0.5027149505564014, 0.5054446430258502, 0.5081891574554764,
        0.5109485743270583, 0.5137229745593818, 0.5165124395106142, 0.5193170509806894,
        0.5221368912137069, 0.5249720429003435, 0.5278225891802786, 0.5306886136446309,
        0.5335702003384117, 0.5364674337629877, 0.5393803988785598, 0.5423091811066545,
        0.5452538663326288, 0.5482145409081883, 0.5511912916539204, 0.5541842058618393,
        0.5571933712979462, 0.5602188762048033, 0.5632608093041209, 0.5663192597993595,
        0.5693943173783458, 0.572486072215902, 0.5755946149764913, 0.5787200368168754,
        0.5818624293887887, 0.585021884841625, 0.5881984958251406, 0.5913923554921704,
        0.5946035575013605, 0.5978321960199137, 0.6010783657263515, 0.6043421618132907,
        0.6076236799902344, 0.6109230164863786, 0.6142402680534349, 0.6175755319684665,
        0.620928906036742, 0.6243004885946023, 0.6276903785123455, 0.6310986751971253,
        0.6345254785958666, 0.637970889198196, 0.6414350080393891, 0.6449179367033329,
        0.6484197773255048, 0.6519406325959679, 0.6554806057623822, 0.659039800633032,
        0.6626183215798706, 0.6662162735415805, 0.6698337620266515, 0.6734708931164728,
        0.6771277734684463, 0.6808045103191123, 0.6845012114872953, 0.688217985377265,
        0.6919549409819159, 0.6957121878859629, 0.6994898362691555, 0.7032879969095076,
        0.7071067811865475, 0.7109463010845827, 0.7148066691959849, 0.718687998724491,
        0.7225904034885232, 0.7265139979245261, 0.7304588970903234, 0.7344252166684908,
        0.7384130729697496, 0.7424225829363761, 0.7464538641456323, 0.7505070348132126,
        0.7545822137967112, 0.7586795205991071, 0.762799075372269, 0.7669409989204777,
        0.7711054127039704, 0.7752924388424999, 0.7795022001189185, 0.7837348199827764,
        0.7879904225539431, 0.7922691326262467, 0.7965710756711334, 0.8008963778413465,
        0.805245165974627, 0.8096175675974316, 0.8140137109286738, 0.8184337248834821,
        0.8228777390769823, 0.8273458838280969, 0.8318382901633681, 0.8363550898207981,
        0.8408964152537144, 0.8454623996346523, 0.8500531768592616, 0.8546688815502312,
        0.8593096490612387, 0.8639756154809185, 0.8686669176368529, 0.8733836930995842,
        0.8781260801866495, 0.8828942179666361, 0.8876882462632604, 0.8925083056594671,
        0.8973545375015533, 0.9022270839033115, 0.9071260877501991, 0.9120516927035263,
        0.9170040432046711, 0.9219832844793128, 0.9269895625416926, 0.9320230241988943,
        0.9370838170551498, 0.9421720895161669, 0.9472879907934827, 0.9524316709088368,
        0.9576032806985735, 0.9628029718180622, 0.9680308967461471, 0.9732872087896164,
        0.9785720620876999, 0.9838856116165875, 0.9892280131939752, 0.9945994234836328,
    ),
    # Schema 8, 256 boundaries.
    (
        0.5, 0.5013556375251013, 0.5027149505564014, 0.5040779490592088,
        0.5054446430258502, 0.5068150424757447, 0.5081891574554764, 0.509566998038869,
        0.5109485743270583, 0.5123338964485679, 0.5137229745593818, 0.5151158188430205,
        0.5165124395106142, 0.5179128468009786, 0.5193170509806894, 0.520725062344158,
        0.5221368912137069, 0.5235525479396449, 0.5249720429003435, 0.526395386502313,
        0.5278225891802786, 0.5292536613972564, 0.5306886136446309, 0.5321274564422321,
        0.5335702003384117, 0.5350168559101208, 0.5364674337629877, 0.5379219445313954,
        0.5393803988785598, 0.5408428074966075, 0.5423091811066545, 0.5437795304588847,
        0.5452538663326288, 0.5467321995364429, 0.5482145409081883, 0.549700901315111,
        0.5511912916539204, 0.5526857228508706, 0.5541842058618393, 0.5556867516724088,
        0.5571933712979462, 0.5587040757836845, 0.5602188762048033, 0.5617377836665098,
        0.5632608093041209, 0.564787964283144, 0.5663192597993595, 0.5678547070789026,
        0.5693943173783458, 0.5709381019847808, 0.572486072215902, 0.5740382394200894,
        0.5755946149764913, 0.5771552102951081, 0.5787200368168754, 0.5802891060137493,
        0.5818624293887887, 0.5834400184762408, 0.585021884841625, 0.5866080400818185,
        0.5881984958251406, 0.5897932637314379, 0.5913923554921704, 0.5929957828304968,
        0.5946035575013605, 0.5962156912915756, 0.5978321960199137, 0.5994530835371903,
        0.6010783657263515, 0.6027080545025619, 0.6043421618132907, 0.6059806996384005,
        0.6076236799902344, 0.6092711149137041, 0.6109230164863786, 0.6125793968185725,
        0.6142402680534349, 0.6159056423670379, 0.6175755319684665, 0.6192499490999082,
        0.620928906036742, 0.622612415087629, 0.6243004885946023, 0.6259931389331581,
        0.6276903785123455, 0.6293922197748583, 0.6310986751971253, 0.6328097572894031,
        0.6345254785958666, 0.6362458516947014, 0.637970889198196, 0.6397006037528346,
        0.6414350080393891, 0.6431741147730128, 0.6449179367033329, 0.6466664866145447,
        0.6484197773255048, 0.6501778216898253, 0.6519406325959679, 0.6537082229673385,
        0.6554806057623822, 0.6572577939746774, 0.659039800633032, 0.6608266388015788,
        0.6626183215798706, 0.6644148621029772, 0.6662162735415805, 0.6680225691020727,
        0.6698337620266515, 0.6716498655934177, 0.6734708931164728, 0.6752968579460171,
        0.6771277734684463, 0.6789636531064505, 0.6808045103191123, 0.6826503586020058,
        0.6845012114872953, 0.6863570825438342, 0.688217985377265, 0.690083933630119,
        0.6919549409819159, 0.6938310211492645, 0.6957121878859629, 0.6975984549830999,
        0.6994898362691555, 0.7013863456101023, 0.7032879969095076, 0.7051948041086352,
        0.7071067811865475, 0.7090239421602076, 0.7109463010845827, 0.7128738720527471,
        0.7148066691959849, 0.7167447066838943, 0.718687998724491, 0.7206365595643126,
        0.7225904034885232, 0.7245495448210174, 0.7265139979245261, 0.7284837772007218,
        0.7304588970903234, 0.7324393720732029, 0.7344252166684908, 0.7364164454346837,
        0.7384130729697496, 0.7404151139112358, 0.7424225829363761, 0.7444354947621984,
        0.7464538641456323, 0.7484777058836176, 0.7505070348132126, 0.7525418658117031,
        0.7545822137967112, 0.7566280937263048, 0.7586795205991071, 0.7607365094544071,
        0.762799075372269, 0.7648672334736434, 0.7669409989204777, 0.7690203869158282,
        0.7711054127039704, 0.7731960915705107, 0.7752924388424999, 0.7773944698885442,
        0.7795022001189185, 0.7816156449856788, 0.7837348199827764, 0.7858597406461707,
        0.7879904225539431, 0.7901268813264122, 0.7922691326262467, 0.7944171921585818,
        0.7965710756711334, 0.7987307989543135, 0.8008963778413465, 0.8030678282083853,
        0.805245165974627, 0.8074284071024302, 0.8096175675974316, 0.8118126635086642,
        0.8140137109286738, 0.8162207259936375, 0.8184337248834821, 0.820652723822003,
        0.8228777390769823, 0.8251087869603088, 0.8273458838280969, 0.8295890460808079,
        0.8318382901633681, 0.8340936325652911, 0.8363550898207981, 0.8386226785089391,
        0.8408964152537144, 0.8431763167241966, 0.8454623996346523, 0.8477546807446661,
        0.8500531768592616, 0.8523579048290255, 0.8546688815502312, 0.8569861239649629,
        0.8593096490612387, 0.8616394738731368, 0.8639756154809185, 0.8663180910111553,
        0.8686669176368529, 0.871022112577578, 0.8733836930995842, 0.8757516765159389,
        0.8781260801866495, 0.8805069215187917, 0.8828942179666361, 0.8852879870317771,
        0.8876882462632604, 0.890095013257712, 0.8925083056594671, 0.8949281411607002,
        0.8973545375015533, 0.8997875124702672, 0.9022270839033115, 0.9046732696855155,
        0.9071260877501991, 0.909585556079304, 0.9120516927035263, 0.9145245157024483,
        0.9170040432046711, 0.9194902933879467, 0.9219832844793128, 0.9244830347552253,
        0.9269895625416926, 0.92950288621441, 0.9320230241988943, 0.9345499949706191,
        0.9370838170551498, 0.93962450902828, 0.9421720895161669, 0.9447265771954693,
        0.9472879907934827, 0.9498563490882775, 0.9524316709088368, 0.9550139751351947,
        0.9576032806985735, 0.9601996065815236, 0.9628029718180622, 0.9654133954938133,
        0.9680308967461471, 0.9706554947643201, 0.9732872087896164, 0.9759260581154889,
        0.9785720620876999, 0.9812252401044634, 0.9838856116165875, 0.9865531961276168,
        0.9892280131939752, 0.9919100824251095, 0.9945994234836328, 0.9972960560854698,
    ),
)
