from __future__ import annotations

from collections.abc import Callable, ItemsView, Iterator, MutableMapping, ValuesView
from itertools import chain

from zict.common import KT, VT, ZictBase, close, discard, flush, locked
from zict.lru import CapacityRecord, LRU, Pin


class Buffer(ZictBase[KT, VT]):
    """Buffer one dictionary on top of another

    This creates a MutableMapping by combining two MutableMappings, one that
    feeds into the other when it overflows, based on an LRU mechanism.  When
    the first evicts elements these get placed into the second. When an item
    is retrieved from the second it is placed back into the first.

    Parameters
    ----------
    fast: MutableMapping
    slow: MutableMapping
    n: float
        Number of elements to keep, or total weight if ``weight`` is used.
    weight: f(k, v) -> float, optional
        Weight of each key/value pair (default: 1)
    fast_to_slow_callbacks: list of callables
        These functions run every time data moves from the fast to the slow
        mapping. They take two arguments, a key and a value.
        If an exception occurs during a fast_to_slow_callbacks (e.g a callback tried
        storing to disk and raised a disk full error) the key will remain in the LRU.
    slow_to_fast_callbacks: list of callables
        These functions run every time data moves form the slow to the fast mapping.

    Notes
    -----
    If you call methods of this class from multiple threads, access will be fast as long
    as all methods of ``fast``, plus ``slow.__contains__`` and ``slow.__delitem__``, are
    fast. ``slow.__getitem__``, ``slow.__setitem__`` and callbacks are not protected
    by locks.

    Values that are being borrowed by a processor can be protected from being moved to
    ``slow`` (and thus from being evicted) with :meth:`pin`. Only the keys that are
    currently in ``fast`` can be pinned. While some keys are pinned, eviction keeps
    moving the other, evictable keys to ``slow``; if the target cannot be met because
    the remaining weight is pinned, :meth:`evict_until_below_target` returns immediately
    with the residual excess instead of waiting for the borrowers to release their pins.
    :attr:`capacity` reports the resulting state.

    Examples
    --------
    >>> fast = {}
    >>> slow = Func(dumps, loads, File('storage/'))  # doctest: +SKIP
    >>> def weight(k, v):
    ...     return sys.getsizeof(v)
    >>> buff = Buffer(fast, slow, 1e8, weight=weight)  # doctest: +SKIP

    See Also
    --------
    LRU
    """

    fast: LRU[KT, VT]
    slow: MutableMapping[KT, VT]
    weight: Callable[[KT, VT], float]
    fast_to_slow_callbacks: list[Callable[[KT, VT], None]]
    slow_to_fast_callbacks: list[Callable[[KT, VT], None]]
    _cancel_restore: dict[KT, bool]

    def __init__(
        self,
        fast: MutableMapping[KT, VT],
        slow: MutableMapping[KT, VT],
        n: float | None = None,
        weight: Callable[[KT, VT], float] = lambda k, v: 1,
        fast_to_slow_callbacks: Callable[[KT, VT], None]
        | list[Callable[[KT, VT], None]]
        | None = None,
        slow_to_fast_callbacks: Callable[[KT, VT], None]
        | list[Callable[[KT, VT], None]]
        | None = None,
        target: float | None = None,
    ):
        super().__init__()
        if target is not None:
            n = target
        if n is None:
            raise TypeError("Buffer requires a target size 'n' (or 'target')")
        self.fast = LRU(
            n,
            fast,
            weight=weight,
            on_evict=[self.fast_to_slow],
            on_cancel_evict=[self._cancel_evict],
        )
        self.slow = slow
        self.weight = weight
        if callable(fast_to_slow_callbacks):
            fast_to_slow_callbacks = [fast_to_slow_callbacks]
        if callable(slow_to_fast_callbacks):
            slow_to_fast_callbacks = [slow_to_fast_callbacks]
        self.fast_to_slow_callbacks = fast_to_slow_callbacks or []
        self.slow_to_fast_callbacks = slow_to_fast_callbacks or []
        self._cancel_restore = {}

    @property
    def n(self) -> float:
        """Maximum weight in the fast mapping before eviction happens.
        Can be updated; this won't trigger eviction by itself; you should call
        :meth:`evict_until_below_target` afterwards.

        See also
        --------
        offset
        evict_until_below_target
        LRU.n
        LRU.offset
        """
        return self.fast.n

    @n.setter
    def n(self, value: float) -> None:
        self.fast.n = value

    @property
    def target(self) -> float:
        """Alias for :attr:`n`, using capacity/target terminology."""
        return self.n

    @target.setter
    def target(self, value: float) -> None:
        self.n = value

    @property
    def offset(self) -> float:
        """Offset to add to the total weight in the fast buffer to determine when
        eviction happens. Note that increasing offset is not the same as decreasing n,
        as the latter also changes what keys qualify as "heavy" and should not be stored
        in fast.

        Always starts at zero and can be updated; this won't trigger eviction by itself;
        you should call :meth:`evict_until_below_target` afterwards.

        See also
        --------
        n
        evict_until_below_target
        LRU.n
        LRU.offset
        """
        return self.fast.offset

    @offset.setter
    def offset(self, value: float) -> None:
        self.fast.offset = value

    def fast_to_slow(self, key: KT, value: VT) -> None:
        self.slow[key] = value
        try:
            for cb in self.fast_to_slow_callbacks:
                cb(key, value)
        # LRU catches exception, raises and makes sure keys are not lost and located in
        # fast.
        except Exception:
            del self.slow[key]
            raise

    def slow_to_fast(self, key: KT) -> VT:
        self._cancel_restore[key] = False
        try:
            with self.unlock():
                value = self.slow[key]
            if self._cancel_restore[key]:
                raise KeyError(key)
        finally:
            del self._cancel_restore[key]

        # Avoid useless movement for heavy values
        w = self.weight(key, value)
        if w <= self.n:
            # Multithreaded edge case:
            # - Thread 1 starts slow_to_fast(x) and puts it at the top of fast
            # - This causes the eviction of older key(s)
            # - While thread 1 is evicting older keys, thread 2 is loading fast with
            #   set_noevict()
            # - By the time the eviction of the older key(s) is done, there is
            #   enough weight in fast that thread 1 will spill x
            # - If the below code was just `self.fast[key] = value; del
            #   self.slow[key]` now the key would be in neither slow nor fast!
            self.fast.set_noevict(key, value)
            del self.slow[key]

        with self.unlock():
            self.fast.evict_until_below_target()
            for cb in self.slow_to_fast_callbacks:
                cb(key, value)

        return value

    @locked
    def __getitem__(self, key: KT) -> VT:
        try:
            return self.fast[key]
        except KeyError:
            return self.slow_to_fast(key)

    def __setitem__(self, key: KT, value: VT) -> None:
        with self.lock:
            discard(self.slow, key)
            if key in self._cancel_restore:
                self._cancel_restore[key] = True
        self.fast[key] = value

    @locked
    def set_noevict(self, key: KT, value: VT) -> None:
        """Variant of ``__setitem__`` that does not move keys from fast to slow if the
        total weight exceeds n
        """
        discard(self.slow, key)
        if key in self._cancel_restore:
            self._cancel_restore[key] = True
        self.fast.set_noevict(key, value)

    def evict_until_below_target(self, n: float | None = None) -> float:
        """Wrapper around :meth:`zict.LRU.evict_until_below_target`.
        Presented here to allow easier overriding.

        Returns
        -------
        float
            How much the fast mapping still exceeds the target once all the evictable
            keys have been moved to slow. Non-zero only if the remaining weight is
            pinned; see :meth:`pin` and :attr:`capacity`.
        """
        return self.fast.evict_until_below_target(n)

    def shrink(self, target: float | None = None) -> float:
        """Lower :attr:`n`/:attr:`target` (when given) and evict to the new target.

        Returns the residual excess report from
        :meth:`evict_until_below_target`.
        """
        if target is not None:
            self.target = target
        return self.evict_until_below_target()

    resize = shrink
    evict_to_target = evict_until_below_target

    @property
    def capacity(self) -> CapacityRecord:
        """Snapshot of the capacity state of the fast mapping.

        See Also
        --------
        capacity_record
        pin
        unpin
        evict_until_below_target
        zict.LRU.capacity
        """
        return self.fast.capacity

    capacity_record = capacity
    capacity_snapshot = capacity
    capacity_report = capacity
    capacity_info = capacity
    stats = capacity
    metrics = capacity

    def get_capacity(self) -> CapacityRecord:
        """Return a current :class:`~zict.CapacityRecord` snapshot."""
        return self.fast.get_capacity()

    def record_capacity(self) -> CapacityRecord:
        """Alias for :attr:`capacity` / :meth:`get_capacity`."""
        return self.fast.record_capacity()

    @property
    def unmet_seconds(self) -> float:
        """Seconds for which the target has currently been continuously unmet."""
        return self.fast.unmet_seconds

    @property
    def last_unmet_seconds(self) -> float:
        """Duration of the most recently completed unmet interval."""
        return self.fast.last_unmet_seconds

    @property
    def pinned_weight(self) -> float:
        """Total weight of the keys that are currently pinned in fast.

        See Also
        --------
        pin
        pinned_keys
        """
        return self.fast.pinned_weight

    @property
    def evictable_weight(self) -> float:
        """Total weight of the keys in fast that are not pinned and could be moved to
        slow.

        See Also
        --------
        pinned_weight
        capacity
        """
        return self.fast.evictable_weight

    @property
    def excess(self) -> float:
        """How much the weight in fast currently exceeds ``n``; zero or positive.

        See Also
        --------
        capacity
        pinned_weight
        evict_until_below_target
        """
        return self.fast.excess

    @property
    def pinned(self) -> set[KT]:
        """A copy of the set of keys currently pinned in fast."""
        return self.fast.pinned

    @property
    def pinned_keys(self) -> set[KT]:
        """A copy of the set of the keys that are currently pinned in fast.

        See Also
        --------
        pinned
        pin
        """
        return self.fast.pinned_keys

    @property
    def pin_counts(self) -> dict[KT, int]:
        """Nested-pin counts for the currently pinned keys."""
        return self.fast.pin_counts

    @property
    def pins(self) -> dict[KT, int]:
        """Alias for :attr:`pin_counts`."""
        return self.fast.pins

    def is_pinned(self, key: object) -> bool:
        """Whether *key* is currently pinned in fast.

        See Also
        --------
        pin
        """
        return self.fast.is_pinned(key)

    @locked
    def pin(self, key: KT) -> Pin[KT]:
        """Pin a value in the fast mapping, protecting it from being moved to slow (and
        thus evicted) while it is being borrowed.

        Pins can be nested and are counted. Use the returned context manager, or
        :meth:`unpin` / :meth:`Pin.release`, to release them::

            with buff.pin(key):
                value = buff[key]
                use(value)

        While any key is pinned, :meth:`evict_until_below_target` still evicts every
        other, non-pinned key. If the target cannot be reached because the pinned keys
        are too heavy, it returns the residual excess instead of blocking.

        Parameters
        ----------
        key: Key that must currently be present in the fast mapping.

        Raises
        ------
        KeyError
            If *key* is not in the fast mapping (e.g. it has already been moved to
            slow).

        See Also
        --------
        unpin
        capacity
        """
        # Let the LRU do the counting (so pins can be nested); the Pin handle we hand
        # back is bound to the Buffer instead, so that overridden unpin() - e.g. in
        # AsyncBuffer - is honoured when it is released.
        self.fast.pin(key)
        return Pin(self, key)

    # Terminology aliases for callers modeling borrowed/held objects.
    borrow = pin
    hold = pin
    protect = pin
    pin_key = pin
    borrow_key = pin
    hold_key = pin

    def unpin(self, key: KT | Pin[KT], evict: bool = True) -> float:
        """Release one pin previously acquired with :meth:`pin`.

        *key* may be the key itself or the :class:`~zict.Pin` handle returned by
        :meth:`pin`. By default this immediately calls
        :meth:`evict_until_below_target`, so releasing the final borrow returns the
        buffer to its configured range when possible. Pass ``evict=False`` to only
        remove the pin; the caller can then evict explicitly.

        Returns the residual excess after eviction (or immediately after unpinning when
        ``evict=False``), as per :attr:`zict.LRU.excess`.

        See Also
        --------
        release
        pin
        capacity
        """
        pin_handle: Pin[KT] | None = None
        if isinstance(key, Pin):
            pin_handle = key
            key = key.key  # type: ignore[assignment]
        with self.lock:
            excess = self.fast.unpin(key)  # type: ignore[arg-type]
        if pin_handle is not None:
            pin_handle._released = True
        if evict:
            return self.evict_until_below_target()
        return excess

    def release(self, pin_or_key: Pin[KT] | KT, evict: bool = True) -> float:
        """Release a pin handle or key. Alias-friendly wrapper around :meth:`unpin`."""
        if isinstance(pin_or_key, Pin):
            if pin_or_key._released:
                return self.excess
            return pin_or_key.release()
        return self.unpin(pin_or_key, evict)

    def release_pin(self, key: KT | Pin[KT], evict: bool = True) -> float:
        """Alias for :meth:`unpin`."""
        return self.unpin(key, evict)

    def release_borrow(self, key: KT | Pin[KT], evict: bool = True) -> float:
        """Alias for :meth:`unpin`."""
        return self.unpin(key, evict)

    unpin_key = unpin
    release_key = unpin

    @locked
    def __delitem__(self, key: KT) -> None:
        if key in self._cancel_restore:
            self._cancel_restore[key] = True
        try:
            del self.fast[key]
        except KeyError:
            del self.slow[key]

    @locked
    def _cancel_evict(self, key: KT, value: VT) -> None:
        discard(self.slow, key)

    def values(self) -> ValuesView[VT]:
        return BufferValuesView(self)

    def items(self) -> ItemsView[KT, VT]:
        return BufferItemsView(self)

    def __len__(self) -> int:
        with self.lock, self.fast.lock:
            return (
                len(self.fast)
                + len(self.slow)
                - sum(
                    k in self.fast and k in self.slow
                    for k in chain(self._cancel_restore, self.fast._cancel_evict)
                )
            )

    def __iter__(self) -> Iterator[KT]:
        """Make sure that the iteration is not disrupted if you evict/restore a key in
        the middle of it
        """
        seen = set()
        while True:
            try:
                for d in (self.fast, self.slow):
                    for key in d:
                        if key not in seen:
                            seen.add(key)
                            yield key
                return
            except RuntimeError:
                pass

    def __contains__(self, key: object) -> bool:
        return key in self.fast or key in self.slow

    def __str__(self) -> str:
        return f"Buffer<{self.fast}, {self.slow}>"

    __repr__ = __str__

    def flush(self) -> None:
        flush(self.fast, self.slow)

    def close(self) -> None:
        close(self.fast, self.slow)


class BufferItemsView(ItemsView[KT, VT]):
    _mapping: Buffer  # FIXME CPython implementation detail
    __slots__ = ()

    def __iter__(self) -> Iterator[tuple[KT, VT]]:
        # Avoid changing the LRU
        return chain(self._mapping.fast.items(), self._mapping.slow.items())


class BufferValuesView(ValuesView[VT]):
    _mapping: Buffer  # FIXME CPython implementation detail
    __slots__ = ()

    def __contains__(self, value: object) -> bool:
        # Avoid changing the LRU
        return any(value == v for v in self)

    def __iter__(self) -> Iterator[VT]:
        # Avoid changing the LRU
        return chain(self._mapping.fast.values(), self._mapping.slow.values())
