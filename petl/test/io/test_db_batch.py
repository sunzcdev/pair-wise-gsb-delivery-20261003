# -*- coding: utf-8 -*-
from __future__ import absolute_import, print_function, division

import sqlite3

import pytest

import petl as etl
from petl.errors import ArgumentError


class BudgetError(Exception):
    """Raised by the test double when a statement exceeds the budget."""


class BudgetCursor(object):

    def __init__(self, connection):
        self.connection = connection
        self.closed = False

    def execute(self, sql):
        self.connection._check(sql, 1)

    def executemany(self, sql, rows):
        rows = list(rows)
        if rows:
            self.connection._check(sql, len(rows))

    def fetchone(self):
        return None

    def fetchmany(self, size=None):
        return []

    def fetchall(self):
        return []

    def close(self):
        self.closed = True


class BudgetConnection(object):
    """Minimal DB-API connection refusing statements over a budget."""

    def __init__(self, max_params, fail_after=None):
        self.max_params = max_params
        self.fail_after = fail_after
        self.statements = []
        self.cursors = []
        self.inserts = 0
        self.commits = 0
        self.rollbacks = 0

    def cursor(self):
        cursor = BudgetCursor(self)
        self.cursors.append(cursor)
        return cursor

    def _check(self, sql, rowcount):
        params = sql.count('?') * rowcount
        if params > self.max_params:
            raise BudgetError(
                'statement needs %d parameters, budget is %d'
                % (params, self.max_params))
        self.statements.append((sql, rowcount, params))
        if sql.startswith('INSERT'):
            self.inserts += 1
        if self.fail_after is not None and self.inserts > self.fail_after:
            raise RuntimeError('driver rejected statement')

    def commit(self):
        self.commits += 1

    def rollback(self):
        self.rollbacks += 1


def make_table(ncols, nrows):
    hdr = tuple('c%d' % i for i in range(ncols))
    rows = [tuple(i * ncols + j for j in range(ncols)) for i in range(nrows)]
    return [hdr] + rows


def insert_statements(connection):
    return [s for s in connection.statements if s[0].startswith('INSERT')]


@pytest.mark.parametrize('load', [etl.todb, etl.appenddb])
def test_budget_batches_inserts_by_written_columns(load):
    connection = BudgetConnection(max_params=7)
    load(make_table(3, 5), connection, 'target', max_params=7)

    # 3 written columns per row, so at most 2 rows fit the budget of 7
    rowcounts = [rowcount for _, rowcount, _ in insert_statements(connection)]
    assert rowcounts == [2, 2, 1]
    assert all(params <= 7 for _, _, params in insert_statements(connection))
    assert connection.commits == 1
    assert connection.rollbacks == 0
    assert all(cursor.closed for cursor in connection.cursors)


@pytest.mark.parametrize('load', [etl.todb, etl.appenddb])
def test_budget_exact_multiple_has_no_empty_tail(load):
    connection = BudgetConnection(max_params=6)
    load(make_table(3, 4), connection, 'target', max_params=6)

    rowcounts = [rowcount for _, rowcount, _ in insert_statements(connection)]
    assert rowcounts == [2, 2]
    assert connection.commits == 1


@pytest.mark.parametrize('load', [etl.todb, etl.appenddb])
def test_single_row_over_budget_errors_before_any_sql(load):
    connection = BudgetConnection(max_params=2)
    with pytest.raises(ArgumentError) as caught:
        load(make_table(3, 2), connection, 'target', max_params=2)

    message = str(caught.value)
    assert 'max_params' in message
    assert '3' in message
    # the error is raised before any SQL runs, not even the truncate
    assert connection.statements == []
    assert connection.cursors == []
    assert connection.commits == 0
    assert connection.rollbacks == 0


@pytest.mark.parametrize('load', [etl.todb, etl.appenddb])
def test_empty_table_with_budget(load):
    connection = BudgetConnection(max_params=5)
    load([('a', 'b')], connection, 'target', max_params=5)

    assert insert_statements(connection) == []
    assert connection.commits == 1
    assert connection.rollbacks == 0


