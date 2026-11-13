from decimal import Decimal

import agate
from agate import Bins, Table, TableSet
from agate.data_types import Number, Text
from agate.exceptions import BinOutOfRangeError
from agate.testcase import AgateTestCase


class TestBinsDefinition(AgateTestCase):
    def setUp(self):
        self.number_type = Number()
        self.column_names = ['number']
        self.column_types = [self.number_type]

    def test_boundaries_are_exact_decimals(self):
        exact = Decimal('0.1000000000000000000000000000001')
        definition = Bins([Decimal('0'), exact])

        self.assertEqual(definition.boundaries, (Decimal('0'), exact))
        self.assertEqual(definition.boundaries[1], exact)

    def test_float_boundaries_rejected(self):
        with self.assertRaises(TypeError):
            Bins([0.1, 0.2])

    def test_integer_and_string_boundaries(self):
        definition = Bins([0, '5', Decimal('10')])

        self.assertEqual(definition.boundaries, (Decimal('0'), Decimal('5'), Decimal('10')))

    def test_boundaries_must_be_strictly_increasing(self):
        with self.assertRaises(ValueError):
            Bins([1, 1])

        with self.assertRaises(ValueError):
            Bins([2, 1])

    def test_at_least_two_boundaries(self):
        with self.assertRaises(ValueError):
            Bins([1])

    def test_invalid_closed(self):
        with self.assertRaises(ValueError):
            Bins([0, 1], closed='middle')

    def test_labels_default_right_closed(self):
        definition = Bins([0, 5, 10])

        self.assertEqual(definition.labels, ('(0 - 5]', '(5 - 10]'))
        self.assertEqual(definition.closed, 'right')

    def test_labels_left_closed(self):
        definition = Bins([0, 5, 10], closed='left')

        self.assertEqual(definition.labels, ('[0 - 5)', '[5 - 10)'))

    def test_custom_labels(self):
        definition = Bins([0, 5, 10], labels=('low', 'high'))

        self.assertEqual(definition.labels, ('low', 'high'))

    def test_custom_labels_must_match_bin_count(self):
        with self.assertRaises(ValueError):
            Bins([0, 5, 10], labels=('only one',))

    def test_labels_must_not_duplicate_overflow_buckets(self):
        with self.assertRaises(ValueError):
            Bins([0, 5], overflow_label='(0 - 5]')

    def test_underflow_and_overflow_labels_must_differ(self):
        with self.assertRaises(ValueError):
            Bins([0, 5], underflow_label='same', overflow_label='same')

    def test_definition_is_independent_of_data(self):
        definition = Bins([0, 5, 10])
        table = Table([[Decimal(1)]], self.column_names, self.column_types)

        table.bins('number', bins=definition)

        self.assertEqual(definition.boundaries, (Decimal('0'), Decimal('5'), Decimal('10')))
        self.assertEqual(definition.labels, ('(0 - 5]', '(5 - 10]'))

    def test_equality(self):
        self.assertEqual(Bins([0, 5, 10]), Bins([Decimal('0'), Decimal('5'), Decimal('10')]))
        self.assertNotEqual(Bins([0, 5, 10]), Bins([0, 5, 10], closed='left'))


class TestBinsClassify(AgateTestCase):
    def test_right_closed_intervals(self):
        definition = Bins([0, 5, 10])

        self.assertEqual(definition.classify(Decimal('0')), '(0 - 5]')
        self.assertEqual(definition.classify(Decimal('5')), '(5 - 10]')
        self.assertEqual(definition.classify(Decimal('10')), '(5 - 10]')
        self.assertEqual(definition.classify(Decimal('-1')), definition.underflow_label)
        self.assertEqual(definition.classify(Decimal('11')), definition.overflow_label)
        self.assertIsNone(definition.classify(None))

    def test_left_closed_intervals(self):
        definition = Bins([0, 5, 10], closed='left')

        self.assertEqual(definition.classify(Decimal('0')), '[0 - 5)')
        self.assertEqual(definition.classify(Decimal('5')), '[5 - 10)')
        self.assertEqual(definition.classify(Decimal('10')), definition.overflow_label)
        self.assertEqual(definition.classify(Decimal('-1')), definition.underflow_label)


