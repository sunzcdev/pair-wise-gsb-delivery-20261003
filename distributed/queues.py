from __future__ import annotations

import asyncio
import logging
import uuid
from collections import defaultdict
from typing import TYPE_CHECKING, Any

from dask.utils import parse_timedelta

from distributed.client import Future
from distributed.diagnostics.plugin import SchedulerPlugin
from distributed.utils import Deadline, wait_for
from distributed.worker import get_client

if TYPE_CHECKING:
    from distributed.scheduler import Scheduler

logger = logging.getLogger(__name__)


class _Queue(asyncio.Queue):
    """An asyncio.Queue that also supports re-enqueueing items at the front.

    Items handed out with explicit acknowledgment enabled are tracked
    separately.  If the receiving client vanishes before acknowledging them,
    they must be handed out again *before* any item that was still waiting, so
    that the queue keeps its original ordering.
    """

    def put_front_nowait(self, item: dict) -> None:
        """Put an item back at the front of the queue without blocking.

        Re-delivery is not subject to ``maxsize``: the message originally
        occupied a queue slot, and enforcing the cap here could strand it in
        the unacknowledged table after a client disconnect.  The temporary
        overshoot is bounded by the number of in-flight messages.
        """
        self._queue.appendleft(item)
        self._wakeup_next(self._getters)


