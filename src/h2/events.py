"""
h2/events
~~~~~~~~~

Defines Event types for HTTP/2.

Events are returned by the H2 state machine to allow implementations to keep
track of events triggered by receiving data. Each time data is provided to the
H2 state machine it processes the data and returns a list of Event objects.
"""
from __future__ import annotations

import binascii
import contextlib
import enum
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

from .settings import ChangedSetting, SettingCodes, Settings, _setting_code_from_int

if TYPE_CHECKING:  # pragma: no cover
    from hpack.struct import Header
    from hyperframe.frame import Frame

    from .errors import ErrorCodes


kw_only = {"kw_only": True}


_LAZY_INIT: Any = object()
"""
Some h2 events are instantiated by the state machine, but its attributes are
subsequently populated by H2Stream. To make this work with strict type annotations
on the events, they are temporarily set to this placeholder value.
This value should never be exposed to users.
"""


class Event:
    """
    Base class for h2 events.
    """


@dataclass(**kw_only)
class RequestReceived(Event):
    """
    The RequestReceived event is fired whenever all of a request's headers
    are received. This event carries the HTTP headers for the given request
    and the stream ID of the new stream.

    In HTTP/2, headers may be sent as a HEADERS frame followed by zero or more
    CONTINUATION frames with the final frame setting the END_HEADERS flag.
    This event is fired after the entire sequence is received.

    .. versionchanged:: 2.3.0
       Changed the type of ``headers`` to :class:`HeaderTuple
       <hpack:hpack.HeaderTuple>`. This has no effect on current users.

    .. versionchanged:: 2.4.0
       Added ``stream_ended`` and ``priority_updated`` properties.
    """

    stream_id: int
    """The Stream ID for the stream this request was made on."""

    headers: list[Header] = _LAZY_INIT
    """The request headers."""

    stream_ended: StreamEnded | None = None
    """
    If this request also ended the stream, the associated
    :class:`StreamEnded <h2.events.StreamEnded>` event will be available
    here.

    .. versionadded:: 2.4.0
    """

    priority_updated: PriorityUpdated | None = None
    """
    If this request also had associated priority information, the
    associated :class:`PriorityUpdated <h2.events.PriorityUpdated>`
    event will be available here.

    .. versionadded:: 2.4.0
    """

    def __repr__(self) -> str:
        return f"<RequestReceived stream_id:{self.stream_id}, headers:{self.headers}>"


@dataclass(**kw_only)
class ResponseReceived(Event):
    """
    The ResponseReceived event is fired whenever response headers are received.
    This event carries the HTTP headers for the given response and the stream
    ID of the new stream.

    .. versionchanged:: 2.3.0
       Changed the type of ``headers`` to :class:`HeaderTuple
       <hpack:hpack.HeaderTuple>`. This has no effect on current users.

    .. versionchanged:: 2.4.0
      Added ``stream_ended`` and ``priority_updated`` properties.
    """

    stream_id: int
    """The Stream ID for the stream this response was made on."""

    headers: list[Header] = _LAZY_INIT
    """The response headers."""

    stream_ended: StreamEnded | None = None
    """
    If this response also ended the stream, the associated
    :class:`StreamEnded <h2.events.StreamEnded>` event will be available
    here.

    .. versionadded:: 2.4.0
    """

    priority_updated: PriorityUpdated | None = None
    """
    If this response also had associated priority information, the
    associated :class:`PriorityUpdated <h2.events.PriorityUpdated>`
    event will be available here.

    .. versionadded:: 2.4.0
    """

    def __repr__(self) -> str:
        return f"<ResponseReceived stream_id:{self.stream_id}, headers:{self.headers}>"


@dataclass(**kw_only)
class TrailersReceived(Event):
    """
    The TrailersReceived event is fired whenever trailers are received on a
    stream. Trailers are a set of headers sent after the body of the
    request/response, and are used to provide information that wasn't known
    ahead of time (e.g. content-length). This event carries the HTTP header
    fields that form the trailers and the stream ID of the stream on which they
    were received.

    .. versionchanged:: 2.3.0
       Changed the type of ``headers`` to :class:`HeaderTuple
       <hpack:hpack.HeaderTuple>`. This has no effect on current users.

    .. versionchanged:: 2.4.0
       Added ``stream_ended`` and ``priority_updated`` properties.
    """

    stream_id: int
    """The Stream ID for the stream on which these trailers were received."""

    headers: list[Header] = _LAZY_INIT
    """The trailers themselves."""

    stream_ended: StreamEnded | None = None
    """
    Trailers always end streams. This property has the associated
    :class:`StreamEnded <h2.events.StreamEnded>` in it.

    .. versionadded:: 2.4.0
    """

    priority_updated: PriorityUpdated | None = None
    """
    If the trailers also set associated priority information, the
    associated :class:`PriorityUpdated <h2.events.PriorityUpdated>`
    event will be available here.

    .. versionadded:: 2.4.0
    """

    def __repr__(self) -> str:
        return f"<TrailersReceived stream_id:{self.stream_id}, headers:{self.headers}>"


