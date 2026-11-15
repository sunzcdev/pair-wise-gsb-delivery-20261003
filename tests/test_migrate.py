"""Test diskcache.migrate offline FanoutCache migration."""

import json
import os
import os.path as op
import shutil
import sqlite3
import time
import warnings

import pytest

import diskcache as dc
from diskcache.migrate import (
    INCOMPLETE_FILENAME,
    MANIFEST_FILENAME,
    FanoutCacheMigration,
    MigrationError,
    begin_maintenance,
    end_maintenance,
    in_maintenance,
    maintenance,
    migration_complete,
)
from diskcache import migrate as migrate_mod

warnings.simplefilter('error')
warnings.simplefilter('ignore', category=dc.EmptyDirWarning)


@pytest.fixture
def paths(tmp_path):
    source = str(tmp_path / 'source')
    destination = str(tmp_path / 'destination')
    yield source, destination
    for directory in (source, destination):
        shutil.rmtree(directory, ignore_errors=True)


def make_source(directory, shards=4, count=100, **settings):
    cache = dc.FanoutCache(directory, shards=shards, **settings)
    for num in range(count):
        key = 'key-%04d' % num
        cache.set(key, num, tag='odd' if num % 2 else 'even')
    return cache


def test_maintenance_marker(paths):
    source, _ = paths
    cache = make_source(source)
    cache.close()

    assert not in_maintenance(source)
    begin_maintenance(source)
    assert in_maintenance(source)
    with pytest.raises(MigrationError):
        begin_maintenance(source)
    end_maintenance(source)
    assert not in_maintenance(source)
    with pytest.raises(MigrationError):
        end_maintenance(source)

    with maintenance(source):
        assert in_maintenance(source)
    assert not in_maintenance(source)


def test_migration_requires_maintenance(paths):
    source, destination = paths
    cache = make_source(source)
    cache.close()

    with pytest.raises(MigrationError):
        FanoutCacheMigration(source, destination, shards=8)


def test_migration_requires_empty_destination(paths):
    source, destination = paths
    cache = make_source(source)
    cache.close()
    begin_maintenance(source)

    os.makedirs(destination)
    with open(op.join(destination, 'junk'), 'w') as writer:
        writer.write('junk')

    with pytest.raises(MigrationError):
        FanoutCacheMigration(source, destination, shards=8)


def test_migration_rejects_same_directory(paths):
    source, _ = paths
    cache = make_source(source)
    cache.close()
    begin_maintenance(source)

    with pytest.raises(MigrationError):
        FanoutCacheMigration(source, source, shards=8)


def test_basic_migration(paths):
    source, destination = paths
    cache = make_source(source, shards=4, count=200)
    expected = {}
    for num in range(200):
        key = 'key-%04d' % num
        expected[key] = cache.get(key, expire_time=True, tag=True)
    cache.close()

    begin_maintenance(source)
    try:
        with FanoutCacheMigration(source, destination, shards=8) as mig:
            stats = mig.run()
    finally:
        end_maintenance(source)

    assert stats['rows'] == 200
    assert stats['files'] == 0
    assert migration_complete(destination)
    assert not op.exists(op.join(destination, INCOMPLETE_FILENAME))

    with dc.FanoutCache(destination, shards=8) as migrated:
        assert len(migrated) == 200
        for key, (value, expire_time, tag) in expected.items():
            result = migrated.get(key, expire_time=True, tag=True)
            assert result == (value, expire_time, tag), key

    # Source is untouched and still usable with the old layout.
    with dc.FanoutCache(source, shards=4) as original:
        assert len(original) == 200
        for key, (value, _, _) in expected.items():
            assert original[key] == value


