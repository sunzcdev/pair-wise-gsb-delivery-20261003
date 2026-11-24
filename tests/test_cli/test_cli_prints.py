import re
from datetime import datetime
from pathlib import PurePosixPath
from unittest import TestCase
from unittest.mock import patch

from rich.console import Console

from pyinfra.api import Config, State
from pyinfra.api.connect import connect_all
from pyinfra.api.operation import add_op
from pyinfra.api.operations import run_ops
from pyinfra.api.state import StateStage
from pyinfra.operations import server

import pyinfra_cli.prints as prints_module
from pyinfra_cli.console import console
from pyinfra_cli.prints import (
    _display_width,
    _format_host_data,
    _scalar_style,
    build_results_json,
    print_inventory,
    print_results,
    print_rows,
)

from ..paramiko_util import FakeBuffer, FakeChannel, PatchSSHTestCase
from ..util import make_inventory


def _rows(*rows: list[str]) -> list[str]:
    output: list[str] = []
    print_rows([(output.append, row) for row in rows])
    return output


def test_display_width_counts_terminal_cells():
    assert _display_width("ascii") == 5
    # East Asian wide and fullwidth characters take two cells each
    assert _display_width("中文") == 4
    assert _display_width("ＡＢＣ") == 6
    # A skin tone modifier and a combining sound mark add no cells
    # (\u304b\u3099 is ka + combining voiced mark, the NFD form of ga)
    assert _display_width("👍🏽") == 2
    assert _display_width("\u304b\u3099") == 2
    # ANSI codes are not printed, so they take no cells
    assert _display_width("\033[1mhello\033[0m") == 5


def test_print_rows_ascii_table_is_unchanged():
    assert _rows(["Operation", "Hosts"], ["short", "1"]) == [
        "Operation   Hosts   ",
        "short       1       ",
    ]


def test_print_rows_pads_cjk_by_display_width():
    # The example from the issue: the CJK row must not push the next column
    # right of the header.
    assert _rows(["Operation", "Hosts"], ["随便输出一些中文试试", "1"]) == [
        "Operation" + " " * 14 + "Hosts   ",
        "随便输出一些中文试试" + " " * 3 + "1       ",
    ]


def test_print_rows_pads_coloured_cjk_in_a_later_column():
    green_ok = "\033[32m成功\033[0m"
    assert _rows(["Name", "Result", "Hosts"], ["a", green_ok, "1"], ["b", "ok", "2"]) == [
        "Name   Result   Hosts   ",
        "a      " + green_ok + " " * 5 + "1       ",
        "b      ok       2       ",
    ]


def test_print_rows_does_not_widen_zero_width_characters():
    # A skin tone modifier and a combining mark are drawn inside the
    # character before them, so they must not add padding.
    assert _rows(["Operation", "Hosts"], ["echo 👍🏽", "1"], ["\u304b\u3099", "2"]) == [
        "Operation   Hosts   ",
        "echo 👍🏽" + " " * 5 + "1       ",
        "\u304b\u3099" + " " * 10 + "2       ",
    ]


def _render_inventory(host_data: dict) -> str:
    inventory = make_inventory(hosts=(("somehost", host_data),))
    state = State(inventory, Config())
    # Use a wide, fixed-width console so the table is never truncated (the shared
    # console's width varies by platform/terminal, which would clip cell content).
    wide_console = Console(width=200, force_terminal=False, highlight=False)
    with patch.object(prints_module, "console", wide_console):
        with wide_console.capture() as capture:
            print_inventory(state)
    return capture.get()