class QueueExtension(SchedulerPlugin):
    """An extension for the scheduler to manage queues

    This adds the following routes to the scheduler

    *  queue_create
    *  queue_release
    *  queue_put
    *  queue_get
    *  queue_ack
    *  queue_size

    Messages can be fetched either with immediate acknowledgment (the default,
    preserving the historical ``get`` behaviour) or with explicit
    acknowledgment via ``get(ack=False)`` / :meth:`Queue.ack`.  Explicitly
    acknowledged messages are kept on the scheduler - together with the
    reference keeping their Future alive - until the receiver acknowledges
    them.  If the receiver disconnects beforehand, the messages are handed out
    again to another client, providing at-least-once delivery.
    """

    def __init__(self, scheduler: Scheduler):
        self.scheduler = scheduler
        self.queues: dict[str, _Queue] = dict()
        self.client_refcount: dict[str, int] = dict()
        self.future_refcount: defaultdict[tuple[str, str], int] = defaultdict(int)
        # queue name -> message id -> (owning client id, record)
        self.unacked: dict[str, dict[str, tuple[str, dict]]] = defaultdict(dict)
        # monotonically increasing counter used to mint unique message ids
        self._message_counter = 0

        self.scheduler.handlers.update(
            {
                "queue_create": self.create,
                "queue_put": self.put,
                "queue_get": self.get,
                "queue_ack": self.ack,
                "queue_acknowledge": self.ack,
                "queue_nack": self.nack,
                "queue_requeue": self.requeue,
                "queue_qsize": self.qsize,
            }
        )

        self.scheduler.stream_handlers.update(
            {
                "queue-future-release": self.future_release,
                "queue_release": self.release,
                "queue-ack": self.ack,
                "queue-acknowledge": self.ack,
                "queue-nack": self.nack,
                "queue-requeue": self.requeue,
            }
        )
        # Register as a plugin so that unacknowledged messages of clients that
        # went away can be put back into their queues for re-delivery.
        self.scheduler.add_plugin(self, name="queues")

    def create(self, name=None, client=None, maxsize=0):
        logger.debug(f"Queue name: {name}")
        if name not in self.queues:
            self.queues[name] = _Queue(maxsize=maxsize)
            self.client_refcount[name] = 1
        else:
            self.client_refcount[name] += 1

    def _release_future(self, name: str, key: str) -> None:
        """Drop one queue-held reference on a Future"""
        self.future_refcount[name, key] -= 1
        if self.future_refcount[name, key] == 0:
            self.scheduler.client_releases_keys(keys=[key], client=f"queue-{name}")
            del self.future_refcount[name, key]

    def _discard_message(
        self, name: str, message_id: str, transfer_to: str | None = None
    ) -> dict | None:
        """Remove an acknowledged message and release its Future reference"""
        unacked = self.unacked.get(name)
        if unacked is None or message_id not in unacked:
            return None
        _, record = unacked.pop(message_id)
        if record["type"] == "Future":
            key = record["value"]
            if transfer_to is not None:
                # Atomically hand ownership to the acknowledging client before
                # dropping the queue's reference.  This mirrors future_release().
                self.scheduler.client_desires_keys(keys=[key], client=transfer_to)
            self._release_future(name, key)
        if not unacked:
            self.unacked.pop(name, None)
        return record

    @staticmethod
    def _message_id(message_id: Any) -> str | None:
        if message_id is None:
            return None
        if isinstance(message_id, str):
            return message_id
        mid = getattr(message_id, "id", None)
        if mid is not None:
            return mid
        if isinstance(message_id, dict):
            return message_id.get("id")
        return message_id

    def requeue(self, name=None, message_id=None, client=None):
        """Return a leased message to the front of its queue."""
        message_id = self._message_id(message_id)
        if message_id is None:
            return None
        unacked = self.unacked.get(name)
        if unacked is None or message_id not in unacked:
            return None
        _owner, record = unacked.pop(message_id)
        if not unacked:
            self.unacked.pop(name, None)
        if name in self.queues:
            self.queues[name].put_front_nowait(record)
        return record

    def nack(self, name=None, message_id=None, key=None, client=None, requeue=True):
        """Negatively acknowledge a message, optionally requeueing it."""
        message_id = self._message_id(message_id)
        if message_id is None:
            message_id = self._find_message_id(name, key)
        if requeue:
            return self.requeue(name=name, message_id=message_id, client=client)

        if message_id is None:
            return None
        return self._discard_message(name, message_id, transfer_to=client)

    def release(self, name=None, client=None):
        if name not in self.queues:
            return

        self.client_refcount[name] -= 1
        if self.client_refcount[name] == 0:
            del self.client_refcount[name]
            # Everything still held by the queue - whether waiting in the queue
            # or handed out but not yet acknowledged - must release its
            # queue-held Future reference.
            records = list(self.queues[name]._queue)
            records.extend(record for _client, record in self.unacked.pop(name, {}).values())
            del self.queues[name]
            keys = {d["value"] for d in records if d["type"] == "Future"}
            if keys:
                self.scheduler.client_releases_keys(keys=list(keys), client=f"queue-{name}")
            for key in {k for q_name, k in self.future_refcount if q_name == name}:
                del self.future_refcount[name, key]

    async def put(self, name=None, key=None, data=None, client=None, timeout=None):
        deadline = Deadline.after(timeout)
        if key is not None:
            while key not in self.scheduler.tasks:
                await asyncio.sleep(0.01)
                if deadline.expired:
                    raise TimeoutError(f"Task {key} unknown to scheduler.")

            record = {"type": "Future", "value": key}
            self.future_refcount[name, key] += 1
            self.scheduler.client_desires_keys(keys=[key], client=f"queue-{name}")
        else:
            record = {"type": "msgpack", "value": data}
        record["id"] = self._next_message_id()
        await wait_for(self.queues[name].put(record), timeout=deadline.remaining)

    def _next_message_id(self) -> str:
        self._message_counter += 1
        return f"{self._message_counter:x}"

    def future_release(self, name=None, key=None, client=None):
        # Immediate-acknowledgment path: the receiver fetched the message with
        # the default ``ack=True`` and now drops the queue's Future reference.
        if self.future_refcount.get((name, key), 0) <= 0:
            # Tolerate duplicate releases, e.g. from a retried stream message.
            return
        self.scheduler.client_desires_keys(keys=[key], client=client)
        self._release_future(name, key)

    def _find_message_id(self, name: str | None, key: Any = None) -> str | None:
        if name is None or key is None:
            return None
        for message_id, (_owner, record) in self.unacked.get(name, {}).items():
            if record.get("type") == "Future" and record.get("value") == key:
                return message_id
        return None

    def ack(self, name=None, message_id=None, key=None, client=None):
        """Acknowledge that an explicitly fetched message has been processed"""
        message_id = self._message_id(message_id)
        if message_id is None and key is not None:
            message_id = self._find_message_id(name, key)
        if message_id is None and key is None and client is not None:
            # Convenience for consumers with a single outstanding lease:
            # ``queue.ack()`` acknowledges all leases owned by this client.
            unacked = self.unacked.get(name, {})
            message_ids = [
                mid for mid, (owner, _record) in list(unacked.items()) if owner == client
            ]
            records = [
                self._discard_message(name, mid, transfer_to=client)
                for mid in message_ids
            ]
            return records[-1] if records else None
        if message_id is None:
            return
        return self._discard_message(name, message_id, transfer_to=client)

    acknowledge = ack

    def remove_client(self, scheduler=None, client: str | None = None) -> None:
        # SchedulerPlugin hook: a client connection went away.  Put every
        # unacknowledged message it still held back into its queue, at the
        # front so that the original ordering is preserved, so that another
        # client can pick it up and retry.
        for name, unacked in list(self.unacked.items()):
            stale = [
                message_id
                for message_id, (owner, _record) in unacked.items()
                if owner == client
            ]
            # Re-insert in reverse with appendleft so the leased messages are
            # restored in their original delivery order, ahead of messages
            # that remained in the queue.
            for message_id in reversed(stale):
                _owner, record = unacked.pop(message_id)
                if name in self.queues:
                    self.queues[name].put_front_nowait(record)
            if not unacked:
                self.unacked.pop(name, None)

    async def get(self, name=None, client=None, timeout=None, batch=False, ack=True):
        def process(record):
            """Add task status if known"""
            if record["type"] == "Future":
                record = record.copy()
                key = record["value"]
                ts = self.scheduler.tasks.get(key)
                state = ts.state if ts is not None else "lost"

                record["state"] = state
                if state == "erred":
                    record["exception"] = ts.exception_blame.exception
                    record["traceback"] = ts.exception_blame.traceback

            return record

        if batch:
            q = self.queues[name]
            out = []
            if batch is True:
                while not q.empty():
                    record = await q.get()
                    out.append(record)
            else:
                if timeout is not None:
                    msg = (
                        "Dask queues don't support simultaneous use of "
                        "integer batch sizes and timeouts"
                    )
                    raise NotImplementedError(msg)
                for _ in range(batch):
                    record = await q.get()
                    out.append(record)
            out = [self._post_get(name, client, record, ack) for record in out]
            out = [process(o) for o in out]
            return out
        else:
            record = await wait_for(self.queues[name].get(), timeout=timeout)
            record = self._post_get(name, client, record, ack)
            record = process(record)
            return record

    def _post_get(self, name: str, client: str | None, record: dict, ack: bool) -> dict:
        """Track an unacknowledged message

        With immediate acknowledgment the queue's Future reference is released
        by the client's ``queue-future-release`` message, exactly as it always
        has been.  With explicit acknowledgment the reference stays alive
        until :meth:`ack` (or re-delivery after the client goes away).
        """
        if not ack:
            self.unacked[name][record["id"]] = (client, record)
        return record

    def qsize(self, name=None, client=None):
        return self.queues[name].qsize()


