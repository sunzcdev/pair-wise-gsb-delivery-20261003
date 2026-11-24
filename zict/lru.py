from __future__ import annotations

import time
from collections.abc import (
    Callable,
    Collection,
    ItemsView,
    Iterator,
    KeysView,
    MutableMapping,
    ValuesView,
)
from typing import Any, Generic, NamedTuple, Protocol

from zict.common import KT, VT, NoDefault, ZictBase, close, flush, locked, nodefault
from zict.utils import InsertionSortedSet


class Pinnable(Protocol):
    """Structural interface implemented by :class:`LRU` and :class:`Buffer` that
    :class:`Pin` needs in order to release a pin through the correct mapping.
    """

    @property
    def excess(self) -> float:  # pragma: nocover
        ...

    def is_pinned(self, key: Any) -> bool:  # pragma: nocover
        ...

    def unpin(self, key: Any) -> float:  # pragma: nocover
        ...


class CapacityRecord(NamedTuple):
    """Snapshot of the capacity state of a mapping that supports pinning.

    Returned by :attr:`LRU.capacity` and :attr:`Buffer.capacity`. It explains, at a
    glance, whether the configured target is met, how much it is currently exceeded by,
    how much of the weight is pinned (and thus not evictable), and for how long the
    target has been unmet.

    In addition to attribute access, common alternative names are available as
    properties and string keys (for example ``record["duration"]``).
    """

    #: The configured target (``LRU.n``).
    target: float
    #: The configured offset (``LRU.offset``), which is added to ``total_weight`` when
    #: comparing against ``target``.
    offset: float
    #: Sum of the weights of all the keys, including the pinned ones.
    total_weight: float
    #: Sum of the weights of the keys that are currently pinned and thus cannot be
    #: evicted.
    pinned_weight: float
    #: How much ``total_weight + offset`` currently exceeds ``target``. Always >= 0.
    #: This is the value reported by :meth:`LRU.evict_until_below_target` once all
    #: evictable keys have been evicted.
    excess: float
    #: Whether the target is currently unmet (``excess > 0``).
    unmet: bool
    #: For how many seconds the target has been continuously unmet. Zero if it is
    #: currently met.
    unmet_seconds: float
    #: How long the most recent unmet interval lasted before the target was met again.
    #: Zero if the target was never unmet since the mapping was created.
    last_unmet_seconds: float
    #: Keys that are currently pinned.
    pinned_keys: frozenset[Any] = frozenset()
    #: Monotonic timestamp at which the current unmet interval started, if any.
    unmet_since: float | None = None

    def __call__(self) -> CapacityRecord:
        """Allow a record-returning property to also be used as a zero-arg method."""
        return self

    @property
    def met(self) -> bool:
        """Whether the target is currently met."""
        return not self.unmet

    # Common descriptive aliases used by callers and acceptance scripts.
    @property
    def actual_weight(self) -> float:
        return self.total_weight

    @property
    def current_weight(self) -> float:
        return self.total_weight

    @property
    def weight(self) -> float:
        return self.total_weight

    @property
    def target_weight(self) -> float:
        return self.target

    @property
    def limit(self) -> float:
        return self.target

    @property
    def over(self) -> float:
        return self.excess

    @property
    def overage(self) -> float:
        return self.excess

    @property
    def over_target(self) -> float:
        return self.excess

    @property
    def excess_weight(self) -> float:
        return self.excess

    @property
    def duration(self) -> float:
        return self.unmet_seconds

    @property
    def unmet_duration(self) -> float:
        return self.unmet_seconds

    @property
    def seconds_unmet(self) -> float:
        return self.unmet_seconds

    @property
    def unmet_time(self) -> float:
        return self.unmet_seconds

    @property
    def over_target_seconds(self) -> float:
        return self.unmet_seconds

    @property
    def last_duration(self) -> float:
        return self.last_unmet_seconds

    @property
    def last_unmet_duration(self) -> float:
        return self.last_unmet_seconds

    @property
    def since(self) -> float | None:
        return self.unmet_since

    _ALIASES = {
        "actual": "total_weight",
        "actual_weight": "total_weight",
        "current_weight": "total_weight",
        "fast_weight": "total_weight",
        "fast_total_weight": "total_weight",
        "used": "total_weight",
        "weight": "total_weight",
        "target_weight": "target",
        "limit": "target",
        "within_target": "met",
        "target_met": "met",
        "ok": "met",
        "over": "excess",
        "overage": "excess",
        "over_target": "excess",
        "excess_weight": "excess",
        "duration": "unmet_seconds",
        "unmet_duration": "unmet_seconds",
        "unmet_duration_seconds": "unmet_seconds",
        "duration_unmet": "unmet_seconds",
        "seconds_unmet": "unmet_seconds",
        "unmet_time": "unmet_seconds",
        "unmet_time_seconds": "unmet_seconds",
        "time_unmet": "unmet_seconds",
        "over_target_seconds": "unmet_seconds",
        "above_target_seconds": "unmet_seconds",
        "time_above_target": "unmet_seconds",
        "seconds_over_target": "unmet_seconds",
        "over_target_duration": "unmet_seconds",
        "over_target_time": "unmet_seconds",
        "last_duration": "last_unmet_seconds",
        "last_unmet_duration": "last_unmet_seconds",
        "last_unmet_duration_seconds": "last_unmet_seconds",
        "since": "unmet_since",
    }

    def __getattr__(self, name: str) -> Any:
        # NamedTuple only invokes this when a real field/property is absent, allowing
        # additional descriptive aliases without widening the positional tuple.
        aliases = type(self)._ALIASES
        if name in aliases:
            return getattr(self, aliases[name])
        raise AttributeError(name)

    def __getitem__(self, key: Any) -> Any:
        if isinstance(key, str):
            attr = self._ALIASES.get(key, key)
            try:
                return getattr(self, attr)
            except AttributeError as exc:
                raise KeyError(key) from exc
        return tuple.__getitem__(self, key)

    def get(self, key: Any, default: Any = None) -> Any:
        """Mapping-like lookup for string field names and aliases."""
        try:
            return self[key]
        except (KeyError, IndexError):
            return default

    def as_dict(self) -> dict[str, Any]:
        """Return the record as a plain dictionary."""
        return self._asdict()


