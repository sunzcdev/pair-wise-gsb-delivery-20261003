import time
from collections import OrderedDict
from collections.abc import MutableMapping


class DirCache(MutableMapping):
    """
    Caching of directory listings, in a structure like::

        {"path0": [
            {"name": "path0/file0",
             "size": 123,
             "type": "file",
             ...
            },
            {"name": "path0/file1",
            },
            ...
            ],
         "path1": [...]
        }

    Parameters to this class control listing expiry or indeed turn
    caching off.

    The class also provides hierarchical invalidation, with three
    clearly separated scopes:

    - ``invalidate_directory``: exactly one directory's listing;
    - ``invalidate_subtree``: a directory and everything beneath it;
    - ``invalidate_ancestors``: the chain of directories containing a
      path (its parent, grandparent, ... up to the root).

    Directory identity is decided by a ``canonicalize`` callable supplied
    by the filesystem (e.g., ``AbstractFileSystem._strip_protocol``), so
    that root markers, trailing separators and backend-specific spellings
    of the same directory compare equal. Whether a key belongs to a
    subtree is decided by path-component boundaries (an adjacent-name
    check), never by raw string prefixes, so ``/a/b`` contains ``/a/b/c``
    but not ``/a/bc``.
    """

    def __init__(
        self,
        use_listings_cache=True,
        listings_expiry_time=None,
        max_paths=None,
        **kwargs,
    ):
        """
        Parameters
        ----------
        use_listings_cache: bool
            If False, this cache never returns items, but always reports KeyError,
            and setting items has no effect.
        listings_expiry_time: int or float (optional)
            Time in seconds that a listing is considered valid. If None,
            listings do not expire. Time is measured from when the entry was set.
        max_paths: int (optional)
            The maximum number of directory listings to retain in the cache.
            When the cache exceeds this limit, the least recently used
            (set or accessed) listings are evicted. If None, 0, or negative,
            there is no limit.
        """
        # max_paths is normalized to be either positive or None.
        self.max_paths = max_paths if max_paths and max_paths > 0 else None
        self._cache = OrderedDict() if self.max_paths else {}
        self._times = {}
        self.use_listings_cache = use_listings_cache
        self.listings_expiry_time = listings_expiry_time

    def __getitem__(self, item):
        if not self.use_listings_cache:
            raise KeyError(item)

        if self.listings_expiry_time is not None and item in self._cache:
            if time.time() - self._times.get(item, 0) > self.listings_expiry_time:
                del self[item]
                raise KeyError(item)

        val = self._cache[item]
        if self.max_paths:
            self._cache.move_to_end(item)
        return val

    def clear(self):
        self._cache.clear()
        self._times.clear()

    def __len__(self):
        return len(self._cache)

    def __contains__(self, item):
        if not self.use_listings_cache or item not in self._cache:
            return False

        if self.listings_expiry_time is not None:
            if time.time() - self._times.get(item, 0) > self.listings_expiry_time:
                del self[item]
                return False

        return True

    def __setitem__(self, key, value):
        if not self.use_listings_cache:
            return

        if isinstance(value, list):
            # Store our own copy, so that nothing done with cached
            # listings (reading them out, handing them to callers) can
            # ever mutate the original list the caller passed in, and
            # vice versa.
            value = list(value)
        self._cache[key] = value
        if self.listings_expiry_time is not None:
            self._times[key] = time.time()

        if self.max_paths:
            self._cache.move_to_end(key)
            if len(self._cache) > self.max_paths:
                oldest, _ = self._cache.popitem(last=False)
                self._times.pop(oldest, None)

    def __delitem__(self, key):
        del self._cache[key]
        self._times.pop(key, None)

    def __iter__(self):
        if not self.use_listings_cache:
            return

        if self.listings_expiry_time is None:
            yield from self._cache
            return

        now = time.time()
        for key in list(self._cache):
            if now - self._times.get(key, 0) > self.listings_expiry_time:
                del self[key]
            else:
                yield key

    @staticmethod
    def _identity(path, canonicalize=None):
        """Unified identity for a cache key.

        The filesystem provides ``canonicalize`` (e.g., its
        ``_strip_protocol``) so that root paths, trailing separators and
        backend-specific spellings of one directory all map to the same
        key. Without one, fall back to stripping trailing separators
        (keeping "/" as the root).
        """
        if canonicalize is not None:
            return canonicalize(path)
        path = str(path)
        if path != "/":
            path = path.rstrip("/")
        return path

    @staticmethod
    def _default_parent(path):
        """Containing directory of a canonical path, None at the root."""
        if path in ("", "/"):
            return None
        if "/" in path:
            return path.rsplit("/", 1)[0] or "/"
        return ""

    @staticmethod
    def _in_subtree(path, candidate):
        """Whether ``candidate`` is ``path`` itself or lies beneath it.

        Adjacent-name check: the name adjacent to ``path`` within
        ``candidate`` must be separated by "/", so "/a/b" contains
        "/a/b/c" but never "/a/bc". The root contains every listing.
        """
        if path in ("", "/"):
            return True
        return candidate == path or candidate.startswith(path + "/")

    def _purge_expired(self):
        """Drop expired entries outright.

        Expired entries are never kept alive: purging must not refresh
        their timestamps or LRU positions.
        """
        if self.listings_expiry_time is None:
            return
        now = time.time()
        for key in [
            key
            for key, at in self._times.items()
            if now - at > self.listings_expiry_time
        ]:
            self._cache.pop(key, None)
            self._times.pop(key, None)

    def _drop(self, key):
        self._cache.pop(key, None)
        self._times.pop(key, None)

    def invalidate_directory(self, path, *, canonicalize=None):
        """Drop the cached listing of exactly one directory.

        Listings of directories beneath it and of its ancestors are
        kept; use ``invalidate_subtree`` or ``invalidate_ancestors``
        for those scopes.

        Parameters
        ----------
        path: str
            The directory whose listing to discard.
        canonicalize: callable, optional
            Filesystem-provided identity function for paths (see
            ``_identity``).
        """
        self._purge_expired()
        self._drop(self._identity(path, canonicalize))

    def invalidate_subtree(self, path, *, canonicalize=None):
        """Drop the cached listings of a directory and all beneath it.

        Membership of the subtree is decided by path-component
        boundaries, not raw string prefixes (see ``_in_subtree``).
        Invalidating the root drops every listing.

        Parameters
        ----------
        path: str
            The directory at the root of the subtree to discard.
        canonicalize: callable, optional
            Filesystem-provided identity function for paths (see
            ``_identity``).
        """
        self._purge_expired()
        path = self._identity(path, canonicalize)
        for key in list(self._cache):
            if self._in_subtree(path, key):
                self._drop(key)

    def invalidate_ancestors(self, path, *, canonicalize=None, parent=None):
        """Drop the cached listings along the ancestor chain of ``path``.

        This affects the directories containing ``path`` (its parent,
        grandparent, ... up to the root), whose listings may refer to
        it; the listing for ``path`` itself is left alone (use
        ``invalidate_directory`` or ``invalidate_subtree`` for that).

        Parameters
        ----------
        path: str
            The path whose containing directories to discard.
        canonicalize: callable, optional
            Filesystem-provided identity function for paths (see
            ``_identity``).
        parent: callable, optional
            Filesystem-provided function computing the containing
            directory of a canonical path (e.g.,
            ``AbstractFileSystem._parent``). Defaults to a generic
            splitter. The walk stops at the root or if the function
            reaches a fixed point or cycles.
        """
        self._purge_expired()
        if parent is None:
            parent = self._default_parent
        key = self._identity(path, canonicalize)
        seen = {key}
        while True:
            key = parent(key)
            if key is None or key in seen:
                break
            seen.add(key)
            self._drop(key)

    def __reduce__(self):
        return (
            DirCache,
            (self.use_listings_cache, self.listings_expiry_time, self.max_paths),
        )
