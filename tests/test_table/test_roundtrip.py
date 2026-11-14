import datetime
import io
import json
from decimal import Decimal

import agate
from agate import NormalizationSchema, Table
from agate.data_types import Boolean, DataType, Date, DateTime, Number, Text, TimeDelta
from agate.exceptions import NormalizationError
from agate.testcase import AgateTestCase


class TestRoundTrip(AgateTestCase):
    def setUp(self):
        self.rows = (
            ('a', Decimal('1'), True, datetime.date(2020, 1, 1), 'x'),
            ('b', Decimal('2.5'), False, datetime.date(2021, 6, 15), 'y'),
            ('c', None, None, None, None)
        )

        self.column_names = ['id', 'num', 'flag', 'd', 'txt']
        self.column_types = [Text(), Number(), Boolean(), Date(), Text()]

        self.table = Table(self.rows, self.column_names, self.column_types)

    def assertTablesEqual(self, a, b):
        self.assertSequenceEqual(a.column_names, b.column_names)
        self.assertSequenceEqual(
            [type(t) for t in a.column_types],
            [type(t) for t in b.column_types]
        )
        self.assertEqual(len(a.rows), len(b.rows))

        for i in range(len(a.rows)):
            self.assertSequenceEqual(tuple(a.rows[i]), tuple(b.rows[i]))

        self.assertEqual(a.row_names, b.row_names)

    def test_round_trip(self):
        normalized = self.table.normalize('id', ['num', 'flag', 'd', 'txt'], record_schema=True)

        self.assertIsNotNone(normalized.normalization_schema)

        restored = normalized.denormalize()

        self.assertTablesEqual(restored, self.table)

    def test_round_trip_exact_values(self):
        normalized = self.table.normalize('id', ['num', 'flag', 'd', 'txt'], record_schema=True)

        # Values in the value column are not cast to the inferred type
        self.assertEqual(normalized.rows[0]['value'], Decimal('1'))
        self.assertIsInstance(normalized.rows[1]['value'], bool)

        restored = normalized.denormalize()

        self.assertEqual(restored.rows[0]['num'], Decimal('1'))
        self.assertIs(restored.rows[0]['flag'], True)
        self.assertEqual(restored.rows[0]['d'], datetime.date(2020, 1, 1))

    def test_round_trip_column_order(self):
        table = Table(
            [(1, 'a', 2.5, True)],
            ['z', 'a', 'm', 'b'],
            [Number(), Text(), Number(), Boolean()]
        )

        normalized = table.normalize('z', ['b', 'm'], record_schema=True)
        restored = normalized.denormalize()

        # Original column order is restored, not the order the properties
        # were passed to normalize
        self.assertColumnNames(restored, ['z', 'm', 'b'])

    def test_round_trip_multiple_keys(self):
        table = Table(
            [(1, 'x', 'a', True), (2, 'y', 'b', False)],
            ['one', 'two', 'three', 'four'],
            [Number(), Text(), Text(), Boolean()]
        )

        restored = table.normalize(['one', 'two'], ['three', 'four'], record_schema=True).denormalize()

        self.assertTablesEqual(restored, table)

    def test_round_trip_all_null_column(self):
        table = Table(
            [(1, None, 'a'), (2, None, 'b')],
            ['id', 'n', 's'],
            [Number(), Number(), Text()]
        )

        restored = table.normalize('id', ['n', 's'], record_schema=True).denormalize()

        self.assertColumnTypes(restored, [Number, Number, Text])
        self.assertRows(restored, [(Decimal(1), None, 'a'), (Decimal(2), None, 'b')])

    def test_round_trip_empty_table(self):
        table = Table([], self.column_names, self.column_types)

        normalized = table.normalize('id', ['num', 'flag', 'd', 'txt'], record_schema=True)

        self.assertEqual(len(normalized.rows), 0)

        restored = normalized.denormalize()

        self.assertColumnNames(restored, self.column_names)
        self.assertColumnTypes(restored, [Text, Number, Boolean, Date, Text])
        self.assertEqual(len(restored.rows), 0)

    def test_round_trip_row_names(self):
        table = Table(self.rows, self.column_names, self.column_types, row_names=['r1', 'r2', 'r3'])

        restored = table.normalize('id', ['num', 'flag', 'd', 'txt'], record_schema=True).denormalize()

        self.assertRowNames(restored, ['r1', 'r2', 'r3'])

    def test_round_trip_no_row_names(self):
        restored = self.table.normalize('id', ['num'], record_schema=True).denormalize()

        self.assertIsNone(restored.row_names)

    def test_round_trip_same_named_properties(self):
        # Properties named after the property/value columns are data, not
        # metadata, and must survive the round trip
        table = Table(
            [(1, 'x', 'y')],
            ['id', 'value', 'property'],
            [Number(), Text(), Text()]
        )

        restored = table.normalize('id', ['value', 'property'], record_schema=True).denormalize()

        self.assertColumnNames(restored, ['id', 'value', 'property'])
        self.assertRows(restored, [(Decimal(1), 'x', 'y')])

    def test_round_trip_repeated_property_names(self):
        # The same property name appearing under different keys describes
        # distinct cells and must be preserved
        table = Table(
            [(1, 'a'), (2, 'b'), (3, 'c')],
            ['id', 'p'],
            [Number(), Text()]
        )

        normalized = table.normalize('id', 'p', record_schema=True)

        self.assertEqual(len(normalized.rows), 3)

        restored = normalized.denormalize()

        self.assertRows(restored, [(Decimal(1), 'a'), (Decimal(2), 'b'), (Decimal(3), 'c')])

    def test_round_trip_custom_column_names(self):
        normalized = self.table.normalize(
            'id', ['num', 'txt'],
            property_column='field',
            value_column='data',
            record_schema=True
        )

        self.assertColumnNames(normalized, ['id', 'field', 'data'])

        restored = normalized.denormalize()

        self.assertTablesEqual(restored, self.table.select(['id', 'num', 'txt']))

    def test_denormalize_duplicate_cell_rejected(self):
        normalized = Table(
            [(1, 'a', 'x'), (1, 'a', 'y')],
            ['id', 'property', 'value'],
            [Number(), Text(), Text()]
        )

        schema = NormalizationSchema(
            key=['id'],
            properties=['a'],
            column_types={'id': Number(), 'a': Text()}
        )

        with self.assertRaises(NormalizationError):
            normalized.denormalize(schema=schema)

    def test_denormalize_unknown_property_rejected(self):
        normalized = Table(
            [(1, 'b', 'x')],
            ['id', 'property', 'value'],
            [Number(), Text(), Text()]
        )

        schema = NormalizationSchema(
            key=['id'],
            properties=['a'],
            column_types={'id': Number(), 'a': Text()}
        )

        with self.assertRaises(NormalizationError):
            normalized.denormalize(schema=schema)

    def test_denormalize_missing_column_rejected(self):
        normalized = Table(
            [(1, 'x')],
            ['id', 'value'],
            [Number(), Text()]
        )

        schema = NormalizationSchema(
            key=['id'],
            properties=['a'],
            column_types={'id': Number(), 'a': Text()}
        )

        with self.assertRaises(NormalizationError):
            normalized.denormalize(schema=schema)

    def test_denormalize_row_names_mismatch_rejected(self):
        table = Table(
            [(1, 'a'), (2, 'b')],
            ['id', 'p'],
            [Number(), Text()],
            row_names=['r1', 'r2']
        )

        normalized = table.normalize('id', 'p', record_schema=True)
        filtered = normalized.where(lambda r: r['id'] == 1)

        with self.assertRaises(NormalizationError):
            filtered.denormalize(schema=normalized.normalization_schema)

    def test_denormalize_value_type_mismatch_rejected(self):
        normalized = Table(
            [(1, 'a', 'not a number')],
            ['id', 'property', 'value'],
            [Number(), Text(), Text()]
        )

        schema = NormalizationSchema(
            key=['id'],
            properties=['a'],
            column_types={'id': Number(), 'a': Number()}
        )

        with self.assertRaises(NormalizationError):
            normalized.denormalize(schema=schema)

    def test_denormalize_invalid_schema_rejected(self):
        normalized = self.table.normalize('id', ['num'], record_schema=True)

        with self.assertRaises(NormalizationError):
            normalized.denormalize(schema='not a schema')

    def test_schema_to_dict_from_dict(self):
        normalized = self.table.normalize('id', ['num', 'flag', 'd', 'txt'], record_schema=True)
        schema = normalized.normalization_schema

        serialized = schema.to_dict()

        # Must be JSON-serializable
        serialized = json.loads(json.dumps(serialized))

        restored_schema = NormalizationSchema.from_dict(serialized)

        self.assertEqual(restored_schema, schema)

        restored = normalized.denormalize(schema=restored_schema)

        self.assertTablesEqual(restored, self.table)

    def test_schema_from_dict_invalid(self):
        with self.assertRaises(NormalizationError):
            NormalizationSchema.from_dict('not a dict')

        with self.assertRaises(NormalizationError):
            NormalizationSchema.from_dict({'key': []})

        with self.assertRaises(NormalizationError):
            NormalizationSchema.from_dict({
                'version': 999,
                'key': ['id'],
                'properties': ['a'],
                'column_order': ['id', 'a'],
                'column_types': {'id': {'name': 'Number', 'params': {}}, 'a': {'name': 'Text', 'params': {}}},
                'property_column': 'property',
                'value_column': 'value'
            })

        with self.assertRaises(NormalizationError):
            NormalizationSchema.from_dict({
                'version': 1,
                'key': ['id'],
                'properties': ['a'],
                'column_order': ['id', 'a'],
                'column_types': {'id': {'name': 'Nonsense', 'params': {}}, 'a': {'name': 'Text', 'params': {}}},
                'property_column': 'property',
                'value_column': 'value'
            })

    def test_schema_mismatched_metadata_rejected(self):
        with self.assertRaises(NormalizationError):
            NormalizationSchema(
                key=['id'],
                properties=['a'],
                column_types={'id': Number()},
                column_order=['id', 'a']
            )

        with self.assertRaises(NormalizationError):
            NormalizationSchema(
                key=['id'],
                properties=['a'],
                column_types={'id': Number(), 'a': Text()},
                column_order=['id', 'b']
            )

    def test_round_trip_csv(self):
        table = Table(
            [
                ('a', Decimal('1.5'), True, datetime.date(2020, 1, 1), 'x'),
                ('b', Decimal('2'), False, datetime.date(2021, 6, 15), 'y')
            ],
            self.column_names,
            self.column_types
        )

        normalized = table.normalize('id', ['num', 'flag', 'd', 'txt'], record_schema=True)

        output = io.StringIO()
        normalized.to_csv(output)
        output.seek(0)

        reread = Table.from_csv(output)

        # The schema survives separately from the data
        restored = reread.denormalize(schema=normalized.normalization_schema)

        self.assertTablesEqual(restored, table)

    def test_normalize_record_schema_validation(self):
        with self.assertRaises(NormalizationError):
            self.table.normalize('missing', ['num'], record_schema=True)

        with self.assertRaises(NormalizationError):
            self.table.normalize('id', ['missing'], record_schema=True)

        with self.assertRaises(NormalizationError):
            self.table.normalize('id', ['id'], record_schema=True)

        with self.assertRaises(NormalizationError):
            self.table.normalize('id', ['num'], property_column='id', record_schema=True)

        with self.assertRaises(NormalizationError):
            self.table.normalize('id', ['num'], property_column='value', value_column='value', record_schema=True)

        with self.assertRaises(NormalizationError):
            self.table.normalize('id', ['num', 'num'], record_schema=True)

    def test_normalize_inference_mode_unchanged(self):
        normalized = self.table.normalize('id', ['num', 'txt'])

        self.assertIsNone(normalized.normalization_schema)

        # Inference mode still casts values to the inferred value type
        self.assertColumnTypes(normalized, [Text, Text, Text])
        self.assertEqual(normalized.rows[0]['value'], '1')

    def test_denormalize_inference_mode_with_schema_false(self):
        normalized = self.table.normalize('id', ['num', 'txt'], record_schema=True)

        inferred = normalized.denormalize('id', schema=False)

        self.assertColumnNames(inferred, ['id', 'num', 'txt'])

    def test_schema_not_propagated_to_derived_tables(self):
        normalized = self.table.normalize('id', ['num'], record_schema=True)

        filtered = normalized.where(lambda r: r['id'] == 'a')

        self.assertIsNone(filtered.normalization_schema)

    def test_round_trip_all_types(self):
        table = Table(
            [(
                Decimal('1.5'),
                True,
                datetime.date(2020, 1, 1),
                datetime.datetime(2020, 1, 1, 12, 30, 15),
                datetime.timedelta(days=1, hours=2),
                'text'
            )],
            ['num', 'flag', 'd', 'dt', 'td', 'txt'],
            [Number(), Boolean(), Date(), DateTime(), TimeDelta(), Text()]
        )

        restored = table.normalize('num', ['flag', 'd', 'dt', 'td', 'txt'], record_schema=True).denormalize()

        self.assertTablesEqual(restored, table)


