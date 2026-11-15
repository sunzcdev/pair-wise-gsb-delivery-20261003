"""
Tests for the optional consistent read view.

These tests cover reading a DBF/memo pair that another program may
replace, truncate or otherwise modify while it is being read.
"""
import os
import shutil

import pytest

from dbfread import DBF, DBFChangedError
from dbfread.consistency import FileIdentity, copy_file_verified

CASES = os.path.join(os.path.dirname(__file__), 'cases')
DBF_SOURCE = os.path.join(CASES, 'memotest.dbf')
MEMO_SOURCE = os.path.join(CASES, 'memotest.FPT')


@pytest.fixture
def table_files(tmp_path):
    """A private copy of the DBF/memo pair that tests may mutate."""
    dbf = str(tmp_path / 'memotest.dbf')
    memo = str(tmp_path / 'memotest.FPT')
    shutil.copy(DBF_SOURCE, dbf)
    shutil.copy(MEMO_SOURCE, memo)
    return dbf, memo


def _replace(path, data):
    """Replace a file the way a writer process typically does.

    On Windows an open read handle (even one opened with delete sharing)
    can be renamed/deleted but can not be overwritten in place, so the
    realistic sequence is: write a new file, move the old one aside, and
    move the new one into its name.
    """
    tmp = path + '.new'
    with open(tmp, 'wb') as f:
        f.write(data)
    try:
        os.replace(tmp, path)
    except PermissionError:
        old = path + '.old'
        if os.path.exists(old):
            os.remove(old)
        os.rename(path, old)
        os.rename(tmp, path)


def _bump_mtime(path):
    """Force the modification time forward without changing content."""
    stat = os.stat(path)
    os.utime(path, (stat.st_atime + 100, stat.st_mtime + 100))


def test_default_interface_is_unchanged(table_files):
    dbf, memo = table_files
    table = DBF(dbf)
    assert table.consistent is False
    assert table.copy_to_temp is False
    assert table.closed is False
    # No identities are pinned in the legacy mode.
    assert table._identities == {}
    assert len(table) == 2
    assert len(table.deleted) == 1


def test_consistent_read_returns_everything(table_files):
    dbf, memo = table_files
    table = DBF(dbf, consistent=True)
    assert table.consistent is True
    assert table.filename == table.source_filename == dbf
    assert table.memofilename == table.source_memofilename == memo
    assert set(table._identities) == {dbf, memo}

    names = [record['NAME'] for record in table]
    assert names == ['Alice', 'Bob']
    # Memos are read through the guarded wrapper.
    assert [record['MEMO'] for record in table] == [
        'Alice memo', 'Bob memo']


def test_file_replacement_mid_iteration_is_detected(table_files):
    dbf, memo = table_files
    table = DBF(dbf, consistent=True)

    with pytest.raises(DBFChangedError):
        for index, _record in enumerate(table):
            if index == 0:
                # Replace the DBF with a different file at the same path.
                _replace(dbf, b'a completely different file' * 10)

    assert table.closed is False


