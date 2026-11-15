"""Test diskcache backup and restore API."""

import hashlib
import json
import multiprocessing as mp
import os
import os.path as op
import shutil
import sqlite3
import tempfile
import threading
import time

import pytest

import diskcache as dc
from diskcache.core import (
    BACKUP_DBNAME,
    BACKUP_MANIFEST_NAME,
    BACKUP_VALUES_DIRNAME,
    _is_relative_path,
)

pytestmark = pytest.mark.filterwarnings('ignore', category=dc.EmptyDirWarning)


@pytest.fixture
def cache():
    with dc.Cache() as cache:
        yield cache
    shutil.rmtree(cache.directory, ignore_errors=True)


def populate(cache):
    """Store a mix of in-database and file-backed values."""
    blob = b'x' * 2**18
    cache['small'] = 'in-database'
    cache[0] = 42
    cache[1] = 1.5
    cache[2] = None
    cache['bytes'] = blob
    cache['text'] = 'y' * 2**18
    cache['object'] = {'nested': list(range(1000))}
    cache.set('expiring', b'z' * 2**18, expire=3600)
    cache.set('tagged', blob, tag='marker')


def assert_restored(source_dir, expected_dir):
    with dc.Cache(source_dir) as source, dc.Cache(expected_dir) as restored:
        source_keys = list(source)
        assert len(restored) == len(source)
        assert set(map(repr, restored)) == set(map(repr, source_keys))

        for key in source_keys:
            assert restored[key] == source[key]

        assert restored.get('tagged', tag=True)[1] == 'marker'
        assert len(restored.check()) == 0

        source_expire = source.get('expiring', expire_time=True)[1]
        restored_expire = restored.get('expiring', expire_time=True)[1]
        assert source_expire == restored_expire
        assert restored_expire > time.time()


def test_backup_restore_roundtrip(cache, tmp_path):
    populate(cache)
    expire_before = cache.get('expiring', expire_time=True)[1]
    cache.close()

    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'

    with dc.Cache(cache.directory) as other:
        assert other.backup(str(backup_dir)) == str(backup_dir)

    assert backup_dir.is_dir()
    assert (backup_dir / BACKUP_DBNAME).is_file()
    assert (backup_dir / BACKUP_MANIFEST_NAME).is_file()
    assert (backup_dir / BACKUP_VALUES_DIRNAME).is_dir()
    # Snapshot must not leave WAL/SHM side-car files behind.
    assert not (backup_dir / (BACKUP_DBNAME + '-wal')).exists()
    assert not (backup_dir / (BACKUP_DBNAME + '-shm')).exists()

    manifest = json.loads(
        (backup_dir / BACKUP_MANIFEST_NAME).read_text(encoding='utf-8')
    )
    assert manifest['format'] == 1
    assert manifest['database']['path'] == BACKUP_DBNAME

    value_files = list((backup_dir / BACKUP_VALUES_DIRNAME).rglob('*.val'))
    assert len(value_files) == len(manifest['files'])
    assert value_files, 'expected file-backed values in backup'

    # Manifest checksums must match the actual backup contents.
    for entry in manifest['files']:
        full_path = backup_dir / entry['path']
        assert full_path.is_file()
        assert full_path.stat().st_size == entry['size']
        digest = hashlib.sha256(full_path.read_bytes()).hexdigest()
        assert digest == entry['sha256']

    result = dc.Cache.restore(str(backup_dir), str(restore_dir))
    assert result == str(restore_dir)

    assert_restored(cache.directory, str(restore_dir))

    with dc.Cache(str(restore_dir)) as restored:
        assert restored.get('expiring', expire_time=True)[1] == expire_before


def test_backup_creates_parent_directory(cache, tmp_path):
    populate(cache)
    nested = tmp_path / 'a' / 'b' / 'backup'
    result = cache.backup(str(nested))
    assert op.isdir(result)


