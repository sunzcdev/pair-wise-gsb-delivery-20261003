import math
import unittest

from prometheus_client.samples import (
    _bound, BucketSpan, CUSTOM_BUCKETS_SCHEMA, Exemplar,
    EXPONENTIAL_SCHEMA_MAX, EXPONENTIAL_SCHEMA_MIN, merge_native_histograms,
    native_histogram_bucket_total, NativeHistogram, NativeHistogramError,
    reduce_native_histogram_resolution, UnsupportedSchemaError,
)


def decode(spans, deltas):
    """Decode spans and deltas into a list of (bucket index, population) pairs."""
    buckets = []
    index = 0
    population = 0
    position = 0
    for offset, length in spans or ():
        index += offset
        for _ in range(length):
            population += (deltas or ())[position]
            buckets.append((index, population))
            position += 1
            index += 1
    return buckets


def canonical_spans(positions):
    """Return the canonical spans for a sequence of ascending bucket indices."""
    spans = []
    previous = 0
    for position in positions:
        if not spans:
            spans.append(BucketSpan(position, 1))
        elif position == previous + 1:
            spans[-1] = BucketSpan(spans[-1].offset, spans[-1].length + 1)
        else:
            spans.append(BucketSpan(position - previous - 1, 1))
        previous = position
    return tuple(spans)


def histogram(
    positive=None,
    negative=None,
    schema=0,
    zero_threshold=0.0,
    zero_count=0.0,
    sum_value=0.0,
    count_value=None,
    pos_spans=Ellipsis,
    pos_deltas=None,
    neg_spans=Ellipsis,
    neg_deltas=None,
):
    """Build a native histogram from mappings of bucket index to population.

    ``positive`` and ``negative`` are dictionaries of bucket index to absolute
    population; everything is encoded as canonical spans and deltas unless an
    explicit span or delta sequence is passed to exercise non-canonical input.
    """
    positive = dict(positive or {})
    negative = dict(negative or {})
    if pos_spans is Ellipsis:
        pos_spans = canonical_spans(sorted(positive))
        pos_deltas = _deltas(sorted(positive), positive)
    if neg_spans is Ellipsis:
        neg_spans = canonical_spans(sorted(negative))
        neg_deltas = _deltas(sorted(negative), negative)
    if count_value is None:
        count_value = (sum(positive.values()) + sum(negative.values()) + zero_count)
    return NativeHistogram(
        count_value=count_value,
        sum_value=sum_value,
        schema=schema,
        zero_threshold=zero_threshold,
        zero_count=zero_count,
        pos_spans=pos_spans or None,
        neg_spans=neg_spans or None,
        pos_deltas=pos_deltas or None,
        neg_deltas=neg_deltas or None,
    )


def _deltas(positions, populations):
    deltas = []
    previous = 0
    for position in positions:
        deltas.append(populations[position] - previous)
        previous = populations[position]
    return tuple(deltas)


def merged_buckets(native_histogram):
    """Return the merged result as two {index: population} dictionaries."""
    return (
        dict(decode(native_histogram.pos_spans, native_histogram.pos_deltas)),
        dict(decode(native_histogram.neg_spans, native_histogram.neg_deltas)),
    )


