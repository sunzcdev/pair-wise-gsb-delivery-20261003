"""
h2/connect
~~~~~~~~~~

Explicit handshake helper state for the extended CONNECT method
(`RFC 8441 <https://www.rfc-editor.org/rfc/rfc8441>`_).

An extended CONNECT request (for example an HTTP/2 WebSocket tunnel) is only
valid when *both* of the following hold:

1. The ``SETTINGS_ENABLE_CONNECT_PROTOCOL`` capability has been negotiated.
   A client may only open such a stream when the *remote* server advertised
   the setting with a value of ``1``; a server may only accept one when it
   advertised the setting itself.
2. The header block carries the correct pseudo-header combination:
   ``:method`` is ``CONNECT`` and a ``:protocol`` pseudo-header field is
   present, together with the ordinary ``:scheme``/``:path``/``:authority``
   tuple.  The pseudo-header validation itself stays in
   :mod:`h2.utilities`; this module never re-implements or bypasses it.

This module is an *optional* convenience layer.  It tracks per-stream
handshake state by consuming the real :mod:`h2.events` objects produced by
:meth:`H2Connection.receive_data <h2.connection.H2Connection.receive_data>`
and by delegating every wire operation to a live :class:`~h2.connection.H2Connection`.

Deliberately out of scope:

* It does not perform any WebSocket frame encoding/decoding.  Once the tunnel
  is established the payload is exchanged with the ordinary
  :meth:`send_data <h2.connection.H2Connection.send_data>` /
  :class:`DataReceived <h2.events.DataReceived>` machinery.
* It does not loosen or replace any header validation performed by h2.
* A stream reset (``RST_STREAM``) and a one-sided ``END_STREAM`` are tracked
  separately: neither one implies that the other endpoint has finished
  sending.
"""

from __future__ import annotations

from enum import Enum
from typing import TYPE_CHECKING

from .events import (
    DataReceived,
    InformationalResponseReceived,
    RequestReceived,
    ResponseReceived,
    StreamReset,
    TrailersReceived,
)
from .exceptions import ProtocolError
from .settings import SettingCodes
from .utilities import (
    HeaderValidationFlags,
    extract_method_header,
    validate_outbound_headers,
)

if TYPE_CHECKING:  # pragma: no cover
    from collections.abc import Iterable, Sequence

    from hpack.struct import HeaderWeaklyTyped

    from .connection import H2Connection
    from .errors import ErrorCodes


class ExtendedConnectState(Enum):
    """
    Lifecycle states of an extended CONNECT handshake on a single stream.

    .. note::
        These states describe the *handshake* (the CONNECT request/response
        exchange), not the underlying HTTP/2 stream state machine.  A tunnel
        can remain open in both directions long after reaching
        :data:`TUNNEL_READY`.
    """

    #: No extended CONNECT request has been observed/sent on this stream yet.
    IDLE = "idle"

    #: The request is in flight: the client has sent (or, on the server, has
    #: received) the extended CONNECT headers, but no final response has been
    #: exchanged.
    REQUEST_PENDING = "request_pending"

    #: The server accepted the tunnel with a successful (``2xx``) response.
    TUNNEL_READY = "tunnel_ready"

    #: The server returned a non-``2xx`` response.  The stream reverts to
    #: ordinary HTTP response/body semantics and is *not* a tunnel.
    REJECTED = "rejected"

    #: The stream was terminated by a ``RST_STREAM`` frame from either side.
    RESET = "reset"


#: Header name of the pseudo-header field that turns an ordinary CONNECT into
#: an extended CONNECT request.
PROTOCOL_PSEUDO_HEADER = b":protocol"


