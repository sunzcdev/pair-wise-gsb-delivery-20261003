from __future__ import annotations

import inspect
import logging
import os
import uuid
from dataclasses import dataclass, field

from .asyn import AsyncFileSystem, _run_coros_in_chunks, sync_wrapper
from .callbacks import DEFAULT_CALLBACK
from .core import filesystem, get_filesystem_class, split_protocol, url_to_fs
from .exceptions import CopyIncompleteError
from .utils import check_contained

_generic_fs = {}
logger = logging.getLogger("fsspec.generic")

#: kwargs that pin a specific object version on either side of a copy
VERSION_COPY_KWARGS = (
    "version_id",
    "source_version_id",
    "target_version_id",
    "version_aware",
)

#: the backend performs the whole copy itself, without bytes passing through us
NATIVE_COPY = "native"
#: bytes are read from the source and written to the destination by fsspec
READ_WRITE_FALLBACK = "read_write"


def set_generic_fs(protocol, **storage_options):
    """Populate the dict used for method=="generic" lookups"""
    _generic_fs[protocol] = filesystem(protocol, **storage_options)


def _resolve_fs(url, method, protocol=None, storage_options=None):
    """Pick instance of backend FS"""
    url = url[0] if isinstance(url, (list, tuple)) else url
    protocol = protocol or split_protocol(url)[0]
    storage_options = storage_options or {}
    if method == "default":
        return filesystem(protocol)
    if method == "generic":
        return _generic_fs[protocol]
    if method == "current":
        cls = get_filesystem_class(protocol)
        return cls.current()
    if method == "options":
        fs, _ = url_to_fs(url, **storage_options.get(protocol, {}))
        return fs
    raise ValueError(f"Unknown FS resolution method: {method}")


def _fs_label(fs):
    """Human-readable identity of a storage *instance*.

    Two backends sharing a protocol string (e.g. two S3 accounts, two memory
    stores) are not the same instance, so the object id is included.
    """
    proto = fs.protocol if isinstance(fs.protocol, str) else fs.protocol[0]
    return f"{proto}:{type(fs).__name__}#{id(fs):x}"


def _version_kwargs(kwargs):
    return {key: kwargs[key] for key in VERSION_COPY_KWARGS if key in kwargs}


@dataclass(frozen=True)
class CopyOperation:
    """One source/destination pair plus the resolved backend instances."""

    source: str
    destination: str
    source_fs: object
    destination_fs: object
    kwargs: dict = field(default_factory=dict)


@dataclass(frozen=True)
class CopyPlan:
    """The route chosen for a copy batch, before any side effect happens.

    ``mode`` is :data:`NATIVE_COPY` when the same backend instance performs the
    whole copy, or :data:`READ_WRITE_FALLBACK` when fsspec must stream the bytes
    itself. Every reason that contributed to the decision is recorded, so the
    plan can explain itself via :meth:`explain`.
    """

    operations: tuple[CopyOperation, ...]
    mode: str
    reasons: tuple[str, ...]
    backend: object | None = None
    capabilities: object | None = None
    metadata: str = "unknown"
    version_kwargs: dict = field(default_factory=dict)

    def explain(self):
        """Return a human-readable explanation of the chosen route."""
        routes = {
            "native": "backend-native server-side copy",
            "read_write": "read/write fallback (bytes streamed through fsspec)",
        }
        lines = [
            f"Copy plan: {routes[self.mode]} for {len(self.operations)} file(s)"
        ]
        first = self.operations[0]
        lines.append(
            f"  source backend:      {_fs_label(first.source_fs)}"
        )
        lines.append(
            f"  destination backend: {_fs_label(first.destination_fs)}"
        )
        if self.version_kwargs:
            lines.append(f"  version arguments:   {self.version_kwargs}")
        lines.append(f"  metadata handling:   {self.metadata} (not re-verified)")
        for reason in self.reasons:
            lines.append(f"  - {reason}")
        return "\n".join(lines)


@dataclass
class VerificationReport:
    """What was actually checked after a copy.

    A property only appears as ``"verified"`` when fsspec itself performed the
    check. Guarantees made by the backend's declared capabilities are reported
    as ``"guaranteed"``; everything that cannot be proven is ``"not_verified"``.
    """

    destination: str
    existence: str
    size: str
    metadata: str
    detail: dict = field(default_factory=dict)


