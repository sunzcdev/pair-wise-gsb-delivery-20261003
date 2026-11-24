import os
import pathlib
import sys

import pytest

from zict import File
from zict.tests import utils_test


def test_mapping(tmp_path, check_fd_leaks):
    """
    Test mapping interface for File().
    """
    z = File(tmp_path)
    utils_test.check_mapping(z)


@pytest.mark.parametrize("dirtype", [str, pathlib.Path, lambda x: x])
def test_implementation(tmp_path, check_fd_leaks, dirtype):
    z = File(dirtype(tmp_path))
    assert not z

    z["x"] = b"123"
    assert os.listdir(tmp_path) == ["x#0"]
    with open(tmp_path / "x#0", "rb") as f:
        assert f.read() == b"123"

    assert "x" in z
    out = z["x"]
    assert isinstance(out, bytearray)
    assert out == b"123"


def test_memmap_implementation(tmp_path, check_fd_leaks):
    z = File(tmp_path, memmap=True)
    assert not z

    mv = memoryview(b"123")
    assert "x" not in z
    z["x"] = mv
    assert os.listdir(tmp_path) == ["x#0"]
    assert "x" in z
    mv2 = z["x"]
    assert mv2 == b"123"
    # Buffer is writeable
    mv2[0] = mv2[1]
    assert mv2 == b"223"


def test_str(tmp_path, check_fd_leaks):
    z = File(tmp_path)
    assert str(z) == repr(z) == f"<File: {tmp_path}, 0 elements>"


def test_setitem_typeerror(tmp_path, check_fd_leaks):
    z = File(tmp_path)
    with pytest.raises(TypeError):
        z["x"] = 123


def test_contextmanager(tmp_path, check_fd_leaks):
    with File(tmp_path) as z:
        z["x"] = b"123"

    with open(tmp_path / "x#0", "rb") as fh:
        assert fh.read() == b"123"


def test_delitem(tmp_path, check_fd_leaks):
    z = File(tmp_path)

    z["x"] = b"123"
    assert os.listdir(tmp_path) == ["x#0"]
    del z["x"]
    assert os.listdir(tmp_path) == []
    # File name is never repeated
    z["x"] = b"123"
    assert os.listdir(tmp_path) == ["x#1"]
    # __setitem__ deletes the previous file
    z["x"] = b"123"
    assert os.listdir(tmp_path) == ["x#2"]


def test_missing_key(tmp_path, check_fd_leaks):
    z = File(tmp_path)

    with pytest.raises(KeyError):
        z["x"]


def test_arbitrary_chars(tmp_path, check_fd_leaks):
    z = File(tmp_path)

    # Avoid hitting the Windows max filename length
    chunk = 16
    for i in range(1, 128, chunk):
        key = "".join(["foo_"] + [chr(i) for i in range(i, min(128, i + chunk))])
        with pytest.raises(KeyError):
            z[key]
        z[key] = b"foo"
        assert z[key] == b"foo"
        assert list(z) == [key]
        assert list(z.keys()) == [key]
        assert list(z.items()) == [(key, b"foo")]
        assert list(z.values()) == [b"foo"]

        zz = File(tmp_path)
        assert zz[key] == b"foo"
        assert list(zz) == [key]
        assert list(zz.keys()) == [key]
        assert list(zz.items()) == [(key, b"foo")]
        assert list(zz.values()) == [b"foo"]
        del zz

        del z[key]
        with pytest.raises(KeyError):
            z[key]


def test_write_list_of_bytes(tmp_path, check_fd_leaks):
    z = File(tmp_path)

    z["x"] = [b"123", b"4567"]
    assert z["x"] == b"1234567"


def test_bad_types(tmp_path, check_fd_leaks):
    z = File(tmp_path)
    utils_test.check_bad_key_types(z)
    utils_test.check_bad_value_types(z)


def test_setitem_atomic_short_write(tmp_path, check_fd_leaks):
    """A write that fails halfway through must not destroy or truncate the old value

    This is what happens e.g. when the disk fills up, or when the value is a list of
    buffers and one of them turns out not to be a buffer.
    """
    z = File(tmp_path)
    z["x"] = b"old"
    old = z["x"]
    assert old == b"old"

    # Short write: the first chunk is written, then the second one blows up
    with pytest.raises(TypeError):
        z["x"] = [b"partial", 123]

    # A reader that fetched the value before the failed write is unaffected
    assert old == b"old"
    # ... and the same instance still serves the old value
    assert z["x"] == b"old"
    assert list(z) == ["x"]
    assert len(z) == 1

    # So does an instance that reopens the directory, which also doesn't index the
    # staging file as a key
    zz = File(tmp_path)
    assert list(zz) == ["x"]
    assert len(zz) == 1
    assert zz["x"] == b"old"
    assert os.listdir(tmp_path) == ["x#0"]

    # A successful write is immediately visible, in both instances
    z["x"] = b"new"
    assert z["x"] == b"new"
    zz = File(tmp_path)
    assert zz["x"] == b"new"
    # The old file has been replaced by a newer one; no staging file is left behind
    assert os.listdir(tmp_path) == [zz.filenames["x"]]
    assert not zz.filenames["x"].endswith(".tmp")
    assert zz.filenames["x"] != "x#0"