class MergeSchemasTest(unittest.TestCase):
    def test_downscales_the_finer_input_to_the_coarser_schema(self):
        fine = histogram({1: 1, 2: 2, 3: 3, 4: 4}, schema=2)
        coarse = histogram({1: 5}, schema=0)
        result = merge_native_histograms(fine, coarse)
        self.assertEqual(result.schema, 0)
        # Schema 2 buckets 1-4 all live in schema 0 bucket 1.
        self.assertEqual(merged_buckets(result), ({1: 15}, {}))
        self.assertEqual(result.count_value, 15)

    def test_reduces_to_the_requested_schema_even_below_the_coarsest_input(self):
        first = histogram({1: 1, 2: 2}, schema=1)
        second = histogram({0: 3}, schema=0)
        result = merge_native_histograms(first, second, schema=-1)
        self.assertEqual(result.schema, -1)
        # Schema 1 buckets 1 and 2 both cover values in (1, 2], which is schema
        # -1 bucket 1; schema 0 bucket 0 covers (0.5, 1], schema -1 bucket 0.
        self.assertEqual(merged_buckets(result), ({0: 3, 1: 3}, {}))

    def test_rejects_a_schema_finer_than_the_coarsest_input(self):
        first = histogram({0: 1}, schema=0)
        second = histogram({0: 1}, schema=-1)
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(first, second, schema=1)
        self.assertIn('finer', str(raised.exception))
        self.assertIn('resolution can only be reduced', str(raised.exception))

    def test_default_schema_is_the_coarsest_input_schema(self):
        first = histogram({0: 1}, schema=4)
        second = histogram({0: 1}, schema=-2)
        self.assertEqual(merge_native_histograms(first, second).schema, -2)

    def test_matches_the_prometheus_resolution_reduction_example(self):
        # The ReduceResolution test case from Prometheus' model/histogram.
        source = NativeHistogram(
            count_value=9.0,
            sum_value=0.0,
            schema=0,
            zero_threshold=0.0,
            zero_count=0.0,
            pos_spans=(BucketSpan(-2, 4), BucketSpan(0, 0), BucketSpan(3, 2)),
            pos_deltas=(1, 2, -2, 1, -1, 0),
            neg_spans=(BucketSpan(0, 4), BucketSpan(0, 0), BucketSpan(3, 2)),
            neg_deltas=(1, 2, -2, 1, -1, 0),
        )
        result = reduce_native_histogram_resolution(source, -1)
        self.assertEqual(result.schema, -1)
        self.assertEqual(result.pos_spans, (BucketSpan(-1, 3), BucketSpan(1, 1)))
        self.assertEqual(result.pos_deltas, (1, 3, -2, 0))
        self.assertEqual(result.neg_spans, (BucketSpan(0, 3), BucketSpan(1, 1)))
        self.assertEqual(result.neg_deltas, (1, 3, -2, 0))
        self.assertEqual(
            native_histogram_bucket_total(result), native_histogram_bucket_total(source))

    def test_reindexing_uses_the_bucket_that_contains_the_source_bucket(self):
        # Schema 1 bucket -1 covers (0.5, 0.7071] and belongs to schema 0
        # bucket 0 (0.5, 1]; bucket 3 covers (2, 2.828] and belongs to schema 0
        # bucket 2 (2, 4].
        result = merge_native_histograms(histogram({-1: 1, 0: 2, 3: 4}, schema=1), schema=0)
        self.assertEqual(merged_buckets(result), ({0: 3, 2: 4}, {}))

    def test_negative_schemas_reduce_correctly(self):
        result = merge_native_histograms(histogram({0: 1, 1: 1, 2: 1, 3: 1}, schema=-1), schema=-3)
        # Schema -1 boundaries are squares: (0.25, 1], (1, 4], (4, 16], (16,
        # 64].  Schema -3 uses (0.25, 1], (1, 256], ... so the last three
        # schema -1 buckets share one schema -3 bucket.
        self.assertEqual(merged_buckets(result), ({0: 1, 1: 3}, {}))


class MergeCountsTest(unittest.TestCase):
    def test_count_and_sum_are_added(self):
        first = histogram({0: 3}, count_value=3, sum_value=1.5)
        second = histogram({0: 4}, count_value=4, sum_value=2.25)
        result = merge_native_histograms(first, second)
        self.assertEqual(result.count_value, 7)
        self.assertEqual(result.sum_value, 3.75)

    def test_bucket_populations_are_preserved(self):
        first = histogram({-2: 1, 0: 4, 5: 2}, {-1: 3, 7: 1})
        second = histogram({1: 6, 5: 1}, {0: 2}, schema=2)
        result = merge_native_histograms(first, second, schema=0)
        self.assertEqual(
            native_histogram_bucket_total(result), native_histogram_bucket_total(first)
            + native_histogram_bucket_total(second))
        positive, negative = merged_buckets(result)
        self.assertEqual(sum(positive.values()) + sum(negative.values()),
                         native_histogram_bucket_total(result) - result.zero_count)

    def test_float_counts_are_summed_exactly(self):
        first = histogram({0: 1}, count_value=0.1, sum_value=0.1)
        second = histogram({0: 1}, count_value=0.2, sum_value=0.2)
        result = merge_native_histograms(first, second)
        self.assertEqual(result.count_value, 0.30000000000000004)
        self.assertEqual(result.sum_value, 0.30000000000000004)

    def test_nan_sum_is_propagated(self):
        result = merge_native_histograms(
            histogram({0: 1}, sum_value=math.nan), histogram({0: 1}, sum_value=3.0))
        self.assertTrue(math.isnan(result.sum_value))

    def test_opposite_infinite_sums_produce_nan(self):
        result = merge_native_histograms(
            histogram({0: 1}, sum_value=math.inf), histogram({0: 1}, sum_value=-math.inf))
        self.assertTrue(math.isnan(result.sum_value))

    def test_infinite_sum_is_propagated(self):
        result = merge_native_histograms(
            histogram({0: 1}, sum_value=math.inf), histogram({0: 1}, sum_value=3.0))
        self.assertEqual(result.sum_value, math.inf)

    def test_bucket_total_reports_the_stored_population(self):
        subject = histogram({0: 2, 3: 1}, {1: 4}, zero_count=5.0)
        self.assertEqual(native_histogram_bucket_total(subject), 12.0)


