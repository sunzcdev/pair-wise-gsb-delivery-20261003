"""
test_data_frame_plan
~~~~~~~~~~~~~~~~~~~~

Tests for the read-only DATA frame planning API (H2Connection.plan_data and
H2Connection.send_planned_data).
"""
from __future__ import annotations

import pytest

import h2.config
import h2.connection
import h2.exceptions
import h2.plan
import h2.settings


class TestDataFramePlan:
    """
    Tests of the read-only framing plan on the send path.
    """

    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]

    DEFAULT_FLOW_WINDOW = 65535
    DEFAULT_MAX_FRAME_SIZE = 16384

    def _connection_with_stream(self):
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers)
        c.clear_outbound_data_buffer()
        return c

    def _send_all(self, c, stream_id, data):
        """
        Send data respecting the peer's maximum frame size.
        """
        max_frame = c.max_outbound_frame_size
        for offset in range(0, len(data), max_frame):
            c.send_data(stream_id, data[offset:offset + max_frame])

    def test_plan_splits_on_max_frame_size(self) -> None:
        """
        A body larger than the peer's max frame size is split into frames of
        at most max_outbound_frame_size bytes.
        """
        c = self._connection_with_stream()

        plan = c.plan_data(1, self.DEFAULT_MAX_FRAME_SIZE * 2 + 100)

        assert plan.fully_planned
        assert plan.blocked_reason is None
        assert [f.data_length for f in plan.frames] == [
            self.DEFAULT_MAX_FRAME_SIZE,
            self.DEFAULT_MAX_FRAME_SIZE,
            100,
        ]
        assert plan.planned_length == self.DEFAULT_MAX_FRAME_SIZE * 2 + 100
        assert plan.remaining_length == 0
        assert all(f.stream_id == 1 for f in plan.frames)
        assert all(f.pad_length is None for f in plan.frames)
        assert all(
            f.flow_controlled_length == f.data_length for f in plan.frames
        )

    def test_plan_accepts_bytes_and_memoryview(self) -> None:
        """
        The body may be passed directly instead of as a length.
        """
        c = self._connection_with_stream()
        data = b"x" * (self.DEFAULT_MAX_FRAME_SIZE + 1)

        assert c.plan_data(1, data) == c.plan_data(1, len(data))
        assert c.plan_data(1, memoryview(data)) == c.plan_data(1, len(data))

    def test_plan_is_read_only(self) -> None:
        """
        Planning consumes no credit, processes no state machine input, and
        queues no outbound data.
        """
        c = self._connection_with_stream()
        window_before = c.local_flow_control_window(1)
        conn_window_before = c.outbound_flow_control_window

        c.plan_data(1, 10_000)

        assert c.local_flow_control_window(1) == window_before
        assert c.outbound_flow_control_window == conn_window_before
        assert c.data_to_send() == b""
        # The stream is still able to send data: the state machine was not
        # advanced and no END_STREAM was emitted.
        c.send_data(1, b"after plan", end_stream=True)

    def test_plan_limited_by_stream_window(self, frame_factory) -> None:
        """
        When the stream-level window is the binding constraint, the plan
        stops there and names the stream window as the blocker.
        """
        c = self._connection_with_stream()
        # Shrink the stream-level outbound window only.
        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 100},
        )
        c.receive_data(f.serialize())

        plan = c.plan_data(1, 1_000)

        assert plan.planned_length == 100
        assert plan.remaining_length == 900
        assert not plan.fully_planned
        assert plan.blocked_reason is (
            h2.plan.DataPlanBlockedReason.STREAM_WINDOW_EXHAUSTED
        )

    def test_plan_limited_by_connection_window(self) -> None:
        """
        When the connection-level window is the binding constraint, the plan
        stops there and names the connection window as the blocker.
        """
        c = self._connection_with_stream()
        # Drain most of the connection-level window on another stream.
        c.send_headers(3, self.example_request_headers)
        self._send_all(c, 3, b"y" * (self.DEFAULT_FLOW_WINDOW - 10))

        plan = c.plan_data(1, 1_000)

        assert plan.planned_length == 10
        assert plan.remaining_length == 990
        assert plan.blocked_reason is (
            h2.plan.DataPlanBlockedReason.CONNECTION_WINDOW_EXHAUSTED
        )

    def test_plan_full_window(self) -> None:
        """
        With a fully exhausted window the sendable prefix is empty and the
        whole body remains, but an empty END_STREAM frame stays sendable.
        """
        c = self._connection_with_stream()
        self._send_all(c, 1, b"z" * self.DEFAULT_FLOW_WINDOW)
        assert c.local_flow_control_window(1) == 0

        plan = c.plan_data(1, 100)

        assert plan.frames == ()
        assert plan.planned_length == 0
        assert plan.remaining_length == 100
        assert plan.blocked_reason is (
            h2.plan.DataPlanBlockedReason.CONNECTION_WINDOW_EXHAUSTED
        )

        # Committing an empty plan sends nothing and consumes nothing.
        c.send_planned_data(plan, b"d" * 100)
        assert c.local_flow_control_window(1) == 0

        # A zero-length END_STREAM frame consumes no credit.
        assert plan.end_stream_sendable
        c.end_stream(1)

    def test_plan_zero_length_body(self) -> None:
        """
        A zero-length body plans as exactly one zero-length frame, even with
        a fully exhausted window.
        """
        c = self._connection_with_stream()
        self._send_all(c, 1, b"z" * self.DEFAULT_FLOW_WINDOW)

        plan = c.plan_data(1, b"")

        assert plan.fully_planned
        assert plan.blocked_reason is None
        assert len(plan.frames) == 1
        assert plan.frames[0].data_length == 0
        assert plan.frames[0].flow_controlled_length == 0

    def test_plan_zero_length_body_never_ends_stream(self) -> None:
        """
        Committing a zero-length plan does not emit END_STREAM.
        """
        c = self._connection_with_stream()

        plan = c.plan_data(1, 0)
        c.send_planned_data(plan, b"")

        # The stream must still be open for more data.
        c.send_data(1, b"more", end_stream=True)

    def test_plan_zero_length_padded_body(self) -> None:
        """
        A padded zero-length frame consumes credit for the padding and
        therefore needs a window at least pad_length + 1 bytes large.
        """
        c = self._connection_with_stream()

        plan = c.plan_data(1, 0, pad_length=5)

        assert plan.fully_planned
        assert len(plan.frames) == 1
        assert plan.frames[0].data_length == 0
        assert plan.frames[0].flow_controlled_length == 6

    def test_plan_zero_length_padded_body_full_window(self) -> None:
        """
        A padded zero-length frame cannot be planned on a fully exhausted
        window: the padding itself needs credit.
        """
        c = self._connection_with_stream()
        self._send_all(c, 1, b"z" * self.DEFAULT_FLOW_WINDOW)

        plan = c.plan_data(1, 0, pad_length=5)

        assert plan.frames == ()
        assert plan.blocked_reason is (
            h2.plan.DataPlanBlockedReason.CONNECTION_WINDOW_EXHAUSTED
        )

    def test_plan_zero_length_padded_body_stream_window_block(
        self, frame_factory,
    ) -> None:
        """
        When only the stream-level window is too small for the padding, the
        stream window is named as the blocker.
        """
        c = self._connection_with_stream()
        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 3},
        )
        c.receive_data(f.serialize())

        plan = c.plan_data(1, 0, pad_length=5)

        assert plan.frames == ()
        assert plan.blocked_reason is (
            h2.plan.DataPlanBlockedReason.STREAM_WINDOW_EXHAUSTED
        )

    def test_plan_accounts_for_padding(self) -> None:
        """
        Each padded frame consumes pad_length + 1 extra bytes of both the
        frame size budget and the flow control window.
        """
        c = self._connection_with_stream()
        pad_length = 10
        # Per frame: 16384 - 11 bytes of data.
        expected_chunk = self.DEFAULT_MAX_FRAME_SIZE - pad_length - 1

        plan = c.plan_data(1, expected_chunk * 2, pad_length=pad_length)

        assert [f.data_length for f in plan.frames] == [
            expected_chunk,
            expected_chunk,
        ]
        assert all(f.pad_length == pad_length for f in plan.frames)
        assert all(
            f.flow_controlled_length == self.DEFAULT_MAX_FRAME_SIZE
            for f in plan.frames
        )

    def test_plan_padding_exceeds_max_frame_size(self) -> None:
        """
        If the padding overhead alone fills the maximum frame size, no data
        can ever be framed and the plan says so. (Unreachable with a
        spec-compliant peer, whose max frame size is at least 16384, so the
        size is forced here to exercise the rule.)
        """
        c = self._connection_with_stream()
        c.max_outbound_frame_size = 16

        plan = c.plan_data(1, 100, pad_length=15)

        assert plan.frames == ()
        assert plan.remaining_length == 100
        assert plan.blocked_reason is (
            h2.plan.DataPlanBlockedReason.PADDING_EXCEEDS_MAX_FRAME_SIZE
        )

    def test_plan_zero_length_padding_exceeds_max_frame_size(self) -> None:
        """
        The padding-overhead rule also applies to zero-length bodies.
        """
        c = self._connection_with_stream()
        c.max_outbound_frame_size = 5

        plan = c.plan_data(1, 0, pad_length=5)

        assert plan.frames == ()
        assert plan.blocked_reason is (
            h2.plan.DataPlanBlockedReason.PADDING_EXCEEDS_MAX_FRAME_SIZE
        )

    def test_plan_validates_pad_length(self) -> None:
        """
        pad_length is validated exactly as in send_data.
        """
        c = self._connection_with_stream()

        with pytest.raises(TypeError, match="pad_length must be an int"):
            c.plan_data(1, 100, pad_length="10")
        with pytest.raises(ValueError, match=r"pad_length must be within range"):
            c.plan_data(1, 100, pad_length=256)
        with pytest.raises(ValueError, match=r"pad_length must be within range"):
            c.plan_data(1, 100, pad_length=-1)

    def test_plan_rejects_negative_length(self) -> None:
        c = self._connection_with_stream()

        with pytest.raises(ValueError, match="data length must be non-negative"):
            c.plan_data(1, -1)

    def test_plan_on_closed_stream(self) -> None:
        """
        Planning on a stream that cannot send data fails the same way
        send_data would, without mutating anything.
        """
        c = self._connection_with_stream()
        c.end_stream(1)

        with pytest.raises(h2.exceptions.StreamClosedError):
            c.plan_data(1, 100)

    def test_plan_on_missing_stream(self) -> None:
        c = self._connection_with_stream()

        with pytest.raises(h2.exceptions.NoSuchStreamError):
            c.plan_data(7, 100)

    def test_send_planned_data_matches_manual_send(self, frame_factory) -> None:
        """
        Committing a plan emits exactly the frames the plan described.
        """
        c = self._connection_with_stream()
        data = b"d" * (self.DEFAULT_MAX_FRAME_SIZE + 50)

        plan = c.plan_data(1, data)
        c.send_planned_data(plan, data)

        assert c.local_flow_control_window(1) == (
            self.DEFAULT_FLOW_WINDOW - len(data)
        )
        # Two DATA frames: 16384 bytes and 50 bytes, neither with END_STREAM.
        f1 = frame_factory.build_data_frame(data[:self.DEFAULT_MAX_FRAME_SIZE])
        f2 = frame_factory.build_data_frame(data[self.DEFAULT_MAX_FRAME_SIZE:])
        assert c.data_to_send() == f1.serialize() + f2.serialize()

    def test_send_planned_data_partial_prefix(self) -> None:
        """
        Only the planned prefix is sent; the remainder is left to the caller.
        """
        c = self._connection_with_stream()
        self._send_all(c, 1, b"z" * (self.DEFAULT_FLOW_WINDOW - 10))
        data = b"d" * 100

        plan = c.plan_data(1, data)
        assert plan.planned_length == 10

        c.send_planned_data(plan, data)

        assert c.local_flow_control_window(1) == 0

    def test_send_planned_data_with_padding(self) -> None:
        """
        Padded plans commit with the padding applied to each frame.
        """
        c = self._connection_with_stream()
        data = b"d" * 100

        plan = c.plan_data(1, data, pad_length=5)
        c.send_planned_data(plan, data)

        assert c.local_flow_control_window(1) == (
            self.DEFAULT_FLOW_WINDOW - 100 - 6
        )

    def test_send_planned_data_rejects_stale_window(self, frame_factory) -> None:
        """
        A plan computed before a WINDOW_UPDATE arrived is rejected, and the
        recomputed plan reflects the new window.
        """
        c = self._connection_with_stream()
        self._send_all(c, 1, b"z" * self.DEFAULT_FLOW_WINDOW)
        plan = c.plan_data(1, 100)
        assert plan.frames == ()

        c.receive_data(
            frame_factory.build_window_update_frame(0, 100).serialize()
            + frame_factory.build_window_update_frame(1, 100).serialize(),
        )

        with pytest.raises(h2.exceptions.StaleDataPlanError):
            c.send_planned_data(plan, b"d" * 100)

        # Recomputing the plan works and reflects the new window.
        new_plan = c.plan_data(1, 100)
        assert new_plan.fully_planned
        c.send_planned_data(new_plan, b"d" * 100)
        assert c.local_flow_control_window(1) == 0

    def test_send_planned_data_rejects_stale_max_frame_size(
        self, frame_factory,
    ) -> None:
        """
        A plan computed before a SETTINGS change to the peer's maximum frame
        size is rejected.
        """
        c = self._connection_with_stream()
        plan = c.plan_data(1, 100)

        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.MAX_FRAME_SIZE: 32768},
        )
        c.receive_data(f.serialize())

        with pytest.raises(h2.exceptions.StaleDataPlanError):
            c.send_planned_data(plan, b"d" * 100)

    def test_send_planned_data_rejects_stale_after_other_send(self) -> None:
        """
        Sending other data after planning consumes credit, staling the plan.
        """
        c = self._connection_with_stream()
        plan = c.plan_data(1, 100)

        c.send_data(1, b"other")

        with pytest.raises(h2.exceptions.StaleDataPlanError):
            c.send_planned_data(plan, b"d" * 100)

    def test_stale_plan_failure_consumes_no_credit(self) -> None:
        """
        A rejected commit leaves the windows untouched.
        """
        c = self._connection_with_stream()
        plan = c.plan_data(1, 100)
        c.send_data(1, b"other")
        window_before = c.local_flow_control_window(1)

        with pytest.raises(h2.exceptions.StaleDataPlanError):
            c.send_planned_data(plan, b"d" * 100)

        assert c.local_flow_control_window(1) == window_before

    def test_send_planned_data_validates_length(self) -> None:
        c = self._connection_with_stream()
        plan = c.plan_data(1, 100)

        with pytest.raises(ValueError, match="does not match planned length"):
            c.send_planned_data(plan, b"d" * 99)

    def test_send_planned_data_accepts_memoryview(self) -> None:
        c = self._connection_with_stream()
        data = b"d" * 100
        plan = c.plan_data(1, data)

        c.send_planned_data(plan, memoryview(data))

        assert c.local_flow_control_window(1) == self.DEFAULT_FLOW_WINDOW - 100
