#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Split SQL statements from a stream of text chunks.

This module implements a statement splitter that consumes text chunk by
chunk (e.g. as packets arrive from the network) and yields complete
statements as soon as they are finished, without ever reading the whole
input up front.  Only the currently unfinished statement is kept in
memory; statements that have been delivered are not retained.

Splitting is chunk-boundary agnostic: cutting the input at any position
-- even in the middle of an escape sequence, a string literal, a
dollar-quoted literal, a comment or right behind a semicolon -- produces
the same statements as :func:`sqlparse.split` on the joined input.
"""

import re
from typing import NamedTuple

from sqlparse import keywords
from sqlparse import tokens as T
from sqlparse.engine.statement_splitter import StatementSplitter
from sqlparse.filters import StripTrailingSemicolonFilter
from sqlparse.lexer import Lexer

# Characters that may start a token which no regex matches yet, but for
# which more input could produce a match (string/quoted-name openers,
# dollar-quoted literals or placeholders, backslash commands).  A lone
# character not listed here can never grow into a larger token.
_HOLDBACK_CHARS = set('\'"`\xb4$\\')

# A "%" placeholder (%s or %(name)s) may still arrive when the buffer
# tail is a prefix of its pattern.
_PERCENT_PREFIX = re.compile(r'%(\(\w*\)?)?$')

# Prefixes of "@name" / "#name" / "##name" tokens.
_AT_HASH_PREFIX = re.compile(r'(?:##|[@#])(?:[A-ZÀ-Ü]\w*)?$', re.IGNORECASE)

# Prefixes of number literals, including hexadecimal and exponent forms
# ("0x", "1E", "1.5E-", ...).
_NUMBER_PREFIX = re.compile(
    r'-?(?:0x[\dA-F]*|(?:\d+(?:\.\d*)?|\.\d+)(?:E-?\d*)?)$', re.IGNORECASE)

_WORD_CHAR = re.compile(r'[$#\w]')
_WHITESPACE = re.compile(r'\s')

# Multi-word keyword patterns of the lexer, as sequences of alternative
# word groups.  A word token must not be committed while the buffer tail
# can still grow into one of these.
_MULTIWORD_SEQS = [
    (('CREATE',), ('OR',), ('REPLACE',)),
    (('END',), ('IF', 'LOOP', 'WHILE', 'FOR', 'CASE')),
    (('IF',), ('NOT',), ('EXISTS',)),
    (('NOT',), ('NULL',)),
    (('NOT',), ('LIKE', 'ILIKE', 'RLIKE')),
    (('NOT',), ('REGEXP',), ('BINARY',)),
    (('REGEXP',), ('BINARY',)),
    (('ASC', 'DESC'), ('NULLS',), ('FIRST', 'LAST')),
    (('NULLS',), ('FIRST', 'LAST')),
    (('UNION',), ('ALL',)),
    (('DOUBLE',), ('PRECISION',)),
    (('GROUP',), ('BY',)),
    (('ORDER',), ('BY',)),
    (('PRIMARY',), ('KEY',)),
    (('HANDLER',), ('FOR',)),
    (('LEFT', 'RIGHT', 'FULL'), ('INNER', 'OUTER', 'STRAIGHT'), ('JOIN',)),
    (('LEFT', 'RIGHT', 'FULL'), ('JOIN',)),
    (('INNER', 'OUTER', 'STRAIGHT'), ('JOIN',)),
    (('CROSS', 'NATURAL'), ('JOIN',)),
    (('LATERAL',), ('VIEW',),
     ('EXPLODE', 'INLINE', 'PARSE_URL_TUPLE', 'POSEXPLODE', 'STACK')),
    (('AT',), ('TIME',), ('ZONE',)),
]

# Prefix of an "AT TIME ZONE '...'" / "WITH' TIME ZONE '...'" cast.
_TZCAST_PREFIX = re.compile(
    r"(AT|WITH')(\s+T?I?M?E?)?(\s+Z?O?N?E?)?(\s*'[^']*)?\s*$",
    re.IGNORECASE)


def _multiword_hold(word, rest):
    """True if `word` + `rest` may still grow into a multi-word keyword."""
    first = word.upper()
    parts = rest.split()
    for seq in _MULTIWORD_SEQS:
        if first not in seq[0]:
            continue
        groups = seq[1:]
        if len(parts) > len(groups):
            continue
        for i, part in enumerate(parts):
            part = part.upper()
            if i < len(parts) - 1:
                if part not in groups[i]:
                    break
            elif not any(g.startswith(part) for g in groups[i]):
                break
        else:
            return True
    return False

#: Token types that are closed by their own trailing delimiter and are
#: therefore final even when they end exactly at the end of the buffer.
_QUOTE_TTYPES = {
    "'": T.String.Single,
    '"': T.String.Symbol,
    '`': T.Name,
    '\xb4': T.Name,
}

# Marker for an opener whose closing delimiter is missing so far.
_UNCLOSED = object()


def _scan_quoted(text, start, quote, escaped, last_choice, final):
    """Scan a quoted literal opening before `start`.

    Returns ``(end, last_choice, resume)``.  ``end`` is the offset just
    past the closing quote if the literal is complete.  Otherwise
    ``last_choice`` is the position of the last quote that backtracking
    in the corresponding lexer regex would turn into the closing quote
    at EOF, and ``resume`` is the offset from which scanning must
    continue when more input arrives (``None`` if everything was
    scanned).
    """
    n = len(text)
    i = start
    while i < n:
        char = text[i]
        if escaped and char == '\\':
            if i + 1 >= n:
                if not final:
                    return None, last_choice, i
                i += 1  # EOF: a trailing backslash is plain content
                continue
            if text[i + 1] == quote:
                last_choice = i + 1
                i += 2
                continue
            i += 1
            continue
        if char == quote:
            if i + 1 >= n:
                if final:
                    return i + 1, None, None
                # The quote may still turn out to be doubled.
                return None, last_choice, i
            if text[i + 1] == quote:
                last_choice = i
                i += 2
                continue
            return i + 1, None, None
        i += 1
    return None, last_choice, None


class IncrementalLexer:
    """Tokenize text that arrives in chunks.

    Tokens are only emitted once more input cannot change them anymore;
    the undecided tail (e.g. an unclosed string literal or a word that
    may still grow) is kept in a small buffer until the next chunk or
    :meth:`close` resolves it.  The concatenation of all tokens emitted
    for a sequence of chunks is identical to tokenizing the joined text
    with :class:`~sqlparse.lexer.Lexer` in one pass.
    """

    def __init__(self, lexer=None):
        self._lexer = lexer or Lexer.get_default_instance()
        self._buf = ''
        # Last committed character, kept so that lookbehind assertions
        # see the same context as in a single-pass tokenization.
        self._context = ''
        # Resume state for a literal whose closer is still missing, or
        # for a word token waiting for the character that decides its
        # lookaheads.  Only valid while nothing was committed after it.
        self._resume = None
        self._closed = False
        #: Set by close() when EOF left an opener unclosed.
        self.unclosed_reason = None

    def feed(self, chunk):
        """Feed a chunk of text and return newly finalized tokens."""
        if self._closed:
            raise ValueError('cannot feed a closed IncrementalLexer')
        self._buf += chunk
        return self._drain(final=False)

    def close(self):
        """Signal end of input and return all remaining tokens."""
        self._closed = True
        return self._drain(final=True)

    def _commit(self, text, pos):
        """Drop everything before `pos`; it has been emitted."""
        new_context = text[pos - 1] if pos > 0 else ''
        # Resume positions are absolute in `text`; shift them so they
        # stay valid for the remaining buffer and its new context.
        delta = len(new_context) - pos
        if delta and self._resume is not None:
            self._resume = self._shift_resume(self._resume, delta)
        self._buf = text[pos:]
        self._context = new_context

    @staticmethod
    def _shift_resume(resume, delta):
        kind = resume[0]
        if kind == 'quote':
            _, quote, escaped, start, last_choice, ttype = resume
            return (kind, quote, escaped, start + delta,
                    None if last_choice is None else last_choice + delta,
                    ttype)
        if kind == 'dollar':
            return kind, resume[1] + delta, resume[2]
        # 'comment', 'bracket' and 'word'
        return kind, resume[1] + delta

    def _drain(self, final):
        text = self._context + self._buf
        pos = len(self._context)
        n = len(text)
        out = []
        if self._resume is not None:
            # The whole buffer is the tail of an undecided token.
            pos = self._resume_scan(text, pos, n, final, out)
            if self._resume is not None:
                # Still undecided; nothing could be committed.
                self._commit(text, pos)
                return out
        while pos < n:
            token = self._scan_one(text, pos, n, final)
            if token is None:  # undecided, wait for more input
                break
            ttype, value, pos = token
            out.append((ttype, value))
        self._commit(text, pos)
        return out

    def _resume_scan(self, text, pos, n, final, out):
        """Continue a token that spanned a chunk boundary.

        Returns the new scan position.  Clears ``self._resume`` when the
        pending token was resolved.
        """
        kind = self._resume[0]
        if kind == 'quote':
            _, quote, escaped, start, last_choice, ttype = self._resume
            end, last_choice, resume = _scan_quoted(
                text, start, quote, escaped, last_choice, final)
            if end is not None:
                self._resume = None
                out.append((ttype, text[pos:end]))
                return end
            if resume is None and final:
                self._resume = None
                if last_choice is not None:
                    end = last_choice + 1
                    out.append((ttype, text[pos:end]))
                    return end
                self._set_unclosed(ttype)
                return pos  # fall through to regular tokenization
            self._resume = ('quote', quote, escaped,
                            resume if resume is not None else n,
                            last_choice, ttype)
            return pos
        if kind == 'comment':
            _, search_from = self._resume
            idx = text.find('*/', search_from)
            if idx != -1:
                self._resume = None
                ttype = T.Comment.Multiline.Hint \
                    if text[pos + 2:pos + 3] == '+' else T.Comment.Multiline
                out.append((ttype, text[pos:idx + 2]))
                return idx + 2
            if final:
                self._resume = None
                self._set_unclosed(T.Comment.Multiline)
                return pos
            self._resume = (kind, max(pos + 2, n - 1))
            return pos
        if kind == 'dollar':
            _, search_from, tag = self._resume
            idx = text.find(tag, search_from)
            if idx != -1:
                self._resume = None
                end = idx + len(tag)
                out.append((T.Literal, text[pos:end]))
                return end
            if final:
                self._resume = None
                self._set_unclosed(T.Literal)
                return pos
            self._resume = (kind, max(pos + len(tag), n - len(tag) + 1),
                            tag)
            return pos
        if kind == 'bracket':
            _, start = self._resume
            end, resume = self._scan_bracket(text, pos, start, final)
            if end is not None:
                self._resume = None
                out.append((T.Name, text[pos:end]))
                return end
            if resume is None:  # failed for good, or EOF
                self._resume = None
                return pos
            self._resume = (kind, resume)
            return pos
        # 'word': a word token waits for the character deciding its
        # trailing lookaheads.
        _, start = self._resume
        j = start
        while j < n and _WHITESPACE.match(text, j):
            j += 1
        if j == n or (text[j] == '.' and j + 1 == n):
            if final:
                self._resume = None
                return pos
            self._resume = (kind, j)
            return pos
        self._resume = None
        return pos

    def _scan_one(self, text, pos, n, final):
        """Scan a single token at `pos`.

        Returns ``(ttype, value, end)`` or ``None`` when the token
        cannot be decided yet and more input is needed.
        """
        char = text[pos]

        # Dollar-quoted literals and multiline comments, resolved at the
        # position the scan has actually reached (as the Lexer does).
        if char == '$' or (char == '/' and text.startswith('/*', pos)):
            span = self._span_at(text, pos, n, final)
            if span is _UNCLOSED:
                return None
            if span is not None:
                return span

        # Quoted literals and quoted names.
        if char in _QUOTE_TTYPES:
            ttype = _QUOTE_TTYPES[char]
            end, last_choice, resume = _scan_quoted(
                text, pos + 1, char, char in '\'"', None, final)
            if end is not None:
                return ttype, text[pos:end], end
            if not final:
                self._resume = ('quote', char, char in '\'"',
                                resume if resume is not None else n,
                                last_choice, ttype)
                return None
            if last_choice is not None:
                end = last_choice + 1
                return ttype, text[pos:end], end
            self._set_unclosed(ttype)
            # No closing quote at all: fall through, the quote character
            # becomes an Error token just like in the single-pass Lexer.

        # Names quoted with [square brackets] (sqlite).
        if char == '[' and self._bracket_opener_ok(text, pos):
            end, resume = self._scan_bracket(text, pos, pos + 1, final)
            if end is not None:
                return T.Name, text[pos:end], end
            if resume is not None and not final:
                self._resume = ('bracket', resume)
                return None
            # Failed: '[' is plain punctuation, handled by the regexes.

        # A token starting here may still be completed by more input
        # into a higher-priority match (placeholder, name, number).
        if not final and self._prefix_hold(text, pos):
            return None

        for rexmatch, action in self._lexer._SQL_REGEX:
            m = rexmatch(text, pos)
            if m:
                break
        if m is None:
            if not final and char in _HOLDBACK_CHARS:
                return None
            return T.Error, char, pos + 1

        value = m.group()
        if action is keywords.PROCESS_AS_KEYWORD:
            ttype, value = self._lexer.is_keyword(value)
        else:
            ttype = action
        if not final and not self._is_final(text, n, pos, m.end(),
                                            ttype, value):
            return None
        return ttype, value, m.end()

    @staticmethod
    def _prefix_hold(text, pos):
        """True if the buffer tail at `pos` is a prefix of a token that
        more input could still complete (placeholder, name, number)."""
        char = text[pos]
        if char == '%':
            return _PERCENT_PREFIX.match(text, pos) is not None
        if char in '@#':
            return _AT_HASH_PREFIX.match(text, pos) is not None
        if char.isdigit() or char in '-.':
            return _NUMBER_PREFIX.match(text, pos) is not None
        return False

    def _span_at(self, text, pos, n, final):
        """Resolve a dollar-quote or comment opener at `pos`."""
        char = text[pos]
        if char == '$':
            m = keywords._DOLLAR_QUOTE_DELIM.match(text, pos)
            if m is None or \
                    keywords._DOLLAR_QUOTE_OPENER_OK.match(text, pos) is None:
                return None
            tag = m.group(1)
            idx = text.find(tag, pos + len(tag))
            if idx != -1:
                return T.Literal, text[pos:idx + len(tag)], idx + len(tag)
            if final:
                self._set_unclosed(T.Literal)
                return None
            self._resume = ('dollar', pos + len(tag), tag)
            return _UNCLOSED
        # Multiline comment opener.
        ttype = T.Comment.Multiline.Hint \
            if text[pos + 2:pos + 3] == '+' else T.Comment.Multiline
        idx = text.find('*/', pos + 2)
        if idx != -1:
            return ttype, text[pos:idx + 2], idx + 2
        if final:
            self._set_unclosed(ttype)
            return None
        self._resume = ('comment', pos + 2)
        return _UNCLOSED

    @staticmethod
    def _bracket_opener_ok(text, pos):
        # Mirrors the lookbehind of the bracket-quoted name regex.
        return pos == 0 or not _WORD_CHAR.match(text, pos - 1) \
            and text[pos - 1] not in '])'

    @staticmethod
    def _scan_bracket(text, pos, start, final):
        """Scan a [bracket-quoted] name; returns ``(end, resume)``."""
        n = len(text)
        i = start
        while i < n:
            char = text[i]
            if char == ']':
                # The pattern requires at least one content character.
                return (i + 1, None) if i > pos + 1 else (None, None)
            if char == '[':
                return None, None
            i += 1
        return None, None if final else n

    def _is_final(self, text, n, pos, end, ttype, value):
        """Check whether a regex token ending at `end` cannot change anymore."""
        if end == n:
            # The token ends at the end of the buffer; only tokens that
            # cannot grow or change type with more input are final.
            if ttype is T.Punctuation and value in ';(),]':
                return True
            if ttype in (T.Comment.Single, T.Comment.Single.Hint) \
                    and value.endswith('\n'):
                return True
            return ttype is T.Error
        if _WORD_CHAR.match(value, len(value) - 1):
            # The token ends in a word character.  Its type may depend
            # on lookahead assertions (e.g. "name .attr" or "func(") or
            # the word may still grow, so the next non-whitespace
            # character must be known.  A trailing dot additionally
            # needs the character behind it ("1." vs "1.5").
            j = end
            while j < n and _WHITESPACE.match(text, j):
                j += 1
            if j == n or (text[j] == '.' and j + 1 == n):
                self._resume = ('word', j)
                return False
            # Multi-word keywords must not be committed while the
            # buffer tail is still a prefix of one.
            if _multiword_hold(value, text[end:]):
                return False
            if _TZCAST_PREFIX.match(text, pos) is not None:
                return False
        return True

    def _set_unclosed(self, ttype):
        if self.unclosed_reason is not None:
            return
        if ttype is T.Literal:
            self.unclosed_reason = 'unclosed dollar-quoted literal'
        elif ttype in (T.Comment.Multiline, T.Comment.Multiline.Hint):
            self.unclosed_reason = 'unclosed multiline comment'
        elif ttype is T.Name:
            self.unclosed_reason = 'unclosed quoted name'
        else:
            self.unclosed_reason = 'unclosed string literal'


class StreamSplitResult(NamedTuple):
    """Result of :meth:`StreamStatementSplitter.close`."""

    #: Statements that were completed by the final flush.
    statements: list
    #: The trailing statement without a terminating semicolon, or None.
    statement: str | None
    #: False when EOF left structures (string, comment, dollar-quoted
    #: literal, parenthesis or BEGIN...END block) unclosed.
    complete: bool
    #: Human readable reason when `complete` is False, else None.
    reason: str | None


class StreamStatementSplitter:
    """Split SQL statements from text chunks fed via :meth:`feed`.

    Completed statements are returned as soon as they are finished and
    are not retained; only the currently unfinished statement is kept
    in memory.  Feeding the same text in different chunk sizes always
    produces the same statements as :func:`sqlparse.split`.
    """

    def __init__(self, encoding=None, strip_semicolon=False):
        self._encoding = encoding
        self._strip_semicolon = strip_semicolon
        self._semicolon_filter = StripTrailingSemicolonFilter() \
            if strip_semicolon else None
        self._lexer = IncrementalLexer()
        self._splitter = StatementSplitter()
        self._closed = False

    def feed(self, chunk):
        """Feed a text chunk; return statements completed by it."""
        if self._closed:
            raise ValueError('cannot feed a closed StreamStatementSplitter')
        if isinstance(chunk, bytes):
            chunk = chunk.decode(self._encoding or 'utf-8')
        elif not isinstance(chunk, str):
            raise TypeError(f'Expected str or bytes, got {type(chunk)!r}')
        return self._process(self._lexer.feed(chunk))

    def close(self):
        """Signal end of input and return a :class:`StreamSplitResult`."""
        self._closed = True
        statements = self._process(self._lexer.close())
        pending = self._splitter._get_pending()
        reason = self._incomplete_reason()
        return StreamSplitResult(
            statements=statements,
            statement=self._convert(pending) if pending is not None else None,
            complete=reason is None,
            reason=reason,
        )

    def _process(self, tokens):
        statements = []
        for ttype, value in tokens:
            stmt = self._splitter._process_token(ttype, value)
            if stmt is not None:
                statements.append(self._convert(stmt))
        return statements

    def _convert(self, stmt):
        if self._semicolon_filter is not None:
            stmt = self._semicolon_filter.process(stmt)
        return str(stmt).strip()

    def _incomplete_reason(self):
        reasons = []
        if self._lexer.unclosed_reason is not None:
            reasons.append(self._lexer.unclosed_reason)
        if self._splitter._parenthesis_level > 0:
            reasons.append('unbalanced parenthesis')
        if self._splitter._block_stack:
            reasons.append(
                f"unclosed block {self._splitter._block_stack[-1]!r}")
        return '; '.join(reasons) if reasons else None