@dataclass
class CopyResult:
    """Outcome of executing a :class:`CopyPlan`."""

    plan: CopyPlan
    completed: list = field(default_factory=list)
    failures: dict = field(default_factory=dict)
    attempted: list = field(default_factory=list)
    fallback_used: bool = False
    native_fallback: bool = False

    @property
    def incomplete(self):
        """Destinations that were attempted but not confirmed complete."""
        return [u for u in self.attempted if u not in self.completed]

    def _raise_if_incomplete(self):
        if not self.failures and not self.incomplete:
            return
        # When every attempt failed and nothing was written, surface the
        # original exception rather than hiding it behind a wrapper.
        if not self.completed and len(self.failures) == len(self.attempted):
            raise next(iter(self.failures.values()))
        raise CopyIncompleteError(
            "Copy did not complete for all destinations; no retry was "
            "attempted because some targets may already have been written",
            completed=self.completed,
            incomplete=self.incomplete,
            failures=self.failures,
        )

    def verify(self):
        """Check the destinations that were reported complete.

        Existence and size are checked when the source size is known. Metadata
        is never inspected by the generic layer: it is only reported as
        guaranteed when the backend's capabilities promise preservation.
        """
        reports = []
        for op in self.plan.operations:
            if op.destination not in self.completed:
                continue
            size_state = "not_verified"
            existence_state = "not_verified"
            detail = {}
            try:
                info = op.destination_fs.info(op.destination)
            except Exception as exc:  # nothing else safe to claim
                reports.append(
                    VerificationReport(
                        op.destination,
                        existence="not_verified",
                        size="not_verified",
                        metadata="not_verified",
                        detail={"error": repr(exc)},
                    )
                )
                continue
            existence_state = "verified" if info else "not_verified"
            try:
                source_info = op.source_fs.info(op.source)
                if (
                    "size" in source_info
                    and "size" in info
                    and source_info["size"] == info["size"]
                ):
                    size_state = "verified"
                    detail["size"] = info["size"]
            except Exception:
                pass
            if self.plan.metadata == "preserved":
                metadata_state = "guaranteed"
            else:
                metadata_state = "not_verified"
            reports.append(
                VerificationReport(
                    op.destination,
                    existence_state,
                    size_state,
                    metadata_state,
                    detail=detail,
                )
            )
        return reports


