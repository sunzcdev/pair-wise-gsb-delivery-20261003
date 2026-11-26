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

import os
import tempfile

from helpers import LuigiTestCase

import luigi
import luigi.scheduler
import luigi.task_history
import luigi.worker

luigi.notifications.DEBUG = True


class SimpleTaskHistory(luigi.task_history.TaskHistory):
    def __init__(self):
        self.actions = []

    def task_scheduled(self, task):
        self.actions.append(("scheduled", task.id))

    def task_finished(self, task, successful):
        self.actions.append(("finished", task.id))

    def task_started(self, task, worker_host):
        self.actions.append(("started", task.id))


class TaskHistoryTest(LuigiTestCase):
    def test_run(self):
        th = SimpleTaskHistory()
        sch = luigi.scheduler.Scheduler(task_history_impl=th)
        with luigi.worker.Worker(scheduler=sch) as w:

            class MyTask(luigi.Task):
                pass

            task = MyTask()
            w.add(task)
            w.run()

            self.assertEqual(th.actions, [("scheduled", task.task_id), ("started", task.task_id), ("finished", task.task_id)])


class TaskSnapshotTest(LuigiTestCase):
    """Paused history writes must not move an immutable snapshot boundary."""

    def setUp(self):
        super().setUp()
        self.history = luigi.task_history.DeferringHistory(defer_kinds=["finished"])
        self.scheduler = luigi.scheduler.Scheduler(task_history_impl=self.history)

    def _add(self, task_id, **kwargs):
        kwargs.setdefault("worker", "w1")
        self.scheduler.add_task(task_id=task_id, **kwargs)

    def _state(self, task_id):
        return self.scheduler.task_list()[task_id]["status"]

    def test_snapshot_boundary_and_late_write(self):
        self._add("parent", deps=["child"])
        self._add("child")
        self.scheduler.get_work(worker="w1")

        # The failure event is decided here but its write is paused.
        self._add("child", status=luigi.scheduler.FAILED, expl="boom")
        self.assertEqual(self._state("child"), luigi.scheduler.FAILED)

        boundary = self.scheduler.history_boundary()
        self.assertEqual(boundary["revision"], self.scheduler._state.revision)
        self.assertEqual(len(boundary["outstanding"]), 1)
        self.assertEqual(boundary["outstanding"][0]["event_name"], luigi.scheduler.FAILED)
        self.assertEqual(boundary["outstanding"][0]["task_id"], "child")

        # The scheduler arranges the retry: live state moves on, history does not.
        self.scheduler._state.get_task("child").retry = 0
        self.scheduler.prune()
        self.assertEqual(self._state("child"), luigi.scheduler.PENDING)

        # Sample after the retry: the graph shows the new round, while the
        # paused failure event is still owed against the earlier one.
        snapshot = self.scheduler.task_snapshot(failed_only=True)
        events_at_snapshot = [event["event_name"] for event in snapshot["events"]]
        self.assertEqual([event["event_name"] for event in snapshot["outstanding"]], [luigi.scheduler.FAILED])
        self.assertEqual(snapshot["failed_tasks"], ["child"])
        self.assertNotIn(luigi.scheduler.FAILED, events_at_snapshot)
        self.assertEqual(snapshot["tasks"]["child"]["status"], luigi.scheduler.PENDING)
        self.assertFalse(snapshot["history"][-1]["landed"])

        # Ordinary history queries still see only what was really written.
        self.assertEqual([kind for kind, *_ in self.history.events], ["scheduled", "scheduled", "started"])

        # The paused write is retried and lands with its decision time.
        decided_ts = self.scheduler.history_boundary()["outstanding"][0]["ts"]
        self.history.release("finished")
        result = self.scheduler.flush_task_history()
        self.assertEqual(result["written"], 1)
        self.assertEqual(result["outstanding"], [])
        self.assertEqual([entry[0] for entry in self.history.events], ["scheduled", "scheduled", "started", "finished"])
        self.assertEqual(self.history.events[-1][3].isoformat(), decided_ts)

        # Reading the same snapshot twice never pulls the late write into it.
        self.assertEqual(self.scheduler.get_task_snapshot(snapshot["snapshot_id"]), snapshot)
        self.assertEqual(self.scheduler.task_snapshot(snapshot["snapshot_id"]), snapshot)
        again = self.scheduler.get_task_snapshot(snapshot["snapshot_id"])
        self.assertEqual([event["event_name"] for event in again["events"]], events_at_snapshot)
        self.assertEqual([event["event_name"] for event in again["outstanding"]], [luigi.scheduler.FAILED])

        # A new snapshot is a new boundary: it sees the late-written failure.
        fresh = self.scheduler.task_snapshot(failed_only=False)
        self.assertNotEqual(fresh["snapshot_id"], snapshot["snapshot_id"])
        fresh_events = [(event["task_id"], event["event_name"]) for event in fresh["events"]]
        self.assertIn(("child", luigi.scheduler.FAILED), fresh_events)
        self.assertEqual(fresh["outstanding"], [])
        for task_id, node in fresh["tasks"].items():
            self.assertEqual(node["status"], self._state(task_id))

    def test_repeated_reads_do_not_absorb_next_round(self):
        self._add("t")
        self.scheduler.get_work(worker="w1")
        self._add("t", status=luigi.scheduler.FAILED)
        self.scheduler._state.get_task("t").retry = 0
        self.scheduler.prune()
        self._add("t", status=luigi.scheduler.PENDING)
        self.scheduler.get_work(worker="w1")

        snapshot = self.scheduler.task_snapshot()
        snapshot_events = [(event["task_id"], event["event_name"]) for event in snapshot["events"]]
        for _ in range(3):
            reread = self.scheduler.get_task_snapshot(snapshot["snapshot_id"])
            self.assertEqual([(event["task_id"], event["event_name"]) for event in reread["events"]], snapshot_events)
            self.assertEqual(reread["revision"], snapshot["revision"])
        self.assertIn(("t", luigi.scheduler.RUNNING), [(event["task_id"], event["event_name"]) for event in self.scheduler.history_boundary()["landed"]])

    def test_snapshot_is_stable_when_tasks_are_pruned(self):
        self._add("t")
        snapshot = self.scheduler.task_snapshot()
        snapshot_tasks = snapshot["tasks"]
        self.scheduler._state.inactivate_tasks(["t"])
        reread = self.scheduler.get_task_snapshot(snapshot["snapshot_id"])
        self.assertEqual(reread["tasks"], snapshot_tasks)
        self.assertNotIn("t", self.scheduler.task_list())

    def test_snapshot_of_failed_graph_includes_dependencies(self):
        self._add("root", deps=["dep"])
        self._add("dep")
        self.scheduler.get_work(worker="w1")
        self._add("dep", status=luigi.scheduler.DONE)
        self._add("root", status=luigi.scheduler.FAILED)

        snapshot = self.scheduler.task_snapshot(failed_only=True)
        self.assertEqual(snapshot["failed_tasks"], ["root"])
        self.assertEqual(set(snapshot["tasks"]), {"root", "dep"})
        self.assertEqual(snapshot["tasks"]["root"]["deps"], ["dep"])

    def test_outstanding_journal_survives_dump_and_load(self):
        with tempfile.TemporaryDirectory() as folder:
            path = os.path.join(folder, "state.pickle")
            self.scheduler._state._state_path = path
            self._add("t")
            self.scheduler.get_work(worker="w1")
            self._add("t", status=luigi.scheduler.FAILED)

            self.scheduler.dump()

            restored = luigi.scheduler.Scheduler(task_history_impl=self.history)
            restored._state._state_path = path
            restored.load()

            boundary = restored.history_boundary()
            self.assertEqual(boundary["history_seq"], self.scheduler.history_boundary()["history_seq"])
            self.assertEqual([event["event_name"] for event in boundary["outstanding"]], [luigi.scheduler.FAILED])
            self.assertEqual([event["event_name"] for event in boundary["landed"]], [luigi.scheduler.PENDING, luigi.scheduler.RUNNING])

            self.history.release("finished")
            result = restored.flush_task_history()
            self.assertEqual(result["written"], 1)
            self.assertEqual(result["outstanding"], [])

    def test_snapshot_identity_survives_dump_and_load(self):
        with tempfile.TemporaryDirectory() as folder:
            path = os.path.join(folder, "state.pickle")
            self.scheduler._state._state_path = path
            self._add("t")
            snapshot = self.scheduler.task_snapshot()
            self.scheduler.dump()

            restored = luigi.scheduler.Scheduler(task_history_impl=self.history)
            restored._state._state_path = path
            restored.load()

            reread = restored.get_task_snapshot(snapshot["snapshot_id"])
            self.assertEqual(reread, snapshot)
            self.assertEqual([item["snapshot_id"] for item in restored.list_task_snapshots()], [snapshot["snapshot_id"]])

    def test_legacy_history_still_works(self):
        th = SimpleTaskHistory()
        sch = luigi.scheduler.Scheduler(task_history_impl=th)
        sch.add_task(worker="w1", task_id="legacy")
        sch.get_work(worker="w1")
        sch.add_task(worker="w1", task_id="legacy", status=luigi.scheduler.DONE)
        self.assertEqual([action for action, _ in th.actions], ["scheduled", "started", "finished"])
        snap = sch.task_snapshot()
        self.assertEqual([event["event_name"] for event in snap["events"]], [luigi.scheduler.PENDING, luigi.scheduler.RUNNING, luigi.scheduler.DONE])
        self.assertEqual(snap["outstanding"], [])
