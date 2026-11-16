"""Layout benchmark for large ``INSERT ... VALUES`` lists.

Measures the reindentation of a statement with n value rows, the shape a
data export produces.  ``_process_values()`` used to probe the row column
and insert the line break per row through ``list.index()`` on the group's
child list, so laying out n rows walked the already laid-out rows n times:
O(n^2) in the number of rows.

Only the layout stage is timed: the payload is parsed (with the grouping
cap lifted, since it is a policy limit rather than a property of the
layout path) once outside the timed call, so the numbers describe the
layout path and not the lexer or grouping.  The ``work`` column reports the
number of child-token comparisons made while laying out, a deterministic
count independent of host speed.  It has to stay constant as rows are
added; a per-row rescan makes it grow quadratically.

Measured on a developer machine (Python 3.12, Windows) with
``--sizes 1000,2000,4000,8000,16000,32000``, the two-column vector::

      rows      input    layout    ratio   comparisons
      2000    26.4 kB    10.9 ms    2.45x            10
      4000    54.4 kB    20.1 ms    1.84x            10
      8000   113.4 kB    43.2 ms    2.15x            10
     16000   239.4 kB    86.4 ms    2.00x            10
     32000   495.4 kB   154.7 ms    1.79x            10

Doubling the row count doubles the time and ``comparisons`` stays at 10
for every size, i.e. the growth is proportional to the input.  Run again
on a busy host the absolute numbers move by tens of percent -- that is
why the table quotes the default sizes and the verdict uses the slope
over the larger ones -- but the ratios stay near 2 and the counter does
not move at all.

Run with:  python benchmarks/bench_values_layout.py
"""

import sys

from _harness import Vector, main

import sqlparse
import sqlparse.engine.grouping as grouping
from sqlparse import filters

# The grouping cap is a policy limit on untrusted input, not a property of
# the layout path.  Lift it so the measured sizes reach the reindentation.
grouping.MAX_GROUPING_TOKENS = None


def values_rows(n):
    """INSERT ... VALUES with n two-column rows."""
    rows = ", ".join(f"({i}, {i * 2})" for i in range(n))
    return f"INSERT INTO t (a, b) VALUES {rows}"


def values_rows_single_column(n):
    """Same with one value per row -- the narrowest possible row."""
    rows = ", ".join(f"({i})" for i in range(n))
    return f"INSERT INTO t VALUES {rows}"


def prepare(sql):
    """Parse once, outside the timed call, and drop the source whitespace."""
    stmt = sqlparse.parse(sql)[0]
    return filters.StripWhitespaceFilter().process(stmt)


def layout(stmt):
    """Run the reindentation and serializer on a prepared statement."""
    stmt = filters.ReindentFilter(char=" ", width=2).process(stmt)
    filters.SerializerUnicode().process(stmt)


def count_scans(stmt):
    """Count child-token comparisons made while laying out a statement."""
    scans = [0]
    original = sqlparse.sql.TokenList.token_index

    def token_index(self, token, start=0):
        start = start if isinstance(start, int) else token_index(self, start)
        for idx in range(start, len(self.tokens)):
            scans[0] += 1
            if self.tokens[idx] is token:
                return idx
        raise ValueError("token is not a child of the list")

    sqlparse.sql.TokenList.token_index = token_index
    try:
        layout(prepare(str(stmt)))
    finally:
        sqlparse.sql.TokenList.token_index = original
    return {"comparisons": scans[0]}


VECTORS = [
    Vector("VALUES rows, 2 columns", values_rows, (1000, 2000, 4000, 8000),
           layout, prepare, count_scans),
    Vector("VALUES rows, 1 column", values_rows_single_column,
           (1000, 2000, 4000, 8000), layout, prepare, count_scans),
]

if __name__ == "__main__":
    sys.exit(main("VALUES list layout", VECTORS,
                  note="sqlparse.format(reindent=True) -- layout stage only",
                  argv=sys.argv[1:]))
