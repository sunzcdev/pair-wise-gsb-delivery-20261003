"""
h2/settings
~~~~~~~~~~~

This module contains a HTTP/2 settings object. This object provides a simple
API for manipulating HTTP/2 settings, keeping track of both the current active
state of the settings and the unacknowledged future values of the settings.
"""
from __future__ import annotations

import collections
import enum
from collections.abc import Iterator, Mapping, MutableMapping
from types import MappingProxyType

from hyperframe.frame import SettingsFrame

from .errors import ErrorCodes
from .exceptions import InvalidSettingsValueError


class SettingCodes(enum.IntEnum):
    """
    All known HTTP/2 setting codes.

    .. versionadded:: 2.6.0
    """

    #: Allows the sender to inform the remote endpoint of the maximum size of
    #: the header compression table used to decode header blocks, in octets.
    HEADER_TABLE_SIZE = SettingsFrame.HEADER_TABLE_SIZE

    #: This setting can be used to disable server push. To disable server push
    #: on a client, set this to 0.
    ENABLE_PUSH = SettingsFrame.ENABLE_PUSH

    #: Indicates the maximum number of concurrent streams that the sender will
    #: allow.
    MAX_CONCURRENT_STREAMS = SettingsFrame.MAX_CONCURRENT_STREAMS

    #: Indicates the sender's initial window size (in octets) for stream-level
    #: flow control.
    INITIAL_WINDOW_SIZE = SettingsFrame.INITIAL_WINDOW_SIZE

    #: Indicates the size of the largest frame payload that the sender is
    #: willing to receive, in octets.
    MAX_FRAME_SIZE = SettingsFrame.MAX_FRAME_SIZE

    #: This advisory setting informs a peer of the maximum size of header list
    #: that the sender is prepared to accept, in octets.  The value is based on
    #: the uncompressed size of header fields, including the length of the name
    #: and value in octets plus an overhead of 32 octets for each header field.
    MAX_HEADER_LIST_SIZE = SettingsFrame.MAX_HEADER_LIST_SIZE

    #: This setting can be used to enable the connect protocol. To enable on a
    #: client set this to 1.
    ENABLE_CONNECT_PROTOCOL = SettingsFrame.ENABLE_CONNECT_PROTOCOL


def _setting_code_from_int(code: int) -> SettingCodes | int:
    """
    Given an integer setting code, returns either one of :class:`SettingCodes
    <h2.settings.SettingCodes>` or, if not present in the known set of codes,
    returns the integer directly.
    """
    try:
        return SettingCodes(code)
    except ValueError:
        return code


class ChangedSetting:

    def __init__(self, setting: SettingCodes | int, original_value: int | None, new_value: int) -> None:
        #: The setting code given. Either one of :class:`SettingCodes
        #: <h2.settings.SettingCodes>` or ``int``
        #:
        #: .. versionchanged:: 2.6.0
        self.setting = setting

        #: The original value before being changed.
        self.original_value = original_value

        #: The new value after being changed.
        self.new_value = new_value

    def __repr__(self) -> str:
        return (
            f"ChangedSetting(setting={self.setting!s}, original_value={self.original_value}, new_value={self.new_value})"
        )


class SettingsBatchState(enum.Enum):
    """
    The lifecycle state of a locally sent SETTINGS batch.

    A batch is created whenever this peer emits a SETTINGS frame carrying
    local updates. Until the remote peer acknowledges that frame the batch is
    :data:`PENDING`; afterwards it is :data:`ACKNOWLEDGED`. If the connection
    is closed before the acknowledgement arrives the batch is
    :data:`ABANDONED` and can never be acknowledged retroactively.
    """

    #: The batch has been sent but the remote peer has not acknowledged it.
    PENDING = "pending"

    #: The remote peer acknowledged this batch, so its values are in force.
    ACKNOWLEDGED = "acknowledged"

    #: The connection closed before this batch was acknowledged.
    ABANDONED = "abandoned"


