"""Offline migration tool for `FanoutCache` shard layouts.

`FanoutCache` maps keys to shards with ``hash(key) % shards`` so opening an
existing directory with a different shard count misplaces every key. This
module copies a `FanoutCache` directory laid out with one shard count into
a new, empty directory laid out with another, and then verifies keys,
values, tags, and remaining time-to-live (TTL).

The migration is strictly *offline*:

* The source directory must be placed in explicit maintenance mode (see
  :func:`begin_maintenance`) before migrating, and all cache processes
  must be stopped. Concurrent writes during migration are **not**
  supported. The maintenance marker is advisory; it does not lock anyone
  out by itself.
* The destination directory is marked with a ``MIGRATION-IN-PROGRESS``
  file until verification passes and must not be used as a normal cache
  before then (see :func:`migration_complete`).
* The source directory is never deleted, renamed, or replaced. After
  verification passes, switching traffic to the new directory is entirely
  the caller's decision.

Large file-backed values are streamed between directories in chunks rather
than loaded into memory. If the migration is interrupted, a manifest in
the destination directory records the checkpoint and the migration may be
resumed by constructing a new `FanoutCacheMigration` with the same
arguments. If the source values or layout changed since the scan, the old
checkpoint is refused and the destination must be discarded and rebuilt
from an empty directory.

Note that only the shard directories are migrated. Named sub-caches,
deques, and indexes created with :meth:`FanoutCache.cache`,
:meth:`FanoutCache.deque`, and :meth:`FanoutCache.index` live outside the
shards and are not copied.

Command line usage::

    python -m diskcache.migrate begin-maintenance /path/to/source
    python -m diskcache.migrate migrate /path/to/source /path/to/dest \
        --shards 16
    python -m diskcache.migrate status /path/to/dest
    python -m diskcache.migrate end-maintenance /path/to/source

"""

import argparse
import contextlib as cl
import functools as ft
import hashlib
import json
import os
import os.path as op
import re
import shutil
import sqlite3
import sys
import time

from .core import DBNAME, DEFAULT_SETTINGS, Disk
from .fanout import FanoutCache

__all__ = [
    'FanoutCacheMigration',
    'MigrationError',
    'begin_maintenance',
    'end_maintenance',
    'in_maintenance',
    'maintenance',
    'migration_complete',
]

MAINTENANCE_FILENAME = 'MAINTENANCE'
MANIFEST_FILENAME = 'migration-manifest.json'
INCOMPLETE_FILENAME = 'MIGRATION-IN-PROGRESS'
MANIFEST_VERSION = 1
CHUNK_SIZE = 2**22  # 4 MiB, matches diskcache.core.Disk read chunk.

SELECT_ROWS = (
    'SELECT rowid, key, raw, store_time, expire_time, access_time,'
    ' access_count, tag, size, mode, filename, value FROM Cache'
    ' WHERE (expire_time IS NULL OR expire_time > ?) AND rowid > ?'
    ' ORDER BY rowid'
)

INSERT_ROW = (
    'INSERT OR IGNORE INTO Cache('
    ' key, raw, store_time, expire_time, access_time,'
    ' access_count, tag, size, mode, filename, value'
    ') VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)'
)

VERIFY_ROW = (
    'SELECT expire_time, tag, size, mode, filename, value'
    ' FROM Cache WHERE key = ? AND raw = ?'
)

SHARD_DIR_PATTERN = re.compile(r'^\d{3}$')


class MigrationError(Exception):
    """Raised when a migration cannot proceed or verification fails."""


def _maintenance_path(directory):
    return op.join(directory, MAINTENANCE_FILENAME)


def begin_maintenance(directory):
    """Place `directory` in explicit maintenance mode.

    Creates a ``MAINTENANCE`` marker file in the cache directory. The
    marker is advisory only: callers must stop all cache readers and
    writers themselves before migrating. The migration tool refuses to
    run while the marker is absent.

    :param str directory: cache directory
    :raises MigrationError: if the directory is missing or already in
        maintenance mode

    """
    if not op.isdir(directory):
        raise MigrationError('directory not found: %s' % directory)
    path = _maintenance_path(directory)
    try:
        descriptor = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
    except FileExistsError:
        raise MigrationError(
            'directory already in maintenance mode: %s' % directory
        ) from None
    with os.fdopen(descriptor, 'w', encoding='utf-8') as writer:
        writer.write(
            'This cache directory is offline for maintenance.\n'
            'Stop all cache readers and writers before proceeding.\n'
            'created_at: %r\n' % (time.time(),)
        )