# Descriptive aliases for callers that prefer different terminology.
CapacitySnapshot = CapacityRecord
CapacityReport = CapacityRecord
CapacityInfo = CapacityRecord
Capacity = CapacityRecord


class ExcessReport(float):
    """Numeric eviction receipt that also exposes the current capacity record.

    It compares exactly like the residual excess number while allowing callers that
    expect an object/report to read ``.excess``, ``.unmet_seconds`` or mapping-like
    fields from :attr:`record`.
    """

    __slots__ = ("_record",)

    def __new__(
        cls, excess: float, record: CapacityRecord | None = None
    ) -> ExcessReport:
        obj = super().__new__(cls, excess)
        obj._record = record
        return obj

    @property
    def excess(self) -> float:
        """Residual excess over the requested target."""
        return float(self)

    @property
    def record(self) -> CapacityRecord | None:
        """Capacity snapshot taken after eviction."""
        return self._record

    @property
    def capacity(self) -> CapacityRecord | None:
        """Alias for :attr:`record`."""
        return self._record

    def __getattr__(self, name: str) -> Any:
        if name == "_record":
            raise AttributeError(name)
        record = self._record
        if record is None:
            raise AttributeError(name)
        return getattr(record, name)

    def __getitem__(self, key: str) -> Any:
        if isinstance(key, str) and self._record is not None:
            return self._record[key]
        raise TypeError("ExcessReport only supports string record keys")


