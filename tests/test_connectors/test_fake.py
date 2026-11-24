import re
from unittest import TestCase
from unittest.mock import patch

from pyinfra.api import Config, HiddenValue, State, StringCommand
from pyinfra.api.connect import connect_all
from pyinfra.connectors.fake import FakeConnector, FakeDisconnectError

from ..util import make_inventory

# Instant execution everywhere so tests never call gevent.sleep.
NO_DELAY = {"fake_delay": 0, "fake_delay_jitter": 0}


def make_fake_inventory(hosts=("@fake",), **data):
    host_data = {**NO_DELAY, **data}
    return make_inventory(hosts=tuple((host, host_data) for host in hosts))


class TestFakeConnectorNamesData(TestCase):
    def test_make_names_data_no_name(self):
        assert list(FakeConnector.make_names_data()) == [("@fake", {}, ["@fake"])]

    def test_make_names_data_single_name(self):
        assert list(FakeConnector.make_names_data("web-1")) == [
            ("@fake/web-1", {}, ["@fake"]),
        ]

    def test_make_names_data_comma_separated(self):
        assert list(FakeConnector.make_names_data("web-1, web-2 ,web-3")) == [
            ("@fake/web-1", {}, ["@fake"]),
            ("@fake/web-2", {}, ["@fake"]),
            ("@fake/web-3", {}, ["@fake"]),
        ]

    def test_make_names_data_ignores_empty_segments(self):
        assert list(FakeConnector.make_names_data("web-1,,")) == [
            ("@fake/web-1", {}, ["@fake"]),
        ]


