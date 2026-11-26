import contextlib
import io
import json
import os
import tempfile
from unittest import TestCase

from pyinfra.api import Config, Inventory, State
from pyinfra.api.plan_reuse import PlanReuseStore
from pyinfra_cli.cli import _load_plan_reuse, _save_plan_reuse
from pyinfra_cli.exceptions import CliError

import pyinfra_cli.prints as prints_module


class TestPlanReuseJsonReport(TestCase):
    def test_plan_reuse_report_is_included_when_a_plan_is_attached(self):
        inventory = Inventory(
            (
                [
                    ("first", {"ssh_hostname": "replacement"}),
                    ("peer", {"ssh_hostname": "same-peer"}),
                ],
                {},
            ),
        )
        state = State(inventory, Config())

        plan = {
            "version": 1,
            "records": {
                "first": {
                    "identity": {
                        "name": "first",
                        "connector": "SSHConnector",
                        "target": {"ssh_hostname": "initial"},
                        "source": "connector",
                    },
                    "facts": {},
                    "steps": {},
                },
                "peer": {
                    "identity": {
                        "name": "peer",
                        "connector": "SSHConnector",
                        "target": {"ssh_hostname": "same-peer"},
                        "source": "connector",
                    },
                    "facts": {},
                    "steps": {"step-hash": {"success": True, "did_change": True}},
                },
            },
        }
        state.set_plan_reuse(PlanReuseStore.from_dict(plan))

        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            prints_module.print_run_json(state, dry=True)

        payload = json.loads(out.getvalue())
        assert "plan_reuse" in payload
        report = payload["plan_reuse"]
        assert report["first"]["reused"] is False
        assert report["first"]["reason"] == "identity_changed"
        assert report["first"]["previous_identity"]["target"] == {"ssh_hostname": "initial"}
        assert report["peer"]["reused"] is True

    def test_no_report_without_a_plan(self):
        inventory = Inventory(([("peer", {"ssh_hostname": "same-peer"})], {}))
        state = State(inventory, Config())

        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            prints_module.print_run_json(state, dry=True)

        assert "plan_reuse" not in json.loads(out.getvalue())


class TestPlanReuseFiles(TestCase):
    def test_load_missing_file_raises(self):
        state = State(Inventory(([("web", {"ssh_hostname": "10.0.0.1"})], {})), Config())

        with self.assertRaises(CliError):
            _load_plan_reuse(state, "no-such-plan.json")

    def test_load_malformed_file_disables_reuse(self):
        state = State(Inventory(([("web", {"ssh_hostname": "10.0.0.1"})], {})), Config())

        with tempfile.TemporaryDirectory() as tmp:
            filename = os.path.join(tmp, "plan.json")
            with open(filename, "w", encoding="utf-8") as f:
                f.write("{not json")

            store = _load_plan_reuse(state, filename)

        assert store.enabled is False

    def test_save_then_load_round_trips(self):
        state = State(Inventory(([("web", {"ssh_hostname": "10.0.0.1"})], {})), Config())

        with tempfile.TemporaryDirectory() as tmp:
            filename = os.path.join(tmp, "plan.json")
            _save_plan_reuse(state, filename)

            store = _load_plan_reuse(state, filename)

        assert store.enabled is True
        assert store.records["web"].identity.target_data == {"ssh_hostname": "10.0.0.1"}
