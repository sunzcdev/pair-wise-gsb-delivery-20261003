import os
import shutil
import stat
import subprocess
import sys

import pytest

from pyinfra.api import FileUploadCommand, StringCommand
from pyinfra.context import ctx_host, ctx_state
from pyinfra.operations import files
from pyinfra_testing.util import FakeState, create_host

SH = shutil.which("sh") or shutil.which("bash")


def _shell_has_required_tools():
    if SH is None:
        return False
    try:
        result = subprocess.run(
            [SH, "-c", "command -v sha1sum && command -v chmod && command -v cut"],
            capture_output=True,
        )
    except OSError:
        return False
    return result.returncode == 0


pytestmark = pytest.mark.skipif(
    not _shell_has_required_tools(),
    reason="requires a POSIX shell with sha1sum/chmod/cut",
)

# Mode assertions are only meaningful on POSIX: on Windows os.chmod can only
# toggle the read-only bit, so the final mode cannot be checked reliably.
IS_WINDOWS = sys.platform == "win32"

DEST = "dest.conf"
SRC_CONTENT = "desired config\n"
EXTERNAL_CONTENT = "external version\n"


def _generate_put_commands(src):
    """Generate the files.put commands for a remote file whose content differs."""
    state = FakeState()
    host = create_host(
        state,
        facts={
            "files.File": {f"path={DEST}": {"mode": 600, "user": "root", "group": "root"}},
            "files.Sha1File": {f"path={DEST}": "nowt"},
        },
    )
    with ctx_state.use(state), ctx_host.use(host):
        return list(files.put._inner(str(src), DEST, mode=644, verify_upload=True))


def _split_commands(commands):
    uploads = [c for c in commands if isinstance(c, FileUploadCommand)]
    attribute_commands = [c.get_raw_value() for c in commands if isinstance(c, StringCommand)]
    assert len(uploads) == 1
    assert len(attribute_commands) == 1
    return uploads[0], attribute_commands[0]


def _run(command, cwd):
    return subprocess.run([SH, "-c", command], cwd=cwd, capture_output=True, text=True)


def test_verify_upload_conflict_leaves_replaced_file_untouched(tmp_path):
    src = tmp_path / "src.conf"
    src.write_text(SRC_CONTENT)
    # A local file standing in for the remote destination
    dest = tmp_path / DEST
    dest.write_text("old remote content\n")

    _, chmod_command = _split_commands(_generate_put_commands(src))

    # The actual command carries the version check before the attribute change
    assert "sha1sum" in chmod_command
    assert "changed after upload" in chmod_command
    assert chmod_command.endswith(f"&& chmod 644 {DEST}")

    # The upload completes...
    shutil.copyfile(src, dest)

    # ...then the file is replaced by another version before the attribute
    # change runs (eg a concurrent process writing the same path)
    dest.write_text(EXTERNAL_CONTENT)
    if not IS_WINDOWS:
        os.chmod(dest, 0o600)

    result = _run(chmod_command, cwd=tmp_path)

    # The conflict is reported and the attribute change does not happen
    assert result.returncode != 0
    assert "changed after upload" in result.stderr
    assert dest.read_text() == EXTERNAL_CONTENT
    if not IS_WINDOWS:
        assert stat.S_IMODE(dest.stat().st_mode) == 0o600


def test_verify_upload_applies_attributes_when_content_matches(tmp_path):
    src = tmp_path / "src.conf"
    src.write_text(SRC_CONTENT)
    dest = tmp_path / DEST
    dest.write_text("old remote content\n")

    _, chmod_command = _split_commands(_generate_put_commands(src))

    # The upload completes and the file is left alone afterwards
    shutil.copyfile(src, dest)

    result = _run(chmod_command, cwd=tmp_path)

    assert result.returncode == 0, result.stderr
    assert dest.read_text() == SRC_CONTENT
    if not IS_WINDOWS:
        assert stat.S_IMODE(dest.stat().st_mode) == 0o644