"""Base classes for all cache backends

.. automodsumm:: requests_cache.backends.base
   :classes-only:
   :nosignatures:
"""

from __future__ import annotations

from abc import ABC
from collections import UserDict
from datetime import datetime
from logging import getLogger
from pickle import PickleError
from threading import local
from typing import TYPE_CHECKING, Iterable, Iterator, List, MutableMapping, Optional, TypeVar, Union
from warnings import warn

from pathlib import Path

from requests import Request, Response

from ..cache_keys import create_key, redact_response
from ..models import AnyRequest, CachedResponse
from ..policy import (
    DEFAULT_CACHE_NAME,
    STRUCTURE_ERRORS,
    CacheSettings,
    CorruptionCallback,
    CorruptionPolicy,
    CorruptEntryError,
    ExpirationTime,
    log_corruption,
    notify_corruption,
)
from ..serializers import SerializerType, init_serializer

# Specific exceptions that may be raised during deserialization
DESERIALIZE_ERRORS = (AttributeError, EOFError, ImportError, PickleError, TypeError, ValueError)

# Deserialization failures that are isolated to a single cache entry
CORRUPT_ENTRY_ERRORS = DESERIALIZE_ERRORS + STRUCTURE_ERRORS

logger = getLogger(__name__)


class BaseCache:
    """Base class for cache backends. Can be used as a non-persistent, in-memory cache.

    This manages higher-level cache operations, including:

    * Saving and retrieving responses
    * Managing redirect history
    * Convenience methods for general cache info
    * Dict-like wrapper methods around the underlying storage

    Notes:

    * Lower-level storage operations are handled by :py:class:`.BaseStorage`.
    * To extend this with your own custom backend, see :ref:`custom-backends`.

    Args:
        cache_name: Cache prefix or namespace, depending on backend
        serializer: Serializer name or instance
        corruption_policy: How to handle a single entry that can't be deserialized: ``report``
            (default), ``delete``, or ``raise``. See :py:class:`.CorruptionPolicy`.
        on_corruption: Optional callback that receives a :py:class:`.CorruptEntryError` for each
            entry that can't be deserialized
        kwargs: Additional backend-specific keyword arguments
    """

    def __init__(self, cache_name: str = DEFAULT_CACHE_NAME, **kwargs):
        self.cache_name = cache_name
        self._corruption_guard = local()
        self.responses: BaseStorage[str, CachedResponse] = DictStorage()
        self.redirects: BaseStorage[str, str] = DictStorage()
        # Init and public access is done in CachedSession. Only corrupt-entry settings are picked
        # up here, so a backend can be configured directly without a session.
        settings_kwargs = {
            k: kwargs[k]
            for k in ('corruption_policy', 'on_corruption', 'read_only')
            if k in kwargs
        }
        self._settings = CacheSettings.from_kwargs(**settings_kwargs)

    @property
    def responses(self) -> BaseStorage:
        """Storage for cached responses"""
        return self._responses

    @responses.setter
    def responses(self, storage: BaseStorage):
        # Keep storage connected to the policy managed at the cache level, for any backend
        storage.corruption_handler = self._handle_corrupt_entry
        self._responses = storage

    @property
    def redirects(self) -> BaseStorage:
        """Storage for redirect aliases"""
        return self._redirects

    @redirects.setter
    def redirects(self, storage: BaseStorage):
        storage.corruption_handler = self._handle_corrupt_entry
        self._redirects = storage

    # Corrupt entry handling
    # ---------------------

    @property
    def corruption_policy(self) -> CorruptionPolicy:
        """How to handle cache entries that can't be deserialized"""
        return self._settings.corruption_policy

    @corruption_policy.setter
    def corruption_policy(self, value):
        self._settings.corruption_policy = CorruptionPolicy.parse(value)

    @property
    def on_corruption(self) -> Optional[CorruptionCallback]:
        """Callback that receives a :py:class:`.CorruptEntryError` for each corrupt entry"""
        return self._settings.on_corruption

    @on_corruption.setter
    def on_corruption(self, value):
        self._settings.on_corruption = value

    def _handle_corrupt_entry(self, event: CorruptEntryError) -> Optional[CachedResponse]:
        """Apply the configured policy to a single corrupt entry.

        A corrupt entry is always isolated: with the default policies the failure never propagates
        as a batch failure, and it never results in clearing the cache. Any error raised while
        isolating the entry is logged, not re-raised.
        """
        policy = self._settings.corruption_policy

        # Don't allow a callback or a recovery operation to recursively trigger recovery
        if getattr(self._corruption_guard, 'active', False):
            if policy == CorruptionPolicy.RAISE:
                raise event from event.error
            return None

        self._corruption_guard.active = True
        try:
            notify_corruption(self._settings.on_corruption, event, logger)
            if policy == CorruptionPolicy.RAISE:
                raise event from event.error
            if policy == CorruptionPolicy.DELETE:
                self._delete_corrupt_entry_safely(event)
        finally:
            self._corruption_guard.active = False
        return None

    def _delete_corrupt_entry_safely(self, event: CorruptEntryError):
        """Delete a corrupt entry and its redirect aliases, without escalating failures"""
        if self._settings.read_only:
            logger.info(
                'Not deleting corrupt cache entry in read-only mode '
                '(key=%r, stage=delete, error=%s)',
                event.key,
                event.exception_name,
                extra={
                    'cache_key': event.key,
                    'stage': 'delete',
                    'exception_type': event.exception_name,
                },
            )
            return

        try:
            self._delete_corrupt_entry(event.key)
        except Exception as delete_error:
            # A failed recovery must not fall back to clearing the database or re-entering
            # corruption handling
            logger.error(
                'Failed to delete corrupt cache entry (key=%r, stage=delete, error=%s): %s',
                event.key,
                type(delete_error).__name__,
                delete_error,
                extra={
                    'cache_key': event.key,
                    'stage': 'delete',
                    'exception_type': type(delete_error).__name__,
                },
            )
            logger.debug(delete_error, exc_info=True)

    def _delete_corrupt_entry(self, key: str):
        """Delete a corrupt response and any redirect aliases that resolve to it"""
        self.responses.bulk_delete([key])
        aliases = self._get_redirect_aliases([key])
        if aliases:
            self.redirects.bulk_delete(aliases)

    def _get_redirect_aliases(self, target_keys: Iterable[str]) -> List[str]:
        """Get all redirect aliases that resolve to the given target key(s), directly or through
        other aliases.

        Aliases are collected iteratively (never recursively), and each alias is visited at most
        once, so self-references and cycles cannot cause unbounded recursion.
        """
        edges = list(self.redirects.items())
        seeds = {k for k in target_keys if k is not None}
        targets = set(seeds)
        aliases: List[str] = []
        while targets:
            next_targets = set()
            for alias, target in edges:
                if alias in aliases or alias in seeds:
                    continue
                if target in targets:
                    aliases.append(alias)
                    next_targets.add(alias)
            targets = next_targets
        return aliases

    @property
    def db_path(self) -> Union[str, Path]:
        """Path to the cache database file.

        Raises:
            NotImplementedError: For backends that do not use a file-based database.
        """
        raise NotImplementedError

    # Main cache operations
    # ---------------------

    def get_response(self, key: str, default=None) -> Optional[CachedResponse]:
        """Retrieve a response from the cache, if it exists

        Args:
            key: Cache key for the response
            default: Value to return if `key` is not in the cache
        """
        try:
            response = self.responses.get(key)
            if response is None:  # Note: bool(requests.Response) is False if status > 400
                response = self.responses[self.redirects[key]]
            return response
        except (AttributeError, KeyError):
            return default

    def save_response(
        self,
        response: Response,
        cache_key: Optional[str] = None,
        expires: Optional[datetime] = None,
    ):
        """Save a response to the cache

        Args:
            cache_key: Cache key for this response; will otherwise be generated based on request
            response: Response to save
            expires: Absolute expiration time for this response
        """
        cache_key = cache_key or self.create_key(response.request)
        cached_response = CachedResponse.from_response(response, expires=expires)
        cached_response = redact_response(cached_response, self._settings.ignored_parameters)
        self.responses[cache_key] = cached_response

        # Save redirect aliases, unless this is a revalidation (i.e., it was saved previously)
        if response.history and not cached_response.revalidated:
            for r in response.history:
                self.redirects[self.create_key(r.request)] = cache_key

    def clear(self):
        """Delete all items from the cache"""
        logger.info('Clearing all items from the cache')
        self.responses.clear()
        self.redirects.clear()

    def close(self):
        """Close any open backend connections"""
        logger.debug('Closing backend connections')
        self.responses.close()
        self.redirects.close()

    def create_key(
        self,
        request: AnyRequest,
        match_headers: Optional[Iterable[str]] = None,
        **kwargs,
    ) -> str:
        """Create a normalized cache key from a request object"""
        key_fn = self._settings.key_fn if self._settings.key_fn is not None else create_key
        return key_fn(
            request=request,
            ignored_parameters=self._settings.ignored_parameters,
            content_root_key=self._settings.content_root_key,
            match_headers=match_headers or self._settings.match_headers,
            serializer=self.responses.serializer,
            **kwargs,
        )

    # Convenience methods
    # --------------------

    def contains(
        self,
        key: Optional[str] = None,
        request: Optional[AnyRequest] = None,
        url: Optional[str] = None,
        verify: bool = True,
    ):
        """Check if the specified request is cached

        Args:
            key: Check for a specific cache key
            request: Check for a matching request, according to current request matching settings
            url: Check for a matching GET request with the specified URL
            verify: Check for requests with or without SSL verification
        """
        if url:
            request = Request('GET', url)
        if request and not key:
            key = self.create_key(request, verify=verify)
        return key in self.responses or key in self.redirects

    def delete(
        self,
        *keys: str,
        expired: bool = False,
        invalid: bool = False,
        older_than: ExpirationTime = None,
        requests: Optional[Iterable[AnyRequest]] = None,
        urls: Optional[Iterable[str]] = None,
        verify: bool = True,
    ):
        """Remove responses from the cache according one or more conditions.

        Args:
            keys: Remove responses with these cache keys
            expired: Remove all expired responses
            invalid: Remove all invalid responses (that can't be deserialized with current settings)
            older_than: Remove responses older than this value, relative to ``response.created_at``
            requests: Remove matching responses, according to current request matching settings
            urls: Remove matching GET requests for the specified URL(s)
            verify: Set to ``False`` to remove responses without SSL verification
        """
        delete_keys: List[str] = list(keys) if keys else []
        if urls:
            requests = list(requests or []) + [Request('GET', url).prepare() for url in urls]
        if requests:
            delete_keys += [self.create_key(request, verify=verify) for request in requests]

        for response in self.filter(
            valid=False, expired=expired, invalid=invalid, older_than=older_than
        ):
            delete_keys.append(response.cache_key)

        logger.debug(f'Deleting up to {len(delete_keys)} responses')
        # For some backends, we don't want to use bulk_delete if there's only one key
        if len(delete_keys) == 1:
            try:
                del self.responses[delete_keys[0]]
            except KeyError:
                pass
        else:
            self.responses.bulk_delete(delete_keys)
        self._prune_redirects()

    def _prune_redirects(self):
        """Remove any redirects that no longer point to an existing response"""
        invalid_redirects = [k for k, v in self.redirects.items() if v not in self.responses]
        self.redirects.bulk_delete(invalid_redirects)

    def filter(
        self,
        valid: bool = True,
        expired: bool = True,
        invalid: bool = False,
        older_than: ExpirationTime = None,
    ) -> Iterator[CachedResponse]:
        """Get responses from the cache, with optional filters for which responses to include:

        Args:
            valid: Include valid and unexpired responses; set to ``False`` to get **only**
                expired/invalid/old responses
            expired: Include expired responses
            invalid: Include invalid responses (as an empty ``CachedResponse``)
            older_than: Get responses older than this value, relative to ``response.created_at``
        """
        if not any([valid, expired, invalid, older_than]):
            return
        # Materialize the keys first, so applying a corrupt-entry policy (which may delete the
        # current entry) can't disturb an in-progress backend cursor and silently drop later items
        keys = list(self.responses.keys())
        for key in keys:
            response = self.get_response(key)

            # Use an empty response as a placeholder for an invalid response, if specified
            if invalid and response is None:
                response = CachedResponse(status_code=504)
                response.cache_key = key
                yield response
            elif response is not None and (
                (valid and not response.is_expired)
                or (expired and response.is_expired)
                or (older_than and response.is_older_than(older_than))
            ):
                yield response

    def recreate_keys(self):
        """Recreate cache keys for all previously cached responses"""
        logger.debug('Recreating all cache keys')
        old_keys = list(self.responses.keys())

        for old_cache_key in old_keys:
            response = self.responses[old_cache_key]
            # Skip entries that can't be deserialized (handled by the corruption policy), so one
            # bad entry doesn't stop keys from being recreated for the rest of the cache
            if response is None:
                continue
            # Adjust empty request body for responses cached before 1.0
            if response.request.body == b'None':
                response.request.body = b''
            new_cache_key = self.create_key(response.request)
            if new_cache_key != old_cache_key:
                self.responses[new_cache_key] = response
                del self.responses[old_cache_key]

    # This is deprecated, but still appears in various examples online, so I'll postpone removing it
    # for awhile longer.
    def remove_expired_responses(self, expire_after: ExpirationTime = None):
        warn(
            'remove_expired_responses() is deprecated; please use delete(expired=True) instead',
            DeprecationWarning,
            stacklevel=2,
        )
        if expire_after:
            self.reset_expiration(expire_after)
        self.delete(expired=True, invalid=True)

    def reset_expiration(self, expire_after: ExpirationTime = None):
        """Set a new expiration value to set on existing cache items

        Args:
            expire_after: New expiration value, **relative to the current time**
        """
        logger.info(f'Resetting expiration with: {expire_after}')
        for response in self.filter():
            response.reset_expiration(expire_after)
            self.responses[response.cache_key] = response

    def update(self, other: 'BaseCache'):
        """Update this cache with the contents of another cache"""
        logger.debug(f'Copying {len(other.responses)} responses from {repr(other)} to {repr(self)}')
        self.responses.update(other.responses)
        self.redirects.update(other.redirects)

    def urls(self, **kwargs) -> List[str]:
        """Get all unique cached URLs. Optionally takes keyword arguments for :py:meth:`.filter`."""
        return sorted({response.url for response in self.filter(**kwargs)})

    def __str__(self):
        return f'<{self.__class__.__name__}(name={self.cache_name})>'

    def __repr__(self):
        return str(self)


