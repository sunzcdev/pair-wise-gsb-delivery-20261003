import os
import pickle
import sqlite3
from datetime import timedelta
from os.path import join
from tempfile import NamedTemporaryFile, gettempdir
from threading import Thread
from unittest.mock import MagicMock, patch

import pytest
from platformdirs import user_cache_dir

from requests_cache.backends import BaseCache, SQLiteCache, SQLiteDict
from requests_cache.backends.sqlite import MEMORY_URI
from requests_cache.models import CachedRequest, CachedResponse
from requests_cache.policy import utcnow
from requests_cache.session import CachedSession
from tests.conftest import MOCKED_URL_JSON, mount_mock_adapter, skip_pypy
from tests.integration.base_cache_test import BaseCacheTest
from tests.integration.base_storage_test import CACHE_NAME, BaseStorageTest


class TestSQLiteDict(BaseStorageTest):
    storage_class = SQLiteDict
    init_kwargs = {'use_temp': True}

    @classmethod
    def teardown_class(cls):
        try:
            os.unlink(f'{CACHE_NAME}.sqlite')
        except Exception:
            pass

    @patch('requests_cache.backends.sqlite.sqlite3')
    def test_connection_kwargs(self, mock_sqlite):
        """A spot check to make sure optional connection kwargs gets passed to connection;
        except for isolation_level, which is ignored
        """
        cache = self.storage_class(
            'test', use_temp=True, timeout=0.5, isolation_level='DEFERRED', invalid_kwarg='???'
        )
        mock_sqlite.connect.assert_called_with(
            cache.db_path, timeout=0.5, isolation_level=None, check_same_thread=False
        )

    def test_use_cache_dir(self):
        relative_path = self.storage_class(CACHE_NAME).db_path
        cache_dir_path = self.storage_class(CACHE_NAME, use_cache_dir=True).db_path
        assert not str(relative_path).startswith(user_cache_dir())
        assert str(cache_dir_path).startswith(user_cache_dir())

    def test_use_temp(self):
        relative_path = self.storage_class(CACHE_NAME).db_path
        temp_path = self.storage_class(CACHE_NAME, use_temp=True).db_path
        assert not str(relative_path).startswith(gettempdir())
        assert str(temp_path).startswith(gettempdir())

    def test_use_memory(self):
        cache = self.init_cache(use_memory=True)
        assert cache.db_path == MEMORY_URI
        for i in range(20):
            cache[f'key_{i}'] = f'value_{i}'
        for i in range(5):
            del cache[f'key_{i}']

        assert len(cache) == 15
        assert set(cache.keys()) == {f'key_{i}' for i in range(5, 20)}
        assert set(cache.values()) == {f'value_{i}' for i in range(5, 20)}

        cache.clear()
        assert len(cache) == 0

    def test_use_memory__uri(self):
        assert self.init_cache(':memory:').db_path == ':memory:'

    def test_non_dir_parent_exists(self):
        """Expect a custom error message if a parent path already exists but isn't a directory"""
        with NamedTemporaryFile() as tmp:
            with pytest.raises(FileExistsError) as exc_info:
                self.storage_class(join(tmp.name, 'invalid_path'))
                assert 'not a directory' in str(exc_info.value)

    def test_bulk_commit(self):
        cache = self.init_cache()
        with cache.bulk_commit():
            pass

        n_items = 1000
        with cache.bulk_commit():
            for i in range(n_items):
                cache[f'key_{i}'] = f'value_{i}'
        assert set(cache.keys()) == {f'key_{i}' for i in range(n_items)}
        assert set(cache.values()) == {f'value_{i}' for i in range(n_items)}

    def test_bulk_delete__chunked(self):
        """When deleting more items than SQLite can handle in a single statement, it should be
        chunked into multiple smaller statements
        """
        # Populate the cache with more items than can fit in a single delete statement
        cache = self.init_cache()
        with cache.bulk_commit():
            for i in range(2000):
                cache[f'key_{i}'] = f'value_{i}'

        keys = list(cache.keys())

        # First pass to ensure that bulk_delete is split across three statements
        with patch.object(cache, 'connection') as mock_connection:
            con = mock_connection().__enter__.return_value
            cache.bulk_delete(keys)
            assert con.execute.call_count == 3

        # Second pass to actually delete keys and make sure it doesn't explode
        cache.bulk_delete(keys)
        assert len(cache) == 0

    def test_bulk_commit__noop(self):
        def do_noop_bulk(cache):
            with cache.bulk_commit():
                pass
            del cache

        cache = self.init_cache()
        thread = Thread(target=do_noop_bulk, args=(cache,))
        thread.start()
        thread.join()

        # make sure connection is not closed by the thread
        cache['key_1'] = 'value_1'
        assert list(cache.keys()) == ['key_1']

    @skip_pypy
    @pytest.mark.parametrize('kwargs', [{'busy_timeout': 5}, {'fast_save': True}, {'wal': True}])
    def test_pragma(self, kwargs):
        """Test settings that make additional PRAGMA statements"""
        cache_1 = self.init_cache('cache_1', **kwargs)
        cache_2 = self.init_cache('cache_2', **kwargs)

        n = 500
        for i in range(n):
            cache_1[f'key_{i}'] = f'value_{i}'
            cache_2[f'key_{i * 2}'] = f'value_{i}'

        assert set(cache_1.keys()) == {f'key_{i}' for i in range(n)}
        assert set(cache_2.values()) == {f'value_{i}' for i in range(n)}

    def test_busy_timeout(self):
        cache = self.init_cache(busy_timeout=5)
        with cache.connection() as con:
            r = con.execute('PRAGMA busy_timeout').fetchone()
            assert r[0] == 5

    def test_wal_sync_mode(self):
        # Should default to 'NORMAL' (1)
        cache = self.init_cache(wal=True)
        with cache.connection() as con:
            r = con.execute('PRAGMA synchronous').fetchone()
            assert r[0] == 1

        # Not recommended, but should still work
        cache = self.init_cache(wal=True, fast_save=True)
        with cache.connection() as con:
            r = con.execute('PRAGMA synchronous').fetchone()
            assert r[0] == 0

    def test_write_acquire_lock(self):
        """Writes to the database acquire the sqlite lock"""
        cache = self.init_cache()
        with patch.object(cache, '_acquire_sqlite_lock') as mock_write:
            cache['key_1'] = 'value_1'
            assert mock_write.call_count == 1

    def test_write_retry_acquire_lock(self):
        """Acquiring the lock should retry BEGIN IMMEDIATE until it succeeds"""
        cache = self.init_cache()
        with patch.object(cache, '_connection') as mock_connection:
            mock_connection.execute.side_effect = [sqlite3.OperationalError] * 10 + [None]
            with cache._acquire_sqlite_lock():
                pass
            begin_calls = [
                c for c in mock_connection.execute.call_args_list if 'BEGIN IMMEDIATE' in str(c)
            ]
            assert len(begin_calls) == 11

    def test_write__error(self):
        """Errors from write operations should propagate"""
        cache = self.init_cache()
        with patch.object(cache, '_write', side_effect=sqlite3.OperationalError('DB is on fire')):
            with pytest.raises(sqlite3.OperationalError):
                cache['key_1'] = 'value_1'
        with patch.object(cache, '_write', side_effect=sqlite3.DatabaseError('HDD also on fire')):
            with pytest.raises(sqlite3.DatabaseError):
                cache['key_1'] = 'value_1'

    def test_vacuum__no_connection(self):
        cache = self.init_cache()
        cache._connection = None
        cache.vacuum()

    def test_vacuum__commits_active_transaction(self):
        cache = self.init_cache()
        mock_con = MagicMock()
        cache._connection = mock_con
        cache._active_transaction = True

        cache.vacuum()

        mock_con.commit.assert_called_once()
        mock_con.execute.assert_called_once_with('VACUUM')
        assert cache._active_transaction is False

    @skip_pypy
    @pytest.mark.parametrize('limit', [None, 50])
    def test_sorted__by_size(self, limit):
        cache = self.init_cache()

        # Insert items with decreasing size
        for i in range(100):
            suffix = 'padding' * (100 - i)
            cache[f'key_{i}'] = f'value_{i}_{suffix}'

        # Sorted items should be in ascending order by size
        items = list(cache.sorted(key='size', limit=limit))
        assert len(items) == (limit or 100)

        prev_item = None
        for item in items:
            assert prev_item is None or len(prev_item) > len(item)

    @skip_pypy
    def test_sorted__reversed(self):
        cache = self.init_cache()

        for i in range(100):
            cache[f'key_{i + 1:03}'] = f'value_{i + 1}'

        items = list(cache.sorted(key='key', reversed=True))
        assert len(items) == 100
        for i, item in enumerate(items):
            assert item == f'value_{100 - i}'

    @skip_pypy
    def test_sorted__invalid_sort_key(self):
        cache = self.init_cache()
        cache['key_1'] = 'value_1'
        with pytest.raises(ValueError):
            list(cache.sorted(key='invalid_key'))

    @skip_pypy
    @pytest.mark.parametrize('limit', [None, 50])
    def test_sorted__by_expires(self, limit):
        cache = self.init_cache()
        now = utcnow()

        # Insert items with decreasing expiration time
        for i in range(100):
            response = CachedResponse(expires=now + timedelta(seconds=101 - i))
            cache[f'key_{i}'] = response

        # Sorted items should be in ascending order by expiration time
        items = list(cache.sorted(key='expires', limit=limit))
        assert len(items) == (limit or 100)

        prev_item = None
        for item in items:
            assert prev_item is None or prev_item.expires < item.expires

    @skip_pypy
    def test_sorted__exclude_expired(self):
        cache = self.init_cache()
        now = utcnow()

        # Make only odd numbered items expired
        for i in range(100):
            delta = 101 - i
            if i % 2 == 1:
                delta -= 101

            response = CachedResponse(status_code=i, expires=now + timedelta(seconds=delta))
            cache[f'key_{i}'] = response

        # Items should only include unexpired (even numbered) items, and still be in sorted order
        items = list(cache.sorted(key='expires', expired=False))
        assert len(items) == 50
        prev_item = None

        for item in items:
            assert prev_item is None or prev_item.expires < item.expires
            assert item.status_code % 2 == 0

    @skip_pypy
    def test_sorted__error(self):
        """sorted() should handle deserialization errors and not return invalid responses"""

        class BadSerializer:
            def loads(self, value):
                response = pickle.loads(value)
                if response.cache_key == 'key_42':
                    raise pickle.PickleError()
                return response

            def dumps(self, value):
                return pickle.dumps(value)

        cache = self.init_cache(serializer=BadSerializer())

        for i in range(100):
            response = CachedResponse(status_code=i)
            response.cache_key = f'key_{i}'
            cache[f'key_{i}'] = response

        # Items should only include unexpired (even numbered) items, and still be in sorted order
        items = list(cache.sorted())
        assert len(items) == 99

    @pytest.mark.parametrize(
        'db_path, use_temp',
        [
            ('filesize_test', True),
            (':memory:', False),
        ],
    )
    def test_size(self, db_path, use_temp):
        """Test approximate expected size of a database, for both file-based and in-memory databases"""
        cache = self.init_cache(db_path, use_temp=use_temp)
        for i in range(100):
            cache[f'key_{i}'] = f'value_{i}'
        assert 10000 < cache.size() < 200000