class _HeadersSent(Event):
    """
    The _HeadersSent event is fired whenever headers are sent.

    This is an internal event, used to determine validation steps on
    outgoing header blocks.
    """



class _ResponseSent(_HeadersSent):
    """
    The _ResponseSent event is fired whenever response headers are sent
    on a stream.

    This is an internal event, used to determine validation steps on
    outgoing header blocks.
    """



class _RequestSent(_HeadersSent):
    """
    The _RequestSent event is fired whenever request headers are sent
    on a stream.

    This is an internal event, used to determine validation steps on
    outgoing header blocks.
    """



class _TrailersSent(_HeadersSent):
    """
    The _TrailersSent event is fired whenever trailers are sent on a
    stream. Trailers are a set of headers sent after the body of the
    request/response, and are used to provide information that wasn't known
    ahead of time (e.g. content-length).

    This is an internal event, used to determine validation steps on
    outgoing header blocks.
    """



class _PushedRequestSent(_HeadersSent):
    """
    The _PushedRequestSent event is fired whenever pushed request headers are
    sent.

    This is an internal event, used to determine validation steps on outgoing
    header blocks.
    """


@dataclass(**kw_only)
class InformationalResponseReceived(Event):
    """
    The InformationalResponseReceived event is fired when an informational
    response (that is, one whose status code is a 1XX code) is received from
    the remote peer.

    The remote peer may send any number of these, from zero upwards. These
    responses are most commonly sent in response to requests that have the
    ``expect: 100-continue`` header field present. Most users can safely
    ignore this event unless you are intending to use the
    ``expect: 100-continue`` flow, or are for any reason expecting a different
    1XX status code.

    .. versionadded:: 2.2.0

    .. versionchanged:: 2.3.0
       Changed the type of ``headers`` to :class:`HeaderTuple
       <hpack:hpack.HeaderTuple>`. This has no effect on current users.

    .. versionchanged:: 2.4.0
       Added ``priority_updated`` property.
    """

    stream_id: int
    """The Stream ID for the stream this informational response was made on."""

    headers: list[Header] = _LAZY_INIT
    """The headers for this informational response."""

    priority_updated: PriorityUpdated | None = None
    """
    If this response also had associated priority information, the
    associated :class:`PriorityUpdated <h2.events.PriorityUpdated>`
    event will be available here.

    .. versionadded:: 2.4.0
    """

    def __repr__(self) -> str:
        return f"<InformationalResponseReceived stream_id:{self.stream_id}, headers:{self.headers}>"


@dataclass(**kw_only)
class DataReceived(Event):
    """
    The DataReceived event is fired whenever data is received on a stream from
    the remote peer. The event carries the data itself, and the stream ID on
    which the data was received.

    .. versionchanged:: 2.4.0
       Added ``stream_ended`` property.
    """

    stream_id: int
    """The Stream ID for the stream this data was received on."""

    data: bytes = _LAZY_INIT
    """The data itself."""

    flow_controlled_length: int = _LAZY_INIT
    """
    The amount of data received that counts against the flow control
    window. Note that padding counts against the flow control window, so
    when adjusting flow control you should always use this field rather
    than ``len(data)``.
    """

    stream_ended: StreamEnded | None = None
    """
    If this data chunk also completed the stream, the associated
    :class:`StreamEnded <h2.events.StreamEnded>` event will be available
    here.

    .. versionadded:: 2.4.0
    """

    def __repr__(self) -> str:
        return (
            "<DataReceived stream_id:{}, "
            "flow_controlled_length:{}, "
            "data:{}>".format(
                self.stream_id,
                self.flow_controlled_length,
                _bytes_representation(self.data[:20]) if self.data else "",
            )
        )


