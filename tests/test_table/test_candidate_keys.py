from agate import Table
from agate.data_types import Number, Text
from agate.table.candidate_keys import CandidateKeyResult, KeyConflict
from agate.testcase import AgateTestCase


class TestCandidateKeys(AgateTestCase):
    def setUp(self):
        self.rows = (
            ('a', 1, 'x'),
            ('b', 1, 'x'),
            ('c', 2, 'y'),
            ('d', 2, 'y'),
        )

        self.column_names = ['id', 'group', 'code']
        self.column_types = [Text(), Number(), Text()]

    def test_single_column_key(self):
        table = Table(self.rows, self.column_names, self.column_types)

        result = table.candidate_keys()

        self.assertIsInstance(result, CandidateKeyResult)
        self.assertEqual(result.keys, (('id',),))
        self.assertTrue(result.exhaustive)
        self.assertEqual(result.unchecked, ())

    def test_supersets_of_keys_are_pruned(self):
        table = Table(self.rows, self.column_names, self.column_types)

        result = table.candidate_keys()

        # ('id',) is a key, so no wider combination containing 'id' may
        # appear, neither as a key nor as a conflict.
        for combo in result.keys:
            if combo != ('id',):
                self.assertNotIn('id', combo)

        for combo in result.conflicts.keys():
            self.assertNotIn('id', combo)

    def test_minimal_composite_key(self):
        rows = (
            ('a', 1),
            ('a', 2),
            ('b', 1),
            ('b', 2),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys()

        self.assertEqual(result.keys, (('x', 'y'),))
        self.assertIn(('x',), result.conflicts)
        self.assertIn(('y',), result.conflicts)

    def test_failure_does_not_prune_wider_combinations(self):
        # ('x',) fails, but ('x', 'y') must still be tested and found.
        rows = (
            ('a', 1),
            ('a', 2),
            ('b', 1),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys()

        self.assertEqual(result.keys, (('x', 'y'),))

    def test_conflict_samples(self):
        rows = (
            ('a', 1),
            ('a', 2),
            ('a', 3),
            ('b', 1),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys()

        samples = result.conflicts[('x',)]
        self.assertEqual(len(samples), 1)

        sample = samples[0]
        self.assertIsInstance(sample, KeyConflict)
        self.assertEqual(sample.values, ('a',))
        self.assertEqual(sample.row_indices, (0, 1))

    def test_conflict_samples_capped(self):
        rows = (
            ('a', 1),
            ('a', 2),
            ('b', 1),
            ('b', 2),
            ('c', 1),
            ('c', 2),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys(max_conflicts=2)

        self.assertEqual(len(result.conflicts[('x',)]), 2)
        self.assertEqual([s.values for s in result.conflicts[('x',)]], [('a',), ('b',)])

    def test_conflict_samples_zero(self):
        rows = (
            ('a', 1),
            ('a', 1),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys(max_conflicts=0)

        self.assertIn(('x',), result.conflicts)
        self.assertEqual(result.conflicts[('x',)], ())

    def test_output_follows_input_column_order(self):
        rows = (
            ('a', 1, 'x'),
            ('b', 1, 'x'),
            ('c', 2, 'x'),
        )
        table = Table(rows, self.column_names, self.column_types)

        result = table.candidate_keys(columns=['code', 'id', 'group'])

        self.assertEqual(result.keys, (('id',),))
        # Conflicts are ordered by the given candidate order, not table order.
        self.assertEqual(list(result.conflicts.keys()), [('code',), ('group',), ('code', 'group')])

    def test_keys_ordered_by_width_then_input_order(self):
        rows = (
            ('a', 1, 'x'),
            ('b', 1, 'y'),
            ('c', 2, 'x'),
        )
        table = Table(rows, ['p', 'q', 'r'], [Text(), Number(), Text()])

        result = table.candidate_keys(columns=['q', 'p', 'r'])

        self.assertEqual(result.keys, (('p',), ('q', 'r')))

    def test_max_width(self):
        rows = (
            ('a', 1),
            ('a', 2),
            ('b', 1),
            ('b', 2),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys(max_width=1)

        self.assertEqual(result.keys, ())
        self.assertTrue(result.exhaustive)
        self.assertEqual(set(result.conflicts.keys()), {('x',), ('y',)})

    def test_max_width_clamped(self):
        table = Table(self.rows, self.column_names, self.column_types)

        result = table.candidate_keys(max_width=99)

        self.assertEqual(result.keys, (('id',),))
        self.assertTrue(result.exhaustive)

    def test_empty_table(self):
        table = Table([], self.column_names, self.column_types)

        result = table.candidate_keys()

        # Every single column is vacuously unique on an empty table.
        self.assertEqual(result.keys, (('id',), ('group',), ('code',)))
        self.assertEqual(result.conflicts, {})
        self.assertTrue(result.exhaustive)

    def test_duplicate_full_rows_no_key(self):
        rows = (
            ('a', 1),
            ('a', 1),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys()

        self.assertEqual(result.keys, ())
        self.assertTrue(result.exhaustive)
        self.assertEqual(set(result.conflicts.keys()), {('x',), ('y',), ('x', 'y')})

    def test_null_strategy_fail_default(self):
        rows = (
            ('a', 1),
            (None, 2),
            ('c', 3),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys()

        self.assertEqual(result.keys, (('y',),))
        samples = result.conflicts[('x',)]
        self.assertEqual(len(samples), 1)
        self.assertEqual(samples[0].values, (None,))
        self.assertEqual(samples[0].row_indices, (1,))

    def test_null_strategy_equal(self):
        rows = (
            ('a', 1),
            (None, 2),
            (None, 3),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys(null_strategy='equal')

        self.assertEqual(result.keys, (('y',),))
        samples = result.conflicts[('x',)]
        self.assertEqual(len(samples), 1)
        self.assertEqual(samples[0].values, (None,))
        self.assertEqual(samples[0].row_indices, (1, 2))

    def test_null_strategy_distinct(self):
        rows = (
            ('a', 1),
            (None, 2),
            (None, 3),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        result = table.candidate_keys(null_strategy='distinct')

        # Nulls never conflict, so 'x' is unique under this strategy.
        self.assertEqual(result.keys, (('x',), ('y',)))
        self.assertEqual(result.conflicts, {})

    def test_max_checks_budget(self):
        rows = (
            ('a', 1),
            ('a', 2),
            ('b', 1),
            ('b', 2),
        )
        table = Table(rows, ['x', 'y'], [Text(), Number()])

        # Budget covers only the two width-1 combinations.
        result = table.candidate_keys(max_checks=2)

        self.assertFalse(result.exhaustive)
        self.assertEqual(result.keys, ())
        self.assertEqual(set(result.conflicts.keys()), {('x',), ('y',)})
        self.assertEqual(result.unchecked, (('x', 'y'),))

    def test_max_checks_with_pruning(self):
        table = Table(self.rows, self.column_names, self.column_types)

        # Four checks cover all width-1 combinations plus ('group', 'code');
        # finding ('id',) prunes every other wider combination, so the
        # budget is never hit.
        result = table.candidate_keys(max_checks=4)

        self.assertEqual(result.keys, (('id',),))
        self.assertTrue(result.exhaustive)
        self.assertEqual(result.unchecked, ())

    def test_max_checks_zero(self):
        table = Table(self.rows, self.column_names, self.column_types)

        result = table.candidate_keys(max_checks=0)

        self.assertFalse(result.exhaustive)
        self.assertEqual(result.keys, ())
        self.assertEqual(result.conflicts, {})
        self.assertEqual(len(result.unchecked), 7)

    def test_invalid_columns(self):
        table = Table(self.rows, self.column_names, self.column_types)

        with self.assertRaises(ValueError):
            table.candidate_keys(columns=['id', 'id'])

        with self.assertRaises(ValueError):
            table.candidate_keys(columns=['nope'])

    def test_invalid_arguments(self):
        table = Table(self.rows, self.column_names, self.column_types)

        with self.assertRaises(ValueError):
            table.candidate_keys(null_strategy='bogus')

        with self.assertRaises(ValueError):
            table.candidate_keys(max_width=0)

        with self.assertRaises(ValueError):
            table.candidate_keys(max_conflicts=-1)

        with self.assertRaises(ValueError):
            table.candidate_keys(max_checks=-1)

    def test_result_repr(self):
        table = Table(self.rows, self.column_names, self.column_types)

        result = table.candidate_keys()

        self.assertIn('1 keys', repr(result))
        self.assertIn('exhaustive', repr(result))
