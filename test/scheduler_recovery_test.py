# -*- coding: utf-8 -*-
#
# Copyright 2026 Spotify AB
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

import os
import shutil
import tempfile
import time

from helpers import unittest

import luigi.scheduler
from luigi.task_status import DONE, PENDING, RUNNING


class RecoveryHandoffTest(unittest.TestCase):
    """
    Recovery handoff after a scheduler restart: restored RUNNING tasks can be
    reclaimed by their original worker within a claim window, and are released
    for takeover once the window expires. Late status updates from previous
    executors are told apart by their run token and ignored.
    """

    def setUp(self):
        self._tmp_dir = tempfile.mkdtemp()
        self.state_path = os.path.join(self._tmp_dir, "state.pickle")

    def tearDown(self):
        shutil.rmtree(self._tmp_dir, ignore_errors=True)

    def _scheduler(self, **kwargs):
        kwargs.setdefault("state_path", self.state_path)
        return luigi.scheduler.Scheduler(**kwargs)

    def _restart(self, scheduler, **kwargs):
        scheduler.dump()
        scheduler = self._scheduler(**kwargs)
        scheduler.load()
        return scheduler

    def _expire_claim(self, scheduler, task_id):
        task = scheduler._state.get_task(task_id)
        task.recovery_deadline = time.time() - 1

    def _kill_worker(self, scheduler, worker_id):
        worker = scheduler._state.get_existing_worker(worker_id)
        worker.last_active = time.time() - 10 * scheduler._config.worker_disconnect_delay

    def test_restored_running_task_awaits_claim(self):
        scheduler = self._scheduler()
        scheduler.add_task(worker="A", task_id="T1")
        self.assertEqual(scheduler.get_work(worker="A")["task_id"], "T1")

        scheduler = self._restart(scheduler)
        task = scheduler._state.get_task("T1")
        self.assertEqual(task.status, RUNNING)
        self.assertEqual(task.worker_running, "A")
        self.assertIsNotNone(task.recovery_deadline)

        # while the claim window is open, no one else can take the task
        self.assertIsNone(scheduler.get_work(worker="B")["task_id"])

    def test_surviving_worker_claims_and_finishes(self):
        scheduler = self._scheduler()
        scheduler.add_task(worker="A", task_id="T1")
        old_run_id = scheduler.get_work(worker="A")["run_id"]

        scheduler = self._restart(scheduler)

        # another worker may not claim the task
        claim = scheduler.claim_task_run(worker="B", task_id="T1")
        self.assertFalse(claim["claimed"])

        # the original worker re-registers and claims its original progress
        claim = scheduler.claim_task_run(worker="A", task_id="T1")
        self.assertTrue(claim["claimed"])
        self.assertEqual(claim["status"], RUNNING)
        self.assertNotEqual(claim["run_id"], old_run_id)

        task = scheduler._state.get_task("T1")
        self.assertIsNone(task.recovery_deadline)

        # the pre-restart run token is now stale and its updates are ignored
        scheduler.add_task(worker="A", task_id="T1", status=DONE, run_id=old_run_id)
        self.assertEqual(scheduler._state.get_task("T1").status, RUNNING)

        # the claimed run finishes with the new token
        scheduler.add_task(worker="A", task_id="T1", status=DONE, run_id=claim["run_id"])
        self.assertEqual(scheduler._state.get_task("T1").status, DONE)

    def test_alive_worker_keeps_task_after_deadline(self):
        scheduler = self._scheduler()
        scheduler.add_task(worker="A", task_id="T1")
        scheduler.get_work(worker="A")

        scheduler = self._restart(scheduler)
        scheduler.ping(worker="A")  # the worker is alive and reports back

        self._expire_claim(scheduler, "T1")
        scheduler.prune()

        task = scheduler._state.get_task("T1")
        self.assertEqual(task.status, RUNNING)
        self.assertEqual(task.worker_running, "A")
        self.assertIsNone(task.recovery_deadline)

    def test_takeover_after_deadline_and_stale_final_state_replay(self):
        scheduler = self._scheduler()
        scheduler.add_task(worker="A", task_id="UP")
        scheduler.add_task(worker="A", task_id="DOWN", deps=["UP"])
        reply = scheduler.get_work(worker="A")
        self.assertEqual(reply["task_id"], "UP")
        stale_run_id = reply["run_id"]

        scheduler = self._restart(scheduler)
        self._kill_worker(scheduler, "A")  # the original executor never comes back
        self._expire_claim(scheduler, "UP")
        scheduler.prune()

        # the unclaimed task is released for others; downstream deps are untouched
        task = scheduler._state.get_task("UP")
        self.assertEqual(task.status, PENDING)
        self.assertIsNone(task.worker_running)
        self.assertEqual(scheduler._state.get_task("DOWN").deps, {"UP"})

        # a late terminal state replayed by the previous executor is ignored
        scheduler.add_task(worker="A", task_id="UP", status=DONE, run_id=stale_run_id)
        self.assertEqual(scheduler._state.get_task("UP").status, PENDING)

        # the dead worker can no longer claim the task either
        self.assertFalse(scheduler.claim_task_run(worker="A", task_id="UP")["claimed"])

        # another worker takes over with a fresh run token
        scheduler.add_task(worker="B", task_id="UP", runnable=True)
        reply = scheduler.get_work(worker="B")
        self.assertEqual(reply["task_id"], "UP")
        self.assertNotEqual(reply["run_id"], stale_run_id)

        # replaying the original worker's terminal state again is still ignored
        scheduler.add_task(worker="A", task_id="UP", status=DONE, run_id=stale_run_id)
        self.assertEqual(scheduler._state.get_task("UP").status, RUNNING)

        # the graph only advances on the valid execution
        scheduler.add_task(worker="B", task_id="UP", status=DONE, run_id=reply["run_id"])
        self.assertEqual(scheduler._state.get_task("UP").status, DONE)

        scheduler.add_task(worker="B", task_id="DOWN", deps=["UP"], runnable=True)
        self.assertEqual(scheduler.get_work(worker="B")["task_id"], "DOWN")

    def test_stale_replay_does_not_unblock_downstream(self):
        scheduler = self._scheduler()
        scheduler.add_task(worker="A", task_id="UP")
        scheduler.add_task(worker="B", task_id="DOWN", deps=["UP"])
        stale_run_id = scheduler.get_work(worker="A")["run_id"]

        scheduler = self._restart(scheduler)
        self._kill_worker(scheduler, "A")
        self._expire_claim(scheduler, "UP")
        scheduler.prune()

        # stale DONE replayed while the task waits for a new executor
        scheduler.add_task(worker="A", task_id="UP", status=DONE, run_id=stale_run_id)
        self.assertEqual(scheduler._state.get_task("UP").status, PENDING)

        # the downstream task must not become schedulable on the stale update
        scheduler.add_task(worker="B", task_id="DOWN", deps=["UP"], runnable=True)
        self.assertIsNone(scheduler.get_work(worker="B")["task_id"])


if __name__ == "__main__":
    unittest.main()
