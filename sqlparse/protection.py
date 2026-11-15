#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Support for protected regions when formatting SQL statements.

Callers may hand the formatter a list of non-overlapping ``(start, end)``
character intervals of the original statement.  Characters inside those
intervals are emitted verbatim: keyword/identifier case options, whitespace
handling, comment stripping and output escaping leave them untouched, while
everything outside the intervals is formatted as usual.  Interval boundaries
must coincide with token boundaries, otherwise
:exc:`~sqlparse.exceptions.SQLParseError` is raised before any processing
takes place.
"""

from bisect import bisect_right
from itertools import pairwise

from sqlparse.exceptions import SQLParseError


class ProtectedStreamItem(tuple):
    """A ``(ttype, value)`` stream item flagged as protected.

    It behaves like the plain 2-tuples yielded by the lexer, so filters
    that do not care about protection can keep unpacking it as
    ``ttype, value``.  Filters that rewrite token values must pass
    protected items through unchanged.
    """

    __slots__ = ()


def normalize_regions(regions):
    """Validate *regions* and return them sorted as ``(start, end)`` pairs.

    Empty regions are dropped.  Raises
    :exc:`~sqlparse.exceptions.SQLParseError` for malformed or
    overlapping regions.
    """
    normalized = []
    for region in regions:
        try:
            start, end = region
        except (TypeError, ValueError):
            raise SQLParseError(
                f'Invalid protected region: {region!r}') from None
        if not (isinstance(start, int) and isinstance(end, int)):
            raise SQLParseError(
                f'Invalid protected region: {region!r}')
        if start < 0:
            raise SQLParseError(
                f'Protected region starts before the statement: {region!r}')
        if end < start:
            raise SQLParseError(
                f'Protected region ends before it starts: {region!r}')
        if start == end:
            continue  # an empty region protects nothing
        normalized.append((start, end))
    normalized.sort()
    for prev, current in pairwise(normalized):
        if current[0] < prev[1]:
            raise SQLParseError(
                f'Protected regions must not overlap: {prev!r} and '
                f'{current!r}')
    return tuple(normalized)


def protect_stream(stream, regions):
    """Validate region boundaries and flag protected stream items.

    The stream is consumed eagerly so that a region boundary that does
    not coincide with a token boundary raises
    :exc:`~sqlparse.exceptions.SQLParseError` before any filter
    processes the statement.  Returns a list of stream items in which
    the items covered by a region are wrapped in
    :class:`ProtectedStreamItem`.
    """
    items = list(stream)
    offsets = []
    pos = 0
    for item in items:
        offsets.append(pos)
        pos += len(item[1])
    boundaries = set(offsets)
    boundaries.add(pos)
    for start, end in regions:
        if start not in boundaries or end not in boundaries:
            raise SQLParseError(
                'Protected region boundaries must coincide with token '
                f'boundaries, got ({start}, {end})')
    marked = []
    idx = 0
    for item, start in zip(items, offsets, strict=True):
        end = start + len(item[1])
        while idx < len(regions) and regions[idx][1] <= start:
            idx += 1
        if (idx < len(regions)
                and regions[idx][0] <= start and end <= regions[idx][1]):
            marked.append(ProtectedStreamItem(item))
        else:
            marked.append(item)
    return marked


def token_is_protected(token):
    """Return ``True`` if *token* is or contains protected content."""
    if token.is_protected:
        return True
    if token.is_group:
        return any(t.is_protected for t in token.flatten())
    return False


def splits_protected_region(tlist, idx):
    """Return ``True`` if inserting a token at *idx* of *tlist* would
    fall inside a protected region, i.e. both neighbors are protected."""
    if idx <= 0 or idx >= len(tlist.tokens):
        return False
    return (token_is_protected(tlist.tokens[idx - 1])
            and token_is_protected(tlist.tokens[idx]))


def protected_spans(stmt):
    """Return the character spans of protected content in ``str(stmt)``.

    The result is a sorted tuple of non-overlapping ``(start, end)``
    pairs.  Adjacent protected tokens are merged into a single span.
    """
    spans = []
    pos = 0
    start = None
    for token in stmt.flatten():
        if token.is_protected:
            if start is None:
                start = pos
        elif start is not None:
            spans.append((start, pos))
            start = None
        pos += len(token.value)
    if start is not None:
        spans.append((start, pos))
    return tuple(spans)


def join_lines_preserving_protected(text, lines, spans):
    """Join *lines* like ``'\\n'.join(line.rstrip() ...)`` would, but
    leave protected characters (including protected line breaks) in
    *text* untouched.  *lines* are the lines of *text* as returned by
    :func:`~sqlparse.utils.split_unquoted_newlines` and *spans* the
    result of :func:`protected_spans`.
    """
    starts = [start for start, _ in spans]

    def is_protected(pos):
        idx = bisect_right(starts, pos) - 1
        return idx >= 0 and pos < spans[idx][1]

    parts = []
    pos = 0
    for line in lines:
        stripped = line.rstrip()
        # Re-add the protected characters that rstrip() removed.
        tail_start = pos + len(stripped)
        tail = line[len(stripped):]
        if tail:
            stripped += ''.join(
                char for offset, char in enumerate(tail, tail_start)
                if is_protected(offset))
        parts.append(stripped)
        sep_start = pos + len(line)
        if sep_start >= len(text):
            break
        sep = '\r\n' if text.startswith('\r\n', sep_start) \
            else text[sep_start]
        # Keep the original line break when it is protected itself.
        parts.append(sep if is_protected(sep_start) else '\n')
        pos = sep_start + len(sep)
    return ''.join(parts)
