from io import StringIO
from subprocess import PIPE
from unittest import TestCase
from unittest.mock import MagicMock, call, mock_open, patch

from pyinfra.api import Config, HiddenValue, State, StringCommand
from pyinfra.api.connect import connect_all
from pyinfra.connectors.util import make_unix_command

from ..util import make_inventory


class ChunkedBuffer:
    """
    File-like buffer that yields output in arbitrary chunks (not lines),
    simulating chunk boundaries falling in the middle of command output.
    """

    def __init__(self, chunks):
        self.chunks = chunks

    def __iter__(self):
        return iter(self.chunks)

    def close(self):
        pass


@patch("pyinfra.connectors.local.mkstemp", lambda: (None, "__tempfile__"))
@patch("pyinfra.connectors.local.os.remove", lambda f: None)
@patch("pyinfra.connectors.local.open", mock_open(read_data="test!"), create=True)
@patch("pyinfra.api.util.open", mock_open(read_data="test!"), create=True)
class TestLocalConnector(TestCase):
    def setUp(self):
        self.fake_popen_patch = patch("pyinfra.connectors.util.Popen")
        self.fake_popen_mock = self.fake_popen_patch.start()

    def tearDown(self):
        self.fake_popen_patch.stop()

    def test_connect_all(self):
        inventory = make_inventory(hosts=("@local",))
        state = State(inventory, Config())
        connect_all(state)
        assert len(state.active_hosts) == 1

    def test_connect_host(self):
        inventory = make_inventory(hosts=("@local",))
        state = State(inventory, Config())
        host = inventory.get_host("@local")
        host.connect(reason=True)
        assert len(state.active_hosts) == 0

    def test_run_shell_command(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())
        host = inventory.get_host("@local")

        command = "echo Šablony"
        self.fake_popen_mock().returncode = 0

        out = host.run_shell_command(command, _stdin="hello", print_output=True)
        assert len(out) == 2

        status, output = out
        assert status is True
        self.fake_popen_mock().stdin.write.assert_called_with(b"hello\n")

        combined_out = host.run_shell_command(
            command,
            _stdin="hello",
            print_output=True,
        )
        assert len(combined_out) == 2

        shell_command = make_unix_command(command).get_raw_value()
        self.fake_popen_mock.assert_called_with(
            shell_command,
            shell=True,
            stdout=PIPE,
            stderr=PIPE,
            stdin=PIPE,
        )

    @patch("pyinfra.api.output._echo")
    def test_run_shell_command_masked(self, fake_echo):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())
        host = inventory.get_host("@local")

        command = StringCommand("echo", HiddenValue("top-secret-stuff"))
        self.fake_popen_mock().returncode = 0

        out = host.run_shell_command(command, print_output=True, print_input=True)
        assert len(out) == 2

        status, output = out
        assert status is True

        self.fake_popen_mock.assert_called_with(
            "sh -c 'echo top-secret-stuff'",
            shell=True,
            stdout=PIPE,
            stderr=PIPE,
            stdin=PIPE,
        )

        fake_echo.assert_called_with(
            f"{host.print_prefix}>>> sh -c 'echo *MASKED*'",
            err=True,
        )

    @patch("pyinfra.api.output._echo")
    def test_run_shell_command_masks_hidden_values_in_output(self, fake_echo):
        """
        A hidden command value written back to stderr/stdout by the remote
        command must be masked in printed output and the stored CommandOutput
        (which feeds failure summaries), while the command itself runs with
        the real value. The hidden value arrives split across two chunks and
        a third chunk carries a public exception location.
        """
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())
        host = inventory.get_host("@local")

        command = StringCommand("deploy", "--token", HiddenValue("top-secret-stuff"))
        fake_process = self.fake_popen_mock()
        fake_process.returncode = 1
        fake_process.stdout = ChunkedBuffer([])
        fake_process.stderr = ChunkedBuffer(
            [
                b"error: token top-secret",
                b"-stuff rejected\n",
                b"Traceback: /srv/app/main.py line 42\n",
            ]
        )

        status, output = host.run_shell_command(command, print_output=True, print_input=True)
        assert status is False

        # The command executed with the real value
        self.fake_popen_mock.assert_called_with(
            "sh -c 'deploy --token top-secret-stuff'",
            shell=True,
            stdout=PIPE,
            stderr=PIPE,
            stdin=PIPE,
        )

        # The stored output (used for failure summaries/reports) is masked,
        # the error source and non-sensitive segments remain intact
        assert output.stderr_lines == [
            "error: token *MASKED* rejected",
            "Traceback: /srv/app/main.py line 42",
        ]
        assert "top-secret-stuff" not in output.output

        # The terminal output was masked too
        fake_echo.assert_any_call(
            f"{host.print_prefix}>>> sh -c 'deploy --token *MASKED*'",
            err=True,
        )
        for echo_call in fake_echo.call_args_list:
            assert "top-secret-stuff" not in echo_call.args[0]

    def test_run_shell_command_success_exit_codes(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())
        host = inventory.get_host("@local")

        command = "echo hi"
        self.fake_popen_mock().returncode = 1

        out = host.run_shell_command(command, _success_exit_codes=[1])
        assert len(out) == 2
        assert out[0] is True

    def test_run_shell_command_error(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())
        host = inventory.get_host("@local")

        command = "echo hi"
        self.fake_popen_mock().returncode = 1

        out = host.run_shell_command(command)
        assert len(out) == 2
        assert out[0] is False

    def test_put_file(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())

        host = inventory.get_host("@local")

        fake_process = MagicMock(returncode=0)
        self.fake_popen_mock.return_value = fake_process

        host.put_file("not-a-file", "not-another-file", print_output=True)

        self.fake_popen_mock.assert_called_with(
            "sh -c 'cp __tempfile__ not-another-file'",
            shell=True,
            stdout=PIPE,
            stderr=PIPE,
            stdin=PIPE,
        )

    def test_put_file_with_spaces(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())

        host = inventory.get_host("@local")

        fake_process = MagicMock(returncode=0)
        self.fake_popen_mock.return_value = fake_process

        host.put_file("not-a-file", "not another file with spaces", print_output=True)

        self.fake_popen_mock.assert_called_with(
            "sh -c 'cp __tempfile__ '\"'\"'not another file with spaces'\"'\"''",
            shell=True,
            stdout=PIPE,
            stderr=PIPE,
            stdin=PIPE,
        )

    def test_put_file_error(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())

        host = inventory.get_host("@local")

        fake_process = MagicMock(returncode=1)
        self.fake_popen_mock.return_value = fake_process

        with self.assertRaises(IOError):
            host.put_file("not-a-file", "not-another-file", print_output=True)

    def test_get_file(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())

        host = inventory.get_host("@local")

        fake_process = MagicMock(returncode=0)
        self.fake_popen_mock.return_value = fake_process

        host.get_file("not-a-file", "not-another-file", print_output=True)

        self.fake_popen_mock.assert_called_with(
            "sh -c 'cp not-a-file __tempfile__'",
            shell=True,
            stdout=PIPE,
            stderr=PIPE,
            stdin=PIPE,
        )

    def test_get_file_error(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())

        host = inventory.get_host("@local")

        fake_process = MagicMock(returncode=1)
        self.fake_popen_mock.return_value = fake_process

        with self.assertRaises(IOError):
            host.get_file("not-a-file", "not-another-file", print_output=True)

    def test_write_stdin(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())
        host = inventory.get_host("@local")

        command = "echo Šablony"
        self.fake_popen_mock().returncode = 0

        host.run_shell_command(command, _stdin=["hello", "abc"], print_output=True)
        self.fake_popen_mock().stdin.write.assert_has_calls(
            [
                call(b"hello\n"),
                call(b"abc\n"),
            ],
        )

    def test_write_stdin_io_object(self):
        inventory = make_inventory(hosts=("@local",))
        State(inventory, Config())
        host = inventory.get_host("@local")

        command = "echo Šablony"
        self.fake_popen_mock().returncode = 0

        host.run_shell_command(command, _stdin=StringIO("hello\nabc"), print_output=True)
        self.fake_popen_mock().stdin.write.assert_has_calls(
            [
                call(b"hello\n"),
                call(b"abc\n"),
            ],
        )
