import io
import json
import pickle

import pytest

import diskcache as dc
from diskcache import Cache, EncodingVersionError, VersionedDisk


def json_writer(value):
    return json.dumps(value).encode('utf-8')


def json_reader(data):
    return json.loads(data.decode('utf-8'))


def json_disk(version, readers=(), **kwargs):
    """Build a VersionedDisk subclass using JSON at the given version."""

    class JsonDisk(VersionedDisk):
        def __init__(self, directory, **kwargs):
            super().__init__(
                directory,
                version=version,
                writer=json_writer,
                reader=json_reader,
                readers={old: json_reader for old in readers},
                **kwargs,
            )

    return JsonDisk


def stored_version(cache, key):
    """Return the encoding version of the stored row for key."""
    db_key, raw = cache.disk.put(key)
    row = cache._sql(
        'SELECT mode, filename, value FROM Cache WHERE key = ? AND raw = ?',
        (db_key, raw),
    ).fetchone()
    return cache.disk.peek_version(*row)


@pytest.fixture
def directory(tmp_path):
    return str(tmp_path / 'cache')


def test_default_roundtrip(directory):
    cache = Cache(directory, disk=VersionedDisk)
    values = [
        'text',
        42,
        3.14,
        b'bytes',
        {'key': ['a', 'b']},
        None,
        (1, 2),
    ]
    for index, value in enumerate(values):
        cache[index] = value
    for index, value in enumerate(values):
        assert cache[index] == value
        assert stored_version(cache, index) == 1
    cache.close()


def test_old_version_readable_with_registered_reader(directory):
    cache = Cache(directory, disk=json_disk(1))
    cache['old'] = {'a': 1}
    cache.close()

    cache = Cache(directory, disk=json_disk(2, readers=(1,)))
    assert cache['old'] == {'a': 1}
    assert stored_version(cache, 'old') == 1

    cache['new'] = {'b': 2}
    assert stored_version(cache, 'new') == 2
    assert cache['new'] == {'b': 2}
    cache.close()


def test_missing_reader_raises_diagnosable_error(directory):
    cache = Cache(directory, disk=json_disk(1))
    cache['old'] = {'a': 1}
    cache.close()

    # Reopen without registering a reader for version 1.
    cache = Cache(directory, disk=json_disk(2))

    with pytest.raises(EncodingVersionError) as exc_info:
        cache.get('old')

    error = exc_info.value
    assert error.version == 1
    assert error.readers == (2,)
    assert error.key == 'old'
    assert 'old' in str(error)

    # Not treated as a cache miss: default is not returned and the entry is
    # not overwritten.
    with pytest.raises(EncodingVersionError):
        cache.get('old', default='fallback')
    with pytest.raises(EncodingVersionError):
        cache['old']
    assert 'old' in cache

    # Statistics do not record a miss for the encoding error.
    assert cache.stats(enable=True) == (0, 0)
    with pytest.raises(EncodingVersionError):
        cache.get('old')
    assert cache.stats() == (0, 0)

    # Registering the reader makes the entry readable again.
    cache.disk.register_reader(1, json_reader)
    assert cache['old'] == {'a': 1}
    cache.close()


def test_get_migrate_preserves_expire_time_and_tag(directory):
    cache = Cache(directory, disk=json_disk(1))
    cache.set('key', [1, 2], expire=1000, tag='tag-a')
    _, expire_time, tag = cache.get('key', expire_time=True, tag=True)
    cache.close()

    cache = Cache(directory, disk=json_disk(2, readers=(1,)))
    value, new_expire_time, new_tag = cache.get(
        'key', expire_time=True, tag=True, migrate=True
    )
    assert value == [1, 2]
    assert new_expire_time == expire_time
    assert new_tag == 'tag-a'
    assert stored_version(cache, 'key') == 2
    cache.close()

    # Entry is now readable without the old reader.
    cache = Cache(directory, disk=json_disk(2))
    assert cache['key'] == [1, 2]
    cache.close()


def test_get_migrate_requires_versioned_disk(directory):
    cache = Cache(directory)
    cache['key'] = 'value'
    with pytest.raises(ValueError):
        cache.get('key', migrate=True)
    with pytest.raises(ValueError):
        cache.migrate_key('key')
    with pytest.raises(ValueError):
        cache.migrate()
    cache.close()


def test_migrate_key_conflict_skips(directory):
    cache = Cache(directory, disk=json_disk(1))
    cache['key'] = {'original': True}
    cache.close()

    cache = Cache(directory, disk=json_disk(2, readers=(1,)))
    disk = cache.disk
    original_store = disk.store
    triggered = []

    def store_wrapper(value, read, key=dc.UNKNOWN):
        # Simulate a concurrent writer replacing the value between the
        # read and the compare-and-swap commit of the migration.
        if key == 'key' and not triggered:
            triggered.append(True)
            cache.set('key', {'replaced': True}, retry=True)
        return original_store(value, read, key=key)

    disk.store = store_wrapper

    assert cache.migrate_key('key') == 'conflict'
    assert cache['key'] == {'replaced': True}
    # The replacement was written with the current version.
    assert stored_version(cache, 'key') == 2
    cache.close()


