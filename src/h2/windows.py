"""
h2/windows
~~~~~~~~~~

Defines tools for managing HTTP/2 flow control windows.

The objects defined in this module are used to automatically manage HTTP/2
flow control windows. Specifically, they keep track of what the size of the
window is, how much data has been consumed from that window, and how much data
the user has already used. It then implements a basic algorithm that attempts
to manage the flow control window without user input, trying to ensure that it
does not emit too many WINDOW_UPDATE frames.
"""
from __future__ import annotations

from .exceptions import FlowControlError

# The largest acceptable value for a HTTP/2 flow control window.
LARGEST_FLOW_CONTROL_WINDOW = 2**31 - 1


class WindowManager:
    """
    A basic HTTP/2 window manager.

    :param max_window_size: The maximum size of the flow control window.
    :type max_window_size: ``int``
    """

    def __init__(self, max_window_size: int) -> None:
        assert max_window_size <= LARGEST_FLOW_CONTROL_WINDOW
        self.max_window_size = max_window_size
        self.current_window_size = max_window_size
        self._bytes_processed = 0

        # Lifetime counters used to keep the flow control accounting honest.
        #
        # ``_bytes_consumed`` counts every flow-controlled byte that has been
        # charged against this window (i.e. the ``flow_controlled_length`` of
        # every DATA frame, including any padding). On a stream window these
        # bytes have all been handed to the application in a
        # :class:`DataReceived <h2.events.DataReceived>` event.
        #
        # ``_bytes_acknowledged`` counts every flow-controlled byte the
        # application has told us it has processed through
        # :meth:`process_bytes`. The difference between the two is the number
        # of received-but-not-yet-acknowledged bytes: this is the credit that
        # must be handed back to the *connection* window when a stream is
        # terminated with RST_STREAM, otherwise that credit would be lost
        # forever (or, if acknowledged again later, returned twice).
        self._bytes_consumed = 0
        self._bytes_acknowledged = 0

        # Once a stream has been abandoned (reset), its window is taken out of
        # service. The unacknowledged bytes are settled once, and any further
        # acknowledgement must be a no-op rather than a second refund.
        self._settled = False

    @property
    def bytes_consumed(self) -> int:
        """
        The total number of flow-controlled bytes ever charged against this
        window. On a stream window these are the bytes that have been handed
        to the application (plus padding), whether or not the application has
        acknowledged them yet.
        """
        return self._bytes_consumed

    @property
    def bytes_acknowledged(self) -> int:
        """
        The total number of flow-controlled bytes that the application has
        acknowledged as processed.
        """
        return self._bytes_acknowledged

    @property
    def unacknowledged_bytes(self) -> int:
        """
        The number of flow-controlled bytes that have been charged against
        this window but which the application has not yet acknowledged as
        processed. On a stream window this is the amount of connection-level
        credit that is still "in flight" for the stream and that must be
        returned when the stream is abandoned.
        """
        return self._bytes_consumed - self._bytes_acknowledged

    def window_consumed(self, size: int) -> None:
        """
        We have received a certain number of bytes from the remote peer. This
        necessarily shrinks the flow control window!

        :param size: The number of flow controlled bytes we received from the
            remote peer.
        :type size: ``int``
        :returns: Nothing.
        :rtype: ``None``
        """
        self.current_window_size -= size
        if self.current_window_size < 0:
            msg = "Flow control window shrunk below 0"
            raise FlowControlError(msg)
        # ``size`` counts the full ``flow_controlled_length`` of the DATA
        # frame, so padding (and the one-byte padding length field) is
        # accounted for exactly like application data.
        self._bytes_consumed += size

    def window_opened(self, size: int) -> None:
        """
        The flow control window has been incremented, either because of manual
        flow control management or because of the user changing the flow
        control settings. This can have the effect of increasing what we
        consider to be the "maximum" flow control window size.

        This does not increase our view of how many bytes have been processed,
        only of how much space is in the window.

        :param size: The increment to the flow control window we received.
        :type size: ``int``
        :returns: Nothing
        :rtype: ``None``
        """
        self.current_window_size += size

        if self.current_window_size > LARGEST_FLOW_CONTROL_WINDOW:
            msg = f"Flow control window mustn't exceed {LARGEST_FLOW_CONTROL_WINDOW}"
            raise FlowControlError(msg)

        self.max_window_size = max(self.current_window_size, self.max_window_size)

    def process_bytes(self, size: int) -> int | None:
        """
        The application has informed us that it has processed a certain number
        of bytes. This may cause us to want to emit a window update frame. If
        we do want to emit a window update frame, this method will return the
        number of bytes that we should increment the window by.

        :param size: The number of flow controlled bytes that the application
            has processed.
        :type size: ``int``
        :returns: The number of bytes to increment the flow control window by,
            or ``None``.
        :rtype: ``int`` or ``None``
        """
        # A stream that has been reset has already had all of its outstanding
        # credit returned to the connection window. A late acknowledgement must
        # not move any window again: doing so would hand the same bytes back
        # twice.
        if self._settled:
            return None

        self._bytes_acknowledged += size
        self._bytes_processed += size
        return self._maybe_update_window()

    def settle(self) -> int:
        """
        Settle the flow control accounting for an abandoned stream.

        When a stream is terminated with RST_STREAM there may be flow
        controlled bytes that were already delivered to the application but
        never acknowledged. The corresponding *connection*-level credit cannot
        stay reserved forever (it would eventually stall every other stream),
        but it also cannot be returned a second time if the application later
        acknowledges the data.

        This method therefore returns the number of received-but-unacknowledged
        flow controlled bytes exactly once, and takes the window out of
        service so that subsequent calls to :meth:`process_bytes` become
        no-ops.

        The returned bytes must be handed back to the **connection** flow
        control window (a ``WINDOW_UPDATE`` frame on stream ``0``); no stream
        level ``WINDOW_UPDATE`` is ever emitted for a closed stream.

        :returns: The number of flow controlled bytes to return to the
            connection window (may be zero).
        :rtype: ``int``
        """
        if self._settled:
            return 0

        self._settled = True
        # Anything the application had already acknowledged was accounted for
        # through ``process_bytes``; only the outstanding balance remains.
        unacknowledged = self.unacknowledged_bytes
        self._bytes_processed = 0
        return unacknowledged

    def _maybe_update_window(self) -> int | None:
        """
        Run the algorithm.

        Our current algorithm can be described like this.

        1. If no bytes have been processed, we immediately return 0. There is
           no meaningful way for us to hand space in the window back to the
           remote peer, so let's not even try.
        2. If there is no space in the flow control window, and we have
           processed at least 1024 bytes (or 1/4 of the window, if the window
           is smaller), we will emit a window update frame. This is to avoid
           the risk of blocking a stream altogether.
        3. If there is space in the flow control window, and we have processed
           at least 1/2 of the window worth of bytes, we will emit a window
           update frame. This is to minimise the number of window update frames
           we have to emit.

        In a healthy system with large flow control windows, this will
        irregularly emit WINDOW_UPDATE frames. This prevents us starving the
        connection by emitting eleventy bajillion WINDOW_UPDATE frames,
        especially in situations where the remote peer is sending a lot of very
        small DATA frames.
        """
        # TODO: Can the window be smaller than 1024 bytes? If not, we can
        # streamline this algorithm.
        if not self._bytes_processed:
            return None

        max_increment = (self.max_window_size - self.current_window_size)
        increment = 0

        # Note that, even though we may increment less than _bytes_processed,
        # we still want to set it to zero whenever we emit an increment. This
        # is because we'll always increment up to the maximum we can.
        if ((self.current_window_size == 0) and (
                self._bytes_processed > min(1024, self.max_window_size // 4))) or self._bytes_processed >= (self.max_window_size // 2):
            increment = min(self._bytes_processed, max_increment)
            self._bytes_processed = 0

        self.current_window_size += increment
        return increment
