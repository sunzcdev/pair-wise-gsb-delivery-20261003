"""Tests for per-run filter sessions in a reused FilterStack."""

import threading

import pytest

from sqlparse import filters
from sqlparse.engine import FilterStack


def _stack(*stmt_filters, postprocess=None):
    stack = FilterStack()
    stack.enable_grouping()
    stack.stmtprocess.extend(stmt_filters)
    stack.postprocess.extend(postprocess or [])
    return stack


def _run(stack, sql):
    return ''.join(str(stmt) for stmt in stack.run(sql))


def _php_stack():
    return _stack(filters.ReindentFilter(),
                  postprocess=[filters.OutputPHPFilter()])


class _RaisesOnStatement:
    """Raises while processing the n-th statement of a run."""

    def __init__(self, n):
        self.n = n
        self.seen = 0

    def process(self, stmt):
        self.seen += 1
        if self.seen == self.n:
            raise RuntimeError('boom')
        return stmt


class _CountingSession:
    """Custom filter that declares a session for each run."""

    sessions = 0

    def new_session(self):
        type(self).sessions += 1
        return type(self)()

    def process(self, stmt):
        return stmt


class TestSessionIsolation:
    def test_second_run_matches_first(self):
        stack = _php_stack()
        sql = 'select a from foo; select b from bar'
        first = _run(stack, sql)
        assert _run(stack, sql) == first

    def test_reused_stack_matches_fresh_stack(self):
        sql = 'select a from foo; select b from bar'
        stack = _php_stack()
        _run(stack, 'select x from y')  # consume a run first
        assert _run(stack, sql) == _run(_php_stack(), sql)

    def test_output_filter_restarts_variable_names(self):
        stack = _stack(postprocess=[filters.OutputPythonFilter()])
        assert _run(stack, 'select 1; select 2') == (
            "sql = 'select 1; '\nsql2 = 'select 2'")
        # The counter must start at 1 again, not continue with sql3.
        assert _run(stack, 'select 3; select 4') == (
            "sql = 'select 3; '\nsql2 = 'select 4'")

    def test_abandoned_run_does_not_affect_next(self):
        stack = _php_stack()
        sql = 'select a from foo; select b from bar'
        expected = _run(_php_stack(), sql)

        abandoned = stack.run(sql)
        next(abandoned)  # consume one statement, then walk away
        abandoned.close()

        assert _run(stack, sql) == expected

    def test_unclosed_generator_does_not_affect_next(self):
        stack = _php_stack()
        sql = 'select a from foo; select b from bar'
        expected = _run(_php_stack(), sql)

        partial = stack.run(sql)
        next(partial)  # left open on purpose

        assert _run(stack, sql) == expected
        partial.close()

    def test_interleaved_runs_are_independent(self):
        stack = _php_stack()
        sql_a = 'select a from foo; select b from bar'
        sql_b = 'select c from baz; select d from qux'
        expected_a = _run(_php_stack(), sql_a)
        expected_b = _run(_php_stack(), sql_b)

        gen_a = stack.run(sql_a)
        gen_b = stack.run(sql_b)
        parts_a, parts_b = [], []
        for _ in range(2):
            parts_a.append(str(next(gen_a)))
            parts_b.append(str(next(gen_b)))
        with pytest.raises(StopIteration):
            next(gen_a)
        with pytest.raises(StopIteration):
            next(gen_b)

        assert ''.join(parts_a) == expected_a
        assert ''.join(parts_b) == expected_b

    def test_closing_one_run_keeps_the_other_alive(self):
        stack = _php_stack()
        sql_a = 'select a from foo; select b from bar'
        sql_b = 'select c from baz; select d from qux'
        expected_b = _run(_php_stack(), sql_b)

        gen_a = stack.run(sql_a)
        gen_b = stack.run(sql_b)
        next(gen_a)
        gen_a.close()

        assert ''.join(str(stmt) for stmt in gen_b) == expected_b

    def test_interleaved_reindent_keeps_statement_separation(self):
        stack = _stack(filters.ReindentFilter())
        expected_a = _run(_stack(filters.ReindentFilter()),
                          'select a from foo; select b from bar')
        expected_b = _run(_stack(filters.ReindentFilter()),
                          'select c from baz; select d from qux')

        gen_a = stack.run('select a from foo; select b from bar')
        gen_b = stack.run('select c from baz; select d from qux')
        parts_a = [str(next(gen_a)), str(next(gen_a))]
        parts_b = [str(next(gen_b)), str(next(gen_b))]

        # '' would mean a blank line leaked in from the other run.
        assert ''.join(parts_a) == expected_a
        assert ''.join(parts_b) == expected_b

    def test_runs_used_from_several_threads(self):
        stack = _php_stack()
        sql = 'select a from foo; select b from bar'
        expected = _run(_php_stack(), sql)
        results, errors = [], []

        def consume():
            try:
                for _ in range(5):
                    results.append(_run(stack, sql))
            except Exception as exc:  # pragma: no cover - assertion helper
                errors.append(exc)

        threads = [threading.Thread(target=consume) for _ in range(4)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()

        assert errors == []
        assert results == [expected] * 20

    def test_failed_run_does_not_pollute_next_run(self):
        stack = _stack(filters.ReindentFilter(),
                       _RaisesOnStatement(2),
                       postprocess=[filters.OutputPHPFilter()])
        expected = _run(_php_stack(), 'select c from baz')

        with pytest.raises(RuntimeError):
            _run(stack, 'select a from foo; select b from bar')

        assert _run(stack, 'select c from baz') == expected

    def test_failed_run_leaves_reindent_prototype_untouched(self):
        reindent = filters.ReindentFilter()
        stack = _stack(reindent, _RaisesOnStatement(1))

        with pytest.raises(RuntimeError):
            _run(stack, 'select a from foo')

        # State lives in the session, not on the filter in the stack.
        assert reindent._last_stmt is None
        assert reindent._curr_stmt is None
        assert reindent.indent == 0
        assert reindent.offset == 0

    def test_reindent_recovers_after_failure(self):
        stack = _stack(filters.ReindentFilter(), _RaisesOnStatement(2))
        with pytest.raises(RuntimeError):
            _run(stack, 'select a from foo; select b from bar')

        # A stale _last_stmt would prepend a blank line here.
        assert _run(stack, 'select c from baz') == 'select c\nfrom baz'

    def test_session_created_once_per_run(self):
        _CountingSession.sessions = 0
        stack = _stack(_CountingSession(), _CountingSession())
        _run(stack, 'select 1; select 2; select 3')
        assert _CountingSession.sessions == 2  # one per filter instance

        _run(stack, 'select 4')
        assert _CountingSession.sessions == 4

    def test_session_created_for_empty_input(self):
        _CountingSession.sessions = 0
        assert _run(_stack(_CountingSession()), '') == ''
        assert _CountingSession.sessions == 1

    def test_one_session_per_filter_instance(self):
        # Registering the same instance twice keeps its single session,
        # a different instance gets its own.
        _CountingSession.sessions = 0
        shared = _CountingSession()
        _run(_stack(shared, shared, _CountingSession()), 'select 1')
        assert _CountingSession.sessions == 2


class TestSessionDeclaration:
    def test_no_deep_copy_of_filter_attributes(self):
        # A lock can be neither deep-copied nor shared for state; a shallow
        # copy is enough because the session resets its own state.
        filter_ = filters.ReindentFilter()
        filter_.lock = threading.Lock()
        stack = _stack(filter_)
        assert _run(stack, 'select a from foo') == 'select a\nfrom foo'

    def test_new_session_returning_none_is_rejected(self):
        class Broken:
            @staticmethod
            def new_session():
                return None

            def process(self, stmt):
                return stmt

        with pytest.raises(TypeError, match='returned None'):
            _run(_stack(Broken()), 'select 1')

    def test_non_callable_new_session_is_rejected(self):
        class Broken:
            new_session = 'nope'

            def process(self, stmt):
                return stmt

        with pytest.raises(TypeError, match='must be callable'):
            _run(_stack(Broken()), 'select 1')

    def test_explicit_none_declaration_opts_out(self):
        class Shared:
            new_session = None

            def __init__(self):
                self.seen = 0

            def process(self, stmt):
                self.seen += 1
                return stmt

        filter_ = Shared()
        stack = _stack(filter_)
        _run(stack, 'select 1; select 2')
        _run(stack, 'select 3')
        # No session: the stack instance is used directly.
        assert filter_.seen == 3


class TestLegacyFilters:
    def test_filter_without_new_session_still_runs(self):
        class Legacy:
            def __init__(self):
                self.seen = 0

            def process(self, stmt):
                self.seen += 1
                return stmt

        filter_ = Legacy()
        stack = _stack(filter_)
        assert _run(stack, 'select 1; select 2') == 'select 1; select 2'
        assert filter_.seen == 2

    def test_legacy_filter_state_is_shared_between_runs(self):
        # Documented reuse limitation: a filter that keeps per-run state
        # but declares no session shares that state across runs.
        class LegacyCounter:
            def __init__(self):
                self.seen = 0

            def process(self, stmt):
                self.seen += 1
                return stmt

        filter_ = LegacyCounter()
        stack = _stack(filter_)
        _run(stack, 'select 1')
        _run(stack, 'select 2')
        assert filter_.seen == 2

    def test_legacy_filter_keeps_state_after_failure(self):
        # The other side of the documented limitation: without a session
        # a legacy filter's state survives a failed or abandoned run.
        class LegacyCounter:
            def __init__(self):
                self.seen = 0

            def process(self, stmt):
                self.seen += 1
                return stmt

        filter_ = LegacyCounter()
        stack = _stack(filter_, _RaisesOnStatement(2))
        with pytest.raises(RuntimeError):
            _run(stack, 'select 1; select 2')
        assert filter_.seen == 2  # both statements reached the filter

        _run(stack, 'select 3')
        assert filter_.seen == 3  # continues from the failed run

    def test_legacy_generator_filter_is_still_closed(self):
        closed = []

        class LegacyStream:
            def process(self, stream):
                try:
                    yield from stream
                finally:
                    closed.append(True)

        stack = FilterStack()
        stack.preprocess.append(LegacyStream())
        gen = stack.run('select 1')
        next(gen)
        gen.close()
        assert closed == [True]
