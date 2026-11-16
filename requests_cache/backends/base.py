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
    CacheSettings,
    CorruptionCallback,
    CorruptionPolicy,
    CorruptEntryError,
    ExpirationTime,
    notify_corruption,
)
from ..serializers import SerializerType, init_serializer

# Specific exceptions that may be raised during deserialization
DESERIALIZE_ERRORS = (AttributeError, EOFError, ImportError, PickleError, TypeError, ValueError)

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
        corruption_policy: How to handle entries that cannot be deserialized: ``report``,
            ``delete``, or ``raise``. See :py:class:`.CorruptionPolicy`.
        on_corruption: Optional callback that receives a :py:class:`.CorruptEntryError`
        kwargs: Additional backend-specific keyword arguments
    """

    def __init__(self, cache_name: str = DEFAULT_CACHE_NAME, **kwargs):
        self.cache_name = cache_name
        self.responses: BaseStorage[str, CachedResponse] = DictStorage()
        self.redirects: BaseStorage[str, str] = DictStorage()
        # Init and public access is done in CachedSession
        self._settings = CacheSettings.from_kwargs(**kwargs)
        self._configure_storages()

    def _configure_storages(self):
        """Connect storage instances to the policy that is managed at the cache level.

        Storages are also usable on their own (with their own policy), but when they belong to a
        cache, the cache applies the policy so it can also clean up redirect aliases and respect
        session-level settings like ``read_only``.
        """
        for storage in (self.responses, self.redirects):
            storage.corruption_handler = self._handle_corruption

    @property
    def corruption_policy(self) -> CorruptionPolicy:
        """How to handle cache entries that cannot be deserialized"""
        return self._settings.corruption_policy

    @corruption_policy.setter
    def corruption_policy(self, value):
        self._settings.corruption_policy = CorruptionPolicy.parse(value)

    def _handle_corruption(self, event: CorruptEntryError) -> Optional[CachedResponse]:
        """Apply the configured policy to a single corrupt entry"""
        # The cache-level policy is authoritative (the storage's own policy may be the default)
        event.policy = self._settings.corruption_policy
        notify_corruption(self._settings.on_corruption, event, logger)

        if self._settings.corruption_policy == CorruptionPolicy.RAISE:
            raise event from event.error

        if self._settings.corruption_policy == CorruptionPolicy.DELETE:
            # Never delete items in read-only mode; just report the failure
            if self._settings.read_only:
                logger.info(
                    'Not deleting corrupt cache entry in read-only mode '
                    '(key=%r, stage=delete, error=%s): %s',
                    event.key,
                    event.exception_name,
                    event.error,
                    extra={
                        'cache_key': event.key,
                        'stage': 'delete',
                        'exception_type': event.exception_name,
                    },
                )
                return None
            try:
                self._delete_corrupted_entry(event.key)
            except Exception as delete_error:
                # A failed recovery must not fall back to clearing the database, and must not
                # recursively re-enter corruption handling.
                logger.exception(
                    'Failed to isolate corrupt cache entry '
                    '(key=%r, stage=delete, error=%s): %s',
                    event.key,
                    type(delete_error).__name__,
                    delete_error,
                    extra={
                        'cache_key': event.key,
                        'stage': 'delete',
                        'exception_type': type(delete_error).__name__,
                    },
                )
        return None

    def _delete_corrupted_entry(self, key: str):
        """Delete a corrupted response along with any redirect aliases that resolve to it"""
        self.responses.bulk_delete([key])
        aliases = self._get_redirect_aliases({key})
        if aliases:
            self.redirects.bulk_delete(aliases)

    def _get_redirect_aliases(self, target_keys: Iterable[str]) -> List[str]:
        """Get all redirect aliases that resolve to the given target key(s).

        This follows reverse redirect links, so a multi-hop alias is removed in the same isolation
        operation. Iteration is bounded by the number of stored aliases, so self-loops and cycles
        cannot recurse indefinitely.
        """
        redirects = dict(self.redirects.items())
        aliases: List[str] = []
        targets = set(target_keys)
        while targets:
            next_targets = set()
            for alias, target in redirects.items():
                if alias not in aliases and target in targets:
                    aliases.append(alias)
                    next_targets.add(alias)
            if not next_targets:
                break
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
            self._configure_storages()
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
        self._configure_storages()
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
        self._configure_storages()
        keys: Iterable[str] = self.responses.keys()
        # If recovery may delete the current item, materialize keys first so it can't disturb an
        # in-progress backend cursor and make later items disappear from this batch
        if not self._settings.read_only and (
            self._settings.corruption_policy == CorruptionPolicy.DELETE
        ):
            keys = list(keys)
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
        corruption_policy: How to handle entries that cannot be deserialized: ``report``,
            ``delete``, or ``raise``. See :py:class:`.CorruptionPolicy`.
        on_corruption: Optional callback that receives a :py:class:`.CorruptEntryError`
        read_only: Set to ``True`` to prevent a ``delete`` policy from removing entries
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
        # Set by BaseCache so storage-level failures can be handled with cache-level context
        self.corruption_handler = None
        # Tracks an in-progress recovery, so a callback or cleanup error can't recurse
        self._corruption_local = local()
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

    def serialize(self, value: VT):
        """Serialize a value, if a serializer is available"""
        if TYPE_CHECKING:
            assert hasattr(self.serializer, 'dumps')
        return self.serializer.dumps(value) if self.serializer else value

    def deserialize(self, key, value: VT):
        """Deserialize a value, if a serializer is available.

        If deserialization fails (usually due to a value saved in an older requests-cache version),
        the configured :py:class:`.CorruptionPolicy` is applied. A single corrupt entry is isolated,
        and never causes all other entries to become unreadable.
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
        except Exception as e:
            return self._handle_corrupt_entry(key, 'deserialize', e)

    def _handle_corrupt_entry(self, key, stage: str, error: BaseException):
        """Log, report, and optionally isolate a single corrupt cache entry"""
        event = CorruptEntryError(key, stage, error, policy=self.corruption_policy)
        logger.error(
            'Unable to deserialize cache entry (key=%r, stage=%s, error=%s): %s',
            event.key,
            event.stage,
            event.exception_name,
            event.error,
            extra={
                'cache_key': event.key,
                'stage': event.stage,
                'exception_type': event.exception_name,
            },
        )
        logger.debug(error, exc_info=True)

        # A callback or recovery operation must not recursively re-enter this handler
        if getattr(self._corruption_local, 'handling', False):
            if self.corruption_policy == CorruptionPolicy.RAISE:
                raise event from error
            return None

        self._corruption_local.handling = True
        try:
            if self.corruption_handler is not None:
                return self.corruption_handler(event)
            return self._apply_corruption_policy(event)
        finally:
            self._corruption_local.handling = False

    def _apply_corruption_policy(self, event: CorruptEntryError):
        """Apply the policy configured directly on this storage"""
        notify_corruption(self.on_corruption, event, logger)

        if self.corruption_policy == CorruptionPolicy.RAISE:
            raise event from event.error

        if self.corruption_policy == CorruptionPolicy.DELETE:
            if self.read_only:
                logger.info(
                    'Not deleting corrupt cache entry in read-only mode '
                    '(key=%r, stage=delete, error=%s): %s',
                    event.key,
                    event.exception_name,
                    event.error,
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
                logger.exception(
                    'Failed to isolate corrupt cache entry '
                    '(key=%r, stage=delete, error=%s): %s',
                    event.key,
                    type(delete_error).__name__,
                    delete_error,
                    extra={
                        'cache_key': event.key,
                        'stage': 'delete',
                        'exception_type': type(delete_error).__name__,
                    },
                )
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
        # UserDict.__init__ doesn't call through to BaseStorage, so initialize policy state here
        BaseStorage.__init__(
            self,
            serializer=kwargs.pop('serializer', None),
            decode_content=kwargs.pop('decode_content', False),
            corruption_policy=kwargs.pop('corruption_policy', None),
            on_corruption=kwargs.pop('on_corruption', None),
            read_only=kwargs.pop('read_only', False),
        )
        super().__init__(*args, **kwargs)
        self.serializer = None

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
