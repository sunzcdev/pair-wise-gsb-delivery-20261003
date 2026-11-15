"""
Dead-letter (rejected-message) storage.

When the consumer dequeues a message whose raw bytes cannot be deserialized or
that references a task which is no longer registered, the message would
historically be logged and silently discarded. When a dead-letter store is
configured, such messages are instead persisted verbatim together with the
context needed to understand and, later, explicitly replay them.

The dead-letter store is intentionally separate from the task result
key/value store: rejected messages have their own tables/collections, their
own retention policy and their own flush method, so they can never collide
with or be mistaken for normal task results.
"""
import contextlib
import sqlite3
import threading
import time
import uuid
from collections import namedtuple


# Failure stages recorded on a rejected message.
STAGE_DESERIALIZE = 'deserialize'
STAGE_REGISTRY = 'registry'

# Lifecycle status of a stored rejection.
STATUS_REJECTED = 'rejected'
STATUS_REPLAYED = 'replayed'


class DeadLetter(namedtuple('DeadLetter', ('id', 'queue', 'data', 'stage',
                                           'error', 'enqueued_at',
                                           'rejected_at', 'status',
                                           'replayed_at', 'origin_id'))):
    """
    A single rejected message.

    :param str id: unique identifier of the rejection record.
    :param str queue: name of the queue the message was dequeued from.
    :param bytes data: the original, unmodified message bytes.
    :param str stage: ``deserialize`` or ``registry``.
    :param str error: representation of the exception that caused rejection.
    :param float enqueued_at: wall-clock time the message was received by the
        worker (i.e. dequeued), as a unix timestamp.
    :param float rejected_at: wall-clock time the rejection was written.
    :param str status: ``rejected`` or ``replayed``.
    :param replayed_at: unix timestamp of the replay request, or ``None``.
    :param origin_id: when this record was itself produced by replaying an
        earlier rejection, the id of that originating rejection record.
    """
    __slots__ = ()

    def __new__(cls, **kwargs):
        kwargs.setdefault('status', STATUS_REJECTED)
        kwargs.setdefault('replayed_at', None)
        kwargs.setdefault('origin_id', None)
        return super(DeadLetter, cls).__new__(cls, **kwargs)


class BaseDeadLetterStorage(object):
    """
    Interface for dead-letter backends.

    :param int max_items: retain at most this many records; older records are
        pruned first. ``None`` disables size-based retention.
    :param int ttl: retain records for at most this many seconds; expired
        records are pruned lazily. ``None`` disables time-based retention.
    """
    def __init__(self, max_items=None, ttl=None):
        if max_items is not None and max_items <= 0:
            raise ValueError('max_items must be a positive integer or None')
        if ttl is not None and ttl <= 0:
            raise ValueError('ttl must be a positive number or None')
        self.max_items = max_items
        self.ttl = ttl

    def close(self):
        pass

    # -- retention ------------------------------------------------------- #

    def prune(self, now=None):
        """Remove records that violate the retention policy."""
        raise NotImplementedError

    # -- write / read ---------------------------------------------------- #

    def add(self, queue, data, stage, error, enqueued_at, now=None,
            origin_id=None):
        """
        Persist a rejected message, returning the created :class:`DeadLetter`.

        Implementations must raise if the record could not be written, so the
        caller never believes an unsaved message was safely stored.
        """
        raise NotImplementedError

    def get(self, id):
        """Return the :class:`DeadLetter` for ``id``, or ``None``."""
        raise NotImplementedError

    def list(self, include_replayed=True, limit=None):
        """
        Return rejection records, newest first.

        Replayed records are excluded when ``include_replayed`` is False.
        """
        raise NotImplementedError

    def count(self, include_replayed=True):
        """Return the number of retained records."""
        raise NotImplementedError

    def delete(self, id):
        """Delete a single record, returning whether it existed."""
        raise NotImplementedError

    def flush(self):
        """Delete every rejection record (and replay bookkeeping)."""
        raise NotImplementedError

    # -- replay ---------------------------------------------------------- #

    def mark_replayed(self, id, now=None):
        """
        Transition a record to the ``replayed`` state.

        :raises KeyError: if no live (non-replayed) record exists for ``id``.
        """
        raise NotImplementedError

    def pending_origin(self, data_hash):
        """
        Return the originating rejection id for a message awaiting replay, or
        ``None``. Used to preserve the association when a replay itself fails.
        """
        raise NotImplementedError

    def register_pending(self, data_hash, origin_id):
        """Remember that a replay of ``origin_id`` with this message hash is
        now in flight (sitting back on the main queue)."""
        raise NotImplementedError

    def clear_pending(self, data_hash):
        """Forget an in-flight replay association."""
        raise NotImplementedError


