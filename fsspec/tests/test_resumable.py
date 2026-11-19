import hashlib

import pytest

import fsspec
from fsspec.callbacks import Callback
from fsspec.implementations.memory import MemoryFileSystem
from fsspec.resumable import (
    ResumableCopyError,
    ResumeUnsupportedError,
    SourceChangedError,
    TargetConflictError,
    load_checkpoint,
    resumable_copy,
)


class FailAfter(Callback):
    """Callback that raises once a given number of bytes were confirmed,
    simulating an interruption mid-copy."""

    def __init__(self, limit):
        self.limit = limit
        self.seen = 0
        super().__init__()

    def relative_update(self, inc=1):
        self.seen += inc
        if self.seen >= self.limit:
            raise RuntimeError("simulated interruption")


@pytest.fixture
def fs():
    m = MemoryFileSystem()
    yield m
    # memory store is class-level; clean up after ourselves
    for path in list(m.store):
        m.store.pop(path, None)
    m.pseudo_dirs[:] = []


def make_source(fs, data, path="/src"):
    fs.pipe_file(path, data)
    return path


def test_basic_copy_completes_and_cleans_up(fs):
    data = b"hello world" * 1000
    make_source(fs, data)
    out = resumable_copy(fs, "/src", "/dst", blocksize=1024)
    assert fs.cat_file("/dst") == data
    assert out["size"] == len(data)
    assert out["sha256"] == hashlib.sha256(data).hexdigest()
    assert out["resumed"] is False
    # memory mv is copy+remove, not atomic: must not claim atomicity
    assert out["atomic"] is False
    # staging and checkpoint are gone once the target is published
    assert not fs.exists("/dst.fsspec-part")
    assert not fs.exists("/dst.fsspec-resume.json")


def test_empty_file(fs):
    make_source(fs, b"")
    out = resumable_copy(fs, "/src", "/dst")
    assert fs.cat_file("/dst") == b""
    assert out["size"] == 0


def test_short_trailing_block(fs):
    # size is not a multiple of the block size
    data = bytes(range(256)) * 10 + b"end"
    make_source(fs, data)
    out = resumable_copy(fs, "/src", "/dst", blocksize=1024)
    assert len(data) % 1024 != 0
    assert fs.cat_file("/dst") == data
    assert out["size"] == len(data)


def test_resume_after_interruption(fs):
    data = bytes(100_000)
    make_source(fs, data)
    with pytest.raises(RuntimeError, match="simulated interruption"):
        resumable_copy(fs, "/src", "/dst", blocksize=4096,
                       callback=FailAfter(10_000))
    # the final target must not be published before completion
    assert not fs.exists("/dst")
    assert fs.exists("/dst.fsspec-part")
    # the failure checkpoint keeps auditable progress
    ckpt = load_checkpoint(fs, "/dst.fsspec-resume.json")
    assert ckpt.status == "failed"
    assert "simulated interruption" in ckpt.error
    assert 0 < ckpt.confirmed < len(data)
    assert ckpt.source_identity["size"] == len(data)
    assert fs.info("/dst.fsspec-part")["size"] == ckpt.confirmed

    out = resumable_copy(fs, "/src", "/dst", blocksize=4096)
    assert out["resumed"] is True
    assert fs.cat_file("/dst") == data
    assert not fs.exists("/dst.fsspec-resume.json")


def test_resume_does_not_guess_from_target_size(fs):
    # A checkpoint confirming fewer bytes than the staging file holds
    # must be treated as a conflict, not as a resume offset.
    data = b"A" * 50_000
    make_source(fs, data)
    with pytest.raises(RuntimeError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096,
                       callback=FailAfter(4096))
    # append garbage behind the confirmed prefix
    with fs.open("/dst.fsspec-part", "ab") as f:
        f.write(b"garbage")
    with pytest.raises(TargetConflictError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096)


def test_source_changed_errors_and_restart(fs):
    make_source(fs, b"X" * 30_000)
    with pytest.raises(RuntimeError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096,
                       callback=FailAfter(4096))
    # source is replaced by a different version
    new_data = b"Y" * 20_000
    fs.pipe_file("/src", new_data)
    with pytest.raises(SourceChangedError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096)
    # explicit restart copies the new version from scratch
    out = resumable_copy(fs, "/src", "/dst", blocksize=4096,
                         on_source_change="restart")
    assert fs.cat_file("/dst") == new_data
    assert out["size"] == len(new_data)


def test_staging_replaced_errors_and_restart(fs):
    data = b"Z" * 30_000
    make_source(fs, data)
    with pytest.raises(RuntimeError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096,
                       callback=FailAfter(4096))
    # someone replaces the staging file with same-size junk
    confirmed = load_checkpoint(fs, "/dst.fsspec-resume.json").confirmed
    fs.pipe_file("/dst.fsspec-part", b"Q" * confirmed)
    with pytest.raises(TargetConflictError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096)
    out = resumable_copy(fs, "/src", "/dst", blocksize=4096,
                         on_staging_conflict="restart")
    assert fs.cat_file("/dst") == data
    assert out["resumed"] is False