class SettingsBatch:
    """
    An immutable snapshot of a single locally sent SETTINGS batch.

    A batch records the identity of the SETTINGS frame that carried it, the
    values that were sent, and (once settled) which acknowledgement confirmed
    them. It deliberately exposes no mutators: the owning
    :class:`SettingsLedger` replaces snapshots as the batch is settled.
    """

    __slots__ = ("_batch_id", "_settings", "_state", "_ack_id", "_changed_settings")

    def __init__(
        self,
        batch_id: int,
        settings: Mapping[SettingCodes | int, int],
        *,
        state: SettingsBatchState = SettingsBatchState.PENDING,
        ack_id: int | None = None,
        changed_settings: Mapping[SettingCodes | int, ChangedSetting] | None = None,
    ) -> None:
        #: Monotonically increasing identity of the batch, unique per ledger.
        self._batch_id = batch_id

        #: The values that were sent in this batch (the local *desired* values).
        self._settings: Mapping[SettingCodes | int, int] = MappingProxyType(dict(settings))

        self._state = state
        self._ack_id = ack_id
        self._changed_settings: Mapping[SettingCodes | int, ChangedSetting] | None = (
            None if changed_settings is None
            else MappingProxyType(dict(changed_settings))
        )

    @property
    def batch_id(self) -> int:
        """
        The identity of this batch in the ledger that owns it.
        """
        return self._batch_id

    @property
    def settings(self) -> Mapping[SettingCodes | int, int]:
        """
        A read-only mapping of the values that were sent in this batch.
        """
        return self._settings

    @property
    def state(self) -> SettingsBatchState:
        """
        The current settlement state of the batch.
        """
        return self._state

    @property
    def acknowledged(self) -> bool:
        """
        Whether the remote peer acknowledged this batch.
        """
        return self._state is SettingsBatchState.ACKNOWLEDGED

    @property
    def ack_id(self) -> int | None:
        """
        The identity of the acknowledgement that settled this batch, if any.

        Acknowledgements carry no identity on the wire, so the ledger assigns
        each received ACK a monotonically increasing number and records it
        here to make the batch/ACK correspondence observable.
        """
        return self._ack_id

    @property
    def changed_settings(self) -> Mapping[SettingCodes | int, ChangedSetting] | None:
        """
        The settings that became effective when this batch was acknowledged.
        """
        return self._changed_settings

    def __repr__(self) -> str:
        return (
            f"SettingsBatch(batch_id={self._batch_id}, state={self._state.value!r}, "
            f"settings={dict(self._settings)!r})"
        )


