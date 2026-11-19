import os
import pickle
from unittest.mock import patch

import pytest

import fsspec
from fsspec.implementations.local import LocalFileOpener, LocalFileSystem
from fsspec.implementations.memory import MemoryFileSystem
from fsspec.transaction import (
    CLEANED,
    CLEANUP_FAILED,
    COMMITTED,
    DISCARDED,
    NOT_ATTEMPTED,
    NOT_COMMITTED,
    NOT_REQUIRED,
    UNKNOWN,
    Transaction,
    TransactionEntry,
    TransactionReport,
)


class FakeFile:
    """Deferred file double with controllable commit/discard behaviour."""

    def __init__(self, path, temp=None, commit_error=None, discard_error=None):
        self.path = path
        self.temp = temp if temp is not None else path + ".tmp"
        self.commit_error = commit_error
        self.discard_error = discard_error
        self.commits = 0
        self.discards = 0

    def commit(self):
        self.commits += 1
        if self.commit_error is not None:
            raise self.commit_error

    def discard(self):
        self.discards += 1
        if self.discard_error is not None:
            raise self.discard_error


class FakeFS:
    def __init__(self):
        self._intrans = True
        self._transaction = None


def _transaction(*files):
    fs = FakeFS()
    t = Transaction(fs)
    t.files.extend(files)
    return t, fs


def test_all_commits_succeed():
    a, b = FakeFile("a"), FakeFile("b")
    t, fs = _transaction(a, b)
    report = t.complete()
    assert report.ok and bool(report)
    assert report.requested == "commit"
    assert report.committed_paths == ["a", "b"]
    assert report.not_committed == []
    assert report.cleanup_failed == []
    assert all(e.cleanup_status == NOT_REQUIRED for e in report.entries)
    assert a.commits == 1 and b.commits == 1
    assert a.discards == 0 and b.discards == 0
    # transaction always releases the filesystem
    assert fs._intrans is False and fs._transaction is None and t.fs is None
    assert t.report is report


def test_commit_failure_keeps_earlier_successes_and_reports_them():
    primary = PermissionError("cannot publish b")
    a = FakeFile("a")
    b = FakeFile("b", commit_error=primary)
    c = FakeFile("c")
    t, fs = _transaction(a, b, c)

    with pytest.raises(PermissionError) as ei:
        t.complete()

    # the original exception is propagated unchanged
    assert ei.value is primary
    report = t.report
    assert report.error is primary
    assert not report.ok
    # a already published: it is reported committed and never rolled back
    assert report.committed_paths == ["a"]
    assert a.discards == 0
    # b failed and c was never attempted: both are reported not committed,
    # with their full identity and (for b) the original error
    assert report.not_committed_paths == ["b", "c"]
    failed = report.failed
    assert [e.path for e in failed] == ["b"]
    assert failed[0].error is primary
    assert failed[0].temp == "b.tmp"
    assert [e.path for e in report.skipped] == ["c"]
    # temporary artifacts of the not-committed files are cleaned up
    assert b.discards == 1 and c.discards == 1
    assert all(e.cleaned for e in report.not_committed)
    assert report.cleanup_failed == []
    # the report is attached to the exception without replacing it
    assert ei.value.transaction_report is report
    # filesystem left deferred-write mode
    assert fs._intrans is False and fs._transaction is None


def test_cleanup_failure_is_reported_and_primary_cause_kept():
    primary = OSError("commit blew up")
    cleanup = PermissionError("cannot remove temp")
    a = FakeFile("a")
    b = FakeFile("b", commit_error=primary, discard_error=cleanup)
    t, fs = _transaction(a, b)

    with pytest.raises(OSError) as ei:
        t.complete()

    # the commit error stays the reported cause, not the cleanup error
    assert ei.value is primary
    report = t.report
    assert report.error is primary
    failed = report.cleanup_failed
    assert [e.path for e in failed] == ["b"]
    assert failed[0].cleanup_error is cleanup
    assert failed[0].cleanup_status == CLEANUP_FAILED
    # the leftover temp is still identified
    assert report.cleanup_failed_paths == ["b"]
    assert report.leftovers[0].identity == {"path": "b", "temp": "b.tmp"}
    assert not report.ok