@dataclass(**kw_only)
class WindowUpdated(Event):
    """
    The WindowUpdated event is fired whenever a flow control window changes
    size. HTTP/2 defines flow control windows for connections and streams: this
    event fires for both connections and streams. The event carries the ID of
    the stream to which it applies (set to zero if the window update applies to
    the connection), and the delta in the window size.
    """

    stream_id: int
    """
    The Stream ID of the stream whose flow control window was changed.
    May be ``0`` if the connection window was changed.
    """

    delta: int = _LAZY_INIT
    """
    The window delta.
    """

    def __repr__(self) -> str:
        return f"<WindowUpdated stream_id:{self.stream_id}, delta:{self.delta}>"


class RemoteSettingsChanged(Event):
    """
    The RemoteSettingsChanged event is fired whenever the remote peer changes
    its settings. It contains a complete inventory of changed settings,
    including their previous values.

    In HTTP/2, settings changes need to be acknowledged. h2 automatically
    acknowledges settings changes for efficiency. However, it is possible that
    the caller may not be happy with the changed setting.

    When this event is received, the caller should confirm that the new
    settings are acceptable. If they are not acceptable, the user should close
    the connection with the error code :data:`PROTOCOL_ERROR
    <h2.errors.ErrorCodes.PROTOCOL_ERROR>`.

    .. versionchanged:: 2.0.0
       Prior to this version the user needed to acknowledge settings changes.
       This is no longer the case: h2 now automatically acknowledges
       them.
    """

    def __init__(self) -> None:
        #: A dictionary of setting byte to
        #: :class:`ChangedSetting <h2.settings.ChangedSetting>`, representing
        #: the changed settings.
        self.changed_settings: dict[int, ChangedSetting] = {}

    @classmethod
    def from_settings(cls,
                      old_settings: Settings | dict[int, int],
                      new_settings: dict[int, int]) -> RemoteSettingsChanged:
        """
        Build a RemoteSettingsChanged event from a set of changed settings.

        :param old_settings: A complete collection of old settings, in the form
                             of a dictionary of ``{setting: value}``.
        :param new_settings: All the changed settings and their new values, in
                             the form of a dictionary of ``{setting: value}``.
        """
        e = cls()
        for setting, new_value in new_settings.items():
            s = _setting_code_from_int(setting)
            original_value = old_settings.get(s)
            change = ChangedSetting(s, original_value, new_value)
            e.changed_settings[s] = change

        return e

    def __repr__(self) -> str:
        return "<RemoteSettingsChanged changed_settings:{{{}}}>".format(
            ", ".join(repr(cs) for cs in self.changed_settings.values()),
        )


@dataclass(**kw_only)
class PingReceived(Event):
    """
    The PingReceived event is fired whenever a PING is received. It contains
    the 'opaque data' of the PING frame. A ping acknowledgment with the same
    'opaque data' is automatically emitted after receiving a ping.

    .. versionadded:: 3.1.0
    """

    ping_data: bytes
    """The data included on the ping."""

    def __repr__(self) -> str:
        return f"<PingReceived ping_data:{_bytes_representation(self.ping_data)}>"


@dataclass(**kw_only)
class PingAckReceived(Event):
    """
    The PingAckReceived event is fired whenever a PING acknowledgment is
    received. It contains the 'opaque data' of the PING+ACK frame, allowing the
    user to correlate PINGs and calculate RTT.

    .. versionadded:: 3.1.0

    .. versionchanged:: 4.0.0
       Removed deprecated but equivalent ``PingAcknowledged``.
    """

    ping_data: bytes
    """The data included on the ping."""

    def __repr__(self) -> str:
        return f"<PingAckReceived ping_data:{_bytes_representation(self.ping_data)}>"


@dataclass(**kw_only)
class StreamEnded(Event):
    """
    The StreamEnded event is fired whenever a stream is ended by a remote
    party. The stream may not be fully closed if it has not been closed
    locally, but no further data or headers should be expected on that stream.
    """

    stream_id: int
    """The Stream ID of the stream that was closed."""

    def __repr__(self) -> str:
        return f"<StreamEnded stream_id:{self.stream_id}>"


