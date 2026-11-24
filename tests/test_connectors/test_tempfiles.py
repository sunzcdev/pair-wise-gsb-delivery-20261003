from __future__ import annotations

import os
from io import StringIO
from pathlib import Path
from unittest import mock

from pyinfra.api import Config, State, StringCommand
from pyinfra.api.operation import OperationMeta
from pyinfra.api.operations import run_host_op
from pyinfra.api.state import StateOperationHostData, StateOperationMeta
from pyinfra.connectors.tempfiles import TempFileRegistry
from pyinfra.connectors.util import CommandOutput

from ..util import make_inventory


def make_local_host():
    inventory = make_inventory(hosts=("@local",))
    state = State(inventory, Config())
    host = inventory.get_host("@local")
    host.init(state)
    return host


class TestTempFileRegistry:
    def test_first_owner_uploads_second_owner_reuses(self):
        host = mock.Mock()
        registry = TempFileRegistry()

        # Both plans register before the content is staged; both still need the
        # upload until someone marks it done.
        assert registry.acquire("/tmp/shared", owner="op-one") is True
        assert registry.acquire("/tmp/shared", owner="op-two") is True

        registry.mark_uploaded("/tmp/shared")

        # A later owner reuses the staged content rather than re-uploading.
        assert registry.acquire("/tmp/shared", owner="op-three") is False

        registry.release(host, "/tmp/shared", "op-one")
        # First owner gone but others still hold the file: no cleanup yet.
        host.run_shell_command.assert_not_called()

        registry.release(host, "/tmp/shared", "op-two")
        # op-three still holds it.
        host.run_shell_command.assert_not_called()

        registry.release(host, "/tmp/shared", "op-three")
        host.run_shell_command.assert_called_once()
        command = host.run_shell_command.call_args.args[0]
        assert "rm" in command.get_raw_value()
        assert "/tmp/shared" in command.get_raw_value()

    def test_cleanup_happens_exactly_once(self):
        host = mock.Mock()
        host.run_shell_command.return_value = (True, mock.Mock())
        registry = TempFileRegistry()

        registry.acquire("/tmp/shared", owner="op-one")
        registry.acquire("/tmp/shared", owner="op-two")

        registry.release(host, "/tmp/shared", "op-one")
        registry.release(host, "/tmp/shared", "op-two")
        # Releasing again (or an owner that already left) must not rm twice.
        registry.release(host, "/tmp/shared", "op-two")

        assert host.run_shell_command.call_count == 1

    def test_context_owners_release_after_failure_and_clean_once(self, tmp_path):
        shared = tmp_path / "shared.conf"
        shared.write_text("shared config fragment", encoding="utf-8")
        removals: list[str] = []

        def remove(path: str) -> None:
            removals.append(path)
            Path(path).unlink()

        registry = TempFileRegistry(remover=remove)
        shared_name = str(shared)

        with registry.hold(None, shared_name, "first") as first_lease:
            with registry.hold(None, shared_name, "second"):
                # The first plan fails and explicitly winds up its ownership.
                first_lease.release()

                # The second real user has not read the fragment yet; it must
                # still be present and readable.
                assert shared.exists()
                assert shared.read_text(encoding="utf-8") == "shared config fragment"

            # The second user exiting is the last release, so cleanup ran once.
            assert removals == [shared_name]
            assert not shared.exists()

        # Exiting the first context after its explicit release is idempotent.
        assert removals == [shared_name]

    def test_release_owner_releases_every_held_file(self):
        host = mock.Mock()
        host.run_shell_command.return_value = (True, mock.Mock())
        registry = TempFileRegistry()

        registry.acquire("/tmp/a", owner="op-one")
        registry.acquire("/tmp/b", owner="op-one")
        registry.acquire("/tmp/a", owner="op-two")

        registry.release_owner(host, "op-one")
        # /tmp/a still has op-two, only /tmp/b is removed.
        removed = [call.args[0].get_raw_value() for call in host.run_shell_command.call_args_list]
        assert len(removed) == 1
        assert "/tmp/b" in removed[0]

        registry.release_owner(host, "op-two")
        assert host.run_shell_command.call_count == 2

    def test_unmanaged_file_keeps_connector_lifecycle(self):
        host = mock.Mock()
        registry = TempFileRegistry()

        # Acquired without an owner: the registry records it but never removes it.
        assert registry.acquire("/tmp/plain") is True
        registry.release(host, "/tmp/plain", None)
        registry.release_all(host)

        host.run_shell_command.assert_not_called()

    def test_release_all_cleans_managed_files_only(self):
        host = mock.Mock()
        host.run_shell_command.return_value = (True, mock.Mock())
        registry = TempFileRegistry()

        registry.acquire("/tmp/plain")
        registry.acquire("/tmp/shared", owner="op-one")

        registry.release_all(host)

        removed = [call.args[0].get_raw_value() for call in host.run_shell_command.call_args_list]
        assert len(removed) == 1
        assert "/tmp/shared" in removed[0]

    def test_cleanup_failure_is_swallowed(self):
        host = mock.Mock()
        host.run_shell_command.return_value = (False, mock.Mock(stderr="nope"))
        registry = TempFileRegistry()

        registry.acquire("/tmp/shared", owner="op-one")
        # Must not raise even when rm fails, so disconnect always completes.
        registry.release(host, "/tmp/shared", "op-one")
        assert host.run_shell_command.call_count == 1


