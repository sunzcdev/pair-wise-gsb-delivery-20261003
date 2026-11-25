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
import pickle
import shutil
import tempfile
import time
from multiprocessing import Process

from helpers import unittest, with_config

import luigi.configuration
import luigi.scheduler
import luigi.server
from luigi.target import FileAlreadyExists
from luigi.task_status import BATCH_RUNNING


class SchedulerIoTest(unittest.TestCase):
    def test_pretty_id_unicode(self):
        scheduler = luigi.scheduler.Scheduler()
        scheduler.add_task(worker="A", task_id="1", params={"foo": "\u2192bar"})
        [task] = list(scheduler._state.get_active_tasks())
        task.pretty_id

    def test_load_old_state(self):
        tasks = {}
        active_workers = {"Worker1": 1e9, "Worker2": time.time()}

        with tempfile.NamedTemporaryFile(delete=True) as fn:
            with open(fn.name, "wb") as fobj:
                state = (tasks, active_workers)
                pickle.dump(state, fobj)

            state = luigi.scheduler.SimpleTaskState(state_path=fn.name)
            state.load()

            self.assertEqual(set(state.get_worker_ids()), {"Worker1", "Worker2"})

    def test_load_broken_state(self):
        with tempfile.NamedTemporaryFile(delete=True) as fn:
            with open(fn.name, "w") as fobj:
                print("b0rk", file=fobj)

            state = luigi.scheduler.SimpleTaskState(state_path=fn.name)
            state.load()  # bad if this crashes

            self.assertEqual(list(state.get_worker_ids()), [])

    @with_config({"scheduler": {"retry_count": "44", "worker_disconnect_delay": "55"}})
    def test_scheduler_with_config(self):
        scheduler = luigi.scheduler.Scheduler()
        self.assertEqual(44, scheduler._config.retry_count)
        self.assertEqual(55, scheduler._config.worker_disconnect_delay)

        # Override
        scheduler = luigi.scheduler.Scheduler(retry_count=66, worker_disconnect_delay=77)
        self.assertEqual(66, scheduler._config.retry_count)
        self.assertEqual(77, scheduler._config.worker_disconnect_delay)

    @with_config({"resources": {"a": "100", "b": "200"}})
    def test_scheduler_with_resources(self):
        scheduler = luigi.scheduler.Scheduler()
        self.assertEqual({"a": 100, "b": 200}, scheduler._resources)

    @with_config({"scheduler": {"record_task_history": "True"}, "task_history": {"db_connection": "sqlite:////none/existing/path/hist.db"}})
    def test_local_scheduler_task_history_status(self):
        ls = luigi.interface._WorkerSchedulerFactory().create_local_scheduler()
        self.assertEqual(False, ls._config.record_task_history)

    def test_load_recovers_tasks_index(self):
        scheduler = luigi.scheduler.Scheduler()
        scheduler.add_task(worker="A", task_id="1")
        scheduler.add_task(worker="B", task_id="2")
        scheduler.add_task(worker="C", task_id="3")
        scheduler.add_task(worker="D", task_id="4")
        self.assertEqual(scheduler.get_work(worker="A")["task_id"], "1")

        with tempfile.NamedTemporaryFile(delete=True) as fn:

            def reload_from_disk(scheduler):
                scheduler._state._state_path = fn.name
                scheduler.dump()
                scheduler = luigi.scheduler.Scheduler()
                scheduler._state._state_path = fn.name
                scheduler.load()
                return scheduler

            scheduler = reload_from_disk(scheduler=scheduler)
            self.assertEqual(scheduler.get_work(worker="B")["task_id"], "2")
            self.assertEqual(scheduler.get_work(worker="C")["task_id"], "3")
            scheduler = reload_from_disk(scheduler=scheduler)
            self.assertEqual(scheduler.get_work(worker="D")["task_id"], "4")

    def test_worker_prune_after_init(self):
        """
        See https://github.com/spotify/luigi/pull/1019
        """
        worker = luigi.scheduler.Worker(123)

        class TmpCfg:
            def __init__(self):
                self.worker_disconnect_delay = 10

        worker.prune(TmpCfg())

    def test_get_empty_retry_policy(self):
        retry_policy = luigi.scheduler._get_empty_retry_policy()
        self.assertEqual(3, len(retry_policy))
        self.assertEqual(["retry_count", "disable_hard_timeout", "disable_window"], list(retry_policy._asdict().keys()))
        self.assertEqual([None, None, None], list(retry_policy._asdict().values()))

    @with_config({"scheduler": {"retry_count": "9", "disable_hard_timeout": "99", "disable_window": "999"}})
    def test_scheduler_get_retry_policy(self):
        s = luigi.scheduler.Scheduler()
        self.assertEqual(luigi.scheduler.RetryPolicy(9, 99, 999), s._config._get_retry_policy())

    @with_config({"scheduler": {"retry_count": "9", "disable_hard_timeout": "99", "disable_window": "999"}})
    def test_generate_retry_policy(self):
        s = luigi.scheduler.Scheduler()

        try:
            s._generate_retry_policy({"inexist_attr": True})
            self.assertFalse(True, "'unexpected keyword argument' error must have been thrown")
        except TypeError:
            self.assertTrue(True)

        retry_policy = s._generate_retry_policy({})
        self.assertEqual(luigi.scheduler.RetryPolicy(9, 99, 999), retry_policy)

        retry_policy = s._generate_retry_policy({"retry_count": 1})
        self.assertEqual(luigi.scheduler.RetryPolicy(1, 99, 999), retry_policy)

        retry_policy = s._generate_retry_policy({"retry_count": 1, "disable_hard_timeout": 11, "disable_window": 111})
        self.assertEqual(luigi.scheduler.RetryPolicy(1, 11, 111), retry_policy)

    @with_config({"scheduler": {"retry_count": "44"}})
    def test_per_task_retry_policy(self):
        cps = luigi.scheduler.Scheduler()

        cps.add_task(worker="test_worker1", task_id="test_task_1", deps=["test_task_2", "test_task_3"])
        tasks = list(cps._state.get_active_tasks())
        self.assertEqual(3, len(tasks))

        tasks = sorted(tasks, key=lambda x: x.id)
        task_1 = tasks[0]
        task_2 = tasks[1]
        task_3 = tasks[2]

        self.assertEqual("test_task_1", task_1.id)
        self.assertEqual("test_task_2", task_2.id)
        self.assertEqual("test_task_3", task_3.id)

        self.assertEqual(luigi.scheduler.RetryPolicy(44, 999999999, 3600), task_1.retry_policy)
        self.assertEqual(luigi.scheduler.RetryPolicy(44, 999999999, 3600), task_2.retry_policy)
        self.assertEqual(luigi.scheduler.RetryPolicy(44, 999999999, 3600), task_3.retry_policy)

        cps._state._tasks = {}
        cps.add_task(
            worker="test_worker2",
            task_id="test_task_4",
            deps=["test_task_5", "test_task_6"],
            retry_policy_dict=luigi.scheduler.RetryPolicy(99, 999, 9999)._asdict(),
        )

        tasks = list(cps._state.get_active_tasks())
        self.assertEqual(3, len(tasks))

        tasks = sorted(tasks, key=lambda x: x.id)
        task_4 = tasks[0]
        task_5 = tasks[1]
        task_6 = tasks[2]

        self.assertEqual("test_task_4", task_4.id)
        self.assertEqual("test_task_5", task_5.id)
        self.assertEqual("test_task_6", task_6.id)

        self.assertEqual(luigi.scheduler.RetryPolicy(99, 999, 9999), task_4.retry_policy)
        self.assertEqual(luigi.scheduler.RetryPolicy(44, 999999999, 3600), task_5.retry_policy)
        self.assertEqual(luigi.scheduler.RetryPolicy(44, 999999999, 3600), task_6.retry_policy)

        cps._state._tasks = {}
        cps.add_task(worker="test_worker3", task_id="test_task_7", deps=["test_task_8", "test_task_9"])
        cps.add_task(worker="test_worker3", task_id="test_task_8", retry_policy_dict=luigi.scheduler.RetryPolicy(99, 999, 9999)._asdict())
        cps.add_task(worker="test_worker3", task_id="test_task_9", retry_policy_dict=luigi.scheduler.RetryPolicy(11, 111, 1111)._asdict())

        tasks = list(cps._state.get_active_tasks())
        self.assertEqual(3, len(tasks))

        tasks = sorted(tasks, key=lambda x: x.id)
        task_7 = tasks[0]
        task_8 = tasks[1]
        task_9 = tasks[2]

        self.assertEqual("test_task_7", task_7.id)
        self.assertEqual("test_task_8", task_8.id)
        self.assertEqual("test_task_9", task_9.id)

        self.assertEqual(luigi.scheduler.RetryPolicy(44, 999999999, 3600), task_7.retry_policy)
        self.assertEqual(luigi.scheduler.RetryPolicy(99, 999, 9999), task_8.retry_policy)
        self.assertEqual(luigi.scheduler.RetryPolicy(11, 111, 1111), task_9.retry_policy)

        # Task 7 which is disable-failures 44 and its has_excessive_failures method returns False under 44
        for i in range(43):
            task_7.add_failure()
        self.assertFalse(task_7.has_excessive_failures())
        task_7.add_failure()
        self.assertTrue(task_7.has_excessive_failures())

        # Task 8 which is disable-failures 99 and its has_excessive_failures method returns False under 44
        for i in range(98):
            task_8.add_failure()
        self.assertFalse(task_8.has_excessive_failures())
        task_8.add_failure()
        self.assertTrue(task_8.has_excessive_failures())

        # Task 9 which is disable-failures 1 and its has_excessive_failures method returns False under 44
        for i in range(10):
            task_9.add_failure()
        self.assertFalse(task_9.has_excessive_failures())
        task_9.add_failure()
        self.assertTrue(task_9.has_excessive_failures())

    @with_config({"scheduler": {"record_task_history": "true"}})
    def test_has_task_history(self):
        cfg = luigi.configuration.get_config()
        with tempfile.NamedTemporaryFile(suffix=".db", delete=True) as fn:
            cfg.set("task_history", "db_connection", "sqlite:///" + fn.name)
            s = luigi.scheduler.Scheduler()
            self.assertTrue(s.has_task_history())

    @with_config({"scheduler": {"record_task_history": "false"}})
    def test_has_no_task_history(self):
        s = luigi.scheduler.Scheduler()
        self.assertFalse(s.has_task_history())

    @with_config({"scheduler": {"pause_enabled": "false"}})
    def test_pause_disabled(self):
        s = luigi.scheduler.Scheduler()
        self.assertFalse(s.is_pause_enabled()["enabled"])
        self.assertFalse(s.is_paused()["paused"])
        s.pause()
        self.assertFalse(s.is_paused()["paused"])

    def test_default_metrics_collector(self):
        from luigi.metrics import MetricsCollector

        s = luigi.scheduler.Scheduler()
        scheduler_state = s._state
        collector = scheduler_state._metrics_collector
        self.assertTrue(isinstance(collector, MetricsCollector))

    @with_config({"scheduler": {"metrics_collector": "datadog"}})
    def test_datadog_metrics_collector(self):
        from luigi.contrib.datadog_metric import DatadogMetricsCollector

        s = luigi.scheduler.Scheduler()
        scheduler_state = s._state
        collector = scheduler_state._metrics_collector
        self.assertTrue(isinstance(collector, DatadogMetricsCollector))

    @with_config({"scheduler": {"metrics_collector": "prometheus"}})
    def test_prometheus_metrics_collector(self):
        from luigi.contrib.prometheus_metric import PrometheusMetricsCollector

        s = luigi.scheduler.Scheduler()
        scheduler_state = s._state
        collector = scheduler_state._metrics_collector
        self.assertTrue(isinstance(collector, PrometheusMetricsCollector))

    @with_config({"scheduler": {"metrics_collector": "custom", "metrics_custom_import": "luigi.contrib.prometheus_metric.PrometheusMetricsCollector"}})
    def test_custom_metrics_collector(self):
        from luigi.contrib.prometheus_metric import PrometheusMetricsCollector

        s = luigi.scheduler.Scheduler()
        scheduler_state = s._state
        collector = scheduler_state._metrics_collector
        self.assertTrue(isinstance(collector, PrometheusMetricsCollector))

    def test_update_priority_with_non_numeric_priority_defaults_to_zero(self):
        s = luigi.scheduler.Scheduler()
        s.add_task(worker="worker", task_id="A")
        task = s._state.get_task("A")
        task.priority = "not_a_number"
        s._update_priority(task, 5, "worker")
        self.assertEqual(task.priority, 5)

    def test_update_priority_cannot_decrease(self):
        s = luigi.scheduler.Scheduler()
        s.add_task(worker="worker", task_id="A", priority=10)
        task = s._state.get_task("A")
        s._update_priority(task, 3, "worker")
        self.assertEqual(task.priority, 10)

    def test_add_task_with_non_numeric_priority_does_not_raise_typeerror(self):
        s = luigi.scheduler.Scheduler()
        s.add_task(worker="worker", task_id="A")
        task = s._state.get_task("A")
        task.priority = "corrupt"
        s.add_task(worker="worker", task_id="A", priority=5)
        self.assertEqual(s._state.get_task("A").priority, 5)