def test_staging_deleted_is_a_conflict(fs):
    make_source(fs, b"d" * 30_000)
    with pytest.raises(RuntimeError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096,
                       callback=FailAfter(4096))
    fs.rm("/dst.fsspec-part")
    with pytest.raises(TargetConflictError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096)


def test_checkpoint_without_staging_starts_cleanly(fs):
    # Simulates a crash right after the checkpoint was first written,
    # before any byte reached the staging file.
    from fsspec.resumable import CopyCheckpoint, source_identity

    data = b"c" * 10_000
    make_source(fs, data)
    ckpt = CopyCheckpoint(
        source="/src",
        target="/dst",
        staging="/dst.fsspec-part",
        source_identity=source_identity(fs, "/src"),
        blocksize=4096,
    )
    ckpt.save(fs, "/dst.fsspec-resume.json")
    out = resumable_copy(fs, "/src", "/dst", blocksize=4096)
    assert fs.cat_file("/dst") == data
    assert out["resumed"] is True


def test_checkpoint_of_other_copy_rejected(fs):
    make_source(fs, b"a" * 10_000)
    with pytest.raises(RuntimeError):
        resumable_copy(fs, "/src", "/dst", blocksize=4096,
                       callback=FailAfter(4096))
    make_source(fs, b"b" * 10_000, path="/other")
    with pytest.raises(ResumableCopyError, match="belongs to copy"):
        resumable_copy(fs, "/other", "/dst", blocksize=4096)


class NoRangeMemoryFileSystem(MemoryFileSystem):
    """Fallback backend: no ranged (conditional) reads."""

    def cat_file(self, path, start=None, end=None, **kwargs):
        if start is not None or end is not None:
            raise ValueError("ranged reads not supported")
        return super().cat_file(path, start=start, end=end, **kwargs)


def test_backend_without_ranged_reads(fs):
    m = NoRangeMemoryFileSystem()
    try:
        data = b"streamed" * 5000
        m.pipe_file("/src", data)
        # a fresh copy works, streaming sequentially from the start
        out = resumable_copy(m, "/src", "/dst", blocksize=4096)
        assert m.cat_file("/dst") == data

        # but an interrupted copy cannot be resumed
        m.pipe_file("/src2", data)
        with pytest.raises(RuntimeError):
            resumable_copy(m, "/src2", "/dst2", blocksize=4096,
                           callback=FailAfter(4096))
        ckpt = load_checkpoint(m, "/dst2.fsspec-resume.json")
        assert ckpt.resume_supported is False
        assert ckpt.confirmed > 0
        with pytest.raises(ResumeUnsupportedError):
            resumable_copy(m, "/src2", "/dst2", blocksize=4096)
    finally:
        for path in list(m.store):
            m.store.pop(path, None)
        m.pseudo_dirs[:] = []


class NoAppendMemoryFileSystem(MemoryFileSystem):
    """Backend without append-mode writes."""

    def _open(self, path, mode="rb", **kwargs):
        if "a" in mode:
            raise ValueError("append not supported")
        return super()._open(path, mode=mode, **kwargs)


def test_backend_without_append_fails_explicitly(fs):
    m = NoAppendMemoryFileSystem()
    try:
        m.pipe_file("/src", b"data" * 1000)
        with pytest.raises(ResumeUnsupportedError, match="append"):
            resumable_copy(m, "/src", "/dst", blocksize=1024)
        # nothing was published
        assert not m.exists("/dst")
    finally:
        for path in list(m.store):
            m.store.pop(path, None)
        m.pseudo_dirs[:] = []


def test_local_filesystem_atomic_publish(tmp_path):
    local = fsspec.filesystem("file", auto_mkdir=True)
    src = tmp_path / "in.bin"
    dst = tmp_path / "out.bin"
    data = bytes(range(256)) * 100
    src.write_bytes(data)
    out = local.cp_file_resumable(str(src), str(dst), blocksize=1024)
    assert dst.read_bytes() == data
    # local same-directory rename is atomic
    assert out["atomic"] is True


def test_local_resume_after_interruption(tmp_path):
    local = fsspec.filesystem("file", auto_mkdir=True)
    src = tmp_path / "in.bin"
    dst = tmp_path / "out.bin"
    data = b"L" * 40_000
    src.write_bytes(data)
    with pytest.raises(RuntimeError):
        local.cp_file_resumable(str(src), str(dst), blocksize=4096,
                                callback=FailAfter(8192))
    assert not dst.exists()
    out = local.cp_file_resumable(str(src), str(dst), blocksize=4096)
    assert out["resumed"] is True
    assert dst.read_bytes() == data