class MergeCanonicalEncodingTest(unittest.TestCase):
    def test_consecutive_buckets_share_one_span(self):
        result = merge_native_histograms(histogram({1: 1}), histogram({2: 1, 3: 1}))
        self.assertEqual(result.pos_spans, (BucketSpan(1, 3),))
        self.assertEqual(result.pos_deltas, (1, 0, 0))

    def test_gaps_between_spans_stay_gaps(self):
        result = merge_native_histograms(histogram({0: 2}), histogram({7: 5}))
        self.assertEqual(result.pos_spans, (BucketSpan(0, 1), BucketSpan(6, 1)))
        self.assertEqual(result.pos_deltas, (2, 3))
        self.assertEqual(merged_buckets(result), ({0: 2, 7: 5}, {}))

    def test_very_sparse_populations_do_not_lose_observations(self):
        sparse = histogram({0: 1, 100000: 2, 2000000000: 3})
        result = merge_native_histograms(sparse)
        self.assertEqual(merged_buckets(result), ({0: 1, 100000: 2, 2000000000: 3}, {}))
        # The last span starts 99999 buckets after the previous one ends, so the
        # gap is kept as an offset instead of being materialized.
        self.assertEqual(result.pos_spans[2], BucketSpan(1999899999, 1))

    def test_empty_spans_are_processed_without_losing_observations(self):
        source = NativeHistogram(
            count_value=12,
            sum_value=0.0,
            schema=0,
            zero_threshold=0.0,
            zero_count=0.0,
            pos_spans=(
                BucketSpan(-1, 0),
                BucketSpan(1, 2),
                BucketSpan(0, 0),
                BucketSpan(3, 1),
            ),
            pos_deltas=(1, 4, 1),
        )
        self.assertEqual(decode(source.pos_spans, source.pos_deltas),
                         [(0, 1), (1, 5), (5, 6)])
        result = merge_native_histograms(source)
        self.assertEqual(merged_buckets(result), ({0: 1, 1: 5, 5: 6}, {}))
        self.assertEqual(native_histogram_bucket_total(result), 12.0)

    def test_empty_spans_that_cover_everything_produce_no_side(self):
        source = NativeHistogram(
            count_value=0.0,
            sum_value=0.0,
            schema=0,
            zero_threshold=0.0,
            zero_count=0.0,
            pos_spans=(BucketSpan(0, 0), BucketSpan(5, 0)),
            neg_spans=(BucketSpan(0, 0),),
        )
        result = merge_native_histograms(source)
        self.assertIsNone(result.pos_spans)
        self.assertIsNone(result.pos_deltas)
        self.assertIsNone(result.neg_spans)
        self.assertIsNone(result.neg_deltas)
        self.assertEqual(result.zero_count, 0.0)

    def test_negative_deltas_are_reencoded(self):
        source = NativeHistogram(
            count_value=8,
            sum_value=0.0,
            schema=0,
            zero_threshold=0.0,
            zero_count=0.0,
            pos_spans=(BucketSpan(0, 3),),
            pos_deltas=(5, -4, 7),
        )
        result = merge_native_histograms(source)
        self.assertEqual(result.pos_deltas, (5, -4, 7))
        self.assertEqual(merged_buckets(result), ({0: 5, 1: 1, 2: 8}, {}))

    def test_zero_population_buckets_are_trimmed(self):
        source = NativeHistogram(
            count_value=4,
            sum_value=0.0,
            schema=0,
            zero_threshold=0.0,
            zero_count=0.0,
            pos_spans=(BucketSpan(0, 4),),
            pos_deltas=(4, -4, 0, 0),
        )
        self.assertEqual(decode(source.pos_spans, source.pos_deltas),
                         [(0, 4), (1, 0), (2, 0), (3, 0)])
        result = merge_native_histograms(source)
        self.assertEqual(result.pos_spans, (BucketSpan(0, 1),))
        self.assertEqual(result.pos_deltas, (4,))

    def test_negative_side_is_preserved(self):
        result = merge_native_histograms(
            histogram(negative={0: 1, 1: 3}), histogram(negative={1: 2, 4: 1}))
        self.assertEqual(merged_buckets(result), ({}, {0: 1, 1: 5, 4: 1}))
        self.assertEqual(result.neg_spans, (BucketSpan(0, 2), BucketSpan(2, 1)))
        self.assertEqual(result.neg_deltas, (1, 4, -4))

    def test_merging_one_histogram_is_idempotent(self):
        source = histogram({-1: 2, 3: 1}, {-2: 1, 0: 4}, zero_count=2.0)
        once = merge_native_histograms(source)
        twice = merge_native_histograms(once)
        self.assertEqual(once, twice)

    def test_merging_accepts_an_iterable_or_separate_arguments(self):
        first = histogram({0: 1})
        second = histogram({1: 2})
        third = histogram({2: 3})
        expected = merge_native_histograms(first, second, third)
        self.assertEqual(merge_native_histograms([first, second, third]), expected)

    def test_result_is_a_native_histogram(self):
        self.assertIsInstance(merge_native_histograms(histogram({0: 1})), NativeHistogram)


