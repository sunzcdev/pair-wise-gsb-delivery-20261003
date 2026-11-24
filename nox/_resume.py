"""Content-addressed resume planning for Nox sessions.

This module is intentionally dependency-free at import time: the planner works
on :class:`~nox.sessions.SessionRunner`-like objects so it can be unit tested
without constructing a complete Nox CLI configuration.
"""

from __future__ import annotations

import hashlib
import inspect
import json
import os
from collections.abc import Mapping
from pathlib import Path
from typing import TYPE_CHECKING, Any

from nox.logger import logger

if TYPE_CHECKING:
    from collections.abc import Iterable, Sequence

__all__ = [
    "artifact_digest",
    "collect_outputs",
    "load_report",
    "plan_for_manifest",
    "session_identity",
    "session_identity_id",
    "snapshot_tree",
    "verify_outputs",
]

# Directories which never contain source artifacts produced by a Nox session.
_PRUNED_DIRECTORIES = {
    ".git",
    ".hg",
    ".svn",
    ".nox",
    ".tox",
    ".venv",
    "venv",
    "env",
    "__pycache__",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
    ".cache",
    "node_modules",
}

_RESUME_FORMAT = "nox-resume-1"


def _json_default(value: Any) -> Any:
    if isinstance(value, (set, frozenset)):
        return sorted(value, key=repr)
    if isinstance(value, Path):
        return os.fspath(value)
    return repr(value)


def canonical_json(value: Any) -> str:
    """Return a stable JSON representation used in identity fingerprints."""
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        default=_json_default,
    )


def digest_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def session_function_source(session: Any) -> str | None:
    """Best-effort stable source fingerprint for a session callable."""
    func = getattr(session, "func", None)
    raw_func = getattr(func, "func", func)
    if raw_func is None:
        return None
    try:
        return inspect.getsource(raw_func)
    except (OSError, TypeError):
        code = getattr(raw_func, "__code__", None)
        if code is None:
            return None
        try:
            return repr(code.co_code) + repr(code.co_consts)
        except Exception:  # noqa: BLE001 - fingerprinting is best effort
            return None


def _safe_mapping(value: Any) -> dict[str, Any]:
    if isinstance(value, Mapping):
        try:
            return dict(value)
        except (TypeError, ValueError):
            return {}
    return {}


def _safe_list(value: Any) -> list[Any]:
    if isinstance(value, (str, bytes)):
        return [value]
    try:
        return list(value or [])
    except TypeError:
        return []


def session_identity(session: Any) -> dict[str, Any]:
    """Build the configuration/variant identity for a session runner.

    The identity must distinguish parameterized matrix cells while remaining
    independent of transient run duration and report location.
    """
    func = getattr(session, "func", None)
    call_spec = _safe_mapping(getattr(func, "call_spec", {}))
    identity: dict[str, Any] = {
        "name": getattr(session, "name", None),
        "signatures": _safe_list(getattr(session, "signatures", [])),
        "args": call_spec,
        "python": getattr(func, "python", None),
        "posargs": _safe_list(getattr(session, "posargs", [])),
        "requires": _safe_list(getattr(func, "requires", [])),
        "venv_backend": getattr(func, "venv_backend", None),
        "allow_parallel": getattr(func, "allow_parallel", None),
    }
    source = session_function_source(session)
    if source is not None:
        identity["source_sha256"] = digest_text(source)
    return identity


def session_identity_id(session_or_identity: Any) -> str:
    identity = (
        session_or_identity
        if isinstance(session_or_identity, Mapping)
        else session_identity(session_or_identity)
    )
    return digest_text(canonical_json(identity))