def test_truncation_mid_iteration_is_detected(table_files):
    dbf, memo = table_files
    table = DBF(dbf, consistent=True)

    with pytest.raises(DBFChangedError):
        for index, _record in enumerate(table):
            if index == 0:
                with open(dbf, 'r+b') as f:
                    f.truncate(os.path.getsize(dbf) // 2)


def test_growth_is_detected(table_files):
    dbf, memo = table_files
    table = DBF(dbf, consistent=True)

    with pytest.raises(DBFChangedError):
        for index, _record in enumerate(table):
            if index == 0:
                with open(dbf, 'ab') as f:
                    f.write(b'extra bytes appended by another program')


def test_modification_time_change_is_detected(table_files):
    dbf, memo = table_files
    table = DBF(dbf, consistent=True)

    with pytest.raises(DBFChangedError):
        for index, _record in enumerate(table):
            if index == 0:
                _bump_mtime(dbf)


def test_removal_is_detected(table_files):
    dbf, memo = table_files
    table = DBF(dbf, consistent=True)

    with pytest.raises(DBFChangedError):
        for index, _record in enumerate(table):
            if index == 0:
                os.remove(dbf)


def test_memo_replacement_is_detected(table_files):
    dbf, memo = table_files
    table = DBF(dbf, consistent=True)

    with pytest.raises(DBFChangedError):
        for index, _record in enumerate(table):
            if index == 0:
                _replace(memo, b'not a memo file' * 20)


def test_copy_to_temp_reads_from_private_copy(table_files):
    dbf, memo = table_files
    table = DBF(dbf, copy_to_temp=True)
    assert table.copy_to_temp is True
    assert table.consistent is True
    # Reading happens from the temp directory, not the source path.
    assert table.filename != dbf
    assert table._tempdir in table.filename
    assert table.source_filename == dbf
    assert table.source_memofilename == memo
    assert os.path.dirname(table.filename) == table._tempdir

    assert [r['NAME'] for r in table] == ['Alice', 'Bob']

    tempdir = table._tempdir
    assert os.path.isdir(tempdir)
    table.close()
    assert not os.path.isdir(tempdir)
    assert table.closed is True
    # The borrowed source files are untouched.
    assert os.path.exists(dbf)
    assert os.path.exists(memo)


def test_copy_to_temp_isolated_from_later_source_change(table_files):
    dbf, memo = table_files
    table = DBF(dbf, copy_to_temp=True)
    # Mutating the source after the copy must not affect the private copy.
    os.remove(dbf)
    os.remove(memo)
    assert [r['NAME'] for r in table] == ['Alice', 'Bob']
    table.close()


def test_copy_to_temp_context_manager_cleans_up(table_files):
    dbf, memo = table_files
    with DBF(dbf, copy_to_temp=True) as table:
        tempdir = table._tempdir
        assert os.path.isdir(tempdir)
        assert len(table) == 2
    assert not os.path.isdir(tempdir)
    assert table.closed is True
    # Source files are never modified.
    assert os.path.exists(dbf)
    assert os.path.exists(memo)


def test_closed_table_cannot_be_iterated(table_files):
    dbf, memo = table_files
    table = DBF(dbf, copy_to_temp=True)
    table.close()
    with pytest.raises(DBFChangedError):
        list(table)
    with pytest.raises(DBFChangedError):
        len(table)


def test_failed_construction_removes_temp_copy(table_files, tmp_path):
    dbf, memo = table_files

    class BrokenParser(object):
        def __init__(self, *args, **kwargs):
            raise ValueError('simulated parse failure')

    import tempfile
    before = set(os.listdir(tempfile.gettempdir()))
    with pytest.raises(ValueError):
        DBF(dbf, copy_to_temp=True, parserclass=BrokenParser)
    after = set(os.listdir(tempfile.gettempdir()))

    # No dbfread temp directory should be left behind.
    leftovers = [name for name in (after - before)
                 if name.startswith('dbfread-')]
    assert leftovers == []
    # The source files are untouched.
    assert os.path.exists(dbf)
    assert os.path.exists(memo)


def test_source_identity_checked_around_copy(table_files, monkeypatch):
    dbf, memo = table_files
    original_copy = shutil.copyfile

    def swap_during_copy(source, destination):
        # Mimic another program replacing the source mid-copy.
        result = original_copy(source, destination)
        if source == dbf:
            _bump_mtime(source)
        return result

    monkeypatch.setattr(shutil, 'copyfile', swap_during_copy)
    with pytest.raises(DBFChangedError):
        DBF(dbf, copy_to_temp=True)


def test_copy_file_verified_detects_change(table_files, monkeypatch):
    dbf, memo = table_files
    destination = dbf + '.copy'
    original_copy = shutil.copyfile

    def change_after_copy(source, dest):
        original_copy(source, dest)
        _bump_mtime(source)

    import dbfread.consistency as consistency
    monkeypatch.setattr(consistency.shutil, 'copyfile', change_after_copy)
    with pytest.raises(DBFChangedError):
        copy_file_verified(dbf, destination)
    # The partial destination is cleaned up.
    assert not os.path.exists(destination)


def test_file_identity_detects_replace_truncate_and_remove(table_files):
    dbf, memo = table_files
    identity = FileIdentity.pin(dbf)
    # No change means no exception.
    identity.check()

    original = open(dbf, 'rb').read()
    _bump_mtime(dbf)
    with pytest.raises(DBFChangedError):
        identity.check()

    # Restore and pin again to test truncation.
    _replace(dbf, original)
    identity = FileIdentity.pin(dbf)
    with open(dbf, 'r+b') as f:
        f.truncate(10)
    with pytest.raises(DBFChangedError):
        identity.check()

    _replace(dbf, original)
    identity = FileIdentity.pin(dbf)
    os.remove(dbf)
    with pytest.raises(DBFChangedError):
        identity.check()
    _replace(dbf, original)


def test_ignore_missing_memofile_works_in_copy_mode(tmp_path):
    source = os.path.join(CASES, 'no_memofile.dbf')
    dbf = str(tmp_path / 'no_memofile.dbf')
    shutil.copy(source, dbf)

    table = DBF(dbf, copy_to_temp=True, ignore_missing_memofile=True)
    assert table.memofilename is None
    assert table.source_memofilename is None
    record = next(iter(table))
    assert record['MEMO'] is None
    table.close()
