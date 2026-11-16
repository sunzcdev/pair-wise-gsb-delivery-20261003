from copy import copy, deepcopy

import pytest

from boltons.statsutils import Stats, mode


def test_stats_basic():
    da = Stats(range(20))
    assert da.mean == 9.5
    assert round(da.std_dev, 2) == 5.77
    assert da.variance == 33.25
    assert da.skewness == 0
    assert round(da.kurtosis, 1) == 1.9
    assert da.median == 9.5


def test_pearson_type_covers_kappa_ge_zero():
    assert Stats([8, 4, 4, 4, 5, 1, 6, 5]).pearson_type == 4
    assert Stats([5, 5, 2, 4, 5, 9, 5, 5]).pearson_type == 5
    assert Stats([0, 0, 0, 0, 0, 0, -11, 17]).pearson_type == 6


def test_mode():
    assert Stats([2, 1, 3, 1]).mode == 1
    # ties resolve to the value seen first in the data
    assert mode([1, 1, 2, 2, 3]) == 1
    # non-numeric, categorical data is supported
    assert mode(['a', 'b', 'b', 'c', 'c', 'c']) == 'c'
    # empty data falls back to the configured default
    assert Stats([], default=None).mode is None


def test_pearson_type_zero_denominator():
    # Supply exact moments to exercise c0 == 0, c2 == 0, and the
    # normal-distribution boundary without sampling noise.
    for skewness, kurtosis, expected in ((2.0, 3.0, 1),
                                         (2.0, 9.0, 3),
                                         (0.0, 3.0, 0)):
        stats = Stats([0])
        stats.skewness = skewness
        stats.kurtosis = kurtosis
        assert stats.pearson_type == expected


def test_histogram_zero_interquartile_range():
    for data in ([5] * 10, [0] * 10 + [100]):
        counts = Stats(data).get_histogram_counts()
        assert counts == [(float(min(data)), len(data))]


def test_sorted_measures_keep_data_order():
    # see #534
    stats = Stats([2, 1, 2, 1])
    assert stats.median == 1.5
    assert stats.mode == 2
    assert stats.data == [2, 1, 2, 1]


def test_clear_cache_resets_is_sorted():
    # see #570
    stats = Stats([1, 2, 3], is_sorted=True)
    assert stats.min == 1
    stats.data.append(0)
    stats.clear_cache()
    assert stats.min == 0
    assert stats.median == 1.5


def test_format_histogram_counts_all_zero():
    # see #529
    from boltons.statsutils import format_histogram_counts

    assert format_histogram_counts([(0.0, 0), (1.0, 0)]) == '0.0: 0 |\n1.0: 0 |'


def test_frozen_snapshot():
    src = [3, 1, 2]
    stats = Stats(src, frozen=True)
    assert stats.frozen is True
    assert stats.version == 0

    # the snapshot is decoupled from the source list
    src.append(100)
    assert stats.mean == 2.0
    assert stats.median == 2
    assert len(stats) == 3

    # the data container itself is immutable
    with pytest.raises(AttributeError):
        stats.data.append(4)

    # every editing method rejects frozen snapshots
    for edit in (lambda: stats.add(4),
                 lambda: stats.extend([4, 5]),
                 lambda: stats.set_data([1, 2]),
                 lambda: stats.trim_relative(0.1)):
        with pytest.raises(RuntimeError):
            edit()

    # failed edits leave data and cache untouched
    assert stats.mean == 2.0
    assert stats.version == 0


def test_edit_methods_invalidate_dependent_stats():
    stats = Stats([1, 2, 3])
    assert stats.version == 0

    # populate caches across quantiles, moments, and histograms
    assert stats.mean == 2.0
    assert stats.median == 2
    assert stats.variance == 2.0 / 3.0
    assert sum(c for _, c in stats.get_histogram_counts()) == 3

    stats.add(4)
    assert stats.version == 1
    # every measure reflects the same, new version of the data
    assert stats.mean == 2.5
    assert stats.median == 2.5
    assert stats.variance == 1.25
    assert stats.get_quantile(0.25) == 1.75
    assert sum(c for _, c in stats.get_histogram_counts()) == 4

    stats.extend([5, 6])
    assert stats.version == 2
    assert stats.mean == 3.5
    assert stats.max == 6

    stats.set_data([10, 20, 30])
    assert stats.version == 3
    assert stats.mean == 20.0
    assert stats.median == 20
    assert stats.data == [10, 20, 30]


def test_failed_edits_preserve_data_and_cache():
    stats = Stats([1, 2, 3])
    assert stats.mean == 2.0  # prime the cache
    assert '_mean' in stats.__dict__

    def bad_iter():
        yield 4
        raise ValueError('boom')

    with pytest.raises(ValueError):
        stats.extend(bad_iter())
    assert stats.data == [1, 2, 3]
    assert '_mean' in stats.__dict__  # cache not invalidated
    assert stats.mean == 2.0
    assert stats.version == 0

    with pytest.raises(TypeError):
        stats.set_data(None)
    assert stats.data == [1, 2, 3]
    assert stats.mean == 2.0
    assert stats.version == 0


def test_set_data_is_sorted():
    stats = Stats([3, 1, 2])
    stats.set_data([1, 2, 3], is_sorted=True)
    assert stats.median == 2
    assert stats.min == 1
    assert stats.max == 3


def test_copy_does_not_share_mutable_cache():
    stats = Stats([3, 1, 2])
    assert stats.median == 2  # populates the sorted-data cache
    assert stats._sorted_data is not None

    for dup in (copy(stats), deepcopy(stats)):
        assert dup is not stats
        assert dup.data == [3, 1, 2]
        assert dup._sorted_data is not stats._sorted_data
        assert dup.mean == stats.mean

        # edits to the copy do not affect the original, and vice versa
        dup.add(100)
        assert dup.mean == 26.5
        assert stats.mean == 2.0
        assert stats.data == [3, 1, 2]
        assert stats.version == 0
        assert dup.version == 1


def test_copy_preserves_default_and_precision():
    stats = Stats([], default=float('nan'))
    stats._pearson_precision = 4

    dup = copy(stats)
    assert len(dup) == 0
    assert dup.default is stats.default
    assert dup.mean is stats.default  # empty data falls back to default
    assert dup._pearson_precision == 4
    assert dup.frozen is False

    frozen_stats = Stats([1, 2, 3], frozen=True)
    frozen_dup = copy(frozen_stats)
    assert frozen_dup.frozen is True
    with pytest.raises(RuntimeError):
        frozen_dup.add(4)


def test_empty_data_edit_roundtrip():
    stats = Stats([], default=None)
    assert stats.mean is None
    stats.add(2.0)
    assert stats.mean == 2.0
    assert stats.median == 2.0
    stats.set_data([])
    assert stats.mean is None
    assert stats.version == 2