def test_all_cleanup_failures_are_reported_separately():
    primary = OSError("commit failed")
    a = FakeFile("a", commit_error=primary, discard_error=OSError("no a"))
    b = FakeFile("b", discard_error=OSError("no b"))
    t, _ = _transaction(a, b)

    with pytest.raises(OSError):
        t.complete()

    report = t.report
    assert [e.path for e in report.cleanup_failed] == ["a", "b"]
    # b was never committed, but its cleanup was attempted and failed, so it
    # is a leftover rather than merely skipped
    assert [e.path for e in report.skipped] == []
    assert [e.path for e in report.leftovers] == ["a", "b"]
    assert report.summary.endswith("2 cleanup failed")


def test_discard_reports_discarded_and_cleanup_status():
    a, b = FakeFile("a"), FakeFile("b")
    t, fs = _transaction(a, b)

    report = t.complete(commit=False)

    assert report.ok
    assert report.requested == "discard"
    assert [e.status for e in report.entries] == [DISCARDED, DISCARDED]
    # discarding removes the temporary artifacts
    assert all(e.cleanup_status == CLEANED for e in report.entries)
    assert report.committed == []
    assert fs._intrans is False


def test_discard_failure_reports_cleanup_and_keeps_going():
    bad = OSError("cannot discard")
    a = FakeFile("a", discard_error=bad)
    b = FakeFile("b")
    t, fs = _transaction(a, b)

    with pytest.raises(OSError) as ei:
        t.complete(commit=False)

    assert ei.value is bad
    report = t.report
    assert report.requested == "discard"
    assert [e.status for e in report.entries] == [NOT_COMMITTED, DISCARDED]
    assert report.failed == []
    assert report.cleanup_failed_paths == ["a"]
    assert report.not_committed_paths == ["a"]
    assert not report.ok
    # b was still processed
    assert b.discards == 1


def test_empty_transaction_is_ok():
    t, fs = _transaction()
    report = t.complete()
    assert report.ok
    assert report.entries == []
    assert fs._intrans is False


def test_cleanup_of_missing_temp_is_idempotent_success():
    class Gone(FakeFile):
        def discard(self):
            self.discards += 1
            raise FileNotFoundError(self.temp)

    primary = OSError("commit failed")
    a = Gone("a", commit_error=primary)
    t, _ = _transaction(a)

    with pytest.raises(OSError):
        t.complete()

    report = t.report
    # a missing temp means nothing is left behind: that is a successful cleanup
    assert report.cleanup_failed == []
    assert [e.path for e in report.cleanup_succeeded] == ["a"]


def test_unknown_status_not_successful():
    report = TransactionReport(requested="commit")
    entry = report.add(FakeFile("a"))
    entry.status = UNKNOWN
    assert report.indeterminate == [entry]
    assert not report.ok


def test_fresh_entry_reports_cleanup_not_attempted():
    entry = TransactionEntry.from_file(FakeFile("a"))
    assert entry.status == NOT_COMMITTED
    assert entry.cleanup_status == NOT_ATTEMPTED
    assert not entry.attempted
    assert entry.identity == {"path": "a", "temp": "a.tmp"}


def test_report_picklable_drops_file_handle():
    entry = TransactionEntry(path="a", temp="a.tmp", status=COMMITTED, file=object())
    report = TransactionReport(entries=[entry])
    loaded = pickle.loads(pickle.dumps(report))
    assert loaded.entries[0].path == "a"
    assert loaded.entries[0].file is None


