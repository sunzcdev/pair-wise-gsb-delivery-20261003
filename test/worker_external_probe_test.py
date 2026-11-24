# -*- coding: utf-8 -*-
#
# Licensed under the Apache License, Version 2.0 (the "License"); you may not
# use this file except in compliance with the License. You may obtain a copy of
# the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
# WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
# License for the specific language governing permissions and limitations under
# the License.
#

"""
Tests for distinguishing the two ways an external task can block:

* the completion probe runs and reports the output is missing, versus
* the probe itself raises (e.g. an external storage that is temporarily
  unreachable), which is retried with bounded backoff.

The blocked external task stays PENDING either way; only tasks depending on it
wait, and the recorded waiting reason follows the real probe outcome.
"""

import os
import shutil
import tempfile

from helpers import unittest

import luigi
from luigi.local_target import LocalTarget
from luigi.scheduler import Scheduler
from luigi.task_status import DONE, EXTERNAL_MISSING, EXTERNAL_PROBE_FAILED, FAILED, PENDING


class ExternalProbeSchedulerTest(unittest.TestCase):
    """Scheduler-side state machine for external task blocking."""

    def setUp(self):
        self.scheduler = Scheduler(retry_delay=0.0, external_probe_retry_delay=100.0, external_probe_retry_count=2)
        self.worker = "worker-1"
        self.tempdir = tempfile.mkdtemp(prefix="luigi-probe-sched-")

    def tearDown(self):
        shutil.rmtree(self.tempdir, ignore_errors=True)

    def _add_external(self, task_id="Ext()", status=PENDING):
        self.scheduler.add_task(worker=self.worker, task_id=task_id, status=status, is_external=True)

    def _add_dependent(self, task_id, dep_id, worker=None):
        self.scheduler.add_task(worker=worker or self.worker, task_id=task_id, status=PENDING, deps=[dep_id])

    def _get(self, task_id):
        return self.scheduler._state.get_task(task_id)

    def test_missing_and_probe_failure_are_distinct_blocking_reasons(self):
        self._add_external("ExtMissing()")
        self.scheduler.report_external_task_missing(worker=self.worker, task_id="ExtMissing()")
        missing = self._get("ExtMissing()")
        self.assertEqual(missing.status, PENDING)
        self.assertEqual(missing.external_state, EXTERNAL_MISSING)

        self._add_external("ExtProbe()")
        self.scheduler.report_external_probe_failure(worker=self.worker, task_id="ExtProbe()", message="boom")
        probe = self._get("ExtProbe()")
        self.assertEqual(probe.status, PENDING)
        self.assertEqual(probe.external_state, EXTERNAL_PROBE_FAILED)
        self.assertEqual(probe.external_state_message, "boom")
        self.assertEqual(probe.external_probe_attempts, 1)

    def test_probe_failure_backoff_keeps_external_unschedulable_but_others_run(self):
        # external task with a failing probe
        self._add_external("Ext()")
        self.scheduler.report_external_probe_failure(worker=self.worker, task_id="Ext()", message="boom")
        # an independent task that has nothing to do with the external one
        self.scheduler.add_task(worker=self.worker, task_id="Independent()", status=PENDING)
        # a task blocked behind the external one
        self._add_dependent("Parent()", "Ext()")

        # While in backoff, the external task and its parent must wait, but the
        # independent task is still handed out normally.
        first = self.scheduler.get_work(worker=self.worker)["task_id"]
        self.assertEqual(first, "Independent()")
        # nothing left that is schedulable: Ext() is in backoff, Parent() blocked
        self.assertIsNone(self.scheduler.get_work(worker=self.worker)["task_id"])

        # once the backoff window elapses the external probe becomes retryable,
        # but the parent still cannot run because the external task is not DONE
        probe = self._get("Ext()")
        probe.external_probe_retry = 0.0
        self.scheduler.prune()

        second = self.scheduler.get_work(worker=self.worker)["task_id"]
        self.assertEqual(second, "Ext()")

        third = self.scheduler.get_work(worker=self.worker)
        self.assertIsNone(third["task_id"])
        self.assertEqual(self._get("Parent()").status, PENDING)

    def test_probe_failure_then_missing_updates_waiting_reason(self):
        self._add_external("Ext()")
        self.scheduler.report_external_probe_failure(worker=self.worker, task_id="Ext()", message="connection reset")
        self.assertEqual(self._get("Ext()").external_state, EXTERNAL_PROBE_FAILED)
        self.assertEqual(self._get("Ext()").external_probe_attempts, 1)

        # a successful probe answering "not ready" supersedes the transient error
        self.scheduler.report_external_task_missing(worker=self.worker, task_id="Ext()")
        task = self._get("Ext()")
        self.assertEqual(task.status, PENDING)
        self.assertEqual(task.external_state, EXTERNAL_MISSING)
        self.assertEqual(task.external_probe_attempts, 0)

    def test_bounded_probe_retries_eventually_fail(self):
        self._add_external("Ext()")
        # retry_count == 2: two failures stay PENDING, the third gives up
        r1 = self.scheduler.report_external_probe_failure(worker=self.worker, task_id="Ext()", message="e1")
        r2 = self.scheduler.report_external_probe_failure(worker=self.worker, task_id="Ext()", message="e2")
        self.assertEqual(r1["status"], PENDING)
        self.assertEqual(r2["status"], PENDING)

        r3 = self.scheduler.report_external_probe_failure(worker=self.worker, task_id="Ext()", message="e3")
        self.assertEqual(r3["status"], FAILED)
        self.assertEqual(self._get("Ext()").status, FAILED)
        # leaving PENDING clears the external blocking bookkeeping
        self.assertIsNone(self._get("Ext()").external_state)

    def test_failed_after_exhausted_budget_is_not_resurrected_by_prune(self):
        self._add_external("Ext()")
        for _ in range(3):
            self.scheduler.report_external_probe_failure(worker=self.worker, task_id="Ext()", message="err")
        task = self._get("Ext()")
        self.assertEqual(task.status, FAILED)
        self.assertIsNone(task.retry)

        # pruning must not crash on the cleared retry and must keep it FAILED
        self.scheduler.prune()
        self.assertEqual(self._get("Ext()").status, FAILED)

    def test_done_only_unblocks_dependent_branch(self):
        self._add_external("Ext()")
        self.scheduler.report_external_probe_failure(worker=self.worker, task_id="Ext()")
        self._add_dependent("Parent()", "Ext()")
        self.scheduler.add_task(worker=self.worker, task_id="Independent()", status=PENDING)

        # during backoff the independent task still runs, the dependent branch waits
        self.assertEqual(self.scheduler.get_work(worker=self.worker)["task_id"], "Independent()")
        self.assertIsNone(self.scheduler.get_work(worker=self.worker)["task_id"])

        # output finally appears: worker reports DONE
        self.scheduler.add_task(worker=self.worker, task_id="Ext()", status=DONE, is_external=True)
        self.assertEqual(self._get("Ext()").status, DONE)
        self.assertIsNone(self._get("Ext()").external_state)

        # now the dependent branch finally gets its run opportunity
        self.assertEqual(self.scheduler.get_work(worker=self.worker)["task_id"], "Parent()")

    def test_serialized_task_exposes_external_blocking_reason(self):
        self._add_external("Ext()")
        self.scheduler.report_external_probe_failure(worker=self.worker, task_id="Ext()", message="nope")
        serialized = self.scheduler._serialize_task("Ext()")
        self.assertEqual(serialized["status"], PENDING)
        self.assertEqual(serialized["external_state"], EXTERNAL_PROBE_FAILED)
        self.assertEqual(serialized["external_state_message"], "nope")
        self.assertGreaterEqual(serialized["external_probe_attempts"], 1)

    def test_rpc_methods_are_registered(self):
        for name in ("report_external_task_missing", "report_external_probe_failure"):
            self.assertIn(name, luigi.scheduler.RPC_METHODS)

    def test_legacy_pickled_task_without_external_fields_loads(self):
        import pickle

        import luigi.scheduler as scheduler_module

        old_task = scheduler_module.Task(task_id="Legacy()", status=PENDING, deps=None)
        for attr in ("external_state", "external_state_message", "external_probe_attempts", "external_probe_retry"):
            if hasattr(old_task, attr):
                delattr(old_task, attr)

        path = os.path.join(self.tempdir, "legacy.pickle")
        with open(path, "wb") as fobj:
            pickle.dump(({"Legacy()": old_task}, {}, {}), fobj)

        state = scheduler_module.SimpleTaskState(state_path=path)
        state.load()
        loaded = state.get_task("Legacy()")
        # attributes are back-filled, so pruning/scheduling won't crash on it
        self.assertIsNone(loaded.external_state)
        self.assertEqual(loaded.external_probe_attempts, 0)
        state.update_status(loaded, self.scheduler._config)