class MergeZeroBucketTest(unittest.TestCase):
    def test_equal_thresholds_add_the_zero_buckets(self):
        first = histogram({0: 1}, zero_threshold=0.5, zero_count=2.0)
        second = histogram({0: 1}, zero_threshold=0.5, zero_count=3.0)
        result = merge_native_histograms(first, second)
        self.assertEqual(result.zero_threshold, 0.5)
        self.assertEqual(result.zero_count, 5.0)
        self.assertEqual(merged_buckets(result), ({0: 2}, {}))

    def test_largest_threshold_wins_and_buckets_inside_it_are_absorbed(self):
        # Schema 0 bucket 0 covers (0.5, 1], bucket -1 covers (0.25, 0.5].
        narrow = histogram({0: 5}, {0: 3}, zero_threshold=0.25, zero_count=1.0)
        wide = histogram({1: 2}, zero_threshold=0.5, zero_count=4.0)
        result = merge_native_histograms(narrow, wide)
        self.assertEqual(result.zero_threshold, 0.5)
        # Narrow's bucket 0 (values up to 1) stays, its negative bucket 0
        # (values down to -1) stays; nothing was fully inside (0.25, 0.5].
        self.assertEqual(merged_buckets(result), ({0: 5, 1: 2}, {0: 3}))
        self.assertEqual(result.zero_count, 5.0)

    def test_threshold_is_raised_when_it_splits_a_populated_bucket(self):
        # Schema 0: bucket 0 covers (0.5, 1].  A threshold of 0.75 falls inside
        # it, so it must be raised to the bucket's upper bound of 1.0.
        populated = histogram({0: 7}, zero_count=1.0)
        result = merge_native_histograms(populated, zero_threshold=0.75)
        self.assertEqual(result.zero_threshold, 1.0)
        self.assertEqual(result.zero_count, 8.0)
        self.assertIsNone(result.pos_spans)
        self.assertEqual(native_histogram_bucket_total(result), 8.0)

    def test_raising_the_threshold_repeats_until_it_clears_every_bucket(self):
        # Threshold 0.75 lands in schema 0 bucket 0; raising it to 1.0 stays a
        # boundary, but with a second histogram whose bucket 0 is empty and
        # bucket 1 populated, the loop must still terminate at a boundary.
        first = histogram({0: 7})
        result = merge_native_histograms(first, zero_threshold=0.9)
        self.assertEqual(result.zero_threshold, 1.0)
        self.assertEqual(result.zero_count, 7.0)

    def test_raised_threshold_absorbs_the_partially_covered_bucket(self):
        # Two populated schema 0 buckets, threshold 0.6 is inside bucket 0
        # (0.5, 1]; the threshold moves to 1.0 and bucket 0 is absorbed while
        # bucket 1 (1, 2] stays.
        source = histogram({0: 4, 1: 6})
        result = merge_native_histograms(source, zero_threshold=0.6)
        self.assertEqual(result.zero_threshold, 1.0)
        self.assertEqual(result.zero_count, 4.0)
        self.assertEqual(merged_buckets(result), ({1: 6}, {}))

    def test_threshold_that_is_already_a_boundary_is_kept(self):
        source = histogram({0: 4, 1: 6})
        result = merge_native_histograms(source, zero_threshold=1.0)
        self.assertEqual(result.zero_threshold, 1.0)
        self.assertEqual(result.zero_count, 4.0)
        self.assertEqual(merged_buckets(result), ({1: 6}, {}))

    def test_threshold_inside_an_empty_gap_is_kept(self):
        # Schema -1 bucket 1 covers (1, 4] and is the only populated bucket.
        # 2.0 falls inside it, so it is not a gap; use schema -1 with only
        # bucket 2 (4, 16] populated, then 2.0 lies in the empty bucket 1 and
        # the threshold is kept.
        source = histogram({2: 4}, schema=-1)
        result = merge_native_histograms(source, zero_threshold=2.0)
        self.assertEqual(result.zero_threshold, 2.0)
        self.assertEqual(result.zero_count, 0.0)
        self.assertEqual(merged_buckets(result), ({2: 4}, {}))

    def test_threshold_that_is_not_a_boundary_but_splits_nothing_is_kept(self):
        # Schema -1 bucket 0 covers (0.5, 1]; 0.75 is inside it, so a histogram
        # with no bucket 0 at all keeps 0.75.
        source = histogram({2: 4}, schema=-1)
        result = merge_native_histograms(source, zero_threshold=0.75)
        self.assertEqual(result.zero_threshold, 0.75)
        self.assertEqual(merged_buckets(result), ({2: 4}, {}))

    def test_negative_buckets_are_absorbed_by_mirrored_boundaries(self):
        source = histogram({}, {0: 7, 1: 5, 2: 3})
        result = merge_native_histograms(source, zero_threshold=1.0)
        self.assertEqual(result.zero_threshold, 1.0)
        # Negative bucket i covers [-bound(i), -bound(i - 1)), so bucket 0 is
        # (-1, -0.5], entirely inside the [-1, 1] zero bucket, while buckets 1
        # and 2 cover (-2, -1] and (-4, -2] and stay.
        self.assertEqual(result.zero_count, 7.0)
        self.assertEqual(merged_buckets(result), ({}, {1: 5, 2: 3}))

    def test_a_larger_requested_threshold_is_allowed(self):
        source = histogram({0: 1, 1: 2}, zero_count=3.0)
        result = merge_native_histograms(source, zero_threshold=2.0)
        self.assertEqual(result.zero_threshold, 2.0)
        self.assertEqual(result.zero_count, 6.0)
        self.assertIsNone(result.pos_spans)

    def test_a_smaller_requested_threshold_is_rejected(self):
        source = histogram({0: 1}, zero_threshold=1.0)
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(source, zero_threshold=0.25)
        self.assertIn('cannot be split back', str(raised.exception))

    def test_the_threshold_is_raised_once_for_all_inputs_together(self):
        # The first histogram has a populated bucket that 0.75 splits, so the
        # final threshold becomes 1.0 and the second histogram's bucket 0 joins
        # the zero bucket as well.
        first = histogram({0: 2})
        second = histogram({0: 3}, zero_threshold=0.0)
        result = merge_native_histograms(first, second, zero_threshold=0.75)
        self.assertEqual(result.zero_threshold, 1.0)
        self.assertEqual(result.zero_count, 5.0)
        self.assertEqual(merged_buckets(result), ({}, {}))

    def test_absorbed_observations_are_kept_in_the_bucket_total(self):
        source = histogram({0: 4, 1: 6}, {-1: 2}, zero_count=1.0)
        result = merge_native_histograms(source, zero_threshold=1.0)
        self.assertEqual(
            native_histogram_bucket_total(result), native_histogram_bucket_total(source))


