import pytest

import fsspec
from fsspec.exceptions import CopyIncompleteError
from fsspec.generic import (
    NATIVE_COPY,
    READ_WRITE_FALLBACK,
    CopyPlan,
    CopyResult,
    rsync,
)
from fsspec.implementations.memory import MemoryFileSystem
from fsspec.registry import _registry, register_implementation
from fsspec.spec import CopyCapabilities
from fsspec.tests.conftest import data, server  # noqa: F401


def test_remote_async_ops(server):
    fsspec.filesystem("http", headers={"give_length": "true", "head_ok": "true"})
    fs = fsspec.filesystem("generic", default_method="current")
    out = fs.info(server.realfile)
    assert out["size"] == len(data)
    assert out["type"] == "file"
    assert fs.isfile(server.realfile)  # this method from superclass


def test_touch_rm(m):
    m.touch("afile")
    m.touch("dir/afile")

    fs = fsspec.filesystem("generic", default_method="current")
    fs.rm("memory://afile")
    assert not m.exists("afile")

    fs.rm("memory://dir", recursive=True)
    assert not m.exists("dir/afile")
    assert not m.exists("dir")


def test_cp_async_to_sync(server, m):
    fsspec.filesystem("http", headers={"give_length": "true", "head_ok": "true"})
    fs = fsspec.filesystem("generic", default_method="current")
    fs.cp([server.realfile], ["memory://realfile"])
    assert m.cat("realfile") == data

    fs.rm("memory://realfile")
    assert not m.exists("realfile")


def test_pipe_cat_sync(m):
    fs = fsspec.filesystem("generic", default_method="current")
    fs.pipe("memory://afile", b"data")
    assert fs.cat("memory://afile") == b"data"


def test_cat_async(server):
    fsspec.filesystem("http", headers={"give_length": "true", "head_ok": "true"})
    fs = fsspec.filesystem("generic", default_method="current")
    assert fs.cat(server.realfile) == data


@pytest.mark.parametrize(
    "method,as_list",
    [("copy", True), ("copy", False), ("cp", True), ("cp_file", False)],
)
def test_cp_one(server, tmpdir, method, as_list):
    fsspec.filesystem("http", headers={"give_length": "true", "head_ok": "true"})
    local = fsspec.filesystem("file")
    fn = f"file://{tmpdir}/afile"
    fs = fsspec.filesystem("generic", default_method="current")

    source, destination = (
        ([server.realfile], [fn]) if as_list else (server.realfile, fn)
    )
    getattr(fs, method)(source, destination)
    assert local.cat(fn) == data
    fs.rm(fn)
    assert not fs.exists(fn)


@pytest.mark.parametrize("mixed", ["source", "destination", "both"])
@pytest.mark.parametrize("reverse", [False, True])
def test_copy_mixed_protocols(m, tmp_path, mixed, reverse):
    m.pipe_file("source-a", b"memory-a")
    m.pipe_file("source-b", b"memory-b")
    local_source = tmp_path / "source-b"
    local_source.write_bytes(b"local-b")
    local_destination = tmp_path / "destination-b"
    fsspec.filesystem("file")
    fs = fsspec.filesystem("generic", default_method="current")
    sources = ["memory://source-a", "memory://source-b"]
    destinations = ["memory://destination-a", "memory://destination-b"]
    if mixed in ("source", "both"):
        sources[1] = local_source.as_uri()
    if mixed in ("destination", "both"):
        destinations[1] = local_destination.as_uri()
    if reverse:
        sources.reverse()
        destinations.reverse()

    side = "destination" if mixed == "destination" else "source"
    with pytest.raises(ValueError, match=f"Multiple {side} backends"):
        fs.copy(sources, destinations, on_error="raise")

    assert not m.exists("destination-a")
    assert not m.exists("destination-b")
    assert not m.exists(local_destination.as_uri())
    assert not local_destination.exists()


def test_copy_mixed_async_source(server, m):
    fsspec.filesystem("http", headers={"give_length": "true", "head_ok": "true"})
    m.pipe_file("source", b"memory-data")
    fs = fsspec.filesystem("generic", default_method="current")

    with pytest.raises(ValueError, match="Multiple source backends"):
        fs.copy(
            [server.realfile, "memory://source"],
            ["memory://from-http", "memory://from-memory"],
            batch_size=1,
            on_error="raise",
        )

    assert not m.exists("from-http")
    assert not m.exists("from-memory")