class TestPrintInventory(TestCase):
    def test_scalars_render_as_flat_key_value_lines(self):
        output = _render_inventory({"role": "web", "port": 80, "enabled": True})

        assert "role: web" in output
        assert "port: 80" in output
        assert "enabled: True" in output
        # Scalars must NOT be dumped as JSON (no quoted keys/values).
        assert '"role"' not in output
        assert '"web"' not in output

    def test_nested_values_render_as_json(self):
        output = _render_inventory({"tags": ["a", "b"], "meta": {"cpu": 4}})

        # Header line for the key, then indented JSON for the value.
        assert "tags:" in output
        assert '"a"' in output and '"b"' in output
        assert "meta:" in output
        assert '"cpu": 4' in output

    def test_non_json_scalars_render_via_str(self):
        created = datetime(2021, 8, 14, 10, 30)
        # PurePosixPath keeps str() stable across platforms (WindowsPath would
        # render with backslashes).
        path = PurePosixPath("/opt/app")
        output = _render_inventory({"created": created, "path": path})

        assert f"created: {created}" in output
        assert f"path: {path}" in output

    def test_re_pattern_does_not_crash(self):
        # Regression: a compiled regex (as a nested dict key) previously crashed
        # the whole `debug-inventory` command trying to JSON-encode it.
        output = _render_inventory(
            {"fake_responses": {re.compile(r"^pip"): {"success": False}}},
        )

        assert "fake_responses:" in output
        assert "success" in output

    def test_empty_data(self):
        # `make_inventory` always injects some data, so exercise the helper
        # directly for the empty case.
        with console.capture() as capture:
            console.print(_format_host_data({}))
        assert "(no data)" in capture.get()

    def test_scalar_styling_matches_json_highlighter(self):
        # Scalars are coloured by type to match Rich's JSON highlighter.
        assert _scalar_style(True) == "json.bool_true"
        assert _scalar_style(False) == "json.bool_false"
        assert _scalar_style(None) == "json.null"
        assert _scalar_style(80) == "json.number"
        assert _scalar_style(1.5) == "json.number"
        assert _scalar_style("web") == "json.str"
        # Non-JSON scalars render unstyled (shown via str()).
        assert _scalar_style(datetime(2021, 8, 14)) == ""
        assert _scalar_style(PurePosixPath("/opt/app")) == ""


def _failing_host_side_effect(failing_host):
    def effect(host, *args, **kwargs):
        if host.name == failing_host:
            return (False, FakeBuffer("", FakeChannel(1)))
        return (True, FakeBuffer("ok", FakeChannel(0)))

    return effect


class TestPrintRolloutResults(PatchSSHTestCase):
    def tearDown(self):
        # Keep gevent-local host/state context from leaking into later tests.
        from pyinfra.context import ctx_host, ctx_state

        ctx_host.reset()
        ctx_state.reset()
        super().tearDown()

    def _paused_rollout_state(self):
        inventory = make_inventory(
            hosts=("a1", "a2", "a3", "a4", "b1", "b2"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
            service_b=(["b1", "b2"], {}),
        )
        state = State(inventory, Config())
        state.current_stage = StateStage.Prepare
        connect_all(state)

        add_op(
            state,
            server.shell,
            "echo roll",
            _batch_size=2,
            _error_budget={"service_a": 1},
        )

        with patch(
            "pyinfra.api.host.Host.run_shell_command",
            autospec=True,
            side_effect=_failing_host_side_effect("a1"),
        ):
            run_ops(state)

        return state

    def test_json_locates_unstarted_batches_and_not_started_hosts(self):
        state = self._paused_rollout_state()
        payload = build_results_json(state)

        assert payload["unstarted_batches"] == {"service_a": [["a3", "a4"]]}

        [operation] = payload["operations"]
        assert sorted(operation["not_started"]) == ["a3", "a4"]
        assert "a1" in operation["error"]
        assert "a2" in operation["success"]
        # The other group finished and has nobody waiting.
        assert "b1" in operation["success"] and "b2" in operation["success"]
        assert payload["totals"]["not_started"] == 2
        assert payload["failed_hosts"] == ["a1"]

    def test_human_results_table_has_not_started_column_and_batch_listing(self):
        state = self._paused_rollout_state()
        wide_console = Console(width=200, force_terminal=False, highlight=False)

        # The table rows are emitted via the pyinfra logger, while the unstarted
        # batch listing goes through the rich console; capture both separately.
        with self.assertLogs("pyinfra", level="INFO") as logs:
            with patch.object(prints_module, "console", wide_console):
                with wide_console.capture() as capture:
                    print_results(state)

        logged = "\n".join(logs.output)
        assert "Not Started" in logged
        assert "2 (a3, a4)" in logged

        output = capture.get()
        assert "Rollout batches not started" in output
        assert "service_a" in output and "a3" in output and "a4" in output
        # The healthy group has no unstarted batch listed.
        assert "service_b" not in output.split("Rollout batches not started")[1]

    def test_non_rolling_results_have_no_not_started_payload(self):
        inventory = make_inventory(hosts=("a1", "a2"))
        state = State(inventory, Config())
        state.current_stage = StateStage.Prepare
        connect_all(state)
        add_op(state, server.shell, "echo plain")

        with patch(
            "pyinfra.api.host.Host.run_shell_command",
            autospec=True,
            side_effect=_failing_host_side_effect(None),
        ):
            run_ops(state)

        payload = build_results_json(state)
        assert "not_started" not in payload["totals"]
        assert "unstarted_batches" not in payload
        assert "not_started" not in payload["operations"][0]