def end_maintenance(directory):
    """Remove the maintenance marker from `directory`.

    :param str directory: cache directory
    :raises MigrationError: if the directory is not in maintenance mode

    """
    path = _maintenance_path(directory)
    try:
        os.remove(path)
    except FileNotFoundError:
        raise MigrationError(
            'directory not in maintenance mode: %s' % directory
        ) from None


def in_maintenance(directory):
    """Return True if `directory` carries a maintenance marker."""
    return op.exists(_maintenance_path(directory))


@cl.contextmanager
def maintenance(directory):
    """Context manager wrapping `begin_maintenance`/`end_maintenance`."""
    begin_maintenance(directory)
    try:
        yield
    finally:
        end_maintenance(directory)


def _manifest_path(directory):
    return op.join(directory, MANIFEST_FILENAME)


def _incomplete_path(directory):
    return op.join(directory, INCOMPLETE_FILENAME)


def migration_complete(destination):
    """Return True if `destination` holds a fully verified migration.

    Until this returns True the destination directory must not be used as
    a normal cache.

    :param str destination: destination directory

    """
    path = _manifest_path(destination)
    if not op.isfile(path):
        return False
    with open(path, 'r', encoding='utf-8') as reader:
        manifest = json.load(reader)
    complete = manifest.get('state') == 'complete'
    return complete and not op.exists(_incomplete_path(destination))


def _connect_ro(db_path):
    """Open a read-only SQLite connection that leaves no files behind."""
    uri = 'file:%s?mode=ro' % db_path.replace(os.sep, '/')
    return sqlite3.connect(uri, uri=True)


def _detect_shard_count(directory):
    """Return shard count for a FanoutCache directory layout."""
    names = [
        name
        for name in os.listdir(directory)
        if SHARD_DIR_PATTERN.match(name)
        and op.isdir(op.join(directory, name))
    ]
    numbers = sorted(int(name) for name in names)
    if not numbers or numbers != list(range(len(numbers))):
        raise MigrationError(
            'shard directories are not consecutive from 000: %s' % directory
        )
    for name in names:
        db_path = op.join(directory, name, DBNAME)
        if not op.isfile(db_path):
            raise MigrationError('shard database not found: %s' % db_path)
    return len(numbers)


