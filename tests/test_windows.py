"""
test_windows
~~~~~~~~~~~~

Unit tests for the low level :class:`h2.windows.WindowManager`, focusing on
the bookkeeping that keeps the inbound flow control accounting honest when a
stream is abandoned.
"""
from __future__ import annotations

from h2.windows import LARGEST_FLOW_CONTROL_WINDOW, WindowManager


class TestWindowManagerSettlement:
    def test_consumed_and_acknowledged_counters(self) -> None:
        """
        ``window_consumed`` tracks every flow controlled byte (including
        padding, since the caller passes ``flow_controlled_length``) while
        ``process_bytes`` tracks the acknowledged subset.
        """
        manager = WindowManager(max_window_size=65535)

        manager.window_consumed(100)
        manager.window_consumed(37)

        assert manager.bytes_consumed == 137
        assert manager.bytes_acknowledged == 0
        assert manager.unacknowledged_bytes == 137
        assert manager.current_window_size == 65535 - 137

        # Small acknowledgements below the emission thresholds do not open the
        # window (the call returns a falsy, zero increment) but are still
        # recorded as acknowledged.
        assert not manager.process_bytes(50)

        assert manager.bytes_consumed == 137
        assert manager.bytes_acknowledged == 50
        assert manager.unacknowledged_bytes == 87

    def test_settle_returns_only_unacknowledged_balance(self) -> None:
        """
        ``settle`` returns exactly the received-but-unacknowledged bytes and
        never the bytes the application already processed.
        """
        manager = WindowManager(max_window_size=65535)

        manager.window_consumed(1000)
        assert not manager.process_bytes(400)

        assert manager.settle() == 600

    def test_settle_is_idempotent(self) -> None:
        """
        The outstanding credit can be settled only once; settling again
        returns zero so the same bytes can never be refunded twice.
        """
        manager = WindowManager(max_window_size=65535)
        manager.window_consumed(1000)

        assert manager.settle() == 1000
        assert manager.settle() == 0

    def test_process_bytes_after_settle_is_a_noop(self) -> None:
        """
        A late acknowledgement of a settled stream never opens the window and
        never changes the accounting: it must be a complete no-op.
        """
        manager = WindowManager(max_window_size=65535)
        manager.window_consumed(1000)
        assert manager.settle() == 1000

        assert manager.process_bytes(1000) is None
        assert manager.bytes_acknowledged == 0
        assert manager.current_window_size == 65535 - 1000

    def test_settle_with_no_unacknowledged_data_returns_zero(self) -> None:
        manager = WindowManager(max_window_size=65535)
        manager.window_consumed(100)
        manager.process_bytes(100)

        assert manager.unacknowledged_bytes == 0
        assert manager.settle() == 0
        # Settling an already balanced manager still retires it exactly once.
        assert manager.settle() == 0

    def test_settle_with_no_data_is_idempotent(self) -> None:
        manager = WindowManager(max_window_size=65535)
        assert manager.settle() == 0
        assert manager.settle() == 0
        assert manager.process_bytes(10) is None

    def test_manual_window_opens_do_not_affect_accounting(self) -> None:
        """
        Manually opening the window changes the available space but must not
        alter the consumed/acknowledged counters used for settlement.
        """
        manager = WindowManager(max_window_size=65535)
        manager.window_consumed(1000)
        manager.window_opened(1000)

        assert manager.bytes_consumed == 1000
        assert manager.bytes_acknowledged == 0
        assert manager.unacknowledged_bytes == 1000
        assert manager.settle() == 1000

    def test_initial_state(self) -> None:
        manager = WindowManager(max_window_size=LARGEST_FLOW_CONTROL_WINDOW)
        assert manager.current_window_size == LARGEST_FLOW_CONTROL_WINDOW
        assert manager.bytes_consumed == 0
        assert manager.bytes_acknowledged == 0
        assert manager.unacknowledged_bytes == 0