class SchedulerWorkerTest(unittest.TestCase):
    def get_pending_ids(self, worker, state):
        return {task.id for task in worker.get_tasks(state, "PENDING")}

    def test_get_pending_tasks_with_many_done_tasks(self):
        sch = luigi.scheduler.Scheduler()
        sch.add_task(worker="NON_TRIVIAL", task_id="A", resources={"a": 1})
        sch.add_task(worker="TRIVIAL", task_id="B", status="PENDING")
        sch.add_task(worker="TRIVIAL", task_id="C", status="DONE")
        sch.add_task(worker="TRIVIAL", task_id="D", status="DONE")

        scheduler_state = sch._state
        trivial_worker = scheduler_state.get_worker("TRIVIAL")
        self.assertEqual({"B"}, self.get_pending_ids(trivial_worker, scheduler_state))

        non_trivial_worker = scheduler_state.get_worker("NON_TRIVIAL")
        self.assertEqual({"A"}, self.get_pending_ids(non_trivial_worker, scheduler_state))


class FailingOnDoubleRunTask(luigi.Task):
    time_to_check_secs = 1
    time_to_run_secs = 2
    output_dir = luigi.Parameter(default="")

    def __init__(self, *args, **kwargs):
        super(FailingOnDoubleRunTask, self).__init__(*args, **kwargs)
        self.file_name = os.path.join(self.output_dir, "AnyTask")

    def complete(self):
        time.sleep(self.time_to_check_secs)  # e.g., establish connection
        exists = os.path.exists(self.file_name)
        time.sleep(self.time_to_check_secs)  # e.g., close connection
        return exists

    def run(self):
        time.sleep(self.time_to_run_secs)
        if os.path.exists(self.file_name):
            raise FileAlreadyExists(self.file_name)
        open(self.file_name, "w").close()


