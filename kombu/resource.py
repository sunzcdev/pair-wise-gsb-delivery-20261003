"""Generic resource pool implementation."""

from __future__ import annotations

import os
from contextlib import nullcontext
from queue import Empty, LifoQueue

from . import exceptions
from .utils.compat import register_after_fork
from .utils.functional import lazy


def _after_fork_cleanup_resource(resource):
    try:
        resource.force_close_all()
    except Exception:
        pass


class Resource:
    """Pool of resources."""

    LimitExceeded = exceptions.LimitExceeded

    close_after_fork = False

    def __init__(self, limit=None, preload=None, close_after_fork=None):
        # bool subclasses int; limit=True would silently become pool size 1
        if isinstance(limit, bool):
            raise TypeError("limit must be an int, not bool")
        self._limit = limit
        self.preload = preload or 0
        self._closed = False
        self.close_after_fork = (
            close_after_fork
            if close_after_fork is not None else self.close_after_fork
        )

        self._resource = LifoQueue()
        self._dirty = set()
        # Reviewable graceful-shrink state: set to the lowered limit while
        # borrowed resources still exceed it, None when not shrinking.
        self._shrink_target = None
        # Number of acquirers currently blocked waiting for a resource.
        self._acquire_waiters = 0
        if self.close_after_fork and register_after_fork is not None:
            register_after_fork(self, _after_fork_cleanup_resource)
        self.setup()

    def setup(self):
        raise NotImplementedError('subclass responsibility')

    def _add_when_empty(self):
        if self.limit and len(self._dirty) >= self.limit:
            raise self.LimitExceeded(self.limit)
        # All taken, put new on the queue and
        # try get again, this way the first in line
        # will get the resource.
        self._resource.put_nowait(self.new())

    def acquire(self, block=False, timeout=None):
        """Acquire resource.

        Arguments:
        ---------
            block (bool): If the limit is exceeded,
                then block until there is an available item.
            timeout (float): Timeout to wait
                if ``block`` is true.  Default is :const:`None` (forever).

        Raises
        ------
            LimitExceeded: if block is false and the limit has been exceeded.
        """
        if self._closed:
            raise RuntimeError('Acquire on closed pool')
        if self.limit:
            while 1:
                if block:
                    self._acquire_waiters += 1
                try:
                    R = self._resource.get(block=block, timeout=timeout)
                except Empty:
                    self._add_when_empty()
                else:
                    try:
                        R = self.prepare(R)
                    except BaseException:
                        if isinstance(R, lazy):
                            # not evaluated yet, just put it back
                            self._resource.put_nowait(R)
                        else:
                            # Evaluated: hand ownership back through the
                            # normal release path so that pool accounting
                            # stays consistent.
                            self._dirty.add(R)
                            self.release(R)
                        raise
                    self._dirty.add(R)
                    break
                finally:
                    if block:
                        self._acquire_waiters -= 1
        else:
            R = self.prepare(self.new())

        def release():
            """Release resource so it can be used by another thread.

            Warnings:
            --------
                The caller is responsible for discarding the object,
                and to never use the resource again.  A new resource must
                be acquired if so needed.
            """
            self.release(R)
        R.release = release

        return R

    def prepare(self, resource):
        return resource

    def close_resource(self, resource):
        resource.close()

    def release_resource(self, resource):
        pass

    def replace(self, resource):
        """Replace existing resource with a new instance.

        This can be used in case of defective resources.
        """
        if self.limit:
            self._dirty.discard(resource)
        self.close_resource(resource)

    def release(self, resource):
        if self.limit:
            if resource not in self._dirty:
                # Not owned by this pool (e.g. released twice): never
                # return a duplicate copy to the idle queue.
                return
            self._dirty.discard(resource)
            if self._should_reclaim():
                # The pool is shrinking: reclaim the returned resource
                # instead of keeping an idle item above the new limit.
                self._reclaim_resource(resource)
            else:
                self._resource.put_nowait(resource)
            self.release_resource(resource)
            self._update_shrink_state()
        else:
            self.close_resource(resource)

    def _should_reclaim(self):
        """Whether a returned resource would exceed the current capacity.

        Blocked acquirers are handed the resource instead, so that they
        always get a deterministic result.
        """
        return (
            self._resource.qsize() + len(self._dirty) >= self.limit
            and not self._acquire_waiters
        )

    def _reclaim_resource(self, resource):
        """Gracefully collect a resource removed by shrinking.

        Errors are ignored: the resource is considered reclaimed either
        way, so pool accounting stays consistent.
        """
        try:
            self.collect_resource(resource)
        except Exception:
            pass

    def _update_shrink_state(self):
        if (self._shrink_target is not None and
                len(self._dirty) + self._resource.qsize()
                <= self._shrink_target):
            self._shrink_target = None

    @property
    def shrink_target(self):
        """Limit the pool is gracefully shrinking to (None if not shrinking).

        Borrowed resources are never closed by a graceful shrink; they are
        reclaimed as they are released, until the pool is back within this
        target.
        """
        return self._shrink_target

    @property
    def shrink_in_progress(self):
        """True while a graceful shrink still waits for borrowed resources."""
        return self._shrink_target is not None

    def collect_resource(self, resource):
        pass

    def force_close_all(self, close_pool=True):
        """Close and remove all resources in the pool (also those in use).

        Used to close resources from parent processes after fork
        (e.g. sockets/connections).

        This is the immediate, forceful counterpart of the graceful
        shrink performed by :meth:`resize`: borrowed resources are
        collected right away instead of when they are released.

        Arguments:
        ---------
            close_pool (bool): If True (default) then the pool is marked
                as closed. In case of False the pool can be reused.
        """
        if self._closed:
            return
        self._closed = close_pool
        self._shrink_target = None
        dirty = self._dirty
        resource = self._resource
        while 1:  # - acquired
            try:
                dres = dirty.pop()
            except KeyError:
                break
            try:
                self.collect_resource(dres)
            except AttributeError:  # Issue #78
                pass
        while 1:  # - available
            try:
                res = resource.queue.pop()
            except IndexError:
                break
            try:
                self.collect_resource(res)
            except AttributeError:
                pass  # Issue #78

    def resize(self, limit, force=False, ignore_errors=False, reset=False):
        # bool subclasses int; limit=True would silently become pool size 1.
        # The limit setter calls resize(), so pool.limit = True is covered.
        if isinstance(limit, bool):
            raise TypeError("limit must be an int, not bool")
        prev_limit = self._limit
        if (self._dirty and prev_limit and
                0 < limit < prev_limit) and not ignore_errors:
            if not force:
                raise RuntimeError(
                    "Can't shrink pool when in use: was={} now={}".format(
                        prev_limit, limit))
            reset = True
        self._limit = limit
        if reset:
            self._shrink_target = None
            try:
                self.force_close_all(close_pool=False)
            except Exception:
                pass
        self.setup()
        if prev_limit is not None and limit < prev_limit:
            # Graceful shrink: excess idle resources are reclaimed now,
            # resources still in use keep working and are reclaimed as
            # they are released (see ``shrink_target``).
            # A limit of 0 means "unlimited", so no shrink is pending.
            self._shrink_target = limit or None
            self._shrink_down(collect=limit > 0)
            self._update_shrink_state()
        elif limit > (prev_limit or 0):
            # Growing the pool adds idle resources, which wakes up
            # acquirers blocked waiting for a resource.
            self._shrink_target = None

    def _shrink_down(self, collect=True):
        resource = self._resource
        # we should remove the least recently used item, but there is no public API in LifoQueue to
        # do so.
        with getattr(resource, 'mutex', nullcontext()):
            # keep in mind the dirty resources are not shrinking
            while len(resource.queue) and (len(resource.queue) + len(self._dirty)) > self.limit:
                R = resource.queue.pop()
                if collect:
                    self._reclaim_resource(R)

    @property
    def limit(self):
        return self._limit

    @limit.setter
    def limit(self, limit):
        self.resize(limit)

    if os.environ.get('KOMBU_DEBUG_POOL'):  # pragma: no cover
        _orig_acquire = acquire
        _orig_release = release

        _next_resource_id = 0

        def acquire(self, *args, **kwargs):
            import traceback
            id = self._next_resource_id = self._next_resource_id + 1
            print(f'+{id} ACQUIRE {self.__class__.__name__}')
            r = self._orig_acquire(*args, **kwargs)
            r._resource_id = id
            print(f'-{id} ACQUIRE {self.__class__.__name__}')
            if not hasattr(r, 'acquired_by'):
                r.acquired_by = []
            r.acquired_by.append(traceback.format_stack())
            return r

        def release(self, resource):
            id = resource._resource_id
            print(f'+{id} RELEASE {self.__class__.__name__}')
            r = self._orig_release(resource)
            print(f'-{id} RELEASE {self.__class__.__name__}')
            self._next_resource_id -= 1
            return r
