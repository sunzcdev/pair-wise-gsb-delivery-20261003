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

"""Tests for the artifact version aware completion cache."""

import os
import shutil
import tempfile

from helpers import LuigiTestCase

import luigi
from luigi import Event, ExternalTask, LocalTarget, Task
from luigi.scheduler import Scheduler
from luigi.worker import Worker


class CompletionCacheArtifactVersionTest(LuigiTestCase):
    """
    The completion cache remembers the artifact version it saw, so a replaced artifact is
    re-checked while unrelated tasks keep their cached answer.
    """

    def setUp(self):
        super(CompletionCacheArtifactVersionTest, self).setUp()
        self.tmpdir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmpdir, True)
        self.revalidated = []

        @Task.event_handler(Event.COMPLETE_REVALIDATED)
        def collect(task, cached_version, current_version):
            self.revalidated.append((task.task_id, cached_version, current_version))

        self._handler = collect

    def tearDown(self):
        Task.remove_event_handler(Event.COMPLETE_REVALIDATED, self._handler)
        super(CompletionCacheArtifactVersionTest, self).tearDown()

    def _path(self, name):
        return os.path.join(self.tmpdir, name)

    def test_unchanged_artifact_stays_cached(self):
        state = {"calls": 0}
        path = self._path("shared")

        class Shared(Task):
            def output(self):
                return luigi.LocalTarget(path)

            def complete(self):
                state["calls"] += 1
                return self.output().exists()

        with Shared().output().open("w") as f:
            f.write("payload")

        task = Shared()
        cache = {}
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        # the version did not change, so complete() only ran once
        self.assertEqual(state["calls"], 1)
        self.assertEqual(self.revalidated, [])

    def test_replaced_artifact_is_revalidated(self):
        state = {"calls": 0}
        path = self._path("shared")

        class Shared(Task):
            def output(self):
                return luigi.LocalTarget(path)

            def complete(self):
                state["calls"] += 1
                return self.output().exists()

        task = Shared()
        cache = {}
        with task.output().open("w") as f:
            f.write("first")
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        self.assertEqual(state["calls"], 1)

        # replace the shared artifact, the next step must see it
        with task.output().open("w") as f:
            f.write("second")

        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        self.assertEqual(state["calls"], 2)
        self.assertEqual(len(self.revalidated), 1)
        task_id, cached_version, current_version = self.revalidated[0]
        self.assertEqual(task_id, task.task_id)
        self.assertNotEqual(cached_version, current_version)

        # the new version is cached again
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        self.assertEqual(state["calls"], 2)

    def test_removed_artifact_invalidates_only_that_dependency(self):
        state = {"calls": 0}
        shared_path = self._path("shared")
        other_path = self._path("other")

        class Shared(Task):
            def output(self):
                return luigi.LocalTarget(shared_path)

            def complete(self):
                state["calls"] += 1
                return self.output().exists()

        class Other(Task):
            def output(self):
                return luigi.LocalTarget(other_path)

            def complete(self):
                state["calls"] += 1
                return self.output().exists()

        shared = Shared()
        other = Other()
        with shared.output().open("w") as f:
            f.write("shared")
        with other.output().open("w") as f:
            f.write("other")

        cache = {}
        self.assertTrue(luigi.worker.check_complete_cached(shared, cache))
        self.assertTrue(luigi.worker.check_complete_cached(other, cache))
        self.assertEqual(state["calls"], 2)

        # replace the shared artifact and drop the other necessary output
        with shared.output().open("w") as f:
            f.write("shared-replaced")
        other.output().remove()

        # the replaced shared file is re-checked and still complete
        self.assertTrue(luigi.worker.check_complete_cached(shared, cache))
        # the dependency whose necessary output was removed turns incomplete
        self.assertFalse(luigi.worker.check_complete_cached(other, cache))

        # only the affected dependencies were re-checked
        self.assertEqual(state["calls"], 4)
        self.assertEqual([entry[0] for entry in self.revalidated], [shared.task_id, other.task_id])
        self.assertNotIn(other.task_id, cache)

        # once the other output is restored, its next check succeeds again
        with other.output().open("w") as f:
            f.write("other-restored")
        self.assertTrue(luigi.worker.check_complete_cached(other, cache))

    def test_task_without_version_keeps_original_rule(self):
        state = {"calls": 0, "present": True}

        class NoVersion(Task):
            def artifact_version(self):
                return None

            def complete(self):
                state["calls"] += 1
                return state["present"]

        task = NoVersion()
        cache = {}
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        state["present"] = False
        # no comparable version, the original caching rule keeps trusting the cache
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        self.assertEqual(state["calls"], 1)
        self.assertEqual(self.revalidated, [])

    def test_legacy_boolean_cache_entries_are_trusted(self):
        state = {"calls": 0}
        path = self._path("shared")

        class Shared(Task):
            def output(self):
                return luigi.LocalTarget(path)

            def complete(self):
                state["calls"] += 1
                return True

        task = Shared()
        cache = {task.task_id: True}
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        self.assertEqual(state["calls"], 0)

    def test_custom_task_version_is_used(self):
        state = {"calls": 0, "version": "v1", "present": True}

        class Versioned(Task):
            def artifact_version(self):
                return state["version"]

            def complete(self):
                state["calls"] += 1
                return state["present"]

        task = Versioned()
        cache = {}
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        self.assertTrue(luigi.worker.check_complete_cached(task, cache))
        self.assertEqual(state["calls"], 1)

        state["version"] = "v2"
        state["present"] = False
        self.assertFalse(luigi.worker.check_complete_cached(task, cache))
        self.assertEqual(state["calls"], 2)
        self.assertEqual(self.revalidated[-1][1:], ("v1", "v2"))

    def test_two_parents_see_replaced_shared_dependency(self):
        """
        Two parents jointly confirmed one dependency. Before one parent starts its next
        step the artifact is replaced and another required output is removed. Only the
        affected checks are redone, so the parent that lost an input is held back.
        """
        shared_path = self._path("shared")
        extra_path = self._path("extra")
        calls = []

        class Shared(Task):
            def output(self):
                return luigi.LocalTarget(shared_path)

            def complete(self):
                calls.append("shared")
                return self.output().exists()

        class Extra(Task):
            def output(self):
                return luigi.LocalTarget(extra_path)

            def complete(self):
                calls.append("extra")
                return self.output().exists()

        shared, extra = Shared(), Extra()
        with shared.output().open("w") as f:
            f.write("v1")
        with extra.output().open("w") as f:
            f.write("extra")

        cache = {}
        self.assertTrue(luigi.worker.check_complete_cached(shared, cache))
        self.assertTrue(luigi.worker.check_complete_cached(extra, cache))
        self.assertEqual(calls, ["shared", "extra"])

        # replace the shared file and remove the other parent's necessary output
        with shared.output().open("w") as f:
            f.write("v2")
        extra.output().remove()

        # both parent checks are redone, nothing else is re-scanned
        self.assertTrue(luigi.worker.check_complete_cached(shared, cache))
        self.assertFalse(luigi.worker.check_complete_cached(extra, cache))
        self.assertEqual(calls, ["shared", "extra", "shared", "extra"])
        # the re-checks are attributable: the replaced file and the removed output
        self.assertEqual([entry[0] for entry in self.revalidated], [shared.task_id, extra.task_id])

    def test_parent_is_held_back_after_shared_artifact_replacement(self):
        """
        End to end: two parents confirm one shared dependency. Before one of them starts,
        the shared file is replaced and that parent's other required output is removed.
        The replaced dependency is re-checked, the parent that still has its inputs runs and
        the one that lost an output is kept from running.
        """
        paths = {name: self._path(name) for name in ("shared", "extra", "a", "b")}

        class Shared(ExternalTask):
            def output(self):
                return luigi.LocalTarget(paths["shared"])

        class Extra(ExternalTask):
            def output(self):
                return luigi.LocalTarget(paths["extra"])

        class ParentA(Task):
            def requires(self):
                return Shared()

            def output(self):
                return luigi.LocalTarget(paths["a"])

            def run(self):
                with self.output().open("w") as f:
                    f.write("a")

        class ParentB(Task):
            def requires(self):
                return [Shared(), Extra()]

            def output(self):
                return luigi.LocalTarget(paths["b"])

            def run(self):
                with self.output().open("w") as f:
                    f.write("b")

        for name in ("shared", "extra"):
            with luigi.LocalTarget(paths[name]).open("w") as f:
                f.write(name)

        sch = Scheduler(retry_delay=100, remove_delay=1000, worker_disconnect_delay=10, stable_done_cooldown_secs=0)
        with Worker(scheduler=sch, worker_id="cache", cache_task_completion=True) as w:
            a, b = ParentA(), ParentB()
            # both parents confirmed the shared dependency once
            self.assertTrue(w.add(a))
            self.assertTrue(w.add(b))
            self.assertEqual(self.revalidated, [])

            # before the next step starts the shared file is replaced ...
            with luigi.LocalTarget(paths["shared"]).open("w") as f:
                f.write("replaced")
            # ... and one parent loses another necessary output
            luigi.LocalTarget(paths["extra"]).remove()

            # next step: only the affected dependencies are re-checked
            self.assertTrue(w.add(a))
            self.assertTrue(w.add(b))

            w.run()

        # the re-check is visible and attributable
        self.assertIn(Shared().task_id, [entry[0] for entry in self.revalidated])
        self.assertIn(Extra().task_id, [entry[0] for entry in self.revalidated])
        self.assertEqual({task_id for task_id, _, _ in self.revalidated}, {Shared().task_id, Extra().task_id})

        # the parent that still has all inputs ran
        self.assertTrue(a.complete())
        # the parent that lost a necessary output was kept from running
        self.assertFalse(b.complete())
        self.assertFalse(b.output().exists())