class SettingsLedger:
    """
    A read-only ledger of the local SETTINGS batches sent on a connection.

    The ledger answers, batch by batch, which local modifications have been
    confirmed by the remote peer. It records:

    * the batch identity (:attr:`SettingsBatch.batch_id`),
    * the values that were sent (:attr:`SettingsBatch.settings`), and
    * the acknowledgement that settled the batch
      (:attr:`SettingsBatch.ack_id` / :attr:`SettingsBatch.changed_settings`).

    The mapping between *desired* and *effective* values is exposed by
    :class:`Settings` itself: :attr:`Settings.desired_values` reports what the
    local settings will become once all outstanding batches are acknowledged,
    while :meth:`Settings.__getitem__` reports the currently effective value.

    .. note::
       Peer settings never appear here. A ledger belongs to one
       :class:`Settings` object, so the local ledger only ever contains
       batches that this peer sent; :class:`Settings` objects that mirror the
       peer's settings have their own, distinct ledger.
    """

    def __init__(self) -> None:
        self._batches: list[SettingsBatch] = []
        self._next_batch_id = 0
        self._next_ack_id = 0
        self._closed = False
        self._anomalies: list[str] = []

    @property
    def batches(self) -> tuple[SettingsBatch, ...]:
        """
        All batches sent so far, in send order. Read-only.
        """
        return tuple(self._batches)

    @property
    def pending(self) -> tuple[SettingsBatch, ...]:
        """
        The batches that have been sent but not yet acknowledged. Read-only.
        """
        return tuple(
            b for b in self._batches if b.state is SettingsBatchState.PENDING
        )

    @property
    def acknowledged(self) -> tuple[SettingsBatch, ...]:
        """
        The batches that the remote peer has acknowledged. Read-only.
        """
        return tuple(
            b for b in self._batches if b.state is SettingsBatchState.ACKNOWLEDGED
        )

    @property
    def abandoned(self) -> tuple[SettingsBatch, ...]:
        """
        The batches that were still outstanding when the connection closed.
        Read-only.
        """
        return tuple(
            b for b in self._batches if b.state is SettingsBatchState.ABANDONED
        )

    @property
    def closed(self) -> bool:
        """
        Whether the connection closed and the ledger stopped accepting ACKs.
        """
        return self._closed

    @property
    def anomalies(self) -> tuple[str, ...]:
        """
        Descriptions of acknowledgements that could not be matched to a batch.

        An acknowledgement in this list never advanced a batch: it arrived
        with no outstanding local batch (or after the connection closed), so
        it must not settle the next batch.
        """
        return tuple(self._anomalies)

    @property
    def next_batch_id(self) -> int:
        """
        The identity that will be assigned to the next recorded batch.
        """
        return self._next_batch_id

    def batch(self, batch_id: int) -> SettingsBatch | None:
        """
        Return the batch with the given identity, if it exists.
        """
        for candidate in self._batches:
            if candidate.batch_id == batch_id:
                return candidate
        return None

    def __len__(self) -> int:
        return len(self._batches)

    def __iter__(self) -> Iterator[SettingsBatch]:
        return iter(tuple(self._batches))

    def __repr__(self) -> str:
        return (
            f"SettingsLedger(batches={len(self._batches)}, "
            f"pending={len(self.pending)}, closed={self._closed}, "
            f"anomalies={len(self._anomalies)})"
        )

    # Internal mutators. These are not part of the read-only surface exposed to
    # callers; they are driven by Settings as frames are sent and settled.

    def _record(self, settings: Mapping[SettingCodes | int, int]) -> SettingsBatch:
        """
        Record a newly sent batch of local settings values.
        """
        batch = SettingsBatch(self._next_batch_id, settings)
        self._next_batch_id += 1
        self._batches.append(batch)
        return batch

    def _oldest_pending_index(self) -> int | None:
        for index, batch in enumerate(self._batches):
            if batch.state is SettingsBatchState.PENDING:
                return index
        return None

    def _settle(
        self,
        index: int,
        changed_settings: Mapping[SettingCodes | int, ChangedSetting],
        ack_id: int,
    ) -> SettingsBatch:
        """
        Mark the batch at ``index`` as acknowledged by the given ACK.
        """
        batch = self._batches[index]
        settled = SettingsBatch(
            batch.batch_id,
            batch.settings,
            state=SettingsBatchState.ACKNOWLEDGED,
            ack_id=ack_id,
            changed_settings=changed_settings,
        )
        self._batches[index] = settled
        return settled

    def _allocate_ack_id(self) -> int:
        """
        Allocate the ledger-wide identity for the next acknowledgement received.
        """
        ack_id = self._next_ack_id
        self._next_ack_id += 1
        return ack_id

    def _record_anomaly(self, reason: str) -> None:
        self._anomalies.append(reason)

    def _close(self) -> None:
        """
        Close the ledger, abandoning every still-pending batch.

        Idempotent: subsequent calls do nothing.
        """
        if self._closed:
            return
        self._closed = True
        for index, batch in enumerate(self._batches):
            if batch.state is SettingsBatchState.PENDING:
                self._batches[index] = SettingsBatch(
                    batch.batch_id,
                    batch.settings,
                    state=SettingsBatchState.ABANDONED,
                )


class SettingsAckResult:
    """
    The outcome of acknowledging one incoming SETTINGS acknowledgement.

    ``batch`` is the batch this ACK settled, or ``None`` when the ACK could
    not be matched to any outstanding batch. ``anomalous`` is ``True`` for
    such unmatched acknowledgements; they are recorded on the ledger and never
    advance a later batch.

    Every acknowledgement consumes one ledger-wide :attr:`ack_id`, including
    anomalous ones, so the correspondence between acks and batches stays
    traceable even when the peer sends an unexpected ack.
    """

    __slots__ = ("_batch", "_changed_settings", "_anomalous", "_ack_id")

    def __init__(
        self,
        batch: SettingsBatch | None,
        changed_settings: dict[SettingCodes | int, ChangedSetting],
        anomalous: bool,
        ack_id: int | None = None,
    ) -> None:
        self._batch = batch
        self._changed_settings = changed_settings
        self._anomalous = anomalous
        self._ack_id = ack_id

    @property
    def batch(self) -> SettingsBatch | None:
        """
        The batch that this acknowledgement settled, if any.
        """
        return self._batch

    @property
    def ack_id(self) -> int | None:
        """
        The ledger-wide identity assigned to this acknowledgement, if any.
        """
        return self._ack_id

    @property
    def changed_settings(self) -> dict[SettingCodes | int, ChangedSetting]:
        """
        The settings that became effective as a result of this acknowledgement.
        """
        return self._changed_settings

    @property
    def anomalous(self) -> bool:
        """
        Whether the acknowledgement could not be matched to a batch.
        """
        return self._anomalous

    def __repr__(self) -> str:
        return (
            f"SettingsAckResult(batch={self._batch!r}, "
            f"changed_settings={self._changed_settings!r}, "
            f"anomalous={self._anomalous})"
        )


