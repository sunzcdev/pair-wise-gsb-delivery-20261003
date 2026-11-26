"""
Machine-readable before/after comparison for a maintenance run where one
operation hash covers multiple hosts with different histories:

+ ``@fake/web-retry`` fails the first attempt and writes on a retry
+ ``@fake/web-skip`` never runs because its ``_if`` precondition is false

The report must show which host really changed, keep retry and skip histories
out of the write count, and remain a superset of the original (baseline) format.
"""

import json
from os import path
from unittest import TestCase

from pyinfra.api import Config, HiddenValue, State, StringCommand
from pyinfra.api.connect import connect_all
from pyinfra.api.operation import add_op
from pyinfra.api.operations import run_ops
from pyinfra.api.state import StateStage
from pyinfra.operations import server
from pyinfra_cli.prints import build_results_json

from ..util import make_inventory

NO_DELAY = {"fake_delay": 0, "fake_delay_jitter": 0}
FIXTURE_DIR = path.join(path.dirname(__file__), "fixtures")


class FlakyResponse(dict):
    """fake_responses entry that fails once, then succeeds (dict so the fake
    connector reads its ``success``/``stderr`` keys)."""

    def __init__(self) -> None:
        super().__init__()
        self.calls = 0

    def get(self, key, default=None):
        if key == "success":
            self.calls += 1
            return self.calls > 1
        if key == "stderr":
            return ["boom"] if self.calls == 1 else []
        if key == "stdout":
            return []
        return default


def _build_maintenance_state(with_secret: bool = False):
    flaky = FlakyResponse()
    inventory = make_inventory(
        hosts=(
            ("@fake/web-retry", {**NO_DELAY, "fake_responses": {"write-config": flaky}}),
            ("@fake/web-skip", NO_DELAY),
        ),
    )
    state = State(inventory, Config())
    state.current_stage = StateStage.Prepare
    connect_all(state)

    retry_host = inventory.get_host("@fake/web-retry")
    skip_host = inventory.get_host("@fake/web-skip")

    if with_secret:
        # The secret travels inside a StringCommand so its masked representation is
        # what ends up in any recorded command text.
        command = [StringCommand("echo token", HiddenValue("s3cr3t"))]
    else:
        command = ["echo write-config"]

    # Same name/args/position on both hosts -> a single shared op_hash.
    add_op(
        state,
        server.shell,
        command,
        name="maintenance write",
        _retries=1,
        _retry_delay=0,
        host=retry_host,
    )
    add_op(
        state,
        server.shell,
        command,
        name="maintenance write",
        _retries=1,
        _retry_delay=0,
        host=skip_host,
        _if=lambda: False,
    )

    run_ops(state)
    return state, inventory


class TestMaintenanceComparison(TestCase):
    def setUp(self):
        self.state, self.inventory = _build_maintenance_state()
        self.results = build_results_json(self.state)
        self.op = self.results["operations"][0]

    def test_single_op_hash_covers_both_hosts(self):
        assert len(self.results["operations"]) == 1
        assert len(self.state.get_op_order()) == 1
        assert self.op["hosts"] == 2
        records = {record["host"]: record for record in self.op["host_results"]}
        assert set(records) == {"@fake/web-retry", "@fake/web-skip"}

    def test_retrying_hosts_final_result_is_verified_change(self):
        record = next(
            record
            for record in self.op["host_results"]
            if record["host"] == "@fake/web-retry"
        )
        # The last verified attempt is the successful write.
        assert record["final_result"] == "changed"
        assert record["changed"] is True
        assert record["retried"] is True
        assert record["retry_attempts"] == 1
        assert record["skip_reason"] is None

        # Both attempts are preserved, failure first, success after.
        assert [attempt["attempt"] for attempt in record["attempts"]] == [0, 1]
        assert [attempt["success"] for attempt in record["attempts"]] == [False, True]
        assert record["attempts"][0]["stderr_lines"] == ["boom"]
        assert record["final_attempt"]["success"] is True
        assert record["final_attempt"]["attempt"] == 1

    def test_skipped_host_never_ran_and_has_no_attempt_history(self):
        record = next(
            record
            for record in self.op["host_results"]
            if record["host"] == "@fake/web-skip"
        )
        assert record["final_result"] == "skipped"
        assert record["changed"] is False
        assert record["skip_reason"] == "precondition"
        assert record["retried"] is False
        assert record["attempts"] == []
        assert record["final_attempt"] is None

    def test_only_the_written_host_counts_as_a_change(self):
        # Retried and skipped histories must not inflate the write count.
        assert self.op["changed"] == ["@fake/web-retry"]
        assert self.op["write_count"] == 1
        assert self.op["retried"] == ["@fake/web-retry"]
        assert self.op["skipped"] == ["@fake/web-skip"]

        totals = self.results["totals"]
        assert totals["write_count"] == 1
        assert totals["changed"] == 1
        assert totals["retried"] == 1
        assert totals["skipped"] == 1
        # The old counters stay consistent with the new ones.
        assert totals["success"] == 1
        assert totals["no_change"] == 0
        assert totals["error"] == 0

        comparison = self.results["comparison"]
        assert comparison == {
            "changed": ["@fake/web-retry"],
            "skipped": ["@fake/web-skip"],
            "write_count": 1,
        }

    def test_commands_follow_the_masking_convention(self):
        state, _ = _build_maintenance_state(with_secret=True)
        op = build_results_json(state)["operations"][0]
        retry_record = next(
            record for record in op["host_results"] if record["host"] == "@fake/web-retry"
        )
        for attempt in retry_record["attempts"]:
            commands = "\n".join(attempt["commands"])
            assert "s3cr3t" not in commands
            assert "*MASKED*" in commands
        # The fake connector itself only ever saw the masked text.
        retry_host = state.inventory.get_host("@fake/web-retry")
        for recorded in retry_host.connector.executed_commands:
            assert "s3cr3t" not in recorded

    def test_payload_is_json_serialisable(self):
        # round-trip through json like the CLI --json printer does
        encoded = json.dumps(self.results)
        assert json.loads(encoded) == self.results

    def test_old_format_baseline_remains_a_subset(self):
        with open(path.join(FIXTURE_DIR, "results_baseline_old_format.json")) as f:
            baseline = json.load(f)

        # Top-level: every old key/value is still present and unchanged; new keys
        # are additive.
        for key in ("totals", "failed_hosts"):
            if key == "totals":
                for old_key, old_value in baseline[key].items():
                    assert self.results[key][old_key] == old_value
            else:
                assert self.results[key] == baseline[key]

        # Per operation: the old fields survive verbatim alongside the new ones.
        baseline_op = baseline["operations"][0]
        for key, value in baseline_op.items():
            if key == "op_hash":
                # The hash is content-derived; only its presence and shape are
                # part of the stable contract.
                assert isinstance(self.op[key], str) and len(self.op[key]) == len(value)
                continue
            assert self.op[key] == value
        assert {"host_results", "skipped", "retried", "changed", "write_count"} <= set(
            self.op,
        )

        # A consumer written against the old schema can still classify every host
        # without knowing about the new fields.
        assert set(self.op["success"]) == set(baseline_op["success"])
        assert self.op["no_change"] == baseline_op["no_change"]
        assert self.op["error"] == baseline_op["error"]