@pytest.mark.parametrize("local", [False, True])
def test_copy_homogeneous_batches(m, tmp_path, local):
    m.pipe({"source-a": b"a", "source-b": b"b"})
    if local:
        fsspec.filesystem("file")
        destinations = [(tmp_path / name).as_uri() for name in ("a", "b")]
        # Different protocol aliases still resolve to the same backend.
        destinations[1] = destinations[1].replace("file://", "local://", 1)
    else:
        destinations = ["memory://a", "memory://b"]
    fs = fsspec.filesystem("generic", default_method="current")

    fs.copy(["memory://source-a", "memory://source-b"], destinations, on_error="raise")

    if local:
        assert (tmp_path / "a").read_bytes() == b"a"
        assert (tmp_path / "b").read_bytes() == b"b"
    else:
        assert m.cat_file("a") == b"a"
        assert m.cat_file("b") == b"b"


@pytest.mark.parametrize("method", ["copy", "cp_file"])
def test_copy_preserves_tempdir(method, m, tmp_path):
    tempdir = tmp_path / "staging"
    tempdir.mkdir()
    sentinel = tempdir / "unrelated"
    sentinel.write_bytes(b"keep me")
    destination = tmp_path / "copied"
    m.pipe_file("source", b"data")
    fs = fsspec.filesystem("generic")

    getattr(fs, method)("memory://source", destination.as_uri(), tempdir=str(tempdir))

    assert destination.read_bytes() == b"data"
    assert tempdir.is_dir()
    assert sentinel.read_bytes() == b"keep me"
    assert list(tempdir.iterdir()) == [sentinel]


@pytest.mark.parametrize("method", ["copy", "cp_file"])
@pytest.mark.parametrize("missing_source", [True, False])
def test_copy_preserves_tempdir_on_error(method, missing_source, m, tmp_path):
    tempdir = tmp_path / "staging"
    tempdir.mkdir()
    sentinel = tempdir / "unrelated"
    sentinel.write_bytes(b"keep me")
    destination = tmp_path / "missing" / "copied"
    if not missing_source:
        m.pipe_file("source", b"data")
    fs = fsspec.filesystem("generic")

    with pytest.raises(FileNotFoundError):
        getattr(fs, method)(
            "memory://source",
            destination.as_uri(),
            tempdir=str(tempdir),
            on_error="raise",
        )

    assert not destination.exists()
    assert tempdir.is_dir()
    assert sentinel.read_bytes() == b"keep me"
    assert list(tempdir.iterdir()) == [sentinel]


def test_rsync_rejects_names_escaping_destination(tmp_path, m):
    m.pipe({"/src/ok": b"ok", "/src/../../escaped": b"data"})
    dest = tmp_path / "out" / "dest"
    dest.mkdir(parents=True)

    with pytest.raises(ValueError, match="outside the destination"):
        rsync("memory://src", f"file://{dest}")
    assert not (tmp_path / "escaped").exists()
    assert not (dest / "ok").exists()


@pytest.mark.parametrize("relative", ["ordinary.txt", "nested/memory:///src/data"])
def test_rsync_preserves_source_url_in_relative_name(m, relative):
    source = f"memory:///src/{relative}"
    destination = f"memory:///dst/{relative}"
    m.pipe_file(source, b"payload")

    copied = rsync("memory:///src", "memory:///dst")

    assert copied == {source: destination}
    assert m.cat(destination) == b"payload"
    assert m.find("/dst") == [destination.removeprefix("memory://")]


@pytest.mark.parametrize("relative", ["ordinary.txt", "nested/memory:///dst/data"])
def test_rsync_delete_missing_preserves_destination_url_in_relative_name(m, relative):
    source = f"memory:///src/{relative}"
    destination = f"memory:///dst/{relative}"
    m.pipe(
        {source: b"payload", destination: b"payload", "memory:///dst/obsolete": b"old"}
    )

    assert rsync("memory:///src", "memory:///dst", delete_missing=True) == {}

    assert m.cat(destination) == b"payload"
    assert not m.exists("/dst/obsolete")


