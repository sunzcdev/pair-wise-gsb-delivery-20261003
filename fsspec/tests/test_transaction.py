"""Tests for context-local filesystem transactions.

Filesystem instances are cached and shared between execution contexts, so a
transaction must only defer writes made from the context that opened it. These
tests cover write attribution, nesting rules, cross-context rejection and the
isolation of commit/cleanup between concurrent transactions.
"""

import threading

import pytest

import fsspec
from fsspec.transaction import Transaction, active_transaction


@pytest.fixture()
def fs():
    filesystem = fsspec.filesystem("memory")
    filesystem.store.clear()
    filesystem.pseudo_dirs.clear()
    filesystem.pseudo_dirs.append("")
    try:
        yield filesystem
    finally:
        filesystem.store.clear()
        filesystem.pseudo_dirs.clear()
        filesystem.pseudo_dirs.append("")


def test_transaction_commits_on_normal_exit(fs):
    with fs.transaction:
        assert fs._intrans
        assert active_transaction(fs) is fs._transaction
        with fs.open("/a", "wb") as f:
            f.write(b"data")
        assert not fs.exists("/a")
    assert not fs._intrans
    assert fs._transaction is None
    assert fs.cat("/a") == b"data"


def test_transaction_discards_on_exception(fs):
    with pytest.raises(RuntimeError, match="boom"):
        with fs.transaction:
            with fs.open("/b", "wb") as f:
                f.write(b"data")
            raise RuntimeError("boom")
    assert not fs.exists("/b")
    assert not fs._intrans
    assert fs._transaction is None


def test_writes_outside_a_transaction_are_immediate(fs):
    with fs.open("/plain", "wb") as f:
        f.write(b"now")
    assert fs.cat("/plain") == b"now"
    assert not fs._intrans


def test_reentrant_nesting_joins_the_same_transaction(fs):
    with fs.transaction as outer:
        with fs.transaction as inner:
            assert inner is outer
            with fs.open("/n", "wb") as f:
                f.write(b"nested")
            assert not fs.exists("/n")
        # the inner, re-entrant exit must not finalise the files
        assert fs._intrans
        assert not fs.exists("/n")
    assert fs.cat("/n") == b"nested"
    assert fs._transaction is None


def test_nesting_a_distinct_transaction_is_rejected(fs):
    other = Transaction(fs)
    with fs.transaction:
        with pytest.raises(RuntimeError, match="already active"):
            with other:
                pass
    assert not fs._intrans
    assert fs._transaction is None


def test_explicit_start_and_end_transaction(fs):
    transaction = fs.start_transaction()
    assert fs._intrans
    try:
        with fs.open("/e", "wb") as f:
            f.write(b"x")
        assert not fs.exists("/e")
    finally:
        fs.end_transaction()
    assert fs.cat("/e") == b"x"
    assert not fs._intrans
    assert transaction.fs is None


def test_end_transaction_without_active_transaction_raises(fs):
    with pytest.raises(RuntimeError, match="No transaction"):
        fs.end_transaction()


def test_foreign_transaction_cannot_be_started_over_an_active_one(fs):
    with fs.transaction:
        foreign = Transaction(fs)
        with pytest.raises(RuntimeError, match="already active"):
            foreign.start()


def test_concurrent_threads_get_independent_transactions(fs):
    errors = []

    def transaction_worker():
        try:
            with fs.transaction:
                # the other thread's transaction must not be visible here
                assert len(fs._transaction.files) == 0
                with fs.open("/deferred", "wb") as f:
                    f.write(b"later")
                assert not fs.exists("/deferred")
                barrier.wait()
            assert fs.cat("/deferred") == b"later"
        except Exception as exc:  # pragma: no cover - failure reporting
            errors.append(exc)

    def ordinary_worker():
        try:
            barrier.wait()
            # an ordinary write from another context is not delayed
            assert not fs._intrans
            assert active_transaction(fs) is None
            with fs.open("/immediate", "wb") as f:
                f.write(b"now")
            assert fs.cat("/immediate") == b"now"
        except Exception as exc:  # pragma: no cover - failure reporting
            errors.append(exc)

    barrier = threading.Barrier(2)
    t1 = threading.Thread(target=transaction_worker)
    t2 = threading.Thread(target=ordinary_worker)
    t1.start()
    t2.start()
    t1.join()
    t2.join()
    assert errors == []
    assert fs.cat("/deferred") == b"later"
    assert fs.cat("/immediate") == b"now"


