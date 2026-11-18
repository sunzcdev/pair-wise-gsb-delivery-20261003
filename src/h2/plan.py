"""
h2/plan
~~~~~~~

Read-only DATA frame planning for the send path.

This module provides objects that describe how a body would be split into
DATA frames *without* mutating any connection or stream state: no flow
control credit is consumed, no state machine input is processed, and no
``END_STREAM`` flag is ever emitted by a plan.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum


class DataPlanBlockedReason(Enum):
    """
    The reason a :class:`DataFramePlan` could not plan the remainder of a
    body.

    .. versionadded:: 4.5.0
    """

    #: The connection-level outbound flow control window is exhausted. More
    #: data can be planned once the peer grants connection-level credit via
    #: a WINDOW_UPDATE frame on stream 0.
    CONNECTION_WINDOW_EXHAUSTED = 0

    #: The stream-level outbound flow control window is exhausted. More data
    #: can be planned once the peer grants stream-level credit via a
    #: WINDOW_UPDATE frame on this stream.
    STREAM_WINDOW_EXHAUSTED = 1

    #: The padding overhead (``pad_length + 1``) is greater than or equal to
    #: the peer's maximum frame size, so not even a single byte of data can
    #: be framed with this padding. No amount of flow control credit will
    #: fix this: reduce ``pad_length``.
    PADDING_EXCEEDS_MAX_FRAME_SIZE = 2


@dataclass(frozen=True)
class PlannedDataFrame:
    """
    A single DATA frame in a plan.

    Describes only sizes, never payload bytes: a plan is an estimate, not a
    buffer.

    .. versionadded:: 4.5.0
    """

    #: The stream the frame would be sent on.
    stream_id: int

    #: The number of body bytes carried by this frame, excluding padding.
    data_length: int

    #: The padding length that would be applied to this frame, or ``None``
    #: for no padding. Mirrors the ``pad_length`` argument of
    #: :meth:`H2Connection.send_data <h2.connection.H2Connection.send_data>`.
    pad_length: int | None

    @property
    def flow_controlled_length(self) -> int:
        """
        The number of flow control bytes this frame would consume: the data
        plus, when padded, the padding bytes and the one-byte Pad Length
        field.
        """
        if self.pad_length is None:
            return self.data_length
        return self.data_length + self.pad_length + 1


@dataclass(frozen=True)
class DataFramePlan:
    """
    A read-only estimate of how a body would be split into DATA frames.

    A plan is a snapshot: it is computed from the connection-level and
    stream-level outbound flow control windows and the peer's maximum frame
    size at planning time. It consumes no flow control credit, processes no
    state machine input, and never sets ``END_STREAM``.

    A plan is only valid for as long as none of those inputs change. Use
    :meth:`H2Connection.send_planned_data
    <h2.connection.H2Connection.send_planned_data>` to commit a plan: it
    revalidates the snapshot and raises :class:`StaleDataPlanError
    <h2.exceptions.StaleDataPlanError>` if the windows or the peer's maximum
    frame size have changed, in which case the plan must be recomputed.

    Rules for edge cases:

    - A zero-length body plans as exactly one zero-length DATA frame. Such a
      frame consumes no credit (unless padding is requested) and is
      therefore always sendable, even with a fully exhausted window.
    - When a window is fully exhausted, ``frames`` is empty,
      ``blocked_reason`` names the exhausted layer, and
      ``remaining_length`` equals ``total_length``.
    - Ending the stream is never part of a plan. An empty DATA frame with
      ``END_STREAM`` consumes no flow control credit, so
      :meth:`H2Connection.end_stream <h2.connection.H2Connection.end_stream>`
      remains sendable regardless of the window; ``end_stream_sendable``
      records this.

    .. versionadded:: 4.5.0
    """

    #: The stream this plan applies to.
    stream_id: int

    #: The total number of body bytes the plan was computed for.
    total_length: int

    #: The padding length applied to every planned frame, or ``None``.
    pad_length: int | None

    #: The sendable prefix: the frames that fit into the current windows,
    #: in order. Never carries ``END_STREAM``.
    frames: tuple[PlannedDataFrame, ...] = ()

    #: Why the remainder of the body could not be planned, or ``None`` if
    #: the whole body was planned.
    blocked_reason: DataPlanBlockedReason | None = None

    #: Whether an empty ``END_STREAM`` frame may be sent on this stream.
    #: This is always ``True`` for a successfully created plan: such a frame
    #: consumes no flow control credit, so even a fully exhausted window
    #: cannot block it.
    end_stream_sendable: bool = True

    # Snapshot of the inputs the plan was computed from. Used by
    # H2Connection.send_planned_data to detect staleness. Not part of the
    # public API.
    _connection_window: int = field(default=0, repr=False, compare=False)
    _stream_window: int = field(default=0, repr=False, compare=False)
    _max_frame_size: int = field(default=0, repr=False, compare=False)

    @property
    def planned_length(self) -> int:
        """
        The number of body bytes covered by :attr:`frames`.
        """
        return sum(f.data_length for f in self.frames)

    @property
    def remaining_length(self) -> int:
        """
        The number of body bytes that could not be planned right now.
        """
        return self.total_length - self.planned_length

    @property
    def fully_planned(self) -> bool:
        """
        Whether the whole body fits into the currently sendable prefix.
        """
        return self.remaining_length == 0


def _blocked_reason(pad_overhead: int,
                    connection_window: int,
                    stream_window: int,
                    max_frame_size: int) -> DataPlanBlockedReason:
    """
    Determine why not even one more byte of data can be framed.
    """
    if pad_overhead >= max_frame_size:
        return DataPlanBlockedReason.PADDING_EXCEEDS_MAX_FRAME_SIZE
    if connection_window <= stream_window:
        return DataPlanBlockedReason.CONNECTION_WINDOW_EXHAUSTED
    return DataPlanBlockedReason.STREAM_WINDOW_EXHAUSTED


def _plan_frames(stream_id: int,
                 total_length: int,
                 pad_length: int | None,
                 connection_window: int,
                 stream_window: int,
                 max_frame_size: int) -> tuple[tuple[PlannedDataFrame, ...], DataPlanBlockedReason | None]:
    """
    Pure computation of a framing plan. Consumes nothing: the window values
    are copied and only the copies are decremented.
    """
    # Padding bytes plus the 1-byte padding length field, as in send_data.
    pad_overhead = 0 if pad_length is None else pad_length + 1

    if total_length == 0:
        # A zero-length body is a single zero-length frame. Unpadded it
        # consumes no credit and is always sendable, even on a fully
        # exhausted window; padded it needs room for the padding in both
        # the window and the maximum frame size, like any other frame.
        if pad_overhead <= min(connection_window, stream_window, max_frame_size):
            return (PlannedDataFrame(stream_id, 0, pad_length),), None
        return (), _blocked_reason(
            pad_overhead, connection_window, stream_window, max_frame_size,
        )

    frames: list[PlannedDataFrame] = []
    blocked_reason = None
    remaining = total_length
    while remaining > 0:
        allowance = min(connection_window, stream_window, max_frame_size)
        capacity = allowance - pad_overhead
        if capacity <= 0:
            # Not even one more byte of data fits in a frame.
            blocked_reason = _blocked_reason(
                pad_overhead, connection_window, stream_window, max_frame_size,
            )
            break
        chunk = min(remaining, capacity)
        frames.append(PlannedDataFrame(stream_id, chunk, pad_length))
        connection_window -= chunk + pad_overhead
        stream_window -= chunk + pad_overhead
        remaining -= chunk

    return tuple(frames), blocked_reason
