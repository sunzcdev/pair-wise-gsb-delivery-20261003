#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Split SQL scripts into statements and client directive events.

Database export scripts, most notably MySQL/MariaDB dumps, contain client
directives that are never sent to the server.  The most common one is
the ``DELIMITER`` command of the MySQL command line client which changes
the statement terminator so that procedure bodies containing semicolons
can be shipped as a single statement.

This module provides :func:`split_script`, a splitter with an optional
client directive mode.  When the mode is enabled, ``DELIMITER`` lines
that occupy a line of their own are recognized as directive events and
the active terminator changes accordingly.  When the mode is disabled
(the default) the script is split exactly like :func:`sqlparse.split`
does.
"""

import bisect
import re
from dataclasses import dataclass
from typing import ClassVar

from sqlparse import lexer
from sqlparse import tokens as T
from sqlparse.engine.filter_stack import FilterStack
from sqlparse.exceptions import SQLParseError

__all__ = ['StatementEvent', 'DirectiveEvent', 'split_script']


@dataclass
class StatementEvent:
    """A single SQL statement found in a script.

    ``statement`` holds the statement text including the terminating
    delimiter, without surrounding whitespace.  ``delimiter`` is the
    terminator that closed the statement or an empty string if the
    statement was closed by the end of the script or by a directive
    line.  ``start`` and ``end`` are character offsets into the source
    script and ``line_no`` is the 1-based line number of the first
    non-whitespace character of the statement.
    """

    statement: str
    delimiter: str
    start: int
    end: int
    line_no: int
    kind: ClassVar[str] = 'statement'


@dataclass
class DirectiveEvent:
    """A client directive line found in a script.

    ``delimiter`` is the new statement terminator requested by the
    directive.  ``start`` and ``end`` are character offsets into the
    source script covering the whole directive line and ``line_no`` is
    the original 1-based line number of the directive.
    """

    delimiter: str
    start: int
    end: int
    line_no: int
    kind: ClassVar[str] = 'directive'


# A DELIMITER directive must occupy a line of its own.  Everything after
# the keyword (minus surrounding whitespace) is the new delimiter.
_DIRECTIVE_RE = re.compile(
    r'[ \t]*DELIMITER(?:[ \t]+(?P<delim>.*?))?[ \t]*', re.IGNORECASE)


class _LiteralMask:
    """Character ranges of *source* covered by strings or comments."""

    def __init__(self, source, encoding=None):
        ranges = []
        pos = 0
        for ttype, value in lexer.tokenize(source, encoding):
            end = pos + len(value)
            if ttype in T.String or ttype in T.Comment:
                ranges.append((pos, end))
            pos = end
        self._ranges = ranges
        self._starts = [start for start, _ in ranges]

    def covered_until(self, start, end):
        """Return the end of the literal covering [start, end) or None."""
        idx = bisect.bisect_right(self._starts, start) - 1
        for candidate in (idx, idx + 1):
            if 0 <= candidate < len(self._ranges):
                rstart, rend = self._ranges[candidate]
                if rstart < end and start < rend:
                    return rend
        return None


def _line_starts(source):
    """Return the offsets at which lines of *source* begin."""
    starts = [0]
    for match in re.finditer('\n', source):
        starts.append(match.end())
    return starts


def _find_directives(source, mask, line_starts):
    """Return a list of (start, end, delimiter, line_no) directives."""
    directives = []
    offset = 0
    for line in source.splitlines(keepends=True):
        body = line.rstrip('\r\n')
        match = _DIRECTIVE_RE.fullmatch(body)
        if match is not None:
            # The directive is only valid outside strings and comments.
            kw_start = offset + (len(body) - len(body.lstrip(' \t')))
            if mask.covered_until(kw_start, kw_start + 9) is None:
                line_no = bisect.bisect_right(line_starts, offset)
                delimiter = match.group('delim')
                if not delimiter:
                    raise SQLParseError(
                        'Empty delimiter in DELIMITER directive '
                        f'at line {line_no}')
                if any(char.isspace() for char in delimiter):
                    raise SQLParseError(
                        'Invalid whitespace in delimiter of DELIMITER '
                        f'directive at line {line_no}')
                directives.append(
                    (offset, offset + len(line), delimiter, line_no))
        offset += len(line)
    return directives


def _find_terminator(source, delimiter, pos, limit, mask):
    """Return offset of *delimiter* in source[pos:limit] outside literals."""
    while pos < limit:
        idx = source.find(delimiter, pos, limit)
        if idx < 0:
            return None
        covered = mask.covered_until(idx, idx + len(delimiter))
        if covered is None:
            return idx
        pos = covered
    return None


def _tile(events, length):
    """Adjust event ranges so that they tile the whole source script.

    After this pass the events are contiguous: concatenating
    ``source[event.start:event.end]`` for all events in order reproduces
    the source script.
    """
    prev_end = 0
    for event in events:
        event.start = prev_end
        prev_end = event.end
    if events:
        events[-1].end = length
    return events


def _split_client_directives(source, encoding=None):
    """Split *source* honoring MySQL client DELIMITER directives."""
    mask = _LiteralMask(source, encoding)
    line_starts = _line_starts(source)
    directives = _find_directives(source, mask, line_starts)
    events = []
    delimiter = ';'
    seg_start = 0
    pos = 0
    pending = 0
    length = len(source)

    def make_statement(start, end, terminator):
        text = source[start:end]
        statement = text.strip()
        if not statement:
            return None
        first = start + (len(text) - len(text.lstrip()))
        return StatementEvent(
            statement=statement,
            delimiter=terminator,
            start=start,
            end=end,
            line_no=bisect.bisect_right(line_starts, first),
        )

    while True:
        limit = directives[pending][0] if pending < len(directives) else length
        idx = _find_terminator(source, delimiter, pos, limit, mask)
        if idx is not None:
            end = idx + len(delimiter)
            event = make_statement(seg_start, end, delimiter)
            if event is not None:
                events.append(event)
            pos = seg_start = end
            continue
        if pending < len(directives):
            dir_start, dir_end, dir_delim, dir_line = directives[pending]
            # An unterminated statement may precede the directive line.
            event = make_statement(seg_start, dir_start, '')
            if event is not None:
                events.append(event)
            events.append(DirectiveEvent(
                delimiter=dir_delim,
                start=dir_start,
                end=dir_end,
                line_no=dir_line,
            ))
            delimiter = dir_delim
            pos = seg_start = dir_end
            pending += 1
            continue
        break

    event = make_statement(seg_start, length, '')
    if event is not None:
        events.append(event)
    return _tile(events, length)


def _split_default(source, encoding=None):
    """Split *source* using the regular statement splitter."""
    line_starts = _line_starts(source)
    events = []
    cursor = 0
    stack = FilterStack()
    for stmt in stack.run(source, encoding):
        text = str(stmt)
        if not text.strip():
            continue
        start = source.find(text, cursor)
        if start < 0:  # defensive, statements are source slices
            start = cursor
        end = start + len(text)
        cursor = end
        stripped = text.strip()
        first = start + (len(text) - len(text.lstrip()))
        events.append(StatementEvent(
            statement=stripped,
            delimiter=';' if stripped.endswith(';') else '',
            start=start,
            end=end,
            line_no=bisect.bisect_right(line_starts, first),
        ))
    return _tile(events, len(source))


def split_script(source, encoding=None, client_directives=False):
    """Split *source* into statement and client directive events.

    Returns a list of :class:`StatementEvent` and :class:`DirectiveEvent`
    instances in source order.  The events carry character offsets so
    that the source script can be reconstructed from the event ranges:
    concatenating ``source[event.start:event.end]`` for all events
    yields the original script.

    If ``client_directives`` is True, MySQL client style ``DELIMITER``
    commands that occupy a line of their own are returned as
    :class:`DirectiveEvent` instances and change the terminator used to
    split subsequent statements.  Directives inside strings or comments
    have no effect.  Empty delimiters or delimiters containing
    whitespace raise :exc:`~sqlparse.exceptions.SQLParseError`.

    If ``client_directives`` is False (the default) the script is split
    exactly like :func:`sqlparse.split` and only statement events are
    returned.
    """
    if client_directives:
        return _split_client_directives(source, encoding)
    return _split_default(source, encoding)
