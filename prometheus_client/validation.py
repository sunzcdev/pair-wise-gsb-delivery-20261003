from collections import Counter
import os
import re
from typing import Dict, List, NamedTuple, Optional, TYPE_CHECKING

from .samples import Sample

if TYPE_CHECKING:
    from .metrics_core import Metric

METRIC_NAME_RE = re.compile(r'^[a-zA-Z_:][a-zA-Z0-9_:]*$')
METRIC_LABEL_NAME_RE = re.compile(r'^[a-zA-Z_][a-zA-Z0-9_]*$')
RESERVED_METRIC_LABEL_NAME_RE = re.compile(r'^__.*$')


def _init_legacy_validation() -> bool:
    """Retrieve name validation setting from environment."""
    return os.environ.get("PROMETHEUS_LEGACY_NAME_VALIDATION", 'False').lower() in ('true', '1', 't')


_legacy_validation = _init_legacy_validation()


def get_legacy_validation() -> bool:
    """Return the current status of the legacy validation setting."""
    return _legacy_validation


def disable_legacy_validation():
    """Disable legacy name validation, instead allowing all UTF8 characters."""
    global _legacy_validation
    _legacy_validation = False


def enable_legacy_validation():
    """Enable legacy name validation instead of allowing all UTF8 characters."""
    global _legacy_validation
    _legacy_validation = True


def _validate_metric_name(name: str) -> None:
    """Raises ValueError if the provided name is not a valid metric name.
    
    This check uses the global legacy validation setting to determine the validation scheme.
    """
    if not name:
        raise ValueError("metric name cannot be empty")
    if _legacy_validation:
        if not METRIC_NAME_RE.match(name):
            raise ValueError("invalid metric name " + name)
    try:
        name.encode('utf-8')
    except UnicodeDecodeError:
        raise ValueError("invalid metric name " + name)


def _is_valid_legacy_metric_name(name: str) -> bool:
    """Returns true if the provided metric name conforms to the legacy validation scheme."""
    if len(name) == 0:
        return False
    return METRIC_NAME_RE.match(name) is not None


def _validate_metric_label_name_token(tok: str) -> None:
    """Raises ValueError if a parsed label name token is invalid. 
    
    UTF-8 names must be quoted.
    """
    if not tok:
        raise ValueError("invalid label name token " + tok)
    quoted = tok[0] == '"' and tok[-1] == '"'
    if not quoted or _legacy_validation:
        if not METRIC_LABEL_NAME_RE.match(tok):
            raise ValueError("invalid label name token " + tok)
        return
    try:
        tok.encode('utf-8')
    except UnicodeDecodeError:
        raise ValueError("invalid label name token " + tok)


def _validate_labelname(l):
    """Raises ValueError if the provided name is not a valid label name.
    
    This check uses the global legacy validation setting to determine the validation scheme.
    """
    if get_legacy_validation():
        if not METRIC_LABEL_NAME_RE.match(l):
            raise ValueError('Invalid label metric name: ' + l)
        if RESERVED_METRIC_LABEL_NAME_RE.match(l):
            raise ValueError('Reserved label metric name: ' + l)
    else:
        try:
            l.encode('utf-8')
        except UnicodeDecodeError:
            raise ValueError('Invalid label metric name: ' + l)
        if RESERVED_METRIC_LABEL_NAME_RE.match(l):
            raise ValueError('Reserved label metric name: ' + l)
        

def _is_valid_legacy_labelname(l: str) -> bool:
    """Returns true if the provided label name conforms to the legacy validation scheme."""
    if len(l) == 0:
        return False
    if METRIC_LABEL_NAME_RE.match(l) is None:
        return False
    return RESERVED_METRIC_LABEL_NAME_RE.match(l) is None


def _validate_labelnames(cls, labelnames):
    """Raises ValueError if any of the provided names is not a valid label name.
    
    This check uses the global legacy validation setting to determine the validation scheme.
    """
    labelnames = tuple(labelnames)
    for l in labelnames:
        _validate_labelname(l)
        if l in cls._reserved_labelnames:
            raise ValueError('Reserved label metric name: ' + l)
    # Duplicate names collapse distinct label sets during collection.
    duplicates = sorted(name for name, count in Counter(labelnames).items() if count > 1)
    if duplicates:
        raise ValueError(f'Duplicate label names: {duplicates!r}')
    return labelnames


def _validate_exemplar(exemplar):
    """Raises ValueError if the exemplar is invalid."""
    runes = 0
    for k, v in exemplar.items():
        _validate_labelname(k)
        runes += len(k)
        runes += len(v)
    if runes > 128:
        raise ValueError(f'Exemplar labels have {runes} UTF-8 characters, exceeding the limit of 128')