class MemoryDeadLetterStorage(BaseDeadLetterStorage):
    def __init__(self, **kwargs):
        super(MemoryDeadLetterStorage, self).__init__(**kwargs)
        self._items = {}
        self._pending = {}
        self._lock = threading.RLock()

    def prune(self, now=None):
        now = time.time() if now is None else now
        with self._lock:
            if self.ttl is not None:
                expired = [dl.id for dl in self._items.values()
                           if dl.rejected_at + self.ttl < now]
                for id in expired:
                    self._items.pop(id, None)
            if self.max_items is not None:
                live = [dl for dl in self._items.values()
                        if dl.status == STATUS_REJECTED]
                live.sort(key=lambda dl: dl.rejected_at)
                excess = len(live) - self.max_items
                for dl in live[:max(0, excess)]:
                    self._items.pop(dl.id, None)
            # Drop in-flight associations whose originating rejection was
            # itself pruned/deleted, or that have aged past the TTL. Entries
            # pointing at retained (even replayed) records are kept so a later
            # re-rejection can still be linked back to its origin.
            for data_hash, (origin, ts) in list(self._pending.items()):
                if origin not in self._items:
                    self._pending.pop(data_hash, None)
                elif self.ttl is not None and ts + self.ttl < now:
                    self._pending.pop(data_hash, None)

    def add(self, queue, data, stage, error, enqueued_at, now=None,
            origin_id=None):
        now = time.time() if now is None else now
        dl = DeadLetter(
            id=str(uuid.uuid4()),
            queue=queue,
            data=bytes(data),
            stage=stage,
            error=error,
            enqueued_at=enqueued_at,
            rejected_at=now,
            origin_id=origin_id)
        with self._lock:
            self._items[dl.id] = dl
            self.prune(now)
        return dl

    def get(self, id):
        with self._lock:
            return self._items.get(id)

    def list(self, include_replayed=True, limit=None):
        with self._lock:
            items = list(self._items.values())
        if not include_replayed:
            items = [dl for dl in items if dl.status == STATUS_REJECTED]
        items.sort(key=lambda dl: dl.rejected_at, reverse=True)
        if limit is not None:
            items = items[:limit]
        return items

    def count(self, include_replayed=True):
        with self._lock:
            if include_replayed:
                return len(self._items)
            return sum(1 for dl in self._items.values()
                       if dl.status == STATUS_REJECTED)

    def delete(self, id):
        with self._lock:
            existed = id in self._items
            self._items.pop(id, None)
            return existed

    def flush(self):
        with self._lock:
            self._items = {}
            self._pending = {}

    def mark_replayed(self, id, now=None):
        now = time.time() if now is None else now
        with self._lock:
            dl = self._items.get(id)
            if dl is None or dl.status != STATUS_REJECTED:
                raise KeyError(id)
            self._items[id] = dl._replace(status=STATUS_REPLAYED,
                                          replayed_at=now)

    def pending_origin(self, data_hash):
        with self._lock:
            entry = self._pending.get(data_hash)
            return entry[0] if entry is not None else None

    def register_pending(self, data_hash, origin_id):
        with self._lock:
            self._pending[data_hash] = (origin_id, time.time())

    def clear_pending(self, data_hash):
        with self._lock:
            self._pending.pop(data_hash, None)