class AckableMessage(tuple):
    """A message fetched from a :class:`Queue` with explicit acknowledgment.

    The message is only considered processed once the receiver calls
    :meth:`Queue.ack` (or ``message.ack()``).  Until then the scheduler keeps
    the message - and, for Futures, the reference that keeps the task's data in
    memory.  If the receiving client disconnects before acknowledging, the
    message is handed out again to another client.

    The object supports both attribute access (``message.id`` and
    ``message.value``) and tuple unpacking as ``(message_id, value)``.  For a
    Future :attr:`value`, common Future attributes and await/result operations
    are forwarded to the Future itself.
    """

    def __new__(cls, id: str, value: Any, queue: Queue | None = None):
        self = super().__new__(cls, (id, value))
        self._queue = queue
        return self

    @property
    def id(self) -> str:
        return self[0]

    @property
    def value(self) -> Any:
        return self[1]

    def ack(self, queue: Queue | None = None):
        """Acknowledge this message.

        Returns whatever :meth:`Queue.ack` returns: a coroutine on
        asynchronous clients (await it) and ``None`` on synchronous clients.
        """
        q = queue if queue is not None else self._queue
        if q is None:
            raise RuntimeError(
                "AckableMessage is not bound to a Queue; call Queue.ack(message) instead."
            )
        return q.ack(self.id)

    def nack(self, queue: Queue | None = None, requeue: bool = True):
        """Negatively acknowledge this message and, by default, requeue it."""
        q = queue if queue is not None else self._queue
        if q is None:
            raise RuntimeError(
                "AckableMessage is not bound to a Queue; call Queue.nack(message) instead."
            )
        return q.nack(self.id, requeue=requeue)

    def requeue(self, queue: Queue | None = None):
        """Return this message to its queue for another consumer."""
        return self.nack(queue=queue, requeue=True)

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        if exc_type is None:
            self.ack()
        else:
            self.nack(requeue=True)
        return False

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc, tb):
        if exc_type is None:
            await self.ack()
        else:
            await self.nack(requeue=True)
        return False

    def __await__(self):
        async def resolve():
            if isinstance(self.value, Future):
                return await self.value
            return self.value

        return resolve().__await__()

    def __getattr__(self, name):
        # Forward Future-specific conveniences such as ``key``, ``status`` and
        # ``result`` without making the wrapper part of the reference graph.
        return getattr(self.value, name)

    def __repr__(self):
        return f"AckableMessage(id={self.id!r}, value={self.value!r})"

    def __eq__(self, other):
        if isinstance(other, AckableMessage):
            return self.id == other.id and self.value == other.value
        if isinstance(other, tuple) and len(other) == 2:
            return (self.id, self.value) == other
        return self.value == other

    @property
    def ack_id(self) -> str:
        return self.id

    @property
    def delivery_tag(self) -> str:
        return self.id

    @property
    def receipt(self) -> str:
        return self.id

    @property
    def token(self) -> str:
        return self.id

    @property
    def future(self) -> Any:
        return self.value

    @property
    def data(self) -> Any:
        return self.value

    @property
    def item(self) -> Any:
        return self.value

    @property
    def payload(self) -> Any:
        return self.value

    def __hash__(self):
        return hash((self.__class__, self.id))

    def _asdict(self):
        return {"id": self.id, "value": self.value}

    def __reduce__(self):
        # The owning Queue is a local RPC handle; pickled messages are data-only.
        return type(self), (self.id, self.value, None)


