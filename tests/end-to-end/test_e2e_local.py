"""
Local integration tests - mostly checking for idempotency on file operations.
"""

from shutil import rmtree
from tempfile import mkdtemp

import pytest


@pytest.fixture()
def temp_dir():
    temp_dir = mkdtemp()
    try:
        yield temp_dir
    finally:
        rmtree(temp_dir)


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_file_no_changes(helpers, temp_dir):
    helpers.run_check_output(  # first run = create the file
        "pyinfra -y -v @local files.file _testfile",
        expected_lines=["@local] Success"],
        cwd=temp_dir,
    )

    helpers.run_check_output(  # second run = no changes
        "pyinfra -y -v @local files.file _testfile",
        expected_lines=["@local] No changes"],
        cwd=temp_dir,
    )


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_directory_no_changes(helpers, temp_dir):
    helpers.run_check_output(  # first run = create the directory
        "pyinfra -y -v @local files.directory _testdir",
        expected_lines=["@local] Success"],
        cwd=temp_dir,
    )

    helpers.run_check_output(  # second run = no changes
        "pyinfra -y -v @local files.directory _testdir",
        expected_lines=["@local] No changes"],
        cwd=temp_dir,
    )

    helpers.run_check_output(  # third run (remove) = remove directory
        "pyinfra -y -v @local files.directory _testdir present=False",
        expected_lines=["@local] Success"],
        cwd=temp_dir,
    )

    helpers.run_check_output(  # fourth run (remove) = no chances
        "pyinfra -y -v @local files.directory _testdir present=False",
        expected_lines=["@local] No changes"],
        cwd=temp_dir,
    )


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_link_no_changes(helpers, temp_dir):
    helpers.run_check_output(  # first run = create the link
        "pyinfra -y -v @local files.link _testlink target=_testfile",
        expected_lines=["@local] Success"],
        cwd=temp_dir,
    )

    helpers.run_check_output(  # second run = no changes
        "pyinfra -y -v @local files.link _testlink target=_testfile",
        expected_lines=["@local] No changes"],
        cwd=temp_dir,
    )


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_line_no_changes(helpers, temp_dir):
    helpers.run_check_output(  # first run = create the line
        "pyinfra -y -v @local files.line _testfile someline",
        expected_lines=["@local] Success"],
        cwd=temp_dir,
    )

    helpers.run_check_output(  # second run = no changes
        "pyinfra -y -v @local files.line _testfile someline",
        expected_lines=["@local] No changes"],
        cwd=temp_dir,
    )

    helpers.run_check_output(  # replace the line
        "pyinfra -y -v @local files.line _testfile someline replace=anotherline",
        expected_lines=["@local] Success"],
        cwd=temp_dir,
    )

    helpers.run_check_output(  # second run replace the line = no changes
        "pyinfra -y -v @local files.line _testfile someline replace=anotherline",
        expected_lines=["@local] No changes"],
        cwd=temp_dir,
    )


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_line_ensure_newline_true(helpers, tmp_path):
    path = tmp_path / "_testfile"

    path.write_bytes(b"hello world")
    helpers.run_check_output(
        "pyinfra -y -v @local files.line _testfile someline ensure_newline=true",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == b"hello world\nsomeline\n"

    path.write_bytes(b"hello world\n")
    helpers.run_check_output(
        "pyinfra -y -v @local files.line _testfile someline ensure_newline=true",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == b"hello world\nsomeline\n"


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_line_ensure_newline_false(helpers, tmp_path):
    path = tmp_path / "_testfile"

    path.write_bytes(b"hello world")
    helpers.run_check_output(
        "pyinfra -y -v @local files.line _testfile someline ensure_newline=false",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == b"hello worldsomeline\n"

    path.write_bytes(b"hello world\n")
    helpers.run_check_output(
        "pyinfra -y -v @local files.line _testfile someline ensure_newline=false",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == b"hello world\nsomeline\n"


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_block_no_changes(helpers, tmp_path):
    path = tmp_path / "_testfile"
    path.write_bytes(b"hello\n")

    helpers.run_check_output(  # first run = add the block
        "pyinfra -y -v @local files.block _testfile content=newcontent",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == (
        b"hello\n# BEGIN PYINFRA BLOCK\nnewcontent\n# END PYINFRA BLOCK\n"
    )

    helpers.run_check_output(  # second run = no changes
        "pyinfra -y -v @local files.block _testfile content=newcontent",
        expected_lines=["@local] No changes"],
        cwd=tmp_path,
    )

    helpers.run_check_output(  # change the content = success
        "pyinfra -y -v @local files.block _testfile content=othercontent",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == (
        b"hello\n# BEGIN PYINFRA BLOCK\nothercontent\n# END PYINFRA BLOCK\n"
    )

    helpers.run_check_output(  # remove the block = success
        "pyinfra -y -v @local files.block _testfile present=false",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == b"hello\n"

    helpers.run_check_output(  # remove again = no changes
        "pyinfra -y -v @local files.block _testfile present=false",
        expected_lines=["@local] No changes"],
        cwd=tmp_path,
    )


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_block_invalid_markers(helpers, tmp_path):
    # the markers must exactly surround a single block - anything else (duplicated,
    # interleaved or unpaired markers) must fail and leave the file untouched
    path = tmp_path / "_testfile"

    for contents in (
        # duplicated blocks
        b"# BEGIN PYINFRA BLOCK\na\n# END PYINFRA BLOCK\n"
        b"# BEGIN PYINFRA BLOCK\nb\n# END PYINFRA BLOCK\n",
        # interleaved markers
        b"# BEGIN PYINFRA BLOCK\n# BEGIN PYINFRA BLOCK\na\n"
        b"# END PYINFRA BLOCK\n# END PYINFRA BLOCK\n",
        # missing the end marker
        b"# BEGIN PYINFRA BLOCK\na\n",
        # missing the begin marker
        b"a\n# END PYINFRA BLOCK\n",
    ):
        path.write_bytes(contents)
        helpers.run(
            "pyinfra -y -v @local files.block _testfile content=newcontent",
            cwd=tmp_path,
            expected_exit_code=1,
        )
        assert path.read_bytes() == contents


def _make_block_deploy(tmp_path, filename, contents, edit):
    """
    Write a deploy that plans a ``files.block`` call and then edits the target file,
    simulating another person changing the file between the plan being made and the
    plan being executed.
    """

    path = tmp_path / filename
    path.write_bytes(contents)

    deploy = tmp_path / "deploy.py"
    deploy.write_text(
        "from pyinfra.operations import files\n"
        "\n"
        "files.block(\n"
        "    name='Update the managed block',\n"
        f"    path={str(path)!r},\n"
        "    content='newcontent',\n"
        ")\n"
        "\n"
        "# another person edits the file before the plan is executed\n"
        f"with open({str(path)!r}, 'wb') as f:\n"
        f"    f.write({edit!r})\n"
    )
    return path


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_block_changed_outside_block_before_execute(helpers, tmp_path):
    # a manual edit *outside* the managed block does not invalidate the plan: the
    # edit and the new block contents must both be present afterwards
    path = _make_block_deploy(
        tmp_path,
        "_testfile",
        b"manual: one\n# BEGIN PYINFRA BLOCK\noldcontent\n# END PYINFRA BLOCK\nmanual: two\n",
        b"manual: edited\n# BEGIN PYINFRA BLOCK\noldcontent\n# END PYINFRA BLOCK\nmanual: two\n",
    )

    helpers.run_check_output(
        "pyinfra -y -v @local deploy.py",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == (
        b"manual: edited\n# BEGIN PYINFRA BLOCK\nnewcontent\n# END PYINFRA BLOCK\nmanual: two\n"
    )


@pytest.mark.end_to_end
@pytest.mark.end_to_end_local
def test_int_local_block_changed_inside_block_before_execute(helpers, tmp_path):
    # an edit *inside* the managed block invalidates the plan: the run must report
    # the stale plan and leave the file byte-for-byte unchanged
    contents = (
        b"manual: one\n# BEGIN PYINFRA BLOCK\noldcontent\nsneaky\n# END PYINFRA BLOCK\n"
    )
    path = _make_block_deploy(
        tmp_path,
        "_testfile",
        b"manual: one\n# BEGIN PYINFRA BLOCK\noldcontent\n# END PYINFRA BLOCK\n",
        contents,
    )

    _, stderr = helpers.run(
        "pyinfra -y -v @local deploy.py",
        cwd=tmp_path,
        expected_exit_code=1,
    )
    assert "changed since this plan was made" in stderr
    assert path.read_bytes() == contents

    # re-prepare the same target: the first run makes the change...
    helpers.run_check_output(
        "pyinfra -y -v @local files.block _testfile content=newcontent",
        expected_lines=["@local] Success"],
        cwd=tmp_path,
    )
    assert path.read_bytes() == (
        b"manual: one\n# BEGIN PYINFRA BLOCK\nnewcontent\n# END PYINFRA BLOCK\n"
    )

    # ...and the second run is a no-op
    helpers.run_check_output(
        "pyinfra -y -v @local files.block _testfile content=newcontent",
        expected_lines=["@local] No changes"],
        cwd=tmp_path,
    )