def test_file_values_streamed(paths):
    source, destination = paths
    settings = {'disk_min_file_size': 64}
    cache = dc.FanoutCache(source, shards=2, **settings)
    payload = os.urandom(2**20)  # 1 MiB, stored as a file.
    big_text = 'abc\n' * 10000  # Stored as a text file.
    cache.set('blob', payload)
    cache.set('text', big_text)
    cache.set('small', b'tiny')
    cache.close()

    begin_maintenance(source)
    try:
        with FanoutCacheMigration(source, destination, shards=6) as mig:
            stats = mig.run()
    finally:
        end_maintenance(source)

    assert stats['files'] == 2
    assert stats['bytes'] >= len(payload)

    with dc.FanoutCache(destination, shards=6) as migrated:
        assert migrated.get('blob') == payload
        assert migrated.get('text') == big_text
        assert migrated.get('small') == b'tiny'
        with migrated.read('blob') as reader:
            assert reader.read() == payload


def test_ttl_and_tags_preserved(paths):
    source, destination = paths
    cache = dc.FanoutCache(source, shards=2)
    cache.set('session', {'user': 1}, expire=3600, tag='sessions')
    cache.set('forever', 'value')
    _, source_expire, source_tag = cache.get(
        'session', expire_time=True, tag=True
    )
    cache.close()

    begin_maintenance(source)
    try:
        with FanoutCacheMigration(source, destination, shards=4) as mig:
            mig.run()
    finally:
        end_maintenance(source)

    with dc.FanoutCache(destination, shards=4) as migrated:
        value, expire_time, tag = migrated.get(
            'session', expire_time=True, tag=True
        )
        assert value == {'user': 1}
        assert tag == 'sessions' == source_tag
        # Absolute expiry is preserved, so remaining TTL is preserved.
        assert expire_time == source_expire
        assert 3500 < expire_time - time.time() <= 3600
        assert migrated.get('forever', expire_time=True) == ('value', None)
        # Tag eviction works on the migrated cache.
        assert migrated.evict('sessions') == 1
        assert migrated.get('session') is None


def test_expired_rows_skipped(paths):
    source, destination = paths
    cache = dc.FanoutCache(source, shards=2)
    cache.set('alive', 1)
    cache.set('dead', 2, expire=0.01)
    time.sleep(0.05)
    cache.close()

    begin_maintenance(source)
    try:
        with FanoutCacheMigration(source, destination, shards=4) as mig:
            stats = mig.run()
    finally:
        end_maintenance(source)

    assert stats['rows'] == 1
    assert stats['expired'] == 1
    with dc.FanoutCache(destination, shards=4) as migrated:
        assert migrated.get('alive') == 1
        assert 'dead' not in migrated


def test_resume_after_interrupt(paths, monkeypatch):
    source, destination = paths
    settings = {'disk_min_file_size': 16}
    cache = dc.FanoutCache(source, shards=2, **settings)
    for num in range(40):
        cache.set('file-%02d' % num, b'x' * 100 * (num + 1))
    cache.close()

    begin_maintenance(source)
    try:
        migration = FanoutCacheMigration(source, destination, shards=8)
        calls = []
        original = FanoutCacheMigration._copy_file

        def flaky(self, *args):
            if len(calls) >= 5:
                raise RuntimeError('simulated crash')
            calls.append(args)
            return original(self, *args)

        monkeypatch.setattr(FanoutCacheMigration, '_copy_file', flaky)
        with pytest.raises(RuntimeError):
            migration.copy(batch_size=5)
        migration.close()
        monkeypatch.undo()

        # The destination is not usable while incomplete.
        assert not migration_complete(destination)
        assert op.exists(op.join(destination, INCOMPLETE_FILENAME))

        # Resume from the manifest checkpoint.
        resumed = FanoutCacheMigration(source, destination, shards=8)
        stats = resumed.run()
        resumed.close()
    finally:
        end_maintenance(source)

    assert stats['rows'] == 40
    assert migration_complete(destination)

    with dc.FanoutCache(destination, shards=8) as migrated:
        for num in range(40):
            assert migrated.get('file-%02d' % num) == b'x' * 100 * (num + 1)