def test_setitem_short_write_while_reading(tmp_path, check_fd_leaks, monkeypatch):
    """Inject a short write (e.g. ENOSPC) while a reader is using the old value.

    Both that reader and a File instance that reopens the directory must still read
    the old, complete value, and the truncated staging file must not become a key.
    """
    import zict.file

    z = File(tmp_path)
    z["x"] = b"old value"

    # A reader that fetched the value right before the failing replacement
    reader = z["x"]
    assert reader == b"old value"

    real_open = open
    written = []

    def failing_open(file, mode="r", *args, **kwargs):
        fh = real_open(file, mode, *args, **kwargs)
        if "w" not in mode:
            return fh

        class ShortFile:
            """Writes a few bytes and then fails as if the disk were full"""

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                fh.close()

            def write(self, data):
                chunk = data[:3]
                fh.write(chunk)
                fh.flush()
                written.append(chunk)
                raise OSError(28, "No space left on device")

            def writelines(self, lines):
                for line in lines:
                    self.write(line)

            def fileno(self):
                return fh.fileno()

        return ShortFile()

    monkeypatch.setattr(zict.file, "open", failing_open, raising=False)
    with pytest.raises(OSError):
        z["x"] = b"a much longer new value"
    monkeypatch.undo()

    assert written == [b"a m"]

    # The reader that started before the failure is unaffected
    assert reader == b"old value"
    # So is this instance, and so is one that reopens the directory
    assert z["x"] == b"old value"
    zz = File(tmp_path)
    assert list(zz) == ["x"]
    assert len(zz) == 1
    assert zz["x"] == b"old value"
    # The half-written staging file was cleaned up and never counted as a key
    assert os.listdir(tmp_path) == ["x#0"]

    # A later successful write is immediately visible to both instances
    z["x"] = b"new value"
    assert z["x"] == b"new value"
    assert File(tmp_path)["x"] == b"new value"


def test_replace_while_old_file_is_open(tmp_path, check_fd_leaks):
    """Replacing a key must be consistent even if the old file can't be unlinked

    On Windows (and with NFS, and any time an mmap or a file descriptor is still open
    on it) the old file may survive the replacement. It must not shadow the new value
    when the directory is reopened, and the old reader must keep its complete value.
    """
    z = File(tmp_path)
    z["x"] = b"old"
    old_path = tmp_path / z.filenames["x"]

    with open(old_path, "rb") as held:
        assert held.read() == b"old"
        z["x"] = b"new"
        # The reader keeps seeing the complete old content through its own handle
        assert held.seek(0) == 0
        assert held.read() == b"old"

        assert z["x"] == b"new"
        zz = File(tmp_path)
        assert list(zz) == ["x"]
        assert len(zz) == 1
        assert zz["x"] == b"new"


def test_delete_after_locked_replacement(tmp_path, check_fd_leaks, monkeypatch):
    """A file that survived a replacement because it was locked must not resurrect

    Simulates Windows, where the old file of a replaced key cannot be unlinked while a
    reader still has it open.
    """
    import zict.file

    real_remove = os.remove
    z = File(tmp_path)
    z["x"] = b"old"
    old_path = str(tmp_path / z.filenames["x"])

    def locked_remove(path):
        if str(path) == old_path:
            raise PermissionError(32, "The process cannot access the file")
        real_remove(path)

    monkeypatch.setattr(zict.file.os, "remove", locked_remove)
    z["x"] = b"new"
    assert z["x"] == b"new"
    # The superseded file could not be unlinked, so both are on disk
    assert sorted(os.listdir(tmp_path)) == ["x#0", "x#1"]
    assert z.filenames["x"] == "x#1"
    # A reopened instance still sees the new value: the newer version always wins
    zz = File(tmp_path)
    assert list(zz) == ["x"]
    assert zz["x"] == b"new"
    monkeypatch.undo()

    # Deleting the key must not leave the stale file behind, or it would resurrect
    zz = File(tmp_path)
    del zz["x"]
    assert os.listdir(tmp_path) == []
    assert not File(tmp_path)


def test_delete_raises_while_stale_file_is_locked(
    tmp_path, check_fd_leaks, monkeypatch
):
    """If a superseded file can't be removed, the deletion fails as a whole"""
    import zict.file

    real_remove = os.remove
    z = File(tmp_path)
    z["x"] = b"old"
    old_path = str(tmp_path / z.filenames["x"])

    def locked_remove(path):
        if str(path) == old_path:
            raise PermissionError(32, "The process cannot access the file")
        real_remove(path)

    monkeypatch.setattr(zict.file.os, "remove", locked_remove)
    z["x"] = b"new"
    with pytest.raises(PermissionError):
        del z["x"]
    # The key is still there, consistently in memory and on disk
    assert z["x"] == b"new"
    assert File(tmp_path)["x"] == b"new"

    monkeypatch.undo()
    del z["x"]
    assert os.listdir(tmp_path) == []


