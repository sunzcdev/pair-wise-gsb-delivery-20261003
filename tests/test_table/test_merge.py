from decimal import Decimal

from agate import Table
from agate.data_types import Number, Text
from agate.data_types.base import DataType
from agate.exceptions import CastError, DataTypeError
from agate.testcase import AgateTestCase


class OpaqueType(DataType):
    """
    A custom data type which does not describe its semantic configuration.
    """
    def cast(self, d):
        return d


class DescribedType(DataType):
    """
    A custom data type which describes its semantic configuration.
    """
    def __init__(self, delimiter=',', **kwargs):
        super().__init__(**kwargs)

        self.delimiter = delimiter
        self._cache = {}

    def semantic_config(self):
        return {
            'null_values': tuple(self.null_values),
            'delimiter': self.delimiter
        }

    def cast(self, d):
        return d


class TestMerge(AgateTestCase):
    def setUp(self):
        self.rows = (
            (1, 4, 'a'),
            (2, 3, 'b'),
            (None, 2, 'c')
        )

        self.number_type = Number()
        self.text_type = Text()

        self.column_names = ['one', 'two', 'three']
        self.column_types = [self.number_type, self.number_type, self.text_type]

    def test_merge(self):
        table_a = Table(self.rows, self.column_names, self.column_types)
        table_b = Table(self.rows, self.column_names)
        table_c = Table.merge([table_a, table_b])

        self.assertIsNot(table_c, table_a)
        self.assertIsNot(table_c, table_b)
        self.assertColumnNames(table_c, self.column_names)
        self.assertColumnTypes(table_c, [Number, Number, Text])
        self.assertRows(table_c, self.rows + self.rows)

    def test_merge_different_names(self):
        table_a = Table(self.rows, self.column_names, self.column_types)

        column_names = ['a', 'b', 'c']

        table_b = Table(self.rows, column_names, self.column_types)
        table_c = Table.merge([table_a, table_b])

        self.assertIsNot(table_c, table_a)
        self.assertIsNot(table_c, table_b)
        self.assertColumnNames(table_c, self.column_names + column_names)
        self.assertColumnTypes(table_c, [Number, Number, Text, Number, Number, Text])
        self.assertSequenceEqual(table_c.rows[0], [1, 4, 'a', None, None, None])
        self.assertSequenceEqual(table_c.rows[3], [None, None, None, 1, 4, 'a'])

        for row in table_c.rows:
            self.assertSequenceEqual(row.keys(), self.column_names + column_names)

    def test_merge_mixed_names(self):
        table_a = Table(self.rows, self.column_names, self.column_types)

        column_names = ['two', 'one', 'four']

        table_b = Table(self.rows, column_names, self.column_types)
        table_c = Table.merge([table_a, table_b])

        self.assertIsNot(table_c, table_a)
        self.assertIsNot(table_c, table_b)
        self.assertColumnNames(table_c, ['one', 'two', 'three', 'four'])
        self.assertColumnTypes(table_c, [Number, Number, Text, Text])
        self.assertSequenceEqual(table_c.rows[0], [1, 4, 'a', None])
        self.assertSequenceEqual(table_c.rows[3], [4, 1, None, 'a'])

        for row in table_c.rows:
            self.assertSequenceEqual(row.keys(), ['one', 'two', 'three', 'four'])

    def test_merge_different_types(self):
        table_a = Table(self.rows, self.column_names, self.column_types)

        column_types = [self.number_type, self.text_type, self.text_type]

        table_b = Table(self.rows, self.column_names, column_types)

        with self.assertRaises(DataTypeError):
            Table.merge([table_a, table_b])

    def test_merge_with_row_names(self):
        table_a = Table(self.rows, self.column_names, self.column_types, row_names='three')

        b_rows = (
            (1, 4, 'd'),
            (2, 3, 'e'),
            (None, 2, 'f')
        )

        table_b = Table(b_rows, self.column_names, self.column_types, row_names='three')
        table_c = Table.merge([table_a, table_b], row_names='three')

        self.assertRowNames(table_c, ['a', 'b', 'c', 'd', 'e', 'f'])

    def test_merge_with_column_names(self):
        table_a = Table(self.rows, self.column_names, self.column_types, row_names='three')

        b_rows = (
            (1, 4, 'd'),
            (2, 3, 'e'),
            (None, 2, 'f')
        )

        c_rows = (
            (1, 4, 'a'),
            (2, 3, 'b'),
            (None, 2, 'c'),
            (None, 4, 'd'),
            (None, 3, 'e'),
            (None, 2, 'f')
        )

        table_b = Table(b_rows, ['a', 'two', 'three'], self.column_types, row_names='three')
        table_c = Table.merge([table_a, table_b], column_names=table_a.column_names)

        self.assertRows(table_c, c_rows)

    def test_merge_strict_types_compatible(self):
        table_a = Table(self.rows, self.column_names, [Number(), Number(), Text()])
        table_b = Table(self.rows, self.column_names, [Number(), Number(), Text()])
        table_c = Table.merge([table_a, table_b], strict_types=True)

        self.assertColumnNames(table_c, self.column_names)
        self.assertColumnTypes(table_c, [Number, Number, Text])
        self.assertRows(table_c, self.rows + self.rows)

    def test_merge_strict_types_conflict(self):
        table_a = Table(self.rows, self.column_names, [Number(locale='en_US'), Number(), Text()])
        table_b = Table(self.rows, self.column_names, [Number(locale='de_DE'), Number(), Text()])

        with self.assertRaises(DataTypeError) as error:
            Table.merge([table_a, table_b], strict_types=True)

        message = str(error.exception)

        self.assertIn('"one"', message)
        self.assertIn('tables 0 and 1', message)
        self.assertIn('locale', message)
        self.assertIn('en_US', message)
        self.assertIn('de_DE', message)

    def test_merge_strict_types_different_classes(self):
        class SubNumber(Number):
            pass

        table_a = Table(self.rows, self.column_names, [Number(), Number(), Text()])
        table_b = Table(self.rows, self.column_names, [SubNumber(), Number(), Text()])

        with self.assertRaises(DataTypeError) as error:
            Table.merge([table_a, table_b], strict_types=True)

        self.assertIn('"one"', str(error.exception))

    def test_merge_strict_types_unknown_compatibility(self):
        rows = ((1, 'a'), (2, 'b'))

        table_a = Table(rows, ['one', 'two'], [OpaqueType(), Text()])
        table_b = Table(rows, ['one', 'two'], [OpaqueType(), Text()])

        with self.assertRaises(DataTypeError) as error:
            Table.merge([table_a, table_b], strict_types=True)

        message = str(error.exception)

        self.assertIn('"one"', message)
        self.assertIn('unknown', message)

    def test_merge_strict_types_custom_semantic_config(self):
        rows = ((1, 'a'), (2, 'b'))

        table_a = Table(rows, ['one', 'two'], [DescribedType(delimiter=','), Text()])
        table_b = Table(rows, ['one', 'two'], [DescribedType(delimiter=';'), Text()])

        with self.assertRaises(DataTypeError) as error:
            Table.merge([table_a, table_b], strict_types=True)

        self.assertIn('delimiter', str(error.exception))

        table_c = Table(rows, ['one', 'two'], [DescribedType(delimiter=','), Text()])
        table_d = Table.merge([table_a, table_c], strict_types=True)

        self.assertRows(table_d, rows + rows)

    def test_merge_not_strict_ignores_config_conflict(self):
        table_a = Table(self.rows, self.column_names, [Number(locale='en_US'), Number(), Text()])
        table_b = Table(self.rows, self.column_names, [Number(locale='de_DE'), Number(), Text()])
        table_c = Table.merge([table_a, table_b])

        self.assertIs(table_c.column_types[0], table_a.column_types[0])
        self.assertRows(table_c, self.rows + self.rows)

    def test_merge_type_override(self):
        table_a = Table(self.rows, self.column_names, [Number(locale='en_US'), Number(), Text()])
        table_b = Table(self.rows, self.column_names, [Number(locale='de_DE'), Number(), Text()])

        target = Number(locale='en_US')
        table_c = Table.merge([table_a, table_b], strict_types=True, type_overrides={'one': target})

        self.assertIs(table_c.column_types[0], target)
        self.assertRows(table_c, self.rows + self.rows)

    def test_merge_type_override_converts_values(self):
        a_rows = (
            ('1', 'a'),
            ('2', 'b')
        )
        b_rows = (
            (Decimal(3), 'c'),
            (Decimal(4), 'd')
        )

        table_a = Table(a_rows, ['one', 'two'], [Text(), Text()])
        table_b = Table(b_rows, ['one', 'two'], [Number(), Text()])
        table_c = Table.merge([table_a, table_b], type_overrides={'one': Number()})

        self.assertColumnTypes(table_c, [Number, Text])
        self.assertSequenceEqual(table_c.columns['one'], [Decimal(1), Decimal(2), Decimal(3), Decimal(4)])

        # Source tables are not modified
        self.assertSequenceEqual(table_a.columns['one'], ['1', '2'])
        self.assertColumnTypes(table_a, [Text, Text])

    def test_merge_type_override_cast_error(self):
        a_rows = (
            ('not a number', 'a'),
        )
        b_rows = (
            (Decimal(3), 'c'),
        )

        table_a = Table(a_rows, ['one', 'two'], [Text(), Text()])
        table_b = Table(b_rows, ['one', 'two'], [Number(), Text()])

        with self.assertRaises(CastError):
            Table.merge([table_a, table_b], type_overrides={'one': Number()})

    def test_merge_type_override_not_a_data_type(self):
        table_a = Table(self.rows, self.column_names, self.column_types)

        with self.assertRaises(DataTypeError):
            Table.merge([table_a], type_overrides={'one': 'number'})

    def test_merge_type_override_missing_column(self):
        table_a = Table(self.rows, self.column_names, self.column_types)

        with self.assertRaises(DataTypeError):
            Table.merge([table_a], type_overrides={'nope': Number()})
