# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

import contextlib
import threading

import pytest

import structlog

from structlog.exceptions import DropEvent
from structlog.processors import (
    DuplicateEventSuppressor,
    SuppressionSummary,
)


class FakeClock:
    def __init__(self, start=0.0):
        self.now = start

    def __call__(self):
        return self.now

    def advance(self, seconds):
        self.now += seconds


@pytest.fixture(name="clock")
def _clock():
    return FakeClock()


class TestDuplicateEventSuppressor:
    def test_first_event_passes_duplicates_dropped(self, clock):
        """
        The first event of a signature passes, further events in the window
        are dropped.
        """
        sup = DuplicateEventSuppressor(window=10.0, clock=clock)

        assert sup(None, "warning", {"event": "boom"}) == {"event": "boom"}

        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom"})

    def test_different_signatures_pass_independently(self, clock):
        """
        Events with different signatures are tracked independently.
        """
        sup = DuplicateEventSuppressor(
            window=10.0, signature_keys=("event", "target"), clock=clock
        )

        assert sup(None, "warning", {"event": "boom", "target": "a"})
        assert sup(None, "warning", {"event": "boom", "target": "b"})

        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom", "target": "a"})

    def test_max_passed_allows_a_burst(self, clock):
        """
        Up to max_passed events per window are let through.
        """
        sup = DuplicateEventSuppressor(window=10.0, max_passed=3, clock=clock)

        for _ in range(3):
            assert sup(None, "warning", {"event": "boom"}) == {"event": "boom"}

        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom"})

    def test_window_expiry_reopens_and_settles_summary(self, clock):
        """
        After the window passes, events flow again and the old window is
        settled with suppressed count and first/last timestamps.
        """
        sup = DuplicateEventSuppressor(window=10.0, clock=clock)

        sup(None, "warning", {"event": "boom", "level": "warning"})
        clock.advance(1.0)
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom", "level": "warning"})
        clock.advance(1.0)
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom", "level": "warning"})

        clock.advance(8.0)  # now == 10.0, window expired
        assert sup(None, "warning", {"event": "boom", "level": "warning"})

        [summary] = sup.flush()
        assert summary == SuppressionSummary(
            signature=("boom",),
            level="warning",
            suppressed_count=2,
            first_seen=0.0,
            last_seen=2.0,
        )

    def test_window_boundary_is_exclusive(self, clock):
        """
        Events exactly window seconds apart belong to different windows.
        """
        sup = DuplicateEventSuppressor(window=5.0, clock=clock)

        sup(None, "warning", {"event": "boom"})
        clock.advance(4.9)
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom"})

        clock.advance(0.1)
        assert sup(None, "warning", {"event": "boom"})

    def test_higher_level_escalation_not_swallowed(self, clock):
        """
        An ERROR event is not swallowed by a WARNING window and settles it.
        """
        sup = DuplicateEventSuppressor(window=100.0, clock=clock)

        sup(None, "warning", {"event": "boom", "level": "warning"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom", "level": "warning"})

        clock.advance(5.0)
        assert sup(None, "error", {"event": "boom", "level": "error"}) == {
            "event": "boom",
            "level": "error",
        }

        [summary] = sup.flush()
        assert summary.suppressed_count == 1
        assert summary.level == "warning"
        assert summary.last_seen == 0.0

    def test_numeric_levels_escalate(self, clock):
        """
        Numeric level values are compared just like level names.
        """
        sup = DuplicateEventSuppressor(window=100.0, clock=clock)

        sup(None, "warning", {"event": "boom", "level": 30})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom", "level": 30})

        assert sup(None, "error", {"event": "boom", "level": 40})

        [summary] = sup.flush()
        assert summary.suppressed_count == 1

    def test_lower_level_does_not_reopen_window(self, clock):
        """
        A lower-severity duplicate stays suppressed in the existing window.
        """
        sup = DuplicateEventSuppressor(window=100.0, clock=clock)

        sup(None, "error", {"event": "boom", "level": "error"})

        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "boom", "level": "warning"})

        # The window keeps the level of its first (error) event and reports
        # the suppressed warning as a regular duplicate.
        [summary] = sup.flush()
        assert summary.level == "error"
        assert summary.suppressed_count == 1

    def test_unknown_level_does_not_escalate(self, clock):
        """
        Missing or unrecognized levels never trigger an escalation.
        """
        sup = DuplicateEventSuppressor(window=100.0, clock=clock)

        sup(None, "warning", {"event": "boom"})
        with pytest.raises(DropEvent):
            sup(None, "error", {"event": "boom", "level": "error"})

        sup2 = DuplicateEventSuppressor(window=100.0, clock=clock)
        sup2(None, "warning", {"event": "boom", "level": "mystery"})
        with pytest.raises(DropEvent):
            sup2(None, "error", {"event": "boom", "level": "error"})

        # Booleans are ints but must never be treated as a level value.
        sup3 = DuplicateEventSuppressor(window=100.0, clock=clock)
        sup3(None, "warning", {"event": "boom", "level": True})
        with pytest.raises(DropEvent):
            sup3(None, "error", {"event": "boom", "level": "error"})

    def test_eviction_settles_lru_entry(self, clock):
        """
        When the cache is full the least recently used window is evicted
        and settled.
        """
        sup = DuplicateEventSuppressor(
            window=100.0, max_signatures=2, clock=clock
        )

        sup(None, "warning", {"event": "a"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a"})

        sup(None, "warning", {"event": "b"})
        # Touch "a" so that "b" becomes the least recently used entry.
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a"})

        sup(None, "warning", {"event": "c"})  # evicts "b"

        # "b" never suppressed anything, so its eviction produces no summary.
        # "a" is still open and gets settled by flush().
        [summary] = sup.flush()
        assert summary.signature == ("a",)
        assert summary.suppressed_count == 2
        assert sup.flush() == []  # everything has been drained

    def test_evicted_summary_with_suppressions(self, clock):
        """
        Evicted windows with suppressed events produce a retrievable summary.
        """
        sup = DuplicateEventSuppressor(
            window=100.0, max_signatures=1, clock=clock
        )

        sup(None, "warning", {"event": "a"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a"})

        sup(None, "warning", {"event": "b"})  # evicts "a"

        [summary] = sup.flush()
        assert summary.signature == ("a",)
        assert summary.suppressed_count == 1
        assert summary.first_seen == 0.0
        assert summary.last_seen == 0.0

    def test_flush_drains_all_open_windows(self, clock):
        """
        flush() settles every open window and empties the processor state.
        """
        sup = DuplicateEventSuppressor(window=100.0, clock=clock)

        sup(None, "warning", {"event": "a", "level": "warning"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a", "level": "warning"})
        sup(None, "error", {"event": "b", "level": "error"})

        summaries = sup.flush()
        assert len(summaries) == 1
        assert summaries[0].signature == ("a",)
        assert summaries[0].suppressed_count == 1

        # A second flush returns nothing and suppression starts fresh.
        assert sup.flush() == []
        assert sup(None, "warning", {"event": "a"}) == {"event": "a"}

    def test_flush_includes_earlier_expired_summaries(self, clock):
        """
        Summaries produced by window expiry are kept until flush().
        """
        sup = DuplicateEventSuppressor(window=5.0, clock=clock)

        sup(None, "warning", {"event": "a"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a"})

        clock.advance(5.0)
        sup(None, "warning", {"event": "a"})

        summaries = sup.flush()
        assert len(summaries) == 1
        assert summaries[0].suppressed_count == 1

    def test_no_summary_without_suppression(self, clock):
        """
        Windows that never suppressed an event do not produce summaries.
        """
        sup = DuplicateEventSuppressor(window=10.0, clock=clock)

        sup(None, "warning", {"event": "a"})
        clock.advance(10.0)
        sup(None, "warning", {"event": "a"})

        assert sup.flush() == []

    def test_on_settle_called_immediately(self, clock):
        """
        The sink receives summaries at settlement time, not only at flush.
        """
        received = []
        sup = DuplicateEventSuppressor(
            window=5.0, clock=clock, on_settle=received.append
        )

        sup(None, "warning", {"event": "a"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a"})

        assert received == []
        clock.advance(5.0)
        sup(None, "warning", {"event": "a"})

        assert len(received) == 1
        assert received[0].suppressed_count == 1
        # The summary is still retrievable via flush().
        assert sup.flush() == received

    def test_on_settle_recursion_is_guarded(self, clock):
        """
        Logging from within the sink cannot re-enter the same suppressor.
        """
        sup = DuplicateEventSuppressor(window=5.0, clock=clock)

        def sink(summary):
            # A re-entrant event must behave like a fresh event instead of
            # recursing into settlement (which would loop forever here).
            sup(None, "warning", {"event": "a"})

        sup._on_settle = sink

        sup(None, "warning", {"event": "a"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a"})

        clock.advance(5.0)
        sup(None, "warning", {"event": "a"})  # settles, sink logs again

        summaries = sup.flush()
        assert summaries[0].suppressed_count == 1

    def test_flush_inside_sink_does_not_double_settle(self, clock):
        """
        A flush() call from within on_settle must not observe the summary
        that is currently being delivered to the sink.
        """
        drained = []

        def sink(summary):
            # The in-flight summary is queued only after the sink returns,
            # so flush() drains earlier summaries but never the current one.
            drained.extend(sup.flush())

        sup = DuplicateEventSuppressor(window=5.0, clock=clock, on_settle=sink)

        # The "old" summary is queued after its sink call returns.
        sup(None, "warning", {"event": "old"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "old"})
        clock.advance(5.0)
        sup(None, "warning", {"event": "old"})

        # Settling "new" makes its sink flush out the queued "old" summary.
        sup(None, "warning", {"event": "new"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "new"})

        summaries = sup.flush()
        assert [s.signature for s in summaries] == [("new",)]
        assert [s.signature for s in drained] == [("old",)]

    def test_missing_signature_key_uses_sentinel(self, clock):
        """
        Missing stable keys still produce a consistent, distinct signature.
        """
        sup = DuplicateEventSuppressor(
            window=10.0, signature_keys=("event", "request_id"), clock=clock
        )

        assert sup(None, "warning", {"event": "a"})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a"})
        assert sup(None, "warning", {"event": "a", "request_id": 1})

    def test_unhashable_signature_value_falls_back_to_repr(self, clock):
        """
        Unhashable values (e.g. lists) are grouped via their repr().
        """
        sup = DuplicateEventSuppressor(
            window=10.0, signature_keys=("event", "payload"), clock=clock
        )

        assert sup(None, "warning", {"event": "a", "payload": [1, 2]})
        with pytest.raises(DropEvent):
            sup(None, "warning", {"event": "a", "payload": [1, 2]})
        assert sup(None, "warning", {"event": "a", "payload": [3]})

    def test_invalid_constructor_arguments(self):
        with pytest.raises(ValueError, match="window"):
            DuplicateEventSuppressor(window=0)
        with pytest.raises(ValueError, match="max_passed"):
            DuplicateEventSuppressor(max_passed=0)
        with pytest.raises(ValueError, match="max_signatures"):
            DuplicateEventSuppressor(max_signatures=0)
        with pytest.raises(ValueError, match="signature_keys"):
            DuplicateEventSuppressor(signature_keys=())

    def test_concurrent_calls_are_thread_safe(self):
        """
        Concurrent logging never corrupts the internal bookkeeping.
        """
        sup = DuplicateEventSuppressor(window=100.0)
        errors = []

        def worker():
            try:
                for _ in range(200):
                    with contextlib.suppress(DropEvent):
                        sup(None, "warning", {"event": "boom"})
            except Exception as exc:  # pragma: no cover - failure path
                errors.append(exc)

        threads = [threading.Thread(target=worker) for _ in range(8)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()

        assert errors == []
        [summary] = sup.flush()
        assert summary.suppressed_count == 8 * 200 - 1

    def test_integration_with_bound_logger(self, clock):
        """
        The processor works end-to-end inside a structlog processor chain and
        flush() reports the suppressed events at shutdown.
        """
        suppressor = DuplicateEventSuppressor(window=10.0, clock=clock)

        with structlog.testing.capture_logs(
            [structlog.processors.add_log_level, suppressor]
        ) as entries:
            log = structlog.get_logger()
            log.warning("disk full")
            log.warning("disk full")
            log.warning("disk full")
            log.error("disk full")  # escalation: passes through

        assert [entry["event"] for entry in entries] == [
            "disk full",
            "disk full",
        ]

        [summary] = suppressor.flush()
        assert summary.signature == ("disk full",)
        assert summary.level == "warning"
        assert summary.suppressed_count == 2
