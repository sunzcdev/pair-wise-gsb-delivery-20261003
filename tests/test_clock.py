"""Test monotonic clock policy for relative TTL expiry."""

import shutil
import tempfile

import pytest

import diskcache as dc


class FakeClock:
    """Controllable clock with independent wall and monotonic times."""

    def __init__(self, wall=1_700_000_000.0, mono=100_000.0):
        self.wall = wall
        self.mono = mono

    def time(self):
        """Return the current wall-clock time in seconds."""
        return self.wall

    def monotonic(self):
        """Return the current monotonic time in seconds."""
        return self.mono

    def advance(self, seconds):
        """Advance both clocks as if real time passed."""
        self.wall += seconds
        self.mono += seconds

    def rollback(self, seconds):
        """Move only the wall clock backwards, like an NTP correction."""
        self.wall -= seconds

    def jump(self, seconds):
        """Move only the wall clock forwards, like an NTP correction."""
        self.wall += seconds


@pytest.fixture
def clock():
    return FakeClock()


@pytest.fixture
def directory():
    path = tempfile.mkdtemp(prefix='diskcache-clock-')
    yield path
    shutil.rmtree(path, ignore_errors=True)


@pytest.fixture
def cache(clock, directory):
    with dc.Cache(directory, clock=clock) as cache:
        yield cache


def test_default_cache_has_no_clock_policy():
    with dc.Cache() as cache:
        assert cache.clock is None
        cache.set('key', 'value', expire=60)
        assert cache.get('key') == 'value'
    shutil.rmtree(cache.directory, ignore_errors=True)


def test_clock_exposed(cache, clock):
    assert cache.clock is clock


def test_rollback_does_not_extend_lifetime(cache, clock):
    cache.set('key', 'value', expire=10)

    clock.advance(5)
    clock.rollback(1000)

    # Five monotonic seconds have passed; the wall-clock rollback must
    # not extend the remaining lifetime of the item.
    assert cache.get('key') == 'value'
    assert 'key' in cache

    clock.advance(6)

    # Eleven monotonic seconds have passed; the item is expired.
    assert cache.get('key') is None
    assert 'key' not in cache


def test_forward_jump_does_not_expire_early(cache, clock):
    cache.set('key', 'value', expire=10)

    clock.jump(1000)

    # The wall clock jumped forward but no monotonic time has passed.
    assert cache.get('key') == 'value'
    assert 'key' in cache

    clock.advance(11)

    assert cache.get('key') is None
    assert 'key' not in cache


def test_absolute_expire_time_stored(cache, clock):
    wall = clock.wall
    cache.set('key', 'value', expire=10)

    # The stored deadline is an absolute time interpretable after a
    # restart of the cache.
    _, expire_time = cache.get('key', expire_time=True)
    assert expire_time == pytest.approx(wall + 10)


def test_get_slow_path_uses_clock_policy(directory, clock):
    with dc.Cache(directory, clock=clock, statistics=1) as cache:
        cache.set('key', 'value', expire=10)

        clock.advance(5)
        clock.rollback(1000)
        assert cache.get('key') == 'value'

        clock.advance(6)
        assert cache.get('key') is None


def test_expired_items_do_not_resurrect_after_rollback(cache, clock):
    cache.set('key', 'value', expire=10)

    clock.advance(11)
    assert cache.get('key') is None

    clock.rollback(1000)

    # The item expired already; the rollback must not resurrect it.
    assert cache.get('key') is None
    assert 'key' not in cache
    assert cache.touch('key', expire=10) is False


def test_old_expired_items_do_not_resurrect_after_reopen(directory, clock):
    with dc.Cache(directory, clock=clock) as cache:
        cache.set('key', 'value', expire=10)

    clock.advance(11)

    with dc.Cache(directory, clock=clock) as cache:
        # The item expired before the cache was re-opened.
        assert cache.get('key') is None

        clock.rollback(1000)

        # The rollback must not resurrect the expired item.
        assert cache.get('key') is None
        assert 'key' not in cache
        assert cache.touch('key', expire=10) is False


def test_reopen_restores_remaining_ttl_from_wall_clock(directory, clock):
    with dc.Cache(directory, clock=clock) as cache:
        cache.set('key', 'value', expire=10)

    # Six wall-clock seconds pass while the cache is closed.
    clock.advance(6)

    with dc.Cache(directory, clock=clock) as cache:
        assert cache.get('key') == 'value'

        # Five more seconds pass; the ten second TTL has elapsed.
        clock.advance(5)
        assert cache.get('key') is None


def test_reopen_after_wall_clock_passed_deadline(directory, clock):
    with dc.Cache(directory, clock=clock) as cache:
        cache.set('key', 'value', expire=10)

    # The wall clock moves past the deadline while the cache is closed.
    clock.advance(11)

    with dc.Cache(directory, clock=clock) as cache:
        assert cache.get('key') is None
        assert 'key' not in cache