def artifact_digest(path: str | os.PathLike[str]) -> str:
    """Hash a file's contents."""
    digest = hashlib.sha256()
    with open(path, "rb") as artifact:
        for chunk in iter(lambda: artifact.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _is_relative_to(path: str, parent: str) -> bool:
    try:
        return os.path.commonpath([os.path.abspath(path), os.path.abspath(parent)]) == os.path.abspath(parent)
    except ValueError:
        return False


def _excluded_directory(dir_path: str, extra_dirs: Iterable[str]) -> bool:
    if os.path.basename(dir_path) in _PRUNED_DIRECTORIES:
        return True
    return any(_same_path_or_inside(dir_path, extra) for extra in extra_dirs)


def _same_path_or_inside(path: str, parent: str) -> bool:
    if not parent:
        return False
    path_abs = os.path.abspath(path)
    parent_abs = os.path.abspath(parent)
    return path_abs == parent_abs or _is_relative_to(path_abs, parent_abs)


def snapshot_tree(
    root: str | os.PathLike[str],
    *,
    extra_directories: Sequence[str | os.PathLike[str]] = (),
) -> dict[str, tuple[int, int]]:
    """Snapshot lightweight file metadata under ``root``.

    Only ``(mtime_ns, size)`` is stored; content hashing is deferred until a
    file is known to be an output.
    """
    root = os.path.abspath(os.fspath(root))
    extra_dirs = [os.path.abspath(os.fspath(p)) for p in extra_directories if p]
    snapshot: dict[str, tuple[int, int]] = {}

    for current, dirnames, filenames in os.walk(root):
        dirnames[:] = [
            name
            for name in dirnames
            if not _excluded_directory(os.path.join(current, name), extra_dirs)
        ]
        for filename in filenames:
            full_path = os.path.join(current, filename)
            try:
                stat = os.stat(full_path)
            except OSError:
                continue
            rel_path = os.path.relpath(full_path, root).replace(os.sep, "/")
            snapshot[rel_path] = (stat.st_mtime_ns, stat.st_size)
    return snapshot


def _artifact_key(absolute: str, root: str) -> str:
    absolute = os.path.abspath(absolute)
    try:
        return os.path.relpath(absolute, root).replace(os.sep, "/")
    except ValueError:
        # Different Windows drives cannot have a relative path; use the
        # drive-qualified absolute form, which os.path.isabs recognizes.
        return absolute.replace(os.sep, "/")


def _normalize_artifact_path(
    path_value: str | os.PathLike[str], root: str
) -> tuple[str, str] | None:
    raw = os.fspath(path_value)
    absolute = raw if os.path.isabs(raw) else os.path.join(root, raw)
    absolute = os.path.abspath(absolute)
    if not os.path.isfile(absolute):
        return None
    return _artifact_key(absolute, root), absolute


def _expand_declared(
    declared: Iterable[str | os.PathLike[str]], root: str
) -> list[str]:
    paths: list[str] = []
    for value in declared:
        raw = os.fspath(value)
        absolute = raw if os.path.isabs(raw) else os.path.join(root, raw)
        absolute = os.path.abspath(absolute)
        if os.path.isdir(absolute):
            for current, dirnames, filenames in os.walk(absolute):
                dirnames[:] = [d for d in dirnames if d not in _PRUNED_DIRECTORIES]
                for filename in filenames:
                    paths.append(os.path.join(current, filename))
        else:
            paths.append(absolute)
    return paths


def collect_outputs(
    before: Mapping[str, tuple[int, int]],
    root: str | os.PathLike[str],
    *,
    declared: Iterable[str | os.PathLike[str]] = (),
    exclude_files: Iterable[str | os.PathLike[str]] = (),
    extra_directories: Sequence[str | os.PathLike[str]] = (),
    owner_id: str | None = None,
) -> list[dict[str, Any]]:
    """Collect and hash files created or changed during a session."""
    root = os.path.abspath(os.fspath(root))
    excluded = {os.path.abspath(os.fspath(p)) for p in exclude_files if p}
    candidates: dict[str, str] = {}
    missing_declared: dict[str, str] = {}

    # Files explicitly declared by the session always count, even if their
    # metadata did not change during this execution.
    for absolute in _expand_declared(declared, root):
        absolute = os.path.abspath(absolute)
        if absolute in excluded:
            continue
        key = _artifact_key(absolute, root)
        if os.path.isfile(absolute):
            candidates[key] = absolute
        else:
            # Preserve missing publications so a partial release cannot be
            # treated as complete resume evidence.
            missing_declared[key] = absolute

    # Observe actual workspace writes. This makes the common matrix case work
    # without requiring a second output declaration API.
    for rel_path, metadata in snapshot_tree(
        root, extra_directories=extra_directories
    ).items():
        absolute = os.path.join(root, rel_path)
        if os.path.abspath(absolute) in excluded:
            continue
        if rel_path not in before or before[rel_path] != metadata:
            candidates[rel_path] = absolute

    outputs: list[dict[str, Any]] = []
    for rel_path, absolute in sorted(candidates.items()):
        try:
            stat = os.stat(absolute)
            record: dict[str, Any] = {
                "path": rel_path,
                "sha256": artifact_digest(absolute),
                "size": stat.st_size,
            }
            if owner_id is not None:
                record["owner"] = owner_id
            outputs.append(record)
        except OSError:
            continue
    for rel_path in sorted(missing_declared):
        outputs.append(
            {
                "path": rel_path,
                "sha256": None,
                "size": None,
                "missing": True,
                **({"owner": owner_id} if owner_id is not None else {}),
            }
        )
    return outputs


def verify_outputs(
    outputs: Sequence[Mapping[str, Any]],
    root: str | os.PathLike[str],
    *,
    owner_id: str | None = None,
) -> tuple[bool, str | None]:
    """Verify that recorded output files still have their recorded contents."""
    if not outputs:
        return False, "no output identity evidence"
    root = os.path.abspath(os.fspath(root))
    for output in outputs:
        path_value = (
            output.get("path")
            or output.get("file")
            or output.get("filename")
            or output.get("name")
        )
        expected_digest = (
            output.get("sha256") or output.get("digest") or output.get("hash")
        )
        if not isinstance(path_value, str) or not expected_digest:
            return False, "output record is incomplete"
        recorded_owner = output.get("owner")
        if owner_id is not None and recorded_owner not in (None, owner_id):
            return False, f"output {path_value!r} is owned by another session"
        normalized = _normalize_artifact_path(path_value, root)
        if normalized is None:
            return False, f"output {path_value!r} is missing"
        _rel_path, absolute = normalized
        try:
            actual_digest = artifact_digest(absolute)
        except OSError as exc:
            return False, f"output {path_value!r} is unreadable: {exc}"
        if actual_digest != expected_digest:
            return False, f"output {path_value!r} changed"
    return True, None


def load_report(path: str | os.PathLike[str] | None) -> dict[str, Any] | None:
    """Load a Nox JSON report, returning ``None`` when it has no usable data."""
    if not path:
        return None
    try:
        with open(path, encoding="utf-8") as report_file:
            report = json.load(report_file)
    except (OSError, ValueError):
        return None
    if not isinstance(report, dict):
        return None
    sessions = report.get("sessions")
    if not isinstance(sessions, list):
        return None
    return report


def _legacy_identity(entry: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "name": entry.get("name"),
        "signatures": list(entry.get("signatures", []) or []),
        "args": dict(entry.get("args", {}) or {}),
    }


def _report_entries_by_identity(
    report: Mapping[str, Any],
) -> dict[str, list[dict[str, Any]]]:
    grouped: dict[str, list[dict[str, Any]]] = {}
    for raw_entry in report.get("sessions", []):
        if not isinstance(raw_entry, dict):
            continue
        # Only completed successes are valid resume evidence. Skipped/failed
        # and partially published entries must execute normally.
        if raw_entry.get("result") != "success":
            continue
        identity = raw_entry.get("identity")
        if not isinstance(identity, dict):
            identity = _legacy_identity(raw_entry)
        identity_id = raw_entry.get("identity_id")
        if not isinstance(identity_id, str):
            identity_id = session_identity_id(identity)
        entry = dict(raw_entry)
        entry["identity"] = identity
        entry["identity_id"] = identity_id
        grouped.setdefault(identity_id, []).append(entry)
    return grouped


def _config_root(global_config: Any) -> str:
    noxfile = getattr(global_config, "noxfile", "noxfile.py") or "noxfile.py"
    return os.path.realpath(os.path.dirname(os.path.abspath(noxfile)))


def _resume_path(global_config: Any) -> str | None:
    # An explicit resume checklist takes precedence. ``resume=True`` means use
    # the normal --report path. The common zero-configuration behavior is to
    # treat an existing --report file as optional resume evidence.
    explicit = getattr(global_config, "resume_file", None) or getattr(
        global_config, "resume_report", None
    )
    if isinstance(explicit, (str, os.PathLike)) and explicit:
        return os.fspath(explicit)
    resume = getattr(global_config, "resume", None)
    if isinstance(resume, (str, os.PathLike)) and resume:
        return os.fspath(resume)
    if resume is False:
        return None
    report = getattr(global_config, "report", None)
    if isinstance(report, (str, os.PathLike)) and report:
        return os.fspath(report)
    return None


def plan_for_manifest(
    manifest: Any,
    global_config: Any = None,
    *,
    records: Sequence[Mapping[str, Any]] | None = None,
    report_path: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    """Return and attach a content-aware resume plan for ``manifest``.

    Sessions are reused only when their complete variant/configuration identity
    has exactly one successful old record and every recorded output still
    matches its content digest. Invalidation then propagates through the actual
    ``requires=` graph.
    """
    existing_plan = getattr(manifest, "_resume_plan", None)
    if (
        existing_plan is not None
        and records is None
        and report_path is None
    ):
        return existing_plan

    path = report_path if report_path is not None else _resume_path(global_config)
    report = None if records is not None else load_report(path)
    if records is not None:
        report = {"sessions": list(records)}

    queue = list(getattr(manifest, "_queue", []) or [])
    plan: dict[str, Any] = {
        "version": _RESUME_FORMAT,
        "report": os.fspath(path) if path else None,
        "reused": [],
        "rerun": [getattr(session, "friendly_name", repr(session)) for session in queue],
        "reasons": {},
        "entries": {},
    }
    manifest._resume_plan = plan
    if report is None:
        return plan

    grouped = _report_entries_by_identity(report)
    root = _config_root(global_config) if global_config is not None else os.getcwd()
    own_fresh: dict[Any, bool] = {}
    fresh_entry: dict[Any, dict[str, Any]] = {}

    for session in queue:
        name = getattr(session, "friendly_name", repr(session))
        identity = session_identity(session)
        identity_id = session_identity_id(identity)
        candidates = grouped.get(identity_id, [])
        if len(candidates) != 1:
            reason = (
                "no matching successful report record"
                if not candidates
                else "ambiguous matching report records"
            )
            own_fresh[session] = False
            plan["reasons"][name] = reason
            continue

        entry = candidates[0]
        outputs = entry.get("outputs")
        if not isinstance(outputs, list):
            outputs = entry.get("artifacts")
        if not isinstance(outputs, list):
            outputs = []
        fresh, reason = verify_outputs(outputs, root, owner_id=identity_id)
        if not fresh:
            own_fresh[session] = False
            plan["reasons"][name] = reason or "output identity could not be verified"
            continue

        own_fresh[session] = True
        fresh_entry[session] = entry
        plan["entries"][id(session)] = entry

    # Build the real requires= graph once. Missing graph evidence is unsafe
    # for reuse because a dependency artifact may have changed.
    dependencies_by_session: dict[Any, list[Any]] = {}
    for session in queue:
        if not own_fresh.get(session, False):
            continue
        try:
            dependencies_by_session[session] = list(
                session.get_direct_dependencies()
            )
        except Exception as exc:  # noqa: BLE001 - broken graph means no reuse
            own_fresh[session] = False
            name = getattr(session, "friendly_name", repr(session))
            plan["reasons"][name] = f"dependency identity unavailable: {exc}"

    # Propagate invalidation through the transitive dependency closure with a
    # fixpoint so correctness does not depend on the queue already being
    # topologically ordered.
    changed = True
    while changed:
        changed = False
        for session in queue:
            if not own_fresh.get(session, False):
                continue
            for dependency in dependencies_by_session.get(session, []):
                if dependency in own_fresh and own_fresh[dependency]:
                    continue
                own_fresh[session] = False
                changed = True
                name = getattr(session, "friendly_name", repr(session))
                dep_name = getattr(dependency, "friendly_name", repr(dependency))
                if dependency in own_fresh:
                    reason = f"dependency {dep_name!r} was affected"
                else:
                    reason = f"dependency {dep_name!r} has no resume evidence"
                plan["reasons"][name] = reason
                break

    reused: list[Any] = []
    rerun: list[str] = []
    for session in queue:
        name = getattr(session, "friendly_name", repr(session))
        if own_fresh.get(session, False):
            reused.append(name)
            entry = fresh_entry[session]
            session.resume_cached = True
            session.resume_entry = entry
            session.resume_reason = plan["reasons"].get(name, "identity and outputs unchanged")
            if global_config is not None:
                # Imported here to keep _resume importable from sessions without
                # creating a module cycle at import time.
                from nox.sessions import Result, Status  # noqa: PLC0415

                result = Result.from_dict(session, entry)
                result.status = Status.SUCCESS
                result.reason = None
                session.result = result
        else:
            rerun.append(name)
            if not plan["reasons"].get(name):
                plan["reasons"][name] = "execution required"
            try:
                session.resume_cached = False
            except Exception:  # noqa: BLE001
                pass

    plan["reused"] = reused
    plan["rerun"] = rerun
    if reused:
        logger.info(
            "Resume plan reuses %d session(s): %s",
            len(reused),
            ", ".join(reused),
        )
    return plan