class _ControllableExternalTask(luigi.ExternalTask):
    """External task whose probe outcome can be scripted test by test."""

    key = luigi.Parameter()

    # class-level registry of per-key outcomes, shared across pickled copies
    outcomes = {}

    def complete(self):
        state = type(self).outcomes[self.key]
        kind = state["kind"]
        if kind == "error":
            state["calls"] += 1
            raise RuntimeError(state.get("message", "probe unavailable"))
        if kind == "missing":
            state["calls"] += 1
            return False
        state["calls"] += 1
        return True


class _DependentTask(luigi.Task):
    key = luigi.Parameter()
    ran = False

    def requires(self):
        return _ControllableExternalTask(key=self.key)

    def output(self):
        return LocalTarget(os.path.join(_DependentTask.tempdir, "parent-{}.out".format(self.key)))

    def run(self):
        type(self).ran = True
        with self.output().open("w") as fobj:
            fobj.write("done")


class ExternalProbeWorkerTest(unittest.TestCase):
    """End-to-end behavior through the Worker (single process)."""

    def setUp(self):
        self.tempdir = tempfile.mkdtemp(prefix="luigi-probe-test-")
        _DependentTask.tempdir = self.tempdir
        _ControllableExternalTask.outcomes = {}

    def tearDown(self):
        shutil.rmtree(self.tempdir, ignore_errors=True)
        _ControllableExternalTask.outcomes = {}

    def _worker(self, **kwargs):
        scheduler = Scheduler(
            retry_delay=0.0,
            external_probe_retry_delay=kwargs.pop("external_probe_retry_delay", 0.0),
            external_probe_retry_count=kwargs.pop("external_probe_retry_count", 5),
        )
        return luigi.worker.Worker(scheduler=scheduler, worker_processes=1, wait_interval=0.001, wait_jitter=0, **kwargs), scheduler

    def test_initial_probe_error_marks_probe_failed_and_keeps_parent_pending(self):
        _ControllableExternalTask.outcomes["a"] = {"kind": "error", "message": "metastore down", "calls": 0}
        worker, scheduler = self._worker()
        parent = _DependentTask(key="a")
        # a probe error means scheduling could not fully verify the graph,
        # mirroring the historical complete()-error behavior
        self.assertFalse(worker.add(parent))

        ext = scheduler._state.get_task(_ControllableExternalTask(key="a").task_id)
        self.assertEqual(ext.status, PENDING)
        self.assertEqual(ext.external_state, EXTERNAL_PROBE_FAILED)
        self.assertIn("metastore down", ext.external_state_message)
        # parent is still just waiting, not failed
        self.assertEqual(scheduler._state.get_task(parent.task_id).status, PENDING)

    def test_missing_probe_answer_marks_external_missing(self):
        _ControllableExternalTask.outcomes["m"] = {"kind": "missing", "calls": 0}
        worker, scheduler = self._worker()
        parent = _DependentTask(key="m")
        # a successful probe answering "not ready" is a normal, non-erroneous state
        self.assertTrue(worker.add(parent))

        ext = scheduler._state.get_task(_ControllableExternalTask(key="m").task_id)
        self.assertEqual(ext.status, PENDING)
        self.assertEqual(ext.external_state, EXTERNAL_MISSING)
        self.assertEqual(scheduler._state.get_task(parent.task_id).status, PENDING)

    def test_probe_failure_then_missing_transitions_waiting_reason(self):
        # phase 1: probe temporarily fails
        _ControllableExternalTask.outcomes["b"] = {"kind": "error", "message": "transient", "calls": 0}
        worker, scheduler = self._worker()
        ext_id = _ControllableExternalTask(key="b").task_id
        worker.add(_DependentTask(key="b"))
        self.assertEqual(scheduler._state.get_task(ext_id).external_state, EXTERNAL_PROBE_FAILED)

        # phase 2: the probe recovers and explicitly reports the output is missing.
        # Re-adding the task re-runs complete() and must update the real reason.
        _ControllableExternalTask.outcomes["b"] = {"kind": "missing", "calls": 0}
        worker.add(_DependentTask(key="b"))
        ext = scheduler._state.get_task(ext_id)
        self.assertEqual(ext.status, PENDING)
        self.assertEqual(ext.external_state, EXTERNAL_MISSING)

    def test_independent_task_runs_while_external_is_blocked(self):
        _ControllableExternalTask.outcomes["c"] = {"kind": "error", "message": "down", "calls": 0}

        class Independent(luigi.Task):
            ran = False

            def complete(self):
                return type(self).ran

            def run(self):
                type(self).ran = True

        worker, scheduler = self._worker()
        worker.add(_DependentTask(key="c"))
        independent = Independent()
        self.assertTrue(worker.add(independent))
        with worker:
            worker.run()

        # the independent task must not be blocked by the failing external probe
        self.assertTrue(Independent.ran)
        # while the external branch remains pending with the probe-failure reason
        ext_id = _ControllableExternalTask(key="c").task_id
        ext = scheduler._state.get_task(ext_id)
        self.assertEqual(ext.status, PENDING)
        self.assertEqual(ext.external_state, EXTERNAL_PROBE_FAILED)
        self.assertFalse(os.path.exists(_DependentTask(key="c").output().path))

    def test_probe_failure_then_output_appears_unblocks_parent_end_to_end(self):
        # first the probe cannot connect, then the external output shows up
        _ControllableExternalTask.outcomes["d"] = {"kind": "error", "message": "down", "calls": 0}
        worker, scheduler = self._worker(retry_external_tasks=True, external_probe_retry_delay=0.0)
        ext_id = _ControllableExternalTask(key="d").task_id
        parent = _DependentTask(key="d")
        worker.add(parent)
        self.assertEqual(scheduler._state.get_task(ext_id).external_state, EXTERNAL_PROBE_FAILED)

        # output appears externally; re-adding probes it as complete -> DONE
        _ControllableExternalTask.outcomes["d"] = {"kind": "present", "calls": 0}
        self.assertTrue(worker.add(parent))
        self.assertEqual(scheduler._state.get_task(ext_id).status, DONE)

        with worker:
            worker.run()
        # the dependent branch was finally allowed to run once the output existed
        self.assertTrue(os.path.exists(parent.output().path))

    def test_runtime_probe_failure_uses_bounded_backoff_and_keeps_parent_pending(self):
        # A successful initial probe says "missing"; the task is then dispatched
        # at runtime, where its probe starts failing transiently.
        _ControllableExternalTask.outcomes["e"] = {"kind": "missing", "calls": 0}
        worker, scheduler = self._worker(
            retry_external_tasks=True,
            keep_alive=True,
            external_probe_retry_delay=0.0,
            external_probe_retry_count=3,
        )
        ext_id = _ControllableExternalTask(key="e").task_id
        parent = _DependentTask(key="e")
        self.assertTrue(worker.add(parent))
        self.assertEqual(scheduler._state.get_task(ext_id).external_state, EXTERNAL_MISSING)

        # now the probe starts failing when the worker runs the external task
        _ControllableExternalTask.outcomes["e"] = {"kind": "error", "message": "transient", "calls": 0}

        # drive one get_work/run cycle manually without the full run loop
        response = scheduler.get_work(worker=worker._id)
        self.assertEqual(response["task_id"], ext_id)
        worker._run_task(ext_id)
        worker._handle_next_task()

        ext = scheduler._state.get_task(ext_id)
        self.assertEqual(ext.status, PENDING)
        self.assertEqual(ext.external_state, EXTERNAL_PROBE_FAILED)
        self.assertEqual(ext.external_probe_attempts, 1)
        # the dependent branch still has not run
        self.assertFalse(os.path.exists(parent.output().path))

    def test_runtime_missing_answer_stays_pending_with_missing_reason(self):
        _ControllableExternalTask.outcomes["f"] = {"kind": "missing", "calls": 0}
        worker, scheduler = self._worker(retry_external_tasks=True)
        ext_id = _ControllableExternalTask(key="f").task_id
        parent = _DependentTask(key="f")
        self.assertTrue(worker.add(parent))

        response = scheduler.get_work(worker=worker._id)
        self.assertEqual(response["task_id"], ext_id)
        worker._run_task(ext_id)
        worker._handle_next_task()

        ext = scheduler._state.get_task(ext_id)
        self.assertEqual(ext.status, PENDING)
        self.assertEqual(ext.external_state, EXTERNAL_MISSING)
        self.assertFalse(os.path.exists(parent.output().path))

    def test_runtime_probe_failures_are_bounded_and_end_in_failed(self):
        _ControllableExternalTask.outcomes["g"] = {"kind": "missing", "calls": 0}
        worker, scheduler = self._worker(
            retry_external_tasks=True,
            external_probe_retry_delay=0.0,
            external_probe_retry_count=2,
        )
        ext_id = _ControllableExternalTask(key="g").task_id
        parent = _DependentTask(key="g")
        self.assertTrue(worker.add(parent))
        _ControllableExternalTask.outcomes["g"] = {"kind": "error", "message": "gone", "calls": 0}

        # first two runtime probes stay within the bounded budget -> PENDING
        for expected_attempt in (1, 2):
            scheduler.get_work(worker=worker._id)
            worker._run_task(ext_id)
            worker._handle_next_task()
            ext = scheduler._state.get_task(ext_id)
            self.assertEqual(ext.status, PENDING)
            self.assertEqual(ext.external_state, EXTERNAL_PROBE_FAILED)
            self.assertEqual(ext.external_probe_attempts, expected_attempt)

        # the third failure exhausts the budget -> the task becomes FAILED
        scheduler.get_work(worker=worker._id)
        worker._run_task(ext_id)
        worker._handle_next_task()
        ext = scheduler._state.get_task(ext_id)
        self.assertEqual(ext.status, FAILED)
        self.assertIsNone(ext.external_state)
        # dependent branch never ran
        self.assertFalse(os.path.exists(parent.output().path))


if __name__ == "__main__":
    unittest.main()