def rsync(
    source,
    destination,
    delete_missing=False,
    source_field="size",
    dest_field="size",
    update_cond="different",
    inst_kwargs=None,
    fs=None,
    **kwargs,
):
    """Sync files between two directory trees

    (experimental)

    Parameters
    ----------
    source: str
        Root of the directory tree to take files from. This must be a directory, but
        do not include any terminating "/" character
    destination: str
        Root path to copy into. The contents of this location should be
        identical to the contents of ``source`` when done. This will be made a
        directory, and the terminal "/" should not be included.
    delete_missing: bool
        If there are paths in the destination that don't exist in the
        source and this is True, delete them. Otherwise, leave them alone.
    source_field: str | callable
        If ``update_field`` is "different", this is the key in the info
        of source files to consider for difference. Maybe a function of the
        info dict.
    dest_field: str | callable
        If ``update_field`` is "different", this is the key in the info
        of destination files to consider for difference. May be a function of
        the info dict.
    update_cond: "different"|"always"|"never"
        If "always", every file is copied, regardless of whether it exists in
        the destination. If "never", files that exist in the destination are
        not copied again. If "different" (default), only copy if the info
        fields given by ``source_field`` and ``dest_field`` (usually "size")
        are different. Other comparisons may be added in the future.
    inst_kwargs: dict|None
        If ``fs`` is None, use this set of keyword arguments to make a
        GenericFileSystem instance
    fs: GenericFileSystem|None
        Instance to use if explicitly given. The instance defines how to
        to make downstream file system instances from paths.

    Returns
    -------
    dict of the copy operations that were performed, {source: destination}
    """
    fs = fs or GenericFileSystem(**(inst_kwargs or {}))
    source = fs._strip_protocol(source)
    destination = fs._strip_protocol(destination)
    allfiles = fs.find(source, withdirs=True, detail=True)
    if not fs.isdir(source):
        raise ValueError("Can only rsync on a directory")
    # The names came from the source listing; ".." in one of them would
    # otherwise place the copy above the destination.
    check_contained(destination, [a.replace(source, destination, 1) for a in allfiles])
    otherfiles = fs.find(destination, withdirs=True, detail=True)
    dirs = [
        a
        for a, v in allfiles.items()
        if v["type"] == "directory"
        and a.replace(source, destination, 1) not in otherfiles
    ]
    logger.debug(f"{len(dirs)} directories to create")
    if dirs:
        fs.make_many_dirs(
            [dirn.replace(source, destination, 1) for dirn in dirs], exist_ok=True
        )
    allfiles = {a: v for a, v in allfiles.items() if v["type"] == "file"}
    logger.debug(f"{len(allfiles)} files to consider for copy")
    to_delete = [
        o
        for o, v in otherfiles.items()
        if o.replace(destination, source, 1) not in allfiles and v["type"] == "file"
    ]
    for k, v in allfiles.copy().items():
        otherfile = k.replace(source, destination, 1)
        if otherfile in otherfiles:
            if update_cond == "always":
                allfiles[k] = otherfile
            elif update_cond == "never":
                allfiles.pop(k)
            elif update_cond == "different":
                inf1 = source_field(v) if callable(source_field) else v[source_field]
                v2 = otherfiles[otherfile]
                inf2 = dest_field(v2) if callable(dest_field) else v2[dest_field]
                if inf1 != inf2:
                    # details mismatch, make copy
                    allfiles[k] = otherfile
                else:
                    # details match, don't copy
                    allfiles.pop(k)
        else:
            # file not in target yet
            allfiles[k] = otherfile
    logger.debug(f"{len(allfiles)} files to copy")
    if allfiles:
        source_files, target_files = zip(*allfiles.items())
        fs.cp(source_files, target_files, **kwargs)
    logger.debug(f"{len(to_delete)} files to delete")
    if delete_missing and to_delete:
        fs.rm(to_delete)
    return allfiles