@pytest.mark.parametrize('load', [etl.todb, etl.appenddb])
def test_conversion_error_mid_batch_keeps_transaction_with_caller(load):
    connection = BudgetConnection(max_params=4)
    failure = ValueError('cannot convert value')

    def rows():
        yield ('a', 'b')
        yield (1, 2)
        yield (3, 4)
        raise failure

    with pytest.raises(ValueError) as caught:
        load(rows(), connection, 'target', max_params=4)

    assert caught.value is failure
    # the first batch of 2 rows ran, then the source failed
    rowcounts = [rowcount for _, rowcount, _ in insert_statements(connection)]
    assert rowcounts == [2]
    # petl must not commit or roll back the caller's transaction
    assert connection.commits == 0
    assert connection.rollbacks == 0
    assert all(cursor.closed for cursor in connection.cursors)


@pytest.mark.parametrize('load', [etl.todb, etl.appenddb])
def test_driver_failure_mid_batch_keeps_transaction_with_caller(load):
    connection = BudgetConnection(max_params=4, fail_after=1)
    with pytest.raises(RuntimeError, match='driver rejected statement'):
        load(make_table(2, 4), connection, 'target', max_params=4)

    # one batch succeeded, the driver rejected the second one
    rowcounts = [rowcount for _, rowcount, _ in insert_statements(connection)]
    assert rowcounts == [2, 2]
    assert connection.commits == 0
    assert connection.rollbacks == 0
    assert all(cursor.closed for cursor in connection.cursors)


def test_budget_applies_to_caller_supplied_cursor():
    connection = BudgetConnection(max_params=4)
    cursor = connection.cursor()
    etl.appenddb(make_table(2, 3), cursor, 'target', max_params=4)

    rowcounts = [rowcount for _, rowcount, _ in insert_statements(connection)]
    assert rowcounts == [2, 1]
    assert connection.commits == 1
    # a borrowed cursor is left open
    assert not cursor.closed


class CountingConnection(sqlite3.Connection):

    def __init__(self, *args, **kwargs):
        super(CountingConnection, self).__init__(*args, **kwargs)
        self.commit_calls = 0

    def commit(self):
        self.commit_calls += 1
        return super(CountingConnection, self).commit()


@pytest.fixture
def sqlite_connection():
    conn = sqlite3.connect(':memory:', factory=CountingConnection)
    conn.execute('CREATE TABLE target (a, b)')
    conn.commit()
    conn.commit_calls = 0
    try:
        yield conn
    finally:
        conn.close()


@pytest.mark.parametrize('load', [etl.todb, etl.appenddb])
def test_tail_batch_smaller_than_budget_sqlite(sqlite_connection, load):
    table = [('a', 'b')] + [(i, i * 10) for i in range(5)]
    load(table, sqlite_connection, 'target', max_params=5)

    actual = sqlite_connection.execute(
        'SELECT * FROM target ORDER BY a').fetchall()
    assert actual == [(i, i * 10) for i in range(5)]
    assert sqlite_connection.commit_calls == 1


@pytest.mark.parametrize('load', [etl.todb, etl.appenddb])
def test_empty_table_sqlite(sqlite_connection, load):
    load([('a', 'b')], sqlite_connection, 'target', max_params=5)

    actual = sqlite_connection.execute('SELECT * FROM target').fetchall()
    assert actual == []
    assert sqlite_connection.commit_calls == 1


def test_conversion_error_sqlite_leaves_transaction_to_caller(
        sqlite_connection):
    failure = ValueError('cannot convert value')

    def rows():
        yield ('a', 'b')
        yield (1, 2)
        yield (3, 4)
        raise failure

    with pytest.raises(ValueError) as caught:
        etl.appenddb(rows(), sqlite_connection, 'target', max_params=4)

    assert caught.value is failure
    assert sqlite_connection.commit_calls == 0
    # the transaction still belongs to the caller, who can roll it back
    sqlite_connection.rollback()
    actual = sqlite_connection.execute('SELECT * FROM target').fetchall()
    assert actual == []


def test_driver_failure_sqlite_leaves_transaction_to_caller(
        sqlite_connection):
    sqlite_connection.execute('CREATE UNIQUE INDEX idx_a ON target (a)')
    table = [('a', 'b'), (1, 10), (2, 20), (2, 30)]

    with pytest.raises(sqlite3.IntegrityError):
        etl.appenddb(table, sqlite_connection, 'target', max_params=4)

    assert sqlite_connection.commit_calls == 0
    sqlite_connection.rollback()
    actual = sqlite_connection.execute('SELECT * FROM target').fetchall()
    assert actual == []
