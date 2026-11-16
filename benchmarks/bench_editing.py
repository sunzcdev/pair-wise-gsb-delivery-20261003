"""Range-editing benchmark.

Measures a single leaf replacement on an already grouped statement
followed by :func:`~sqlparse.check_structure`.  Both paths walk the whole
tree, so their cost must grow linearly with the number of tokens; a
regression here would make editing large statements quadratic.

Run with:  python benchmarks/bench_editing.py
"""

import sys

from _harness import Vector, main

import sqlparse
from sqlparse.editing import RangeEditor, check_structure

from sqlparse.engine import grouping

# The grouping guards would reject the larger payloads before the measured
# editing path is reached.
grouping.MAX_GROUPING_DEPTH = None
grouping.MAX_GROUPING_TOKENS = None


def wide_column_list(n):
    columns = ', '.join(f'col_{i}' for i in range(n))
    return f'SELECT {columns} FROM t'


def nested_parentheses(n):
    return 'SELECT ' + '(' * n + '1' + ')' * n


def edit_first_column(sql):
    statement = sqlparse.parse(sql)[0]
    text = str(statement)
    if 'col_0,' in text:
        start, end = text.index('col_0,'), text.index('col_0,') + 5
    else:
        start, end = text.rindex('1'), text.rindex('1') + 1
    RangeEditor(statement).replace(start, end, 'X')
    return check_structure(statement)


VECTORS = [
    Vector('wide column list edit', wide_column_list,
           (250, 500, 1000, 2000), edit_first_column),
    Vector('nested parentheses edit', nested_parentheses,
           (50, 100, 200, 400), edit_first_column),
]

if __name__ == '__main__':
    sys.exit(main('range editing', VECTORS,
                  note='parse + one leaf replacement + check_structure',
                  argv=sys.argv[1:]))