class TestLocalConnectorSharedTempFiles:
    def test_two_owners_same_temp_file_single_cleanup(self, tmp_path):
        host = make_local_host()
        connector = host.connector

        tmp = str(tmp_path)
        shared_temp = str(tmp_path / "pyinfra-shared")
        dest_one = str(tmp_path / "dest-one")
        dest_two = str(tmp_path / "dest-two")
        content = StringIO("shared config fragment")

        rms: list[str] = []
        writes: list[str] = []

        real_open = open

        def tracking_open(path, *args, **kwargs):
            if str(path) == shared_temp and "w" in (args[0] if args else kwargs.get("mode", "")):
                writes.append(str(path))
            return real_open(path, *args, **kwargs)

        def fake_run_shell_command(command, **kwargs):
            raw = command.get_raw_value()
            # Simulate the remote `cp temp dest` by copying the staging file.
            if "cp " in raw and "rm -f" not in raw:
                target = dest_one if dest_one in raw else dest_two
                Path(target).write_bytes(Path(shared_temp).read_bytes())
                return True, mock.Mock(stderr="")
            if "rm -f" in raw:
                rms.append(raw)
                Path(shared_temp).unlink(missing_ok=True)
                return True, mock.Mock(stderr="")
            raise AssertionError(f"unexpected command: {raw}")

        with (
            mock.patch("pyinfra.connectors.local.open", side_effect=tracking_open, create=True),
            mock.patch.object(
                connector, "run_shell_command", side_effect=fake_run_shell_command
            ),
            mock.patch.object(
                host, "run_shell_command", side_effect=fake_run_shell_command
            ),
        ):
            # Both operation plans declare they rely on the same staging file
            # before either of them runs, mirroring two plans sharing one upload.
            host.acquire_temp_file(shared_temp, "op-one")
            host.acquire_temp_file(shared_temp, "op-two")

            # Owner one uploads and reads the shared fragment.
            host.executing_op_hash = "op-one"
            status = connector.put_file(
                content,
                dest_one,
                remote_temp_filename=shared_temp,
            )
            assert status is True
            assert Path(shared_temp).exists()
            assert len(writes) == 1
            assert Path(dest_one).read_text() == "shared config fragment"

            # Owner one fails before owner two has read the file: releasing it
            # must leave the shared fragment in place for the remaining owner.
            host.release_owner_temp_files("op-one")
            assert Path(shared_temp).exists(), "first owner's release removed the shared file"

            # Owner two reuses the same upload (no re-upload) and reads it.
            host.executing_op_hash = "op-two"
            status = connector.put_file(
                StringIO("ignored, reuse path"),
                dest_two,
                remote_temp_filename=shared_temp,
            )
            assert status is True
            assert len(writes) == 1, "the shared fragment must be uploaded only once"
            assert Path(dest_two).read_text() == "shared config fragment"

            # Last owner exits -> exactly one cleanup of the shared file.
            host.release_owner_temp_files("op-two")

        assert not Path(shared_temp).exists()
        assert len(rms) == 1
        assert shared_temp in rms[0]

    def test_regular_upload_keeps_original_lifecycle(self, tmp_path):
        host = make_local_host()
        connector = host.connector

        tmp = str(tmp_path)
        dest = str(tmp_path / "dest")
        explicit_temp = str(tmp_path / "explicit-unregistered")
        created_temp: list[str] = []
        removed_temp: list[str] = []

        real_mkstemp = __import__("pyinfra.connectors.local", fromlist=["mkstemp"]).mkstemp

        def fake_mkstemp():
            fd, name = real_mkstemp(dir=tmp)
            # The connector re-opens the path itself; close the raw fd so Windows
            # does not hold a lock that blocks the immediate os.remove below.
            os.close(fd)
            created_temp.append(name)
            return fd, name

        def fake_remove(name):
            removed_temp.append(name)
            Path(name).unlink(missing_ok=True)

        def fake_run_shell_command(command, **kwargs):
            raw = command.get_raw_value()
            if "cp " in raw:
                # The staging file was written by the connector; mirror its
                # bytes into the destination without invoking a real shell.
                staging = next(p for p in created_temp if p in raw)
                Path(dest).write_bytes(Path(staging).read_bytes())
                return True, mock.Mock(stderr="")
            raise AssertionError(f"unexpected command: {raw}")

        with (
            mock.patch("pyinfra.connectors.local.mkstemp", side_effect=fake_mkstemp),
            mock.patch("pyinfra.connectors.local.os.remove", side_effect=fake_remove),
            mock.patch.object(
                connector, "run_shell_command", side_effect=fake_run_shell_command
            ),
        ):
            # Even a normal operation owner must not turn an unregistered upload
            # into deferred/shared ownership.
            host.executing_op_hash = "regular-op"
            status = connector.put_file(
                StringIO("plain upload"),
                dest,
                remote_temp_filename=explicit_temp,
            )

        assert status is True
        assert Path(dest).read_text() == "plain upload"
        # The private staging file is removed immediately, as it always was,
        # and never enters the shared-file registry.
        assert created_temp == removed_temp
        assert host.temp_file_registry._entries == {}

    def test_first_acquire_reports_new_until_upload_marked(self):
        host = make_local_host()
        shared_temp = "/tmp/pyinfra-shared"

        # Owners can register ahead of the upload; the file is "needed" until the
        # content has actually been staged.
        assert host.acquire_temp_file(shared_temp, "op-one") is True
        assert host.acquire_temp_file(shared_temp, "op-two") is True

        host.mark_temp_file_uploaded(shared_temp)

        assert host.acquire_temp_file(shared_temp, "op-three") is False

    def test_unmanaged_acquire_stays_unmanaged(self):
        host = make_local_host()
        plain_temp = "/tmp/pyinfra-plain"

        assert host.acquire_temp_file(plain_temp) is True
        host.mark_temp_file_uploaded(plain_temp)
        assert host.acquire_temp_file(plain_temp) is False
        # No owner ever attached, so the connector (not the registry) cleans up.
        assert host.temp_file_registry._entries[plain_temp].managed is False


