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
from sqlparse.exceptions import SQLParseError
from sqlparse.utils import consume


#: Sentinel used in :class:`LexerConfig` keyword mappings to remove a
#: keyword inherited from the base vocabulary.
REMOVE_KEYWORD = object()


class LexerConfig:
    """Independent, immutable lexical configuration.

    A config captures everything the :class:`Lexer` needs to tokenize SQL
    without touching the process-wide default lexer: an optional custom
    ``SQL_REGEX`` list and keyword overrides that are layered on top of the
    built-in vocabulary.

    :param sql_regex: Optional list of ``(regex, token_type)`` pairs as used
        in :data:`sqlparse.keywords.SQL_REGEX`. When ``None`` the built-in
        regex list is inherited.
    :param keywords: Optional mapping of keyword -> token type. A value of
        :data:`REMOVE_KEYWORD` removes an inherited keyword. Keys are stored
        normalized to upper case.

    The provided mappings are validated and copied in the constructor, so a
    :class:`LexerConfig` keeps working even if the caller mutates the
    dictionaries it was built from. Invalid input raises
    :class:`~sqlparse.exceptions.SQLParseError` before any SQL is read.
    """

    __slots__ = ('_sql_regex', '_keywords', '_removed_keywords')

    def __init__(self, sql_regex=None, keywords=None):
        self._sql_regex = self._validate_sql_regex(sql_regex)
        self._keywords, self._removed_keywords = self._validate_keywords(
            keywords)

    @staticmethod
    def _validate_sql_regex(sql_regex):
        if sql_regex is None:
            return None
        if not isinstance(sql_regex, (list, tuple)):
            raise SQLParseError(
                'sql_regex must be a list of (regex, token_type) pairs')

        compiled = []
        for entry in sql_regex:
            if not isinstance(entry, (list, tuple)) or len(entry) != 2:
                raise SQLParseError(
                    'Each sql_regex entry must be a (regex, token_type) pair')
            pattern, action = entry
            if not isinstance(pattern, str):
                raise SQLParseError('sql_regex patterns must be strings')
            if not isinstance(action, tokens._TokenType) \
                    and action is not keywords.PROCESS_AS_KEYWORD:
                raise SQLParseError(
                    'sql_regex actions must be a token type or '
                    'keywords.PROCESS_AS_KEYWORD')
            try:
                re.compile(pattern, re.IGNORECASE | re.UNICODE)
            except re.error as err:
                raise SQLParseError(
                    f'Invalid sql_regex pattern {pattern!r}: {err}') from err
            compiled.append((pattern, action))
        return compiled

    @staticmethod
    def _validate_keywords(keyword_map):
        overrides = {}
        removed = set()
        if keyword_map is None:
            return overrides, removed
        if not isinstance(keyword_map, dict):
            raise SQLParseError(
                'keywords must be a mapping of keyword -> token type')

        for key, value in keyword_map.items():
            if not isinstance(key, str) or not key:
                raise SQLParseError(
                    'keyword names must be non-empty strings')
            name = key.upper()
            if value is REMOVE_KEYWORD:
                removed.add(name)
                overrides.pop(name, None)
                continue
            if not isinstance(value, tokens._TokenType):
                raise SQLParseError(
                    f'keyword {key!r} must map to a token type or '
                    'lexer.REMOVE_KEYWORD')
            overrides[name] = value
            removed.discard(name)
        return overrides, removed

    @property
    def sql_regex(self):
        """A copy of the configured regex list, or ``None`` for default."""
        if self._sql_regex is None:
            return None
        return list(self._sql_regex)

    @property
    def keyword_overrides(self):
        """A copy of the keyword -> token type overrides (upper-cased)."""
        return dict(self._keywords)

    @property
    def removed_keywords(self):
        """A copy of the set of removed inherited keywords."""
        return set(self._removed_keywords)


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

    @classmethod
    def from_config(cls, config):
        """Build an independent lexer from a :class:`LexerConfig`.

        The returned lexer starts from the default vocabulary, then applies
        the config's keyword additions, removals and regex overrides. It is
        a separate instance: neither it nor the config affect the default
        singleton, and multiple configs can be interleaved safely.
        """
        if not isinstance(config, LexerConfig):
            raise SQLParseError(
                'lexer_config must be a sqlparse.lexer.LexerConfig instance')
        lex = cls()
        lex.default_initialization(copy_dicts=True)
        if config.sql_regex is not None:
            lex.set_SQL_REGEX(config.sql_regex)
        overrides = config.keyword_overrides
        # Keywords are resolved against the dictionaries in insertion order,
        # so a keyword that already exists in the base vocabulary has to be
        # removed before the override dictionary is appended.
        lex.remove_keywords(config.removed_keywords | set(overrides))
        if overrides:
            lex.add_keywords(overrides)
        return lex

    def default_initialization(self, copy_dicts=False):
        """Initialize the lexer with default dictionaries.
        Useful if you need to revert custom syntax settings.

        When ``copy_dicts`` is true the keyword dictionaries are copied
        before being loaded, so later removals or overrides cannot mutate
        the module-level defaults shared with other lexers.
        """
        self.clear()
        self.set_SQL_REGEX(keywords.SQL_REGEX)
        default_dicts = (
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
        for kwdict in default_dicts:
            self.add_keywords(dict(kwdict) if copy_dicts else kwdict)

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

    def remove_keywords(self, keyword_names):
        """Remove keywords by name from every loaded keyword dictionary.

        Names are matched case-insensitively. Unknown names are ignored, so
        a config can remove a keyword that a dialect does not define.
        """
        names = {name.upper() for name in keyword_names}
        for kwdict in self._keywords:
            for name in names:
                kwdict.pop(name, None)

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


def _resolve_lexer(lexer_config):
    """Return the lexer to use for a call.

    ``None`` selects the process-wide default singleton. An explicit
    :class:`LexerConfig` builds a dedicated lexer; anything else is rejected
    rather than silently falling back to the default lexer.
    """
    if lexer_config is None:
        return Lexer.get_default_instance()
    return Lexer.from_config(lexer_config)


def tokenize(sql, encoding=None, lexer_config=None):
    """Tokenize sql.

    Tokenize *sql* using the :class:`Lexer` and return a 2-tuple stream
    of ``(token type, value)`` items.

    :param lexer_config: Optional :class:`LexerConfig`. When given, an
        independent lexer built from that config is used instead of the
        process-wide default singleton.
    """
    return _resolve_lexer(lexer_config).get_tokens(sql, encoding)
