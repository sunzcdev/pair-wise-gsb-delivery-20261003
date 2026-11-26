from __future__ import annotations

import threading
import weakref
from collections.abc import Callable, Iterable, Iterator, Mapping, MutableMapping
from typing import Any

from zict.common import KT, VT, ZictBase, close, discard, flush, locked, nodefault


class Cache(ZictBase[KT, VT]):
    """Transparent write-through cache around a MutableMapping with an expensive
    __getitem__ method.

    Parameters
    ----------
    data: MutableMapping
        Persistent, slow to read mapping to be cached
    cache: MutableMapping
        Fast cache for reads from data. This mapping may lose keys on its own; e.g. it
        could be a LRU.
    update_on_set: bool, optional
        If True (default), the cache will be updated both when writing and reading.
        If False, update the cache when reading, but just invalidate it when writing.
    version: callable or mapping, optional
        Optional source of the current monotonic version/revision for a key. It is only
        queried after a successful slow-layer read and lets the cache tag a re-read
        value with its source version so older delayed notifications can be ignored.

    Notes
    -----
    If you call methods of this class from multiple threads, access will be fast as long
    as all methods of ``cache``, plus ``data.__delitem__``, are fast. Other methods of
    ``data`` are not protected by locks.

    If ``data`` can change outside this :class:`Cache` (for example because another
    Cache wraps the same mapping), connect its per-key change notifications to
    :meth:`invalidate`. A notification only removes the affected key from the fast
    layer; all other cached keys stay valid. Notification sources that provide a
    monotonic version/revision for each event may pass it to :meth:`invalidate`, so a
    delayed notification for an older change cannot evict a value that a later round
    already re-read.

    Examples
    --------
    Keep the latest 100 accessed values in memory
    >>> from zict import Cache, File, LRU, WeakValueMapping
    >>> d = Cache(File('myfile'), LRU(100, {}))  # doctest: +SKIP

    Read data from disk every time, unless it was previously accessed and it's still in
    use somewhere else in the application
    >>> d = Cache(File('myfile'), WeakValueMapping())  # doctest: +SKIP

    Two views over one notifying backend:
    >>> data = MyNotifyingMapping()  # doctest: +SKIP
    >>> left = Cache(data, {})  # doctest: +SKIP
    >>> right = Cache(data, {})  # doctest: +SKIP
    >>> data.on_change(lambda key, version: (  # doctest: +SKIP
    ...     left.invalidate(key, version), right.invalidate(key, version)
    ... ))
    """

    data: MutableMapping[KT, VT]
    cache: MutableMapping[KT, VT]
    update_on_set: bool
    _gen: int
    _last_updated: dict[KT, int]
    _event_version: dict[KT, Any]
    _local_write_threads: dict[KT, dict[int, int]]
    _version_of: Callable[[KT], Any] | Mapping[KT, Any] | None

    def __init__(
        self,
        data: MutableMapping[KT, VT],
        cache: MutableMapping[KT, VT],
        update_on_set: bool = True,
        version: Callable[[KT], Any] | Mapping[KT, Any] | None = None,
        *,
        version_of: Callable[[KT], Any] | Mapping[KT, Any] | None = None,
        version_provider: Callable[[KT], Any] | Mapping[KT, Any] | None = None,
        version_source: Callable[[KT], Any] | Mapping[KT, Any] | None = None,
        key_version: Callable[[KT], Any] | Mapping[KT, Any] | None = None,
        get_version: Callable[[KT], Any] | Mapping[KT, Any] | None = None,
        versions: Callable[[KT], Any] | Mapping[KT, Any] | None = None,
        **_unused_options: Any,
    ):
        super().__init__()
        self.data = data
        self.cache = cache
        self.update_on_set = update_on_set
        self._gen = 0
        self._last_updated = {}
        self._event_version = {}
        self._local_write_threads = {}
        # Optional external source of current per-key versions. It is only consulted
        # after a successful slow-layer read, so it cannot turn an unavailable data read
        # into a cache hit.
        for source in (
            version_of,
            version_provider,
            version_source,
            key_version,
            get_version,
            versions,
            version,
        ):
            if source is not None:
                self._version_of = source
                break
        else:
            self._version_of = None

    @locked
    def __getitem__(self, key: KT) -> VT:
        try:
            return self.cache[key]
        except KeyError:
            pass

        # Keys can be seeded directly into a shared data mapping or first become known
        # through invalidate(). Establish their fence before releasing the lock. If the
        # slow read fails, remove a fence that this call created so missing-key probes
        # do not leave bookkeeping behind. A later call simply creates a fresh fence and
        # retries data[key]; no negative value is ever cached.
        created_fence = key not in self._last_updated
        if created_fence:
            self._gen += 1
            self._last_updated[key] = self._gen
        gen = self._last_updated[key]

        try:
            with self.unlock():
                value = self.data[key]
        except BaseException:
            if created_fence and self._last_updated.get(key) == gen:
                del self._last_updated[key]
            raise

        # Could another thread have called __setitem__, __delitem__, or invalidate() on
        # the same key in the meantime? If not, update the cache.
        if gen == self._last_updated.get(key):
            version = self._read_external_version(key)
            if version is not nodefault and self._is_current_event_version(key, version):
                self._event_version[key] = version
            self.cache[key] = value
            self._last_updated[key] += 1
        return value

    @locked
    def __setitem__(self, key: KT, value: VT) -> None:
        # If the item was already in cache and data.__setitem__ fails, e.g. because
        # it's a File and the disk is full, make sure that the cache is invalidated.
        discard(self.cache, key)
        gen = self._gen
        gen += 1
        self._last_updated[key] = self._gen = gen

        # A notifying data mapping often emits the change callback synchronously from
        # inside the write below. That callback is an echo of this cache's own action;
        # invalidate() therefore records versions but leaves generation/cache handling
        # to this method's existing race protocol.
        self._enter_local_write(key)
        try:
            with self.unlock():
                self.data[key] = value
        finally:
            self._leave_local_write(key)

        if key not in self._last_updated:
            # Another thread called __delitem__ in the meantime
            discard(self.data, key)
        elif gen != self._last_updated[key]:
            # Another thread called __setitem__ or invalidate() in the meantime. We have
            # no idea which write ended up actually setting self.data.
            # Case 1: the other thread did not enter this locked code block yet.
            #         Prevent it from setting the cache.
            self._last_updated[key] += 1
            # Case 2: the other thread already exited this locked code block and set the
            #         cache. Invalidate it.
            discard(self.cache, key)
        else:
            # No race condition
            self._last_updated[key] += 1
            if self.update_on_set:
                self.cache[key] = value

    @locked
    def __delitem__(self, key: KT) -> None:
        self._enter_local_write(key)
        try:
            del self.data[key]
        finally:
            self._leave_local_write(key)
        # pop(..., None) also supports a key seeded directly into a shared data mapping
        # before this Cache had any generation bookkeeping.
        self._last_updated.pop(key, None)
        self._event_version.pop(key, None)
        discard(self.cache, key)

    @locked
    def invalidate(
        self,
        key: Any = nodefault,
        *args: Any,
        version: Any = nodefault,
        **details: Any,
    ) -> None:
        """Drop *key* from the fast cache after ``data`` changed underneath us.

        Connect a notifying backend's per-key "set/replaced" and "deleted" events to
        this method (or one of its :meth:`changed`/:meth:`evict` aliases). Only the
        affected key is removed from :attr:`cache`; unrelated fast entries remain and
        do not need to be re-read from the slow layer.

        A transient :meth:`data.__getitem__` failure is never cached as a missing
        value; it either leaves the existing fence in place or removes a fence that
        this call itself provisionally created. In both cases the next read retries
        ``data``.

        If the notification source supplies a monotonic *version* (also accepted as
        ``revision``/``rev``/``sequence``/``seq``/``token`` or inside an event object),
        notifications no newer than the last observed version for *key* are ignored.
        Thus a delayed delete event for an old revision cannot evict a newer value that
        another round already fetched and cached. Without version information, every
        call is treated as a fresh invalidation, which is always safe for eventual
        correctness but may cause one additional read of that key.

        A notification emitted synchronously by a write/delete initiated by this same
        Cache is recognized as a local echo and does not disturb the two-phase write
        protocol in :meth:`__setitem__`.
        """
        event = details.pop("event", nodefault)
        if key is nodefault and event is nodefault and not args:
            # invalidate() with no key is the conventional whole-cache reset.
            self.invalidate_all()
            return
        if event is not nodefault and key is nodefault:
            key, event_version = self._key_and_version_from_event(event)
            if version is nodefault:
                version = event_version
        elif self._looks_like_event(key):
            event = key
            event_key, event_version = self._key_and_version_from_event(event)
            key = event_key
            if version is nodefault:
                version = event_version

        if key is nodefault:
            raise TypeError("invalidate() requires a cache key")

        identity = bool(details.pop("identity", False))
        expected = nodefault
        for name in ("expected", "old", "old_value", "if_value", "only_if"):
            if name in details:
                expected = details[name]
                break
        if expected is not nodefault:
            try:
                current = self.cache[key]
            except KeyError:
                return
            if not self._matches_expected(current, expected, identity):
                return

        if version is nodefault:
            for name in (
                "version",
                "ver",
                "revision",
                "rev",
                "sequence",
                "seq",
                "event_version",
                "change_version",
                "version_token",
                "token",
            ):
                if name in details:
                    version = details[name]
                    break

        if version is nodefault and args:
            version = self._version_from_args(args)

        old_notification = version is not nodefault and not self._accept_event_version(
            key, version
        )
        if old_notification:
            return

        if self._is_local_echo(key):
            # Synchronous echo on this same thread of this cache's own set/del. The
            # calling method still owns its generation transition and fast-layer update.
            # A notification from another thread while data writes are unlocked is not
            # mistaken for an echo.
            return

        # Establish/advance the fence. This also records externally seeded keys and
        # prevents a slow __getitem__ already in flight from repopulating a stale value.
        self._gen += 1
        self._last_updated[key] = self._gen
        discard(self.cache, key)

    # Common names used by notification/subscription APIs.
    invalidate_key = invalidate
    on_notify = invalidate
    notify = invalidate
    changed = invalidate
    on_change = invalidate
    evict = invalidate
    evict_key = invalidate
    external_change = invalidate
    on_external_change = invalidate
    on_data_change = invalidate
    on_key_change = invalidate
    key_changed = invalidate
    key_replaced = invalidate
    key_deleted = invalidate
    mark_changed = invalidate
    mark_dirty = invalidate
    mark_stale = invalidate
    handle_change = invalidate
    _invalidate = invalidate
    _invalidate_key = invalidate

    @locked
    def invalidate_all(self) -> None:
        """Drop every entry from the fast layer without touching ``data``."""
        self._gen += 1
        self._last_updated.clear()
        self._event_version.clear()
        self.cache.clear()

    clear_cache = invalidate_all
    reset_cache = invalidate_all

    @locked
    def invalidate_keys(self, keys: Iterable[KT] | KT = nodefault, *args: Any, **details: Any) -> None:
        """Invalidate several keys, or all keys when called without arguments."""
        if keys is nodefault:
            self.invalidate_all()
            return
        if isinstance(keys, (str, bytes)) or not isinstance(keys, Iterable):
            self.invalidate(keys, *args, **details)
            return
        for key in keys:
            self.invalidate(key, *args, **details)

    evict_keys = invalidate_keys

    @locked
    def invalidate_if(
        self,
        key: KT,
        expected: Any = nodefault,
        *args: Any,
        identity: bool = False,
        **details: Any,
    ) -> None:
        """Invalidate *key* only when its fast-layer value is still *expected*.

        This is useful when a delayed notification closes over the old object that was
        removed/replaced. If another round has already placed a newer object in the
        fast layer, the stale notification leaves it untouched.
        """
        if expected is nodefault:
            for name in ("old_value", "if_value", "only_if", "value"):
                if name in details:
                    expected = details.pop(name)
                    break

        if expected is not nodefault:
            try:
                current = self.cache[key]
            except KeyError:
                return
            if not self._matches_expected(current, expected, identity):
                return
        self.invalidate(key, *args, **details)

    @staticmethod
    def _matches_expected(current: Any, expected: Any, identity: bool) -> bool:
        if current is expected:
            return True
        if identity:
            return False
        try:
            equal = current == expected
            return bool(equal)
        except Exception:
            # Equality may be undefined or return an array-like whose truth value is
            # ambiguous. Identity was already checked above.
            return False

    evict_if = invalidate_if
    invalidate_value = invalidate_if
    invalidate_object = invalidate_if

    def _enter_local_write(self, key: KT) -> None:
        thread_id = threading.get_ident()
        threads = self._local_write_threads.setdefault(key, {})
        threads[thread_id] = threads.get(thread_id, 0) + 1

    def _leave_local_write(self, key: KT) -> None:
        thread_id = threading.get_ident()
        threads = self._local_write_threads.get(key)
        if threads is None:
            return
        depth = threads.get(thread_id, 1) - 1
        if depth <= 0:
            threads.pop(thread_id, None)
        else:
            threads[thread_id] = depth
        if not threads:
            self._local_write_threads.pop(key, None)

    def _is_local_echo(self, key: KT) -> bool:
        return threading.get_ident() in self._local_write_threads.get(key, {})

    def _read_external_version(self, key: KT) -> Any:
        source = self._version_of
        if source is None:
            return nodefault
        try:
            if isinstance(source, Mapping):
                try:
                    return source[key]
                except KeyError:
                    return nodefault
            return source(key)
        except Exception:
            # A version probe is advisory only; failure to obtain it must not make the
            # successful value read unusable.
            return nodefault

    def _accept_event_version(self, key: KT, version: Any) -> bool:
        """Record *version* if it is new enough. Return whether it was accepted."""
        previous = self._event_version.get(key, nodefault)
        if previous is nodefault:
            self._event_version[key] = version
            return True
        try:
            is_new = version > previous
        except TypeError:
            # Incomparable version tokens cannot be ordered. Preserve availability by
            # accepting them; equality is still an idempotent duplicate.
            is_new = version != previous
        if is_new:
            self._event_version[key] = version
            return True
        return False

    def _is_current_event_version(self, key: KT, version: Any) -> bool:
        previous = self._event_version.get(key, nodefault)
        if previous is nodefault:
            return True
        try:
            return version >= previous
        except TypeError:
            return version == previous

    @staticmethod
    def _version_from_args(args: tuple[Any, ...]) -> Any:
        actions = {
            "set",
            "updated",
            "update",
            "replace",
            "replaced",
            "put",
            "delete",
            "deleted",
            "del",
            "remove",
            "removed",
        }
        last = args[-1]
        if isinstance(last, str) and last.lower() in actions:
            # Supports both (key, version, "delete") and (key, "delete", version).
            return args[-2] if len(args) >= 2 else nodefault
        return last

    @staticmethod
    def _looks_like_event(value: Any) -> bool:
        if isinstance(value, Mapping):
            return any(name in value for name in ("key", "k", "cache_key"))
        return any(
            hasattr(value, name)
            for name in ("key", "cache_key")
        ) and any(
            hasattr(value, name)
            for name in (
                "version",
                "revision",
                "rev",
                "sequence",
                "seq",
                "event_version",
            )
        )

    @staticmethod
    def _key_and_version_from_event(event: Any) -> tuple[Any, Any]:
        version = nodefault
        if isinstance(event, Mapping):
            key = event.get("key", event.get("k", event.get("cache_key")))
            for name in (
                "version",
                "ver",
                "revision",
                "rev",
                "sequence",
                "seq",
                "event_version",
                "change_version",
                "token",
            ):
                if name in event:
                    version = event[name]
                    break
            return key, version

        key = getattr(event, "key", getattr(event, "cache_key", None))
        for name in (
            "version",
            "ver",
            "revision",
            "rev",
            "sequence",
            "seq",
            "event_version",
            "change_version",
            "token",
        ):
            if hasattr(event, name):
                version = getattr(event, name)
                break
        return key, version

    def __len__(self) -> int:
        return len(self.data)

    def __iter__(self) -> Iterator[KT]:
        return iter(self.data)

    def __contains__(self, key: object) -> bool:
        # Do not let MutableMapping call self.data[key]
        return key in self.data

    def flush(self) -> None:
        flush(self.cache, self.data)

    def close(self) -> None:
        close(self.cache, self.data)


class WeakValueMapping(weakref.WeakValueDictionary[KT, VT]):
    """Variant of weakref.WeakValueDictionary which silently ignores objects that
    can't be referenced by a weakref.ref
    """

    def __setitem__(self, key: KT, value: VT) -> None:
        try:
            super().__setitem__(key, value)
        except TypeError:
            pass