def test_entry_describe_supports_backend_attribute_names():
    class TargetTemp:
        target = "final"
        path = "/tmp/xyz"

    class Sftp:
        targetpath = "remote/final"
        temppath = "/tmp/sftp"

    class Cached:
        path = "remote/final"
        fn = "/local/cache"

    assert TransactionEntry.from_file(TargetTemp()).identity == {
        "path": "final",
        "temp": "/tmp/xyz",
    }
    assert TransactionEntry.from_file(Sftp()).identity == {
        "path": "remote/final",
        "temp": "/tmp/sftp",
    }
    assert TransactionEntry.from_file(Cached()).identity == {
        "path": "remote/final",
        "temp": "/local/cache",
    }


def test_context_manager_exception_does_not_mask_original():
    class Boom(Exception):
        pass

    class Exploding(FakeFile):
        def commit(self):
            raise Boom("commit")

    a = Exploding("a")
    t, fs = _transaction(a)
    t.fs = fs
    fs._transaction = t
    with pytest.raises(Boom):
        t.__exit__(None, None, None)
    assert fs._intrans is False
    assert t.report.error is not None


def test_context_manager_commit_error_propagates_from_clean_exit():
    class Boom(Exception):
        pass

    class Exploding(FakeFile):
        def commit(self):
            raise Boom("commit")

    a = Exploding("a")
    t, fs = _transaction(a)
    t.fs = fs
    fs._transaction = t
    # no exception from the body, but the commit fails: it must be raised
    with pytest.raises(Boom):
        t.__exit__(None, None, None)
    assert fs._intrans is False and fs._transaction is None
    assert t.report is not None and not t.report.ok


def test_context_manager_original_exception_preserved_over_cleanup_error():
    class BodyError(Exception):
        pass

    cleanup = OSError("cannot discard temp")

    a = FakeFile("a", discard_error=cleanup)
    t, fs = _transaction(a)
    t.fs = fs
    fs._transaction = t
    original = BodyError("body failed")
    # the body error must win over the cleanup error during rollback
    assert t.__exit__(BodyError, original, None) is False
    assert fs._intrans is False
    assert t.report.requested == "discard"
    assert t.report.cleanup_failed_paths == ["a"]
    assert original.transaction_report is t.report


def test_local_commit_failure_leaves_committed_targets_on_disk(tmpdir):
    fs = LocalFileSystem()
    real_commit = LocalFileOpener.commit

    def commit(self):
        if self.path.endswith("b"):
            raise PermissionError(self.path)
        real_commit(self)

    with patch.object(LocalFileOpener, "commit", commit):
        with pytest.raises(PermissionError):
            with fs.transaction as txn:
                for name in ("a", "b", "c"):
                    with fs.open(str(tmpdir / name), "wb") as f:
                        f.write(b"data")

    report = txn.report
    assert not report.ok
    # a really landed and must not be rolled back
    assert (tmpdir / "a").exists()
    assert report.committed_paths == [fs._strip_protocol(str(tmpdir / "a"))]
    # b and c did not, and their temporary files are gone
    assert not (tmpdir / "b").exists()
    assert not (tmpdir / "c").exists()
    assert report.not_committed_paths == [
        fs._strip_protocol(str(tmpdir / "b")),
        fs._strip_protocol(str(tmpdir / "c")),
    ]
    assert all(e.temp for e in report.not_committed)
    assert report.cleanup_failed == []
    assert not fs._intrans


def test_end_transaction_returns_report_and_clears_state(tmpdir):
    fs = LocalFileSystem()
    fs.start_transaction()
    with fs.open(str(tmpdir / "x"), "wb") as f:
        f.write(b"hello")
    report = fs.end_transaction()
    assert report.ok
    assert report.committed_paths == [fs._strip_protocol(str(tmpdir / "x"))]
    assert fs._transaction is None
    assert fs.cat(str(tmpdir / "x")) == b"hello"