class SqliteDeadLetterStorage(BaseDeadLetterStorage):
    """
    Dead-letter store backed by a local SQLite database. Kept fully separate
    from the queue/result tables so rejected messages have an independent
    retention policy and never appear in the result store.
    """
    table_letter = ('create table if not exists dead_letter ('
                    'id text primary key, queue text not null, '
                    'data blob not null, stage text not null, '
                    'error text not null, enqueued_at real not null, '
                    'rejected_at real not null, status text not null, '
                    'replayed_at real, origin_id text)')
    table_pending = ('create table if not exists dead_letter_pending ('
                     'data_hash text primary key, origin_id text not null, '
                     'created_at real not null)')
    ddl = [table_letter, table_pending]

    def __init__(self, filename='huey_dead_letter.db', timeout=5,
                 create_tables=True, **kwargs):
        super(SqliteDeadLetterStorage, self).__init__(**kwargs)
        self.filename = filename
        self.timeout = timeout
        self.lock = threading.Lock()
        self._conn = None
        if create_tables:
            self.initialize_schema()

    def close(self):
        if self._conn is None:
            return False
        with self.lock:
            self._conn.close()
            self._conn = None
        return True

    @property
    def conn(self):
        if self._conn is None:
            self._conn = sqlite3.connect(
                self.filename, timeout=self.timeout, check_same_thread=False)
            self._conn.isolation_level = None  # Autocommit.
        return self._conn

    @contextlib.contextmanager
    def db(self, commit=False):
        with self.lock:
            cursor = self.conn.cursor()
            try:
                if commit:
                    cursor.execute('begin exclusive')
                yield cursor
            except Exception:
                if commit:
                    self.conn.rollback()
                raise
            else:
                if commit:
                    self.conn.commit()
            finally:
                cursor.close()

    def initialize_schema(self):
        with self.db(commit=True) as curs:
            for sql in self.ddl:
                curs.execute(sql)

    def _to_dead_letter(self, row):
        return DeadLetter(
            id=row[0], queue=row[1], data=bytes(row[2]), stage=row[3],
            error=row[4], enqueued_at=row[5], rejected_at=row[6],
            status=row[7], replayed_at=row[8], origin_id=row[9])

    def _prune_cursor(self, curs, now):
        # Retention enforcement over an already-open cursor/transaction.
        if self.ttl is not None:
            cutoff = now - self.ttl
            curs.execute('delete from dead_letter where rejected_at < ?',
                         (cutoff,))
            curs.execute('delete from dead_letter_pending where created_at < ?',
                         (cutoff,))
        if self.max_items is not None:
            curs.execute('select id from dead_letter where status = ? '
                         'order by rejected_at asc', (STATUS_REJECTED,))
            ids = [row[0] for row in curs.fetchall()]
            excess = ids[:max(0, len(ids) - self.max_items)]
            if excess:
                curs.execute('delete from dead_letter where id in (%s)' %
                             ','.join('?' * len(excess)), excess)
        # Drop associations whose originating rejection no longer exists
        # (pruned or deleted), keeping this bookkeeping bounded.
        curs.execute('delete from dead_letter_pending where origin_id not in '
                     '(select id from dead_letter)')

    def prune(self, now=None):
        now = time.time() if now is None else now
        with self.db(commit=True) as curs:
            self._prune_cursor(curs, now)

    def add(self, queue, data, stage, error, enqueued_at, now=None,
            origin_id=None):
        now = time.time() if now is None else now
        dl = DeadLetter(
            id=str(uuid.uuid4()),
            queue=queue,
            data=bytes(data),
            stage=stage,
            error=error,
            enqueued_at=enqueued_at,
            rejected_at=now,
            origin_id=origin_id)
        with self.db(commit=True) as curs:
            curs.execute(
                'insert into dead_letter (id, queue, data, stage, error, '
                'enqueued_at, rejected_at, status, replayed_at, origin_id) '
                'values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
                (dl.id, dl.queue, memoryview(dl.data), dl.stage, dl.error,
                 dl.enqueued_at, dl.rejected_at, dl.status, dl.replayed_at,
                 dl.origin_id))
            self._prune_cursor(curs, now)
        return dl

    def get(self, id):
        with self.db() as curs:
            curs.execute('select id, queue, data, stage, error, enqueued_at, '
                         'rejected_at, status, replayed_at, origin_id '
                         'from dead_letter where id = ?', (id,))
            row = curs.fetchone()
        return self._to_dead_letter(row) if row is not None else None

    def list(self, include_replayed=True, limit=None):
        sql = ('select id, queue, data, stage, error, enqueued_at, '
               'rejected_at, status, replayed_at, origin_id from dead_letter')
        params = ()
        if not include_replayed:
            sql += ' where status = ?'
            params = (STATUS_REJECTED,)
        sql += ' order by rejected_at desc'
        if limit is not None:
            sql += ' limit ?'
            params = params + (limit,)
        with self.db() as curs:
            curs.execute(sql, params)
            rows = curs.fetchall()
        return [self._to_dead_letter(row) for row in rows]

    def count(self, include_replayed=True):
        sql = 'select count(*) from dead_letter'
        params = ()
        if not include_replayed:
            sql += ' where status = ?'
            params = (STATUS_REJECTED,)
        with self.db() as curs:
            curs.execute(sql, params)
            return curs.fetchone()[0]

    def delete(self, id):
        with self.db(commit=True) as curs:
            curs.execute('delete from dead_letter where id = ?', (id,))
            return curs.rowcount == 1

    def flush(self):
        with self.db(commit=True) as curs:
            curs.execute('delete from dead_letter')
            curs.execute('delete from dead_letter_pending')

    def mark_replayed(self, id, now=None):
        now = time.time() if now is None else now
        with self.db(commit=True) as curs:
            curs.execute('update dead_letter set status = ?, replayed_at = ? '
                         'where id = ? and status = ?',
                         (STATUS_REPLAYED, now, id, STATUS_REJECTED))
            if curs.rowcount != 1:
                raise KeyError(id)

    def pending_origin(self, data_hash):
        with self.db() as curs:
            curs.execute('select origin_id from dead_letter_pending '
                         'where data_hash = ?', (data_hash,))
            row = curs.fetchone()
        return row[0] if row is not None else None

    def register_pending(self, data_hash, origin_id):
        with self.db(commit=True) as curs:
            curs.execute('insert or replace into dead_letter_pending '
                         '(data_hash, origin_id, created_at) values (?, ?, ?)',
                         (data_hash, origin_id, time.time()))

    def clear_pending(self, data_hash):
        with self.db(commit=True) as curs:
            curs.execute('delete from dead_letter_pending where data_hash = ?',
                         (data_hash,))