class ConservationInvariantTest(unittest.TestCase):
    """Checks the invariants that must hold for any accepted merge."""

    def test_bucket_total_is_preserved_across_resolution_and_threshold_changes(self):
        first = histogram({-3: 1, -1: 2, 0: 4, 7: 1}, {-2: 3, 5: 1},
                          schema=2, zero_threshold=0.0, zero_count=1.0)
        second = histogram({-1: 5, 2: 2, 9: 3}, {0: 2},
                           schema=4, zero_threshold=0.0, zero_count=4.0)
        expected = (native_histogram_bucket_total(first)
                    + native_histogram_bucket_total(second))
        result = merge_native_histograms(first, second, schema=-2, zero_threshold=100.0)
        self.assertEqual(native_histogram_bucket_total(result), expected)

    def test_merging_two_reductions_matches_merging_both_at_once(self):
        first = histogram({1: 2, 4: 3}, schema=3)
        second = histogram({0: 1, 6: 4}, schema=1)
        at_once = merge_native_histograms(first, second, schema=-1)
        staged = merge_native_histograms(
            reduce_native_histogram_resolution(first, 1),
            second, schema=-1)
        self.assertEqual(merged_buckets(at_once), merged_buckets(staged))
        self.assertEqual(at_once.schema, staged.schema)

    def test_the_merged_spans_describe_exactly_the_deltas(self):
        first = histogram({-5: 2, 0: 1, 3: 4, 40: 1}, {-9: 1, 1: 2})
        second = histogram({2: 5, 10000: 1}, schema=4)
        for schema in (-4, -2, -1, 0):
            with self.subTest(schema=schema):
                result = merge_native_histograms(first, second, schema=schema)
                for spans, deltas in ((result.pos_spans, result.pos_deltas),
                                      (result.neg_spans, result.neg_deltas)):
                    spans = tuple(spans or ())
                    deltas = tuple(deltas or ())
                    # Each side is either absent or has exactly one delta per
                    # bucket covered by its spans.
                    self.assertEqual(len(deltas), sum(span.length for span in spans))
                    # An absent side has neither spans nor deltas.
                    self.assertEqual(bool(spans), bool(deltas))

    def test_no_delta_is_negative_without_a_population_to_take_it_from(self):
        first = histogram({0: 1, 1: 9, 2: 2}, {0: 5, 1: 1})
        result = merge_native_histograms(first, schema=0)
        # The absolute populations must never dip below zero, which decode
        # verifies implicitly by rebuilding them from the deltas.
        self.assertEqual(merged_buckets(result), ({0: 1, 1: 9, 2: 2}, {0: 5, 1: 1}))

    def test_merging_many_histograms_produces_one_canonical_result(self):
        inputs = [histogram({index: 1, index + 3: 2}, schema=index % 5) for index in range(8)]
        result = merge_native_histograms(inputs, schema=0)
        self.assertEqual(result.schema, 0)
        self.assertEqual(native_histogram_bucket_total(result),
                         sum(native_histogram_bucket_total(item) for item in inputs))
        self.assertEqual(result.count_value, 24)
        spans = result.pos_spans
        self.assertEqual(spans, tuple(spans))
        for span in spans:
            self.assertGreater(span.length, 0)
        for index in range(1, len(spans)):
            self.assertGreaterEqual(spans[index].offset, 0)

    def test_canonical_result_is_unchanged_by_a_second_merge(self):
        source = histogram({-7: 1, 0: 3, 2: 2, 900: 5}, {1: 2, 4: 1},
                           zero_count=3.0, schema=3)
        once = merge_native_histograms(source, schema=0)
        twice = merge_native_histograms(once)
        self.assertEqual(once, twice)

    def test_float_zero_counts_do_not_trip_the_conservation_check(self):
        # A zero count that is not exactly representable makes the summation
        # order matter; the check must still accept an exact merge.
        inputs = [
            histogram({0: 1}, zero_count=0.1),
            histogram({1: 1}, zero_count=0.2),
            histogram({2: 1}, zero_count=0.3),
        ]
        result = merge_native_histograms(inputs)
        self.assertAlmostEqual(
            native_histogram_bucket_total(result),
            sum(native_histogram_bucket_total(item) for item in inputs))
        self.assertAlmostEqual(result.zero_count, 0.6)