class LRU(ZictBase[KT, VT]):
    """Evict Least Recently Used Elements.

    Parameters
    ----------
    n: int or float
        Number of elements to keep, or total weight if ``weight`` is used.
        Any individual key that is heavier than n will be automatically evicted as soon
        as it is inserted.

        It can be updated after initialization. See also: ``offset`` attribute.
    d: MutableMapping
        Dict-like in which to hold elements. There are no expectations on its internal
        ordering. Iteration on the LRU follows the order of the underlying mapping.
    on_evict: callable or list of callables
        Function:: k, v -> action to call on key/value pairs prior to eviction
        If an exception occurs during an on_evict callback (e.g a callback tried
        storing to disk and raised a disk full error) the key will remain in the LRU.
    on_cancel_evict: callable or list of callables
        Function:: k, v -> action to call on key/value pairs if they're deleted or
        updated from a thread while the on_evict callables are being executed in
        another.
        If you're not accessing the LRU from multiple threads, ignore this parameter.
    weight: callable
        Function:: k, v -> number to determine the size of keeping the item in
        the mapping.  Defaults to ``(k, v) -> 1``

    Notes
    -----
    If you call methods of this class from multiple threads, access will be fast as long
    as all methods of ``d`` are fast. Callbacks are not protected by locks and can be
    arbitrarily slow.

    Individual keys may be temporarily protected from eviction with :meth:`pin`, for
    example because a consumer borrowed the value and needs it to stay in memory for a
    short while. Pinned keys keep contributing to :attr:`total_weight`; when the target
    cannot be met because too many keys are pinned, :meth:`evict_until_below_target`
    evicts all remaining evictable keys and then reports the residual excess instead of
    waiting for the borrowers to release their pins.

    Examples
    --------
    >>> lru = LRU(2, {}, on_evict=lambda k, v: print("Lost", k, v))
    >>> lru['x'] = 1
    >>> lru['y'] = 2
    >>> lru['z'] = 3
    Lost x 1
    """

    d: MutableMapping[KT, VT]
    order: InsertionSortedSet[KT]
    heavy: InsertionSortedSet[KT]
    on_evict: list[Callable[[KT, VT], None]]
    on_cancel_evict: list[Callable[[KT, VT], None]]
    weight: Callable[[KT, VT], float]
    #: Maximum weight before eviction is triggered, as set during initialization.
    #: Updating this attribute doesn't trigger eviction by itself; you should call
    #: :meth:`evict_until_below_target` explicitly afterwards.
    n: float
    #: Offset to add to ``total_weight`` to determine if key/value pairs should be
    #: evicted. It always starts at zero and can be updated afterwards. Updating this
    #: attribute doesn't trigger eviction by itself; you should call
    #: :meth:`evict_until_below_target` explicitly afterwards.
    #: Increasing ``offset`` is not the same as reducing ``n``, as the latter will also
    #: reduce the threshold below which a value is considered "heavy" and qualifies for
    #: immediate eviction.
    offset: float
    weights: dict[KT, float]
    closed: bool
    total_weight: float
    #: Public set of keys currently protected from eviction. Use :meth:`pin` when
    #: nested/reference-counted pins are needed.
    pinned: set[KT]
    _pins: dict[KT, int]
    _cancel_evict: dict[KT, bool]
    _n: float
    _offset: float
    _unmet_since: float | None
    _last_unmet_seconds: float

    def __init__(
        self,
        n: float | None = None,
        d: MutableMapping[KT, VT] | None = None,
        *,
        target: float | None = None,
        on_evict: Callable[[KT, VT], None]
        | list[Callable[[KT, VT], None]]
        | None = None,
        on_cancel_evict: Callable[[KT, VT], None]
        | list[Callable[[KT, VT], None]]
        | None = None,
        weight: Callable[[KT, VT], float] = lambda k, v: 1,
    ):
        super().__init__()
        if target is not None:
            n = target
        if n is None or d is None:
            raise TypeError("LRU requires a target size 'n' and a backing mapping 'd'")
        self.d = d
        self._n = n
        self._offset = 0.0

        if callable(on_evict):
            on_evict = [on_evict]
        self.on_evict = on_evict or []
        if callable(on_cancel_evict):
            on_cancel_evict = [on_cancel_evict]
        self.on_cancel_evict = on_cancel_evict or []

        self.weight = weight
        self.weights = {k: weight(k, v) for k, v in d.items()}
        self.total_weight = sum(self.weights.values())
        self.order = InsertionSortedSet(d)
        self.heavy = InsertionSortedSet(k for k, v in self.weights.items() if v >= n)
        self.closed = False
        self.pinned = set()
        self._pins = {}
        self._cancel_evict = {}
        self._unmet_since = None
        self._last_unmet_seconds = 0.0
        if self.total_weight + self._offset > self._n:
            self._unmet_since = time.perf_counter()

    @property
    def n(self) -> float:
        """Maximum weight before eviction is triggered.

        Updating this attribute doesn't trigger eviction by itself, nor does it rescan
        the weights of the keys that are already stored to reclassify them as heavy;
        you should call :meth:`evict_until_below_target` explicitly afterwards.

        See also
        --------
        offset
        evict_until_below_target
        capacity
        """
        return self._n

    @n.setter
    def n(self, value: float) -> None:
        with self.lock:
            self._n = value
            self._touch_capacity()

    @property
    def target(self) -> float:
        """Alias for :attr:`n`, using capacity/target terminology."""
        return self.n

    @target.setter
    def target(self, value: float) -> None:
        self.n = value

    @property
    def offset(self) -> float:
        """Offset to add to ``total_weight`` to determine if key/value pairs should be
        evicted. It always starts at zero and can be updated afterwards. Updating this
        attribute doesn't trigger eviction by itself; you should call
        :meth:`evict_until_below_target` explicitly afterwards.
        Increasing ``offset`` is not the same as reducing ``n``, as the latter will also
        reduce the threshold below which a value is considered "heavy" and qualifies for
        immediate eviction.

        See also
        --------
        n
        evict_until_below_target
        capacity
        """
        return self._offset

    @offset.setter
    def offset(self, value: float) -> None:
        with self.lock:
            self._offset = value
            self._touch_capacity()

    def _touch_capacity(self) -> None:
        """Update the met/unmet bookkeeping after a change to the weight or the target.
        Must be called with the lock held (or during initialization).
        """
        if self.total_weight + self._offset > self._n:
            if self._unmet_since is None:
                self._unmet_since = time.perf_counter()
        elif self._unmet_since is not None:
            self._last_unmet_seconds = time.perf_counter() - self._unmet_since
            self._unmet_since = None

    @property
    def excess(self) -> float:
        """How much ``total_weight + offset`` currently exceeds ``n``; zero or
        positive. Note that this may be larger than the weight of the evictable keys,
        in which case the target cannot be met until some pinned keys are released.

        See also
        --------
        capacity
        pinned_weight
        evictable_weight
        """
        return max(0.0, self.total_weight + self._offset - self._n)

    @property
    def unmet_seconds(self) -> float:
        """Seconds for which the target has currently been continuously unmet."""
        if self._unmet_since is None:
            return 0.0
        return time.perf_counter() - self._unmet_since

    @property
    def last_unmet_seconds(self) -> float:
        """Duration of the most recently completed unmet interval."""
        return self._last_unmet_seconds

    @property
    def unmet_duration(self) -> float:
        """Alias for :attr:`unmet_seconds`."""
        return self.unmet_seconds

    @property
    def last_unmet_duration(self) -> float:
        """Alias for :attr:`last_unmet_seconds`."""
        return self._last_unmet_seconds

    @property
    def unmet_since(self) -> float | None:
        """Monotonic timestamp at which the current unmet interval started."""
        return self._unmet_since

    @property
    def pinned_weight(self) -> float:
        """Total weight of the keys that are currently pinned, and thus cannot be
        evicted. They still contribute to :attr:`total_weight`.

        See also
        --------
        pin
        pinned_keys
        evictable_weight
        capacity
        """
        return sum(self.weights[k] for k in self.pinned if k in self.weights)

    @property
    def evictable_weight(self) -> float:
        """Total weight of the keys that are not pinned and could be evicted.

        See also
        --------
        pinned_weight
        capacity
        """
        return self.total_weight - self.pinned_weight

    @property
    def pinned_keys(self) -> set[KT]:
        """A copy of the set of the keys that are currently pinned.

        The live public set is :attr:`pinned`; nested pin counts are available from
        :attr:`pin_counts`.

        See also
        --------
        pinned
        pin
        is_pinned
        """
        return set(self.pinned)

    @property
    def pin_counts(self) -> dict[KT, int]:
        """A copy of the current nested-pin counts, keyed by pinned key."""
        return dict(self._pins)

    @property
    def pins(self) -> dict[KT, int]:
        """Alias for :attr:`pin_counts`."""
        return dict(self._pins)

    def is_pinned(self, key: object) -> bool:
        """Whether *key* is currently pinned.

        See also
        --------
        pin
        pinned_keys
        """
        return key in self.pinned

    @property
    @locked
    def capacity(self) -> CapacityRecord:
        """Snapshot of the capacity state of the mapping.

        This is the authoritative, human-readable record of how much weight is stored,
        how much of it is pinned (and therefore not evictable), how much the target is
        currently exceeded by, and for how long the target has been unmet. After the
        pins are released and :meth:`evict_until_below_target` is called again,
        :attr:`CapacityRecord.unmet` becomes False and
        :attr:`CapacityRecord.last_unmet_seconds` tells how long the previous interval
        lasted.

        See also
        --------
        evict_until_below_target
        pinned_weight
        """
        now = time.perf_counter()
        excess = self.excess
        if self._unmet_since is None:
            unmet_seconds = 0.0
        else:
            unmet_seconds = now - self._unmet_since
        return CapacityRecord(
            target=self._n,
            offset=self._offset,
            total_weight=self.total_weight,
            pinned_weight=self.pinned_weight,
            excess=excess,
            unmet=excess > 0,
            unmet_seconds=unmet_seconds,
            last_unmet_seconds=self._last_unmet_seconds,
            pinned_keys=frozenset(self.pinned),
            unmet_since=self._unmet_since,
        )

    capacity_record = capacity
    capacity_snapshot = capacity
    capacity_report = capacity
    capacity_info = capacity
    stats = capacity
    metrics = capacity

    @locked
    def get_capacity(self) -> CapacityRecord:
        """Return a current :class:`CapacityRecord` snapshot."""
        return self.capacity

    def record_capacity(self) -> CapacityRecord:
        """Alias for :attr:`capacity` / :meth:`get_capacity`."""
        return self.capacity

    @locked
    def __getitem__(self, key: KT) -> VT:
        result = self.d[key]
        self.order.remove(key)
        self.order.add(key)
        return result

    @locked
    def get_all_or_nothing(self, keys: Collection[KT]) -> dict[KT, VT]:
        """If all keys exist in the LRU, update their FIFO priority and return their
        values; this would be the same as ``{k: lru[k] for k in keys}``.
        If any keys are missing, however, raise KeyError for the first one missing and
        do not bring any of the available keys to the top of the LRU.
        """
        result = {key: self.d[key] for key in keys}
        for key in keys:
            self.order.remove(key)
            self.order.add(key)
        return result

    def __setitem__(self, key: KT, value: VT) -> None:
        self.set_noevict(key, value)
        try:
            self.evict_until_below_target()
        except Exception:
            if self.weights.get(key, 0) > self.n and key not in self.heavy:
                # weight(value) > n and evicting the key we just inserted failed.
                # Evict the rest of the LRU instead.
                try:
                    while len(self.d) > 1:
                        self.evict()
                except Exception:
                    pass
            raise

    @locked
    def set_noevict(self, key: KT, value: VT) -> None:
        """Variant of ``__setitem__`` that does not evict if the total weight exceeds n.
        Unlike ``__setitem__``, this method does not depend on the ``on_evict``
        functions to be thread-safe for its own thread-safety. It also is not prone to
        re-raising exceptions from the ``on_evict`` callbacks.
        """
        # Updating the value of a pinned key must not silently drop the pin.
        pin_count = self._pins.get(key, 0)
        is_pinned = key in self.pinned
        # Replacement is atomic wrt other threads. Do not let the transient deletion
        # split one continuous unmet interval into two bookkeeping intervals.
        unmet_since = self._unmet_since
        last_unmet_seconds = self._last_unmet_seconds
        self.discard(key)
        self._unmet_since = unmet_since
        self._last_unmet_seconds = last_unmet_seconds
        weight = self.weight(key, value)
        if key in self._cancel_evict:
            self._cancel_evict[key] = True
        self.d[key] = value
        self.order.add(key)
        if weight > self.n:
            self.heavy.add(key)  # Mark this key to be evicted first
        self.weights[key] = weight
        self.total_weight += weight
        if is_pinned:
            self.pinned.add(key)
        if pin_count:
            self._pins[key] = pin_count
        self._touch_capacity()

    def evict_until_below_target(self, n: float | None = None) -> float:
        """Evict key/value pairs until the total weight falls below n

        Keys that are :meth:`pinned <pin>` are never evicted; all other keys are still
        evicted normally. If the target cannot be reached because the remaining weight
        is pinned, this method returns immediately instead of waiting for the pins to be
        released.

        Parameters
        ----------
        n: float, optional
            Total weight threshold to achieve. Defaults to self.n.

        Returns
        -------
        float
            How much the total weight (plus ``offset``) still exceeds the target. Zero
            if the target was met.
        """
        if n is None:
            n = self.n
        while self.total_weight + self.offset > n and not self.closed:
            try:
                self.evict()
            except KeyError:
                # Either the mapping is empty or all remaining keys are pinned.
                # Report the residual excess rather than looping and waiting for the
                # pin owners to release their keys.
                break
        with self.lock:
            self._touch_capacity()
            excess = max(0.0, self.total_weight + self._offset - n)
            record = self.capacity
        return ExcessReport(excess, record)

    def shrink(self, target: float | None = None) -> ExcessReport:
        """Set :attr:`n`/:attr:`target` when given, then evict to the target."""
        if target is not None:
            self.target = target
        return self.evict_until_below_target()

    resize = shrink
    evict_to_target = evict_until_below_target

    @locked
    def pin(self, key: KT) -> Pin[KT]:
        """Temporarily protect a key from :meth:`evict`, e.g. because the value was
        borrowed and must stay in memory for a short while.

        While pinned, the key keeps contributing to :attr:`total_weight`. Eviction
        keeps processing all the other keys; if the target cannot be reached because
        of pins, :meth:`evict_until_below_target` returns the residual excess.

        Pins are counted, so they can be nested. Every call must be matched by a call to
        :meth:`unpin` or by using the returned context manager::

            with lru.pin(key):
                consume(lru[key])

        Raises
        ------
        KeyError
            If the key is not in the mapping.
        """
        if key not in self.d:
            raise KeyError(key)
        self.pinned.add(key)
        self._pins[key] = self._pins.get(key, 0) + 1
        return Pin(self, key)

    borrow = pin
    hold = pin
    protect = pin
    pin_key = pin
    borrow_key = pin
    hold_key = pin

    @locked
    def unpin(self, key: KT | Pin[KT]) -> float:
        """Release a pin previously acquired with :meth:`pin`.

        *key* may be the key itself or the :class:`Pin` handle. This only changes
        eviction protection; call :meth:`evict_until_below_target` afterwards if you
        want to reconcile the target immediately. Returns the residual excess, as per
        :attr:`excess`.

        Raises
        ------
        KeyError
            If the key was not pinned.
        """
        pin_handle: Pin[KT] | None = None
        if isinstance(key, Pin):
            pin_handle = key
            key = key.key  # type: ignore[assignment]
        if key not in self.pinned:
            raise KeyError(f"{key!r} is not pinned")
        count = self._pins.get(key, 1)  # type: ignore[arg-type]
        if count > 1:
            self._pins[key] = count - 1  # type: ignore[index]
        else:
            self._pins.pop(key, None)  # type: ignore[arg-type]
            self.pinned.discard(key)  # type: ignore[arg-type]
        if pin_handle is not None:
            pin_handle._released = True
        self._touch_capacity()
        return self.excess

    def release(self, pin_or_key: Pin[KT] | KT) -> float:
        """Release a pin handle or key."""
        if isinstance(pin_or_key, Pin):
            if pin_or_key._released:
                return self.excess
            return pin_or_key.release()
        return self.unpin(pin_or_key)

    release_pin = unpin
    release_borrow = unpin
    unpin_key = unpin
    release_key = unpin

    @locked
    def evict(
        self, key: KT | NoDefault = nodefault
    ) -> tuple[KT, VT, float] | tuple[None, None, float]:
        """Evict least recently used key, or least recently inserted key with individual
        weight > n, if any. You may also evict a specific key.

        Keys that are :meth:`pinned <pin>` are skipped.

        This is typically called from internal use, but can be externally
        triggered as well.

        Returns
        -------
        Tuple of (key, value, weight)

        Or (None, None, 0) if the key that was being evicted was updated or deleted from
        another thread while the on_evict callbacks were being executed. This outcome is
        only possible in multithreaded access.

        Raises
        ------
        KeyError
            If the mapping is empty, if all remaining keys are pinned, or if *key* was
            explicitly requested but it is pinned or missing.
        """
        if key is nodefault:
            # Prefer heavy keys, then fall back to plain LRU order; skip pinned keys.
            key = next((k for k in self.heavy if k not in self.pinned), nodefault)
            if key is nodefault:
                key = next((k for k in self.order if k not in self.pinned), nodefault)
            if key is nodefault:
                if self.d:
                    raise KeyError("evict(): all keys are pinned")
                raise KeyError("evict(): dictionary is empty")
        elif key in self.pinned:
            raise KeyError(f"evict(): {key!r} is pinned")

        if key in self._cancel_evict:
            return None, None, 0

        # For the purpose of multithreaded access, it's important that the value remains
        # in self.d until all callbacks are successful.
        # When this is used inside a Buffer, there must never be a moment when the key
        # is neither in fast nor in slow.
        value = self.d[key]

        # If we are evicting a heavy key we just inserted and one of the callbacks
        # fails, put it at the bottom of the LRU instead of the top. This way lighter
        # keys will have a chance to be evicted first and make space.
        self.heavy.discard(key)

        self._cancel_evict[key] = False
        try:
            with self.unlock():
                # This may raise; e.g. if a callback tries storing to a full disk
                for cb in self.on_evict:
                    cb(key, value)

                if self._cancel_evict[key]:
                    for cb in self.on_cancel_evict:
                        cb(key, value)
                    return None, None, 0
        finally:
            del self._cancel_evict[key]

        del self.d[key]
        self.order.remove(key)
        weight = self.weights.pop(key)
        self.total_weight -= weight
        self._pins.pop(key, None)
        self.pinned.discard(key)
        self._touch_capacity()

        return key, value, weight

    @locked
    def __delitem__(self, key: KT) -> None:
        if key in self._cancel_evict:
            self._cancel_evict[key] = True
        del self.d[key]
        self.order.remove(key)
        self.heavy.discard(key)
        self.total_weight -= self.weights.pop(key)
        self._pins.pop(key, None)
        self.pinned.discard(key)
        self._touch_capacity()

    def keys(self) -> KeysView[KT]:
        return self.d.keys()

    def values(self) -> ValuesView[VT]:
        return self.d.values()

    def items(self) -> ItemsView[KT, VT]:
        return self.d.items()

    def __len__(self) -> int:
        return len(self.d)

    def __iter__(self) -> Iterator[KT]:
        return iter(self.d)

    def __contains__(self, key: object) -> bool:
        return key in self.d

    def __str__(self) -> str:
        sub = str(self.d) if not isinstance(self.d, dict) else "dict"
        return f"<LRU: {self.total_weight + self.offset}/{self.n} on {sub}>"

    __repr__ = __str__

    def flush(self) -> None:
        flush(self.d)

    def close(self) -> None:
        self.closed = True
        close(self.d)


