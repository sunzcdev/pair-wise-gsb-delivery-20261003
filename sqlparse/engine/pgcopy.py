#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Support for PostgreSQL plain-text dump scripts.

Plain-text dumps produced by ``pg_dump`` embed table data inline: a
``COPY ... FROM stdin;`` statement is followed by raw data rows and the
block is terminated by a line containing only ``\\.``.  The data rows
are not SQL and must never reach the SQL lexer -- they may contain
unbalanced quotes, semicolons and anything else.  This module scans a
dump script and splits it into ordinary SQL text and :class:`CopyBlock`
instances *before* any lexing happens, so the COPY statement and its
data block stay associated and the data is preserved verbatim.
"""

import re
from io import TextIOBase

from sqlparse import keywords, lexer, sql
from sqlparse import tokens as T

__all__ = ['CopyBlock', 'CopyStatement', 'split_dump', 'statement_from_block']

#: Line content that terminates a COPY data block.
_TERMINATOR = '\\.'

# A COPY statement starts at the beginning of a physical line (pg_dump
# always emits it this way) and the keyword is not part of a longer word.
_COPY_KEYWORD = re.compile(r'COPY(?![\w$])', re.IGNORECASE)

# In pg_dump output "FROM stdin" always comes last in the COPY statement.
_FROM_STDIN = re.compile(r'\bFROM\s+STDIN\s*$', re.IGNORECASE)


class CopyBlock:
    """A ``COPY ... FROM stdin`` statement and its inline data block.

    ``header``
        The COPY statement itself, up to and including the newline of
        the physical line that ends the statement.
    ``data``
        The raw data rows, preserved verbatim.  No attempt is made to
        interpret the TSV encoding.
    ``terminator``
        The terminator line (``\\.`` including its line ending), or an
        empty string if the end of the input was reached first.
    """

    __slots__ = ('data', 'header', 'terminator')

    def __init__(self, header, data, terminator):
        self.header = header
        self.data = data
        self.terminator = terminator

    @property
    def complete(self):
        """False when the input ended before the terminator line."""
        return bool(self.terminator)

    def __str__(self):
        return self.header + self.data + self.terminator

    def __repr__(self):
        return (f'<CopyBlock complete={self.complete} '
                f'header={self.header!r}>')


class CopyStatement(sql.Statement):
    """A ``COPY ... FROM stdin`` statement together with its data block.

    The COPY statement itself is lexed normally; the data rows and the
    terminator are attached as raw text tokens so that ``str(stmt)``
    reproduces the block verbatim.
    """

    __slots__ = ('copy_data_complete',)

    def __init__(self, tokens, copy_data_complete):
        super().__init__(tokens)
        #: False when the input ended before the terminator line.
        self.copy_data_complete = copy_data_complete


def _skip_string(text, pos):
    """Skip a single-quoted string starting at *pos*."""
    i = pos + 1
    end = len(text)
    while i < end:
        char = text[i]
        if char == '\\':
            i += 2
        elif char == "'":
            if text[i + 1:i + 2] == "'":
                i += 2  # '' is an escaped quote
            else:
                return i + 1
        else:
            i += 1
    return end


def _skip_identifier(text, pos):
    """Skip a double-quoted identifier starting at *pos*."""
    i = pos + 1
    end = len(text)
    while i < end:
        if text[i] == '"':
            if text[i + 1:i + 2] == '"':
                i += 2  # "" is an escaped quote
            else:
                return i + 1
        else:
            i += 1
    return end


def _skip_line_comment(text, pos):
    """Skip a ``--`` comment; returns the offset of the next newline."""
    nl = text.find('\n', pos)
    return len(text) if nl == -1 else nl


def _scan_copy_header(text, pos, spans):
    """Locate the end of the COPY statement starting at *pos*.

    Returns ``(stmt_end, data_start)`` where ``stmt_end`` is the offset
    just behind the ``;`` terminating the statement and ``data_start``
    the offset of the first data line.  Returns None if the statement
    is never terminated by a semicolon.
    """
    i = pos
    end = len(text)
    while i < end:
        char = text[i]
        if char == ';':
            nl = text.find('\n', i + 1)
            return i + 1, end if nl == -1 else nl + 1
        if i in spans.openers:
            resolved = spans.resolve(i)
            if resolved is not None:
                i = resolved[0]
                continue
        if char == "'":
            i = _skip_string(text, i)
        elif char == '"':
            i = _skip_identifier(text, i)
        elif char == '-' and text.startswith('--', i):
            i = _skip_line_comment(text, i)
        else:
            i += 1
    return None


def _find_terminator(text, pos):
    """Find the terminator line in the data starting at *pos*.

    The terminator is only recognized as a complete physical line whose
    content is ``\\.`` (CRLF line endings and a missing final newline
    are handled).  Returns ``(data_end, block_end)`` or None if the
    input ends before the terminator.
    """
    end = len(text)
    line = pos
    while line < end:
        nl = text.find('\n', line)
        line_end = end if nl == -1 else nl
        content = text[line:line_end]
        if content.endswith('\r'):
            content = content[:-1]
        if content == _TERMINATOR:
            return line, end if nl == -1 else nl + 1
        if nl == -1:
            break
        line = nl + 1
    return None


def _match_copy_block(text, pos, spans):
    """Try to parse a COPY ... FROM stdin block starting at *pos*.

    Returns ``(CopyBlock, end_offset)`` or ``(None, None)`` if there is
    no such block at *pos*.
    """
    if _COPY_KEYWORD.match(text, pos) is None:
        return None, None
    header = _scan_copy_header(text, pos, spans)
    if header is None:
        return None, None
    stmt_end, data_start = header
    if _FROM_STDIN.search(text, pos, stmt_end - 1) is None:
        # COPY ... TO ... or COPY ... FROM PROGRAM ...: no inline data.
        return None, None
    term = _find_terminator(text, data_start)
    if term is None:
        # EOF before the terminator: the block is incomplete.
        return CopyBlock(text[pos:data_start], text[data_start:], ''), len(text)
    data_end, block_end = term
    block = CopyBlock(text[pos:data_start], text[data_start:data_end],
                      text[data_end:block_end])
    return block, block_end


def split_dump(text, encoding=None):
    """Split a PostgreSQL dump script into SQL text and COPY blocks.

    Yields ``str`` segments (ordinary SQL that still needs to be lexed
    and split) and :class:`CopyBlock` instances.  Concatenating
    ``str(segment)`` for all segments reproduces the input.
    """
    if isinstance(text, TextIOBase):
        text = text.read()
    if isinstance(text, bytes):
        if encoding:
            text = text.decode(encoding)
        else:
            try:
                text = text.decode('utf-8')
            except UnicodeDecodeError:
                text = text.decode('unicode-escape')
    elif not isinstance(text, str):
        raise TypeError(f"Expected text or file-like object, got {type(text)!r}")

    if 'COPY' not in text.upper():
        yield text
        return

    spans = keywords.find_delimited_spans(text)
    end = len(text)
    pos = seg_start = 0
    at_line_start = True
    while pos < end:
        char = text[pos]
        if char in ' \t':
            pos += 1
        elif char in '\r\n':
            at_line_start = True
            pos += 1
        elif pos in spans.openers and (r := spans.resolve(pos)) is not None:
            at_line_start = False
            pos = r[0]
        elif char == "'":
            at_line_start = False
            pos = _skip_string(text, pos)
        elif char == '"':
            at_line_start = False
            pos = _skip_identifier(text, pos)
        elif char == '-' and text.startswith('--', pos):
            at_line_start = False
            pos = _skip_line_comment(text, pos)
        elif at_line_start and char in 'Cc':
            block, block_end = _match_copy_block(text, pos, spans)
            if block is None:
                at_line_start = False
                pos += 1
            else:
                if seg_start < pos:
                    yield text[seg_start:pos]
                yield block
                pos = seg_start = block_end
                at_line_start = True
        else:
            at_line_start = False
            pos += 1
    if seg_start < end:
        yield text[seg_start:]


def statement_from_block(block, leading=''):
    """Build a :class:`CopyStatement` from a :class:`CopyBlock`.

    ``leading`` is whitespace between statements that belongs in front
    of the COPY statement.
    """
    tokens = []
    if leading:
        tokens.append(sql.Token(T.Whitespace, leading))
    tokens.extend(sql.Token(ttype, value)
                  for ttype, value in lexer.tokenize(block.header))
    if block.data:
        tokens.append(sql.Token(T.Text, block.data))
    if block.terminator:
        tokens.append(sql.Token(T.Text, block.terminator))
    return CopyStatement(tokens, block.complete)