class TargetArtifactVersionTest(LuigiTestCase):
    def setUp(self):
        super(TargetArtifactVersionTest, self).setUp()
        self.tmpdir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmpdir, True)

    def test_local_target_version_tracks_replacement(self):
        path = os.path.join(self.tmpdir, "artifact")
        target = LocalTarget(path)
        self.assertEqual(target.artifact_version(), ("missing", path))

        with target.open("w") as f:
            f.write("first")
        version = target.artifact_version()
        self.assertEqual(version[0], "file")
        # an untouched file reports the same version
        self.assertEqual(version, target.artifact_version())

        # replacing the file, as luigi writes atomically, changes the version
        with target.open("w") as f:
            f.write("second")
        self.assertNotEqual(version, target.artifact_version())

        target.remove()
        self.assertEqual(target.artifact_version(), ("missing", path))

    def test_task_version_combines_outputs(self):
        paths = [os.path.join(self.tmpdir, name) for name in ("one", "two")]

        class TwoOutputs(Task):
            def output(self):
                return [LocalTarget(path) for path in paths]

        task = TwoOutputs()
        # missing outputs still describe themselves, so a later removal stays visible
        self.assertEqual([version[0] for version in task.artifact_version()], ["missing", "missing"])

        for path in paths:
            with LocalTarget(path).open("w") as f:
                f.write("data")
        before = task.artifact_version()
        self.assertEqual(len(before), 2)

        with LocalTarget(paths[1]).open("w") as f:
            f.write("changed")
        self.assertNotEqual(before, task.artifact_version())

    def test_task_without_outputs_has_no_version(self):
        class NoOutputs(Task):
            pass

        self.assertIsNone(NoOutputs().artifact_version())

    def test_target_default_version_is_none(self):
        class PlainTarget(luigi.Target):
            def exists(self):
                return True

        self.assertIsNone(PlainTarget().artifact_version())
