import threading
from collections import UserDict
from concurrent.futures import ThreadPoolExecutor

import pytest

from zict import Cache


class NotifyingDict(UserDict):
    """Backing mapping with per-key listeners and optionally delayed events."""

    def __init__(self):
        super().__init__()
        self.listeners = []
        self.pending = []
        self.versions = {}
        self.next_version = 0
        self.reads = 0
        self.fail_next_reads = 0

    def on_change(self, fn):
        self.listeners.append(fn)
        return fn

    def _next_version(self, key):
        self.next_version += 1
        self.versions[key] = self.next_version
        return self.next_version

    def _emit_now(self, key, version, action):
        for fn in list(self.listeners):
            fn(key, version, action)

    def _emit_later(self, key, version, action):
        self.pending.append((key, version, action))

    def flush_notifications(self):
        pending, self.pending = self.pending, []
        for event in pending:
            self.deliver(event)

    def deliver_one_notification(self):
        assert self.pending
        self.deliver(self.pending.pop(0))

    def deliver(self, event):
        key, version, action = event
        self._emit_now(key, version, action)

    def external_set(self, key, value):
        self.data[key] = value
        version = self._next_version(key)
        self._emit_later(key, version, "set")
        return version

    def external_del(self, key):
        del self.data[key]
        version = self._next_version(key)
        self._emit_later(key, version, "delete")
        return version

    def __setitem__(self, key, value):
        super().__setitem__(key, value)
        self._emit_now(key, self._next_version(key), "set")

    def __delitem__(self, key):
        super().__delitem__(key)
        self._emit_now(key, self._next_version(key), "delete")

    def __getitem__(self, key):
        self.reads += 1
        if self.fail_next_reads:
            self.fail_next_reads -= 1
            raise OSError("synthetic transient slow-layer failure")
        return super().__getitem__(key)


def _notifying_pair(use_version_source=True):
    data = NotifyingDict()
    kwargs = {"versions": data.versions} if use_version_source else {}
    left = Cache(data, {}, **kwargs)
    right = Cache(data, {}, **kwargs)

    def notify(key, version, action):
        left.invalidate(key, version, action)
        right.invalidate(key, version, action)

    data.on_change(notify)
    return data, left, right


def test_invalidate_only_drops_affected_key():
    data, left, right = _notifying_pair()
    left["x"] = 1
    left["y"] = 2
    assert right["x"] == 1
    assert right["y"] == 2
    assert left.cache == {"x": 1, "y": 2}
    assert right.cache == {"x": 1, "y": 2}

    data.external_set("x", 10)
    data.flush_notifications()

    assert left.cache == {"y": 2}
    assert right.cache == {"y": 2}

    data.reads = 0
    assert left["x"] == 10
    assert right["x"] == 10
    assert left["y"] == 2
    assert right["y"] == 2
    # One slow read per view for x; y is never re-read.
    assert data.reads == 2


def test_invalidate_on_external_delete():
    data, left, right = _notifying_pair()
    left["x"] = 1
    assert right["x"] == 1

    data.external_del("x")
    data.flush_notifications()

    assert left.cache == {}
    assert right.cache == {}
    with pytest.raises(KeyError):
        left["x"]
    with pytest.raises(KeyError):
        right["x"]


def test_synchronous_local_notification_is_echo():
    data, left, right = _notifying_pair()

    left["x"] = 1
    assert left.cache == {"x": 1}
    assert right.cache == {}
    assert right["x"] == 1

    del left["x"]
    assert left.cache == {}
    assert "x" not in left._last_updated


def test_slow_read_failure_is_retryable_without_negative_cache():
    data = NotifyingDict()
    data.data["x"] = 1
    cache = Cache(data, {})

    data.fail_next_reads = 1
    with pytest.raises(OSError):
        cache["x"]
    assert cache.cache == {}
    # A fence created only for this failed external-key read is cleaned up.
    assert "x" not in cache._last_updated

    assert cache["x"] == 1
    assert cache.cache == {"x": 1}

    # An explicit notification fence is also retained safely across failure.
    second = Cache(data, {})
    second.invalidate("x", 1)
    data.fail_next_reads = 1
    with pytest.raises(OSError):
        second["x"]
    assert "x" in second._last_updated
    assert second["x"] == 1