@dataclass(**kw_only)
class StreamReset(Event):
    """
    The StreamReset event is fired in two situations. The first is when the
    remote party forcefully resets the stream. The second is when the remote
    party has made a protocol error which only affects a single stream. In this
    case, h2 will terminate the stream early and return this event.

    .. versionchanged:: 2.0.0
       This event is now fired when h2 automatically resets a stream.
    """

    stream_id: int
    """
    The Stream ID of the stream that was reset.
    """

    error_code: ErrorCodes | int = _LAZY_INIT
    """
    The error code given.
    """

    remote_reset: bool = True
    """
    Whether the remote peer sent a RST_STREAM or we did.
    """

    def __repr__(self) -> str:
        return f"<StreamReset stream_id:{self.stream_id}, error_code:{self.error_code!s}, remote_reset:{self.remote_reset}>"


class PushedStreamReceived(Event):
    """
    The PushedStreamReceived event is fired whenever a pushed stream has been
    received from a remote peer. The event carries on it the new stream ID, the
    ID of the parent stream, and the request headers pushed by the remote peer.
    """

    def __init__(self) -> None:
        #: The Stream ID of the stream created by the push.
        self.pushed_stream_id: int | None = None

        #: The Stream ID of the stream that the push is related to.
        self.parent_stream_id: int | None = None

        #: The request headers, sent by the remote party in the push.
        self.headers: list[Header] | None = None

    def __repr__(self) -> str:
        return (
            f"<PushedStreamReceived pushed_stream_id:{self.pushed_stream_id}, parent_stream_id:{self.parent_stream_id}, "
            f"headers:{self.headers}>"
        )


class SettingsAcknowledged(Event):
    """
    The SettingsAcknowledged event is fired whenever a settings ACK is received
    from the remote peer. The event carries on it the settings that were
    acknowledged, in the same format as
    :class:`h2.events.RemoteSettingsChanged`.
    """

    def __init__(self) -> None:
        #: A dictionary of setting byte to
        #: :class:`ChangedSetting <h2.settings.ChangedSetting>`, representing
        #: the changed settings.
        self.changed_settings: dict[SettingCodes | int, ChangedSetting] = {}

    def __repr__(self) -> str:
        s = ", ".join(repr(cs) for cs in self.changed_settings.values())
        return f"<SettingsAcknowledged changed_settings:{{{s}}}>"


class PriorityUpdated(Event):
    """
    The PriorityUpdated event is fired whenever a stream sends updated priority
    information. This can occur when the stream is opened, or at any time
    during the stream lifetime.

    This event is purely advisory, and does not need to be acted on.

    .. versionadded:: 2.0.0
    """

    def __init__(self) -> None:
        #: The ID of the stream whose priority information is being updated.
        self.stream_id: int | None = None

        #: The new stream weight. May be the same as the original stream
        #: weight. An integer between 1 and 256.
        self.weight: int | None = None

        #: The stream ID this stream now depends on. May be ``0``.
        self.depends_on: int | None = None

        #: Whether the stream *exclusively* depends on the parent stream. If it
        #: does, this stream should inherit the current children of its new
        #: parent.
        self.exclusive: bool | None = None

    def __repr__(self) -> str:
        return (
            f"<PriorityUpdated stream_id:{self.stream_id}, weight:{self.weight}, depends_on:{self.depends_on}, "
            f"exclusive:{self.exclusive}>"
        )


class ConnectionTerminated(Event):
    """
    The ConnectionTerminated event is fired when a connection is torn down by
    the remote peer using a GOAWAY frame. Once received, no further action may
    be taken on the connection: a new connection must be established.
    """

    def __init__(self) -> None:
        #: The error code cited when tearing down the connection. Should be
        #: one of :class:`ErrorCodes <h2.errors.ErrorCodes>`, but may not be if
        #: unknown HTTP/2 extensions are being used.
        self.error_code: ErrorCodes | int | None = None

        #: The stream ID of the last stream the remote peer saw. This can
        #: provide an indication of what data, if any, never reached the remote
        #: peer and so can safely be resent.
        self.last_stream_id: int | None = None

        #: Additional debug data that can be appended to GOAWAY frame.
        self.additional_data: bytes | None = None

    def __repr__(self) -> str:
        return (
            "<ConnectionTerminated error_code:{!s}, last_stream_id:{}, "
            "additional_data:{}>".format(
                self.error_code,
                self.last_stream_id,
                _bytes_representation(
                    self.additional_data[:20]
                    if self.additional_data else None),
            )
        )


