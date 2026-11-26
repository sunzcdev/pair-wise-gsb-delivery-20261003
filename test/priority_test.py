# -*- coding: utf-8 -*-
#
# Copyright 2012-2015 Spotify AB
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

from helpers import unittest

import luigi
import luigi.notifications
import luigi.scheduler

luigi.notifications.DEBUG = True


class PrioTask(luigi.Task):
    prio = luigi.Parameter()
    run_counter = 0

    @property
    def priority(self):
        return self.prio

    def requires(self):
        if self.prio > 10:
            return PrioTask(self.prio - 10)

    def run(self):
        self.t = PrioTask.run_counter
        PrioTask.run_counter += 1

    def complete(self):
        return hasattr(self, "t")


class PriorityTest(unittest.TestCase):
    def test_priority(self):
        p, q, r = PrioTask(1), PrioTask(2), PrioTask(3)
        luigi.build([p, q, r], local_scheduler=True)
        self.assertTrue(r.t < q.t < p.t)

    def test_priority_w_dep(self):
        x, y, z = PrioTask(25), PrioTask(15), PrioTask(5)
        a, b, c = PrioTask(24), PrioTask(14), PrioTask(4)
        luigi.build([a, b, c, x, y, z], local_scheduler=True)
        self.assertTrue(z.t < y.t < x.t < c.t < b.t < a.t)


class SharedDepTest(unittest.TestCase):
    """
    Two targets share a not-yet-finished dependency, one of them with a higher priority.
    """

    def setUp(self):
        self.scheduler = luigi.scheduler.Scheduler(
            retry_delay=100,
            remove_delay=1000,
            worker_disconnect_delay=10,
            disable_persist=10,
            disable_window=10,
            retry_count=3,
            disable_hard_timeout=60 * 60,
            stable_done_cooldown_secs=0,
        )

    def add(self, task_id, **kwargs):
        self.scheduler.add_task(worker="worker", task_id=task_id, **kwargs)

    def priority_of(self, task_id):
        return self.scheduler._state.get_task(task_id).priority

    def test_shared_dependency_is_prioritized_then_falls_back(self):
        # S is the unfinished shared dependency; HI has a high priority, LO a
        # low one, and MID is an independent task with a medium priority.
        self.add("S", priority=0)
        self.add("LO", priority=1, deps=["S"])
        self.add("HI", priority=10, deps=["S"])
        self.add("MID", priority=5)

        # Candidate query before the high priority target is withdrawn: S
        # inherits its priority so that it gets queued in time.
        self.assertEqual(10, self.priority_of("S"))
        self.assertEqual("S", self.scheduler.get_work(worker="worker")["task_id"])

        # Withdraw HI (it never managed to run) and query the candidates again.
        self.scheduler._state.inactivate_tasks(["HI"])

        # The initial explicit priority of S (0) is not lowered...
        self.assertEqual(0, self.scheduler._state.get_task("S").explicit_priority)
        # ...but the inherited priority falls back to the remaining LO target.
        self.assertEqual(1, self.priority_of("S"))
        # Scheduling follows the actual dependency graph again.
        self.assertEqual("MID", self.scheduler.get_work(worker="worker")["task_id"])

    def test_other_parent_still_waits_for_shared_dependency(self):
        self.add("S", priority=0)
        self.add("LO", priority=1, deps=["S"])
        self.add("HI", priority=10, deps=["S"])

        self.assertEqual("S", self.scheduler.get_work(worker="worker")["task_id"])

        # Even while running, the shared dependency still blocks the remaining
        # parent; withdrawing the high priority target does not release it.
        self.scheduler._state.inactivate_tasks(["HI"])
        self.assertIsNone(self.scheduler.get_work(worker="worker")["task_id"])

        self.add("S", status=luigi.scheduler.DONE)
        self.assertEqual("LO", self.scheduler.get_work(worker="worker")["task_id"])