class StableDoneCooldownSecsTest(unittest.TestCase):
    def setUp(self):
        self.p = tempfile.mkdtemp()

    def tearDown(self):
        shutil.rmtree(self.p)

    def run_task(self):
        return luigi.build([FailingOnDoubleRunTask(output_dir=self.p)], detailed_summary=True, parallel_scheduling=True, parallel_scheduling_processes=2)

    @with_config({"worker": {"keep_alive": "false"}})
    def get_second_run_result_on_double_run(self):
        server_process = Process(target=luigi.server.run)
        process = Process(target=self.run_task)
        try:
            # scheduler is started
            server_process.start()
            # first run is started
            process.start()
            time.sleep(FailingOnDoubleRunTask.time_to_run_secs + FailingOnDoubleRunTask.time_to_check_secs)
            # second run of the same task is started
            second_run_result = self.run_task()
            return second_run_result
        finally:
            process.join(1)
            server_process.terminate()
            server_process.join(1)

    @with_config({"scheduler": {"stable_done_cooldown_secs": "5"}})
    def test_sending_same_task_twice_with_cooldown_does_not_lead_to_double_run(self):
        second_run_result = self.get_second_run_result_on_double_run()
        self.assertEqual(second_run_result.scheduling_succeeded, True)

    @with_config({"scheduler": {"stable_done_cooldown_secs": "0"}})
    def test_sending_same_task_twice_without_cooldown_leads_to_double_run(self):
        second_run_result = self.get_second_run_result_on_double_run()
        self.assertEqual(second_run_result.scheduling_succeeded, False)