class GenericFileSystem(AsyncFileSystem):
    """Wrapper over all other FS types

    <experimental!>

    This implementation is a single unified interface to be able to run FS operations
    over generic URLs, and dispatch to the specific implementations using the URL
    protocol prefix.

    Note: instances of this FS are always async, even if you never use it with any async
    backend.
    """

    protocol = "generic"  # there is no real reason to ever use a protocol with this FS

    def __init__(self, default_method="default", storage_options=None, **kwargs):
        """

        Parameters
        ----------
        default_method: str (optional)
            Defines how to configure backend FS instances. Options are:
            - "default": instantiate like FSClass(), with no
              extra arguments; this is the default instance of that FS, and can be
              configured via the config system
            - "generic": takes instances from the `_generic_fs` dict in this module,
              which you must populate before use. Keys are by protocol
            - "options": expects storage_options, a dict mapping protocol to
              kwargs to use when constructing the filesystem
            - "current": takes the most recently instantiated version of each FS
        """
        self.method = default_method
        self.st_opts = storage_options
        super().__init__(**kwargs)

    def _parent(self, path):
        fs = _resolve_fs(path, self.method, storage_options=self.st_opts)
        return fs.unstrip_protocol(fs._parent(path))

    def _strip_protocol(self, path):
        # normalization only
        fs = _resolve_fs(path, self.method, storage_options=self.st_opts)
        return fs.unstrip_protocol(fs._strip_protocol(path))

    async def _find(self, path, maxdepth=None, withdirs=False, detail=False, **kwargs):
        fs = _resolve_fs(path, self.method, storage_options=self.st_opts)
        if fs.async_impl:
            out = await fs._find(
                path, maxdepth=maxdepth, withdirs=withdirs, detail=True, **kwargs
            )
        else:
            out = fs.find(
                path, maxdepth=maxdepth, withdirs=withdirs, detail=True, **kwargs
            )
        result = {}
        for k, v in out.items():
            v = v.copy()  # don't corrupt target FS dircache
            name = fs.unstrip_protocol(k)
            v["name"] = name
            result[name] = v
        if detail:
            return result
        return list(result)

    async def _info(self, url, **kwargs):
        fs = _resolve_fs(url, self.method, storage_options=self.st_opts)
        if fs.async_impl:
            out = await fs._info(url, **kwargs)
        else:
            out = fs.info(url, **kwargs)
        out = out.copy()  # don't edit originals
        out["name"] = fs.unstrip_protocol(out["name"])
        return out

    async def _ls(
        self,
        url,
        detail=True,
        **kwargs,
    ):
        fs = _resolve_fs(url, self.method, storage_options=self.st_opts)
        if fs.async_impl:
            out = await fs._ls(url, detail=True, **kwargs)
        else:
            out = fs.ls(url, detail=True, **kwargs)
        out = [o.copy() for o in out]  # don't edit originals
        for o in out:
            o["name"] = fs.unstrip_protocol(o["name"])
        if detail:
            return out
        else:
            return [o["name"] for o in out]

    async def _cat_file(
        self,
        url,
        **kwargs,
    ):
        fs = _resolve_fs(url, self.method, storage_options=self.st_opts)
        if fs.async_impl:
            return await fs._cat_file(url, **kwargs)
        else:
            return fs.cat_file(url, **kwargs)

    async def _pipe_file(
        self,
        path,
        value,
        **kwargs,
    ):
        fs = _resolve_fs(path, self.method, storage_options=self.st_opts)
        if fs.async_impl:
            return await fs._pipe_file(path, value, **kwargs)
        else:
            return fs.pipe_file(path, value, **kwargs)

    async def _rm(self, url, **kwargs):
        urls = url
        if isinstance(urls, str):
            urls = [urls]
        fs = _resolve_fs(urls[0], self.method, storage_options=self.st_opts)
        if fs.async_impl:
            await fs._rm(urls, **kwargs)
        else:
            fs.rm(url, **kwargs)

    async def _makedirs(self, path, exist_ok=False):
        logger.debug("Make dir %s", path)
        fs = _resolve_fs(path, self.method, storage_options=self.st_opts)
        if fs.async_impl:
            await fs._makedirs(path, exist_ok=exist_ok)
        else:
            fs.makedirs(path, exist_ok=exist_ok)

    def rsync(self, source, destination, **kwargs):
        """Sync files between two directory trees

        See `func:rsync` for more details.
        """
        rsync(source, destination, fs=self, **kwargs)

    def plan_copy(self, path1, path2, **kwargs):
        """Decide how a copy batch will be executed, without performing it.

        The decision is based on the resolved backend *instances* (two
        backends using the same protocol string are treated as different
        stores), any version arguments and the capabilities the backends
        declare. No target is touched while building the plan.

        Returns
        -------
        CopyPlan
        """
        path1 = [path1] if isinstance(path1, str) else list(path1)
        path2 = [path2] if isinstance(path2, str) else list(path2)
        if len(path1) != len(path2):
            raise ValueError("source and destination lists must have equal length")

        sources = [
            _resolve_fs(u, self.method, storage_options=self.st_opts) for u in path1
        ]
        dests = [
            _resolve_fs(u, self.method, storage_options=self.st_opts) for u in path2
        ]

        # Validate the whole batch before planning any side effect.
        fs, fs2 = sources[0], dests[0]
        for side, backends, expected in (
            ("source", sources, fs),
            ("destination", dests, fs2),
        ):
            if any(backend is not expected for backend in backends[1:]):
                raise ValueError(f"Multiple {side} backends are not supported")

        operations = tuple(
            CopyOperation(u1, u2, f1, f2, dict(kwargs))
            for u1, u2, f1, f2 in zip(path1, path2, sources, dests)
        )
        versions = _version_kwargs(kwargs)
        reasons = []
        same_instance = fs is fs2

        if not same_instance:
            reasons.append(
                f"source and destination are different storage instances "
                f"({_fs_label(fs)} vs {_fs_label(fs2)}); a native server-side "
                "copy cannot cross them"
            )
            return CopyPlan(
                operations,
                READ_WRITE_FALLBACK,
                tuple(reasons),
                version_kwargs=versions,
                metadata="unknown",
            )

        capabilities = fs.copy_capabilities()
        if capabilities.native_copy is False:
            reasons.append(
                f"{_fs_label(fs)} declares that it does not support native copy"
            )
            return CopyPlan(
                operations,
                READ_WRITE_FALLBACK,
                tuple(reasons),
                backend=fs,
                capabilities=capabilities,
                version_kwargs=versions,
                metadata="unknown",
            )

        if versions and not capabilities.versioned_copy:
            reasons.append(
                "version arguments were requested "
                f"({sorted(versions)}) but {_fs_label(fs)} does not declare "
                "version-aware native copy"
            )
            return CopyPlan(
                operations,
                READ_WRITE_FALLBACK,
                tuple(reasons),
                backend=fs,
                capabilities=capabilities,
                version_kwargs=versions,
                metadata="unknown",
            )

        if capabilities.native_copy is True:
            reasons.append(f"{_fs_label(fs)} declares native server-side copy support")
        else:
            reasons.append(
                f"{_fs_label(fs)} makes no native-copy declaration; its copy "
                "method will be attempted"
            )
        if versions:
            reasons.append("the backend declares version-aware native copy")
        reasons.append(
            f"metadata handling on the native path is '{capabilities.metadata}'"
        )
        return CopyPlan(
            operations,
            NATIVE_COPY,
            tuple(reasons),
            backend=fs,
            capabilities=capabilities,
            version_kwargs=versions,
            metadata=capabilities.metadata,
        )

    async def _execute_plan(
        self, plan, tempdir=None, batch_size=None, on_error="ignore"
    ):
        """Execute a plan, tracking what was actually attempted and completed."""
        result = CopyResult(plan=plan)
        if plan.mode is READ_WRITE_FALLBACK:
            await self._run_read_write(
                plan, result, tempdir=tempdir, batch_size=batch_size, on_error=on_error
            )
            return result

        # Native path: one backend instance handles the whole batch.
        fs = plan.backend
        sources = [op.source for op in plan.operations]
        targets = [op.destination for op in plan.operations]
        kwargs = plan.operations[0].kwargs
        before = {url: self._target_marker(fs, url) for url in targets}
        try:
            if fs.async_impl:
                await fs._copy(sources, targets, **kwargs)
            else:
                fs.copy(sources, targets, **kwargs)
        except NotImplementedError as exc:
            # Only safe when nothing could have been written yet. Compare the
            # targets against their pre-call state to prove no side effect.
            changed = [
                url
                for url in targets
                if self._target_marker(fs, url) != before[url]
            ]
            if changed:
                result.attempted.extend(targets)
                for url in changed:
                    result.failures[url] = exc
                logger.warning(
                    "Native copy failed after targets were touched; not retrying: %s",
                    changed,
                )
                result._raise_if_incomplete()
            logger.info(
                "Backend %s raised NotImplementedError before any side effect; "
                "falling back to read/write copy: %s",
                _fs_label(fs),
                exc,
            )
            result.fallback_used = True
            result.native_fallback = True
            fallback = CopyPlan(
                plan.operations,
                READ_WRITE_FALLBACK,
                plan.reasons
                + (
                    "native copy raised NotImplementedError before writing; "
                    "falling back while still side-effect free",
                ),
                backend=fs,
                capabilities=plan.capabilities,
                version_kwargs=plan.version_kwargs,
                metadata="unknown",
            )
            result.plan = fallback
            await self._run_read_write(
                fallback,
                result,
                tempdir=tempdir,
                batch_size=batch_size,
                on_error=on_error,
            )
            return result
        except Exception as exc:
            # The batch may be partially written. Never blindly resend; the
            # target states are unknown even for the ones that did not raise.
            result.attempted.extend(targets)
            for url in targets:
                result.failures[url] = exc
            logger.error(
                "Native copy failed with target state unknown; no retry is "
                "attempted: %s",
                exc,
            )
            if on_error == "raise":
                raise CopyIncompleteError(
                    "Native copy failed with the state of some targets "
                    "unknown; no retry was attempted because data may already "
                    "have been written",
                    completed=result.completed,
                    incomplete=result.incomplete,
                    failures=result.failures,
                ) from exc
            return result

        result.completed.extend(targets)
        result.attempted.extend(targets)
        return result

    @staticmethod
    def _target_marker(fs, url):
        """Cheap fingerprint used only to detect writes during a failed copy."""
        try:
            info = fs.info(url)
        except Exception as exc:
            return ("missing", type(exc).__name__)
        return ("present", info.get("size"), info.get("modified"))

    async def _run_read_write(
        self, plan, result, tempdir=None, batch_size=None, on_error="ignore"
    ):
        import tempfile

        result.attempted.extend(op.destination for op in plan.operations)
        with tempfile.TemporaryDirectory(dir=tempdir) as staging:
            coros = [
                _copy_file_op(
                    op.source_fs,
                    op.source,
                    op.destination_fs,
                    op.destination,
                    os.path.join(staging, uuid.uuid4().hex),
                )
                for op in plan.operations
            ]
            outcomes = await _run_coros_in_chunks(
                coros, batch_size=batch_size, return_exceptions=True
            )
        for op, outcome in zip(plan.operations, outcomes):
            if isinstance(outcome, Exception):
                result.failures[op.destination] = outcome
                if on_error == "raise":
                    result._raise_if_incomplete()
            else:
                result.completed.append(op.destination)
        if on_error == "raise":
            result._raise_if_incomplete()

    async def _cp_file(
        self,
        url,
        url2,
        blocksize=2**20,
        callback=DEFAULT_CALLBACK,
        tempdir: str | None = None,
        **kwargs,
    ):
        plan = self.plan_copy(url, url2, **kwargs)
        logger.debug("%s", plan.explain())
        result = await self._execute_plan(
            plan, tempdir=tempdir, batch_size=1, on_error="raise"
        )
        result._raise_if_incomplete()
        return result

    async def _make_many_dirs(self, urls, exist_ok=True):
        fs = _resolve_fs(urls[0], self.method, storage_options=self.st_opts)
        if fs.async_impl:
            coros = [fs._makedirs(u, exist_ok=exist_ok) for u in urls]
            await _run_coros_in_chunks(coros)
        else:
            for u in urls:
                fs.makedirs(u, exist_ok=exist_ok)

    make_many_dirs = sync_wrapper(_make_many_dirs)

    async def _copy(
        self,
        path1: list[str],
        path2: list[str],
        recursive: bool = False,
        on_error: str = "ignore",
        maxdepth: int | None = None,
        batch_size: int | None = None,
        tempdir: str | None = None,
        **kwargs,
    ):
        # TODO: special case for one FS being local, which can use get/put
        # TODO: special case for one being memFS, which can use cat/pipe
        if recursive:
            raise NotImplementedError("Please use fsspec.generic.rsync")
        path1 = [path1] if isinstance(path1, str) else path1
        path2 = [path2] if isinstance(path2, str) else path2

        # Planning validates the whole batch (including rejecting mixed
        # backends) before any destination is touched.
        plan = self.plan_copy(path1, path2, **kwargs)
        logger.debug("%s", plan.explain())
        result = await self._execute_plan(
            plan, tempdir=tempdir, batch_size=batch_size, on_error=on_error
        )
        if on_error == "raise":
            result._raise_if_incomplete()
        return result


