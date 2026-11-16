#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""filter"""

from types import GeneratorType

from sqlparse import lexer
from sqlparse.engine import grouping
from sqlparse.engine.statement_splitter import StatementSplitter
from sqlparse.exceptions import SQLParseError
from sqlparse.filters import StripTrailingSemicolonFilter


class _RunSessions:
    """Filter instances owned by a single :meth:`FilterStack.run`.

    A ``FilterStack`` is reusable: ``run()`` may be called more than once,
    two runs may be consumed interleaved, and a run may be abandoned or
    fail halfway through.  A stateful filter therefore must not keep its
    state on the object that was added to the stack, because that object
    is shared by every run.  Filters opt in by implementing
    ``new_session()``, which returns the object that processes one run.
    The stack calls it once when the run starts and keeps the result in
    that run's frame.  Runs consequently never share state, and closing
    or failing one run leaves the others untouched.

    Filters without ``new_session()`` are used as they are.  That keeps
    existing custom filters working, but their state is shared between
    runs and stays behind when a run is abandoned or fails.
    """

    def __init__(self):
        self._sessions = {}
        self._streams = []

    def session(self, filter_):
        """Return the instance that handles *filter_* in this run."""
        key = id(filter_)
        try:
            return self._sessions[key][1]
        except KeyError:
            pass

        session = self._new_session(filter_)
        # Keep a reference to the prototype so its id() stays unique while
        # the run is alive.
        self._sessions[key] = filter_, session
        return session

    @staticmethod
    def _new_session(filter_):
        """Call the filter's ``new_session`` hook, if it declares one."""
        new_session = getattr(filter_, 'new_session', None)
        if new_session is None:
            # No declaration (or an explicit opt-out): the filter is
            # stateless or handles sharing itself, use it directly.
            return filter_
        if not callable(new_session):
            raise TypeError(
                f'{type(filter_).__name__}.new_session must be callable or '
                f'None, got {new_session!r}')

        session = new_session()
        if session is None:
            raise TypeError(
                f'{type(filter_).__name__}.new_session() returned None, '
                'expected a filter instance for this run')
        return session

    def track(self, stream):
        """Remember a token stream created by a filter in this run.

        Only generator objects are closed afterwards: a generator is
        one-shot, so the object returned by ``filter.process(...)`` was
        created for this run and can't be used by another one.  Other
        iterables are left alone because a filter might return a shared
        object that closing would take away from its other runs.
        """
        if isinstance(stream, GeneratorType):
            self._streams.append(stream)
        return stream

    def close(self):
        """Release everything that belongs to this run.

        Only streams created by this run are closed.  Streams of other
        runs of the same stack are owned by those runs and stay open.
        """
        streams, self._streams = self._streams, []
        self._sessions.clear()
        # Close the consumers before the streams they consume.
        for stream in reversed(streams):
            try:
                stream.close()
            except (RuntimeError, ValueError):
                # A generator that ignores GeneratorExit raises RuntimeError
                # and an already running one raises ValueError.  Neither may
                # turn the teardown of a finished run into a crash.
                pass


class FilterStack:
    """A sequence of filters applied to a token stream.

    Instances are reusable: :meth:`run` can be called repeatedly, and its
    generators can be consumed interleaved or abandoned.  Each call to
    :meth:`run` is a session; a stateful filter isolates its per-run state
    by declaring a ``new_session()`` method.
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
        # Every run gets its own sessions, so interleaved and abandoned
        # runs cannot see each other's state.  They are created up front
        # so the hooks also run for input that yields no statement.
        sessions = _RunSessions()
        try:
            for filter_ in (self.preprocess + self.stmtprocess
                            + self.postprocess):
                sessions.session(filter_)

            stream = sessions.track(lexer.tokenize(sql, encoding))
            # Process token stream
            for filter_ in self.preprocess:
                stream = sessions.track(
                    sessions.session(filter_).process(stream))

            stream = sessions.track(StatementSplitter().process(stream))

            # Output: Stream processed Statements
            for stmt in stream:
                if self._grouping:
                    stmt = grouping.group(stmt)

                for filter_ in self.stmtprocess:
                    sessions.session(filter_).process(stmt)

                for filter_ in self.postprocess:
                    stmt = sessions.session(filter_).process(stmt)

                yield stmt
        except RecursionError as err:
            raise SQLParseError('Maximum recursion depth exceeded') from err
        finally:
            sessions.close()