class TestSQLiteCache(BaseCacheTest):
    backend_class = SQLiteCache
    init_kwargs = {'use_temp': True}

    @classmethod
    def teardown_class(cls):
        try:
            os.unlink(CACHE_NAME)
        except Exception:
            pass

    @patch.object(BaseCache, 'clear', side_effect=IOError)
    @patch('requests_cache.backends.sqlite.unlink', side_effect=os.unlink)
    def test_clear__failure(self, mock_unlink, mock_clear):
        """When a corrupted cache prevents a normal DROP TABLE, clear() should still succeed"""
        session = self.init_session(clear=False)
        session.cache.responses['key_1'] = 'value_1'
        db_path = session.cache.responses.db_path
        session.cache.clear()

        assert len(session.cache.responses) == 0
        mock_unlink.assert_called_once_with(db_path)

    @patch.object(BaseCache, 'clear', side_effect=IOError)
    def test_clear__file_already_deleted(self, mock_clear):
        session = self.init_session(clear=False)
        session.cache.responses['key_1'] = 'value_1'
        os.unlink(session.cache.responses.db_path)
        session.cache.clear()

        assert len(session.cache.responses) == 0

    def test_db_path(self):
        """This is just provided as an alias, since both requests and redirects share the same db
        file
        """
        session = self.init_session()
        assert session.cache.db_path == session.cache.responses.db_path

    def test_count(self):
        """count() should work the same as len(), but with the option to exclude expired responses"""
        session = self.init_session()
        now = utcnow()
        session.cache.responses['key_1'] = CachedResponse(expires=now + timedelta(1))
        session.cache.responses['key_2'] = CachedResponse(expires=now - timedelta(1))

        assert session.cache.count() == 2
        assert session.cache.count(expired=False) == 1

    def test_delete__single_key(self):
        """Vacuum should not be used after delete if there is only a single key"""
        session = self.init_session()
        session.cache.responses['key_1'] = 'value_1'

        with patch.object(SQLiteDict, 'vacuum') as mock_vacuum:
            session.cache.delete('key_1')
            mock_vacuum.assert_not_called()

    def test_delete__skip_vacuum(self):
        """Vacuum should not be used after delete if disabled"""
        session = self.init_session()
        session.cache.responses['key_1'] = 'value_1'
        session.cache.responses['key_2'] = 'value_2'

        with patch.object(SQLiteDict, 'vacuum') as mock_vacuum:
            session.cache.delete('key_1', 'key_2', vacuum=False)
            mock_vacuum.assert_not_called()

    def test_vacuum__frees_disk_space(self):
        """vacuum() should reclaim disk space after bulk deletion (regression test for #1156)"""
        session = self.init_session()
        db_path = session.cache.responses.db_path

        for i in range(200):
            cr = CachedResponse(status_code=200, url=f'https://example.com/{i}')
            cr.request = CachedRequest(method='GET', url=f'https://example.com/{i}')
            cr._content = b'x' * 50_000
            session.cache.save_response(cr, cache_key=f'k{i}')

        size_before = os.path.getsize(db_path)
        session.cache.delete(older_than=timedelta(seconds=-1))
        assert session.cache.responses.count() == 0

        session.cache.responses.vacuum()
        size_after = os.path.getsize(db_path)

        assert size_after < size_before / 10, (
            f'vacuum() did not reclaim disk space: {size_before} -> {size_after} bytes'
        )

    @patch.object(SQLiteDict, 'sorted')
    def test_filter__expired(self, mock_sorted):
        """Filtering by expired should use a more efficient SQL query"""
        session = self.init_session()

        session.cache.filter()
        mock_sorted.assert_called_with(expired=True)

        session.cache.filter(expired=False)
        mock_sorted.assert_called_with(expired=False)

    def test_sorted(self):
        """Test wrapper method for SQLiteDict.sorted(), with all arguments combined"""
        session = self.init_session(clear=False)
        now = utcnow()

        # Insert items with decreasing expiration time
        for i in range(500):
            delta = 1000 - i
            if i > 400:
                delta -= 2000

            response = CachedResponse(status_code=i, expires=now + timedelta(seconds=delta))
            session.cache.responses[f'key_{i}'] = response

        # Sorted items should be in ascending order by expiration time
        items = list(session.cache.sorted(key='expires', expired=False, reversed=True, limit=100))
        assert len(items) == 100

        prev_item = None
        for item in items:
            assert prev_item is None or prev_item.expires < item.expires
            assert item.cache_key
            assert not item.is_expired