class UnsupportedSchemaTest(unittest.TestCase):
    def test_custom_bucket_schema_is_reported_with_a_reason(self):
        with self.assertRaises(UnsupportedSchemaError) as raised:
            merge_native_histograms(histogram(schema=CUSTOM_BUCKETS_SCHEMA))
        self.assertEqual(raised.exception.schema, CUSTOM_BUCKETS_SCHEMA)
        self.assertIn('custom bucket', raised.exception.reason)
        self.assertIn(str(CUSTOM_BUCKETS_SCHEMA), str(raised.exception))

    def test_reserved_schemas_are_reported_with_a_reason(self):
        for schema in (9, 20, 52, -5, -9):
            with self.subTest(schema=schema):
                with self.assertRaises(UnsupportedSchemaError) as raised:
                    merge_native_histograms(histogram(schema=schema))
                self.assertEqual(raised.exception.schema, schema)
                self.assertIn('reserved', raised.exception.reason)

    def test_unknown_schemas_are_reported_with_a_reason(self):
        for schema in (53, -54, 1000, -1000):
            with self.subTest(schema=schema):
                with self.assertRaises(UnsupportedSchemaError) as raised:
                    merge_native_histograms(histogram(schema=schema))
                self.assertEqual(raised.exception.schema, schema)
                self.assertIn('unknown', raised.exception.reason)

    def test_a_non_integer_schema_is_reported(self):
        for schema in ('0', None, 1.0, True):
            with self.subTest(schema=schema):
                with self.assertRaises(UnsupportedSchemaError) as raised:
                    merge_native_histograms(histogram(schema=schema))
                self.assertIn('integer', raised.exception.reason)

    def test_an_unsupported_target_schema_is_reported(self):
        source = histogram({0: 1}, schema=0)
        with self.assertRaises(UnsupportedSchemaError) as raised:
            merge_native_histograms(source, schema=CUSTOM_BUCKETS_SCHEMA)
        self.assertEqual(raised.exception.schema, CUSTOM_BUCKETS_SCHEMA)

    def test_every_supported_schema_can_be_merged(self):
        for schema in range(EXPONENTIAL_SCHEMA_MIN, EXPONENTIAL_SCHEMA_MAX + 1):
            with self.subTest(schema=schema):
                result = merge_native_histograms(histogram({0: 1, 2: 2}, schema=schema))
                self.assertEqual(result.schema, schema)
                self.assertEqual(merged_buckets(result), ({0: 1, 2: 2}, {}))