class FanoutCacheMigration:
    """Copy a FanoutCache directory into a new shard layout, offline.

    :param str source: existing FanoutCache directory (in maintenance
        mode)
    :param str destination: new, empty directory for the migrated cache
    :param int shards: shard count for the destination layout
    :param int source_shards: shard count of the source (default None,
        detected from the directory layout)
    :param disk: `Disk` subclass matching the source cache
    :param int chunk_size: buffer size for streaming file values

    """

    def __init__(
        self,
        source,
        destination,
        shards,
        source_shards=None,
        disk=Disk,
        chunk_size=CHUNK_SIZE,
    ):
        source = op.abspath(op.expanduser(str(source)))
        destination = op.abspath(op.expanduser(str(destination)))

        if shards < 1:
            raise ValueError('shards must be a positive integer')
        if not op.isdir(source):
            raise MigrationError('source directory not found: %s' % source)
        if not in_maintenance(source):
            raise MigrationError(
                'source directory must be in maintenance mode during'
                ' migration; stop all cache processes and run'
                ' begin_maintenance first: %s' % source
            )
        if op.normcase(source) == op.normcase(destination):
            raise MigrationError('source and destination must differ')

        if source_shards is None:
            source_shards = _detect_shard_count(source)
        else:
            for num in range(source_shards):
                db_path = op.join(source, '%03d' % num, DBNAME)
                if not op.isfile(db_path):
                    raise MigrationError(
                        'shard database not found: %s' % db_path
                    )

        self._source = source
        self._destination = destination
        self._shards = shards
        self._source_shards = source_shards
        self._disk = disk
        self._chunk_size = chunk_size
        self._manifest_path = _manifest_path(destination)

        if op.isfile(self._manifest_path):
            self._manifest = self._load_manifest()
            self._check_resume()
        else:
            if op.isdir(destination) and os.listdir(destination):
                raise MigrationError(
                    'destination directory must be empty: %s' % destination
                )
            os.makedirs(destination, exist_ok=True)
            self._manifest = self._scan()
            self._write_incomplete_marker()
            self._save_manifest()

        if self._manifest['state'] != 'complete':
            self._write_incomplete_marker()

        settings = dict(self._manifest['destination']['settings'])
        self._dest = FanoutCache(
            directory=destination,
            shards=shards,
            disk=disk,
            **settings,
        )

    ####################################################################
    # Manifest handling.
    ####################################################################

    def _load_manifest(self):
        with open(self._manifest_path, 'r', encoding='utf-8') as reader:
            manifest = json.load(reader)
        if manifest.get('version') != MANIFEST_VERSION:
            raise MigrationError(
                'unsupported manifest version: %r' % manifest.get('version')
            )
        return manifest

    def _save_manifest(self):
        # Write-then-rename so an interrupted migration never leaves a
        # truncated manifest behind.
        tmp_path = self._manifest_path + '.tmp'
        with open(tmp_path, 'w', encoding='utf-8') as writer:
            json.dump(self._manifest, writer, indent=2, sort_keys=True)
        os.replace(tmp_path, self._manifest_path)

    def _write_incomplete_marker(self):
        path = _incomplete_path(self._destination)
        with open(path, 'w', encoding='utf-8') as writer:
            writer.write(
                'Migration incomplete. Do not use this directory as a'
                ' cache until migration_complete() returns True.\n'
            )

    def _check_resume(self):
        """Validate a saved checkpoint against the current source."""
        manifest = self._manifest
        source = manifest['source']
        if source['directory'] != self._source:
            raise MigrationError(
                'manifest source %r does not match %r'
                % (source['directory'], self._source)
            )
        if source['shards'] != self._source_shards:
            raise MigrationError(
                'source layout changed: manifest records %d shards,'
                ' found %d; discarding the destination and restarting'
                ' is required' % (source['shards'], self._source_shards)
            )
        if manifest['destination']['shards'] != self._shards:
            raise MigrationError(
                'destination shard count changed: manifest records %d,'
                ' requested %d; use a new empty directory instead'
                % (manifest['destination']['shards'], self._shards)
            )
        current = self._fingerprint()
        if current != source['fingerprint']:
            raise MigrationError(
                'source values or layout changed since the migration'
                ' scan; refusing to reuse the old checkpoint. Discard'
                ' the destination directory and restart the migration'
                ' from an empty directory.'
            )

    ####################################################################
    # Source scanning and fingerprinting.
    ####################################################################

    def _shard_dir(self, num):
        return op.join(self._source, '%03d' % num)

    def _checkpoint_source(self):
        # Fold any WAL content into the main database so the fingerprint
        # is stable. Requires maintenance mode: no other process may hold
        # the database.
        for num in range(self._source_shards):
            db_path = op.join(self._shard_dir(num), DBNAME)
            con = sqlite3.connect(db_path)
            try:
                busy, _, _ = con.execute(
                    'PRAGMA wal_checkpoint(TRUNCATE)'
                ).fetchone()
                if busy:
                    raise MigrationError(
                        'source database is busy; close all connections'
                        ' before migrating: %s' % db_path
                    )
            finally:
                con.close()

    def _fingerprint(self):
        """Content and file fingerprint of every source shard."""
        result = {}
        for num in range(self._source_shards):
            shard_dir = self._shard_dir(num)
            files = {}
            for name in (DBNAME, DBNAME + '-wal'):
                path = op.join(shard_dir, name)
                if op.exists(path):
                    stat = os.stat(path)
                    files[name] = [stat.st_size, stat.st_mtime_ns]
            db_path = op.join(shard_dir, DBNAME)
            con = _connect_ro(db_path)
            try:
                row = con.execute(
                    'SELECT COUNT(*), COALESCE(MAX(rowid), 0),'
                    ' COALESCE(SUM(size), 0) FROM Cache'
                ).fetchone()
            finally:
                con.close()
            result['%03d' % num] = {
                'files': files,
                'rows': row[0],
                'max_rowid': row[1],
                'size_sum': row[2],
            }
        return result

    def _read_settings(self):
        """Read cache settings from the first source shard."""
        db_path = op.join(self._shard_dir(0), DBNAME)
        con = _connect_ro(db_path)
        try:
            rows = con.execute('SELECT key, value FROM Settings').fetchall()
        finally:
            con.close()
        settings = {
            key: value for key, value in rows if key in DEFAULT_SETTINGS
        }
        # FanoutCache divides size_limit by the shard count, so scale the
        # per-shard limit back up to the original total.
        size_limit = settings.get(
            'size_limit', DEFAULT_SETTINGS['size_limit']
        )
        settings['size_limit'] = size_limit * self._source_shards
        return settings

    def _scan(self):
        """Build a fresh manifest for a new migration."""
        self._checkpoint_source()
        now = time.time()
        expired = 0
        for num in range(self._source_shards):
            db_path = op.join(self._shard_dir(num), DBNAME)
            con = _connect_ro(db_path)
            try:
                (count,) = con.execute(
                    'SELECT COUNT(*) FROM Cache'
                    ' WHERE expire_time IS NOT NULL AND expire_time <= ?',
                    (now,),
                ).fetchone()
            finally:
                con.close()
            expired += count
        return {
            'version': MANIFEST_VERSION,
            'state': 'copying',
            'created_at': now,
            # Rows expired at scan time are skipped consistently across
            # resume attempts, keeping the checkpoint deterministic.
            'scanned_at': now,
            'source': {
                'directory': self._source,
                'shards': self._source_shards,
                'fingerprint': self._fingerprint(),
            },
            'destination': {
                'shards': self._shards,
                'settings': self._read_settings(),
            },
            'progress': {
                '%03d' % num: {'rowid': 0, 'done': False}
                for num in range(self._source_shards)
            },
            'stats': {'rows': 0, 'files': 0, 'bytes': 0, 'expired': expired},
        }

    ####################################################################
    # Copy phase.
    ####################################################################

    def copy(self, batch_size=1000):
        """Copy source rows into the destination layout.

        Safe to call again after an interruption; completed batches are
        skipped using the manifest checkpoint.

        :param int batch_size: rows copied per transaction
        :return: stats dict with rows, files, bytes, and expired counts

        """
        self._require_maintenance()
        manifest = self._manifest
        if manifest['state'] != 'copying':
            return dict(manifest['stats'])

        scanned_at = manifest['scanned_at']
        for num in range(self._source_shards):
            name = '%03d' % num
            progress = manifest['progress'][name]
            if progress['done']:
                continue
            shard_dir = self._shard_dir(num)
            db_path = op.join(shard_dir, DBNAME)
            con = _connect_ro(db_path)
            try:
                last_rowid = progress['rowid']
                while True:
                    rows = con.execute(
                        SELECT_ROWS, (scanned_at, last_rowid)
                    ).fetchmany(batch_size)
                    if not rows:
                        break
                    self._copy_rows(shard_dir, rows)
                    last_rowid = rows[-1][0]
                    progress['rowid'] = last_rowid
                    self._save_manifest()
            finally:
                con.close()
            progress['done'] = True
            self._save_manifest()

        manifest['state'] = 'verifying'
        self._save_manifest()
        return dict(manifest['stats'])

    def _copy_rows(self, shard_dir, rows):
        dest = self._dest
        hash_key = dest._hash  # pylint: disable=protected-access
        count = dest._count  # pylint: disable=protected-access
        disk = dest._shards[0].disk  # pylint: disable=protected-access

        def destination_index(row):
            key = disk.get(row[1], row[2])
            return hash_key(key) % count

        # Stream file values first so the database never references a
        # missing file.
        for row in rows:
            filename = row[10]
            if filename is not None:
                index = destination_index(row)
                shard = dest._shards[index]  # pylint: disable=protected-access
                self._copy_file(shard_dir, shard.directory, filename, row[8])

        by_shard = {}
        for row in rows:
            by_shard.setdefault(destination_index(row), []).append(row)

        for index, shard_rows in by_shard.items():
            shard = dest._shards[index]  # pylint: disable=protected-access
            con = shard._con  # pylint: disable=protected-access
            try:
                con.execute('BEGIN')
                for row in shard_rows:
                    con.execute(INSERT_ROW, row[1:])
                con.execute('COMMIT')
            except Exception:
                con.execute('ROLLBACK')
                raise

        stats = self._manifest['stats']
        stats['rows'] += len(rows)
        stats['files'] += sum(1 for row in rows if row[10] is not None)
        stats['bytes'] += sum(
            row[8] for row in rows if row[10] is not None
        )

    def _copy_file(self, source_dir, dest_dir, filename, size):
        """Stream one file value between shard directories."""
        source_path = op.join(source_dir, filename)
        dest_path = op.join(dest_dir, filename)
        if op.exists(dest_path):
            # Already copied by an earlier attempt that crashed before
            # the checkpoint was saved.
            if op.getsize(dest_path) == size:
                return
            raise MigrationError(
                'filename collision with different size: %s' % dest_path
            )
        os.makedirs(op.dirname(dest_path), exist_ok=True)
        tmp_path = dest_path + '.tmp'
        try:
            with open(source_path, 'rb') as reader:
                with open(tmp_path, 'wb') as writer:
                    shutil.copyfileobj(reader, writer, self._chunk_size)
            if op.getsize(tmp_path) != size:
                raise MigrationError(
                    'size mismatch copying file: %s' % source_path
                )
            os.replace(tmp_path, dest_path)
        except Exception:
            with cl.suppress(OSError):
                os.remove(tmp_path)
            raise

    ####################################################################
    # Verify phase.
    ####################################################################

    def verify(self):
        """Verify keys, values, tags, and TTLs against the source.

        :return: True when verification passes
        :raises MigrationError: if the source changed or any row differs

        """
        self._require_maintenance()
        manifest = self._manifest
        if manifest['state'] == 'complete':
            return True
        if manifest['state'] != 'verifying':
            raise MigrationError('copy phase has not completed')

        # The source must be bit-for-bit identical to the scanned state.
        if self._fingerprint() != manifest['source']['fingerprint']:
            raise MigrationError(
                'source values or layout changed since the migration'
                ' scan; discard the destination and restart'
            )

        scanned_at = manifest['scanned_at']
        dest = self._dest
        hash_key = dest._hash  # pylint: disable=protected-access
        count = dest._count  # pylint: disable=protected-access
        disk = dest._shards[0].disk  # pylint: disable=protected-access

        mismatches = []
        total = 0
        for num in range(self._source_shards):
            shard_dir = self._shard_dir(num)
            db_path = op.join(shard_dir, DBNAME)
            con = _connect_ro(db_path)
            try:
                cursor = con.execute(
                    'SELECT key, raw, expire_time, tag, size, mode,'
                    ' filename, value FROM Cache'
                    ' WHERE expire_time IS NULL OR expire_time > ?'
                    ' ORDER BY rowid',
                    (scanned_at,),
                )
                while True:
                    rows = cursor.fetchmany(1000)
                    if not rows:
                        break
                    for row in rows:
                        total += 1
                        problem = self._verify_row(shard_dir, disk,
                                                   hash_key, count, row)
                        if problem is not None:
                            mismatches.append(problem)
                            if len(mismatches) >= 10:
                                break
                    if len(mismatches) >= 10:
                        break
            finally:
                con.close()
            if len(mismatches) >= 10:
                break

        if not mismatches:
            dest_total = sum(
                shard._sql(  # pylint: disable=protected-access
                    'SELECT COUNT(*) FROM Cache'
                ).fetchone()[0]
                for shard in dest._shards  # pylint: disable=protected-access
            )
            if dest_total != total:
                mismatches.append(
                    'row count mismatch: source %d, destination %d'
                    % (total, dest_total)
                )

        if mismatches:
            raise MigrationError(
                'verification failed: %s' % '; '.join(mismatches)
            )

        manifest['state'] = 'complete'
        manifest['completed_at'] = time.time()
        self._save_manifest()
        with cl.suppress(OSError):
            os.remove(_incomplete_path(self._destination))
        return True

    def _verify_row(self, shard_dir, disk, hash_key, count, row):
        key, raw, expire_time, tag, size, mode, filename, value = row
        index = hash_key(disk.get(key, raw)) % count
        shard = self._dest._shards[index]  # pylint: disable=protected-access
        rows = shard._sql(VERIFY_ROW, (key, raw)).fetchall()  # pylint: disable=protected-access
        if not rows:
            return 'missing key in destination: %r' % (disk.get(key, raw),)
        (d_expire, d_tag, d_size, d_mode, d_filename, d_value), = rows
        label = repr(disk.get(key, raw))
        if d_expire != expire_time:
            return 'expire_time mismatch for %s' % label
        if d_tag != tag:
            return 'tag mismatch for %s' % label
        if d_size != size or d_mode != mode:
            return 'size/mode mismatch for %s' % label
        if filename is None:
            if d_filename is not None or d_value != value:
                return 'value mismatch for %s' % label
        else:
            if d_filename != filename:
                return 'filename mismatch for %s' % label
            source_hash = self._hash_file(op.join(shard_dir, filename))
            dest_hash = self._hash_file(
                op.join(shard.directory, d_filename)
            )
            if source_hash != dest_hash:
                return 'file value mismatch for %s' % label
        return None

    def _hash_file(self, path):
        digest = hashlib.sha256()
        with open(path, 'rb') as reader:
            for chunk in iter(ft.partial(reader.read, self._chunk_size), b''):
                digest.update(chunk)
        return digest.hexdigest()

    ####################################################################
    # Driver and accessors.
    ####################################################################

    def run(self, batch_size=1000):
        """Copy and then verify. Returns the migration stats dict."""
        self.copy(batch_size=batch_size)
        self.verify()
        return dict(self._manifest['stats'])

    @property
    def state(self):
        """Migration state: copying, verifying, or complete."""
        return self._manifest['state']

    @property
    def source(self):
        """Source directory."""
        return self._source

    @property
    def destination(self):
        """Destination directory."""
        return self._destination

    def _require_maintenance(self):
        if not in_maintenance(self._source):
            raise MigrationError(
                'source directory left maintenance mode; refusing to'
                ' continue: %s' % self._source
            )

    def close(self):
        """Close destination cache connections."""
        self._dest.close()

    def __enter__(self):
        return self

    def __exit__(self, *exception):
        self.close()