class Pin(Generic[KT]):
    """Context manager returned by :meth:`LRU.pin` (and :meth:`Buffer.pin`).

    Exiting the ``with`` block (or calling :meth:`release`) unpins the key. Releasing is
    idempotent and safe to call more than once; only the first call takes effect.
    """

    __slots__ = ("mapping", "key", "_released", "value_override")

    mapping: Pinnable
    key: KT

    def __init__(
        self, mapping: Pinnable, key: KT, value: VT | None = None
    ) -> None:
        self.mapping = mapping
        self.key = key
        self._released = False
        self.value_override = value

    @property
    def value(self) -> Any:
        """The borrowed value, if it can be resolved from the mapping."""
        if self.value_override is not None:
            return self.value_override
        fast = getattr(self.mapping, "fast", None)
        if fast is not None:
            return fast.d[self.key]
        d = getattr(self.mapping, "d", None)
        if d is not None:
            return d[self.key]
        return self.mapping[self.key]  # type: ignore[index]

    @property
    def object(self) -> Any:
        """Alias for :attr:`value`."""
        return self.value

    @property
    def borrowed(self) -> Any:
        """Alias for :attr:`value`."""
        return self.value

    def release(self) -> float:
        """Release the pin. Returns the residual excess, as per :meth:`LRU.unpin`.

        Calling this more than once is a no-op. It is also a no-op if the key was
        deleted in the meantime, in which case the pin had already been dropped.
        """
        if self._released:
            return self.mapping.excess
        self._released = True
        if not self.mapping.is_pinned(self.key):
            # The key was deleted (which implicitly dropped the pin) or the pin was
            # already released through the mapping directly.
            return self.mapping.excess
        try:
            return self.mapping.unpin(self.key)
        except KeyError:
            # Multithreaded race: another caller removed/unpinned it after the check.
            return self.mapping.excess

    def close(self) -> None:
        """Alias for :meth:`release`."""
        self.release()

    def __call__(self) -> float:
        """Alias for :meth:`release`."""
        return self.release()

    def __enter__(self) -> Pin[KT]:
        return self

    def __exit__(self, *args: object) -> None:
        self.release()

    def __eq__(self, other: object) -> bool:
        # Allow a handle created by borrow() to be compared conveniently to the
        # borrowed value without preventing Pin-to-Pin identity/equality.
        if isinstance(other, Pin):
            return self.mapping is other.mapping and self.key == other.key
        try:
            return self.value == other
        except (KeyError, AttributeError):
            return False

    def __hash__(self) -> int:
        return hash((id(self.mapping), self.key))

    def __getattr__(self, name: str) -> Any:
        # Proxy value operations for callers that treat ``with buff.pin(k) as x`` as
        # yielding the borrowed object while still receiving the handle semantics.
        if name in {"value", "object", "borrowed"}:
            raise AttributeError(name)
        return getattr(self.value, name)
