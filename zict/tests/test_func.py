import gc
from collections.abc import MutableMapping

import pytest

from zict import Func, FuncCommitError, FuncEncodingError
from zict.common import ZictBase
from zict.tests import utils_test


def inc(x):
    return x + 1


def dec(x):
    return x - 1


def rotl(x):
    return x[1:] + x[:1]


def rotr(x):
    return x[-1:] + x[:-1]


def test_simple():
    d = {}
    f = Func(inc, dec, d)
    f["x"] = 10
    assert f["x"] == 10
    assert d["x"] == 11

    assert "x" in f
    assert list(f) == ["x"]
    assert list(f.values()) == [10]
    assert list(f.items()) == [("x", 10)]

    assert all(s in str(f) for s in ["inc", "dec", "x", "Func"])
    assert all(s in repr(f) for s in ["inc", "dec", "x", "Func"])

    del f["x"]
    assert "x" not in d


def test_mapping():
    """
    Test mapping interface for Func().
    """
    d = {}
    z = Func(rotl, rotr, d)
    utils_test.check_mapping(z)
    utils_test.check_closing(z)


@pytest.mark.parametrize("wrapped_cls", [MutableMapping, ZictBase])
def test_update_descopes_early(wrapped_cls):
    """Test that Func.update() descopes the output of self.dump as soon as it can, if
    the wrapped mapping allows, and doesn't store everything into a list.
    """

    class Dumped:
        n = 0

        def __init__(self):
            gc.collect()  # Only necessary on pypy
            Dumped.n += 1
            assert Dumped.n < 3

        def __del__(self):
            Dumped.n -= 1

    class Dummy(wrapped_cls):
        def __setitem__(self, key, value):
            pass

        def __getitem__(self, key, value):
            raise KeyError(key)

        def __delitem__(self, key):
            raise KeyError(key)

        def __iter__(self):
            return iter(())

        def __len__(self):
            return 0

    d = Func(lambda v: Dumped(), lambda w: None, Dummy())
    d.update(dict.fromkeys(range(10)))


def test_prepared_update_encodes_once_spills_and_preserves_order(tmp_path):
    calls = []

    def dump(value):
        calls.append(value)
        return value.encode()

    backend = {}
    func = Func(dump, bytes.decode, backend)

    def items():
        yield "k", "first"
        yield "spare", "ok"
        yield "k", "last"
        yield "tail", "end"

    plan = func.prepare_update(items(), memory_window=2, spill_directory=tmp_path)

    # Preparation has already made the single encoder/input pass, but nothing is
    # visible in the backing mapping yet.
    assert calls == ["first", "ok", "last", "end"]
    assert backend == {}
    assert plan.report.spilled_count == 2
    assert plan.report.memory_count == 2
    assert list(tmp_path.iterdir())  # unique spill directory exists before commit

    report = plan.commit()

    # Commit replays encoded values; it must not encode them again.
    assert calls == ["first", "ok", "last", "end"]
    assert report.committed
    assert report.written_keys == ["k", "spare", "k", "tail"]
    assert backend == {
        "k": b"last",
        "spare": b"ok",
        "tail": b"end",
    }
    assert func["k"] == "last"
    assert list(tmp_path.iterdir()) == []


def test_prepared_update_encoding_failure_then_retry(tmp_path):
    calls = []

    def dump(value):
        calls.append(value)
        if value == "bad":
            raise ValueError("synthetic encoder failure")
        return value.encode()

    backend = {}
    func = Func(dump, bytes.decode, backend)

    def bad_items():
        yield "k", "first"
        yield "spare", "ok"
        yield "k", "last"
        yield "tail", "bad"

    with pytest.raises(ValueError) as exc_info:
        func.prepare_update(bad_items(), memory_window=2, spill_directory=tmp_path)

    error = exc_info.value
    assert error.update_stage == "encode"
    assert error.phase == "encode"
    assert error.update_report.error_kind == "encoding"
    assert error.update_report.failed_key == "tail"
    assert error.update_report.reached_write is False
    assert isinstance(error, FuncEncodingError)
    assert not isinstance(error, FuncCommitError)
    assert backend == {}
    assert list(tmp_path.iterdir()) == []

    # Replace only the bad tail by supplying a fresh one-shot stream.  The
    # successful retry has one encoder pass and commit does not double calls.
    calls.clear()
    plan = func.prepare_update(
        iter(
            [
                ("k", "first"),
                ("spare", "ok"),
                ("k", "last"),
                ("tail", "good"),
            ]
        ),
        memory_window=2,
        spill_directory=tmp_path,
    )
    assert calls == ["first", "ok", "last", "good"]
    plan.commit()
    assert calls == ["first", "ok", "last", "good"]
    assert backend["k"] == b"last"
    assert backend["tail"] == b"good"
    assert list(tmp_path.iterdir()) == []


def test_prepared_update_backend_write_failure_is_distinguishable(tmp_path):
    written = []

    class Backend(dict):
        def __setitem__(self, key, value):
            written.append(key)
            if key == "second":
                raise OSError("synthetic second-key write failure")
            super().__setitem__(key, value)

    calls = []

    def dump(value):
        calls.append(value)
        return value.encode()

    backend = Backend()
    func = Func(dump, bytes.decode, backend)

    with pytest.raises(OSError) as exc_info:
        func.update(
            iter([("first", "a"), ("second", "b"), ("third", "c")]),
            memory_window=1,
            spill_directory=tmp_path,
        )

    error = exc_info.value
    assert error.update_stage == "write"
    assert error.phase == "write"
    assert error.update_report.error_kind == "write"
    assert error.update_report.reached_write is True
    assert error.update_report.written_count == 1
    assert error.update_report.written_keys == ["first"]
    assert error.update_report.failed_key == "second"
    assert isinstance(error, FuncCommitError)
    assert not isinstance(error, FuncEncodingError)
    assert written == ["first", "second"]
    assert backend == {"first": b"a"}
    assert list(tmp_path.iterdir()) == []


def test_prepare_only_update_returns_uncommitted_plan(tmp_path):
    backend = {}
    func = Func(lambda v: v.encode(), bytes.decode, backend)
    plan = func.update(
        [("a", "x")],
        prepare=True,
        memory_window=1,
        spill_directory=tmp_path,
    )
    assert plan.report.stage == "ready"
    assert backend == {}
    plan.commit()
    assert backend == {"a": b"x"}
    assert list(tmp_path.iterdir()) == []