def test_concurrent_transactions_keep_their_own_file_queues(fs):
    errors = []
    ready = threading.Event()
    release = threading.Event()

    def worker(name):
        try:
            with fs.transaction:
                with fs.open(f"/{name}", "wb") as f:
                    f.write(name.encode())
                assert len(fs._transaction.files) == 1
                ready.set()
                release.wait(timeout=5)
            assert fs.cat(f"/{name}") == name.encode()
        except Exception as exc:  # pragma: no cover - failure reporting
            errors.append(exc)

    t1 = threading.Thread(target=worker, args=("one",))
    t2 = threading.Thread(target=worker, args=("two",))
    t1.start()
    t2.start()
    assert ready.wait(timeout=5)
    # both transactions are open concurrently on the cached instance
    release.set()
    t1.join()
    t2.join()
    assert errors == []
    assert fs.cat("/one") == b"one"
    assert fs.cat("/two") == b"two"


def test_transaction_cannot_be_finished_from_another_context(fs):
    box = {}

    def starter():
        transaction = Transaction(fs)
        transaction.start()
        box["transaction"] = transaction

    thread = threading.Thread(target=starter)
    thread.start()
    thread.join()
    transaction = box["transaction"]

    with pytest.raises(RuntimeError, match="different context"):
        transaction.complete()
    with pytest.raises(RuntimeError, match="different context"):
        transaction._unwind(commit=True)


def test_failing_transaction_does_not_block_another(fs):
    class FakeFile:
        def __init__(self, name, fail_commit):
            self.path = name
            self.fail_commit = fail_commit
            self.discarded = False

        def commit(self):
            if self.fail_commit:
                raise PermissionError(self.path)
            fs.pipe_file(self.path, b"committed")

        def discard(self):
            self.discarded = True

    outcome = {}

    def failing_worker():
        transaction = Transaction(fs)
        try:
            with transaction:
                bad = FakeFile("/bad", True)
                good = FakeFile("/good-in-bad", False)
                transaction.files.append(bad)
                transaction.files.append(good)
        except PermissionError as exc:
            outcome["error"] = exc
            outcome["transaction"] = transaction

    def succeeding_worker():
        with fs.transaction:
            with fs.open("/healthy", "wb") as f:
                f.write(b"fine")

    t1 = threading.Thread(target=failing_worker)
    t2 = threading.Thread(target=succeeding_worker)
    t1.start()
    t2.start()
    t1.join()
    t2.join()

    assert isinstance(outcome["error"], PermissionError)
    # every file of the failed transaction was cleaned up
    assert all(f.discarded for f in outcome["transaction"].files)
    # the other transaction still committed normally
    assert fs.cat("/healthy") == b"fine"
    assert not fs.exists("/bad")
    # the shared, cached instance is usable and not stuck in a transaction
    assert not fs._intrans
    with fs.open("/after", "wb") as f:
        f.write(b"after")
    assert fs.cat("/after") == b"after"


def test_failed_commit_still_discards_remaining_files(fs):
    class FakeFile:
        def __init__(self, name, fail_commit):
            self.path = name
            self.fail_commit = fail_commit
            self.discarded = False

        def commit(self):
            if self.fail_commit:
                raise PermissionError(self.path)

        def discard(self):
            self.discarded = True

    transaction = fs.start_transaction()
    files = [FakeFile("a", False), FakeFile("b", True), FakeFile("c", False)]
    for f in files:
        transaction.files.append(f)
    with pytest.raises(PermissionError):
        transaction.complete()
    # the file that raised and everything still queued were discarded; the
    # file committed before the failure was already finalised
    assert not files[0].discarded
    assert files[1].discarded and files[2].discarded
    assert not fs._intrans


def test_completing_twice_is_a_noop(fs):
    transaction = fs.start_transaction()
    with fs.open("/z", "wb") as f:
        f.write(b"z")
    fs.end_transaction()
    # a second completion must not raise or touch anything
    transaction.complete()
    assert fs.cat("/z") == b"z"


def test_each_context_starts_without_a_transaction(fs):
    seen = []

    def worker():
        seen.append(active_transaction(fs))

    with fs.transaction:
        thread = threading.Thread(target=worker)
        thread.start()
        thread.join()
        assert active_transaction(fs) is not None
    assert seen == [None]
