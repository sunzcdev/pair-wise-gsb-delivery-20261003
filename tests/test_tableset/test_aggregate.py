from collections import OrderedDict
from decimal import Decimal

from agate import Table, TableSet
from agate.aggregations import Count, MaxLength, Mean, Min, Sum
from agate.data_types import Boolean, Number, Text
from agate.exceptions import DataTypeError
from agate.testcase import AgateTestCase


class TestAggregate(AgateTestCase):
    def setUp(self):
        self.table1 = (
            ('a', 1),
            ('a', 3),
            ('b', 2)
        )

        self.table2 = (
            ('b', 0),
            ('a', 2),
            ('c', 5)
        )

        self.table3 = (
            ('a', 1),
            ('a', 2),
            ('c', 3)
        )

        self.text_type = Text()
        self.number_type = Number()

        self.column_names = ['letter', 'number']
        self.column_types = [self.text_type, self.number_type]

        self.tables = OrderedDict([
            ('table1', Table(self.table1, self.column_names, self.column_types)),
            ('table2', Table(self.table2, self.column_names, self.column_types)),
            ('table3', Table(self.table3, self.column_names, self.column_types))
        ])

    def test_aggregate_key_name(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        new_table = tableset.aggregate([
            ('count', Count())
        ])

        self.assertIsInstance(new_table, Table)
        self.assertColumnNames(new_table, ('test', 'count'))
        self.assertColumnTypes(new_table, [Text, Number])

    def test_aggregate_key_type(self):
        tables = OrderedDict([
            (1, Table(self.table1, self.column_names, self.column_types)),
            (2, Table(self.table2, self.column_names, self.column_types)),
            (3, Table(self.table3, self.column_names, self.column_types))
        ])

        tableset = TableSet(tables.values(), tables.keys(), key_name='test', key_type=self.number_type)

        new_table = tableset.aggregate([
            ('count', Count())
        ])

        self.assertIsInstance(new_table, Table)
        self.assertColumnNames(new_table, ('test', 'count'))
        self.assertColumnTypes(new_table, [Number, Number])

    def test_aggregate_row_names(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        new_table = tableset.aggregate([
            ('count', Count())
        ])

        self.assertRowNames(new_table, ['table1', 'table2', 'table3'])

    def test_aggregate_sum(self):
        tableset = TableSet(self.tables.values(), self.tables.keys())

        new_table = tableset.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number'))
        ])

        self.assertIsInstance(new_table, Table)
        self.assertColumnNames(new_table, ('group', 'count', 'number_sum'))
        self.assertColumnTypes(new_table, [Text, Number, Number])
        self.assertRows(new_table, [
            ('table1', 3, 6),
            ('table2', 3, 7),
            ('table3', 3, 6)
        ])

    def test_aggregate_min(self):
        tableset = TableSet(self.tables.values(), self.tables.keys())

        new_table = tableset.aggregate([
            ('count', Count()),
            ('number_min', Min('number'))
        ])

        self.assertIsInstance(new_table, Table)
        self.assertColumnNames(new_table, ('group', 'count', 'number_min'))
        self.assertColumnTypes(new_table, [Text, Number, Number])
        self.assertRows(new_table, [
            ('table1', 3, 1),
            ('table2', 3, 0),
            ('table3', 3, 1)
        ])

    def test_aggregate_two_ops(self):
        tableset = TableSet(self.tables.values(), self.tables.keys())

        new_table = tableset.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number')),
            ('number_mean', Mean('number'))
        ])

        self.assertIsInstance(new_table, Table)
        self.assertColumnNames(new_table, ('group', 'count', 'number_sum', 'number_mean'))
        self.assertColumnTypes(new_table, [Text, Number, Number, Number])
        self.assertRows(new_table, [
            ('table1', 3, 6, 2),
            ('table2', 3, 7, Decimal(7) / 3),
            ('table3', 3, 6, 2)
        ])

    def test_aggregate_max_length(self):
        tableset = TableSet(self.tables.values(), self.tables.keys())

        new_table = tableset.aggregate([
            ('count', Count()),
            ('letter_max_length', MaxLength('letter'))
        ])

        self.assertIsInstance(new_table, Table)
        self.assertColumnNames(new_table, ('group', 'count', 'letter_max_length'))
        self.assertColumnTypes(new_table, [Text, Number, Number])
        self.assertRows(new_table, [
            ('table1', 3, 1),
            ('table2', 3, 1),
            ('table3', 3, 1)
        ])

    def test_aggregate_sum_invalid(self):
        tableset = TableSet(self.tables.values(), self.tables.keys())

        with self.assertRaises(DataTypeError):
            tableset.aggregate([('letter_sum', Sum('letter'))])

    def test_aggregeate_bad_column(self):
        tableset = TableSet(self.tables.values(), self.tables.keys())

        with self.assertRaises(KeyError):
            tableset.aggregate([('one_sum', Sum('one'))])

        with self.assertRaises(KeyError):
            tableset.aggregate([('bad_sum', Sum('bad'))])

    def test_nested_aggregation(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        results = nested.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number'))
        ])

        self.assertIsInstance(results, Table)
        self.assertColumnNames(results, ('test', 'letter', 'count', 'number_sum'))
        self.assertColumnTypes(results, (Text, Text, Number, Number))
        self.assertRows(results, [
            ('table1', 'a', 2, 4),
            ('table1', 'b', 1, 2),
            ('table2', 'b', 1, 0),
            ('table2', 'a', 1, 2),
            ('table2', 'c', 1, 5),
            ('table3', 'a', 2, 3),
            ('table3', 'c', 1, 3)
        ])

    def test_nested_aggregate_row_names(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        results = nested.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number'))
        ])

        self.assertRowNames(results, [
            ('table1', 'a'),
            ('table1', 'b'),
            ('table2', 'b'),
            ('table2', 'a'),
            ('table2', 'c'),
            ('table3', 'a'),
            ('table3', 'c'),
        ])
        self.assertSequenceEqual(results.rows[('table1', 'a')], ('table1', 'a', 2, 4))
        self.assertSequenceEqual(results.rows[('table2', 'c')], ('table2', 'c', 1, 5))

    def test_grouping_sets_structure(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        results = nested.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number'))
        ], grouping_sets=[
            ('test',),
            ('test', 'letter'),
            ()
        ])

        self.assertColumnNames(results, (
            'test', 'letter', 'grouping_test', 'grouping_letter', 'count', 'number_sum'
        ))
        self.assertColumnTypes(results, [
            Text, Text, Boolean, Boolean, Number, Number
        ])

    def test_grouping_sets_rows(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        results = nested.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number'))
        ], grouping_sets=[
            ('test',),
            ('test', 'letter'),
            ()
        ])

        self.assertRows(results, [
            ('table1', None, False, True, 3, 6),
            ('table2', None, False, True, 3, 7),
            ('table3', None, False, True, 3, 6),
            ('table1', 'a', False, False, 2, 4),
            ('table1', 'b', False, False, 1, 2),
            ('table2', 'b', False, False, 1, 0),
            ('table2', 'a', False, False, 1, 2),
            ('table2', 'c', False, False, 1, 5),
            ('table3', 'a', False, False, 2, 3),
            ('table3', 'c', False, False, 1, 3),
            (None, None, True, True, 9, 19)
        ])

    def test_grouping_sets_row_names(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        results = nested.aggregate([
            ('count', Count())
        ], grouping_sets=[
            ('test',),
            ()
        ])

        self.assertRowNames(results, [
            ('table1', None),
            ('table2', None),
            ('table3', None),
            (None, None)
        ])

    def test_grouping_sets_grand_total_recomputes_mean(self):
        # Unequal group sizes so a mean of group means would be wrong.
        table = Table((
            ('a', 1),
            ('a', 3),
            ('a', 5),
            ('b', 10)
        ), ['letter', 'number'], [self.text_type, self.number_type])

        tableset = table.group_by('letter')

        results = tableset.aggregate([
            ('count', Count()),
            ('number_mean', Mean('number'))
        ], grouping_sets=[
            ('letter',),
            ()
        ])

        self.assertRows(results, [
            ('a', False, 3, Decimal(3)),
            ('b', False, 1, Decimal(10)),
            (None, True, 4, Decimal(19) / 4)
        ])

    def test_grouping_sets_null_group_distinct_from_rollup(self):
        table = Table((
            (None, 1),
            (None, 3),
            ('a', 2)
        ), ['letter', 'number'], [self.text_type, self.number_type])

        tableset = table.group_by('letter')

        results = tableset.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number'))
        ], grouping_sets=[
            ('letter',),
            ()
        ])

        # A real null group keeps its None key but is marked as not rolled up,
        # while the grand total has a rolled-up marker.
        self.assertRows(results, [
            (None, False, 2, 4),
            ('a', False, 1, 2),
            (None, True, 3, 6)
        ])

    def test_grouping_sets_empty_input_grand_total(self):
        table = Table((), ['letter', 'number'], [self.text_type, self.number_type])

        tableset = table.group_by('letter')

        results = tableset.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number')),
            ('number_mean', Mean('number')),
            ('number_min', Min('number'))
        ], grouping_sets=[
            ('letter',),
            ()
        ])

        self.assertRows(results, [
            (None, True, 0, 0, None, None)
        ])

    def test_grouping_sets_does_not_mutate_tableset(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        def leaf_counts(tableset):
            counts = []

            for key, value in tableset.items():
                if isinstance(value, TableSet):
                    counts.extend((key,) + item for item in leaf_counts(value))
                else:
                    counts.append((key, len(value.rows)))

            return counts

        before = leaf_counts(nested)

        nested.aggregate([
            ('count', Count())
        ], grouping_sets=[('test',), ()])

        after = leaf_counts(nested)

        self.assertEqual(before, after)

    def test_grouping_sets_order_follows_request(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        results = nested.aggregate([
            ('count', Count())
        ], grouping_sets=[
            (),
            ('test', 'letter'),
            ('test',)
        ])

        self.assertSequenceEqual(
            [tuple(row[2:4]) for row in results.rows],
            [
                (True, True),
                (False, False),
                (False, False),
                (False, False),
                (False, False),
                (False, False),
                (False, False),
                (False, False),
                (False, True),
                (False, True),
                (False, True)
            ]
        )

    def test_grouping_sets_dimensions_normalized_to_hierarchy_order(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        results = nested.aggregate([
            ('count', Count())
        ], grouping_sets=[
            ('letter', 'test')
        ])

        first_row = tuple(results.rows[0])

        self.assertEqual(first_row[0], 'table1')
        self.assertEqual(first_row[1], 'a')

    def test_grouping_sets_duplicate_rejected(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        with self.assertRaises(ValueError):
            nested.aggregate([
                ('count', Count())
            ], grouping_sets=[
                ('test',),
                ('test',)
            ])

    def test_grouping_sets_duplicate_after_normalization_rejected(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        with self.assertRaises(ValueError):
            nested.aggregate([
                ('count', Count())
            ], grouping_sets=[
                ('test', 'letter'),
                ('letter', 'test')
            ])

    def test_grouping_sets_unknown_key(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        with self.assertRaises(KeyError):
            nested.aggregate([
                ('count', Count())
            ], grouping_sets=[
                ('nope',)
            ])

    def test_grouping_sets_non_sequence_member(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        with self.assertRaises(ValueError):
            nested.aggregate([
                ('count', Count())
            ], grouping_sets=[
                'test'
            ])

    def test_grouping_sets_empty_sequence_rejected(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        with self.assertRaises(ValueError):
            nested.aggregate([
                ('count', Count())
            ], grouping_sets=[])

    def test_grouping_sets_marker_name_collision_rejected(self):
        tableset = TableSet(self.tables.values(), self.tables.keys(), key_name='test')

        nested = tableset.group_by('letter')

        with self.assertRaises(ValueError):
            nested.aggregate([
                ('grouping_letter', Count())
            ], grouping_sets=[
                ()
            ])

    def test_grouping_sets_single_level_tableset(self):
        tableset = TableSet(self.tables.values(), self.tables.keys())

        results = tableset.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number'))
        ], grouping_sets=[
            ('group',),
            ()
        ])

        self.assertColumnNames(results, (
            'group', 'grouping_group', 'count', 'number_sum'
        ))
        self.assertRows(results, [
            ('table1', False, 3, 6),
            ('table2', False, 3, 7),
            ('table3', False, 3, 6),
            (None, True, 9, 19)
        ])

    def test_grouping_sets_default_behavior_unchanged(self):
        tableset = TableSet(self.tables.values(), self.tables.keys())

        new_table = tableset.aggregate([
            ('count', Count()),
            ('number_sum', Sum('number'))
        ])

        self.assertColumnNames(new_table, ('group', 'count', 'number_sum'))
        self.assertColumnTypes(new_table, [Text, Number, Number])
        self.assertRows(new_table, [
            ('table1', 3, 6),
            ('table2', 3, 7),
            ('table3', 3, 6)
        ])