class AlternativeServiceAvailable(Event):
    """
    The AlternativeServiceAvailable event is fired when the remote peer
    advertises an `RFC 7838 <https://tools.ietf.org/html/rfc7838>`_ Alternative
    Service using an ALTSVC frame.

    This event always carries the origin to which the ALTSVC information
    applies. That origin is either supplied by the server directly, or inferred
    by h2 from the ``:authority`` pseudo-header field that was sent by
    the user when initiating a given stream.

    This event also carries what RFC 7838 calls the "Alternative Service Field
    Value", which is formatted like a HTTP header field and contains the
    relevant alternative service information. h2 does not parse or in any
    way modify that information: the user is required to do that.

    This event can only be fired on the client end of a connection.

    .. versionadded:: 2.3.0
    """

    def __init__(self) -> None:
        #: The origin to which the alternative service field value applies.
        #: This field is either supplied by the server directly, or inferred by
        #: h2 from the ``:authority`` pseudo-header field that was sent
        #: by the user when initiating the stream on which the frame was
        #: received.
        self.origin: bytes | None = None

        #: The ALTSVC field value. This contains information about the HTTP
        #: alternative service being advertised by the server. h2 does
        #: not parse this field: it is left exactly as sent by the server. The
        #: structure of the data in this field is given by `RFC 7838 Section 3
        #: <https://tools.ietf.org/html/rfc7838#section-3>`_.
        self.field_value: bytes | None = None

    def __repr__(self) -> str:
        return (
            "<AlternativeServiceAvailable origin:{}, field_value:{}>".format(
                (self.origin or b"").decode("utf-8", "ignore"),
                (self.field_value or b"").decode("utf-8", "ignore"),
            )
        )


@dataclass(**kw_only)
class UnknownFrameReceived(Event):
    """
    The UnknownFrameReceived event is fired when the remote peer sends a frame
    that h2 does not understand. This occurs primarily when the remote
    peer is employing HTTP/2 extensions that h2 doesn't know anything
    about.

    RFC 7540 requires that HTTP/2 implementations ignore these frames. h2
    does so. However, this event is fired to allow implementations to perform
    special processing on those frames if needed (e.g. if the implementation
    is capable of handling the frame itself).

    .. versionadded:: 2.7.0
    """

    frame: Frame

    def __repr__(self) -> str:
        return "<UnknownFrameReceived>"


class StreamOrigin(enum.Enum):
    """
    Which endpoint initiated an observed HTTP/2 stream.

    This is used to keep server push streams and locally initiated streams
    distinct, and to flag records whose origin is unknown because the observer
    attached part-way through.

    .. versionadded:: 4.5.0
    """

    #: The stream was initiated by this endpoint.
    LOCAL = "local"

    #: The stream was initiated by the peer.
    REMOTE = "remote"


class StreamOutcome(enum.Enum):
    """
    The settlement state of an observed HTTP/2 stream.

    The value tracks *both* directions of a stream: HTTP/2 streams are
    half-closed, so a cleanly finished stream is only ``COMPLETED`` once the
    local and the remote direction have both ended. A stream that was reset is
    always ``RESET``, regardless of how far it had progressed.

    .. versionadded:: 4.5.0
    """

    #: The stream is still open in both directions.
    OPEN = "open"

    #: This endpoint has finished sending, but the peer has not.
    HALF_CLOSED_LOCAL = "half_closed_local"

    #: The peer has finished sending, but this endpoint has not.
    HALF_CLOSED_REMOTE = "half_closed_remote"

    #: Both directions ended cleanly.
    COMPLETED = "completed"

    #: The stream was reset, locally or by the peer.
    RESET = "reset"