def test_backup_destination_exists(cache, tmp_path):
    cache['key'] = b'v' * 2**18
    existing = tmp_path / 'exists'
    existing.mkdir()

    with pytest.raises(FileExistsError):
        cache.backup(str(existing))


def test_restore_destination_exists(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    target = tmp_path / 'target'
    target.mkdir()
    (target / 'sentinel').write_text('do not touch')

    cache.backup(str(backup_dir))

    with pytest.raises(FileExistsError):
        dc.Cache.restore(str(backup_dir), str(target))

    # Failure must leave the existing target untouched.
    assert (target / 'sentinel').read_text() == 'do not touch'


def test_backup_empty_cache(cache, tmp_path):
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    manifest = json.loads(
        (backup_dir / BACKUP_MANIFEST_NAME).read_text(encoding='utf-8')
    )
    assert manifest['files'] == []

    dc.Cache.restore(str(backup_dir), str(restore_dir))

    with dc.Cache(str(restore_dir)) as restored:
        assert len(restored) == 0
        assert len(restored.check()) == 0


def test_backup_snapshot_excludes_later_entries(cache, tmp_path):
    from unittest import mock

    cache['before'] = b'a' * 2**18
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'

    original_con = type(cache)._con
    original_backup = sqlite3.Connection.backup

    class ConnectionProxy:
        def __init__(self, connection):
            self._connection = connection

        def __getattr__(self, name):
            return getattr(self._connection, name)

        def backup(self, target, *args, **kwargs):
            result = original_backup(
                self._connection, target, *args, **kwargs
            )
            # Rows written after the snapshot never enter the backup.
            cache['after'] = b'b' * 2**18
            return result

    @property
    def patched_con(self):
        return ConnectionProxy(original_con.fget(self))

    with mock.patch.object(type(cache), '_con', patched_con):
        cache.backup(str(backup_dir))

    assert 'after' in cache

    dc.Cache.restore(str(backup_dir), str(restore_dir))

    with dc.Cache(str(restore_dir)) as restored:
        assert 'before' in restored
        assert 'after' not in restored


def test_backup_pins_replaced_value_file(cache, tmp_path):
    cache['key'] = b'old' * 2**16
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'

    original_link = os.link
    replacing = {'done': False}

    def link_then_replace(src, dst):
        result = original_link(src, dst)
        if not replacing['done'] and src.endswith('.val'):
            replacing['done'] = True
            # Simulate another process replacing the value after pinning.
            cache['key'] = b'new' * 2**16
        return result

    with pytest.MonkeyPatch.context() as monkeypatch:
        monkeypatch.setattr(os, 'link', link_then_replace)
        cache.backup(str(backup_dir))

    dc.Cache.restore(str(backup_dir), str(restore_dir))

    with dc.Cache(str(restore_dir)) as restored:
        # Snapshot predates the replacement.
        assert restored['key'] == b'old' * 2**16

    assert cache['key'] == b'new' * 2**16


def test_backup_retries_when_file_disappears(cache, tmp_path):
    import diskcache.core as core

    cache['key'] = b'value' * 2**15
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'

    original = core._link_or_copy
    attempts = {'count': 0}

    def flaky_link_or_copy(src, dst):
        if src.endswith('.val'):
            attempts['count'] += 1
            if attempts['count'] == 1:
                raise FileNotFoundError(src)
        return original(src, dst)

    with pytest.MonkeyPatch.context() as monkeypatch:
        monkeypatch.setattr(core, '_link_or_copy', flaky_link_or_copy)
        cache.backup(str(backup_dir))

    assert attempts['count'] == 2

    dc.Cache.restore(str(backup_dir), str(restore_dir))

    with dc.Cache(str(restore_dir)) as restored:
        assert restored['key'] == b'value' * 2**15


def test_backup_no_retry_raises(cache):
    import diskcache.core as core

    cache['key'] = b'value' * 2**15

    def fail_link_or_copy(src, dst):
        if src.endswith('.val'):
            raise FileNotFoundError(src)
        return core._link_or_copy(src, dst)

    with pytest.MonkeyPatch.context() as monkeypatch:
        monkeypatch.setattr(core, '_link_or_copy', fail_link_or_copy)
        with pytest.raises(dc.BackupError):
            cache.backup(tempfile.mktemp(), retry=False)


def test_backup_concurrent_writers(tmp_path):
    cache_dir = tmp_path / 'cache'
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'

    with dc.Cache(str(cache_dir)) as cache:
        for num in range(20):
            cache[num] = b'%d' % num * 2**15

    stop = threading.Event()
    errors = []

    def writer():
        try:
            with dc.Cache(str(cache_dir)) as other:
                num = 1000
                while not stop.is_set():
                    other[num] = b'w' * 2**15
                    other.delete(num)
                    num += 1
        except Exception as exc:  # pragma: no cover
            errors.append(exc)

    thread = threading.Thread(target=writer)
    thread.start()

    try:
        with dc.Cache(str(cache_dir)) as backup_cache:
            backup_cache.backup(str(backup_dir))
    finally:
        stop.set()
        thread.join()

    assert not errors

    dc.Cache.restore(str(backup_dir), str(restore_dir))

    with dc.Cache(str(restore_dir)) as restored:
        assert len(restored.check()) == 0
        for num in range(20):
            assert restored[num] == b'%d' % num * 2**15


def test_restore_detects_corrupted_value_file(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    entry = json.loads(
        (backup_dir / BACKUP_MANIFEST_NAME).read_text(encoding='utf-8')
    )['files'][0]
    with open(backup_dir / entry['path'], 'wb') as writer:
        writer.write(b'corrupted')

    with pytest.raises(dc.BackupError, match='mismatch'):
        dc.Cache.restore(str(backup_dir), str(restore_dir))

    assert not restore_dir.exists()


def test_restore_detects_corrupted_database(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    db_path = backup_dir / BACKUP_DBNAME
    with open(db_path, 'r+b') as writer:
        writer.seek(100)
        writer.write(b'\x00' * 8)

    with pytest.raises(dc.BackupError):
        dc.Cache.restore(str(backup_dir), str(restore_dir))

    assert not restore_dir.exists()


def test_restore_detects_missing_file(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    manifest = json.loads(
        (backup_dir / BACKUP_MANIFEST_NAME).read_text(encoding='utf-8')
    )
    os.remove(backup_dir / manifest['files'][0]['path'])

    with pytest.raises(dc.BackupError, match='missing'):
        dc.Cache.restore(str(backup_dir), str(restore_dir))

    assert not restore_dir.exists()


def test_restore_rejects_path_escape(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    manifest_path = backup_dir / BACKUP_MANIFEST_NAME
    manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
    manifest['files'][0]['path'] = '../escape.val'
    manifest_path.write_text(json.dumps(manifest))

    with pytest.raises(dc.BackupError, match='invalid file path'):
        dc.Cache.restore(str(backup_dir), str(restore_dir))

    assert not restore_dir.exists()
    assert not (tmp_path / 'escape.val').exists()


def test_restore_rejects_absolute_source(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    manifest_path = backup_dir / BACKUP_MANIFEST_NAME
    manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
    manifest['files'][0]['source'] = op.abspath(__file__)
    manifest_path.write_text(json.dumps(manifest))

    with pytest.raises(dc.BackupError, match='invalid source path'):
        dc.Cache.restore(str(backup_dir), str(restore_dir))


def test_restore_rejects_bad_format(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    manifest_path = backup_dir / BACKUP_MANIFEST_NAME
    manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
    manifest['format'] = 999
    manifest_path.write_text(json.dumps(manifest))

    with pytest.raises(dc.BackupError, match='format'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_restore_missing_manifest(tmp_path):
    empty = tmp_path / 'empty-backup'
    empty.mkdir()

    with pytest.raises(dc.BackupError, match='manifest'):
        dc.Cache.restore(str(empty), str(tmp_path / 'out'))


def test_restore_missing_backup_directory(tmp_path):
    with pytest.raises(dc.BackupError, match='does not exist'):
        dc.Cache.restore(
            str(tmp_path / 'nope'), str(tmp_path / 'out')
        )


def test_restore_detects_manifest_database_mismatch(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    manifest_path = backup_dir / BACKUP_MANIFEST_NAME
    manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
    manifest['files'].pop()
    manifest_path.write_text(json.dumps(manifest))

    with pytest.raises(dc.BackupError, match='does not match'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def _load_manifest(backup_dir):
    manifest_path = backup_dir / BACKUP_MANIFEST_NAME
    return json.loads(manifest_path.read_text(encoding='utf-8'))


def _write_manifest(backup_dir, manifest):
    (backup_dir / BACKUP_MANIFEST_NAME).write_text(json.dumps(manifest))


def test_restore_rejects_malformed_manifest(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    (backup_dir / BACKUP_MANIFEST_NAME).write_text('{not json')

    with pytest.raises(dc.BackupError, match='manifest'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_restore_rejects_non_object_manifest(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))
    (backup_dir / BACKUP_MANIFEST_NAME).write_text(json.dumps([1, 2]))

    with pytest.raises(dc.BackupError, match='root must be an object'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_restore_rejects_bad_manifest_structure(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    manifest = _load_manifest(backup_dir)
    manifest['database'] = 'not-a-dict'
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='structure'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_restore_rejects_non_dict_file_entry(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    manifest = _load_manifest(backup_dir)
    manifest['files'].append('bogus')
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='manifest entry'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_restore_rejects_duplicate_manifest_entry(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    manifest = _load_manifest(backup_dir)
    manifest['files'].append(dict(manifest['files'][0]))
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='duplicate'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_restore_rejects_invalid_size_and_digest(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    manifest = _load_manifest(backup_dir)
    original_size = manifest['files'][0]['size']
    manifest['files'][0]['size'] = -1
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='invalid size'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out1'))

    manifest = _load_manifest(backup_dir)
    manifest['files'][0]['size'] = original_size
    manifest['files'][0]['sha256'] = 'abc'
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='invalid checksum'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out2'))


def test_restore_rejects_database_path_escape(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    manifest = _load_manifest(backup_dir)
    manifest['database']['path'] = '../../escape.db'
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='invalid file path'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_restore_rejects_decoy_database(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    # Swap in a valid but unrelated SQLite database with matching checksum,
    # so the database-vs-manifest reference check is what rejects the backup.
    decoy = tmp_path / 'decoy.db'
    decoy_db = sqlite3.connect(str(decoy))
    decoy_db.execute('CREATE TABLE Cache (filename TEXT)')
    decoy_db.execute("INSERT INTO Cache VALUES ('ab/cd/decoy.val')")
    decoy_db.commit()
    decoy_db.close()

    shutil.copyfile(decoy, backup_dir / BACKUP_DBNAME)
    manifest = _load_manifest(backup_dir)
    manifest['database']['size'] = decoy.stat().st_size
    manifest['database']['sha256'] = hashlib.sha256(
        decoy.read_bytes()
    ).hexdigest()
    manifest['files'] = []
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='does not match'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_restore_rejects_database_with_escaping_path(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    db_path = backup_dir / BACKUP_DBNAME
    connection = sqlite3.connect(str(db_path))
    connection.execute(
        "UPDATE Cache SET filename = ? WHERE rowid = ("
        " SELECT rowid FROM Cache WHERE filename IS NOT NULL LIMIT 1)",
        ('../escape.val',),
    )
    connection.commit()
    connection.close()

    # Recompute the database checksum so only the path check can fail.
    manifest = _load_manifest(backup_dir)
    manifest['database']['sha256'] = hashlib.sha256(
        db_path.read_bytes()
    ).hexdigest()
    manifest['database']['size'] = db_path.stat().st_size
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='invalid file path'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_backup_rejects_invalid_database_path(cache, tmp_path):
    cache['key'] = b'v' * 2**15
    ((rowid,),) = cache._sql(
        'SELECT rowid FROM Cache WHERE filename IS NOT NULL LIMIT 1'
    ).fetchall()
    cache._sql(
        'UPDATE Cache SET filename = ? WHERE rowid = ?',
        ('../escape.val', rowid),
    )

    with pytest.raises(dc.BackupError, match='invalid file path'):
        cache.backup(str(tmp_path / 'backup'))


def test_backup_cleans_up_on_error(cache, tmp_path):
    import diskcache.core as core

    populate(cache)
    backup_dir = tmp_path / 'backup'

    def boom(*args, **kwargs):
        raise OSError('simulated copy failure')

    with pytest.MonkeyPatch.context() as monkeypatch:
        monkeypatch.setattr(core.shutil, 'copyfile', boom)
        with pytest.raises(OSError):
            cache.backup(str(backup_dir))

    # No destination and no temporary directories left behind.
    assert not backup_dir.exists()
    leftovers = [
        name
        for name in os.listdir(cache.directory)
        if name.startswith('.diskcache-backup-')
    ]
    assert leftovers == []


def test_backup_rename_failure_cleans_up(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'

    original_rename = os.rename

    def fail_rename(src, dst):
        if op.abspath(dst) == op.abspath(str(backup_dir)):
            raise OSError('simulated rename failure')
        return original_rename(src, dst)

    with pytest.MonkeyPatch.context() as monkeypatch:
        monkeypatch.setattr(os, 'rename', fail_rename)
        with pytest.raises(OSError):
            cache.backup(str(backup_dir))

    assert not backup_dir.exists()


def test_restore_race_with_created_target(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    original_rename = os.rename

    def create_target_then_rename(src, dst):
        if op.abspath(dst) == op.abspath(str(restore_dir)):
            os.makedirs(str(restore_dir))
        return original_rename(src, dst)

    with pytest.MonkeyPatch.context() as monkeypatch:
        monkeypatch.setattr(os, 'rename', create_target_then_rename)
        with pytest.raises(FileExistsError):
            dc.Cache.restore(str(backup_dir), str(restore_dir))

    # The target created by the racing process must remain in place.
    assert restore_dir.is_dir()


def test_restore_target_appears_before_rename(cache, tmp_path):
    from unittest import mock

    populate(cache)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    real_exists = op.exists
    checks = {'count': 0}

    def appears_during_restore(path):
        if op.abspath(str(path)) == op.abspath(str(restore_dir)):
            checks['count'] += 1

            if checks['count'] == 2:
                restore_dir.mkdir()

        return real_exists(path)

    with mock.patch('diskcache.core.op.exists', appears_during_restore):
        with pytest.raises(FileExistsError):
            dc.Cache.restore(str(backup_dir), str(restore_dir))

    assert restore_dir.is_dir()


def test_restore_database_open_failure(cache, tmp_path):
    import diskcache.core as core
    from unittest import mock

    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    with mock.patch.object(
        core.sqlite3,
        'connect',
        side_effect=sqlite3.OperationalError('simulated open failure'),
    ):
        with pytest.raises(dc.BackupError, match='cannot open backup'):
            dc.Cache.restore(
                str(backup_dir), str(tmp_path / 'out')
            )


def test_restore_database_query_failure(cache, tmp_path):
    import diskcache.core as core
    from unittest import mock

    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    original_connect = core.sqlite3.connect

    class BrokenConnection:
        def __init__(self, path, *args, **kwargs):
            self._inner = original_connect(path, *args, **kwargs)

        def execute(self, statement, *args, **kwargs):
            if statement.strip().startswith('SELECT DISTINCT filename'):
                raise sqlite3.DatabaseError('simulated query failure')
            return self._inner.execute(statement, *args, **kwargs)

        def close(self):
            self._inner.close()

    with mock.patch.object(
        core.sqlite3, 'connect', side_effect=BrokenConnection
    ):
        with pytest.raises(dc.BackupError, match='unreadable'):
            dc.Cache.restore(
                str(backup_dir), str(tmp_path / 'out')
            )


def test_restore_ignores_unrelated_files(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))

    # Files not referenced by the manifest must not be restored.
    junk = backup_dir / 'junk.bin'
    junk.write_bytes(b'not part of the cache')
    nested = backup_dir / BACKUP_VALUES_DIRNAME / 'stray.val'
    nested.write_bytes(b'stray')

    dc.Cache.restore(str(backup_dir), str(restore_dir))

    assert not (restore_dir / 'junk.bin').exists()
    assert not (restore_dir / BACKUP_VALUES_DIRNAME / 'stray.val').exists()

    with dc.Cache(str(restore_dir)) as restored:
        assert len(restored.check(fix=True)) == 0


def test_ttl_uses_absolute_expiry(cache, tmp_path):
    cache.set('key', b'v' * 2**18, expire=100)
    expire_at = cache.get('key', expire_time=True)[1]

    time.sleep(0.05)
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'
    cache.backup(str(backup_dir))
    dc.Cache.restore(str(backup_dir), str(restore_dir))

    with dc.Cache(str(restore_dir)) as restored:
        restored_expire = restored.get('key', expire_time=True)[1]
        assert restored_expire == expire_at


def test_backup_works_with_json_disk(tmp_path):
    with dc.Cache(str(tmp_path / 'cache'), disk=dc.JSONDisk) as cache:
        cache['a'] = {'x': [1, 2, 3]}
        cache['b'] = 'z' * 2**17
        backup_dir = tmp_path / 'backup'
        restore_dir = tmp_path / 'restored'
        cache.backup(str(backup_dir))

    dc.Cache.restore(str(backup_dir), str(restore_dir))

    with dc.Cache(
        str(restore_dir), disk=dc.JSONDisk
    ) as restored:
        assert restored['a'] == {'x': [1, 2, 3]}
        assert restored['b'] == 'z' * 2**17
        assert len(restored.check()) == 0


def test_is_relative_path_helper():
    assert _is_relative_path('ab/cd.val')
    assert _is_relative_path('ab\\cd.val')
    assert _is_relative_path('a')
    assert not _is_relative_path('')
    assert not _is_relative_path('/ab/cd.val')
    assert not _is_relative_path('ab/../cd.val')
    assert not _is_relative_path('ab/./cd.val')
    assert not _is_relative_path('../cd.val')
    assert not _is_relative_path('ab/')
    assert not _is_relative_path('C:\\ab\\cd.val')


def _child_process_writer(cache_dir, ready, stop_flag):
    """Top-level worker so spawn-based multiprocessing can import it."""
    cache = dc.Cache(cache_dir)
    ready.set()
    num = 0
    while not stop_flag.wait(0.001):
        cache['child-%d' % num] = b'c' * 2**15
        num += 1
    cache.close()


def test_backup_while_other_process_writes(tmp_path):
    cache_dir = str(tmp_path / 'cache')
    backup_dir = str(tmp_path / 'backup')
    restore_dir = str(tmp_path / 'restored')

    ctx = mp.get_context('spawn')
    ready = ctx.Event()
    stop_flag = ctx.Event()
    process = ctx.Process(
        target=_child_process_writer,
        args=(cache_dir, ready, stop_flag),
    )
    process.start()

    try:
        assert ready.wait(30)
        with dc.Cache(cache_dir) as cache:
            cache['parent'] = b'p' * 2**15
            cache.backup(backup_dir)
    finally:
        stop_flag.set()
        process.join(30)
        assert process.exitcode == 0

    dc.Cache.restore(backup_dir, restore_dir)

    with dc.Cache(restore_dir) as restored:
        assert 'parent' in restored
        assert len(restored.check()) == 0


def test_restore_rejects_path_outside_backup(cache, tmp_path):
    from unittest import mock

    populate(cache)
    backup_dir = tmp_path / 'backup'
    outside = tmp_path / 'outside.val'
    outside.write_bytes(b'x')
    cache.backup(str(backup_dir))

    manifest = _load_manifest(backup_dir)
    manifest['files'][0]['path'] = '../outside.val'
    _write_manifest(backup_dir, manifest)

    # Defense in depth: bypass the string validator so the common-path
    # containment check is what stops the escaping manifest path.
    with mock.patch(
        'diskcache.core._is_relative_path', return_value=True
    ):
        with pytest.raises(dc.BackupError, match='escapes the backup'):
            dc.Cache.restore(
                str(backup_dir), str(tmp_path / 'out')
            )

    assert not (tmp_path / 'out').exists()


def test_restore_rejects_database_failing_integrity(cache, tmp_path):
    populate(cache)
    backup_dir = tmp_path / 'backup'
    cache.backup(str(backup_dir))

    db_path = backup_dir / BACKUP_DBNAME

    with open(db_path, 'r+b') as writer:
        writer.seek(52)
        writer.write(b'\x00\x01')

    # Recompute the checksum so verification reaches PRAGMA integrity_check.
    manifest = _load_manifest(backup_dir)
    manifest['database']['sha256'] = hashlib.sha256(
        db_path.read_bytes()
    ).hexdigest()
    manifest['database']['size'] = db_path.stat().st_size
    _write_manifest(backup_dir, manifest)

    with pytest.raises(dc.BackupError, match='integrity check'):
        dc.Cache.restore(str(backup_dir), str(tmp_path / 'out'))


def test_fanout_backup_restore(tmp_path):
    cache_dir = tmp_path / 'fanout'
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'

    with dc.FanoutCache(str(cache_dir), shards=4) as cache:
        for num in range(50):
            cache.set(num, b'%d' % num * 2**15)
        cache.set('expiring', 'v' * 2**17, expire=600)
        expire_at = cache.get('expiring', expire_time=True)[1]
        cache.backup(str(backup_dir))

    manifest = json.loads(
        (backup_dir / BACKUP_MANIFEST_NAME).read_text(encoding='utf-8')
    )
    assert manifest['type'] == 'fanout'
    assert manifest['shards'] == 4

    for num in range(4):
        shard_backup = backup_dir / ('%03d' % num)
        assert (shard_backup / BACKUP_MANIFEST_NAME).is_file()

    restored = dc.FanoutCache.restore(
        str(backup_dir), str(restore_dir)
    )

    try:
        assert restored._count == 4
        assert len(restored) == 51
        for num in range(50):
            assert restored[num] == b'%d' % num * 2**15
        assert (
            restored.get('expiring', expire_time=True)[1] == expire_at
        )
        assert len(restored.check()) == 0
    finally:
        restored.close()
        shutil.rmtree(cache_dir, ignore_errors=True)


def test_fanout_restore_rejects_corrupt_shard(tmp_path):
    cache_dir = tmp_path / 'fanout'
    backup_dir = tmp_path / 'backup'
    restore_dir = tmp_path / 'restored'

    with dc.FanoutCache(str(cache_dir), shards=2) as cache:
        cache['a'] = b'v' * 2**15
        cache.backup(str(backup_dir))

    shard_manifest = backup_dir / '000' / BACKUP_MANIFEST_NAME
    manifest = json.loads(shard_manifest.read_text(encoding='utf-8'))
    entry = manifest['files'][0]
    with open(backup_dir / '000' / entry['path'], 'wb') as writer:
        writer.write(b'bad')

    with pytest.raises(dc.BackupError):
        dc.FanoutCache.restore(str(backup_dir), str(restore_dir))

    assert not restore_dir.exists()
    shutil.rmtree(cache_dir, ignore_errors=True)


def test_fanout_backup_destination_exists(tmp_path):
    cache_dir = tmp_path / 'fanout'
    existing = tmp_path / 'existing'
    existing.mkdir()

    with dc.FanoutCache(str(cache_dir), shards=2) as cache:
        with pytest.raises(FileExistsError):
            cache.backup(str(existing))

    shutil.rmtree(cache_dir, ignore_errors=True)


def test_fanout_restore_destination_exists(tmp_path):
    cache_dir = tmp_path / 'fanout'
    backup_dir = tmp_path / 'backup'
    target = tmp_path / 'target'
    target.mkdir()

    with dc.FanoutCache(str(cache_dir), shards=2) as cache:
        cache['a'] = b'v' * 2**15
        cache.backup(str(backup_dir))

    with pytest.raises(FileExistsError):
        dc.FanoutCache.restore(str(backup_dir), str(target))

    shutil.rmtree(cache_dir, ignore_errors=True)


def test_fanout_restore_bad_manifest(tmp_path):
    cache_dir = tmp_path / 'fanout'
    backup_dir = tmp_path / 'backup'

    with dc.FanoutCache(str(cache_dir), shards=2) as cache:
        cache['a'] = b'v' * 2**15
        cache.backup(str(backup_dir))

    manifest = backup_dir / BACKUP_MANIFEST_NAME
    valid = json.loads(manifest.read_text(encoding='utf-8'))

    manifest.write_text('{broken')
    with pytest.raises(dc.BackupError, match='cannot read'):
        dc.FanoutCache.restore(
            str(backup_dir), str(tmp_path / 'out1')
        )

    wrong_type = dict(valid)
    wrong_type['type'] = 'unknown'
    manifest.write_text(json.dumps(wrong_type))
    with pytest.raises(dc.BackupError, match='invalid fanout'):
        dc.FanoutCache.restore(
            str(backup_dir), str(tmp_path / 'out2')
        )

    zero_shards = dict(valid)
    zero_shards['shards'] = 0
    manifest.write_text(json.dumps(zero_shards))
    with pytest.raises(dc.BackupError, match='invalid shard count'):
        dc.FanoutCache.restore(
            str(backup_dir), str(tmp_path / 'out3')
        )

    shutil.rmtree(cache_dir, ignore_errors=True)


def test_fanout_backup_cleans_up_on_failure(tmp_path):
    cache_dir = tmp_path / 'fanout'
    backup_dir = tmp_path / 'backup'

    with dc.FanoutCache(str(cache_dir), shards=2) as cache:
        cache['a'] = b'v' * 2**15

        original = cache._shards[1].backup

        def fail_backup(directory, retry=True):
            raise OSError('simulated shard failure')

        cache._shards[1].backup = fail_backup
        try:
            with pytest.raises(OSError):
                cache.backup(str(backup_dir))
        finally:
            cache._shards[1].backup = original

    assert not backup_dir.exists()
    leftovers = [
        name
        for name in os.listdir(str(tmp_path))
        if name.startswith('.diskcache-backup-')
    ]
    assert leftovers == []
    shutil.rmtree(cache_dir, ignore_errors=True)


def test_fanout_restore_cleans_up_on_failure(tmp_path):
    import diskcache.fanout as fanout

    cache_dir = tmp_path / 'fanout'
    backup_dir = tmp_path / 'backup'

    with dc.FanoutCache(str(cache_dir), shards=2) as cache:
        cache['a'] = b'v' * 2**15
        cache.backup(str(backup_dir))

    original = fanout.Cache.restore

    def fail_restore(source, directory):
        raise OSError('simulated restore failure')

    with pytest.MonkeyPatch.context() as monkeypatch:
        monkeypatch.setattr(fanout.Cache, 'restore', staticmethod(fail_restore))
        with pytest.raises(OSError):
            dc.FanoutCache.restore(
                str(backup_dir), str(tmp_path / 'restored')
            )

    assert not (tmp_path / 'restored').exists()
    shutil.rmtree(cache_dir, ignore_errors=True)