class MalformedInputTest(unittest.TestCase):
    def test_negative_absolute_population_is_rejected(self):
        source = NativeHistogram(1, 0.0, 0, 0.0, 0.0, (BucketSpan(0, 2),), None, (1, -3), None)
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(source)
        self.assertIn('negative absolute population', str(raised.exception))

    def test_too_few_deltas_are_rejected(self):
        source = NativeHistogram(1, 0.0, 0, 0.0, 0.0, (BucketSpan(0, 3),), None, (1, 1), None)
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(source)
        self.assertIn('more buckets', str(raised.exception))

    def test_too_many_deltas_are_rejected(self):
        source = NativeHistogram(1, 0.0, 0, 0.0, 0.0, (BucketSpan(0, 1),), None, (1, 1), None)
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(source)
        self.assertIn('deltas were provided', str(raised.exception))

    def test_negative_offset_on_a_later_span_is_rejected(self):
        source = NativeHistogram(
            1, 0.0, 0, 0.0, 0.0,
            (BucketSpan(0, 1), BucketSpan(-1, 1)), None, (1, 1), None)
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(source)
        self.assertIn('negative offset', str(raised.exception))

    def test_negative_span_length_is_rejected(self):
        source = NativeHistogram(1, 0.0, 0, 0.0, 0.0, (BucketSpan(0, -1),), None, None, None)
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(source)
        self.assertIn('negative length', str(raised.exception))

    def test_negative_zero_threshold_is_rejected(self):
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(histogram({0: 1}, zero_threshold=-0.5))
        self.assertIn('must not be negative', str(raised.exception))

    def test_negative_zero_count_is_rejected(self):
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms(histogram({0: 1}, zero_count=-1.0))
        self.assertIn('must not be negative', str(raised.exception))

    def test_non_finite_counts_are_rejected(self):
        for kwargs in ({'count_value': math.nan}, {'count_value': math.inf},
                       {'count_value': 1, 'zero_count': math.nan},
                       {'count_value': 1, 'zero_count': math.inf}):
            with self.subTest(kwargs=kwargs):
                with self.assertRaises(NativeHistogramError) as raised:
                    merge_native_histograms(histogram({0: 1}, **kwargs))
                self.assertIn('finite', str(raised.exception))

    def test_non_finite_zero_thresholds_are_rejected(self):
        for threshold in (math.nan, math.inf):
            with self.subTest(threshold=threshold):
                with self.assertRaises(NativeHistogramError) as raised:
                    merge_native_histograms(histogram({0: 1}, zero_threshold=threshold))
                self.assertIn('finite', str(raised.exception))

    def test_a_non_native_histogram_is_rejected(self):
        with self.assertRaises(TypeError) as raised:
            merge_native_histograms(histogram({0: 1}), 'not a histogram')
        self.assertIn('NativeHistogram', str(raised.exception))

    def test_an_empty_iterable_is_rejected(self):
        with self.assertRaises(NativeHistogramError) as raised:
            merge_native_histograms([])
        self.assertIn('at least one', str(raised.exception))

    def test_mixed_argument_styles_are_rejected(self):
        with self.assertRaises(TypeError):
            merge_native_histograms([histogram({0: 1})], histogram({0: 1}))

    def test_malformed_input_does_not_modify_any_input(self):
        spans = (BucketSpan(0, 1),)
        deltas = (4,)
        good = NativeHistogram(5, 0.0, 0, 0.0, 1.0, spans, None, deltas, None)
        bad = NativeHistogram(1, 0.0, 0, 0.0, 0.0, (BucketSpan(0, 2),), None, (1, -3), None)
        with self.assertRaises(NativeHistogramError):
            merge_native_histograms(good, bad)
        self.assertEqual(good.pos_spans, spans)
        self.assertEqual(good.pos_deltas, deltas)
        self.assertEqual(good.zero_count, 1.0)