class TestSQLiteCorruptEntries:
    """Tests for isolating single cache entries that can't be deserialized (see CorruptionPolicy)"""

    def _init_corrupt_cache(
        self,
        tempfile_path,
        corruption_policy='report',
        read_only=False,
        on_corruption=None,
    ):
        """Create a cache with valid responses on both sides of one corrupt entry"""
        session = CachedSession(
            cache_name=tempfile_path,
            backend='sqlite',
            corruption_policy=corruption_policy,
            read_only=read_only,
            on_corruption=on_corruption,
        )
        for key in ('key_a', 'key_c', 'key_e'):
            session.cache.responses[key] = CachedResponse(
                status_code=200, url=f'https://example.com/{key}'
            )

        # Write a value that can't be unpickled directly, so we don't go through serialize()
        with session.cache.responses.connection(commit=True) as con:
            con.execute(
                'INSERT OR REPLACE INTO responses (key, value, expires) VALUES (?, ?, ?)',
                ('key_bad', sqlite3.Binary(b'not a valid pickled response'), None),
            )

        # Direct and multi-hop aliases pointing at the corrupt entry, plus an unrelated alias
        session.cache.redirects['alias_bad'] = 'key_bad'
        session.cache.redirects['alias_alias_bad'] = 'alias_bad'
        session.cache.redirects['alias_c'] = 'key_c'
        return session

    def test_corrupt_entry__report_continues_batch(self, tempfile_path, caplog):
        """A corrupt entry between two valid ones should not stop the rest of the batch"""
        session = self._init_corrupt_cache(tempfile_path)

        items = list(session.cache.sorted(key='key'))

        # Both neighbors are readable, and the corrupt entry is treated as a miss
        assert [item.cache_key for item in items] == ['key_a', 'key_c', 'key_e']
        assert session.cache.get_response('key_bad') is None
        assert session.cache.get_response('key_a').url == 'https://example.com/key_a'

        # Report-only leaves the entry and its aliases in place
        assert 'key_bad' in session.cache.responses
        assert session.cache.redirects['alias_bad'] == 'key_bad'

        # Diagnostics identify the key, stage, and exception category
        assert 'key_bad' in caplog.text
        assert 'stage=deserialize' in caplog.text
        assert 'UnpicklingError' in caplog.text

    def test_corrupt_entry__delete_removes_entry_and_aliases(self, tempfile_path, caplog):
        session = self._init_corrupt_cache(tempfile_path, corruption_policy='delete')

        items = list(session.cache.sorted(key='key'))

        # Subsequent reads still work, and the corrupt entry is gone
        assert [item.cache_key for item in items] == ['key_a', 'key_c', 'key_e']
        assert 'key_bad' not in session.cache.responses
        assert session.cache.get_response('key_c').url == 'https://example.com/key_c'

        # Aliases that resolve to the corrupt entry are isolated in the same operation
        assert 'alias_bad' not in session.cache.redirects
        assert 'alias_alias_bad' not in session.cache.redirects
        # An unrelated alias is untouched
        assert session.cache.redirects['alias_c'] == 'key_c'

        assert 'key_bad' in caplog.text
        assert 'stage=deserialize' in caplog.text

    def test_corrupt_entry__delete_then_refetch(self, tempfile_path):
        """Deleting a corrupt entry should allow it to be fetched and re-cached"""
        session = mount_mock_adapter(
            CachedSession(cache_name=tempfile_path, backend='sqlite', corruption_policy='delete')
        )
        url = MOCKED_URL_JSON
        session.get(url)
        key = list(session.cache.responses.keys())[0]

        # Corrupt the cached value in place
        with session.cache.responses.connection(commit=True) as con:
            con.execute(
                'UPDATE responses SET value = ? WHERE key = ?',
                (sqlite3.Binary(b'garbage'), key),
            )

        assert session.get(url).from_cache is False
        assert session.get(url).from_cache is True
        assert session.cache.get_response(key) is not None

    def test_corrupt_entry__raise(self, tempfile_path):
        from requests_cache.policy import CorruptEntryError

        session = self._init_corrupt_cache(tempfile_path, corruption_policy='raise')

        with pytest.raises(CorruptEntryError) as exc_info:
            session.cache.get_response('key_bad')

        error = exc_info.value
        assert error.key == 'key_bad'
        assert error.stage == 'deserialize'
        assert error.exception_category == 'pickle'
        assert isinstance(error.__cause__, pickle.UnpicklingError)

        # A strict failure is isolated to the bad entry, which is left in place
        assert session.cache.get_response('key_a').url == 'https://example.com/key_a'
        assert 'key_bad' in session.cache.responses

    def test_corrupt_entry__read_only_never_deletes(self, tempfile_path):
        session = self._init_corrupt_cache(
            tempfile_path, corruption_policy='delete', read_only=True
        )

        assert session.cache.get_response('key_bad') is None
        assert 'key_bad' in session.cache.responses
        assert session.cache.redirects['alias_bad'] == 'key_bad'
        assert session.cache.get_response('key_a').url == 'https://example.com/key_a'

    def test_corrupt_entry__callback_failure_is_not_recursive(self, tempfile_path, caplog):
        events = []

        def on_corruption(event):
            events.append(event)
            raise RuntimeError('callback failed')

        session = self._init_corrupt_cache(
            tempfile_path, corruption_policy='delete', on_corruption=on_corruption
        )

        assert session.cache.get_response('key_bad') is None

        # The callback is called once; its error is reported but doesn't re-enter recovery
        assert len(events) == 1
        assert events[0].key == 'key_bad'
        assert 'stage=callback' in caplog.text
        assert 'key_bad' not in session.cache.responses
        assert session.cache.get_response('key_a').url == 'https://example.com/key_a'

    def test_corrupt_entry__callback_receives_event(self, tempfile_path):
        events = []
        session = self._init_corrupt_cache(tempfile_path, on_corruption=events.append)

        assert session.cache.get_response('key_bad') is None

        assert len(events) == 1
        assert events[0].key == 'key_bad'
        assert events[0].as_dict()['exception_type'] == 'UnpicklingError'

    def test_corrupt_entry__callback_reading_same_entry_does_not_recurse(self, tempfile_path):
        """If a callback reads the corrupt entry again, recovery doesn't re-enter itself"""
        calls = []

        def on_corruption(event):
            calls.append(event)
            # Reading the same entry from within the callback must not start another recovery
            assert session.cache.get_response(event.key) is None

        session = self._init_corrupt_cache(
            tempfile_path, corruption_policy='delete', on_corruption=on_corruption
        )
        assert session.cache.get_response('key_bad') is None

        # Only the outer event is reported, and the entry is deleted once
        assert len(calls) == 1
        assert 'key_bad' not in session.cache.responses

    def test_corrupt_entry__connection_error_is_not_isolation(self, tempfile_path):
        """A database connection error isn't a single corrupt entry, and must not clear the cache"""
        session = self._init_corrupt_cache(tempfile_path, corruption_policy='delete')

        with patch.object(
            session.cache.responses,
            'connection',
            side_effect=sqlite3.OperationalError('unable to open database file'),
        ):
            with pytest.raises(sqlite3.OperationalError):
                session.cache.get_response('key_a')

        # Nothing was treated as corrupt, and nothing was deleted
        assert len(session.cache.responses) == 4
        assert 'key_bad' in session.cache.responses
        assert session.cache.redirects['alias_bad'] == 'key_bad'

    def test_corrupt_entry__delete_failure_does_not_clear_cache(self, tempfile_path, caplog):
        """If isolation itself fails, the failure is logged and the cache is left intact"""
        session = self._init_corrupt_cache(tempfile_path, corruption_policy='delete')

        with patch.object(
            session.cache,
            '_delete_corrupt_entry',
            side_effect=sqlite3.OperationalError('disk is on fire'),
        ):
            assert session.cache.get_response('key_bad') is None

        assert 'stage=delete' in caplog.text
        assert 'disk is on fire' in caplog.text
        # The rest of the cache is untouched
        assert len(session.cache.responses) == 4
        assert session.cache.get_response('key_a').url == 'https://example.com/key_a'

    def test_corrupt_entry__strict_policy_does_not_block_later_reads(self, tempfile_path):
        """Even with the strict policy, a handled error doesn't prevent later reads"""
        from requests_cache.policy import CorruptEntryError

        session = self._init_corrupt_cache(tempfile_path, corruption_policy='raise')

        with pytest.raises(CorruptEntryError):
            session.cache.get_response('key_bad')

        for key in ('key_c', 'key_e'):
            assert session.cache.get_response(key).url == f'https://example.com/{key}'

    @pytest.mark.parametrize('policy', ['report', 'delete', 'raise'])
    def test_corrupt_entry__standalone_storage(self, tempfile_path, policy):
        """A storage instance used on its own applies its own policy"""
        from requests_cache.policy import CorruptEntryError

        cache = SQLiteDict(tempfile_path, 'responses', corruption_policy=policy)
        cache['good'] = CachedResponse(status_code=200, url='https://example.com/good')
        with cache.connection(commit=True) as con:
            con.execute(
                'INSERT OR REPLACE INTO responses (key, value, expires) VALUES (?, ?, ?)',
                ('bad', sqlite3.Binary(b'garbage'), None),
            )

        if policy == 'raise':
            with pytest.raises(CorruptEntryError):
                cache['bad']
        else:
            assert cache['bad'] is None

        # Deletion depends on the policy, and the good entry is always readable
        assert ('bad' in cache) is (policy != 'delete')
        assert cache['good'].url == 'https://example.com/good'

    def test_corrupt_entry__membership_check_does_not_deserialize(self, tempfile_path):
        """A membership check is read-only, and must not delete a corrupt entry"""
        session = self._init_corrupt_cache(tempfile_path, corruption_policy='delete')

        assert 'key_bad' in session.cache.responses
        assert 'alias_bad' in session.cache.redirects
        assert 'key_bad' in session.cache.responses

    def test_corrupt_entry__sorted_keeps_error_status_responses(self, tempfile_path):
        """Responses with error status codes are falsy but valid, so they must still be returned"""
        session = CachedSession(cache_name=tempfile_path, backend='sqlite')
        session.cache.responses['error'] = CachedResponse(status_code=500, url='https://e.com/err')
        session.cache.responses['ok'] = CachedResponse(status_code=200, url='https://e.com/ok')

        assert [r.cache_key for r in session.cache.sorted(key='key')] == ['error', 'ok']
