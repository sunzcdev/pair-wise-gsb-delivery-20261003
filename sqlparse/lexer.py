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
from collections.abc import Iterable, Mapping
from io import TextIOBase
from threading import Lock

from sqlparse import keywords, tokens
from sqlparse.exceptions import SQLParseError
from sqlparse.utils import consume


class Lexer:
    """The Lexer supports configurable syntax.
    To add support for additional keywords, use the `add_keywords` method."""

    _default_instance = None
    _lock = Lock()

    # Development notes:
    # - This class is prepared to be able to support additional SQL dialects
    #   in the future by adding additional functions that take the place of
    #   the function default_initialization().
    # - The lexer class uses an explicit singleton behavior with the
    #   instance-getter method get_default_instance(). This mechanism has
    #   the advantage that the call signature of the entry-points to the
    #   sqlparse library are not affected. Also, usage of sqlparse in third
    #   party code does not need to be adapted. On the other hand, mutating
    #   the default instance does not allow for multiple SQL dialects to be
    #   parsed in the same process.
    #   Such behavior is supported by passing a LexerConfig (or a suitably
    #   initialized Lexer) as the ``lexer`` argument to the entry-point
    #   functions (such as `parse`). The lexer object is passed down through
    #   FilterStack, which never falls back to the default instance when an
    #   explicit lexer was given. The default_instance part does not need
    #   to change interface for this thread safe approach.

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

    def set_SQL_REGEX(self, SQL_REGEX):
        """Set the list of regex that will parse the SQL."""
        FLAGS = re.IGNORECASE | re.UNICODE
        self._SQL_REGEX = [
            (re.compile(rx, FLAGS).match, tt)
            for rx, tt in SQL_REGEX
        ]

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

    def get_tokens(self, text, encoding=None):
        """
        Return an iterable of (tokentype, value) pairs generated from
        `text`. If `unfiltered` is set to `True`, the filtering mechanism
        is bypassed even if filters are defined.

        Also preprocess the text, i.e. expand tabs and strip it if
        wanted and applies registered filters.

        Split ``text`` into (tokentype, text) pairs.

        ``stack`` is the initial stack (default: ``['root']``)
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
                yield tokens.Error, char


def tokenize(sql, encoding=None):
    """Tokenize sql.

    Tokenize *sql* using the :class:`Lexer` and return a 2-tuple stream
    of ``(token type, value)`` items.
    """
    return Lexer.get_default_instance().get_tokens(sql, encoding)


class LexerConfig:
    """An independent lexical configuration.

    A ``LexerConfig`` can be passed as the ``lexer`` argument to
    :func:`sqlparse.parse`, :func:`sqlparse.parsestream`,
    :func:`sqlparse.split` and :func:`sqlparse.format`. It allows a single
    process to audit SQL from different sources with different keyword
    tables without mutating the shared default :class:`Lexer` instance.

    The configuration is validated eagerly: invalid keyword mappings,
    removals or regex definitions raise :class:`~sqlparse.exceptions.
    SQLParseError` at construction time, i.e. before any SQL is read.

    Everything passed in is copied, so later mutations of the dictionaries
    or lists used at construction time do not affect the configuration.

    :param keywords: Optional mapping of keyword (str) to token type,
        added on top of (and taking precedence over) the inherited tables.
    :param inherit: If True (default), start from the same keyword tables
        the default lexer uses. If False, only *keywords* are recognized.
    :param remove_keywords: Optional iterable of keywords to delete from
        the inherited tables.
    :param sql_regex: Optional list of ``(regex, token type)`` pairs
        replacing the default tokenizing regexes. A token type of
        ``sqlparse.keywords.PROCESS_AS_KEYWORD`` routes matches through
        the keyword lookup.
    """

    _BASE_TABLES = (
        keywords.KEYWORDS_COMMON,
        keywords.KEYWORDS_ORACLE,
        keywords.KEYWORDS_MYSQL,
        keywords.KEYWORDS_PLPGSQL,
        keywords.KEYWORDS_HQL,
        keywords.KEYWORDS_MSACCESS,
        keywords.KEYWORDS_SNOWFLAKE,
        keywords.KEYWORDS_BIGQUERY,
        keywords.KEYWORDS,
    )
    _DEFAULT_SQL_REGEX = keywords.SQL_REGEX
    _PROCESS_AS_KEYWORD = keywords.PROCESS_AS_KEYWORD

    def __init__(self, keywords=None, *, inherit=True,
                 remove_keywords=(), sql_regex=None):
        if not isinstance(inherit, bool):
            raise SQLParseError(
                f'inherit must be a boolean, got {inherit!r}')
        self._inherit = inherit

        self._extra_keywords = self._validate_keywords(keywords)
        self._remove_keywords = self._validate_removals(remove_keywords)
        self._sql_regex = self._validate_sql_regex(sql_regex)

    @staticmethod
    def _validate_keywords(mapping):
        if mapping is None:
            return {}
        if not isinstance(mapping, Mapping):
            raise SQLParseError(
                'keywords must be a mapping of keyword -> token type, '
                f'got {mapping!r}')
        extra = {}
        for word, ttype in mapping.items():
            if not isinstance(word, str) or not word.strip():
                raise SQLParseError(
                    f'keyword must be a non-empty string, got {word!r}')
            if not isinstance(ttype, tokens._TokenType):
                raise SQLParseError(
                    f'token type for keyword {word!r} must be a token '
                    f'type, got {ttype!r}')
            extra[word.upper()] = ttype
        return extra

    @staticmethod
    def _validate_removals(removals):
        if removals is None:
            return frozenset()
        if isinstance(removals, (str, bytes)) \
                or not isinstance(removals, Iterable):
            raise SQLParseError(
                'remove_keywords must be an iterable of keywords, '
                f'got {removals!r}')
        removed = set()
        for word in removals:
            if not isinstance(word, str) or not word.strip():
                raise SQLParseError(
                    f'removed keyword must be a non-empty string, '
                    f'got {word!r}')
            removed.add(word.upper())
        return frozenset(removed)

    @classmethod
    def _validate_sql_regex(cls, sql_regex):
        if sql_regex is None:
            return list(cls._DEFAULT_SQL_REGEX)
        if isinstance(sql_regex, (str, bytes)) \
                or not isinstance(sql_regex, Iterable):
            raise SQLParseError(
                'sql_regex must be an iterable of (regex, token type) '
                f'pairs, got {sql_regex!r}')
        validated = []
        for item in sql_regex:
            try:
                rx, action = item
            except (TypeError, ValueError):
                raise SQLParseError(
                    'sql_regex entries must be (regex, token type) '
                    f'pairs, got {item!r}') from None
            if not isinstance(rx, str):
                raise SQLParseError(
                    f'sql_regex pattern must be a string, got {rx!r}')
            if not isinstance(action, tokens._TokenType) \
                    and action is not cls._PROCESS_AS_KEYWORD:
                raise SQLParseError(
                    f'sql_regex action for {rx!r} must be a token type '
                    f'or keywords.PROCESS_AS_KEYWORD, got {action!r}')
            try:
                re.compile(rx, re.IGNORECASE | re.UNICODE)
            except re.error as err:
                raise SQLParseError(
                    f'invalid sql_regex pattern {rx!r}: {err}') from err
            validated.append((rx, action))
        return validated

    @property
    def keywords(self):
        """A copy of the additional keyword mapping."""
        return dict(self._extra_keywords)

    @property
    def remove_keywords(self):
        """The frozenset of keywords removed from the inherited tables."""
        return self._remove_keywords

    @property
    def inherit(self):
        """Whether the default keyword tables are inherited."""
        return self._inherit

    def build_lexer(self):
        """Create a fresh :class:`Lexer` from this configuration.

        Each call returns a new, independent lexer; the shared default
        instance is never touched.
        """
        lex = Lexer()
        lex.clear()
        lex.set_SQL_REGEX(self._sql_regex)
        if self._extra_keywords:
            # Added first so that it takes precedence over (overrides)
            # the inherited tables.
            lex.add_keywords(dict(self._extra_keywords))
        if self._inherit:
            removed = self._remove_keywords
            merged = {}
            for table in self._BASE_TABLES:
                for word, ttype in table.items():
                    if word not in removed and word not in merged:
                        merged[word] = ttype
            lex.add_keywords(merged)
        return lex