KT = TypeVar('KT')
VT = TypeVar('VT')


class BaseStorage(MutableMapping[KT, VT], ABC):
    """Base class for client-agnostic storage implementations. Notes:

    * This provides a common dictionary-like interface for the underlying storage operations
      (create, read, update, delete).
    * One ``BaseStorage`` instance corresponds to a single table/hash/collection, or whatever the
      backend-specific equivalent may be.
    * ``BaseStorage`` subclasses contain no behavior specific to ``requests``, which are handled by
      :py:class:`.BaseCache` subclasses.
    * ``BaseStorage`` also contains a serializer object (defaulting to :py:mod:`pickle`), which
      determines how :py:class:`.CachedResponse` objects are saved internally. See :ref:`serializers`
      for details.

    Args:
        serializer: Custom serializer that provides ``loads`` and ``dumps`` methods.
            If not provided, values will be written as-is.
        decode_content: Decode response body JSON or text into a human-readable format
        corruption_policy: How to handle a value that can't be deserialized (``report``,
            ``delete``, or ``raise``). Defaults to the policy of the parent cache, if any.
        on_corruption: Optional callback that receives a :py:class:`.CorruptEntryError`
        read_only: Set to ``True`` to prevent this storage from deleting corrupt entries
        kwargs: Additional backend-specific keyword arguments
    """

    def __init__(
        self,
        serializer: Optional[SerializerType] = None,
        decode_content: bool = False,
        corruption_policy: Optional[CorruptionPolicy] = None,
        on_corruption: Optional[CorruptionCallback] = None,
        read_only: bool = False,
        **kwargs,
    ):
        self.serializer = init_serializer(serializer, decode_content)
        self.corruption_policy = CorruptionPolicy.parse(corruption_policy)
        self.on_corruption = on_corruption
        self.read_only = read_only
        self.corruption_handler = None  # Set by the parent BaseCache, if any
        logger.debug(f'Initialized {type(self).__name__} with serializer: {self.serializer}')

    def bulk_delete(self, keys: Iterable[KT]):
        """Delete multiple keys from the cache, without raising errors for missing keys.

        This is a naive, generic implementation that subclasses should override with a more
        efficient backend-specific implementation, if possible.
        """
        for k in keys:
            try:
                del self[k]
            except KeyError:
                pass

    def close(self):
        """Close any open backend connections"""

    def __contains__(self, key) -> bool:
        """Check for a key without deserializing its value.

        The default mapping behavior would read and deserialize the value, which could apply a
        ``delete`` corruption policy (silently deleting an entry) during what is meant to be a
        read-only check. Subclasses should override this with an efficient backend-specific
        implementation where possible.
        """
        return any(k == key for k in self)

    def serialize(self, value: VT):
        """Serialize a value, if a serializer is available"""
        if TYPE_CHECKING:
            assert hasattr(self.serializer, 'dumps')
        return self.serializer.dumps(value) if self.serializer else value

    def deserialize(self, key, value: VT):
        """Deserialize a value, if a serializer is available.

        If deserialization fails (usually due to a value saved in an older requests-cache version),
        the failure is isolated to this single entry and the configured
        :py:class:`.CorruptionPolicy` is applied. ``None`` is returned for any policy other than
        ``raise``, so callers can treat the entry as a cache miss.

        Errors raised by the backend itself (for example, a database connection error) are not
        deserialization failures, and are propagated to the caller.
        """
        if not self.serializer:
            return value
        if TYPE_CHECKING:
            assert hasattr(self.serializer, 'loads')

        try:
            obj = self.serializer.loads(value)
            # Set cache key, if it's a response object
            try:
                obj.cache_key = key
            except AttributeError:
                pass
            return obj
        except CORRUPT_ENTRY_ERRORS as e:
            return self._handle_corrupt_entry(key, e)

    def _handle_corrupt_entry(self, key, error: BaseException):
        """Report a single corrupt entry, and apply the configured policy to it"""
        event = CorruptEntryError(key, 'deserialize', error, policy=self.corruption_policy)
        log_corruption(event, logger)
        logger.debug(error, exc_info=True)

        if self.corruption_handler is not None:
            return self.corruption_handler(event)
        return self._apply_corruption_policy(event)

    def _apply_corruption_policy(self, event: CorruptEntryError):
        """Apply the policy configured directly on this storage instance"""
        if self.corruption_policy == CorruptionPolicy.RAISE:
            raise event from event.error

        notify_corruption(self.on_corruption, event, logger)
        if self.corruption_policy == CorruptionPolicy.DELETE:
            if self.read_only:
                logger.info(
                    'Not deleting corrupt cache entry in read-only mode '
                    '(key=%r, stage=delete, error=%s)',
                    event.key,
                    event.exception_name,
                    extra={
                        'cache_key': event.key,
                        'stage': 'delete',
                        'exception_type': event.exception_name,
                    },
                )
                return None
            try:
                self.bulk_delete([event.key])
            except Exception as delete_error:
                logger.error(
                    'Failed to delete corrupt cache entry (key=%r, stage=delete, error=%s): %s',
                    event.key,
                    type(delete_error).__name__,
                    delete_error,
                    extra={
                        'cache_key': event.key,
                        'stage': 'delete',
                        'exception_type': type(delete_error).__name__,
                    },
                )
                logger.debug(delete_error, exc_info=True)
        return None

    def __str__(self):
        return str(list(self.keys()))


class DictStorage(UserDict, BaseStorage):
    """A basic dict wrapper class for non-persistent, in-memory storage

    .. note::
        This is mostly a placeholder for when no other backends are available. For in-memory
        caching, either :py:class:`.SQLiteCache` (with `use_memory=True`) or :py:class:`.RedisCache`
        is recommended instead.

    """

    def __init__(self, *args, **kwargs):
        # Storage-level options are handled by BaseStorage; everything else is passed to UserDict
        storage_kwargs = {
            k: kwargs.pop(k)
            for k in (
                'serializer',
                'decode_content',
                'corruption_policy',
                'on_corruption',
                'read_only',
            )
            if k in kwargs
        }
        super().__init__(*args, **kwargs)
        BaseStorage.__init__(self, **storage_kwargs)
        self.serializer = None

    def __contains__(self, key) -> bool:
        return key in self.data

    def __getitem__(self, key):
        """An additional step is needed here for response data. The original response object
        is still in memory, and hasn't gone through a serialize/deserialize loop. So, the file-like
        response body has already been read, and needs to be reset.
        """
        item = super().__getitem__(key)
        if getattr(item, 'raw', None):
            item.raw.reset()
        try:
            item.cache_key = key
        except AttributeError:
            pass
        return item