class Settings(MutableMapping[SettingCodes | int, int]):
    """
    An object that encapsulates HTTP/2 settings state.

    HTTP/2 Settings are a complex beast. Each party, remote and local, has its
    own settings and a view of the other party's settings. When a settings
    frame is emitted by a peer it cannot assume that the new settings values
    are in place until the remote peer acknowledges the setting. In principle,
    multiple settings changes can be "in flight" at the same time, all with
    different values.

    This object encapsulates this mess. It provides a dict-like interface to
    settings, which return the *current* values of the settings in question.
    Additionally, it keeps track of the stack of proposed values: each time an
    acknowledgement is sent/received, it updates the current values with the
    stack of proposed values. On top of all that, it validates the values to
    make sure they're allowed, and raises :class:`InvalidSettingsValueError
    <h2.exceptions.InvalidSettingsValueError>` if they are not.

    Finally, this object understands what the default values of the HTTP/2
    settings are, and sets those defaults appropriately.

    .. versionchanged:: 2.2.0
       Added the ``initial_values`` parameter.

    .. versionchanged:: 2.5.0
       Added the ``max_header_list_size`` property.

    .. versionchanged:: 5.0.0
       Added the read-only update ledger (:attr:`Settings.ledger`) together
       with :attr:`Settings.desired_values` and
       :attr:`Settings.effective_values`.

    :param client: (optional) Whether these settings should be defaulted for a
        client implementation or a server implementation. Defaults to ``True``.
    :type client: ``bool``
    :param initial_values: (optional) Any initial values the user would like
        set, rather than RFC 7540's defaults.
    :type initial_vales: ``MutableMapping``
    """

    def __init__(self, client: bool = True, initial_values: dict[SettingCodes, int] | None = None) -> None:
        self._client = client

        # Backing object for the settings. This is a dictionary of
        # (setting: [list of values]), where the first value in the list is the
        # current value of the setting. Strictly this doesn't use lists but
        # instead uses collections.deque to avoid repeated memory allocations.
        #
        # This contains the default values for HTTP/2.
        self._settings: dict[SettingCodes | int, collections.deque[int]] = {
            SettingCodes.HEADER_TABLE_SIZE: collections.deque([4096]),
            SettingCodes.ENABLE_PUSH: collections.deque([int(client)]),
            SettingCodes.INITIAL_WINDOW_SIZE: collections.deque([65535]),
            SettingCodes.MAX_FRAME_SIZE: collections.deque([16384]),
            SettingCodes.ENABLE_CONNECT_PROTOCOL: collections.deque([0]),
        }
        if initial_values is not None:
            for key, value in initial_values.items():
                invalid = _validate_setting(key, value)
                if invalid:
                    msg = f"Setting {key} has invalid value {value}"
                    raise InvalidSettingsValueError(
                        msg,
                        error_code=invalid,
                    )
                self._settings[key] = collections.deque([value])

        # The ledger of local SETTINGS update batches. Each Settings object has
        # its own ledger, so batches sent by one peer can never be mixed up with
        # the settings this object holds for the other peer.
        self._ledger = SettingsLedger()

    @property
    def ledger(self) -> SettingsLedger:
        """
        The read-only ledger of local SETTINGS update batches.

        Each call to :meth:`H2Connection.update_settings
        <h2.connection.H2Connection.update_settings>` records one batch here,
        and each acknowledgement received from the remote peer settles at most
        one batch. The ledger exposes batch identity, the sent values and the
        acknowledgement that confirmed them, without ever changing which values
        are currently effective.

        Settings held for the *remote* peer have a separate, unrelated ledger:
        the peer's settings never appear in the local ledger.

        .. versionadded:: 5.0.0
        """
        return self._ledger

    @property
    def effective_values(self) -> Mapping[SettingCodes | int, int]:
        """
        A read-only snapshot of the currently effective local values.

        This is the same data :meth:`__getitem__` exposes, presented as a
        mapping. Values from batches the remote peer has not yet acknowledged
        are excluded: reading this property never applies a change early.

        .. versionadded:: 5.0.0
        """
        return MappingProxyType({
            key: values[0]
            for key, values in self._settings.items()
            if values[0] is not None
        })

    @property
    def desired_values(self) -> Mapping[SettingCodes | int, int]:
        """
        A read-only snapshot of the local values once all changes are applied.

        This includes values that have been staged locally but not yet
        acknowledged by the remote peer, so it differs from
        :attr:`effective_values` whenever a change is outstanding. Reading it
        does not apply anything: window sizes and other side effects still only
        change at their normal acknowledgement points.

        .. versionadded:: 5.0.0
        """
        # The last value in each deque is always a real staged value, so no
        # placeholder filtering is needed here.
        return MappingProxyType({
            key: values[-1]
            for key, values in self._settings.items()
        })

    def record_batch(self, settings: Mapping[SettingCodes | int, int]) -> SettingsBatch:
        """
        Record a batch of local updates that has just been sent to the peer.

        This is called by :meth:`H2Connection.update_settings
        <h2.connection.H2Connection.update_settings>` when it emits a SETTINGS
        frame. The batch is recorded as pending until the matching
        acknowledgement arrives.

        .. versionadded:: 5.0.0

        :param settings: The ``{setting: value}`` mapping that was sent.
        :returns: The newly recorded :class:`SettingsBatch`.
        """
        return self._ledger._record(settings)

    def validate_local_settings(self, settings: Mapping[SettingCodes | int, int]) -> None:
        """
        Validate a set of local updates before any of them is staged.

        This lets callers reject an invalid batch atomically: if any value is
        invalid, :class:`InvalidSettingsValueError
        <h2.exceptions.InvalidSettingsValueError>` is raised before the
        settings stack or ledger is modified.

        .. versionadded:: 5.0.0
        """
        for key, value in settings.items():
            invalid = _validate_setting(key, value)
            if invalid:
                msg = f"Setting {key} has invalid value {value}"
                raise InvalidSettingsValueError(
                    msg,
                    error_code=invalid,
                )

    def acknowledge_local(self) -> SettingsAckResult:
        """
        Settle one local update batch in response to a SETTINGS ACK.

        The oldest pending batch is settled. If no batch is pending, the ACK is
        treated as anomalous and recorded on the ledger: it never advances a
        later batch. Acknowledgement that arrives after the connection closed
        is likewise anomalous, because its batch is already abandoned.

        The one exception is a connection that has not recorded any update
        batch yet, where an ACK belongs to the initial connection SETTINGS;
        that ACK keeps the legacy behaviour and settles nothing.

        This method also moves the effective values forward, exactly as
        :meth:`acknowledge` does, so window sizes and other side effects still
        take place at the acknowledgement point rather than when the batch was
        recorded.

        .. versionadded:: 5.0.0

        :returns: A :class:`SettingsAckResult` describing what was settled.
        """
        if self._ledger.closed:
            ack_id = self._ledger._allocate_ack_id()
            self._ledger._record_anomaly(
                "SETTINGS ACK received after the connection closed",
            )
            return SettingsAckResult(None, {}, anomalous=True, ack_id=ack_id)

        index = self._ledger._oldest_pending_index()

        if index is None:
            if len(self._ledger) == 0:
                # No local update batch has been sent yet: this ACK belongs to
                # the initial connection SETTINGS frame. Keep the historical
                # behaviour for any values staged directly on the object, and
                # leave the ack unnumbered because no batch was involved.
                return SettingsAckResult(None, self._drain_values(None), anomalous=False)

            ack_id = self._ledger._allocate_ack_id()
            self._ledger._record_anomaly(
                "SETTINGS ACK received with no locally sent update batch outstanding",
            )
            return SettingsAckResult(None, {}, anomalous=True, ack_id=ack_id)

        batch = self._ledger._batches[index]
        changed_settings = self._drain_values(set(batch.settings))
        ack_id = self._ledger._allocate_ack_id()
        settled = self._ledger._settle(index, changed_settings, ack_id)
        return SettingsAckResult(settled, changed_settings, anomalous=False, ack_id=ack_id)

    def close_ledger(self) -> None:
        """
        Settle the ledger as closed, abandoning still-pending batches.

        Called when the connection closes, whether locally or by the peer. Any
        batch that was never acknowledged can no longer become effective, so it
        is marked :data:`SettingsBatchState.ABANDONED` rather than being
        silently reported as acknowledged.

        .. versionadded:: 5.0.0
        """
        self._ledger._close()

    def _drain_values(
        self,
        keys: set[SettingCodes | int] | None,
    ) -> dict[SettingCodes | int, ChangedSetting]:
        """
        Move the effective values forward by one step.

        When ``keys`` is ``None`` every setting with an outstanding proposed
        value is advanced, matching the historical :meth:`acknowledge`
        behaviour. Otherwise only the given keys are advanced, so a batch only
        applies the settings it actually sent.
        """
        changed_settings: dict[SettingCodes | int, ChangedSetting] = {}

        for key, values in self._settings.items():
            if keys is not None and key not in keys:
                continue
            if len(values) > 1:
                old_setting = values.popleft()
                new_setting = values[0]
                changed_settings[key] = ChangedSetting(
                    key, old_setting, new_setting,
                )

        return changed_settings

    def acknowledge(self) -> dict[SettingCodes | int, ChangedSetting]:
        """
        The settings have been acknowledged, either by the user (remote
        settings) or by the remote peer (local settings).

        This is the historical, unfiltered acknowledgement entry point: it
        advances every setting that has an outstanding proposed value and does
        not touch the local update ledger. Local connections should use
        :meth:`acknowledge_local` instead, which settles exactly one batch so
        the ledger and the effective values stay in step.

        :returns: A dict of {setting: ChangedSetting} that were applied.
        """
        return self._drain_values(None)

    # Provide easy-access to well known settings.
    @property
    def header_table_size(self) -> int:
        """
        The current value of the :data:`HEADER_TABLE_SIZE
        <h2.settings.SettingCodes.HEADER_TABLE_SIZE>` setting.
        """
        return self[SettingCodes.HEADER_TABLE_SIZE]

    @header_table_size.setter
    def header_table_size(self, value: int) -> None:
        self[SettingCodes.HEADER_TABLE_SIZE] = value

    @property
    def enable_push(self) -> int:
        """
        The current value of the :data:`ENABLE_PUSH
        <h2.settings.SettingCodes.ENABLE_PUSH>` setting.
        """
        return self[SettingCodes.ENABLE_PUSH]

    @enable_push.setter
    def enable_push(self, value: int) -> None:
        self[SettingCodes.ENABLE_PUSH] = value

    @property
    def initial_window_size(self) -> int:
        """
        The current value of the :data:`INITIAL_WINDOW_SIZE
        <h2.settings.SettingCodes.INITIAL_WINDOW_SIZE>` setting.
        """
        return self[SettingCodes.INITIAL_WINDOW_SIZE]

    @initial_window_size.setter
    def initial_window_size(self, value: int) -> None:
        self[SettingCodes.INITIAL_WINDOW_SIZE] = value

    @property
    def max_frame_size(self) -> int:
        """
        The current value of the :data:`MAX_FRAME_SIZE
        <h2.settings.SettingCodes.MAX_FRAME_SIZE>` setting.
        """
        return self[SettingCodes.MAX_FRAME_SIZE]

    @max_frame_size.setter
    def max_frame_size(self, value: int) -> None:
        self[SettingCodes.MAX_FRAME_SIZE] = value

    @property
    def max_concurrent_streams(self) -> int:
        """
        The current value of the :data:`MAX_CONCURRENT_STREAMS
        <h2.settings.SettingCodes.MAX_CONCURRENT_STREAMS>` setting.
        """
        return self.get(SettingCodes.MAX_CONCURRENT_STREAMS, 2**32+1)

    @max_concurrent_streams.setter
    def max_concurrent_streams(self, value: int) -> None:
        self[SettingCodes.MAX_CONCURRENT_STREAMS] = value

    @property
    def max_header_list_size(self) -> int | None:
        """
        The current value of the :data:`MAX_HEADER_LIST_SIZE
        <h2.settings.SettingCodes.MAX_HEADER_LIST_SIZE>` setting. If not set,
        returns ``None``, which means unlimited.

        .. versionadded:: 2.5.0
        """
        return self.get(SettingCodes.MAX_HEADER_LIST_SIZE, None)

    @max_header_list_size.setter
    def max_header_list_size(self, value: int) -> None:
        self[SettingCodes.MAX_HEADER_LIST_SIZE] = value

    @property
    def enable_connect_protocol(self) -> int:
        """
        The current value of the :data:`ENABLE_CONNECT_PROTOCOL
        <h2.settings.SettingCodes.ENABLE_CONNECT_PROTOCOL>` setting.
        """
        return self[SettingCodes.ENABLE_CONNECT_PROTOCOL]

    @enable_connect_protocol.setter
    def enable_connect_protocol(self, value: int) -> None:
        self[SettingCodes.ENABLE_CONNECT_PROTOCOL] = value

    # Implement the MutableMapping API.
    def __getitem__(self, key: SettingCodes | int) -> int:
        val = self._settings[key][0]

        # Things that were created when a setting was received should stay
        # KeyError'd.
        if val is None:
            raise KeyError

        return val

    def __setitem__(self, key: SettingCodes | int, value: int) -> None:
        invalid = _validate_setting(key, value)
        if invalid:
            msg = f"Setting {key} has invalid value {value}"
            raise InvalidSettingsValueError(
                msg,
                error_code=invalid,
            )

        try:
            items = self._settings[key]
        except KeyError:
            items = collections.deque([None])  # type: ignore
            self._settings[key] = items

        items.append(value)

    def validate_received_setting(self, setting: SettingCodes | int, value: int) -> None:
        """
        Validate a setting received from the peer that owns this Settings
        object.

        Servers may advertise ``ENABLE_PUSH`` only as ``0`` in received
        SETTINGS frames.
        """
        invalid = _validate_setting(setting, value, client=self._client)

        if invalid != ErrorCodes.NO_ERROR:
            msg = f"Setting {setting} has invalid value {value}"
            raise InvalidSettingsValueError(
                msg,
                error_code=invalid,
            )

    def __delitem__(self, key: SettingCodes | int) -> None:
        del self._settings[key]

    def __iter__(self) -> Iterator[SettingCodes | int]:
        return self._settings.__iter__()

    def __len__(self) -> int:
        return len(self._settings)

    def __eq__(self, other: object) -> bool:
        if isinstance(other, Settings):
            return self._settings == other._settings
        return NotImplemented

    def __ne__(self, other: object) -> bool:
        if isinstance(other, Settings):
            return not self == other
        return NotImplemented

    # be explicit that Settings is not providing a hash implementation
    # see https://docs.python.org/3/reference/datamodel.html#object.__hash__
    __hash__ = MutableMapping.__hash__


