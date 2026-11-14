"""Compensation-chain (saga) example using MemoryHuey.

Simulates a serial collection pipeline where each stage acquires an
external temporary resource (temp table, remote scratch file, ...). The
third stage fails, so the completed stages are compensated in reverse
order. One compensation fails transiently and is retried explicitly via
``result.retry_compensations()`` -- compensations are at-least-once and
must be re-entrant.

Run directly:  python memory_saga.py
"""

from huey import MemoryHuey, Saga

# Quiet the consumer's error logging for the deliberately-failing task.
import logging
logging.getLogger('huey').setLevel(logging.CRITICAL)

huey = MemoryHuey(immediate=True)

# Ordered record of every external side-effect actually invoked.
call_log = []

# Simulated external resource registry; compensations are re-entrant:
# releasing an already-released resource is a no-op.
resources = {'users': False, 'orders': False, 'inventory': False}
flaky_release = {'orders': 1}  # first release attempt for orders fails.


def acquire(name):
    call_log.append('acquire:%s' % name)
    resources[name] = True
    return name


def release(name):
    # Re-entrant compensation: safe to call more than once.
    call_log.append('release:%s' % name)
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
        call_log.append('failed:%s(%s)' % (result.failed_stage, result.error))
        pending = [r.name for r in result.pending_compensations]
        if pending:
            call_log.append('pending-compensations:%s' % ','.join(pending))
            # Explicitly retry only the unfinished compensations; the
            # successful forward tasks are not re-executed.
            result.retry_compensations()
    return result.ok


if __name__ == '__main__':
    run_chain()
    print('actual call order:')
    for i, entry in enumerate(call_log, 1):
        print('  %2d. %s' % (i, entry))
    print('resources still held:', [k for k, v in resources.items() if v])