class TestFakeConnector(TestCase):
    def test_connect_all(self):
        inventory = make_fake_inventory(hosts=("@fake",))
        state = State(inventory, Config())
        connect_all(state)
        assert len(state.active_hosts) == 1

    def test_connect_multiple_named_hosts(self):
        inventory = make_inventory(
            hosts=(("@fake/web-1", NO_DELAY), ("@fake/web-2", NO_DELAY)),
        )
        state = State(inventory, Config())
        connect_all(state)
        assert len(state.active_hosts) == 2

    def test_default_command_succeeds_with_empty_output(self):
        inventory = make_fake_inventory()
        State(inventory, Config())
        host = inventory.get_host("@fake")

        status, output = host.run_shell_command("echo hi")

        assert status is True
        assert output.output_lines == []

    def test_executed_commands_recorded(self):
        inventory = make_fake_inventory()
        State(inventory, Config())
        host = inventory.get_host("@fake")

        host.run_shell_command("echo one")
        host.run_shell_command("echo two")

        assert host.connector.executed_commands == ["echo one", "echo two"]

    def test_response_substring_match(self):
        inventory = make_fake_inventory(
            fake_responses={"git --version": "git version 2.40.0"},
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        status, output = host.run_shell_command("git --version")

        assert status is True
        assert output.stdout_lines == ["git version 2.40.0"]

    def test_response_regexp_match(self):
        inventory = make_fake_inventory(
            fake_responses={
                re.compile(r"^apt-get .*install"): {
                    "success": False,
                    "stderr": "E: locked",
                },
            },
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        status, output = host.run_shell_command("apt-get -y install nginx")

        assert status is False
        assert output.stderr_lines == ["E: locked"]

    def test_response_dict_stdout_stderr_success(self):
        inventory = make_fake_inventory(
            fake_responses={
                "check": {
                    "stdout": ["line 1", "line 2"],
                    "stderr": "a warning",
                    "success": False,
                },
            },
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        status, output = host.run_shell_command("run check now")

        assert status is False
        assert output.stdout_lines == ["line 1", "line 2"]
        assert output.stderr_lines == ["a warning"]

    def test_response_first_match_wins(self):
        inventory = make_fake_inventory(
            fake_responses={
                "git": "first",
                "git --version": "second",
            },
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        _, output = host.run_shell_command("git --version")

        assert output.stdout_lines == ["first"]

    def test_unmatched_command_succeeds_empty(self):
        inventory = make_fake_inventory(
            fake_responses={"never-matched": "nope"},
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        status, output = host.run_shell_command("something else")

        assert status is True
        assert output.output_lines == []

    def test_put_file(self):
        inventory = make_fake_inventory()
        State(inventory, Config())
        host = inventory.get_host("@fake")

        assert host.put_file("local", "remote") is True

    def test_get_file(self):
        inventory = make_fake_inventory()
        State(inventory, Config())
        host = inventory.get_host("@fake")

        assert host.get_file("remote", "local") is True

    @patch("pyinfra.api.output._echo")
    def test_run_shell_command_masked(self, fake_echo):
        inventory = make_fake_inventory()
        State(inventory, Config())
        host = inventory.get_host("@fake")

        command = StringCommand("echo", HiddenValue("top-secret-stuff"))
        status, _ = host.run_shell_command(command, print_input=True)

        assert status is True
        # The masked value is echoed and recorded, never the secret.
        fake_echo.assert_called_with(
            f"{host.print_prefix}>>> echo *MASKED*",
            err=True,
        )
        assert host.connector.executed_commands == ["echo *MASKED*"]

    def test_delay_is_invoked(self):
        inventory = make_inventory(hosts=(("@fake", {"fake_delay": 2, "fake_delay_jitter": 0}),))
        State(inventory, Config())
        host = inventory.get_host("@fake")

        with patch("pyinfra.connectors.fake.gevent.sleep") as fake_sleep:
            host.run_shell_command("echo hi")

        fake_sleep.assert_called_once_with(2.0)

    def test_disconnect_raises_after_recording_write(self):
        inventory = make_fake_inventory(
            fake_responses={"apply-config": {"disconnect": True}},
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        with self.assertRaises(FakeDisconnectError):
            host.run_shell_command("apply-config")

        # The write was recorded first, then the acknowledgement was "lost".
        assert host.connector.executed_commands == ["apply-config"]
        assert host.connector.disconnected_commands == ["apply-config"]

    def test_disconnect_is_uncertain_error(self):
        # ConnectionError is an OSError, the family the engine treats as
        # uncertain (outcome cannot be inferred from the failure alone).
        assert issubclass(FakeDisconnectError, OSError)

    def test_disconnect_once_only_fires_first_time(self):
        inventory = make_fake_inventory(
            fake_responses={
                "apply-config": {"disconnect": True, "disconnect_once": True},
            },
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        with self.assertRaises(FakeDisconnectError):
            host.run_shell_command("apply-config")

        # Second invocation returns the canned (default success) response.
        status, output = host.run_shell_command("apply-config")
        assert status is True
        assert output.output_lines == []

        # The write itself was recorded both times, but only the first lost
        # its acknowledgement.
        assert host.connector.executed_commands == ["apply-config", "apply-config"]
        assert host.connector.disconnected_commands == ["apply-config"]

    def test_disconnect_without_once_fires_every_time(self):
        inventory = make_fake_inventory(
            fake_responses={"apply-config": {"disconnect": True}},
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        for _ in range(3):
            with self.assertRaises(FakeDisconnectError):
                host.run_shell_command("apply-config")

        assert len(host.connector.disconnected_commands) == 3

    def test_disconnect_once_tracks_matchers_independently(self):
        inventory = make_fake_inventory(
            fake_responses={
                "first": {"disconnect": True, "disconnect_once": True},
                "second": {"disconnect": True, "disconnect_once": True},
            },
        )
        State(inventory, Config())
        host = inventory.get_host("@fake")

        with self.assertRaises(FakeDisconnectError):
            host.run_shell_command("first write")
        with self.assertRaises(FakeDisconnectError):
            host.run_shell_command("second write")

        # Each matcher gets its own one-shot budget.
        status, _ = host.run_shell_command("first again")
        assert status is True
        assert host.connector.disconnected_commands == ["first write", "second write"]
