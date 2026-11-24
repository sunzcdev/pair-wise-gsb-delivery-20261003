import os
import sys
import zipfile
from collections.abc import MutableMapping

import pytest

from zict import Zip
from zict.tests import utils_test


@pytest.fixture
def fn(tmp_path, check_fd_leaks):
    yield tmp_path / "tmp.zip"


def test_simple(fn):
    z = Zip(fn)
    assert isinstance(z, MutableMapping)
    assert not z

    assert list(z) == list(z.keys()) == []
    assert list(z.values()) == []
    assert list(z.items()) == []

    z["x"] = b"123"
    assert list(z) == list(z.keys()) == ["x"]
    assert list(z.values()) == [b"123"]
    assert list(z.items()) == [("x", b"123")]
    assert z["x"] == b"123"

    z.flush()
    zz = zipfile.ZipFile(fn, mode="r")
    assert zz.read("x") == b"123"

    z["y"] = b"456"
    assert z["y"] == b"456"


def test_setitem_typeerror(fn):
    z = Zip(fn)
    with pytest.raises(TypeError):
        z["x"] = 123


def test_contextmanager(fn):
    with Zip(fn) as z:
        z["x"] = b"123"

    zz = zipfile.ZipFile(fn, mode="r")
    assert zz.read("x") == b"123"


def test_missing_key(fn):
    z = Zip(fn)

    with pytest.raises(KeyError):
        z["x"]


def test_close(fn):
    z = Zip(fn)

    z["x"] = b"123"
    z.close()

    zz = zipfile.ZipFile(fn, mode="r")
    assert zz.read("x") == b"123"

    with pytest.raises(IOError):
        z["y"] = b"123"


def test_bytearray(fn):
    data = bytearray(b"123")
    with Zip(fn) as z:
        z["x"] = data

    with Zip(fn) as z:
        assert z["x"] == b"123"


def test_memoryview(fn):
    data = memoryview(b"123")
    with Zip(fn) as z:
        z["x"] = data

    with Zip(fn) as z:
        assert z["x"] == b"123"


def check_mapping(z):
    """Shorter version of utils_test.check_mapping, as zip supports neither update nor
    delete
    """
    assert isinstance(z, MutableMapping)
    utils_test.check_empty_mapping(z)

    z["abc"] = b"456"
    z["xyz"] = b"12"
    assert len(z) == 2
    assert z["abc"] == b"456"

    utils_test.check_items(z, [("abc", b"456"), ("xyz", b"12")])

    assert "abc" in z
    assert "xyz" in z
    assert "def" not in z

    with pytest.raises(KeyError):
        z["def"]


def test_mapping(fn):
    """
    Test mapping interface for Zip().
    """
    with Zip(fn) as z:
        check_mapping(z)
        utils_test.check_closing(z)


def test_no_delete_update(fn):
    with Zip(fn) as z:
        z["x"] = b"123"
        with pytest.raises(NotImplementedError):
            del z["x"]
        with pytest.raises(NotImplementedError):
            z["x"] = b"456"
        assert len(z) == 1
        assert z["x"] == b"123"


def test_bad_types(fn):
    with Zip(fn) as z:
        utils_test.check_bad_key_types(z, has_del=False)
        utils_test.check_bad_value_types(z)


def make_legacy_archive(fn):
    """Write an archive, as produced by other tools, where the same key
    appears multiple times; reading resolves to the last entry.
    """
    with zipfile.ZipFile(fn, mode="w") as zf:
        zf.writestr("x", b"old")
        zf.writestr("y", b"first")
    with zipfile.ZipFile(fn, mode="a") as zf:
        zf.writestr("x", b"older")
        zf.writestr("y", b"second")
    with zipfile.ZipFile(fn, mode="a") as zf:
        zf.writestr("x", b"newest")
        zf.writestr("y", b"third")


def test_compact(fn):
    make_legacy_archive(fn)
    with Zip(fn) as z:
        # Without an explicit compact(), the usual read rules apply:
        # reads see the last entry for each key
        assert z["x"] == b"newest"
        assert z["y"] == b"third"
        assert len(z) == 6

        z.compact()

        assert len(z) == 2
        assert list(z) == ["x", "y"]
        assert z["x"] == b"newest"
        assert z["y"] == b"third"

    # New readers see each key exactly once, with its latest value
    with zipfile.ZipFile(fn, mode="r") as zf:
        assert sorted(zi.filename for zi in zf.infolist()) == ["x", "y"]
        assert zf.read("x") == b"newest"
        assert zf.read("y") == b"third"

    # The mapping is still fully usable afterwards
    with Zip(fn) as z:
        z["z"] = b"more"
        assert z["z"] == b"more"