@dataclass
class StreamLifecycleRecord:
    """
    A snapshot of the lifecycle of a single HTTP/2 stream.

    Records are created and maintained by
    :class:`StreamLifecycleObserver`. They never hold header fields or body
    bytes: only timestamps and byte counts are retained.

    .. note:: Timestamps are values returned by the observer's clock callable.
       They are only meaningful relative to other timestamps taken by the same
       observer. A value of ``None`` means the event was not observed, either
       because the observer only started watching the stream part-way through
       its life, or because the clock callable raised.

    .. versionadded:: 4.5.0
    """

    stream_id: int
    """The stream ID this record describes."""

    origin: StreamOrigin | None = None
    """
    Whether this endpoint or the peer initiated the stream, or ``None`` if the
    observer could not tell (for example because it was attached after the
    stream was opened).
    """

    is_push: bool = False
    """
    Whether this stream is a server push stream. Push streams are kept
    distinct from ordinary locally-initiated streams.
    """

    initiated_at: float | None = None
    """Timestamp taken when the stream was observed to open."""

    response_headers_at: float | None = None
    """
    Timestamp taken when the first *final* response header block was observed.
    Informational (1XX) responses are counted separately and do not fill this
    field in.
    """

    first_body_at: float | None = None
    """
    Timestamp taken when the first non-empty received body chunk was observed.
    """

    local_closed_at: float | None = None
    """Timestamp taken when this endpoint finished sending on the stream."""

    remote_closed_at: float | None = None
    """Timestamp taken when the peer finished sending on the stream."""

    settled_at: float | None = None
    """Timestamp taken when the stream was settled, exactly once."""

    reset_at: float | None = None
    """Timestamp taken when the stream was reset."""

    reset_error_code: int | None = None
    """The HTTP/2 error code carried by the reset, if any."""

    reset_by_remote: bool | None = None
    """Whether the peer reset the stream. ``False`` means this endpoint did."""

    informational_responses: int = 0
    """How many informational (1XX) responses were observed."""

    response_headers_count: int = 0
    """How many final response header blocks were observed."""

    body_chunks: int = 0
    """How many non-empty body chunks were observed."""

    body_bytes: int = 0
    """The total number of body bytes received, excluding padding."""

    flow_controlled_bytes: int = 0
    """
    The total flow-controlled length of all received DATA frames, including
    padding. This is a count only: no body content is retained.
    """

    clock_failed: bool = False
    """
    Whether any clock call for this stream raised. If so, the timing data is
    incomplete, but no protocol state was affected.
    """

    # These flags record *whether* a transition happened, independently of
    # whether a timestamp could be taken for it. A failing clock must not make
    # the record forget that a direction closed, or the observer could settle a
    # stream twice.
    _opened: bool = field(default=False, repr=False)
    _local_closed: bool = field(default=False, repr=False)
    _remote_closed: bool = field(default=False, repr=False)
    _settled: bool = field(default=False, repr=False)
    _reset: bool = field(default=False, repr=False)

    @property
    def outcome(self) -> StreamOutcome:
        """
        The current settlement state of the stream.
        """
        if self._reset:
            return StreamOutcome.RESET
        if self._local_closed and self._remote_closed:
            return StreamOutcome.COMPLETED
        if self._local_closed:
            return StreamOutcome.HALF_CLOSED_LOCAL
        if self._remote_closed:
            return StreamOutcome.HALF_CLOSED_REMOTE
        return StreamOutcome.OPEN

    @property
    def settled(self) -> bool:
        """
        Whether this record has been fully settled. Settling happens exactly
        once per stream: either cleanly once both directions ended, or as soon
        as the stream is reset.
        """
        return self._settled

    @property
    def local_ended(self) -> bool:
        """Whether this endpoint has finished sending on the stream."""
        return self._local_closed

    @property
    def remote_ended(self) -> bool:
        """Whether the peer has finished sending on the stream."""
        return self._remote_closed

    @property
    def timings_complete(self) -> bool:
        """
        Whether the timings can be trusted as a complete picture of the
        stream. This is ``False`` when the observer attached part-way through
        the stream or when a clock call raised.
        """
        return self._opened and self.initiated_at is not None and not self.clock_failed

    @property
    def time_to_response_headers(self) -> float | None:
        """Time between stream initiation and final response headers."""
        return _elapsed(self.initiated_at, self.response_headers_at)

    @property
    def time_to_first_body(self) -> float | None:
        """Time between stream initiation and the first body chunk."""
        return _elapsed(self.initiated_at, self.first_body_at)

    @property
    def total_duration(self) -> float | None:
        """
        Time between stream initiation and settlement.

        This is a per-stream measurement: it is never derived from how long the
        connection as a whole has been alive.
        """
        return _elapsed(self.initiated_at, self.settled_at)

    def __repr__(self) -> str:
        return (
            f"<StreamLifecycleRecord stream_id:{self.stream_id}, "
            f"origin:{self.origin}, is_push:{self.is_push}, "
            f"outcome:{self.outcome}>"
        )