def test_stale_versioned_notification_does_not_evict_new_value():
    data, left, right = _notifying_pair()
    left["x"] = "old"
    left["y"] = "stable"
    assert right["x"] == "old"
    assert right["y"] == "stable"

    delete_version = data.external_del("x")
    new_version = data.external_set("x", "new")
    assert new_version > delete_version

    # The newer replacement event overtakes the delayed delete event.
    for cache in (left, right):
        cache.invalidate("x", new_version)

    data.reads = 0
    assert left["x"] == right["x"] == "new"
    reads_after_new_value = data.reads
    assert reads_after_new_value == 2

    # The old delete notification is stale and must not remove the new fast copies.
    for cache in (left, right):
        cache.invalidate("x", delete_version, "delete")
    assert left.cache["x"] == "new"
    assert right.cache["x"] == "new"
    assert data.reads == reads_after_new_value

    # Unrelated keys remain fast hits.
    assert left["y"] == right["y"] == "stable"
    assert data.reads == reads_after_new_value


def test_delayed_first_delete_notification_end_to_end():
    data, left, right = _notifying_pair()
    left["x"] = "v1"
    left["keep"] = "stable"
    assert right["x"] == "v1"
    assert right["keep"] == "stable"

    delete_version = data.external_del("x")
    replace_version = data.external_set("x", "v2")
    assert data.data == {"x": "v2", "keep": "stable"}

    # Before either delayed event is delivered, fast layers still hit v1.
    data.reads = 0
    assert left["x"] == "v1"
    assert right["x"] == "v1"
    assert data.reads == 0

    # The recreate event is delivered before the delayed first delete event.
    left.invalidate("x", replace_version, "set")
    right.invalidate("x", replace_version, "set")
    assert left["x"] == right["x"] == "v2"
    assert data.reads == 2

    # Another refresh round can also observe the current source version.
    left.invalidate("x")
    right.invalidate("x")
    assert left["x"] == right["x"] == "v2"
    assert data.reads == 4

    # Finally the stale delete arrives and is ignored.
    left.invalidate("x", delete_version, "delete")
    right.invalidate("x", delete_version, "delete")
    assert left.cache["x"] == right.cache["x"] == "v2"

    assert left["keep"] == right["keep"] == "stable"
    assert data.reads == 4


def test_stale_object_notification_can_compare_expected_value():
    data = {}
    left = Cache(data, {})
    right = Cache(data, {})

    old = object()
    new = object()
    data["x"] = old
    assert left["x"] is old
    assert right["x"] is old

    # The backend delete event closes over the old object, but is delivered late.
    delayed = [lambda: left.invalidate_if("x", old, identity=True),
               lambda: right.invalidate_if("x", old, identity=True)]

    data["x"] = new
    left.invalidate("x")
    right.invalidate("x")
    assert left["x"] is new
    assert right["x"] is new

    for callback in delayed:
        callback()

    # The late event describes old, so it must not evict the new object.
    assert left.cache["x"] is new
    assert right.cache["x"] is new


def test_invalidate_fences_inflight_external_read():
    in_get = threading.Event()
    block_get = threading.Event()

    class Slow(UserDict):
        def __getitem__(self, key):
            value = self.data[key]
            in_get.set()
            assert block_get.wait(timeout=5)
            return value

    data = Slow({"x": "old"})
    cache = Cache(data, {})
    cache.invalidate("x")

    with ThreadPoolExecutor(1) as executor:
        future = executor.submit(cache.__getitem__, "x")
        assert in_get.wait(timeout=5)
        data.data["x"] = "new"
        cache.invalidate("x")
        block_get.set()
        assert future.result() == "old"

    assert cache.cache == {}
    assert cache["x"] == "new"
    assert cache.cache == {"x": "new"}