class InputsAreNotModifiedTest(unittest.TestCase):
    def test_spans_and_deltas_of_the_inputs_are_untouched(self):
        spans = (BucketSpan(-1, 2), BucketSpan(0, 0), BucketSpan(4, 2))
        deltas = (3, -1, 2, 5)
        first = NativeHistogram(
            20, 1.0, 2, 0.5, 4.0, spans, spans, deltas, deltas)
        second = histogram({0: 1, 2: 3}, schema=0, zero_threshold=0.25)
        spans_copy = tuple(spans)
        deltas_copy = tuple(deltas)
        first_copy = first
        second_copy = second
        merge_native_histograms(first, second, schema=-1, zero_threshold=2.0)
        self.assertEqual(first.pos_spans, spans_copy)
        self.assertEqual(first.neg_spans, spans_copy)
        self.assertEqual(first.pos_deltas, deltas_copy)
        self.assertEqual(first.neg_deltas, deltas_copy)
        self.assertEqual(first, first_copy)
        self.assertEqual(second, second_copy)

    def test_lists_passed_as_spans_and_deltas_are_untouched(self):
        spans = [BucketSpan(-1, 2), BucketSpan(0, 0), BucketSpan(4, 2)]
        deltas = [3, -1, 2, 5]
        source = NativeHistogram(20, 1.0, 2, 0.0, 0.0, spans, None, deltas, None)
        merge_native_histograms(source, schema=-1)
        self.assertEqual(spans, [BucketSpan(-1, 2), BucketSpan(0, 0), BucketSpan(4, 2)])
        self.assertEqual(deltas, [3, -1, 2, 5])

    def test_the_result_shares_no_sequences_with_the_inputs(self):
        spans = (BucketSpan(0, 2),)
        deltas = (1, 1)
        source = NativeHistogram(2, 0.0, 0, 0.0, 0.0, spans, None, deltas, None)
        result = merge_native_histograms(source)
        self.assertIsNot(result.pos_spans, source.pos_spans)
        self.assertIsNot(result.pos_deltas, source.pos_deltas)


class ExemplarTest(unittest.TestCase):
    def test_exemplars_are_collected_from_all_inputs(self):
        first = histogram({0: 1})._replace(nh_exemplars=(Exemplar({'a': 'b'}, 1.0),))
        second = histogram({0: 1})._replace(nh_exemplars=(Exemplar({'c': 'd'}, 2.0),))
        result = merge_native_histograms(first, second)
        self.assertEqual(tuple(result.nh_exemplars), (
            Exemplar({'a': 'b'}, 1.0), Exemplar({'c': 'd'}, 2.0)))

    def test_no_exemplars_stay_none(self):
        self.assertIsNone(merge_native_histograms(histogram({0: 1})).nh_exemplars)


class BoundaryTableTest(unittest.TestCase):
    def test_boundaries_match_prometheus_bucket_layout(self):
        # Schema 0 has buckets (0.5, 1], (1, 2], (2, 4], ...
        self.assertEqual([_bound(i, 0) for i in range(-2, 3)], [0.25, 0.5, 1.0, 2.0, 4.0])
        # Schema -1 doubles the width: (0.25, 1], (1, 4], ...
        self.assertEqual([_bound(i, -1) for i in range(1, 4)], [4.0, 16.0, 64.0])
        # The last bucket before the +Inf bucket is capped at the largest float.
        self.assertEqual(_bound(1023, 0), 8.98846567431158e+307)
        self.assertEqual(_bound(2048, 0), math.inf)

    def test_boundaries_are_non_decreasing(self):
        # Very small boundaries underflow to 0.0 for the coarsest negative
        # schemas, so the guarantee is monotonicity rather than strict growth.
        for schema in range(EXPONENTIAL_SCHEMA_MIN, EXPONENTIAL_SCHEMA_MAX + 1):
            with self.subTest(schema=schema):
                previous = -math.inf
                for index in range(-300, 300):
                    current = _bound(index, schema)
                    self.assertGreaterEqual(current, previous)
                    previous = current

    def test_boundaries_are_strictly_increasing_near_zero(self):
        for schema in range(EXPONENTIAL_SCHEMA_MIN, EXPONENTIAL_SCHEMA_MAX + 1):
            with self.subTest(schema=schema):
                previous = _bound(-1, schema)
                for index in range(0, 20):
                    current = _bound(index, schema)
                    self.assertGreater(current, previous)
                    previous = current


if __name__ == '__main__':
    unittest.main()
