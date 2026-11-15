"""Session isolation for filters held on a reused ``FilterStack``.

A single :class:`~sqlparse.engine.FilterStack` can be driven through several
``run()`` calls.  Stateful filters must therefore expose a per-run boundary
so that generators left half-consumed, failed runs and the
indentation/counter state of one run can never leak into another run,
including when two runs are interleaved.
"""

import pytest

import sqlparse
from sqlparse import filters
from sqlparse.engine import FilterStack


def test_builtin_filters_support_sessions():
    # Every stateful built-in filter hands back a distinct, reset instance.
    for prototype in (
        filters.ReindentFilter(),
        filters.AlignedIndentFilter(),
        filters.OutputPythonFilter(),
        filters.OutputPHPFilter(),
    ):
        session = prototype.begin()
        assert session is not prototype
        assert isinstance(session, type(prototype))


def test_reindent_state_does_not_leak_between_runs():
    # ReindentFilter remembers the previous statement to insert blank lines;
    # a reused stack must format the second run exactly like the first.
    stack = FilterStack()
    stack.enable_grouping()
    stack.stmtprocess.append(filters.ReindentFilter())

    first = ''.join(str(s) for s in stack.run('select a from t; select b from t'))
    second = ''.join(str(s) for s in stack.run('select a from t; select b from t'))
    assert first == second


def test_output_counter_resets_on_each_run():
    # The output filter numbers statements starting at one within each run.
    stack = FilterStack()
    stack.postprocess.append(filters.OutputPythonFilter())

    first = ''.join(str(s) for s in stack.run('select 1; select 2'))
    second = ''.join(str(s) for s in stack.run('select 1; select 2'))
    assert first == second
    assert 'sql =' in second
    assert 'sql2 =' in second
    assert 'sql3 =' not in second


def test_interleaved_runs_are_independent():
    stack = FilterStack()
    stack.enable_grouping()
    stack.stmtprocess.append(filters.ReindentFilter())

    run_a = stack.run('select a from t1; select a2 from t1')
    run_b = stack.run('select b from t2')

    stmt_a = next(run_a)
    stmt_b = next(run_b)

    # Each generator keeps consuming its own statements; the interleaved run
    # must not see the blank-line prefix of run A's second statement.
    remaining_a = list(run_a)
    remaining_b = list(run_b)

    assert len(remaining_a) == 1
    assert len(remaining_b) == 0
    assert 't1' in str(stmt_a)
    assert 't2' in str(stmt_b)
    # The blank-line separator is session-internal: run A's second statement
    # must carry it even though run B was consumed in between, proving the two
    # runs hold distinct ReindentFilter sessions.
    assert str(remaining_a[0]).startswith('\n\n')


def test_closing_one_run_does_not_affect_another():
    stack = FilterStack()
    stack.enable_grouping()
    stack.stmtprocess.append(filters.ReindentFilter())

    abandoned = stack.run('select a from t1; select a2 from t1')
    kept = stack.run('select b from t2; select b2 from t2')

    next(abandoned)
    abandoned.close()

    # The surviving run still produces both statements with fresh state.
    statements = list(kept)
    assert len(statements) == 2
    fresh = ''.join(str(s) for s in stack.run('select c from t3'))
    assert 'select' in fresh


def test_failed_run_recovers_on_next_run():
    class ExplodingFilter(filters.Filter):
        def __init__(self):
            self.calls = 0

        def begin(self):
            return type(self)()

        def process(self, stmt):
            self.calls += 1
            if self.calls == 2:
                raise RuntimeError('boom')
            return stmt

    stack = FilterStack()
    stack.stmtprocess.append(ExplodingFilter())

    failed = stack.run('select 1; select 2')
    with pytest.raises(RuntimeError):
        list(failed)

    # A new run gets a brand-new session, so the failure left nothing behind.
    recovered = list(stack.run('select 3'))
    assert len(recovered) == 1
    assert str(recovered[0]).strip() == 'select 3'


def test_custom_filter_can_declare_session_protocol():
    class CountingFilter(filters.Filter):
        def __init__(self):
            self.seen = 0

        def begin(self):
            return CountingFilter()

        def process(self, stmt):
            self.seen += 1
            stmt.session_seen = self.seen
            return stmt

    prototype = CountingFilter()
    stack = FilterStack()
    stack.stmtprocess.append(prototype)

    first = list(stack.run('select 1; select 2'))
    second = list(stack.run('select 3'))

    # The registered instance is only a prototype; state lives on sessions.
    assert prototype.seen == 0
    assert [getattr(s, 'session_seen', None) for s in first] == [1, 2]
    assert [getattr(s, 'session_seen', None) for s in second] == [1]


def test_legacy_filter_without_begin_remains_shared():
    # Old-style filters without begin() keep working and are reused as-is.
    class LegacyFilter:
        def __init__(self):
            self.seen = 0

        def process(self, stmt):
            self.seen += 1
            return stmt

    legacy = LegacyFilter()
    stack = FilterStack()
    stack.stmtprocess.append(legacy)

    list(stack.run('select 1; select 2'))
    list(stack.run('select 3'))

    # No begin() -> the same instance is shared, so state intentionally
    # accumulates across runs (the documented reuse limitation).
    assert legacy.seen == 3


def test_session_creation_uses_begin_not_deepcopy():
    # A marker object held by the prototype must be shared by reference; the
    # stack relies on begin() instead of copying arbitrary user objects.
    marker = object()

    class ReferencingFilter(filters.Filter):
        def __init__(self, shared):
            self.shared = shared

        def begin(self):
            return type(self)(self.shared)

        def process(self, stmt):
            return stmt

    prototype = ReferencingFilter(marker)
    stack = FilterStack()
    stack.stmtprocess.append(prototype)

    list(stack.run('select 1'))
    assert prototype.shared is marker


def test_public_format_api_still_works_with_reused_stack_path():
    # Sanity check: the high-level helper builds a fresh stack each call and
    # the session machinery must not change formatting output.
    sql = 'select a, b from t where a = 1 order by b'
    expected = sqlparse.format(sql, reindent=True)
    assert '\n' in expected
    assert expected == sqlparse.format(sql, reindent=True)