def main(argv=None):
    """Command line interface for offline FanoutCache migration."""
    parser = argparse.ArgumentParser(
        prog='diskcache.migrate',
        description=(
            'Offline migration of a FanoutCache directory to a new shard'
            ' count. The source must be placed in maintenance mode and'
            ' all cache processes stopped first; concurrent writes are'
            ' not supported.'
        ),
    )
    commands = parser.add_subparsers(dest='command', required=True)

    begin = commands.add_parser(
        'begin-maintenance', help='mark a cache directory as offline'
    )
    begin.add_argument('source')

    end = commands.add_parser(
        'end-maintenance', help='remove the offline marker'
    )
    end.add_argument('source')

    migrate = commands.add_parser(
        'migrate', help='copy and verify into a new shard layout'
    )
    migrate.add_argument('source')
    migrate.add_argument('destination')
    migrate.add_argument(
        '--shards', type=int, required=True,
        help='shard count for the destination layout',
    )
    migrate.add_argument(
        '--source-shards', type=int, default=None,
        help='shard count of the source (default: detected)',
    )

    status = commands.add_parser(
        'status', help='show migration or maintenance state'
    )
    status.add_argument('directory')

    args = parser.parse_args(argv)

    if args.command == 'begin-maintenance':
        begin_maintenance(args.source)
        print('maintenance mode enabled: %s' % args.source)
    elif args.command == 'end-maintenance':
        end_maintenance(args.source)
        print('maintenance mode disabled: %s' % args.source)
    elif args.command == 'migrate':
        with FanoutCacheMigration(
            args.source,
            args.destination,
            shards=args.shards,
            source_shards=args.source_shards,
        ) as migration:
            stats = migration.run()
        print('migration complete: %s' % args.destination)
        print('stats: %s' % stats)
    elif args.command == 'status':
        path = _manifest_path(args.directory)
        if op.isfile(path):
            with open(path, 'r', encoding='utf-8') as reader:
                manifest = json.load(reader)
            print('state: %s' % manifest['state'])
            print('complete: %s' % migration_complete(args.directory))
        else:
            print('maintenance: %s' % in_maintenance(args.directory))
    return 0


if __name__ == '__main__':
    sys.exit(main())