def test_stale_file_retried_on_next_setitem(tmp_path, check_fd_leaks, monkeypatch):
    """A leftover file is cleaned up as soon as the OS lets go of it"""
    import zict.file

    real_remove = os.remove
    z = File(tmp_path)
    z["x"] = b"v0"
    z["x"] = b"v1"
    assert os.listdir(tmp_path) == ["x#1"]

    # Pretend a reader still has "x#1" open, so it can't be unlinked
    def locked_remove(path):
        if str(path) == str(tmp_path / "x#1"):
            raise PermissionError(32, "The process cannot access the file")
        real_remove(path)

    monkeypatch.setattr(zict.file.os, "remove", locked_remove)
    z["x"] = b"v2"
    assert sorted(os.listdir(tmp_path)) == ["x#1", "x#2"]
    assert z._stale == {"x#1"}
    assert z["x"] == b"v2"
    assert File(tmp_path)["x"] == b"v2"
    monkeypatch.undo()

    # The reader is gone; the next replacement retries the cleanup
    z["x"] = b"v3"
    assert os.listdir(tmp_path) == ["x#3"]
    assert z._stale == set()
    assert File(tmp_path)["x"] == b"v3"


def test_stale_file_only_blocks_its_own_key(tmp_path, check_fd_leaks, monkeypatch):
    """A locked leftover of one key never makes another key undeletable"""
    import zict.file

    real_remove = os.remove
    z = File(tmp_path)
    z["x"] = b"v0"
    z["y"] = b"y0"

    def locked_remove(path):
        if str(path) == str(tmp_path / "y#1"):
            raise PermissionError(32, "The process cannot access the file")
        real_remove(path)

    monkeypatch.setattr(zict.file.os, "remove", locked_remove)
    z["y"] = b"y1"
    assert z._stale == {"y#1"}
    z["y"] = b"y2"
    assert z._stale == {"y#1"}

    # Deleting "x" is unaffected by the leftover of "y"
    del z["x"]
    assert "x" not in z
    assert list(File(tmp_path)) == ["y"]
    assert z._stale == {"y#1"}

    monkeypatch.undo()
    del z["y"]
    assert os.listdir(tmp_path) == []


def test_ignore_uncommitted_files(tmp_path, check_fd_leaks):
    """Files left behind by an interrupted __setitem__ are not keys"""
    z = File(tmp_path)
    z["x"] = b"old"

    # Simulate a process that died while writing a new value of "x", plus one that
    # died while creating a brand new key "y"
    (tmp_path / "x#1.0123456789abcdef0123456789abcdef.tmp").write_bytes(b"ol")
    (tmp_path / "y#2.0123456789abcdef0123456789abcdef.tmp").write_bytes(b"n")

    zz = File(tmp_path)
    assert list(zz) == ["x"]
    assert len(zz) == 1
    assert "y" not in zz
    assert zz["x"] == b"old"
    # The leftovers are not deleted: another live File instance may still be writing
    # to them
    assert len(os.listdir(tmp_path)) == 3

    # A subsequent successful write doesn't collide with the stale version numbers,
    # and doesn't pick up the stale staging files either
    zz["x"] = b"new"
    assert zz["x"] == b"new"
    assert zz.filenames["x"] not in ("x#0", "x#1")
    assert not zz.filenames["x"].endswith(".tmp")
    zzz = File(tmp_path)
    assert list(zzz) == ["x"]
    assert zzz["x"] == b"new"


def test_legacy_file_names(tmp_path, check_fd_leaks):
    """Files written by zict < 3.0 (no '#<version>' suffix) are still readable"""
    (tmp_path / "x").write_bytes(b"old")
    (tmp_path / "a%20b").write_bytes(b"spam")

    z = File(tmp_path)
    assert z["x"] == b"old"
    assert z["a b"] == b"spam"
    assert sorted(z) == ["a b", "x"]

    z["x"] = b"new"
    assert z["x"] == b"new"
    zz = File(tmp_path)
    assert zz["x"] == b"new"
    # The legacy file has been replaced with a versioned one
    assert zz.filenames["x"] != "x"
    assert sorted(zz) == ["a b", "x"]
    assert zz["a b"] == b"spam"


@pytest.mark.stress
@pytest.mark.repeat(utils_test.REPEAT_STRESS_TESTS)
def test_stress_different_keys_threadsafe(tmp_path):
    z = File(tmp_path)
    utils_test.check_different_keys_threadsafe(z)
    utils_test.check_mapping(z)


@pytest.mark.stress
@pytest.mark.repeat(utils_test.REPEAT_STRESS_TESTS)
@pytest.mark.skipif(sys.platform == "win32", reason="Can't delete file with open fd")
def test_stress_same_key_threadsafe(tmp_path):
    z = File(tmp_path)
    utils_test.check_same_key_threadsafe(z)
    utils_test.check_mapping(z)