def test_rsync(tmpdir, m):
    from fsspec.generic import GenericFileSystem, rsync

    fs = GenericFileSystem()
    fs.pipe("memory:///deep/path/afile", b"data1")
    fs.pipe("memory:///deep/afile", b"data2")

    with pytest.raises(ValueError):
        rsync("memory:///deep/afile", f"file://{tmpdir}")
    rsync("memory://", f"file://{tmpdir}")

    allfiles = fs.find(f"file://{tmpdir}", withdirs=True, detail=True)
    pos_tmpdir = fsspec.implementations.local.make_path_posix(str(tmpdir))  # for WIN
    assert set(allfiles) == {
        f"file://{pos_tmpdir}{_}"
        for _ in [
            "",
            "/deep",
            "/deep/path",
            "/deep/path/afile",
            "/deep/afile",
        ]
    }
    fs.rm("memory:///deep/afile")
    rsync("memory://", f"file://{tmpdir}", delete_missing=True)
    allfiles2 = fs.find(f"file://{tmpdir}", withdirs=True, detail=True)
    assert set(allfiles2) == {
        f"file://{pos_tmpdir}{_}"
        for _ in [
            "",
            "/deep",
            "/deep/path",
            "/deep/path/afile",
        ]
    }
    # the file was not updated, since size was correct
    assert (
        allfiles[f"file://{pos_tmpdir}/deep/path/afile"]
        == allfiles2[f"file://{pos_tmpdir}/deep/path/afile"]
    )


class _OptionsRequiredFS(MemoryFileSystem):
    """Backend that raises unless its storage_options reached the constructor."""

    protocol = "optsrequired"

    def __init__(self, *args, token=None, **kwargs):
        if token is None:
            raise ValueError("storage_options were not propagated")
        super().__init__(*args, **kwargs)

    @classmethod
    def _strip_protocol(cls, path):
        if isinstance(path, str):
            path = path.removeprefix(f"{cls.protocol}://")
        return MemoryFileSystem._strip_protocol(path)


@pytest.fixture()
def opts_fs(m):
    register_implementation(
        _OptionsRequiredFS.protocol, _OptionsRequiredFS, clobber=True
    )
    m.pipe_file("/afile", b"hello")
    m.makedirs("/adir", exist_ok=True)
    m.pipe_file("/adir/nested", b"world")
    yield fsspec.filesystem(
        "generic",
        default_method="options",
        storage_options={_OptionsRequiredFS.protocol: {"token": "set"}},
    )
    _registry.pop(_OptionsRequiredFS.protocol, None)
    _OptionsRequiredFS.clear_instance_cache()


def test_storage_options_propagated_to_backend(opts_fs):
    opts_fs.info("optsrequired:///afile")
    opts_fs.ls("optsrequired:///adir")
    opts_fs.cat_file("optsrequired:///afile")
    opts_fs.isdir("optsrequired:///adir")
    opts_fs.find("optsrequired:///adir")


def test_storage_options_propagated_cross_protocol(opts_fs, tmpdir):
    rsync(
        "optsrequired:///adir",
        f"file://{tmpdir}",
        inst_kwargs={
            "default_method": "options",
            "storage_options": {_OptionsRequiredFS.protocol: {"token": "set"}},
        },
    )
    assert (tmpdir / "nested").read_binary() == b"world"


class _RecordingMemoryFS(MemoryFileSystem):
    """Memory backend that records which copy path was exercised."""

    protocol = "recmem"

    def __init__(self, *args, **kwargs):
        super().__init__(*args, global_store=False, **kwargs)
        self.native_calls = []
        self.get_calls = []
        self.put_calls = []
        self._capabilities = CopyCapabilities(
            native_copy=True, versioned_copy=False, metadata="reset"
        )

    def copy_capabilities(self):
        return self._capabilities

    def cp_file(self, path1, path2, **kwargs):
        self.native_calls.append((path1, path2))
        return super().cp_file(path1, path2, **kwargs)

    def get_file(self, path1, path2, **kwargs):
        self.get_calls.append(path1)
        return super().get_file(path1, path2, **kwargs)

    def put_file(self, path1, path2, **kwargs):
        self.put_calls.append(path2)
        return super().put_file(path1, path2, **kwargs)


def _patch_backends(monkeypatch, routing):
    """Make generic URL resolution return specific backend instances."""
    import fsspec.generic as generic

    def resolve(url, method, protocol=None, storage_options=None):
        url = url[0] if isinstance(url, (list, tuple)) else url
        for prefix, backend in routing.items():
            if url.startswith(prefix):
                return backend
        raise KeyError(url)

    monkeypatch.setattr(generic, "_resolve_fs", resolve)