class TestTableBinsDefinition(AgateTestCase):
    def setUp(self):
        self.number_type = Number()
        self.column_names = ['number']
        self.column_types = [self.number_type]
        self.rows = [[Decimal(i)] for i in range(0, 10)]

    def test_count_with_definition(self):
        table = Table(self.rows, self.column_names, self.column_types)
        new_table = table.bins('number', bins=Bins([0, 5, 10]))

        self.assertColumnNames(new_table, ['number', 'Count'])
        self.assertColumnTypes(new_table, [Text, Number])
        self.assertRowNames(new_table, [
            '(-\u221e - 0)',
            '(0 - 5]',
            '(5 - 10]',
            '(10 - \u221e)',
            None,
        ])
        self.assertSequenceEqual(new_table.rows[0], ['(-\u221e - 0)', 0])
        self.assertSequenceEqual(new_table.rows[1], ['(0 - 5]', 5])
        self.assertSequenceEqual(new_table.rows[2], ['(5 - 10]', 5])
        self.assertSequenceEqual(new_table.rows[3], ['(10 - \u221e)', 0])
        self.assertSequenceEqual(new_table.rows[4], [None, 0])

    def test_empty_bins_keep_fixed_positions(self):
        table = Table([[Decimal(1)], [Decimal(2)]], self.column_names, self.column_types)
        new_table = table.bins('number', bins=Bins([0, 5, 10, 15]))

        self.assertEqual(len(new_table.rows), 6)
        self.assertEqual([row[1] for row in new_table.rows], [0, 2, 0, 0, 0, 0])
        self.assertSequenceEqual(new_table.rows[3], ['(10 - 15]', 0])

    def test_underflow_and_overflow_buckets(self):
        rows = [[Decimal(-4)], [Decimal(-1)], [Decimal(3)], [Decimal(11)], [Decimal(42)]]
        table = Table(rows, self.column_names, self.column_types)
        new_table = table.bins('number', bins=Bins([0, 5, 10]))

        counts = {row[0]: row[1] for row in new_table.rows}

        self.assertEqual(counts['(-\u221e - 0)'], 2)
        self.assertEqual(counts['(0 - 5]'], 1)
        self.assertEqual(counts['(5 - 10]'], 0)
        self.assertEqual(counts['(10 - \u221e)'], 2)

    def test_end_bins_are_not_widened(self):
        rows = [[Decimal(-99)], [Decimal(1)], [Decimal(99)]]
        table = Table(rows, self.column_names, self.column_types)
        new_table = table.bins('number', bins=Bins([0, 5, 10]))

        labels = [row[0] for row in new_table.rows]

        self.assertIn('(-\u221e - 0)', labels)
        self.assertIn('(10 - \u221e)', labels)
        self.assertEqual(new_table.rows[1][0], '(0 - 5]')
        self.assertEqual(new_table.rows[1][1], 1)

    def test_nulls_counted_separately(self):
        rows = [[Decimal(1)], [None], [None]]
        table = Table(rows, self.column_names, self.column_types)
        new_table = table.bins('number', bins=Bins([0, 5]))

        null_row = new_table.rows[-1]

        self.assertIsNone(null_row[0])
        self.assertEqual(null_row[1], 2)

    def test_on_bad_raise(self):
        table = Table([[Decimal(99)]], self.column_names, self.column_types)

        with self.assertRaises(BinOutOfRangeError):
            table.bins('number', bins=Bins([0, 5]), on_bad='raise')

    def test_on_bad_drop(self):
        table = Table([[Decimal(99)], [Decimal(1)]], self.column_names, self.column_types)
        new_table = table.bins('number', bins=Bins([0, 5]), on_bad='drop')

        labels = [row[0] for row in new_table.rows]

        self.assertNotIn('(5 - \u221e)', labels)
        self.assertNotIn('(-\u221e - 0)', labels)
        self.assertEqual(len(new_table.rows), 2)
        self.assertEqual(new_table.rows[0], ['(0 - 5]', 1])

    def test_invalid_on_bad(self):
        table = Table(self.rows, self.column_names, self.column_types)

        with self.assertRaises(ValueError):
            table.bins('number', bins=Bins([0, 5]), on_bad='explode')

    def test_non_number_column_rejected(self):
        table = Table([['a']], ['text'], [Text()])

        with self.assertRaises(ValueError):
            table.bins('text', bins=Bins([0, 5]))

    def test_left_closed_counting(self):
        rows = [[Decimal(0)], [Decimal(5)], [Decimal(10)]]
        table = Table(rows, self.column_names, self.column_types)
        new_table = table.bins('number', bins=Bins([0, 5, 10], closed='left'))

        counts = {row[0]: row[1] for row in new_table.rows}

        self.assertEqual(counts['[0 - 5)'], 1)
        self.assertEqual(counts['[5 - 10)'], 1)
        self.assertEqual(counts['(10 - \u221e)'], 1)

    def test_explicit_start_zero_not_unset(self):
        # Data spans 1..5 with rounded minimum 1. With an explicit start=0 the
        # spread is 5 (five width-1 bins); if 0 were treated as unset the
        # minimum (1) would be used instead, giving a width-0.8 first bin.
        table = Table([[Decimal(i)] for i in range(1, 6)], self.column_names, self.column_types)
        new_table = table.bins('number', 5, start=0, end=5)

        self.assertEqual(new_table.rows[0][0], '[1 - 2)')
        self.assertEqual(new_table.rows[-1][0], '[4 - 5]')
        self.assertEqual(len(new_table.rows), 4)
        # Width-1 spacing proves start=0 was used rather than the minimum of 1.
        self.assertEqual(new_table.rows[1][0], '[2 - 3)')

        # The reusable definition path always preserves the exact zero edge.
        explicit = table.bins('number', bins=Bins([0, 1, 2, 3, 4, 5]))

        self.assertEqual(explicit.rows[1][0], '(0 - 1]')
        self.assertEqual(explicit.rows[1][1], 0)
        self.assertEqual(explicit.rows[2], ['(1 - 2]', 1])