def test_compact_empty(fn):
    with Zip(fn) as z:
        z.compact()  # The file doesn't exist yet; nothing to do
        z["x"] = b"123"
        z.compact()
        assert z["x"] == b"123"
        assert len(z) == 1


def test_compact_readonly(fn):
    make_legacy_archive(fn)
    with Zip(fn, mode="r") as z:
        with pytest.raises(ValueError, match="read-only"):
            z.compact()


def test_compact_closed(fn):
    z = Zip(fn)
    z.close()
    with pytest.raises(OSError, match="closed"):
        z.compact()


def test_compact_failed_write_leaves_old_archive(fn, tmp_path, monkeypatch):
    """A compaction that fails while writing the new file must not corrupt
    the old archive: readers that already hold it open, as well as mappings
    opened afterwards, still see the old contents, and the partial output is
    not left behind.
    """
    make_legacy_archive(fn)
    # An existing reader, holding the old directory index
    reader = zipfile.ZipFile(fn, mode="r")

    real_writestr = zipfile.ZipFile.writestr
    writes = 0

    def failing_writestr(self, zinfo_or_arcname, data, *args, **kwargs):
        nonlocal writes
        writes += 1
        if writes > 1:
            raise OSError("disk full")
        return real_writestr(self, zinfo_or_arcname, data, *args, **kwargs)

    monkeypatch.setattr(zipfile.ZipFile, "writestr", failing_writestr)

    z = Zip(fn)
    z["w"] = b"extra"
    with pytest.raises(OSError, match="disk full"):
        z.compact()

    # The failed output has not been mistaken for the archive
    assert os.listdir(tmp_path) == [os.path.basename(fn)]

    # The existing reader can finish reading the old contents
    assert reader.read("x") == b"newest"
    assert reader.read("y") == b"third"
    reader.close()

    # A freshly opened mapping also sees the old archive
    assert z["x"] == b"newest"
    assert z["y"] == b"third"
    assert z["w"] == b"extra"

    # A retry now succeeds
    monkeypatch.undo()
    z.compact()
    assert len(z) == 3
    assert z["x"] == b"newest"
    assert z["w"] == b"extra"
    z.close()


def test_compact_replace_blocked_can_retry(fn, tmp_path, monkeypatch):
    """Simulate Windows file locking: if old handles block the replacement,
    the caller is informed that the archive is still the old version and can
    retry once the readers are done.
    """
    make_legacy_archive(fn)
    z = Zip(fn)
    assert z["x"] == b"newest"

    real_replace = os.replace

    def blocked_replace(src, dst):
        raise PermissionError("file in use by another process")

    monkeypatch.setattr("zict.zip.os.replace", blocked_replace)
    with pytest.raises(PermissionError, match="in use"):
        z.compact()

    # Still the old version; the temporary output was cleaned up
    assert os.listdir(tmp_path) == [os.path.basename(fn)]
    assert z["x"] == b"newest"
    assert len(z) == 6

    # Readers are done; the retry goes through
    monkeypatch.undo()
    z.compact()
    assert len(z) == 2
    assert z["x"] == b"newest"
    z.close()

    # New readers only see the latest value for each key
    with zipfile.ZipFile(fn, mode="r") as zf:
        assert sorted(zi.filename for zi in zf.infolist()) == ["x", "y"]


@pytest.mark.skipif(sys.platform != "win32", reason="Windows-only file locking")
def test_compact_replace_blocked_by_open_handle_windows(fn):
    """On Windows, a reader holding the archive open blocks the replacement;
    compact() must fail loudly and leave the old archive in place."""
    make_legacy_archive(fn)
    z = Zip(fn)
    with open(fn, "rb") as old_handle:
        with pytest.raises(PermissionError):
            z.compact()
        # Still the old version
        assert z["x"] == b"newest"
        assert len(z) == 6
        assert old_handle.read()
    # Once the reader is done, the retry succeeds
    z.compact()
    assert len(z) == 2
    assert z["x"] == b"newest"
    z.close()