def test_plan_native_copy_for_same_backend_instance(m, monkeypatch):
    fs = fsspec.filesystem("generic", default_method="current")
    m.pipe_file("source", b"payload")

    plan = fs.plan_copy("memory://source", "memory://target")

    assert isinstance(plan, CopyPlan)
    assert plan.mode == NATIVE_COPY
    assert plan.backend is m
    assert plan.metadata == "reset"  # MemoryFS declares this explicitly
    explanation = plan.explain()
    assert "native" in explanation
    assert "metadata handling" in explanation


def test_plan_does_not_equate_protocol_strings_with_instances(monkeypatch):
    source_fs = _RecordingMemoryFS(skip_instance_cache=True)
    dest_fs = _RecordingMemoryFS(skip_instance_cache=True)
    source_fs.pipe_file("source", b"payload")
    _patch_backends(
        monkeypatch,
        {"memory://source": source_fs, "memory://target": dest_fs},
    )
    fs = fsspec.filesystem("generic")

    plan = fs.plan_copy("memory://source", "memory://target")

    assert plan.mode == READ_WRITE_FALLBACK
    assert plan.operations[0].source_fs is source_fs
    assert plan.operations[0].destination_fs is dest_fs
    assert "different storage instances" in plan.explain()


def test_version_requirement_forces_fallback_without_backend_support(m):
    fs = fsspec.filesystem("generic", default_method="current")
    m.pipe_file("source", b"payload")

    plan = fs.plan_copy(
        "memory://source", "memory://target", version_id="v1"
    )

    assert plan.mode == READ_WRITE_FALLBACK
    assert plan.version_kwargs == {"version_id": "v1"}
    assert "version" in plan.explain().lower()


def test_native_copy_allowed_when_version_support_declared(monkeypatch):
    backend = _RecordingMemoryFS(skip_instance_cache=True)
    backend._capabilities = CopyCapabilities(
        native_copy=True, versioned_copy=True, metadata="preserved"
    )
    backend.pipe_file("source", b"payload")
    _patch_backends(
        monkeypatch, {"memory://source": backend, "memory://target": backend}
    )
    fs = fsspec.filesystem("generic")

    plan = fs.plan_copy(
        "memory://source", "memory://target", source_version_id="v7"
    )

    assert plan.mode == NATIVE_COPY
    assert plan.metadata == "preserved"
    assert plan.version_kwargs == {"source_version_id": "v7"}


def test_declared_native_unsupport_plans_fallback_without_side_effects(
    monkeypatch,
):
    backend = _RecordingMemoryFS(skip_instance_cache=True)
    backend._capabilities = CopyCapabilities(native_copy=False)
    backend.pipe_file("source", b"payload")
    _patch_backends(
        monkeypatch, {"memory://source": backend, "memory://target": backend}
    )
    fs = fsspec.filesystem("generic")

    plan = fs.plan_copy("memory://source", "memory://target")
    result = fs.copy("memory://source", "memory://target", on_error="raise")

    assert plan.mode == READ_WRITE_FALLBACK
    assert backend.native_calls == []  # native path never attempted
    assert backend.get_calls == ["memory://source"]
    assert backend.put_calls == ["memory://target"]
    assert backend.cat_file("/target") == b"payload"
    assert isinstance(result, CopyResult)
    assert result.completed == ["memory://target"]


def test_runtime_not_implemented_falls_back_before_side_effects(monkeypatch):
    backend = _RecordingMemoryFS(skip_instance_cache=True)
    # No declaration either way: the native method is attempted first.
    backend._capabilities = CopyCapabilities(native_copy=None)
    backend.pipe_file("source", b"payload")
    backend.cp_file = lambda *a, **k: (_ for _ in ()).throw(NotImplementedError("no"))
    _patch_backends(
        monkeypatch, {"memory://source": backend, "memory://target": backend}
    )
    fs = fsspec.filesystem("generic")

    result = fs.copy("memory://source", "memory://target", on_error="raise")

    assert result.fallback_used is True
    assert result.native_fallback is True
    assert result.plan.mode == READ_WRITE_FALLBACK
    assert backend.get_calls == ["memory://source"]
    assert backend.put_calls == ["memory://target"]
    assert backend.cat_file("/target") == b"payload"