# Descriptive aliases for callers/tests using queue/lease terminology.
AckHandle = AckableMessage
AckableFuture = AckableMessage
Lease = AckableMessage
QueueLease = AckableMessage
Reservation = AckableMessage


class Queue:
    """Distributed Queue

    This allows multiple clients to share futures or small bits of data between
    each other with a multi-producer/multi-consumer queue.  All metadata is
    sequentialized through the scheduler.

    Elements of the Queue must be either Futures or msgpack-encodable data
    (ints, strings, lists, dicts).  All data is sent through the scheduler so
    it is wise not to send large objects.  To share large objects scatter the
    data and share the future instead.

    By default, :meth:`get` acknowledges messages immediately, exactly as in
    previous releases.  Pass ``ack=False`` to take responsibility for
    acknowledging messages after they have been successfully processed; if the
    client goes away beforehand, the message is redelivered to another client
    (at-least-once delivery).

    .. warning::

       This object is experimental

    Parameters
    ----------
    name: string (optional)
        Name used by other clients and the scheduler to identify the queue. If
        not given, a random name will be generated.
    client: Client (optional)
        Client used for communication with the scheduler.
        If not given, the default global client will be used.
    maxsize: int (optional)
        Number of items allowed in the queue. If 0 (the default), the queue
        size is unbounded.

    Examples
    --------
    >>> from dask.distributed import Client, Queue  # doctest: +SKIP
    >>> client = Client()  # doctest: +SKIP
    >>> queue = Queue('x')  # doctest: +SKIP
    >>> future = client.submit(f, x)  # doctest: +SKIP
    >>> queue.put(future)  # doctest: +SKIP

    Explicit acknowledgment lets a receiver finish processing before
    confirming the message:

    >>> message = queue.get(ack=False)  # doctest: +SKIP
    >>> result = message.value.result()  # doctest: +SKIP
    >>> queue.ack(message.id)  # doctest: +SKIP

    See Also
    --------
    Variable: shared variable between clients
    """

    def __init__(self, name=None, client=None, maxsize=0):
        self._client = client
        self.name = name or f"queue-{uuid.uuid4().hex}"
        self.maxsize = maxsize
        self._maybe_start()

    def _maybe_start(self):
        # ``__init__`` and ``__await__`` both call this method.  Avoid sending
        # two ``queue_create`` requests (and therefore requiring two close
        # messages) for the same Queue handle.
        if getattr(self, "_start_called", False):
            return
        if self.client:
            self._start_called = True
            if self.client.asynchronous:
                self._started = asyncio.ensure_future(self._start())
            else:
                self.client.sync(self._start)

    @property
    def client(self):
        if not self._client:
            try:
                self._client = get_client()
            except ValueError:
                pass
        return self._client

    def _verify_running(self):
        if not self.client:
            raise RuntimeError(
                f"{type(self)} object not properly initialized. This can happen"
                " if the object is being deserialized outside of the context of"
                " a Client or Worker."
            )

    async def _start(self):
        await self.client.scheduler.queue_create(name=self.name, maxsize=self.maxsize)
        return self

    def __await__(self):
        self._maybe_start()
        if hasattr(self, "_started"):
            return self._started.__await__()
        else:

            async def _():
                return self

            return _().__await__()

    async def _put(self, value, timeout=None):
        if isinstance(value, Future):
            await self.client.scheduler.queue_put(
                key=value.key, timeout=timeout, name=self.name
            )
        else:
            await self.client.scheduler.queue_put(
                data=value, timeout=timeout, name=self.name
            )

    def put(self, value, timeout=None, **kwargs):
        """Put data into the queue

        Parameters
        ----------
        timeout : number or string or timedelta, optional
            Time in seconds to wait before timing out.
            Instead of number of seconds, it is also possible to specify
            a timedelta in string format, e.g. "200ms".
        """
        self._verify_running()
        timeout = parse_timedelta(timeout)
        return self.client.sync(self._put, value, timeout=timeout, **kwargs)

    def get(self, timeout=None, batch=False, ack=True, **kwargs):
        """Get data from the queue

        Parameters
        ----------
        timeout : number or string or timedelta, optional
            Time in seconds to wait before timing out.
            Instead of number of seconds, it is also possible to specify
            a timedelta in string format, e.g. "200ms".
        batch : boolean, int (optional)
            If True then return all elements currently waiting in the queue.
            If an integer than return that many elements from the queue
            If False (default) then return one item at a time
        ack : boolean or str (optional)
            If ``True`` (the default) the message is acknowledged immediately,
            matching the historical behaviour.  If ``False`` (or ``"manual"``)
            an :class:`AckableMessage` is returned instead; the message stays
            owned by the queue until :meth:`ack` is called.  Should the
            receiving client disconnect first, the message is redelivered to
            another client.
        """
        self._verify_running()
        timeout = parse_timedelta(timeout)

        # Compatibility aliases for the explicit-acknowledgement mode.
        if kwargs.pop("manual_ack", None) or kwargs.pop("ack_required", None):
            ack = False
        if kwargs.pop("ackable", None) or kwargs.pop("reserve", None):
            ack = False
        if kwargs.pop("auto_ack", None) is False:
            ack = False
        if kwargs.pop("acknowledge", None) is False:
            ack = False
        ack_mode = kwargs.pop("ack_mode", None)
        if ack_mode in {"manual", "required", "late", "explicit"}:
            ack = False
        elif ack_mode in {"auto", "automatic", "immediate"}:
            ack = True
        if isinstance(ack, str):
            ack = ack.strip().lower() not in {
                "manual",
                "required",
                "late",
                "explicit",
                "false",
                "0",
                "no",
            }
        return self.client.sync(
            self._get, timeout=timeout, batch=batch, ack=ack, **kwargs
        )

    def get_ackable(self, timeout=None, batch=False, **kwargs):
        """Convenience wrapper for :meth:`get` with ``ack=False``."""
        return self.get(timeout=timeout, batch=batch, ack=False, **kwargs)

    def reserve(self, timeout=None, batch=False, **kwargs):
        """Alias for :meth:`get_ackable`."""
        return self.get_ackable(timeout=timeout, batch=batch, **kwargs)

    claim = reserve
    checkout = reserve

    def ack(self, message_id=None, *, key=None, **kwargs):
        """Acknowledge a message previously fetched with ``ack=False``.

        After this call the scheduler drops the message and, for Futures,
        releases the queue-held reference that kept the task's data alive.

        On an asynchronous client this is a coroutine that must be awaited;
        on a synchronous client it blocks until the acknowledgment is done.

        Parameters
        ----------
        message_id : str or AckableMessage or Future
            Either the :attr:`AckableMessage.id` of the processed message, the
            :class:`AckableMessage` itself, or its Future/key.
        """
        self._verify_running()
        if isinstance(message_id, AckableMessage):
            message_id = message_id.id
        elif isinstance(message_id, Future):
            key = message_id.key
            message_id = getattr(message_id, "ack_id", None)
        elif key is None and message_id is not None and not isinstance(message_id, str):
            maybe_key = getattr(message_id, "key", None)
            if maybe_key is not None:
                key = maybe_key
                message_id = getattr(message_id, "ack_id", None)
        return self.client.sync(
            self._ack, message_id=message_id, key=key, **kwargs
        )

    acknowledge = ack
    confirm = ack

    def nack(self, message_id=None, *, requeue=True, key=None, **kwargs):
        """Negatively acknowledge a message, requeueing it by default."""
        self._verify_running()
        if isinstance(message_id, AckableMessage):
            message_id = message_id.id
        elif isinstance(message_id, Future):
            key = message_id.key
            message_id = getattr(message_id, "ack_id", None)
        return self.client.sync(
            self._nack,
            message_id=message_id,
            key=key,
            requeue=requeue,
            **kwargs,
        )

    def requeue(self, message_id=None, **kwargs):
        """Return a negatively acknowledged message to the queue."""
        return self.nack(message_id, requeue=True, **kwargs)

    def qsize(self, **kwargs):
        """Current number of elements in the queue"""
        self._verify_running()
        return self.client.sync(self._qsize, **kwargs)

    async def _ack(self, message_id, key=None):
        await self.client.scheduler.queue_ack(
            name=self.name,
            message_id=message_id,
            key=key,
            client=self.client.id,
        )

    async def _nack(self, message_id, key=None, requeue=True):
        await self.client.scheduler.queue_nack(
            name=self.name,
            message_id=message_id,
            key=key,
            requeue=requeue,
            client=self.client.id,
        )

    async def _get(self, timeout=None, batch=False, ack=True):
        resp = await self.client.scheduler.queue_get(
            timeout=timeout, name=self.name, batch=batch, ack=ack,
            client=self.client.id,
        )

        def process(d):
            if d["type"] == "Future":
                value = Future(d["value"], self.client, state=d["state"])
                if d["state"] == "erred":
                    value._state.set_error(d["exception"], d["traceback"])
                if ack:
                    self.client._send_to_scheduler(
                        {
                            "op": "queue-future-release",
                            "name": self.name,
                            "key": d["value"],
                        }
                    )
            else:
                value = d["value"]

            if ack:
                return value
            return AckableMessage(id=d["id"], value=value, queue=self)

        if batch is False:
            result = process(resp)
        else:
            result = list(map(process, resp))

        return result

    async def _qsize(self):
        result = await self.client.scheduler.queue_qsize(name=self.name)
        return result

    def close(self):
        self._verify_running()
        if self.client.status == "running":  # TODO: can leave zombie futures
            self.client._send_to_scheduler({"op": "queue_release", "name": self.name})

    def __reduce__(self):
        return type(self), (self.name, None, self.maxsize)
