"""Comment stripping benchmark.

Measures ``format(sql, strip_comments=True)`` on input whose size is
dominated by comments.  A filter that removes comments one at a time from
the middle of a token list shifts the remaining tokens on every removal,
which is quadratic in the number of comments; the same holds for walking
up the parent chain to find a comment neighbour.  Both shapes below make
that visible, one with flat comment lists and one with comments spread
across many groups.

Run with:  python benchmarks/bench_strip_comments.py
"""

import sys

from _harness import Vector, main

import sqlparse


def line_comments(n):
    """n line comments in front of a statement."""
    return '-- c\n' * n + 'select 1'


def commented_columns(n):
    """A column list where every entry carries a comment."""
    columns = ', '.join(f'/* c */ col_{i}' for i in range(n))
    return f'select {columns} from t'


def strip(sql):
    return sqlparse.format(sql, strip_comments=True,
                           strip_comments_keep='none')


VECTORS = [
    Vector('line comments', line_comments, (1000, 2000, 4000, 8000), strip),
    Vector('commented columns', commented_columns, (200, 400, 800, 1600),
           strip),
]

if __name__ == '__main__':
    sys.exit(main('comment stripping', VECTORS,
                  note='sqlparse.format(strip_comments=True)',
                  argv=sys.argv[1:]))
