# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

import asyncio
import contextvars
import logging
import threading

import pytest

import structlog

from structlog.contextvars import (
    _SEQUENCE_CONTEXT,
    add_sequence_number,
    bind_contextvars,
    bind_sequence_context,
    clear_contextvars,
    fork_sequence_context,
    get_contextvars,
    get_sequence_context,
    merge_contextvars,
    reset_sequence_context,
    sequence_context,
)
from structlog.testing import capture_logs


@pytest.fixture(autouse=True)
def _reset_sequence_context():
    """
    Make sure no sequence context or structlog contextvars leak between
    tests (in either direction).
    """
    clear_contextvars()
    yield
    clear_contextvars()
    _SEQUENCE_CONTEXT.set(None)


class TestSequenceContext:
    def test_no_active_sequence_is_noop(self):
        """
        Without an active sequence context, add_sequence_number passes the
        event dict through unchanged.
        """
        assert {"event": "x"} == add_sequence_number(
            None, None, {"event": "x"}
        )

    def test_sequential_numbers_and_name(self):
        """
        Within a sequence context, committed events get monotonically
        increasing numbers starting at 1, plus the sequence name as seq_id.
        """
        with sequence_context("req"):
            ed1 = add_sequence_number(None, None, {"event": "a"})
            ed2 = add_sequence_number(None, None, {"event": "b"})
            ed3 = add_sequence_number(None, None, {"event": "c"})

        assert 1 == ed1["seq"]
        assert 2 == ed2["seq"]
        assert 3 == ed3["seq"]
        assert "req" == ed1["seq_id"] == ed2["seq_id"] == ed3["seq_id"]

    def test_unnamed_sequence_has_no_seq_id(self):
        """
        An unnamed sequence only adds seq, no seq_id.
        """
        with sequence_context():
            ed = add_sequence_number(None, None, {})

        assert {"seq": 1} == ed

    def test_exit_stops_numbering_next_request(self):
        """
        After the sequence context is left, events are not numbered anymore,
        so one request's sequence never leaks numbers into the next request.
        A new context starts a fresh sequence at 1.
        """
        with sequence_context("req-1"):
            assert 1 == add_sequence_number(None, None, {})["seq"]

        assert get_sequence_context() is None
        assert {} == add_sequence_number(None, None, {})

        with sequence_context("req-2"):
            assert 1 == add_sequence_number(None, None, {})["seq"]

    def test_nested_sequence_contexts(self):
        """
        Nested sequence contexts restore the outer sequence on exit.
        """
        with sequence_context("outer"):
            assert 1 == add_sequence_number(None, None, {})["seq"]

            with sequence_context("inner"):
                ed = add_sequence_number(None, None, {})
                assert 1 == ed["seq"]
                assert "inner" == ed["seq_id"]

            ed = add_sequence_number(None, None, {})
            assert 2 == ed["seq"]
            assert "outer" == ed["seq_id"]

    def test_sequence_not_leaked_into_contextvars(self):
        """
        The sequence context is not part of the regular structlog context:
        neither get_contextvars nor merge_contextvars pick it up.
        """
        bind_contextvars(a=1)
        try:
            with sequence_context("req"):
                assert {"a": 1} == get_contextvars()
                assert {"a": 1} == merge_contextvars(None, None, {})
        finally:
            clear_contextvars()

    def test_bind_and_reset(self):
        """
        bind_sequence_context shares an existing sequence with the current
        context; reset_sequence_context removes it again.
        """
        with sequence_context("req") as sequence:
            pass

        assert get_sequence_context() is None

        token = bind_sequence_context(sequence)
        assert get_sequence_context() is sequence
        assert 1 == add_sequence_number(None, None, {})["seq"]

        reset_sequence_context(token)
        assert get_sequence_context() is None
        assert {} == add_sequence_number(None, None, {})

    def test_copied_context_shares_counter(self):
        """
        Plainly copying a context (like asyncio.Task creation does) must not
        produce a second, same-named sequence starting from zero: all copies
        draw from the same counter.
        """
        with sequence_context("req"):
            ctx = contextvars.copy_context()

            ed1 = ctx.run(lambda: add_sequence_number(None, None, {}))
            ed2 = ctx.run(lambda: add_sequence_number(None, None, {}))
            ed3 = add_sequence_number(None, None, {})

        assert (1, 2, 3) == (ed1["seq"], ed2["seq"], ed3["seq"])
        assert "req" == ed1["seq_id"] == ed2["seq_id"] == ed3["seq_id"]

    async def test_async_subtasks_share_sequence(self):
        """
        Async subtasks share the parent's sequence: numbers reflect the
        commit order across tasks, not per-task counters.
        """
        sub1_committed = asyncio.Event()
        sub2_committed = asyncio.Event()

        async def sub1():
            ed1 = add_sequence_number(None, None, {"task": 1})
            sub1_committed.set()
            await sub2_committed.wait()
            ed2 = add_sequence_number(None, None, {"task": 1})
            return [ed1, ed2]

        async def sub2():
            await sub1_committed.wait()
            ed = add_sequence_number(None, None, {"task": 2})
            sub2_committed.set()
            return [ed]

        with sequence_context("req"):
            (r1, r2) = await asyncio.gather(
                asyncio.create_task(sub1()), asyncio.create_task(sub2())
            )

        # Commit order was: sub1, sub2, sub1 -- regardless of scheduling.
        assert [1, 3] == [ed["seq"] for ed in r1]
        assert [2] == [ed["seq"] for ed in r2]

    def test_thread_delayed_write_keeps_commit_number(self):
        """
        A thread that commits early but writes out late keeps the number it
        drew at commit time: sorting the written records by seq recovers the
        commit order, not the write order.
        """
        a_committed = threading.Event()
        b_written = threading.Event()
        written = []

        def worker_a(sequence):
            token = bind_sequence_context(sequence)
            try:
                ed = add_sequence_number(None, None, {"event": "a"})
                a_committed.set()
                # Delay the write until b has committed *and* written.
                assert b_written.wait(5)
                written.append(ed)
            finally:
                reset_sequence_context(token)

        def worker_b(sequence):
            token = bind_sequence_context(sequence)
            try:
                assert a_committed.wait(5)
                ed = add_sequence_number(None, None, {"event": "b"})
                written.append(ed)
                b_written.set()
            finally:
                reset_sequence_context(token)

        with sequence_context("req") as sequence:
            ta = threading.Thread(target=worker_a, args=(sequence,))
            tb = threading.Thread(target=worker_b, args=(sequence,))
            ta.start()
            tb.start()
            ta.join(5)
            tb.join(5)

        # Write order is b, a ...
        assert ["b", "a"] == [ed["event"] for ed in written]
        # ... but the commit order a, b is recoverable via seq.
        assert ["a", "b"] == [
            ed["event"] for ed in sorted(written, key=lambda ed: ed["seq"])
        ]
        assert [1, 2] == sorted(ed["seq"] for ed in written)

    def test_threads_never_draw_duplicate_numbers(self):
        """
        Many threads sharing one sequence draw unique numbers: the counter
        is thread-safe.
        """
        with sequence_context("req") as sequence:
            drawn = []
            lock = threading.Lock()

            def worker():
                token = bind_sequence_context(sequence)
                try:
                    for _ in range(100):
                        n = add_sequence_number(None, None, {})["seq"]
                        with lock:
                            drawn.append(n)
                finally:
                    reset_sequence_context(token)

            threads = [threading.Thread(target=worker) for _ in range(10)]
            for t in threads:
                t.start()
            for t in threads:
                t.join(30)

        assert sorted(drawn) == list(range(1, 1001))