class TestOperationOwnerRelease:
    def _register_op(self, state, host, op_hash, commands):
        state.op_meta[op_hash] = StateOperationMeta((0,))
        state.op_meta[op_hash].global_arguments = {
            "_ignore_errors": False,
            "_continue_on_error": False,
            "_timeout": 0,
            "_retries": 0,
            "_retry_delay": 5,
        }

        def command_generator():
            for command in commands:
                yield command

        state.set_op_data_for_host(
            host,
            op_hash,
            StateOperationHostData(
                command_generator,
                state.op_meta[op_hash].global_arguments,
                OperationMeta(op_hash, True),
            ),
        )

    def test_failed_operation_still_releases_its_temp_files(self):
        inventory = make_inventory(hosts=("@local",))
        state = State(inventory, Config())
        host = inventory.get_host("@local")
        host.init(state)

        shared_temp = "/tmp/pyinfra-shared"
        rms: list[str] = []

        def fake_run_shell_command(*args, **kwargs):
            # Works whether patched as the connector class method (connector,
            # command, ...) or called directly (command, ...): the StringCommand
            # is the last positional argument that exposes get_raw_value.
            cmd = next(a for a in args if hasattr(a, "get_raw_value"))
            raw = cmd.get_raw_value()
            if "rm -f" in raw:
                rms.append(raw)
                return True, CommandOutput([])
            # The operation's own shell command fails.
            return False, CommandOutput([])

        # Two operation plans hold the same shared temp file before execution.
        host.acquire_temp_file(shared_temp, "op-one")
        host.acquire_temp_file(shared_temp, "op-two")
        host.mark_temp_file_uploaded(shared_temp)

        self._register_op(state, host, "op-one", [StringCommand("false")])

        with mock.patch.object(host, "run_shell_command", side_effect=fake_run_shell_command):
            result = run_host_op(state, host, "op-one")

            assert result is False
            # Even though op-one failed, its teardown released it - but op-two
            # still holds the file, so no cleanup has happened yet.
            assert rms == []
            assert "op-one" not in host.temp_file_registry._entries[shared_temp].owners
            assert "op-two" in host.temp_file_registry._entries[shared_temp].owners

            # When the last owner finishes, the single cleanup runs.
            host.release_owner_temp_files("op-two")
            assert len(rms) == 1
            assert shared_temp in rms[0]