class TestTableSetBinsDefinition(AgateTestCase):
    def setUp(self):
        self.number_type = Number()
        self.column_names = ['number']
        self.column_types = [self.number_type]

        self.table_one = Table([[Decimal(i)] for i in range(0, 5)], self.column_names, self.column_types)
        self.table_two = Table(
            [[Decimal(i)] for i in range(5, 12)] + [[None], [Decimal(-3)], [Decimal(99)]],
            self.column_names,
            self.column_types,
        )
        self.tableset = TableSet(
            [self.table_one, self.table_two],
            ['one', 'two'],
            key_name='partition',
        )
        self.definition = Bins([0, 5, 10])

    def test_tableset_uses_shared_definition(self):
        new_tableset = self.tableset.bins('number', bins=self.definition)

        self.assertIsInstance(new_tableset, TableSet)

        labels_one = [row[0] for row in new_tableset['one'].rows]
        labels_two = [row[0] for row in new_tableset['two'].rows]

        self.assertEqual(labels_one, labels_two)

    def test_partition_counts_add_up_to_merged_counts(self):
        new_tableset = self.tableset.bins('number', bins=self.definition)
        merged = Table.merge([self.table_one, self.table_two]).bins('number', bins=self.definition)

        for i, merged_row in enumerate(merged.rows):
            partition_sum = (
                new_tableset['one'].rows[i][1] + new_tableset['two'].rows[i][1]
            )

            self.assertEqual(partition_sum, merged_row[1])
            self.assertEqual(new_tableset['one'].rows[i][0], merged_row[0])
            self.assertEqual(new_tableset['two'].rows[i][0], merged_row[0])

    def test_tableset_on_bad_raise(self):
        with self.assertRaises(BinOutOfRangeError):
            self.tableset.bins('number', bins=self.definition, on_bad='raise')

    def test_bins_module_exported(self):
        self.assertIs(agate.Bins, Bins)
        self.assertTrue(issubclass(BinOutOfRangeError, ValueError))