class TestForkSequenceContext:
    def test_fork_has_independent_counter_and_derived_name(self):
        """
        An explicit fork starts its own counter at 1 and derives its name
        from the parent, so it never collides with the parent's name. The
        parent's counter is unaffected.
        """
        with sequence_context("req") as parent:
            forked = fork_sequence_context()

            assert get_sequence_context() is forked
            assert "req/1" == forked.name

            ed = add_sequence_number(None, None, {})
            assert 1 == ed["seq"]
            assert "req/1" == ed["seq_id"]

            # The fork did not consume any of the parent's numbers.
            assert 1 == parent.next()

    def test_second_fork_gets_unique_name(self):
        """
        Forking forks the *currently active* sequence, so a fork of a fork
        gets a nested derived name. Sibling forks of the same parent get
        distinct names too. No two sequences ever share a name.
        """
        with sequence_context("req") as parent:
            fork1 = fork_sequence_context()
            fork1_1 = fork_sequence_context()

            assert "req/1" == fork1.name
            assert "req/1/1" == fork1_1.name

            # Sibling fork: re-bind the parent first.
            bind_sequence_context(parent)
            fork2 = fork_sequence_context()
            assert "req/2" == fork2.name

    def test_fork_with_explicit_name(self):
        """
        A fork can be given an explicit name.
        """
        with sequence_context("req"):
            forked = fork_sequence_context(name="worker")

        assert "worker" == forked.name

    def test_fork_requires_active_sequence(self):
        """
        Forking without an active sequence context is an error: sequences
        only come into existence explicitly.
        """
        with pytest.raises(RuntimeError, match="no sequence context"):
            fork_sequence_context()


class TestSequenceFilteringRule:
    def test_level_filtered_events_do_not_consume_numbers(self):
        """
        The fixed rule: events dropped by log-level filtering never reach
        the processors, so they do not consume sequence numbers.
        """
        structlog.configure(
            wrapper_class=structlog.make_filtering_bound_logger(logging.INFO)
        )

        with (
            sequence_context("req"),
            capture_logs(processors=[add_sequence_number]) as logs,
        ):
            log = structlog.get_logger()
            log.debug("filtered out")
            log.info("first")
            log.debug("filtered out")
            log.info("second")

        assert [1, 2] == [ed["seq"] for ed in logs]
        assert ["first", "second"] == [ed["event"] for ed in logs]