def _validate_setting(
    setting: SettingCodes | int,
    value: int,
    *,
    client: bool | None = None,
) -> ErrorCodes:
    """
    Confirms that a specific setting has a well-formed value. If the setting is
    invalid, returns an error code. Otherwise, returns 0 (NO_ERROR).

    If ``client`` is set, the setting originated from a peer with that role.
    """
    if setting == SettingCodes.ENABLE_PUSH:
        # RFC 9113 section 6.5.2: "A client MUST treat receipt of a
        # SETTINGS frame with SETTINGS_ENABLE_PUSH set to 1 as a connection
        # error (Section 5.4.1) of type PROTOCOL_ERROR."
        if value not in (0, 1) or (client is False and value != 0):
            return ErrorCodes.PROTOCOL_ERROR
    elif setting == SettingCodes.INITIAL_WINDOW_SIZE:
        if not 0 <= value <= 2147483647:  # 2^31 - 1
            return ErrorCodes.FLOW_CONTROL_ERROR
    elif setting == SettingCodes.MAX_FRAME_SIZE:
        if not 16384 <= value <= 16777215:  # 2^14 and 2^24 - 1
            return ErrorCodes.PROTOCOL_ERROR
    elif setting == SettingCodes.MAX_HEADER_LIST_SIZE:
        if value < 0:
            return ErrorCodes.PROTOCOL_ERROR
    elif setting == SettingCodes.ENABLE_CONNECT_PROTOCOL and value not in (0, 1):
        return ErrorCodes.PROTOCOL_ERROR

    return ErrorCodes.NO_ERROR
