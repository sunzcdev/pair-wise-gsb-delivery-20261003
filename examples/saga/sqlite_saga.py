"""Compensation-chain (saga) example using SqliteHuey.

Same serial collection pipeline as the Memory example, but backed by
SqliteHuey. The call order of every external side-effect is persisted
into a SQLite table so the unwind sequence can be inspected after the
run, and a transiently-failing compensation is retried explicitly.

Run directly:  python sqlite_saga.py
"""

import os
import sqlite3

from huey import Saga, SqliteHuey

# Quiet the consumer's error logging for the deliberately-failing task.
import logging
logging.getLogger('huey').setLevel(logging.CRITICAL)

DB = os.path.join(os.path.dirname(__file__), 'saga_demo.db')

huey = SqliteHuey(filename=DB, immediate=True)


def log_call(entry):
    conn = sqlite3.connect(DB)
    try:
        conn.execute('create table if not exists call_log '
                     '(id integer primary key autoincrement, entry text)')
        conn.execute('insert into call_log (entry) values (?)', (entry,))
        conn.commit()
    finally:
        conn.close()


resources = {'users': False, 'orders': False, 'inventory': False}
flaky_release = {'orders': 1}


def acquire(name):
    log_call('acquire:%s' % name)
    resources[name] = True
    return name


def release(name):
    # Re-entrant compensation: safe to call more than once.
    log_call('release:%s' % name)
    if flaky_release.get(name):
        flaky_release[name] -= 1
        raise RuntimeError('release:%s temporarily unavailable' % name)
    resources[name] = False


@huey.task()
def collect_users():
    return acquire('users')


@huey.task()
def collect_orders():
    return acquire('orders')


@huey.task()
def collect_inventory():
    # Fails before producing a side-effect. A stage that fails partway
    # must undo its own partial work -- the chain only compensates
    # stages that *completed*.
    raise IOError('inventory source unreachable')


def build_saga():
    # Stage actions enqueue real huey tasks (executed synchronously in
    # immediate mode); ``.get()`` unwraps the return value, and a failing
    # task propagates its error to the saga.
    saga = Saga('collection-chain')
    saga.add(lambda: collect_users().get(), name='collect_users',
             compensate=lambda v: release(v))
    saga.add(lambda: collect_orders().get(), name='collect_orders',
             compensate=lambda v: release(v))
    # The failing stage's compensation is registered but never runs,
    # because the stage itself did not complete.
    saga.add(lambda: collect_inventory().get(), name='collect_inventory',
             compensate=lambda v: release(v))
    return saga


@huey.task()
def run_chain():
    result = build_saga().run()
    if not result.ok:
        log_call('failed:%s(%s)' % (result.failed_stage, result.error))
        pending = [r.name for r in result.pending_compensations]
        if pending:
            log_call('pending-compensations:%s' % ','.join(pending))
            result.retry_compensations()
    return result.ok


if __name__ == '__main__':
    if os.path.exists(DB):
        os.unlink(DB)
    run_chain()
    conn = sqlite3.connect(DB)
    try:
        rows = conn.execute('select id, entry from call_log order by id')
        print('actual call order (from sqlite):')
        for row_id, entry in rows:
            print('  %2d. %s' % (row_id, entry))
    finally:
        conn.close()
    print('resources still held:', [k for k, v in resources.items() if v])
    os.unlink(DB)