def test_source_change_refused_on_resume(paths, monkeypatch):
    source, destination = paths
    cache = make_source(source, shards=2, count=20)
    cache.close()

    begin_maintenance(source)
    try:
        migration = FanoutCacheMigration(source, destination, shards=4)
        monkeypatch.setattr(
            FanoutCacheMigration,
            '_copy_rows',
            lambda *args: (_ for _ in ()).throw(RuntimeError('crash')),
        )
        with pytest.raises(RuntimeError):
            migration.copy()
        migration.close()
        monkeypatch.undo()

        # Concurrent writes are unsupported: any source change after the
        # scan invalidates the checkpoint.
        cache = dc.FanoutCache(source, shards=2)
        cache.set('intruder', 'value')
        cache.close()

        with pytest.raises(MigrationError, match='refusing to reuse'):
            FanoutCacheMigration(source, destination, shards=4)
    finally:
        end_maintenance(source)


def test_layout_change_refused_on_resume(paths, monkeypatch):
    source, destination = paths
    cache = make_source(source, shards=2, count=10)
    cache.close()

    begin_maintenance(source)
    try:
        migration = FanoutCacheMigration(source, destination, shards=4)
        migration.close()

        with pytest.raises(MigrationError, match='shard count'):
            FanoutCacheMigration(source, destination, shards=8)
    finally:
        end_maintenance(source)


def test_incomplete_until_verified(paths):
    source, destination = paths
    cache = make_source(source, shards=2, count=10)
    cache.close()

    begin_maintenance(source)
    try:
        migration = FanoutCacheMigration(source, destination, shards=4)
        migration.copy()
        assert migration.state == 'verifying'
        assert not migration_complete(destination)
        assert op.exists(op.join(destination, INCOMPLETE_FILENAME))
        migration.verify()
        assert migration.state == 'complete'
        assert migration_complete(destination)
        migration.close()
    finally:
        end_maintenance(source)


def test_verify_detects_tampering(paths):
    source, destination = paths
    cache = make_source(source, shards=2, count=10)
    cache.close()

    begin_maintenance(source)
    try:
        migration = FanoutCacheMigration(source, destination, shards=4)
        migration.copy()

        # Tamper with the destination before verification.
        shard = migration._dest._shards[0]
        shard.set('injected', 'value')

        with pytest.raises(MigrationError, match='row count mismatch'):
            migration.verify()
        migration.close()
    finally:
        end_maintenance(source)


def test_settings_propagated(paths):
    source, destination = paths
    cache = dc.FanoutCache(
        source, shards=2, tag_index=True, statistics=True
    )
    cache.set('a', 1, tag='group')
    cache.close()

    begin_maintenance(source)
    try:
        with FanoutCacheMigration(source, destination, shards=4) as mig:
            mig.run()
    finally:
        end_maintenance(source)

    with dc.FanoutCache(destination, shards=4) as migrated:
        assert migrated.tag_index == 1
        assert migrated.evict('group') == 1


def test_manifest_records_layout(paths):
    source, destination = paths
    cache = make_source(source, shards=2, count=5)
    cache.close()

    begin_maintenance(source)
    try:
        migration = FanoutCacheMigration(source, destination, shards=4)
        migration.close()
        with open(op.join(destination, MANIFEST_FILENAME)) as reader:
            manifest = json.load(reader)
    finally:
        end_maintenance(source)

    assert manifest['source']['shards'] == 2
    assert manifest['destination']['shards'] == 4
    assert manifest['state'] == 'copying'
    assert set(manifest['progress']) == {'000', '001'}


def test_cli(paths, capsys):
    source, destination = paths
    cache = make_source(source, shards=2, count=10)
    cache.close()

    with pytest.raises(MigrationError):
        migrate_mod.main(['migrate', source, destination, '--shards', '4'])

    assert migrate_mod.main(['begin-maintenance', source]) == 0
    assert in_maintenance(source)

    assert (
        migrate_mod.main(
            ['migrate', source, destination, '--shards', '4']
        )
        == 0
    )
    assert migration_complete(destination)

    assert migrate_mod.main(['status', destination]) == 0
    out, _ = capsys.readouterr()
    assert 'state: complete' in out

    assert migrate_mod.main(['end-maintenance', source]) == 0
    assert not in_maintenance(source)