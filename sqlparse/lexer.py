#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""SQL Lexer"""
import re

# This code is based on the SqlLexer in pygments.
# http://pygments.org/
# It's separated from the rest of pygments to increase performance
# and to allow some customizations.
from io import TextIOBase
from threading import Lock

from sqlparse import keywords, tokens
from sqlparse.exceptions import IncompleteInputError
from sqlparse.utils import consume

# Opening quote characters of string-like literals. When such a character
# reaches the lexer's error branch none of the string rules could consume
# it, which (in recovery mode) means its closing quote is missing.
_STRING_OPENERS = ("'", '"', '`', '´')

# Token types whose rules can delimit a string-like literal. The first
# rule starting with an opening quote wins, just like in the match loop.
_STRING_TTYPES = (tokens.String.Single, tokens.String.Symbol, tokens.Name)


def _build_closed_string_patterns(sql_regex):
    """Map each opening quote to the regex of its closed literal.

    Reuse the very patterns the lexer was configured with, so the
    incompleteness check cannot drift from what the lexer normally
    accepts, also for a custom configuration.
    """
    closed = {}
    for pattern, action in sql_regex:
        if not isinstance(action, tokens._TokenType) \
                or action not in _STRING_TTYPES:
            continue
        for quote in _STRING_OPENERS:
            if pattern.startswith(quote) and quote not in closed:
                closed[quote] = re.compile(pattern, re.IGNORECASE | re.UNICODE)
    return tuple((re.compile(re.escape(quote)), closed[quote])
                 for quote in _STRING_OPENERS
                 if quote in closed)


