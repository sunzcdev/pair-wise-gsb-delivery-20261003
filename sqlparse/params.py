#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Locate driver parameter placeholders in SQL text.

Queries frequently combine driver placeholders with dialect operators
that use the same characters (``?`` JSON operators, ``::`` casts,
``%`` modulo), so classifying placeholders by character alone is
error-prone. This module scans SQL text in a single pass, honoring the
selected dialect's operator lexing, and reports every placeholder
occurrence together with its source range.

The scanner is read-only: it never rewrites the SQL and never extracts
literal values. Text inside quoted strings, quoted identifiers, comments
and ``::`` casts is never reported as a parameter, and ``%%`` escapes
are not miscounted as ``format`` placeholders.
"""

import re
from typing import NamedTuple

from sqlparse.exceptions import MixedParameterStylesError

# Supported parameter styles, modeled after DB-API 2.0 paramstyles plus
# the PostgreSQL dollar-numbered variant.
QMARK = 'qmark'        #: anonymous positional: ``?``
NUMERIC = 'numeric'    #: numbered positional: ``:1``
NAMED = 'named'        #: named: ``:name``
FORMAT = 'format'      #: ANSI C printf format codes: ``%s``
PYFORMAT = 'pyformat'  #: Python extended format codes: ``%(name)s``
DOLLAR = 'dollar'      #: PostgreSQL numbered: ``$1``

STYLES = frozenset((QMARK, NUMERIC, NAMED, FORMAT, PYFORMAT, DOLLAR))

#: Supported dialect names. ``None`` and ``'generic'`` select the
#: dialect-neutral lexing rules.
DIALECTS = frozenset(('generic', 'postgres', 'postgresql', 'mysql'))

_COLON_PARAM = re.compile(r':([A-Za-z_][A-Za-z0-9_]*|[0-9]+)')
_PERCENT_PARAM = re.compile(r'%(?:\((\w+)\))?s')
_DOLLAR_PARAM = re.compile(r'\$([0-9]+|[A-Za-z_][A-Za-z0-9_]*)')
# A dollar-quoted string tag cannot start with a digit, so this never
# conflicts with a ``$1`` placeholder.
_DOLLAR_QUOTE_OPEN = re.compile(r'\$[A-Za-z_][A-Za-z0-9_]*\$|\$\$')


class Parameter(NamedTuple):
    """A single parameter placeholder occurrence."""

    #: The placeholder exactly as it appears in the source.
    text: str
    #: One of the style constants defined in this module.
    style: str
    #: The placeholder name, or ``None`` for anonymous placeholders.
    name: str | None
    #: 0-based ordinal of this occurrence in the returned list.
    index: int
    #: Offset of the first character in the source text.
    start: int
    #: Offset one past the last character in the source text.
    end: int


def _skip_string(sql, pos, quote, backslash):
    # Return the offset behind the quoted string opening at `pos`.
    # Doubled quotes escape themselves; a backslash escapes the next
    # character only where the dialect says so. An unterminated string
    # runs to the end of the text.
    n = len(sql)
    pos += 1
    while pos < n:
        char = sql[pos]
        if backslash and char == '\\':
            pos += 2
        elif char == quote:
            if pos + 1 < n and sql[pos + 1] == quote:
                pos += 2
            else:
                return pos + 1
        else:
            pos += 1
    return n


def _skip_line_comment(sql, pos):
    end = sql.find('\n', pos)
    return len(sql) if end == -1 else end + 1


def _scan(sql, dialect):
    # Yield (style, name, start, end) for every placeholder in `sql`.
    postgres = dialect in ('postgres', 'postgresql')
    mysql = dialect == 'mysql'
    i, n = 0, len(sql)
    while i < n:
        char = sql[i]
        if char == "'":
            # PostgreSQL E'...' strings use backslash escapes.
            backslash = mysql or (
                postgres and i > 0 and sql[i - 1] in 'eE'
                and (i == 1
                     or not (sql[i - 2].isalnum() or sql[i - 2] == '_'))
            )
            i = _skip_string(sql, i, "'", backslash)
        elif char == '"' or char == '`':
            i = _skip_string(sql, i, char, False)
        elif char == '-' and sql.startswith('--', i):
            i = _skip_line_comment(sql, i)
        elif char == '#' and sql.startswith('# ', i):
            i = _skip_line_comment(sql, i)
        elif char == '/' and sql.startswith('/*', i):
            end = sql.find('*/', i + 2)
            i = n if end == -1 else end + 2
        elif char == ':':
            if sql.startswith('::', i) or sql.startswith(':=', i):
                # Type cast or assignment, never a placeholder.
                i += 2
                continue
            m = _COLON_PARAM.match(sql, i)
            if m is None:
                i += 1
                continue
            name = m.group(1)
            style = NUMERIC if name[0].isdigit() else NAMED
            yield style, name, i, m.end()
            i = m.end()
        elif char == '?':
            if postgres and i + 1 < n and sql[i + 1] in '|&':
                # PostgreSQL JSON existence operators ?| and ?&.
                i += 2
            else:
                yield QMARK, None, i, i + 1
                i += 1
        elif char == '%':
            if sql.startswith('%%', i):
                # Escaped percent, not a placeholder.
                i += 2
                continue
            m = _PERCENT_PARAM.match(sql, i)
            if m is None:
                i += 1
                continue
            name = m.group(1)
            if name is None:
                yield FORMAT, None, i, m.end()
            else:
                yield PYFORMAT, name, i, m.end()
            i = m.end()
        elif char == '$':
            if postgres:
                m = _DOLLAR_QUOTE_OPEN.match(sql, i)
                if m is not None:
                    end = sql.find(m.group(), m.end())
                    if end != -1:
                        i = end + m.end() - m.start()
                        continue
                    # An unterminated dollar quote is not a quote at all;
                    # fall through and treat the text as ordinary input.
            m = _DOLLAR_PARAM.match(sql, i)
            if m is None:
                i += 1
            else:
                yield DOLLAR, m.group(1), i, m.end()
                i = m.end()
        else:
            i += 1


def find_params(sql, style=None, dialect=None):
    """Scan *sql* and return a list of :class:`Parameter` occurrences.

    :param sql: A string containing one or more SQL statements.
    :param style: Restrict recognition to a single parameter style (one
        of the constants in :data:`STYLES`). When ``None`` (default),
        all known styles are recognized and the statement must use
        exactly one of them.
    :param dialect: The SQL dialect whose operator lexing is preserved
        while scanning (one of :data:`DIALECTS`, default ``None`` for
        dialect-neutral rules).
    :returns: A list of :class:`Parameter` instances in source order.
    :raises MixedParameterStylesError: if *style* is ``None`` and the
        statement mixes placeholders of more than one style.
    :raises ValueError: for an unknown *style* or *dialect*.
    """
    if style is not None and style not in STYLES:
        raise ValueError(
            f'Unknown parameter style: {style!r} '
            f'(expected one of {sorted(STYLES)})')
    if dialect is None:
        dialect = 'generic'
    elif dialect not in DIALECTS:
        raise ValueError(
            f'Unknown dialect: {dialect!r} '
            f'(expected one of {sorted(DIALECTS)})')

    found = [hit for hit in _scan(sql, dialect)
             if style is None or hit[0] == style]
    if style is None:
        seen = {}
        for hit_style, name, start, end in found:
            seen.setdefault(hit_style, sql[start:end])
        if len(seen) > 1:
            details = ', '.join(
                f'{s} (e.g. {text!r})' for s, text in sorted(seen.items()))
            raise MixedParameterStylesError(
                f'Statement mixes parameter styles: {details}. '
                f'Pass style=... to select a single style.')
    return [Parameter(sql[start:end], hit_style, name, index, start, end)
            for index, (hit_style, name, start, end) in enumerate(found)]
