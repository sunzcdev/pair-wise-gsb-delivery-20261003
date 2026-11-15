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


def _session_for(filter_):
    """Return the filter instance to use for a single run.

    A filter that defines ``begin()`` gets a fresh, run-private instance so
    that state accumulated while consuming the generator cannot leak into the
    next run.  Filters without ``begin()`` are returned unchanged for
    backwards compatibility -- they must be safe to share across runs
    themselves.
    """
    begin = getattr(filter_, 'begin', None)
    if begin is None:
        return filter_
    return begin()


class FilterStack:
    """A pipeline of filters applied to a token stream.

    A stack can be reused across several :meth:`run` calls, and the returned
    generators may be interleaved or closed early.  Each :meth:`run` opens a
    new session: filters exposing ``begin()`` hand back a fresh, run-private
    instance, so in-progress state (indentation, counters, generators) from a
    consumed, failed or abandoned run never reaches another run.  Filters
    without ``begin()`` are shared as-is for backwards compatibility and must
    remain stateless to be safely reusable.
    """

    def __init__(self, strip_semicolon=False):
        self.preprocess = []
        self.stmtprocess = []
        self.postprocess = []
        self._grouping = False
        if strip_semicolon:
            self.stmtprocess.append(StripTrailingSemicolonFilter())

    def enable_grouping(self):
        self._grouping = True

    def run(self, sql, encoding=None):
        # Build the session for this run up front.  Everything below lives in
        # the generator's local frame, so two interleaved ``run()`` calls
        # each hold their own filters and closing one generator does not
        # touch the other.  Stateful filters hand back a reset copy via
        # ``begin()``; a failed or abandoned run therefore cannot pollute the
        # prototype reused by subsequent runs.
        preprocess = [_session_for(f) for f in self.preprocess]
        stmtprocess = [_session_for(f) for f in self.stmtprocess]
        postprocess = [_session_for(f) for f in self.postprocess]
        splitter = StatementSplitter()

        try:
            stream = lexer.tokenize(sql, encoding)
            # Process token stream
            for filter_ in preprocess:
                stream = filter_.process(stream)

            stream = splitter.process(stream)

            # Output: Stream processed Statements
            for stmt in stream:
                if self._grouping:
                    stmt = grouping.group(stmt)

                for filter_ in stmtprocess:
                    filter_.process(stmt)

                for filter_ in postprocess:
                    stmt = filter_.process(stmt)

                yield stmt
        except RecursionError as err:
            raise SQLParseError('Maximum recursion depth exceeded') from err