class StreamLifecycleObserver:
    """
    An optional observer that records the lifecycle of individual HTTP/2
    streams from the events h2 actually produces.

    A gateway can attach one of these (through
    :attr:`H2Configuration.stream_lifecycle_observer
    <h2.config.H2Configuration.stream_lifecycle_observer>`) and then query
    per-stream outcomes and timings without inferring anything from the total
    time spent on the connection.

    The observer follows these rules:

    - Stream initiation, final response headers, first body chunk, clean
      completion and reset are recorded as they happen.
    - Informational (1XX) responses are counted, but are never treated as the
      final response.
    - Server push streams are kept distinct from ordinary locally initiated
      streams (see :attr:`StreamLifecycleRecord.is_push`).
    - Half-close only settles the direction it describes. A stream is only
      ``COMPLETED`` once both directions have ended.
    - If the observer is attached part-way through a stream, or a clock call
      raises, the affected timestamps stay ``None`` and
      :attr:`StreamLifecycleRecord.timings_complete` becomes ``False``. Clock
      failures never propagate into the protocol state machine.
    - No header fields or body content are retained: only byte counts.

    An observer belongs to a single connection: attach a *separate* observer to
    each :class:`H2Connection <h2.connection.H2Connection>` you want to watch.
    Sharing one observer between both ends of a connection would conflate their
    independent views of the same stream IDs.

    The observer is not thread-safe. ``H2Connection`` is not thread-safe
    either, so callers should keep both under the same lock.

    :param clock: A zero-argument callable returning a numeric timestamp.
        Defaults to :func:`time.monotonic`. Exceptions raised by the callable
        are swallowed and recorded on the affected stream.
    :param on_settled: An optional callable invoked exactly once when a stream
        settles (completes or is reset). Exceptions raised by it are swallowed
        so that observation can never disturb the protocol state machine.
    :param max_records: An optional upper bound on retained records. When
        exceeded, the oldest settled records are discarded first. Unsettled
        records are never discarded.

    .. versionadded:: 4.5.0
    """

    def __init__(self,
                 clock: Callable[[], float] | None = None,
                 on_settled: Callable[[StreamLifecycleRecord], None] | None = None,
                 max_records: int | None = None) -> None:
        if max_records is not None and max_records <= 0:
            msg = "max_records must be a positive integer or None"
            raise ValueError(msg)
        self._clock = clock or time.monotonic
        self._on_settled = on_settled
        self._max_records = max_records
        self._records: dict[int, StreamLifecycleRecord] = {}

    @property
    def records(self) -> dict[int, StreamLifecycleRecord]:
        """
        A shallow copy of the currently retained records, keyed by stream ID.
        """
        return dict(self._records)

    def get(self, stream_id: int) -> StreamLifecycleRecord | None:
        """
        Return the record for ``stream_id``, or ``None`` if it is unknown.
        """
        return self._records.get(stream_id)

    def stream_opened(self,
                      stream_id: int,
                      *,
                      origin: StreamOrigin | None = None,
                      is_push: bool = False,
                      local_closed: bool = False,
                      remote_closed: bool = False) -> StreamLifecycleRecord:
        """
        Record that a stream has been opened.

        :param stream_id: The stream that was opened.
        :param origin: Whether this endpoint or the peer initiated the stream.
            ``None`` means unknown.
        :param is_push: Whether this is a server push stream.
        :param local_closed: Whether the local direction is already closed when
            the stream opens (for example a client request sent with
            ``END_STREAM``).
        :param remote_closed: Whether the remote direction is already closed
            when the stream opens (for example the response side of a locally
            pushed stream).
        """
        record = self._records.get(stream_id)
        if record is None:
            record = StreamLifecycleRecord(stream_id=stream_id)
            self._records[stream_id] = record
        record.origin = origin if record.origin is None else record.origin
        record.is_push = record.is_push or is_push
        record._opened = True
        if record.initiated_at is None:
            record.initiated_at = self._timestamp(record)
        if local_closed:
            self._close_local(record)
        if remote_closed:
            self._close_remote(record)
        self._trim()
        return record

    def stream_closed_locally(self, stream_id: int) -> None:
        """
        Record that this endpoint has finished sending on ``stream_id``.

        Only the local direction is settled: the stream is not complete until
        the peer also finishes. An unknown stream gets an incomplete record so
        that late observers still see the direction change.
        """
        self._close_local(self._ensure(stream_id))

    def stream_closed_remotely(self, stream_id: int) -> None:
        """
        Record that the peer has finished sending on ``stream_id``.

        Only the remote direction is settled: the stream is not complete until
        this endpoint also finishes.
        """
        self._close_remote(self._ensure(stream_id))

    def stream_reset(self,
                     stream_id: int,
                     *,
                     error_code: int | None = None,
                     remote_reset: bool = False) -> None:
        """
        Record that ``stream_id`` was reset.

        Reset settles a stream exactly once. Further lifecycle events for an
        already-settled stream are ignored, so a late frame cannot flip a
        completed stream into a reset (or vice versa).
        """
        record = self._ensure(stream_id)
        if record.settled:
            return
        record._reset = True
        record.reset_error_code = error_code
        record.reset_by_remote = remote_reset
        record.reset_at = self._timestamp(record)
        self._settle(record)

    def process_event(self, event: Event) -> None:
        """
        Update the observer from an event produced by
        :meth:`H2Connection.receive_data
        <h2.connection.H2Connection.receive_data>`.

        This is only needed when driving the observer manually. When the
        observer is attached through
        :class:`H2Configuration <h2.config.H2Configuration>` the connection
        feeds received events to it automatically.
        """
        if isinstance(event, InformationalResponseReceived):
            record = self._ensure(event.stream_id)
            record.informational_responses += 1
        elif isinstance(event, ResponseReceived):
            record = self._ensure(event.stream_id)
            record.response_headers_count += 1
            if record.response_headers_at is None:
                record.response_headers_at = self._timestamp(record)
        elif isinstance(event, DataReceived):
            record = self._ensure(event.stream_id)
            record.flow_controlled_bytes += event.flow_controlled_length
            if event.data:
                record.body_chunks += 1
                record.body_bytes += len(event.data)
                if record.first_body_at is None:
                    record.first_body_at = self._timestamp(record)
        elif isinstance(event, StreamEnded):
            self._close_remote(self._ensure(event.stream_id))
        elif isinstance(event, StreamReset):
            self.stream_reset(
                event.stream_id,
                error_code=event.error_code,
                remote_reset=event.remote_reset,
            )
        elif isinstance(event, RequestReceived):
            # A request can only be received by the server, so a stream that
            # was not seen opening is still known to be remote-initiated. Its
            # initiation timestamp remains unknown.
            record = self._ensure(event.stream_id)
            if record.origin is None:
                record.origin = StreamOrigin.REMOTE

    def clear(self) -> None:
        """
        Discard every retained record.
        """
        self._records.clear()

    def _ensure(self, stream_id: int) -> StreamLifecycleRecord:
        record = self._records.get(stream_id)
        if record is None:
            record = StreamLifecycleRecord(stream_id=stream_id)
            self._records[stream_id] = record
            self._trim()
        return record

    def _close_local(self, record: StreamLifecycleRecord) -> None:
        if record.settled or record._local_closed:
            return
        record._local_closed = True
        record.local_closed_at = self._timestamp(record)
        self._maybe_complete(record)

    def _close_remote(self, record: StreamLifecycleRecord) -> None:
        if record.settled or record._remote_closed:
            return
        record._remote_closed = True
        record.remote_closed_at = self._timestamp(record)
        self._maybe_complete(record)

    def _maybe_complete(self, record: StreamLifecycleRecord) -> None:
        if record._local_closed and record._remote_closed:
            self._settle(record)

    def _settle(self, record: StreamLifecycleRecord) -> None:
        # Callers guarantee this runs at most once per stream: the *_closed
        # helpers guard against re-closing, and stream_reset guards on
        # ``record.settled``.
        record._settled = True
        record.settled_at = self._timestamp(record)
        if self._on_settled is not None:
            # Observation must never change protocol behaviour, so a failing
            # listener is deliberately swallowed.
            with contextlib.suppress(Exception):
                self._on_settled(record)

    def _timestamp(self, record: StreamLifecycleRecord) -> float | None:
        with contextlib.suppress(Exception):
            return float(self._clock())
        record.clock_failed = True
        return None

    def _trim(self) -> None:
        if self._max_records is None:
            return
        while len(self._records) > self._max_records:
            victim = next(
                (sid for sid, record in self._records.items() if record.settled),
                None,
            )
            if victim is None:
                # Only unsettled records are left; never evict them.
                return
            del self._records[victim]


def _elapsed(start: float | None, end: float | None) -> float | None:
    if start is None or end is None:
        return None
    return end - start


def _bytes_representation(data: bytes | None) -> str | None:
    """
    Converts a bytestring into something that is safe to print on all Python
    platforms.

    This function is relatively expensive, so it should not be called on the
    mainline of the code. It's safe to use in things like object repr methods
    though.
    """
    if data is None:
        return None

    return binascii.hexlify(data).decode("ascii")
