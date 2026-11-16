#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""filter"""

from sqlparse import lexer
from sqlparse.engine import grouping, pgcopy
from sqlparse.engine.statement_splitter import StatementSplitter
from sqlparse.exceptions import SQLParseError
from sqlparse.filters import StripTrailingSemicolonFilter


class FilterStack:
    def __init__(self, strip_semicolon=False, pg_dump=False):
        self.preprocess = []
        self.stmtprocess = []
        self.postprocess = []
        self._grouping = False
        #: Enable PostgreSQL dump script mode: COPY ... FROM stdin
        #: statements are associated with their inline data blocks and
        #: the data is kept away from the SQL lexer.
        self.pg_dump = pg_dump
        if strip_semicolon:
            self.stmtprocess.append(StripTrailingSemicolonFilter())

    def enable_grouping(self):
        self._grouping = True

    def _split_sql(self, sql, encoding=None):
        stream = lexer.tokenize(sql, encoding)
        # Process token stream
        for filter_ in self.preprocess:
            stream = filter_.process(stream)
        yield from StatementSplitter().process(stream)

    def _split_statements(self, sql, encoding):
        if not self.pg_dump:
            yield from self._split_sql(sql, encoding)
            return
        # Dump script mode: COPY ... FROM stdin blocks (including their
        # raw data) are carved out of the text before lexing; only the
        # remaining segments go through the regular SQL lexer, so normal
        # statement splitting resumes right after each block.
        pending_ws = ''
        for segment in pgcopy.split_dump(sql, encoding):
            if isinstance(segment, pgcopy.CopyBlock):
                yield pgcopy.statement_from_block(segment,
                                                  leading=pending_ws)
                pending_ws = ''
                continue
            text = pending_ws + segment
            produced = list(self._split_sql(text))
            # The statement splitter drops a trailing whitespace-only
            # remainder; carry it over to the next statement instead.
            consumed = sum(len(str(stmt)) for stmt in produced)
            pending_ws = text[consumed:]
            yield from produced

    def run(self, sql, encoding=None):
        try:
            for stmt in self._split_statements(sql, encoding):
                if self._grouping:
                    stmt = grouping.group(stmt)

                for filter_ in self.stmtprocess:
                    filter_.process(stmt)

                for filter_ in self.postprocess:
                    stmt = filter_.process(stmt)

                yield stmt
        except RecursionError as err:
            raise SQLParseError('Maximum recursion depth exceeded') from err