def test_touch_uses_clock_policy(cache, clock):
    cache.set('key', 'value', expire=10)

    clock.advance(5)
    assert cache.touch('key', expire=20) is True

    clock.rollback(1000)
    clock.advance(16)

    # Twenty-one monotonic seconds since set, sixteen since touch.
    assert cache.get('key') == 'value'

    clock.advance(5)

    # Twenty-one monotonic seconds since touch; the item is expired.
    assert cache.get('key') is None


def test_add_uses_clock_policy(cache, clock):
    assert cache.add('key', 'value', expire=10) is True

    clock.rollback(1000)

    # The item is still alive per the monotonic clock.
    assert cache.add('key', 'other', expire=10) is False
    assert cache.get('key') == 'value'

    clock.advance(11)

    # The item expired per the monotonic clock and may be re-added.
    assert cache.add('key', 'other', expire=10) is True
    assert cache.get('key') == 'other'


def test_queue_items_use_clock_policy(cache, clock):
    cache.push('first', expire=10)

    clock.rollback(1000)

    # The pushed item is still alive per the monotonic clock.
    (key, value), expire_time = cache.peek(expire_time=True)
    assert value == 'first'
    assert expire_time is not None

    clock.advance(11)

    # The pushed item expired per the monotonic clock.
    assert cache.peek() == (None, None)
    assert cache.pull() == (None, None)

    # Items without TTL are unaffected by wall-clock changes.
    cache.push('forever')
    clock.rollback(1000)
    clock.advance(1000)
    key, value = cache.pull()
    assert value == 'forever'
    assert cache.pull() == (None, None)


def test_expire_uses_clock_policy(cache, clock):
    cache.set('short', 'value', expire=10)
    cache.set('long', 'value', expire=20)

    clock.rollback(1000)

    # Nothing has expired per the monotonic clock.
    assert cache.expire() == 0

    clock.advance(11)

    # Only the short-lived item has expired.
    assert cache.expire() == 1
    assert cache.get('short') is None
    assert cache.get('long') == 'value'


def test_expire_with_explicit_now(cache, clock):
    wall = clock.wall
    cache.set('short', 'value', expire=10)
    cache.set('long', 'value', expire=20)

    # An explicitly given time is used as-is.
    assert cache.expire(now=wall + 15) == 1
    assert cache.get('short') is None
    assert cache.get('long') == 'value'

    assert cache.expire(now=wall + 25) == 1
    assert cache.get('long') is None


def test_items_without_ttl_unaffected(cache, clock, directory):
    cache.set('key', 'value')

    clock.rollback(1000)
    assert cache.get('key') == 'value'

    clock.jump(2000)
    assert cache.get('key') == 'value'

    clock.advance(100000)
    assert cache.get('key') == 'value'

    cache.close()

    with dc.Cache(directory, clock=clock) as reopened:
        assert reopened.get('key') == 'value'


def test_peekitem_uses_clock_policy(cache, clock):
    cache.set('key', 'value', expire=10)

    clock.rollback(1000)
    assert cache.peekitem() == ('key', 'value')

    clock.advance(11)

    with pytest.raises(KeyError):
        cache.peekitem()


def test_pop_and_delete_use_clock_policy(cache, clock):
    cache.set('key', 'value', expire=10)

    clock.rollback(1000)
    assert cache.pop('key') == 'value'

    cache.set('key', 'value', expire=10)
    clock.advance(11)
    clock.rollback(1000)

    # Expired per the monotonic clock; rollback changes nothing.
    assert cache.pop('key') is None
    assert cache.delete('key') is False


def test_memoize_uses_clock_policy(cache, clock):
    calls = []

    @cache.memoize(expire=10)
    def compute(arg):
        calls.append(arg)
        return len(calls)

    assert compute('arg') == 1

    clock.rollback(1000)
    assert compute('arg') == 1

    clock.advance(11)
    assert compute('arg') == 2


def test_fanout_cache_uses_clock_policy(clock):
    directory = tempfile.mkdtemp(prefix='diskcache-clock-')
    try:
        with dc.FanoutCache(directory, shards=4, clock=clock) as cache:
            cache.set('key', 'value', expire=10)

            clock.rollback(1000)
            assert cache.get('key') == 'value'
            assert cache.expire() == 0

            clock.advance(11)
            assert cache.get('key') is None
            assert cache.expire() == 1
    finally:
        shutil.rmtree(directory, ignore_errors=True)


def test_real_clock_policy():
    with dc.Cache(clock=dc.Clock()) as cache:
        cache.set('key', 'value', expire=60)
        assert cache.get('key') == 'value'
        _, expire_time = cache.get('key', expire_time=True)
        assert expire_time is not None
    shutil.rmtree(cache.directory, ignore_errors=True)