class MetricFamilyIssue(NamedTuple):
    """A single problem found while validating a metric family.

    Attributes:
        rule: Machine-readable identifier of the violated rule,
            e.g. 'histogram.buckets_sorted' or 'labels.inconsistent'.
        message: Human-readable description of the problem.
        sample_name: Name of the specific sample the issue points at,
            or None for family-level issues.
        sample_labels: Labels of the specific sample, or None.
        sample_index: Position of the sample in metric.samples, or None.
    """
    rule: str
    message: str
    sample_name: Optional[str] = None
    sample_labels: Optional[Dict[str, str]] = None
    sample_index: Optional[int] = None


class MetricFamilyValidationError(ValueError):
    """Raised when a metric family fails validation.

    Carries the list of MetricFamilyIssue objects that were found.
    """

    def __init__(self, metric_name: str, issues: List[MetricFamilyIssue]) -> None:
        self.metric_name = metric_name
        self.issues: List[MetricFamilyIssue] = list(issues)
        details = '; '.join(
            '{rule}: {message}{location}'.format(
                rule=issue.rule,
                message=issue.message,
                location=' [sample {} #{}]'.format(issue.sample_name, issue.sample_index)
                if issue.sample_name is not None else '',
            )
            for issue in self.issues
        )
        super().__init__(f'Invalid metric family {metric_name!r}: {details}')


# Sample name suffixes each metric type is expected to use, relative to the
# family name. Mirrors the families produced by metrics_core.
_DERIVED_SAMPLE_SUFFIXES = {
    'counter': ('_total', '_created'),
    'gauge': ('',),
    'unknown': ('',),
    'summary': ('', '_count', '_sum', '_created'),
    'histogram': ('_bucket', '_count', '_sum', '_created'),
    'gaugehistogram': ('_bucket', '_gcount', '_gsum'),
    'info': ('_info',),
    'stateset': ('',),
}

# Composite types with classic buckets: type -> (bucket, count, sum) suffixes.
_HISTOGRAM_LIKE_SUFFIXES = {
    'histogram': ('_bucket', '_count', '_sum'),
    'gaugehistogram': ('_bucket', '_gcount', '_gsum'),
}

# Types with paired count/sum samples.
_COUNT_SUM_SUFFIXES = {
    'histogram': ('_count', '_sum'),
    'gaugehistogram': ('_gcount', '_gsum'),
    'summary': ('_count', '_sum'),
}


def _sample_issue(rule: str, message: str, index: int, sample: Sample) -> MetricFamilyIssue:
    return MetricFamilyIssue(
        rule=rule,
        message=message,
        sample_name=sample.name,
        sample_labels=dict(sample.labels) if sample.labels is not None else None,
        sample_index=index,
    )


def _safe_float(value: object) -> Optional[float]:
    try:
        return float(value)  # type: ignore
    except (TypeError, ValueError):
        return None


def _parse_bucket_bound(le: object) -> Optional[float]:
    """Parse an 'le' label value. NaN is not a usable bucket boundary."""
    bound = _safe_float(le)
    if bound is None or bound != bound:
        return None
    return bound