class SetBatchIdentityTest(unittest.TestCase):
    """
    Behavior of batch parameters declared as set-like.

    Equivalent targets submitted in a different order must resolve to the same
    active batch (for a given environment), while another environment gets its
    own batch. Members keep reporting under their original serialized task ids,
    and batches persisted by an old scheduler version must remain settleable.
    """

    FAMILY = "SetBatchJob"
    BATCH_PARAM = "targets"
    ENV_PARAM = "env"

    def _scheduler(self, state_path=None, retry_delay=-1):
        scheduler = luigi.scheduler.Scheduler(retry_delay=retry_delay)
        if state_path is not None:
            scheduler._state._state_path = state_path
        return scheduler

    def _register_batcher(self, scheduler, worker="worker-1"):
        scheduler.add_task_batcher(
            worker=worker,
            task_family=self.FAMILY,
            batched_args=[self.BATCH_PARAM],
            set_batched_args=[self.BATCH_PARAM],
            max_batch_size=float("inf"),
        )

    def _add_task(self, scheduler, task_id, targets, environment, worker="worker-1", batchable=True):
        scheduler.add_task(
            worker=worker,
            task_id=task_id,
            family=self.FAMILY,
            params={self.BATCH_PARAM: targets, self.ENV_PARAM: environment},
            status="PENDING",
            batchable=batchable,
        )

    def _task_ids(self, reply):
        return set(reply["batch_task_ids"])

    def test_set_disambiguated_targets_share_one_active_batch(self):
        scheduler = self._scheduler()
        self._register_batcher(scheduler)

        # A batch family carries a set of targets. Two submissions describe the
        # same set in different orders, in the same environment.
        self._add_task(scheduler, "job-ab", "a,b", "prod")
        self._add_task(scheduler, "job-ba", "b,a", "prod")
        # Duplicate submission of the equivalent request.
        self._add_task(scheduler, "job-ab-2", "a,b", "prod")
        # An unrelated member that is still part of the same environment batch.
        self._add_task(scheduler, "job-c", "c", "prod")

        reply = scheduler.get_work(worker="worker-1")
        first_batch_id = reply["batch_id"]
        self.assertEqual({"job-ab", "job-ba", "job-ab-2", "job-c"}, self._task_ids(reply))
        # Results are still reported with each member's original serialized params.
        self.assertEqual(["a,b", "b,a", "a,b", "c"], reply["task_params"][self.BATCH_PARAM])

        # The equivalent request resolves to a single canonical identity, even
        # though the members were serialized in different orders.
        registry = scheduler._state._active_batch_registry
        self.assertEqual([first_batch_id], list(registry.values()))
        [canonical_key] = registry.keys()

        # Re-submitting the equivalent request must not create another batch.
        self._add_task(scheduler, "job-ab-3", "b,a", "prod")
        follow_up = scheduler.get_work(worker="worker-1")
        # The newly submitted member joins the already active batch under its id;
        # the reply lists every member using its own originally serialized params.
        self.assertEqual(first_batch_id, follow_up["batch_id"])
        self.assertEqual(
            {"job-ab", "job-ba", "job-ab-2", "job-c", "job-ab-3"},
            self._task_ids(follow_up),
        )
        self.assertIn("b,a", follow_up["task_params"][self.BATCH_PARAM])
        self.assertEqual([first_batch_id], list(scheduler._state._active_batch_registry.values()))
        self.assertEqual(canonical_key, next(iter(scheduler._state._active_batch_registry)))

    def test_different_environments_never_share_a_batch(self):
        scheduler = self._scheduler()
        self._register_batcher(scheduler)

        self._add_task(scheduler, "prod-ab", "a,b", "prod")
        self._add_task(scheduler, "prod-ba", "b,a", "prod")
        self._add_task(scheduler, "stage-ab", "a,b", "staging")
        self._add_task(scheduler, "stage-ba", "b,a", "staging")

        prod_reply = scheduler.get_work(worker="worker-1")
        self.assertEqual({"prod-ab", "prod-ba"}, self._task_ids(prod_reply))
        prod_batch_id = prod_reply["batch_id"]

        stage_reply = scheduler.get_work(worker="worker-1")
        self.assertEqual({"stage-ab", "stage-ba"}, self._task_ids(stage_reply))
        self.assertNotEqual(prod_batch_id, stage_reply["batch_id"])

        # The two batches have distinct canonical identities.
        self.assertNotEqual(prod_reply["batch_key"], stage_reply["batch_key"])
        self.assertEqual(
            2,
            len(set(scheduler._state._active_batch_registry.values())),
        )

    def test_ordered_batch_parameter_is_not_treated_as_a_set(self):
        scheduler = self._scheduler()
        # Register the targets parameter as an ordinary (ordered) batch parameter.
        scheduler.add_task_batcher(
            worker="worker-1",
            task_family=self.FAMILY,
            batched_args=[self.BATCH_PARAM],
            max_batch_size=float("inf"),
        )
        # Same environment, two members that differ only in the ordering. They can
        # still be combined into a single batch run by the worker.
        self._add_task(scheduler, "job-ab", "a,b", "prod")
        self._add_task(scheduler, "job-ba", "b,a", "prod")

        reply = scheduler.get_work(worker="worker-1")
        self.assertEqual({"job-ab", "job-ba"}, self._task_ids(reply))
        # The serialized values are preserved verbatim (order is significant).
        self.assertEqual(["a,b", "b,a"], reply["task_params"][self.BATCH_PARAM])

    def test_failure_retry_keeps_finished_members_with_their_worker(self):
        scheduler = self._scheduler()
        self._register_batcher(scheduler)

        self._add_task(scheduler, "job-a", "a", "prod")
        self._add_task(scheduler, "job-b", "b", "prod")

        reply = scheduler.get_work(worker="worker-1")
        batch_id = reply["batch_id"]
        self.assertEqual({"job-a", "job-b"}, self._task_ids(reply))

        # One member fails; the whole batch settles and goes back to PENDING for retry.
        scheduler.add_task(worker="worker-1", task_id="job-a", family=self.FAMILY, status="FAILED", batch_id=batch_id)
        self.assertEqual("FAILED", scheduler._state.get_task("job-a").status)
        self.assertEqual("FAILED", scheduler._state.get_task("job-b").status)
        self.assertIsNone(scheduler._state.get_task("job-a").batch_id)
        # Once the batch has settled, its identity is free again.
        self.assertEqual({}, scheduler._state._active_batch_registry)

        # Simulate the retry delay expiring and re-submit the same request.
        for task_id, target in (("job-a", "a"), ("job-b", "b")):
            scheduler.add_task(
                worker="worker-1",
                task_id=task_id,
                family=self.FAMILY,
                params={self.BATCH_PARAM: target, self.ENV_PARAM: "prod"},
                status="PENDING",
                batchable=True,
            )

        retry_reply = scheduler.get_work(worker="worker-1")
        # A brand new batch is created for the retry.
        self.assertIn("batch_id", retry_reply)
        self.assertEqual({"job-a", "job-b"}, self._task_ids(retry_reply))

        # job-b now completes successfully and must not be re-dispatched to another worker.
        scheduler.add_task(worker="worker-1", task_id="job-b", family=self.FAMILY, status="DONE", batch_id=retry_reply["batch_id"])
        self.assertEqual("DONE", scheduler._state.get_task("job-b").status)

        # Another worker asking for the still-pending target only gets that member;
        # the completed member is never handed over.
        self._register_batcher(scheduler, worker="worker-2")
        self._add_task(scheduler, "job-a-c", "a", "prod", worker="worker-2")
        other_reply = scheduler.get_work(worker="worker-2")
        # Only a single pending member exists, so no batch is formed and the
        # completed member is never handed over.
        self.assertNotIn("batch_id", other_reply)
        self.assertEqual("job-a-c", other_reply["task_id"])
        self.assertEqual("DONE", scheduler._state.get_task("job-b").status)

    def _build_legacy_archive(self, state_path):
        """
        Write a state file in the pre-feature format:

        * batchers stored as plain (batched_args, max_batch_size) tuples
        * Task objects without a ``batch_key`` attribute
        * no active-batch registry (3-tuple state)
        """
        tasks = {}
        retry_policy = luigi.scheduler.RetryPolicy(999999999, 999999999, 3600)
        for task_id, targets in (("legacy-a", "a"), ("legacy-b", "b")):
            task = luigi.scheduler.Task(
                task_id=task_id,
                status=BATCH_RUNNING,
                deps=None,
                family=self.FAMILY,
                params={self.BATCH_PARAM: targets, self.ENV_PARAM: "legacy-env"},
                retry_policy=retry_policy,
            )
            task.batchable = True
            task.batch_id = "legacy-batch-id"
            task.worker_running = "legacy-worker"
            # Emulate the old class: no batch_key attribute at all.
            del task.batch_key
            tasks[task_id] = task

        active_workers = {"legacy-worker": luigi.scheduler.Worker("legacy-worker")}
        task_batchers = {
            "legacy-worker": {
                self.FAMILY: ((self.BATCH_PARAM,), float("inf")),
            }
        }
        with open(state_path, "wb") as fobj:
            pickle.dump((tasks, active_workers, task_batchers), fobj)

    def test_legacy_running_batch_can_still_be_settled(self):
        state_dir = tempfile.mkdtemp()
        try:
            state_path = os.path.join(state_dir, "state.pickle")
            self._build_legacy_archive(state_path)

            scheduler = self._scheduler(state_path=state_path)
            scheduler.load()

            # The old batcher tuple is upgraded transparently.
            registration = scheduler._state.get_batcher("legacy-worker", self.FAMILY)
            self.assertEqual((self.BATCH_PARAM,), registration.batch_param_names)

            # The old batch is registered under its own legacy identity and cannot
            # be claimed by a newly created batch of another environment.
            self.assertEqual("legacy-batch-id", scheduler._state.get_active_batch(("legacy", "legacy-batch-id")))
            legacy_tasks = scheduler._state.get_batch_running_tasks("legacy-batch-id")
            self.assertEqual({"legacy-a", "legacy-b"}, {task.id for task in legacy_tasks})

            # A new environment submitting the same targets must get its own batch.
            self._register_batcher(scheduler)
            self._add_task(scheduler, "new-a", "a", "new-env")
            self._add_task(scheduler, "new-b", "b", "new-env")
            new_reply = scheduler.get_work(worker="worker-1")
            self.assertIn("batch_id", new_reply)
            self.assertNotEqual("legacy-batch-id", new_reply["batch_id"])
            self.assertEqual({"new-a", "new-b"}, self._task_ids(new_reply))

            # The legacy worker settles its own batch using the original batch id.
            scheduler.add_task(
                worker="legacy-worker",
                task_id="legacy-a",
                family=self.FAMILY,
                status="DONE",
                batch_id="legacy-batch-id",
            )
            self.assertEqual("DONE", scheduler._state.get_task("legacy-a").status)
            self.assertEqual("DONE", scheduler._state.get_task("legacy-b").status)
            self.assertIsNone(scheduler._state.get_task("legacy-a").batch_id)
            self.assertNotIn(("legacy", "legacy-batch-id"), scheduler._state._active_batch_registry)

            # And it survives a dump/load round trip with the new on-disk format.
            scheduler.dump()
            reloaded = self._scheduler(state_path=state_path)
            reloaded.load()
            self.assertEqual("DONE", reloaded._state.get_task("legacy-a").status)
            self.assertEqual("DONE", reloaded._state.get_task("legacy-b").status)
            # The new environment's batch is untouched and still in flight.
            self.assertEqual("BATCH_RUNNING", reloaded._state.get_task("new-a").status)
            self.assertEqual(
                new_reply["batch_id"],
                reloaded._state.get_task("new-a").batch_id,
            )
        finally:
            shutil.rmtree(state_dir)

    def test_restart_preserves_active_batch_identity_across_failure(self):
        state_dir = tempfile.mkdtemp()
        try:
            state_path = os.path.join(state_dir, "state.pickle")
            scheduler = self._scheduler(state_path=state_path)
            self._register_batcher(scheduler)

            self._add_task(scheduler, "job-a", "a", "prod")
            self._add_task(scheduler, "job-b", "b", "prod")
            reply = scheduler.get_work(worker="worker-1")
            batch_id = reply["batch_id"]

            # One member fails, state is persisted, then the scheduler restarts.
            scheduler.add_task(worker="worker-1", task_id="job-a", family=self.FAMILY, status="FAILED", batch_id=batch_id)
            scheduler.dump()

            restarted = self._scheduler(state_path=state_path)
            restarted.load()
            self.assertEqual("FAILED", restarted._state.get_task("job-a").status)
            self.assertEqual("FAILED", restarted._state.get_task("job-b").status)

            # Re-issue the identical request after the failure.
            self._register_batcher(restarted)
            for task_id, target in (("job-a", "a"), ("job-b", "b")):
                restarted.add_task(
                    worker="worker-1",
                    task_id=task_id,
                    family=self.FAMILY,
                    params={self.BATCH_PARAM: target, self.ENV_PARAM: "prod"},
                    status="PENDING",
                    batchable=True,
                )
            retry_reply = restarted.get_work(worker="worker-1")
            self.assertEqual({"job-a", "job-b"}, self._task_ids(retry_reply))
            # The failed batch had been unregistered when it settled, so the retry
            # is registered as a fresh active batch rather than reviving the old one.
            registry = restarted._state._active_batch_registry
            self.assertEqual(1, len(registry))
            self.assertEqual(retry_reply["batch_id"], next(iter(registry.values())))
            # Both members were FAILED and are now BATCH_RUNNING again as a retry.
            for task_id in ("job-a", "job-b"):
                self.assertEqual("BATCH_RUNNING", restarted._state.get_task(task_id).status)
        finally:
            shutil.rmtree(state_dir)
