.. _saga:

Compensation chains (sagas)
===========================

A *compensation chain* (also known as a saga) runs a serial chain of stages
where each stage may register a **compensation** callback. When a later stage
fails, the compensations of the stages that already completed are executed in
the reverse of their completion order, undoing external side-effects such as
temporary files, remote scratch resources or reservations.

The helper lives in :mod:`huey.saga` and is framework-agnostic: it can run
standalone or inside a regular huey task.

.. code-block:: python

    from huey import Saga

    saga = Saga('collection-chain')
    saga.add(fetch_users, compensate=release_users)
    saga.add(fetch_orders, compensate=release_orders, retries=2)
    saga.add(fetch_inventory, compensate=release_inventory)

    result = saga.run()
    if not result.ok:
        print('stage %s failed: %s' % (result.failed_stage, result.error))

Semantics
---------

* Only stages that **truly completed** enter the compensation list.
* Compensations run in the **reverse of completion order**.
* A stage that was never executed, or that itself failed, is **not**
  compensated -- a stage that fails partway must clean up its own partial
  side-effects.
* While a stage still has ordinary retries left (``retries=`` /
  ``retry_delay=``), **no compensation is triggered**; unwinding only begins
  once the retries are exhausted.
* A compensation receives the return value of its forward stage.
* If a compensation fails, both the original failure and the compensation
  failure are preserved on the result object
  (``result.error`` and the corresponding ``CompensationRecord.error``), and
  the remaining compensations still run.
* Side-effects are invoked with **at-least-once** semantics: a compensation
  may be called more than once, so compensation callbacks **must be
  re-entrant** (safe to invoke repeatedly).

Retrying unfinished compensations
---------------------------------

Compensations that did not complete can be retried explicitly without
re-executing any forward task that already succeeded:

.. code-block:: python

    result = saga.run()
    if not result.ok and result.pending_compensations:
        # Re-runs only the unfinished compensations, in unwind order.
        result.retry_compensations(raise_on_error=True)

Each :py:class:`CompensationRecord` tracks its ``status``
(``pending`` / ``done`` / ``failed``), the last ``error`` and the number of
``attempts``.

Raising on compensation failure
-------------------------------

Pass ``raise_on_compensation_error=True`` to :py:meth:`Saga.run` to raise a
:py:class:`huey.saga.CompensationError` when any compensation fails. The
exception carries both the original failure (``original_error``) and the list
of ``(stage_name, exception)`` compensation failures (``errors``).

Examples
--------

Runnable examples showing the actual call order when a chain partially
completes and then fails are available for both the in-memory and SQLite
backends:

* ``examples/saga/memory_saga.py``
* ``examples/saga/sqlite_saga.py``