def test_migrate_key_status(directory):
    cache = Cache(directory, disk=json_disk(1))
    cache['key'] = [1]
    cache.close()

    cache = Cache(directory, disk=json_disk(2, readers=(1,)))
    assert cache.migrate_key('missing') == 'missing'
    assert cache.migrate_key('key') == 'migrated'
    assert cache.migrate_key('key') == 'current'
    cache.close()


def test_failed_migration_preserves_file_value(directory):
    settings = {'disk_min_file_size': 8}
    cache = Cache(directory, disk=json_disk(1), **settings)
    cache['file-key'] = {'data': 'x' * 100}
    assert stored_version(cache, 'file-key') == 1
    cache.close()

    def bad_writer(value):
        raise RuntimeError('cannot encode')

    class BadDisk(VersionedDisk):
        def __init__(self, directory, **kwargs):
            super().__init__(
                directory,
                version=2,
                writer=bad_writer,
                reader=json_reader,
                readers={1: json_reader},
                **kwargs,
            )

    cache = Cache(directory, disk=BadDisk, **settings)

    with pytest.raises(RuntimeError):
        cache.migrate_key('file-key')

    # The old file is still in place and readable with the old reader.
    assert stored_version(cache, 'file-key') == 1
    assert cache['file-key'] == {'data': 'x' * 100}
    cache.close()


def test_batch_migrate(directory):
    cache = Cache(directory, disk=json_disk(1))
    cache['a'] = [1]
    cache['b'] = [2]
    cache.close()

    # Entry written with a version that will have no registered reader.
    cache = Cache(directory, disk=json_disk(99))
    cache['unknown'] = [99]
    cache.close()

    cache = Cache(directory, disk=json_disk(2, readers=(1,)))
    cache['current'] = [3]

    # Force a conflict on key 'b' during migration.
    disk = cache.disk
    original_store = disk.store
    triggered = []

    def store_wrapper(value, read, key=dc.UNKNOWN):
        if key == 'b' and not triggered:
            triggered.append(True)
            cache.set('b', ['replaced'], retry=True)
        return original_store(value, read, key=key)

    disk.store = store_wrapper

    report = cache.migrate(batch_size=2)

    assert sorted(report.migrated) == ['a']
    assert report.conflicted == ['b']
    assert [key for key, _ in report.failed] == ['unknown']

    key, error = report.failed[0]
    assert key == 'unknown'
    assert isinstance(error, EncodingVersionError)
    assert error.version == 99

    assert cache['a'] == [1]
    assert stored_version(cache, 'a') == 2
    assert cache['b'] == ['replaced']
    assert cache['current'] == [3]
    cache.close()


def test_legacy_disk_entries_migrate(directory):
    # Write entries with the plain Disk serializer.
    cache = Cache(directory)
    cache['pickled'] = {'a': [1, 2]}
    cache['scalar'] = 42
    cache['raw-bytes'] = b'raw'
    cache.close()

    # Legacy entries are readable and reported as version 0.
    cache = Cache(directory, disk=VersionedDisk)
    assert cache['pickled'] == {'a': [1, 2]}
    assert cache['scalar'] == 42
    assert cache['raw-bytes'] == b'raw'
    assert stored_version(cache, 'pickled') == 0

    report = cache.migrate()
    assert sorted(report.migrated) == ['pickled', 'raw-bytes', 'scalar']
    assert report.conflicted == []
    assert report.failed == []
    assert stored_version(cache, 'pickled') == 1
    cache.close()

    # Entries are now tagged with the current version.
    cache = Cache(directory, disk=VersionedDisk)
    assert cache['pickled'] == {'a': [1, 2]}
    assert cache['scalar'] == 42
    assert cache['raw-bytes'] == b'raw'
    cache.close()


def test_read_true_opaque_bytes(directory):
    cache = Cache(directory, disk=VersionedDisk)
    data = b'x' * 1000
    cache.set('file', io.BytesIO(data), read=True)

    with cache.read('file') as handle:
        assert handle.read() == data

    # Opaque bytes are stored untagged, like the base Disk.
    assert stored_version(cache, 'file') == 0
    cache.close()


def test_file_backed_value_migration(directory):
    settings = {'disk_min_file_size': 8}
    cache = Cache(directory, disk=json_disk(1), **settings)
    cache['big'] = {'data': 'y' * 100}
    cache.close()

    cache = Cache(directory, disk=json_disk(2, readers=(1,)), **settings)
    assert cache.migrate_key('big') == 'migrated'
    assert stored_version(cache, 'big') == 2
    assert cache['big'] == {'data': 'y' * 100}
    cache.close()


def test_register_reader_validation(directory):
    cache = Cache(directory, disk=VersionedDisk)
    with pytest.raises(ValueError):
        cache.disk.register_reader(0, pickle.loads)
    with pytest.raises(ValueError):
        cache.disk.register_reader(1, 'not-callable')
    with pytest.raises(ValueError):
        VersionedDisk(directory, version=0)
    cache.close()