class UpperCase(DataType):
    """
    A custom data type used to test restorer registration.
    """
    def cast(self, d):
        if d is None:
            return None

        return str(d).upper()


class TestRoundTripCustomTypes(AgateTestCase):
    def tearDown(self):
        # Ensure the test type stays registered for other tests in this
        # class but does not leak into other test modules' expectations
        pass

    def test_unregistered_custom_type_rejected(self):
        class Unregistered(DataType):
            def cast(self, d):
                return d

        table = Table([(1, 'a')], ['id', 'u'], [Number(), Unregistered()])

        with self.assertRaises(NormalizationError):
            table.normalize('id', 'u', record_schema=True)

    def test_registered_custom_type(self):
        agate.register_data_type(UpperCase, restore=lambda params: UpperCase(), python_type=str)

        table = Table([(1, 'a'), (2, 'b')], ['id', 'u'], [Number(), UpperCase()])

        normalized = table.normalize('id', 'u', record_schema=True)
        restored = normalized.denormalize()

        self.assertIsInstance(restored.column_types[1], UpperCase)
        self.assertRows(restored, [(Decimal(1), 'A'), (Decimal(2), 'B')])

        schema = normalized.normalization_schema.to_dict()

        self.assertEqual(schema['column_types']['u']['name'], 'UpperCase')

        restored_schema = NormalizationSchema.from_dict(schema)

        self.assertIsInstance(restored_schema.column_types['u'], UpperCase)

    def test_register_data_type_validation(self):
        with self.assertRaises(ValueError):
            agate.register_data_type('not a class', restore=lambda params: None)

        with self.assertRaises(ValueError):
            agate.register_data_type(UpperCase, restore='not callable')
