from agate import (
    COMPUTED,
    ORIGINAL,
    Computation,
    ComputationSpec,
    Dependency,
    Formula,
    Number,
    Table,
)
from agate.exceptions import DataTypeError
from agate.testcase import AgateTestCase


class CountingFormula(Formula):
    def __init__(self, data_type, func, dependencies=None):
        super().__init__(data_type, func, dependencies=dependencies)
        self.runs = 0

    def run(self, table):
        self.runs += 1
        return super().run(table)


class TestComputeDependencies(AgateTestCase):
    def setUp(self):
        self.table = Table(
            ((1, 2), (3, 4)),
            ('a', 'b'),
            (Number(), Number())
        )

    def test_computes_in_dependency_order(self):
        second = CountingFormula(Number(), lambda r: r['first'] * 2, dependencies=['first'])

        new_table = self.table.compute_dependencies([
            ('second', second, ['first']),
            ('first', Formula(Number(), lambda r: r['a'] + r['b']), ['a', 'b'])
        ])

        self.assertColumnNames(new_table, ['a', 'b', 'first', 'second'])
        self.assertSequenceEqual(new_table.columns['first'], (3, 7))
        self.assertSequenceEqual(new_table.columns['second'], (6, 14))
        self.assertEqual(second.runs, 1)

    def test_computation_runs_only_once(self):
        first = CountingFormula(Number(), lambda r: r['a'] + 1, dependencies=['a'])

        self.table.compute_dependencies([
            ('first', first, ['a']),
            ('second', Formula(Number(), lambda r: r['first'] + 1), ['first']),
            ('third', Formula(Number(), lambda r: r['first'] + 2), ['first'])
        ])

        self.assertEqual(first.runs, 1)

    def test_replacement_distinguishes_original_and_computed(self):
        new_table = self.table.compute_dependencies([
            ComputationSpec(
                'a',
                Formula(Number(), lambda r: r['a'] * 10),
                [Dependency('a', ORIGINAL)],
                replace=True
            ),
            ComputationSpec(
                'from_original',
                Formula(Number(), lambda r: r['a']),
                [Dependency('a', ORIGINAL)]
            ),
            ComputationSpec(
                'from_computed',
                Formula(Number(), lambda r: r['a']),
                [Dependency('a', COMPUTED)]
            )
        ], replace=True)

        self.assertSequenceEqual(new_table.columns['from_original'], (1, 3))
        self.assertSequenceEqual(new_table.columns['from_computed'], (10, 30))

    def test_structural_errors_happen_before_run(self):
        computation = CountingFormula(Number(), lambda r: r['missing'])

        with self.assertRaises(ValueError):
            self.table.compute_dependencies([('x', computation, ['missing'])])

        self.assertEqual(computation.runs, 0)

        with self.assertRaises(ValueError):
            self.table.compute_dependencies([
                ('x', computation, ['y']),
                ('y', computation, ['x'])
            ])

        self.assertEqual(computation.runs, 0)

        with self.assertRaises(ValueError):
            self.table.compute_dependencies([
                ('x', computation, ['a']),
                ('x', computation, ['b'])
            ])

        self.assertEqual(computation.runs, 0)

    def test_empty_table_is_still_checked(self):
        table = Table((), ('a', 'b'), (Number(), Number()))
        computation = CountingFormula(Number(), lambda r: r['a'] + r['b'], dependencies=['a', 'b'])

        new_table = table.compute_dependencies([
            ComputationSpec('sum', computation, ['a', 'b'])
        ])

        self.assertEqual(len(new_table.rows), 0)
        self.assertColumnNames(new_table, ['a', 'b', 'sum'])
        self.assertColumnTypes(new_table, [Number, Number, Number])
        self.assertEqual(computation.runs, 1)

    def test_output_row_count_and_type_are_validated(self):
        class TooShort(Computation):
            def get_computed_data_type(self, table):
                return Number()

            def run(self, table):
                return []

        class WrongType(Computation):
            def get_computed_data_type(self, table):
                return Number()

            def run(self, table):
                return ['not a number' for row in table.rows]

        with self.assertRaises(ValueError):
            self.table.compute_dependencies([('x', TooShort(), ['a'])])

        with self.assertRaises(DataTypeError):
            self.table.compute_dependencies([('x', WrongType(), ['a'])])

    def test_old_compute_keeps_original_table_semantics(self):
        with self.assertRaises(KeyError):
            self.table.compute([
                ('first', Formula(Number(), lambda r: r['a'] + 1)),
                ('second', Formula(Number(), lambda r: r['first'] + 1))
            ])