def validate_metric_family(metric: "Metric") -> List[MetricFamilyIssue]:
    """Validate a single metric family and its samples.

    This is intended for custom collectors: problems such as unsorted
    histogram buckets or inconsistent label sets are reported here, before
    the family is exposed to a Prometheus scrape. The input metric is only
    read, never modified.

    Checks performed:
      * family type is a known metric type;
      * unit is a suffix of the family name;
      * every sample name is a valid derived name for the family type;
      * label names are consistent across the family's series, structural
        labels ('le', 'quantile', the stateset state label) aside;
      * composite structure: histogram buckets are sorted, cumulative,
        end at +Inf and agree with the count sample; count/sum pairs are
        complete;
      * special float values are legal for the metric type: NaN and
        negative values are rejected for counters and counts, while gauges
        and sums may use NaN and +/-Inf. Note that a negative bucket
        boundary is legal and is not the same problem as a negative count.

    Returns a list of MetricFamilyIssue, empty if the family is valid.
    """
    issues: List[MetricFamilyIssue] = []
    name = getattr(metric, 'name', None)
    typ = getattr(metric, 'type', None)
    unit = getattr(metric, 'unit', '') or ''
    samples = list(getattr(metric, 'samples', None) or [])

    # Imported here to avoid a circular import at module load time.
    from .metrics_core import METRIC_TYPES

    if not isinstance(name, str) or not name:
        issues.append(MetricFamilyIssue('family.name', 'metric family has no valid name'))
        name = ''
    if not isinstance(typ, str) or typ not in METRIC_TYPES:
        issues.append(MetricFamilyIssue('family.type', f'invalid metric type: {typ!r}'))
        typ = ''
    if unit and not name.endswith('_' + unit):
        issues.append(MetricFamilyIssue(
            'family.unit',
            f'metric name {name!r} does not end with the unit suffix {"_" + unit!r}'))

    allowed_suffixes = _DERIVED_SAMPLE_SUFFIXES.get(typ, ())
    bucket_count_sum = _HISTOGRAM_LIKE_SUFFIXES.get(typ)
    count_sum = _COUNT_SUM_SUFFIXES.get(typ)

    expected_label_names = None
    seen_series: Dict[tuple, int] = {}
    bucket_groups: Dict[frozenset, list] = {}
    count_samples: Dict[frozenset, tuple] = {}
    sum_samples: Dict[frozenset, tuple] = {}

    for index, sample in enumerate(samples):
        sname = sample.name
        labels = sample.labels or {}

        for lname in labels:
            if RESERVED_METRIC_LABEL_NAME_RE.match(lname):
                issues.append(_sample_issue(
                    'labels.reserved', f'label name {lname!r} is reserved', index, sample))

        series_key = (sname, frozenset(labels.items()))
        if series_key in seen_series:
            issues.append(_sample_issue(
                'series.duplicate',
                f'duplicate of sample #{seen_series[series_key]} with the same name and labels',
                index, sample))
        else:
            seen_series[series_key] = index

        suffix = None
        for candidate in allowed_suffixes:
            if sname == name + candidate:
                suffix = candidate
                break
        if suffix is None:
            if allowed_suffixes:
                issues.append(_sample_issue(
                    'sample.derived_name',
                    f'sample name {sname!r} is not a valid derived name for a {typ} family named {name!r}',
                    index, sample))
            # Without a recognized suffix no further per-type checks apply.
            continue

        # Structural labels depend on the role of the sample.
        structural = set()
        if suffix == '_bucket':
            if 'le' not in labels:
                issues.append(_sample_issue(
                    f'{typ}.bucket_le_missing', "bucket sample is missing the 'le' label", index, sample))
            structural.add('le')
        elif 'le' in labels:
            issues.append(_sample_issue(
                'sample.le_misplaced', "'le' label is only valid on _bucket samples", index, sample))
        if typ == 'summary' and suffix == '':
            structural.add('quantile')
        elif 'quantile' in labels:
            issues.append(_sample_issue(
                'sample.quantile_misplaced', "'quantile' label is only valid on summary quantile samples", index, sample))
        if typ == 'stateset':
            if name not in labels:
                issues.append(_sample_issue(
                    'stateset.state_label_missing',
                    f'stateset sample is missing the state label named after the metric {name!r}',
                    index, sample))
            structural.add(name)

        signature = frozenset(k for k in labels if k not in structural)
        if expected_label_names is None:
            expected_label_names = signature
        elif signature != expected_label_names:
            issues.append(_sample_issue(
                'labels.inconsistent',
                'label names {} differ from the rest of the family {}'.format(
                    sorted(signature), sorted(expected_label_names)),
                index, sample))

        # Special float values are judged per metric type: NaN and negative
        # values are never legal for counters or counts, while gauges and
        # sums may legitimately use NaN and +/-Inf.
        value = sample.value
        fvalue = _safe_float(value) if value is not None else None
        if value is None:
            if getattr(sample, 'native_histogram', None) is None:
                issues.append(_sample_issue('sample.value_missing', 'sample has no value', index, sample))
        elif fvalue is None:
            issues.append(_sample_issue(
                'sample.value_invalid', f'sample value {value!r} is not a number', index, sample))
        else:
            isnan = fvalue != fvalue
            if typ == 'counter' and suffix == '_total':
                if isnan:
                    issues.append(_sample_issue(
                        'counter.value_nan', 'counter value must not be NaN', index, sample))
                elif fvalue < 0:
                    issues.append(_sample_issue(
                        'counter.value_negative', f'counter value {fvalue} is negative', index, sample))
            elif suffix == '_bucket':
                if isnan:
                    issues.append(_sample_issue(
                        f'{typ}.bucket_count_nan', 'bucket count must not be NaN', index, sample))
                elif fvalue < 0:
                    issues.append(_sample_issue(
                        f'{typ}.bucket_count_negative', f'bucket count {fvalue} is negative', index, sample))
            elif suffix in ('_count', '_gcount'):
                if isnan:
                    issues.append(_sample_issue(
                        f'{typ}.count_nan', 'count must not be NaN', index, sample))
                elif fvalue < 0:
                    issues.append(_sample_issue(
                        f'{typ}.count_negative', f'count {fvalue} is negative', index, sample))
            elif typ == 'stateset':
                if isnan or fvalue not in (0.0, 1.0):
                    issues.append(_sample_issue(
                        'stateset.value', f'stateset value {fvalue} is not 0 or 1', index, sample))
            elif typ == 'info':
                if isnan or fvalue != 1.0:
                    issues.append(_sample_issue(
                        'info.value', f'info value {fvalue} is not 1', index, sample))

        # Group composite samples by their non-structural labels.
        if bucket_count_sum is not None or count_sum is not None:
            base_key = frozenset((k, v) for k, v in labels.items() if k not in structural)
            if bucket_count_sum is not None and suffix == bucket_count_sum[0]:
                bound = _parse_bucket_bound(labels.get('le'))
                if bound is None:
                    issues.append(_sample_issue(
                        f'{typ}.bucket_bound_invalid',
                        f'bucket boundary {labels.get("le")!r} is not a valid float',
                        index, sample))
                bucket_groups.setdefault(base_key, []).append((index, sample, bound))
            elif count_sum is not None and suffix == count_sum[0]:
                count_samples[base_key] = (index, sample)
            elif count_sum is not None and suffix == count_sum[1]:
                sum_samples[base_key] = (index, sample)

    # Composite structure checks for histogram-like families.
    if bucket_count_sum is not None:
        count_suffix = bucket_count_sum[1]
        for base_key, entries in bucket_groups.items():
            valid_entries = [(i, s, b) for i, s, b in entries if b is not None]
            for (pi, ps, pb), (ci, cs, cb) in zip(valid_entries, valid_entries[1:]):
                if cb <= pb:
                    issues.append(_sample_issue(
                        f'{typ}.buckets_sorted',
                        f'bucket boundary {cb} is not greater than the previous boundary {pb}',
                        ci, cs))
                pvalue, cvalue = _safe_float(ps.value), _safe_float(cs.value)
                if (pvalue is not None and cvalue is not None
                        and pvalue == pvalue and cvalue == cvalue and cvalue < pvalue):
                    issues.append(_sample_issue(
                        f'{typ}.buckets_cumulative',
                        f'bucket count {cvalue} is lower than the previous bucket count {pvalue}',
                        ci, cs))
            if valid_entries and valid_entries[-1][2] != float('inf'):
                li, ls, lb = valid_entries[-1]
                issues.append(_sample_issue(
                    f'{typ}.bucket_inf_missing',
                    f'last bucket boundary is {lb}, expected +Inf',
                    li, ls))
            count_entry = count_samples.get(base_key)
            if count_entry is not None and valid_entries and valid_entries[-1][2] == float('inf'):
                ci, cs = count_entry
                count_value = _safe_float(cs.value)
                inf_value = _safe_float(valid_entries[-1][1].value)
                if (count_value is not None and inf_value is not None
                        and count_value == count_value and count_value != inf_value):
                    issues.append(_sample_issue(
                        f'{typ}.count_mismatch',
                        f'{count_suffix} value {count_value} does not match the +Inf bucket count {inf_value}',
                        ci, cs))

    # Count/sum pairs must be complete. Families with negative bucket
    # boundaries legitimately omit both, so only unpaired samples are issues.
    if count_sum is not None:
        count_suffix, sum_suffix = count_sum
        for base_key, (ci, cs) in count_samples.items():
            if base_key not in sum_samples:
                issues.append(_sample_issue(
                    f'{typ}.count_sum_unpaired',
                    f'{count_suffix} sample has no matching {sum_suffix} sample',
                    ci, cs))
        for base_key, (si, ss) in sum_samples.items():
            if base_key not in count_samples:
                issues.append(_sample_issue(
                    f'{typ}.count_sum_unpaired',
                    f'{sum_suffix} sample has no matching {count_suffix} sample',
                    si, ss))

    return issues


def assert_valid_metric_family(metric: "Metric") -> None:
    """Raise MetricFamilyValidationError if the metric family is invalid.

    Like validate_metric_family, this only reads the input metric.
    """
    issues = validate_metric_family(metric)
    if issues:
        raise MetricFamilyValidationError(getattr(metric, 'name', ''), issues)