class Lexer:
    """The Lexer supports configurable syntax.
    To add support for additional keywords, use the `add_keywords` method."""

    _default_instance = None
    _lock = Lock()

    # Rules derived from the configured SQL_REGEX; empty until the lexer
    # has been initialised.
    _closed_string_patterns = ()

    # Development notes:
    # - This class is prepared to be able to support additional SQL dialects
    #   in the future by adding additional functions that take the place of
    #   the function default_initialization().
    # - The lexer class uses an explicit singleton behavior with the
    #   instance-getter method get_default_instance(). This mechanism has
    #   the advantage that the call signature of the entry-points to the
    #   sqlparse library are not affected. Also, usage of sqlparse in third
    #   party code does not need to be adapted. On the other hand, the current
    #   implementation does not easily allow for multiple SQL dialects to be
    #   parsed in the same process.
    #   Such behavior can be supported in the future by passing a
    #   suitably initialized lexer object as an additional parameter to the
    #   entry-point functions (such as `parse`). Code will need to be written
    #   to pass down and utilize such an object. The current implementation
    #   is prepared to support this thread safe approach without the
    #   default_instance part needing to change interface.

    @classmethod
    def get_default_instance(cls):
        """Returns the lexer instance used internally
        by the sqlparse core functions."""
        with cls._lock:
            if cls._default_instance is None:
                cls._default_instance = cls()
                cls._default_instance.default_initialization()
        return cls._default_instance

    def default_initialization(self):
        """Initialize the lexer with default dictionaries.
        Useful if you need to revert custom syntax settings."""
        self.clear()
        self.set_SQL_REGEX(keywords.SQL_REGEX)
        self.add_keywords(keywords.KEYWORDS_COMMON)
        self.add_keywords(keywords.KEYWORDS_ORACLE)
        self.add_keywords(keywords.KEYWORDS_MYSQL)
        self.add_keywords(keywords.KEYWORDS_PLPGSQL)
        self.add_keywords(keywords.KEYWORDS_HQL)
        self.add_keywords(keywords.KEYWORDS_MSACCESS)
        self.add_keywords(keywords.KEYWORDS_SNOWFLAKE)
        self.add_keywords(keywords.KEYWORDS_BIGQUERY)
        self.add_keywords(keywords.KEYWORDS)

    def clear(self):
        """Clear all syntax configurations.
        Useful if you want to load a reduced set of syntax configurations.
        After this call, regexps and keyword dictionaries need to be loaded
        to make the lexer functional again."""
        self._SQL_REGEX = []
        self._keywords = []
        self._closed_string_patterns = ()

    def set_SQL_REGEX(self, SQL_REGEX):
        """Set the list of regex that will parse the SQL."""
        FLAGS = re.IGNORECASE | re.UNICODE
        self._SQL_REGEX = [
            (re.compile(rx, FLAGS).match, tt)
            for rx, tt in SQL_REGEX
        ]
        # Derived from the configured patterns, so a custom rule set is
        # checked for unclosed string literals with its own rules.
        self._closed_string_patterns = _build_closed_string_patterns(
            SQL_REGEX)

    def add_keywords(self, keywords):
        """Add keyword dictionaries. Keywords are looked up in the same order
        that dictionaries were added."""
        self._keywords.append(keywords)

    def is_keyword(self, value):
        """Checks for a keyword.

        If the given value is in one of the KEYWORDS_* dictionary
        it's considered a keyword. Otherwise, tokens.Name is returned.
        """
        val = value.upper()
        for kwdict in self._keywords:
            if val in kwdict:
                return kwdict[val], value
        else:
            return tokens.Name, value

    def get_tokens(self, text, encoding=None, raise_on_incomplete=False):
        """
        Return an iterable of (tokentype, value) pairs generated from
        `text`. If `unfiltered` is set to `True`, the filtering mechanism
        is bypassed even if filters are defined.

        Also preprocess the text, i.e. expand tabs and strip it if
        wanted and applies registered filters.

        Split ``text`` into (tokentype, text) pairs.

        ``stack`` is the initial stack (default: ``['root']``)

        If *raise_on_incomplete* is ``True`` an unterminated multiline
        comment or string literal raises :class:`IncompleteInputError`
        instead of falling back to per-character error tokens. The default
        keeps the historical, non-validating tokenization.
        """
        if isinstance(text, TextIOBase):
            text = text.read()

        if isinstance(text, str):
            pass
        elif isinstance(text, bytes):
            if encoding:
                text = text.decode(encoding)
            else:
                try:
                    text = text.decode('utf-8')
                except UnicodeDecodeError:
                    text = text.decode('unicode-escape')
        else:
            raise TypeError(f"Expected text or file-like object, got {type(text)!r}")

        delimited_spans = keywords.find_delimited_spans(text)
        span_openers = delimited_spans.openers

        iterable = enumerate(text)
        for pos, char in iterable:
            # Only positions the lexer actually reaches may open a
            # dollar-quoted literal or a multiline comment; a delimiter
            # inside a string literal or behind a "--" comment is skipped
            # along with its surrounding token and never resolved.
            if pos in span_openers:
                resolved = delimited_spans.resolve(pos)
                if resolved is not None:
                    end, ttype = resolved
                    yield ttype, text[pos:end]
                    consume(iterable, end - pos - 1)
                    continue
                # A multiline comment without its closing ``*/`` has no
                # alternative reading, so the rest of the input cannot be
                # split into statements any more. An unmatched dollar-quote
                # opener may still be ordinary text and keeps its
                # historical tokenization.
                if raise_on_incomplete \
                        and delimited_spans.is_comment_opener(pos):
                    raise IncompleteInputError(
                        "Unclosed multiline comment starting at position "
                        f"{pos}.",
                        remainder=text[pos:])

            for rexmatch, action in self._SQL_REGEX:
                m = rexmatch(text, pos)

                if not m:
                    continue
                elif isinstance(action, tokens._TokenType):
                    yield action, m.group()
                elif action is keywords.PROCESS_AS_KEYWORD:
                    yield self.is_keyword(m.group())

                consume(iterable, m.end() - pos - 1)
                break
            else:
                if raise_on_incomplete and self._is_unclosed_string(text, pos):
                    raise IncompleteInputError(
                        "Unclosed string literal starting at position "
                        f"{pos}.",
                        remainder=text[pos:])
                yield tokens.Error, char

    def _is_unclosed_string(self, text, pos):
        """Return True if a string opened at *pos* has no closing quote.

        Only a quote character that no string rule could consume reaches
        the check, so other error characters (say, a lone backslash left
        over by a greedy match) are still reported as ordinary errors.
        """
        for opener, closed in self._closed_string_patterns:
            if opener.match(text, pos) is not None:
                return closed.match(text, pos) is None
        return False


def tokenize(sql, encoding=None, raise_on_incomplete=False):
    """Tokenize sql.

    Tokenize *sql* using the :class:`Lexer` and return a 2-tuple stream
    of ``(token type, value)`` items.
    """
    return Lexer.get_default_instance().get_tokens(
        sql, encoding, raise_on_incomplete=raise_on_incomplete)
