from __future__ import annotations

from collections import defaultdict
from collections.abc import Callable, Iterable, Iterator, Mapping, MutableMapping
from typing import Generic, TypeVar

from zict.common import KT, VT, ZictBase, close, discard, flush, locked

MKT = TypeVar("MKT")


class Sieve(ZictBase[KT, VT], Generic[KT, VT, MKT]):
    """Store values in different mappings based on a selector's
    output.

    This creates a MutableMapping combining several underlying
    MutableMappings for storage.  Items are dispatched based on
    a selector function provided by the user.

    Parameters
    ----------
    mappings: dict of {mapping key: MutableMapping}
    selector: callable (key, value) -> mapping key

    Notes
    -----
    If you call methods of this class from multiple threads, access will be fast as long
    as the ``__contains__`` and ``__delitem__`` methods of all underlying mappins are
    fast. ``__getitem__`` and ``__setitem__`` methods of the underlying mappings are not
    protected by locks.

    Examples
    --------
    >>> small = {}
    >>> large = DataBase()                        # doctest: +SKIP
    >>> mappings = {True: small, False: large}    # doctest: +SKIP
    >>> def is_small(key, value):                 # doctest: +SKIP
    ...     return sys.getsizeof(value) < 10000   # doctest: +SKIP
    >>> d = Sieve(mappings, is_small)             # doctest: +SKIP
    """

    mappings: Mapping[MKT, MutableMapping[KT, VT]]
    selector: Callable[[KT, VT], MKT]
    key_to_mapping: dict[KT, MutableMapping[KT, VT]]
    gen: int

    def __init__(
        self,
        mappings: Mapping[MKT, MutableMapping[KT, VT]],
        selector: Callable[[KT, VT], MKT],
    ):
        super().__init__()
        self.mappings = mappings
        self.selector = selector
        self.key_to_mapping = {}
        self.gen = 0

    def __getitem__(self, key: KT) -> VT:
        # Note that this may raise KeyError if you call it in the middle of __setitem__
        # or update for an already existing key
        return self.key_to_mapping[key][key]

    @locked
    def __setitem__(self, key: KT, value: VT) -> None:
        discard(self, key)
        mkey = self.selector(key, value)
        mapping = self.mappings[mkey]
        self.key_to_mapping[key] = mapping
        self.gen += 1
        gen = self.gen

        with self.unlock():
            mapping[key] = value

        if gen != self.gen and self.key_to_mapping.get(key) is not mapping:
            # Multithreaded race condition
            discard(mapping, key)

    @locked
    def __delitem__(self, key: KT) -> None:
        mapping = self.key_to_mapping.pop(key)
        self.gen += 1
        discard(mapping, key)

    @locked
    def _do_update(self, items: Iterable[tuple[KT, VT]]) -> None:
        # Optimized update() implementation issuing a single update()
        # call per underlying mapping.
        updates = defaultdict(list)
        self.gen += 1
        gen = self.gen

        for key, value in items:
            old_mapping = self.key_to_mapping.pop(key, None)
            if old_mapping is not None:
                discard(old_mapping, key)
            mkey = self.selector(key, value)
            mapping = self.mappings[mkey]
            updates[mkey].append((key, value))
            self.key_to_mapping[key] = mapping

        with self.unlock():
            for mkey, mitems in updates.items():
                mapping = self.mappings[mkey]
                mapping.update(mitems)

        if gen != self.gen:
            # Multithreaded race condition
            for mkey, mitems in updates.items():
                mapping = self.mappings[mkey]
                for key, _ in mitems:
                    if self.key_to_mapping.get(key) is not mapping:
                        discard(mapping, key)

    def migrate(self) -> None:
        """Move every key to the mapping picked by the current :attr:`selector`.

        This is useful after swapping :attr:`selector` for a new distribution
        rule: keys written before the change still live in the mappings chosen
        by the old rule, while new writes already follow the new rule. This
        method relocates the old keys so that every value ends up in the
        mapping chosen by the current rule.

        The migration is incremental and resumable:

        - keys are moved one at a time and keys already in the right mapping
          are skipped, so a call that fails partway through can simply be
          retried; only the keys that still need migration are moved;
        - if writing a key to its new mapping fails, the key is left untouched
          in its original mapping, where it remains fully readable and
          writable;
        - if a key is overwritten or deleted by another thread while it is
          being migrated, the concurrent write wins: the migration of that key
          is aborted (to be retried later if still needed) and the value
          written by the other thread is never reverted.

        Raises
        ------
        Exception
            If one or more keys could not be migrated, all remaining keys are
            still attempted and the first exception encountered is re-raised
            at the end.
        """
        first_exc: Exception | None = None
        for key in list(self.key_to_mapping):
            try:
                self._migrate_key(key)
            except Exception as exc:
                if first_exc is None:
                    first_exc = exc
        if first_exc is not None:
            raise first_exc

    @locked
    def _migrate_key(self, key: KT) -> None:
        try:
            old_mapping = self.key_to_mapping[key]
        except KeyError:
            return  # The key was deleted by another thread
        value = old_mapping[key]
        new_mapping = self.mappings[self.selector(key, value)]
        if new_mapping is old_mapping:
            return  # Already in the right mapping
        self.gen += 1
        gen = self.gen

        with self.unlock():
            new_mapping[key] = value

        if self.gen != gen:
            # The key was overwritten or deleted by another thread while we
            # were copying it. The concurrent write wins: drop our stale copy
            # (unless the key now points to the new mapping anyway) and leave
            # the key unmigrated, so that the move can be retried later.
            if self.key_to_mapping.get(key) is not new_mapping:
                discard(new_mapping, key)
            return
        self.key_to_mapping[key] = new_mapping
        discard(old_mapping, key)

    def __len__(self) -> int:
        return len(self.key_to_mapping)

    def __iter__(self) -> Iterator[KT]:
        return iter(self.key_to_mapping)

    def __contains__(self, key: object) -> bool:
        return key in self.key_to_mapping

    def __str__(self) -> str:
        return f"Sieve<{self.mappings}>"

    __repr__ = __str__

    def flush(self) -> None:
        flush(*self.mappings.values())

    def close(self) -> None:
        close(*self.mappings.values())