class ExtendedConnectHelper:
    """
    Per-stream helper that makes the extended CONNECT handshake explicit.

    One instance tracks exactly one stream on one
    :class:`~h2.connection.H2Connection`.  The connection still owns all wire
    behaviour and all validation; this object merely:

    * answers *can I open an extended CONNECT stream right now?*
      (:meth:`can_initiate`),
    * opens the stream through the connection (:meth:`initiate`),
    * consumes the real h2 events returned by
      :meth:`receive_data <h2.connection.H2Connection.receive_data>` to decide
      whether the tunnel was accepted (:meth:`process_events`),
    * records, independently for each endpoint, who has finished sending via
      :attr:`local_end_stream` / :attr:`remote_end_stream`, and who reset via
      :attr:`reset_by_remote`.

    The helper is role-aware: on a client connection it drives the request and
    observes the response; on a server connection it observes the request and
    drives the response.

    :param connection: The live h2 connection this stream belongs to.
    :type connection: :class:`h2.connection.H2Connection`

    :param stream_id: The stream ID used for the extended CONNECT exchange.
    :type stream_id: ``int``
    """

    def __init__(self, connection: H2Connection, stream_id: int) -> None:
        self._connection = connection
        self.stream_id = stream_id

        self._state = ExtendedConnectState.IDLE

        # Independent half-close tracking. A reset and a one-sided END_STREAM
        # must never be conflated, and one side ending does not imply the other
        # side ended.
        self._local_end_stream = False
        self._remote_end_stream = False
        self._reset_by_remote: bool | None = None

    def __repr__(self) -> str:
        return f"<{type(self).__name__} stream_id:{self.stream_id} state:{self._state.value!r}>"

    # ------------------------------------------------------------------
    # Static, allocation-free classification helpers.
    # ------------------------------------------------------------------
    @staticmethod
    def is_extended_connect_request(headers: Iterable[HeaderWeaklyTyped]) -> bool:
        """
        Return ``True`` if ``headers`` are an extended CONNECT request.

        The definition used is exactly the one enforced by h2's header
        validation (`RFC 8441 §4 <https://www.rfc-editor.org/rfc/rfc8441#section-4>`_):
        ``:method`` is ``CONNECT`` *and* a ``:protocol`` pseudo-header field is
        present.  An ordinary CONNECT (no ``:protocol``) therefore returns
        ``False`` and is never mistaken for an extended request.

        Both ``str`` and ``bytes`` header names/values are accepted.
        """
        method = extract_method_header(headers)
        if method != b"CONNECT":
            return False
        return any((name.encode("utf-8") if isinstance(name, str) else name) == PROTOCOL_PSEUDO_HEADER for name, _value in headers)

    @staticmethod
    def _validate_extended_connect_headers(headers: Iterable[HeaderWeaklyTyped]) -> None:
        """
        Run the *existing* outbound header validation against an extended
        CONNECT header block, without bypassing any of h2's rules.
        """
        bytes_headers = [
            (
                name if isinstance(name, bytes) else name.encode("ascii"),
                value if isinstance(value, bytes) else value.encode("ascii"),
            )
            for name, value in headers
        ]
        flags = HeaderValidationFlags(
            is_client=True,
            is_trailer=False,
            is_response_header=False,
            is_push_promise=False,
        )
        # The validation pipeline is generator based; exhaust it so that any
        # ProtocolError is raised here and now.
        list(validate_outbound_headers(bytes_headers, flags))

    # ------------------------------------------------------------------
    # Read-only state surface.
    # ------------------------------------------------------------------
    @property
    def state(self) -> ExtendedConnectState:
        """The current :class:`ExtendedConnectState` of the handshake."""
        return self._state

    @property
    def tunnel_accepted(self) -> bool:
        """
        Whether the tunnel has been accepted (a successful ``2xx`` response
        has been observed).
        """
        return self._state == ExtendedConnectState.TUNNEL_READY

    @property
    def local_end_stream(self) -> bool:
        """
        Whether *this* endpoint has finished sending on the stream, either by
        sending an ``END_STREAM`` frame or by resetting.
        """
        return self._local_end_stream

    @property
    def remote_end_stream(self) -> bool:
        """
        Whether the *remote* endpoint has finished sending, either by sending
        an ``END_STREAM`` frame or by resetting.
        """
        return self._remote_end_stream

    @property
    def reset_by_remote(self) -> bool | None:
        """
        ``True`` if the remote peer sent the ``RST_STREAM``, ``False`` if the
        local peer reset, or ``None`` if the stream has not been reset.
        """
        return self._reset_by_remote

    @property
    def is_reset(self) -> bool:
        """Whether the stream was terminated by a ``RST_STREAM`` frame."""
        return self._state == ExtendedConnectState.RESET

    @property
    def both_ended(self) -> bool:
        """
        Whether *both* endpoints have finished sending.  This is only ever
        true after two independent half-close observations; a single
        ``END_STREAM`` or a reset on one side is insufficient.
        """
        return self._local_end_stream and self._remote_end_stream

    # ------------------------------------------------------------------
    # Client side: capability gating + opening the stream.
    # ------------------------------------------------------------------
    def can_initiate(self, headers: Sequence[HeaderWeaklyTyped] | None = None) -> bool:
        """
        Whether this client may open an extended CONNECT stream now.

        Capability negotiation alone is required and, when ``headers`` are
        supplied, the pseudo-header combination is checked as well.  This never
        sends anything: use :meth:`initiate` to actually open the stream.

        :param headers: Optional extended CONNECT request headers. When given,
            they are run through the normal h2 outbound validation, so an
            ordinary CONNECT or an incomplete pseudo-header tuple makes this
            return ``False`` (or raise, exactly as
            :meth:`H2Connection.send_headers
            <h2.connection.H2Connection.send_headers>` would).
        :returns: ``True`` if the negotiated capability (and, when provided,
            the headers) permit an extended CONNECT request.
        """
        if not self._connection.config.client_side:
            return False
        if self._state != ExtendedConnectState.IDLE:
            return False
        # The client needs the *server* to have advertised the capability.
        if self._connection.remote_settings.get(SettingCodes.ENABLE_CONNECT_PROTOCOL, 0) != 1:
            return False
        if self.stream_id in self._connection.streams:
            return False
        if headers is not None:
            if not self.is_extended_connect_request(headers):
                return False
            self._validate_extended_connect_headers(headers)
        return True

    def initiate(
        self,
        headers: Sequence[HeaderWeaklyTyped],
        *,
        end_stream: bool = False,
    ) -> None:
        """
        Open the extended CONNECT stream through the underlying connection.

        The connection performs every check (stream state, flow control book
        keeping, and the full outbound header validation); this helper only
        adds the ``SETTINGS_ENABLE_CONNECT_PROTOCOL`` negotiation precondition
        and records the pending handshake.

        :param headers: The extended CONNECT request headers. They must carry
            ``:method=CONNECT`` plus ``:protocol`` and the regular
            ``:scheme``/``:path``/``:authority`` tuple.
        :param end_stream: Forwarded to
            :meth:`H2Connection.send_headers
            <h2.connection.H2Connection.send_headers>`.
        :raises h2.exceptions.ProtocolError: If the remote peer did not
            negotiate the extended CONNECT capability, the stream was already
            used, or the headers are not a valid extended CONNECT request.
        """
        if not self._connection.config.client_side:
            msg = "Only a client connection can initiate an extended CONNECT request."
            raise ProtocolError(msg)
        if self._state != ExtendedConnectState.IDLE:
            msg = f"Cannot initiate an extended CONNECT in state {self._state!r}"
            raise ProtocolError(msg)
        if self._connection.remote_settings.get(SettingCodes.ENABLE_CONNECT_PROTOCOL, 0) != 1:
            msg = "Peer has not enabled SETTINGS_ENABLE_CONNECT_PROTOCOL; an extended CONNECT request cannot be sent."
            raise ProtocolError(msg)
        if self.stream_id in self._connection.streams:
            msg = f"Stream {self.stream_id} has already been opened."
            raise ProtocolError(msg)
        if not self.is_extended_connect_request(headers):
            msg = "Headers are not an extended CONNECT request (missing :method CONNECT/:protocol)."
            raise ProtocolError(msg)
        # Let h2 itself validate the pseudo-header combination and all other
        # header rules before anything goes on the wire.
        self._validate_extended_connect_headers(headers)

        self._connection.send_headers(self.stream_id, headers, end_stream=end_stream)
        self._state = ExtendedConnectState.REQUEST_PENDING
        if end_stream:
            self._local_end_stream = True

    # ------------------------------------------------------------------
    # Event-driven state transitions. Feed this the output of receive_data().
    # ------------------------------------------------------------------
    def process_events(self, events: Iterable[object]) -> None:
        """
        Update the handshake state from real h2 events.

        Pass in (a subset of) the event list returned by
        :meth:`H2Connection.receive_data
        <h2.connection.H2Connection.receive_data>`.  Events belonging to other
        streams are ignored.  The events are never synthesized: this helper
        only observes what the connection genuinely produced.
        """
        for event in events:
            if not hasattr(event, "stream_id") or event.stream_id != self.stream_id:
                continue
            self._process_single_event(event)

    def _process_single_event(self, event: object) -> None:
        if isinstance(event, RequestReceived):
            self._on_request_received(event)
            return

        if isinstance(event, StreamReset):
            # A reset is meaningful even if the handshake has not produced any
            # other tracked event yet, but only once this helper owns an
            # extended CONNECT stream.
            if self._state != ExtendedConnectState.IDLE:
                self._on_stream_reset(event)
            return

        # Everything below only applies to a stream this helper is tracking.
        # An ordinary CONNECT (or any other non-extended stream) leaves this
        # helper in IDLE, so its DATA/END_STREAM/trailer frames are ignored.
        if self._state == ExtendedConnectState.IDLE:
            return

        if isinstance(event, InformationalResponseReceived):
            # 1xx responses neither accept nor reject the tunnel.
            return
        if isinstance(event, ResponseReceived):
            self._on_response_received(event)
        elif isinstance(event, TrailersReceived):
            # Trailers always carry END_STREAM.
            self._mark_remote_end_stream()
        elif isinstance(event, DataReceived) and event.stream_ended is not None:
            self._mark_remote_end_stream()

    def _on_request_received(self, event: RequestReceived) -> None:
        # Server side: only extended CONNECT requests are tracked. An ordinary
        # CONNECT never enters the extended handshake state machine.
        if not self.is_extended_connect_request(event.headers):
            return
        self._state = ExtendedConnectState.REQUEST_PENDING
        if event.stream_ended is not None:
            self._mark_remote_end_stream()

    def _on_response_received(self, event: ResponseReceived) -> None:
        # Client side: a successful (2xx) status accepts the tunnel, any other
        # final status rejects it and the stream reverts to ordinary HTTP
        # response/body semantics.
        status = self._status_from_headers(event.headers)
        if status is not None and 200 <= status < 300:
            self._state = ExtendedConnectState.TUNNEL_READY
        else:
            self._state = ExtendedConnectState.REJECTED
        if event.stream_ended is not None:
            self._mark_remote_end_stream()

    def _on_stream_reset(self, event: StreamReset) -> None:
        self._state = ExtendedConnectState.RESET
        if event.remote_reset:
            self._reset_by_remote = True
            self._remote_end_stream = True
        else:
            self._reset_by_remote = False
            self._local_end_stream = True

    def _mark_remote_end_stream(self) -> None:
        self._remote_end_stream = True

    @staticmethod
    def _status_from_headers(headers: Iterable[HeaderWeaklyTyped]) -> int | None:
        for name, value in headers:
            name_b = name if isinstance(name, bytes) else name.encode("ascii")
            if name_b == b":status":
                value_b = value if isinstance(value, bytes) else value.encode("ascii")
                try:
                    return int(value_b)
                except ValueError:
                    return None
        return None

    # ------------------------------------------------------------------
    # Server side: accept / reject the pending extended CONNECT.
    # ------------------------------------------------------------------
    def accept(self, headers: Sequence[HeaderWeaklyTyped] | None = None) -> None:
        """
        Accept the extended CONNECT tunnel from the server side.

        Sends a ``200`` response (merged with any extra ``headers``) through
        the connection, which applies its normal response validation.  This is
        only allowed after an extended CONNECT request has been observed and
        while the handshake is still pending.

        :param headers: Optional additional response headers (e.g. an
            ``Sec-WebSocket-Protocol`` selection).
        :raises h2.exceptions.ProtocolError: If no extended CONNECT request is
            pending, a response was already sent, or the local endpoint did not
            advertise the capability.
        """
        if self._connection.config.client_side:
            msg = "Only a server connection can accept an extended CONNECT request."
            raise ProtocolError(msg)
        if self._state != ExtendedConnectState.REQUEST_PENDING:
            msg = f"Cannot accept an extended CONNECT in state {self._state!r}"
            raise ProtocolError(msg)
        if self._connection.local_settings.get(SettingCodes.ENABLE_CONNECT_PROTOCOL, 0) != 1:
            msg = "This server did not advertise SETTINGS_ENABLE_CONNECT_PROTOCOL; it cannot accept an extended CONNECT request."
            raise ProtocolError(msg)

        response_headers: list[HeaderWeaklyTyped] = [(":status", "200")]
        if headers:
            response_headers.extend(headers)
        self._connection.send_headers(self.stream_id, response_headers)
        self._state = ExtendedConnectState.TUNNEL_READY

    def reject(self, status_code: int = 403, headers: Sequence[HeaderWeaklyTyped] | None = None) -> None:
        """
        Reject an extended CONNECT request with a non-``2xx`` response.

        The stream stays an ordinary HTTP response/body stream afterwards; it
        is not a tunnel.  Any valid non-``2xx`` status may be given.

        :param status_code: The rejection status code (defaults to ``403``).
        :param headers: Optional additional response headers.
        :raises h2.exceptions.ProtocolError: If no extended CONNECT request is
            pending, a response was already sent, or the status is ``2xx``.
        """
        if self._connection.config.client_side:
            msg = "Only a server connection can reject an extended CONNECT request."
            raise ProtocolError(msg)
        if self._state != ExtendedConnectState.REQUEST_PENDING:
            msg = f"Cannot reject an extended CONNECT in state {self._state!r}"
            raise ProtocolError(msg)
        if 200 <= status_code < 300:
            msg = "A 2xx status accepts rather than rejects an extended CONNECT request."
            raise ProtocolError(msg)

        response_headers: list[HeaderWeaklyTyped] = [(":status", str(status_code))]
        if headers:
            response_headers.extend(headers)
        self._connection.send_headers(self.stream_id, response_headers)
        self._state = ExtendedConnectState.REJECTED

    # ------------------------------------------------------------------
    # Local half-close / reset bookkeeping. These delegate to the connection
    # for the actual wire operation and merely mirror the resulting state.
    # ------------------------------------------------------------------
    def end_stream(self) -> None:
        """
        Gracefully finish *this* endpoint's sending via
        :meth:`H2Connection.end_stream
        <h2.connection.H2Connection.end_stream>`.  This does not affect the
        remote half of the stream.
        """
        self._connection.end_stream(self.stream_id)
        self._local_end_stream = True

    def send_data(self, data: bytes, *, end_stream: bool = False) -> None:
        """
        Send tunnel payload via :meth:`H2Connection.send_data
        <h2.connection.H2Connection.send_data>`.

        No WebSocket framing is applied: ``data`` is sent verbatim as an
        HTTP/2 DATA frame payload.
        """
        self._connection.send_data(self.stream_id, data, end_stream=end_stream)
        if end_stream:
            self._local_end_stream = True

    def reset_stream(self, error_code: ErrorCodes | int = 0) -> None:
        """
        Reset the stream locally via :meth:`H2Connection.reset_stream
        <h2.connection.H2Connection.reset_stream>`.

        A reset marks the *local* endpoint as finished; it does not imply the
        remote endpoint finished.
        """
        self._connection.reset_stream(self.stream_id, error_code)
        self._state = ExtendedConnectState.RESET
        self._reset_by_remote = False
        self._local_end_stream = True
