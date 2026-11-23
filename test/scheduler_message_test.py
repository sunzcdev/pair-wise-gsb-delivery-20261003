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
import time

from helpers import LuigiTestCase, RunOnceTask

import luigi
import luigi.scheduler
import luigi.worker
from luigi.scheduler import (
    SCHEDULER_MESSAGE_DISPATCHED,
    SCHEDULER_MESSAGE_EXPIRED,
    SCHEDULER_MESSAGE_PENDING,
    SCHEDULER_MESSAGE_RESPONDED,
)
from luigi.task_status import DONE, FAILED, PENDING, RUNNING


def fast_worker(scheduler, **kwargs):
    kwargs.setdefault("ping_interval", 0.5)
    kwargs.setdefault("force_multiprocessing", True)
    return luigi.worker.Worker(scheduler=scheduler, **kwargs)


class WriteMessageToFile(luigi.Task):
    path = luigi.Parameter()

    accepts_messages = True

    def output(self):
        return luigi.LocalTarget(self.path)

    def run(self):
        msg = ""

        time.sleep(1)
        if not self.scheduler_messages.empty():
            msg = self.scheduler_messages.get().content

        with self.output().open("w") as f:
            f.write(msg + "\n")


class SchedulerMessageTest(LuigiTestCase):
    def test_scheduler_methods(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        sch.add_task(task_id="foo-task", worker="foo-worker")

        res = sch.send_scheduler_message("foo-worker", "foo-task", "message content")
        message_id = res["message_id"]
        self.assertTrue(len(message_id) > 0)
        self.assertIn("-", message_id)

        sch.add_scheduler_message_response("foo-task", message_id, "message response")
        res = sch.get_scheduler_message_response("foo-task", message_id)
        response = res["response"]
        self.assertEqual(response, "message response")

    def test_receive_messsage(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        with fast_worker(sch) as w:
            with tempfile.NamedTemporaryFile() as tmp:
                if os.path.exists(tmp.name):
                    os.remove(tmp.name)

                task = WriteMessageToFile(path=tmp.name)
                w.add(task)

                sch.send_scheduler_message(w._id, task.task_id, "test")
                w.run()

                self.assertTrue(os.path.exists(tmp.name))
                with open(tmp.name, "r") as f:
                    self.assertEqual(str(f.read()).strip(), "test")

    def test_receive_messages_disabled(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        with fast_worker(sch, force_multiprocessing=False) as w:

            class MyTask(RunOnceTask):
                def run(self):
                    self.had_queue = self.scheduler_messages is not None
                    super(MyTask, self).run()

            task = MyTask()
            w.add(task)

            sch.send_scheduler_message(w._id, task.task_id, "test")
            w.run()

            self.assertFalse(task.had_queue)

    def test_send_messages_disabled(self):
        sch = luigi.scheduler.Scheduler(send_messages=False)
        with fast_worker(sch) as w:
            with tempfile.NamedTemporaryFile() as tmp:
                if os.path.exists(tmp.name):
                    os.remove(tmp.name)

                task = WriteMessageToFile(path=tmp.name)
                w.add(task)

                sch.send_scheduler_message(w._id, task.task_id, "test")
                w.run()

                self.assertTrue(os.path.exists(tmp.name))
                with open(tmp.name, "r") as f:
                    self.assertEqual(str(f.read()).strip(), "")


class SchedulerMessageLifecycleTest(LuigiTestCase):
    def _start_task(self, sch, task_id="t", worker="w"):
        sch.add_task(task_id=task_id, worker=worker, status=PENDING, accepts_messages=True)
        sch.add_task(task_id=task_id, worker=worker, status=RUNNING, accepts_messages=True)
        return sch._state.get_task(task_id)

    def test_new_run_gets_fresh_run_id_and_expires_open_messages(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        task = self._start_task(sch)
        first_run_id = task.run_id
        self.assertIsNotNone(first_run_id)

        res = sch.send_scheduler_message("w", "t", "slow down")
        first_message_id = res["message_id"]

        # First run fails without answering.
        sch.add_task(task_id="t", worker="w", status=FAILED, accepts_messages=True)
        self.assertEqual(sch.get_scheduler_message_status("t", first_message_id)["status"], SCHEDULER_MESSAGE_EXPIRED)

        # The task is retried with the same task_id but a new run.
        sch.add_task(task_id="t", worker="w", status=PENDING, accepts_messages=True)
        task = self._start_task(sch)
        self.assertNotEqual(task.run_id, first_run_id)

        # The old message stays expired, it must not become pending again.
        self.assertEqual(sch.get_scheduler_message_status("t", first_message_id)["status"], SCHEDULER_MESSAGE_EXPIRED)

        # A message sent during the second run belongs to the second run.
        res = sch.send_scheduler_message("w", "t", "slow down again")
        second_message_id = res["message_id"]
        second_record = task.get_scheduler_message(second_message_id)
        self.assertEqual(second_record.run_id, task.run_id)
        self.assertNotEqual(second_record.run_id, first_run_id)
        self.assertEqual(second_record.status, SCHEDULER_MESSAGE_PENDING)

    def test_two_messages_keep_distinct_status(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        self._start_task(sch)

        first = sch.send_scheduler_message("w", "t", "first")["message_id"]
        sch.add_task(task_id="t", worker="w", status=FAILED, accepts_messages=True)

        sch.add_task(task_id="t", worker="w", status=PENDING, accepts_messages=True)
        self._start_task(sch)
        second = sch.send_scheduler_message("w", "t", "second")["message_id"]

        self.assertEqual(sch.get_scheduler_message_status("t", first)["status"], SCHEDULER_MESSAGE_EXPIRED)
        self.assertEqual(sch.get_scheduler_message_status("t", second)["status"], SCHEDULER_MESSAGE_PENDING)

        # Admin can list both messages and tell their run ownership apart.
        messages = sch.get_scheduler_messages("t")["messages"]
        self.assertEqual(len(messages), 2)
        by_id = {m["message_id"]: m for m in messages}
        self.assertEqual(by_id[first]["status"], SCHEDULER_MESSAGE_EXPIRED)
        self.assertEqual(by_id[second]["status"], SCHEDULER_MESSAGE_PENDING)
        self.assertNotEqual(by_id[first]["run_id"], by_id[second]["run_id"])

    def test_dispatch_marks_message_dispatched(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        self._start_task(sch)
        message_id = sch.send_scheduler_message("w", "t", "content")["message_id"]

        # The worker fetches its rpc messages through ping.
        sch.ping(worker="w")
        self.assertEqual(sch.get_scheduler_message_status("t", message_id)["status"], SCHEDULER_MESSAGE_DISPATCHED)

    def test_response_lifecycle_and_consumption(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        self._start_task(sch)
        message_id = sch.send_scheduler_message("w", "t", "content")["message_id"]

        sch.ping(worker="w")
        sch.add_scheduler_message_response("t", message_id, "the response")
        self.assertEqual(sch.get_scheduler_message_status("t", message_id)["status"], SCHEDULER_MESSAGE_RESPONDED)

        # First read consumes the response (legacy pop behaviour).
        res = sch.get_scheduler_message_response("t", message_id)
        self.assertEqual(res["response"], "the response")
        self.assertEqual(res["status"], SCHEDULER_MESSAGE_RESPONDED)
        res = sch.get_scheduler_message_response("t", message_id)
        self.assertIsNone(res["response"])

    def test_waiting_on_expired_message_does_not_block_forever(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        self._start_task(sch)
        message_id = sch.send_scheduler_message("w", "t", "content")["message_id"]

        # No response ever comes; the run ends.
        sch.add_task(task_id="t", worker="w", status=FAILED, accepts_messages=True)
        res = sch.get_scheduler_message_response("t", message_id)
        self.assertIsNone(res["response"])
        self.assertEqual(res["status"], SCHEDULER_MESSAGE_EXPIRED)

    def test_late_response_after_expiry_is_recorded_but_not_reopened(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        self._start_task(sch)
        message_id = sch.send_scheduler_message("w", "t", "content")["message_id"]

        sch.add_task(task_id="t", worker="w", status=FAILED, accepts_messages=True)
        # A zombie worker replies after the run already ended.
        sch.add_scheduler_message_response("t", message_id, "late")
        record = sch._state.get_task("t").get_scheduler_message(message_id)
        self.assertEqual(record.status, SCHEDULER_MESSAGE_EXPIRED)
        self.assertEqual(record.response, "late")

    def test_successful_run_expires_unanswered_messages(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        task = self._start_task(sch)
        message_id = sch.send_scheduler_message("w", "t", "content")["message_id"]

        sch.add_task(task_id="t", worker="w", status=DONE, accepts_messages=True)
        self.assertEqual(task.status, DONE)
        self.assertEqual(sch.get_scheduler_message_status("t", message_id)["status"], SCHEDULER_MESSAGE_EXPIRED)

    def test_unknown_message_and_task(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        self.assertIsNone(sch.get_scheduler_message_status("t", "missing")["status"])
        self.assertIsNone(sch.get_scheduler_message_response("t", "missing")["response"])
        self.assertEqual(sch.get_scheduler_messages("t")["messages"], [])

    def test_serialized_task_carries_run_id_and_message_summary(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        self._start_task(sch)
        first = sch.send_scheduler_message("w", "t", "one")["message_id"]
        sch.ping(worker="w")
        second = sch.send_scheduler_message("w", "t", "two")["message_id"]

        serialized = sch._serialize_task("t")
        self.assertIsNotNone(serialized["run_id"])
        self.assertEqual(serialized["scheduler_messages"][SCHEDULER_MESSAGE_DISPATCHED], 1)
        self.assertEqual(serialized["scheduler_messages"][SCHEDULER_MESSAGE_PENDING], 1)

        sch.add_scheduler_message_response("t", second, "ok")
        serialized = sch._serialize_task("t")
        self.assertEqual(serialized["scheduler_messages"][SCHEDULER_MESSAGE_RESPONDED], 1)
        self.assertEqual(serialized["scheduler_messages"][SCHEDULER_MESSAGE_DISPATCHED], 1)

    def test_rpc_message_carries_run_id(self):
        sch = luigi.scheduler.Scheduler(send_messages=True)
        task = self._start_task(sch)
        sch.send_scheduler_message("w", "t", "content")
        rpc_messages = sch.ping(worker="w")["rpc_messages"]
        self.assertEqual(len(rpc_messages), 1)
        self.assertEqual(rpc_messages[0]["kwargs"]["run_id"], task.run_id)


class SchedulerMessageWorkerIsolationTest(LuigiTestCase):
    def test_worker_discards_message_from_previous_run(self):
        import multiprocessing

        sch = luigi.scheduler.Scheduler(send_messages=True)
        sch.add_task(task_id="t", worker="w", status=PENDING, accepts_messages=True)
        sch.add_task(task_id="t", worker="w", status=RUNNING, accepts_messages=True)
        first_run_id = sch._state.get_task("t").run_id

        w = luigi.worker.Worker(scheduler=sch, worker_id="w", ping_interval=0.5, force_multiprocessing=False)
        # Simulate a stale rpc message queued for the previous run while the
        # worker is now executing a new run of the same task_id.
        reporter = luigi.worker.TaskStatusReporter(sch, "t", "w", multiprocessing.Queue(), run_id="new-run-id")

        class FakeTaskProcess:
            def __init__(self, status_reporter):
                self.status_reporter = status_reporter

            def is_alive(self):
                return False

        w._running_tasks["t"] = FakeTaskProcess(reporter)
        try:
            # A message belonging to the old run must be dropped.
            w.dispatch_scheduler_message(task_id="t", message_id="old", content="old", run_id=first_run_id)
            self.assertTrue(reporter.scheduler_messages.empty())

            # A message belonging to the current run is delivered.
            w.dispatch_scheduler_message(task_id="t", message_id="new", content="new", run_id="new-run-id")
            delivered = reporter.scheduler_messages.get()
            self.assertEqual(delivered.content, "new")
            self.assertEqual(delivered.run_id, "new-run-id")
        finally:
            w._running_tasks.clear()


class SchedulerMessageStateCompatibilityTest(LuigiTestCase):
    def test_old_pickled_task_gets_message_attributes(self):
        import pickle

        from luigi.scheduler import Task

        # State written by an older release has neither run_id nor
        # scheduler_messages, only the legacy responses mapping.
        old = Task("old", RUNNING, None)
        old_dict = {key: value for key, value in old.__dict__.items() if key not in ("run_id", "scheduler_messages")}
        old_dict["scheduler_message_responses"] = {"m1": "r1"}

        restored = Task.__new__(Task)
        restored.__setstate__(pickle.loads(pickle.dumps(old_dict)))

        self.assertIsNone(restored.run_id)
        self.assertEqual(restored.scheduler_messages, {})
        # A response stored the old way is still readable.
        sch = luigi.scheduler.Scheduler(send_messages=True)
        sch._state._tasks["old"] = restored
        sch._state._status_tasks[RUNNING]["old"] = restored
        res = sch.get_scheduler_message_response("old", "m1")
        self.assertEqual(res["response"], "r1")
        self.assertIsNone(res["status"])

    def test_new_task_round_trips_through_pickle(self):
        import pickle

        from luigi.scheduler import Task

        task = Task("t", PENDING, None)
        run_id = task.start_new_run()
        restored = pickle.loads(pickle.dumps(task))
        self.assertEqual(restored.run_id, run_id)
        self.assertEqual(restored.scheduler_messages, {})

