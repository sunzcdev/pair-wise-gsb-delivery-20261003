from agate import Table, TableSet
from agate.exceptions import CastError
from agate.data_types import Number, Text
from agate.testcase import AgateTestCase
from agate.type_tester import TypeTester


class TestDenormalize(AgateTestCase):
    def setUp(self):
        self.rows = (
            ('Jane', 'Code', 'gender', 'female'),
            ('Jane', 'Code', 'age', '27'),
            ('Jim', 'Program', 'gender', 'male'),
            ('Jim', 'Bytes', 'age', '24')
        )

        self.text_type = Text()

        self.column_names = ['first_name', 'last_name', 'property', 'value']
        self.column_types = [self.text_type, self.text_type, self.text_type, self.text_type]

    def test_denormalize(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.denormalize('first_name', 'property', 'value')

        normal_rows = (
            ('Jane', 'female', 27),
            ('Jim', 'male', 24),
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['first_name', 'gender', 'age'])
        self.assertColumnTypes(normalized_table, [Text, Text, Number])
        self.assertRowNames(normalized_table, ['Jane', 'Jim'])

    def test_denormalize_no_key(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.denormalize(None, 'property', 'value')

        # NB: value has been overwritten
        normal_rows = (
            ('male', 24),
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['gender', 'age'])
        self.assertColumnTypes(normalized_table, [Text, Number])

    def test_denormalize_multiple_keys(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.denormalize(['first_name', 'last_name'], 'property', 'value')

        normal_rows = (
            ('Jane', 'Code', 'female', 27),
            ('Jim', 'Program', 'male', None),
            ('Jim', 'Bytes', None, 24),
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['first_name', 'last_name', 'gender', 'age'])
        self.assertColumnTypes(normalized_table, [Text, Text, Text, Number])
        self.assertRowNames(normalized_table, [('Jane', 'Code'), ('Jim', 'Program'), ('Jim', 'Bytes')])

    def test_denormalize_default_value(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.denormalize(['first_name', 'last_name'], 'property', 'value', default_value='hello')

        normal_rows = (
            ('Jane', 'Code', 'female', '27'),
            ('Jim', 'Program', 'male', 'hello'),
            ('Jim', 'Bytes', 'hello', '24'),
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['first_name', 'last_name', 'gender', 'age'])
        self.assertColumnTypes(normalized_table, [Text, Text, Text, Text])

    def test_denormalize_column_types(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.denormalize(None, 'property', 'value', column_types=[Text(), Number()])

        # NB: value has been overwritten
        normal_rows = (
            ('male', 24),
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['gender', 'age'])
        self.assertColumnTypes(normalized_table, [Text, Number])

    def test_denormalize_column_type_tester(self):
        table = Table(self.rows, self.column_names, self.column_types)

        type_tester = TypeTester(force={'gender': Text()})
        normalized_table = table.denormalize(None, 'property', 'value', column_types=type_tester)

        # NB: value has been overwritten
        normal_rows = (
            ('male', 24),
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['gender', 'age'])
        self.assertColumnTypes(normalized_table, [Text, Number])

    def test_denormalize_type_tester_not_mutated(self):
        table = Table(self.rows, self.column_names, self.column_types)

        type_tester = TypeTester(force={'gender': Text()})
        original_force = dict(type_tester._force)

        table.denormalize('first_name', 'property', 'value', column_types=type_tester)

        self.assertEqual(type_tester._force, original_force)
        self.assertNotIn('first_name', type_tester._force)

    def test_denormalize_type_tester_reused_across_tables(self):
        text_table = Table(
            (
                ('a', 'gender', 'female'),
                ('a', 'age', '27'),
                ('b', 'gender', 'male'),
                ('b', 'age', '24'),
            ),
            self.column_names,
            self.column_types
        )
        number_table = Table(
            (
                (1, 'gender', 'female'),
                (1, 'age', '27'),
                (2, 'gender', 'male'),
                (2, 'age', '24'),
            ),
            self.column_names,
            [Number(), self.text_type, self.text_type, self.text_type]
        )

        tester = TypeTester()

        shared_text = text_table.denormalize('first_name', 'property', 'value', column_types=tester)
        shared_number = number_table.denormalize('first_name', 'property', 'value', column_types=tester)

        fresh_text = text_table.denormalize('first_name', 'property', 'value', column_types=TypeTester())
        fresh_number = number_table.denormalize('first_name', 'property', 'value', column_types=TypeTester())

        self.assertEqual(tester._force, {})
        self.assertColumnTypes(shared_text, [type(t) for t in fresh_text.column_types])
        self.assertColumnTypes(shared_number, [type(t) for t in fresh_number.column_types])
        self.assertIsInstance(shared_number.column_types[0], Number)
        self.assertIsInstance(shared_text.column_types[0], Text)

    def test_denormalize_type_tester_force_takes_priority(self):
        table = Table(
            (
                (1, 'gender', 'female'),
                (1, 'age', '27'),
                (2, 'gender', 'male'),
                (2, 'age', '24'),
            ),
            ['id', 'property', 'value'],
            [Number(), self.text_type, self.text_type]
        )

        type_tester = TypeTester(force={'id': Text()})
        original_force = type_tester._force

        normalized_table = table.denormalize('id', 'property', 'value', column_types=type_tester)

        # The caller's explicit force wins over the key type inferred internally.
        self.assertColumnTypes(normalized_table, [Text, Text, Number])
        self.assertIs(type_tester._force, original_force)
        self.assertIsInstance(type_tester._force['id'], Text)

    def test_denormalize_type_tester_inference_failure_restored(self):
        table = Table(
            (
                ('Jane', 'Code', 'note', 'abc'),
            ),
            self.column_names,
            self.column_types
        )

        type_tester = TypeTester(types=[Number()])
        original_force = dict(type_tester._force)

        with self.assertRaises(CastError):
            table.denormalize('first_name', 'property', 'value', column_types=type_tester)

        self.assertEqual(type_tester._force, original_force)
        self.assertNotIn('first_name', type_tester._force)

    def test_denormalize_type_tester_empty_table_restored(self):
        table = Table((), self.column_names, self.column_types)

        type_tester = TypeTester(force={'gender': Text()})
        original_force = dict(type_tester._force)

        table.denormalize('first_name', 'property', 'value', column_types=type_tester)

        self.assertEqual(type_tester._force, original_force)
        self.assertNotIn('first_name', type_tester._force)

    def test_denormalize_tableset_type_tester_not_mutated(self):
        tables = TableSet([
            Table(self.rows, self.column_names, self.column_types),
            Table(self.rows, self.column_names, self.column_types)
        ], ['one', 'two'], key_name='group')

        type_tester = TypeTester()

        new_tableset = tables.denormalize('first_name', 'property', 'value', column_types=type_tester)

        self.assertIsInstance(new_tableset, TableSet)
        self.assertEqual(type_tester._force, {})

        for table in new_tableset.values():
            self.assertColumnTypes(table, [Text, Text, Number])
