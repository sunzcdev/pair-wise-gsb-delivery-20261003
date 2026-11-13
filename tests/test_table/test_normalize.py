from agate import Table
from agate.data_types import Number, Text
from agate.testcase import AgateTestCase
from agate.type_tester import TypeTester


class TestNormalize(AgateTestCase):
    def setUp(self):
        self.rows = (
            (1, 'c', 4, 'a'),
            (2, 'e', 3, 'b'),
            (None, 'g', 2, 'c')
        )

        self.number_type = Number()
        self.text_type = Text()

        self.column_names = ['one', 'two', 'three', 'four']
        self.column_types = [self.number_type, self.text_type, self.number_type, self.text_type]

    def test_normalize(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.normalize('one', 'three')

        normal_rows = (
            (1, 'three', 4),
            (2, 'three', 3),
            (None, 'three', 2)
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['one', 'property', 'value'])
        self.assertColumnTypes(normalized_table, [Number, Text, Number])

    def test_normalize_column_types(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.normalize('one', 'three', column_types=[Text(), Text()])

        normal_rows = (
            (1, 'three', '4'),
            (2, 'three', '3'),
            (None, 'three', '2')
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['one', 'property', 'value'])
        self.assertColumnTypes(normalized_table, [Number, Text, Text])

    def test_normalize_column_type_tester(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.normalize('one', 'three', column_types=TypeTester(force={'value': Text()}))

        normal_rows = (
            (1, 'three', '4'),
            (2, 'three', '3'),
            (None, 'three', '2')
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['one', 'property', 'value'])
        self.assertColumnTypes(normalized_table, [Number, Text, Text])

    def test_normalize_type_tester_not_mutated(self):
        table = Table(self.rows, self.column_names, self.column_types)

        type_tester = TypeTester(force={'value': Text()})
        original_force = dict(type_tester._force)

        table.normalize('one', 'three', column_types=type_tester)

        self.assertEqual(type_tester._force, original_force)
        self.assertNotIn('one', type_tester._force)

    def test_normalize_type_tester_reused_across_tables(self):
        number_table = Table(self.rows, self.column_names, self.column_types)
        text_table = Table(
            (
                ('1', 'c', '4', 'a'),
                ('2', 'e', '3', 'b'),
                (None, 'g', '2', 'c')
            ),
            self.column_names,
            [self.text_type, self.text_type, self.text_type, self.text_type]
        )

        tester = TypeTester()

        shared_number = number_table.normalize('one', 'three', column_types=tester)
        shared_text = text_table.normalize('one', 'three', column_types=tester)

        fresh_number = number_table.normalize('one', 'three', column_types=TypeTester())
        fresh_text = text_table.normalize('one', 'three', column_types=TypeTester())

        self.assertEqual(tester._force, {})
        self.assertColumnTypes(shared_number, [type(t) for t in fresh_number.column_types])
        self.assertColumnTypes(shared_text, [type(t) for t in fresh_text.column_types])
        self.assertIsInstance(shared_number.column_types[0], Number)
        self.assertIsInstance(shared_text.column_types[0], Text)

    def test_normalize_type_tester_force_takes_priority(self):
        table = Table(self.rows, self.column_names, self.column_types)

        type_tester = TypeTester(force={'one': Text()})
        original_force = type_tester._force

        normalized_table = table.normalize('one', 'three', column_types=type_tester)

        # The caller's explicit force wins over the key type inferred internally.
        self.assertColumnTypes(normalized_table, [Text, Text, Number])
        self.assertIs(type_tester._force, original_force)
        self.assertIsInstance(type_tester._force['one'], Text)

    def test_normalize_type_tester_empty_table_restored(self):
        table = Table((), self.column_names, self.column_types)

        type_tester = TypeTester(force={'value': Text()})
        original_force = dict(type_tester._force)

        table.normalize('one', 'three', column_types=type_tester)

        self.assertEqual(type_tester._force, original_force)
        self.assertNotIn('one', type_tester._force)

    def test_normalize_multiple_fields(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.normalize('one', ['three', 'four'])

        normal_rows = (
            (1, 'three', '4'),
            (1, 'four', 'a'),
            (2, 'three', '3'),
            (2, 'four', 'b'),
            (None, 'three', '2'),
            (None, 'four', 'c')
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['one', 'property', 'value'])
        self.assertColumnTypes(normalized_table, [Number, Text, Text])

    def test_normalize_multiple_keys(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.normalize(['one', 'two'], ['three', 'four'])

        normal_rows = (
            (1, 'c', 'three', '4'),
            (1, 'c', 'four', 'a'),
            (2, 'e', 'three', '3'),
            (2, 'e', 'four', 'b'),
            (None, 'g', 'three', '2'),
            (None, 'g', 'four', 'c')
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['one', 'two', 'property', 'value'])
        self.assertColumnTypes(normalized_table, [Number, Text, Text, Text])

    def test_normalize_change_order(self):
        table = Table(self.rows, self.column_names, self.column_types)

        normalized_table = table.normalize('three', ['one', 'four'])

        normal_rows = (
            (4, 'one', '1'),
            (4, 'four', 'a'),
            (3, 'one', '2'),
            (3, 'four', 'b'),
            (2, 'one', None),
            (2, 'four', 'c')
        )

        self.assertRows(normalized_table, normal_rows)
        self.assertColumnNames(normalized_table, ['three', 'property', 'value'])
        self.assertColumnTypes(normalized_table, [Number, Text, Text])
