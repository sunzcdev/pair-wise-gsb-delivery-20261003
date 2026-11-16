"""Test diskcache.core.Cache pin handles."""

import os
import os.path as op
import shutil
import tempfile
import time

import pytest

import diskcache as dc


@pytest.fixture
def directory():
    path = tempfile.mkdtemp(prefix='diskcache-pin-')
    yield path
    shutil.rmtree(path, ignore_errors=True)


def make_cache(directory):
    # Small minimum file size so short byte strings use file storage.
    return dc.Cache(directory, disk_min_file_size=64)


def pin_count(cache):
    ((count,),) = cache._sql('SELECT COUNT(pin_id) FROM Pin').fetchall()
    return count


def test_pin_read_after_replace(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    old_value = b'a' * 100
    cache1.set('key', old_value)

    pin = cache1.pin('key')
    assert isinstance(pin, dc.Pin)
    old_path = pin.name

    # Another instance replaces the key while the pin is held.
    cache2.set('key', b'b' * 100)
    assert cache2.get('key') == b'b' * 100

    # Pinned old version file survives and is fully readable.
    assert op.exists(old_path)
    assert pin.read() == old_value

    # Last pin release cleans up the old file.
    pin.release()
    assert pin.closed
    assert not op.exists(old_path)
    assert pin_count(cache1) == 0

    cache1.close()
    cache2.close()


def test_pin_read_after_delete(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100)
    pin = cache1.pin('key')
    old_path = pin.name

    assert cache2.delete('key')
    assert cache1.get('key') is None

    assert op.exists(old_path)
    assert pin.read() == b'a' * 100

    pin.release()
    assert not op.exists(old_path)

    cache1.close()
    cache2.close()


def test_pin_read_after_evict(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100, tag='group')
    pin = cache1.pin('key')
    old_path = pin.name

    assert cache2.evict('group') == 1
    assert cache2.get('key') is None

    assert op.exists(old_path)
    assert pin.read() == b'a' * 100

    pin.release()
    assert not op.exists(old_path)

    cache1.close()
    cache2.close()


def test_pin_read_after_expire(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100, expire=0.1)
    pin = cache1.pin('key')
    old_path = pin.name

    time.sleep(0.2)
    assert cache2.expire() == 1
    assert cache2.get('key') is None

    # Item expired but the pinned file is still readable.
    assert op.exists(old_path)
    assert pin.read() == b'a' * 100

    pin.release()
    assert not op.exists(old_path)

    cache1.close()
    cache2.close()


def test_pin_multiple_handles(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100)
    pin1 = cache1.pin('key')
    pin2 = cache2.pin('key')
    old_path = pin1.name

    cache2.set('key', b'b' * 100)

    # File survives until the last pin is released.
    pin1.release()
    assert op.exists(old_path)
    assert pin2.read() == b'a' * 100

    pin2.release()
    assert not op.exists(old_path)

    cache1.close()
    cache2.close()


def test_pin_context_manager(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100)

    with cache1.pin('key') as pin:
        old_path = pin.name
        cache2.delete('key')
        assert pin.read() == b'a' * 100

    assert pin.closed
    assert not op.exists(old_path)

    cache1.close()
    cache2.close()


def test_pin_lease_expiry_reclaims_file(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100)
    pin = cache1.pin('key', lease=0.1)
    old_path = pin.name

    cache2.delete('key')
    assert op.exists(old_path)

    # Simulate abnormal process exit: the handle is abandoned without
    # release and the OS closes the file.
    pin._handle.close()

    # Lease expires and the abandoned pin is reclaimed by expire().
    time.sleep(0.2)
    cache2.expire()
    assert not op.exists(old_path)
    assert pin_count(cache2) == 0

    pin.release()
    pin.release()  # Double release is a no-op.

    cache1.close()
    cache2.close()


def test_pin_expired_release_keeps_new_file(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100)
    pin = cache1.pin('key', lease=0.1)

    cache2.delete('key')
    time.sleep(0.2)
    cache2.expire()  # Reclaims the expired pin and old file.

    # A new version is written after the pin was reclaimed.
    cache2.set('key', b'b' * 100)
    handle = cache2.get('key', read=True)
    new_path = handle.name
    handle.close()

    # Late release of the reclaimed pin must not delete the new file.
    pin.release()
    assert op.exists(new_path)
    assert cache2.get('key') == b'b' * 100

    cache1.close()
    cache2.close()


def test_pin_interleaved_lifecycle(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    for num in range(5):
        value = bytes([num]) * 100
        cache1.set('key', value, tag='round')

        # Both instances pin the current version.
        pin1 = cache1.pin('key')
        pin2 = cache2.pin('key')
        old_path = pin1.name

        # The other instance replaces the value.
        cache2.set('key', b'x' * 100, tag='round')
        assert cache1.get('key') == b'x' * 100

        # Both pins read the old version in full.
        assert pin1.read() == value
        assert pin2.read() == value

        pin1.release()
        assert op.exists(old_path)  # pin2 still holds the file.
        pin2.release()
        assert not op.exists(old_path)

        # Pin then evict from the other instance.
        with cache1.pin('key') as pin3:
            path3 = pin3.name
            assert cache2.evict('round') == 1
            assert cache1.get('key') is None
            assert pin3.read() == b'x' * 100

        assert not op.exists(path3)
        assert pin_count(cache1) == 0

    cache1.close()
    cache2.close()


def test_pin_small_value_no_overhead(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('int', 42)
    cache1.set('bytes', b'a' * 10)  # Below disk_min_file_size.

    # Small values are returned directly and create no pin rows.
    assert cache1.pin('int') == 42
    assert cache2.pin('bytes') == b'a' * 10
    assert pin_count(cache1) == 0

    cache1.close()
    cache2.close()


def test_normal_get_creates_no_pin(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100)

    # Ordinary get/read incur no pin overhead.
    assert cache2.get('key') == b'a' * 100
    handle = cache2.get('key', read=True)
    handle.close()
    handle = cache2.read('key')
    handle.close()
    assert pin_count(cache1) == 0

    cache1.close()
    cache2.close()


def test_pin_missing_key(directory):
    cache = make_cache(directory)

    assert cache.pin('missing') is None
    assert cache.pin('missing', default='default') == 'default'
    assert pin_count(cache) == 0

    cache.close()


def test_pin_check_consistency(directory):
    cache1 = make_cache(directory)
    cache2 = make_cache(directory)

    cache1.set('key', b'a' * 100)
    pin = cache1.pin('key')
    cache2.delete('key')

    # Pinned files are known files; check reports no warnings.
    warns = cache1.check()
    messages = [str(warn.message) for warn in warns]
    assert not any('unknown file' in message for message in messages)

    pin.release()
    cache1.close()
    cache2.close()