def test_native_failure_after_partial_write_is_reported_not_retried(
    monkeypatch,
):
    backend = _RecordingMemoryFS(skip_instance_cache=True)
    backend.pipe_file("a", b"1")
    backend.pipe_file("b", b"22")

    def flaky_copy(path1, path2, **kwargs):
        # First file of the batch lands, then the backend blows up.
        MemoryFileSystem.cp_file(backend, path1[0], path2[0])
        raise RuntimeError("backend connection lost mid-batch")

    backend.copy = flaky_copy
    _patch_backends(
        monkeypatch,
        {
            "memory://a": backend,
            "memory://b": backend,
            "memory://ta": backend,
            "memory://tb": backend,
        },
    )
    fs = fsspec.filesystem("generic")

    with pytest.raises(CopyIncompleteError) as exc_info:
        fs.copy(
            ["memory://a", "memory://b"],
            ["memory://ta", "memory://tb"],
            on_error="raise",
        )

    err = exc_info.value
    assert set(err.failures) == {"memory://ta", "memory://tb"}
    assert "retry" in str(err).lower()
    # Nothing was retried through the read/write path.
    assert backend.get_calls == []
    assert backend.put_calls == []
    assert backend.isfile("/ta")  # partial write is reported, not hidden


def test_memory_double_observes_native_then_fallback_path(monkeypatch):
    source_fs = _RecordingMemoryFS(skip_instance_cache=True)
    dest_fs = _RecordingMemoryFS(skip_instance_cache=True)
    source_fs.pipe_file("source", b"payload")
    _patch_backends(
        monkeypatch,
        {"memory://source": source_fs, "memory://target": dest_fs},
    )
    fs = fsspec.filesystem("generic")

    fs.cp_file("memory://source", "memory://target")

    # Same protocol, different instances: streaming must be observable.
    assert source_fs.native_calls == []
    assert source_fs.get_calls == ["memory://source"]
    assert dest_fs.put_calls == ["memory://target"]
    assert dest_fs.native_calls == []
    assert dest_fs.cat_file("/target") == b"payload"


def test_copy_result_verify_does_not_overclaim_metadata(monkeypatch):
    backend = _RecordingMemoryFS(skip_instance_cache=True)
    backend.pipe_file("source", b"payload")
    _patch_backends(
        monkeypatch, {"memory://source": backend, "memory://target": backend}
    )
    fs = fsspec.filesystem("generic")

    result = fs.cp_file("memory://source", "memory://target")
    reports = result.verify()

    assert len(reports) == 1
    report = reports[0]
    assert report.existence == "verified"
    assert report.size == "verified"
    # MemoryFS declares metadata="reset", so preservation must not be claimed.
    assert report.metadata == "not_verified"


def test_verify_reports_guarantee_only_when_backend_promises_it(monkeypatch):
    backend = _RecordingMemoryFS(skip_instance_cache=True)
    backend._capabilities = CopyCapabilities(
        native_copy=True, metadata="preserved"
    )
    backend.pipe_file("source", b"payload")
    _patch_backends(
        monkeypatch, {"memory://source": backend, "memory://target": backend}
    )
    fs = fsspec.filesystem("generic")

    result = fs.cp_file("memory://source", "memory://target")
    report = result.verify()[0]

    # The generic layer did not inspect metadata itself; it only relays the
    # backend's declared guarantee.
    assert report.metadata == "guaranteed"
    assert report.size == "verified"


def test_copy_capabilities_validation():
    with pytest.raises(ValueError):
        CopyCapabilities(metadata="magic")
    with pytest.raises(ValueError):
        CopyCapabilities(native_copy=False, versioned_copy=True)
    # Conservative default: no promises at all.
    default = CopyCapabilities()
    assert default.native_copy is None
    assert default.versioned_copy is False
    assert default.metadata == "unknown"


def test_plan_copy_rejects_mixed_backends_before_any_write(m, tmp_path):
    m.pipe_file("source-a", b"a")
    m.pipe_file("source-b", b"b")
    local_source = tmp_path / "source-b"
    local_source.write_bytes(b"b")
    fsspec.filesystem("file")
    fs = fsspec.filesystem("generic", default_method="current")

    with pytest.raises(ValueError, match="Multiple source backends"):
        fs.plan_copy(
            ["memory://source-a", local_source.as_uri()],
            ["memory://da", "memory://db"],
        )

    assert not m.exists("da")
    assert not m.exists("db")