async def copy_file_op(
    fs1, url1, fs2, url2, tempdir=None, batch_size=20, on_error="ignore"
):
    """Stream a batch of files from ``fs1`` to ``fs2`` via a local staging dir.

    Kept as a module-level helper for callers that already work with resolved
    backend instances. Returns the per-file outcome list (exceptions included)
    when ``on_error == "return"``.
    """
    import tempfile

    with tempfile.TemporaryDirectory(dir=tempdir) as tempdir:
        coros = [
            _copy_file_op(
                fs1,
                u1,
                fs2,
                u2,
                os.path.join(tempdir, uuid.uuid4().hex),
            )
            for u1, u2 in zip(url1, url2)
        ]
        out = await _run_coros_in_chunks(
            coros, batch_size=batch_size, return_exceptions=True
        )
    if on_error == "return":
        return out
    elif on_error == "raise":
        for o in out:
            if isinstance(o, Exception):
                raise o


async def _copy_file_op(fs1, url1, fs2, url2, local, on_error="ignore"):
    if fs1.async_impl:
        await fs1._get_file(url1, local)
    else:
        fs1.get_file(url1, local)
    if fs2.async_impl:
        await fs2._put_file(local, url2)
    else:
        fs2.put_file(local, url2)
    os.unlink(local)
    logger.debug("Copy %s -> %s; done", url1, url2)


async def maybe_await(cor):
    if inspect.iscoroutine(cor):
        return await cor
    else:
        return cor