def test_end_transaction_invalidates_cache_on_failure(tmpdir):
    fs = LocalFileSystem()
    real_commit = LocalFileOpener.commit

    def commit(self):
        if self.path.endswith("b"):
            raise PermissionError(self.path)
        real_commit(self)

    fs.start_transaction()
    with fs.open(str(tmpdir / "a"), "wb") as f:
        f.write(b"data")
    with fs.open(str(tmpdir / "b"), "wb") as f:
        f.write(b"data")

    with patch.object(LocalFileOpener, "commit", commit), pytest.raises(
        PermissionError
    ) as ei:
        fs.end_transaction()

    # the cache list was consumed and the filesystem left transaction mode
    assert fs._invalidated_caches_in_transaction == []
    assert fs._transaction is None
    assert fs._intrans is False
    assert ei.value.transaction_report is not None


def test_memory_transaction_report():
    fs = MemoryFileSystem(skip_instance_cache=True, global_store=False)
    with fs.transaction:
        fs.pipe_file("one", b"1")
        fs.pipe_file("two", b"2")
    assert fs.cat("one") == b"1"
    assert fs.cat("two") == b"2"


def test_memory_transaction_discard_report():
    fs = MemoryFileSystem(skip_instance_cache=True, global_store=False)
    with pytest.raises(RuntimeError):
        with fs.transaction as txn:
            fs.pipe_file("one", b"1")
            raise RuntimeError("abort")
    report = txn.report
    assert report.requested == "discard"
    assert report.ok
    assert [e.status for e in report.entries] == [DISCARDED]
    assert not fs.exists("one")


def test_local_cleanup_failure_is_visible_and_keeps_primary(tmpdir):
    fs = LocalFileSystem()
    real_commit = LocalFileOpener.commit
    real_discard = LocalFileOpener.discard

    def commit(self):
        if self.path.endswith("b"):
            raise PermissionError(self.path)
        real_commit(self)

    def discard(self):
        if os.path.basename(self.path) == "b":
            raise OSError("temporary file is locked")
        real_discard(self)

    with patch.object(LocalFileOpener, "commit", commit), patch.object(
        LocalFileOpener, "discard", discard
    ):
        with pytest.raises(PermissionError) as ei:
            with fs.transaction as txn:
                for name in ("a", "b", "c"):
                    with fs.open(str(tmpdir / name), "wb") as f:
                        f.write(b"data")

    assert isinstance(ei.value, PermissionError)  # not the cleanup OSError
    report = txn.report
    assert report.cleanup_failed_paths == [fs._strip_protocol(str(tmpdir / "b"))]
    leftover = report.cleanup_failed[0]
    assert isinstance(leftover.cleanup_error, OSError)
    # the leftover temp really is still there; the report says so honestly
    assert os.path.exists(leftover.temp)
    # c was cleaned up successfully
    assert [e.path for e in report.cleanup_succeeded] == [
        fs._strip_protocol(str(tmpdir / "c"))
    ]
    assert not report.ok


def test_simplecache_bulk_commit_is_indeterminate_on_failure(tmp_path, m, monkeypatch):
    fs = fsspec.filesystem(
        "simplecache", fs=m, cache_storage=str(tmp_path), skip_instance_cache=True
    )

    def failing_put(*args, **kwargs):
        raise OSError("upload did not complete")

    monkeypatch.setattr(m, "put", failing_put)
    with pytest.raises(OSError) as ei:
        with fs.transaction as txn:
            fs.pipe_file("one", b"1")
            fs.pipe_file("two", b"2")

    report = txn.report
    # one bulk call, no per-file outcome: every file is indeterminate rather
    # than claiming the whole batch landed or did not
    assert ei.value is report.error
    assert sorted(report.committed_paths) == []
    assert sorted(e.path for e in report.indeterminate) == ["/one", "/two"]
    assert not report.ok


def test_simplecache_transaction_report(tmp_path, m):
    fs = fsspec.filesystem(
        "simplecache", fs=m, cache_storage=str(tmp_path), skip_instance_cache=True
    )
    with fs.transaction as txn:
        fs.pipe_file("one", b"1")
        fs.pipe_file("two", b"2")
    report = txn.report
    assert report.ok
    assert sorted(report.committed_paths) == ["/one", "/two"]
    assert m.cat("/one") == b"1"
    assert m.cat("/two") == b"2"
