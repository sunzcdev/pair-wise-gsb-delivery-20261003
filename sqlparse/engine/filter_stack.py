#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""filter"""

from sqlparse import lexer
from sqlparse.engine import grouping
from sqlparse.engine.statement_splitter import StatementSplitter
from sqlparse.exceptions import SQLParseError
from sqlparse.filters import StripTrailingSemicolonFilter


class FilterStack:
    def __init__(self, strip_semicolon=False, recover=False):
        self.preprocess = []
        self.stmtprocess = []
        self.postprocess = []
        self._grouping = False
        self._recover = recover
        if strip_semicolon:
            self.stmtprocess.append(StripTrailingSemicolonFilter())

    def enable_grouping(self):
        self._grouping = True

    def run(self, sql, encoding=None):
        try:
            stream = lexer.tokenize(
                sql, encoding, raise_on_incomplete=self._recover)
            # Process token stream
            for filter_ in self.preprocess:
                stream = filter_.process(stream)

            stream = StatementSplitter(recover=self._recover).process(stream)

            # Output: Stream processed Statements
            for stmt in stream:
                # Statements flagged by the splitter (an incomplete
                # trailing remainder) or by a failed grouping keep their
                # original text. The formatting filters assume a
                # successfully grouped tree, so they are skipped for those.
                skip_stmt_filters = self._recover and (
                    stmt.is_incomplete or stmt.grouping_error is not None)
                if self._grouping and not skip_stmt_filters:
                    try:
                        stmt = grouping.group(stmt)
                    except RecursionError:
                        # Isolate the failing statement instead of losing
                        # the rest of the batch; without recovery the outer
                        # handler turns this into a SQLParseError.
                        if not self._recover:
                            raise
                        stmt.grouping_error = SQLParseError(
                            'Maximum recursion depth exceeded')
                        skip_stmt_filters = True
                    except SQLParseError as err:
                        if not self._recover:
                            raise
                        # Keep the ungrouped statement and its diagnosis
                        # so the batch can continue with the next one.
                        stmt.grouping_error = err
                        skip_stmt_filters = True

                if not skip_stmt_filters:
                    for filter_ in self.stmtprocess:
                        filter_.process(stmt)

                for filter_ in self.postprocess:
                    stmt = filter_.process(stmt)

                yield stmt
        except RecursionError as err:
            raise SQLParseError('Maximum recursion depth exceeded') from err
