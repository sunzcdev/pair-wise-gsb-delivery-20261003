"""Lexer benchmark for delimited literals (GHSA-prg7-hcfm-mfcr).

Measures parse time for SQL containing many unique, unmatched opening
delimiters for the two lexer constructs that used a lazy dot-all regex
(`[\\s\\S]*?`) terminated by a backreference or a literal closing
sequence:

- Dollar-quoted literals, e.g. `$a0$x $a1$x ... $aN$x` (backreference).
- Multiline comments, e.g. `/* unique0 ... /* unique1 ...` (literal `*/`).
- Template regions, e.g. `{{ ... '}}' ... }} ...` (the closing delimiter
  is hidden inside a quoted segment, so each span has to be scanned to
  find the real closer).

Without a closing delimiter, such a quantifier applied at every text
position has to scan to the end of the remaining input for every opener,
which is O(n^2) in the number of openers.  Delimiters are resolved from
precomputed positions now, so growth should be linear.

Run with:  python benchmarks/bench_lexer_delimiters.py
"""

import sys

from _harness import Vector, main

from sqlparse.engine import grouping

# Disable the grouping-stage DoS guards.  They fire only after lexing
# completes and do not bound regex CPU time, so they would otherwise mask
# the lexer's true timing behind a SQLParseError at larger n.
grouping.MAX_GROUPING_DEPTH = None
grouping.MAX_GROUPING_TOKENS = None

SIZES = (250, 500, 1000, 2000, 4000, 8000)


def unmatched_dollar_quotes(n):
    """n never-closed dollar-quote openers, each with a unique tag so the
    backreference cannot short-circuit on an earlier match."""
    return ' '.join(f'$a{i}$x' for i in range(n))


def unclosed_comments(n):
    """n never-closed multiline comment openers.  No '*/' appears anywhere,
    so the closing literal can never short-circuit the scan."""
    return ' '.join(f'/* unique{i} comment never closed' for i in range(n))


def unclosed_template_regions(n):
    """n never-closed template openers.  Each region also holds a quoted
    segment, exercising the quote-skipping path on every resolve while
    no closing delimiter can ever match."""
    return ' '.join("{{ unique%d x ~ 'never closed" % i for i in range(n))


def template_regions_with_quoted_closers(n):
    """n closed template regions whose naive first-found closer sits
    inside a quoted segment.  The resolver has to skip the quote before
    pairing the real closer for every region."""
    return ' '.join("{{ x%d ~ '}}' }} " % i for i in range(n))


VECTORS = [
    Vector('unmatched dollar-quote openers', unmatched_dollar_quotes, SIZES),
    Vector('unclosed multiline comments', unclosed_comments, SIZES),
    Vector('unclosed template regions', unclosed_template_regions, SIZES),
    Vector('template regions with quoted closers',
           template_regions_with_quoted_closers, SIZES),
]

if __name__ == '__main__':
    sys.exit(main('lexer delimiters', VECTORS,
                  note='GHSA-prg7-hcfm-mfcr -- sqlparse.parse',
                  argv=sys.argv[1:]))
