#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""filter"""

from sqlparse.engine import grouping
from sqlparse.engine.statement_splitter import StatementSplitter
from sqlparse.exceptions import SQLParseError
from sqlparse.filters import StripTrailingSemicolonFilter
from sqlparse.lexer import Lexer, LexerConfig


class FilterStack:
    def __init__(self, strip_semicolon=False, lexer=None):
        self.preprocess = []
        self.stmtprocess = []
        self.postprocess = []
        self._grouping = False
        self._lexer = self._resolve_lexer(lexer)
        if strip_semicolon:
            self.stmtprocess.append(StripTrailingSemicolonFilter())

    @staticmethod
    def _resolve_lexer(lexer):
        """Resolve the lexer to use for this stack.

        ``None`` selects the shared default lexer instance (the historical
        singleton behavior). A :class:`~sqlparse.lexer.LexerConfig` is
        materialized into a fresh lexer, and a :class:`~sqlparse.lexer.Lexer`
        is used as-is. Anything else is rejected right away instead of
        silently falling back to the default lexer.
        """
        if lexer is None:
            return Lexer.get_default_instance()
        if isinstance(lexer, LexerConfig):
            return lexer.build_lexer()
        if isinstance(lexer, Lexer):
            return lexer
        raise SQLParseError(
            'lexer must be None, a sqlparse.lexer.Lexer instance or a '
            f'sqlparse.lexer.LexerConfig, got {lexer!r}')

    def enable_grouping(self):
        self._grouping = True

    def run(self, sql, encoding=None):
        try:
            stream = self._lexer.get_tokens(sql, encoding)
            # Process token stream
            for filter_ in self.preprocess:
                stream = filter_.process(stream)

            stream = StatementSplitter().process(stream)

            # Output: Stream processed Statements
            for stmt in stream:
                if self._grouping:
                    stmt = grouping.group(stmt)

                for filter_ in self.stmtprocess:
                    filter_.process(stmt)

                for filter_ in self.postprocess:
                    stmt = filter_.process(stmt)

                yield stmt
        except RecursionError as err:
            raise SQLParseError('Maximum recursion depth exceeded') from err
