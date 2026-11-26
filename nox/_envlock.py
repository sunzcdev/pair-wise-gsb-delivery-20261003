from __future__ import annotations

import contextlib
import hashlib
import json
import os
import re
import threading
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Any

from filelock import FileLock, Timeout as FileLockTimeout

if TYPE_CHECKING:
    from collections.abc import Iterator

__all__ = [
    "EnvironmentLease",
    "EnvironmentLeaseTimeout",
    "EnvironmentLock",
    "environment_lock_path",
    "inspect_environment",
]


class EnvironmentLeaseTimeout(TimeoutError):
    """Raised when an environment lease cannot be acquired in time."""


def environment_lock_path(location: str | os.PathLike[str]) -> Path:
    """Return the cross-process arbitration directory for *location*.

    The directory is a sibling rather than a child of the environment.  A
    recreate is allowed to remove the environment itself, but it must never
    remove the lock that arbitrates that operation.
    """
    path = Path(location)
    try:
        path = path.resolve(strict=False)
    except TypeError:  # pragma: no cover - Python < 3.6 style paths
        path = Path(os.path.realpath(os.fspath(location)))
    name = path.name or "environment"
    safe_name = re.sub(r"[^A-Za-z0-9_.-]+", "-", name).strip("-.")[:24] or "env"
    # The hash keeps the arbitration sibling short on Windows while making
    # differently-cased or relative spellings of the same directory agree.
    digest = hashlib.sha256(os.path.normcase(str(path)).encode("utf-8")).hexdigest()[:10]
    return path.parent / f".{safe_name}-{digest}.lock"


def _utc_now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _write_json(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    # Keep this suffix short: Windows CI temp paths can already be close to
    # MAX_PATH and long unique temporary names make otherwise valid JSON
    # files fail with FileNotFoundError.  Callers hold control.lock when they
    # write shared state; unique metadata paths need no unique temp suffix.
    temporary = path.with_name(path.name + ".tmp")
    try:
        temporary.write_text(json.dumps(data, sort_keys=True), encoding="utf-8")
        os.replace(temporary, path)
    finally:
        with contextlib.suppress(FileNotFoundError, OSError):
            temporary.unlink()


def _read_json(path: Path) -> dict[str, Any] | None:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return None
    return value if isinstance(value, dict) else None


def _lock_is_held(path: Path) -> bool:
    """Return whether an OS lock at *path* is currently held.

    A non-blocking probe either sees the active holder's byte-range lock or
    acquires the lock left behind by a dead process.  In the latter case the
    probe releases it immediately.
    """
    if not path.exists():
        return False
    probe = FileLock(str(path), timeout=0)
    try:
        probe.acquire(blocking=False)
    except FileLockTimeout:
        return True
    else:
        probe.release()
        return False


@dataclass
class _LeaseFiles:
    holder_lock: FileLock | None = None
    holder_lock_path: Path | None = None
    holder_json: Path | None = None
    writer_lock: FileLock | None = None
    writer_lock_path: Path | None = None
    writer_json: Path | None = None
    request_lock: FileLock | None = None
    request_lock_path: Path | None = None
    request_json: Path | None = None


class EnvironmentLease:
    """Cross-process shared/exclusive lease for one Nox environment directory.

    Shared leases protect commands which are using an environment.  An
    exclusive lease is a recreate request: it waits for every shared holder to
    finish, while its held writer lock makes the request visible to later
    readers and writers.  Cancelling the wait closes the lease and removes the
    request metadata; process death releases the operating-system locks.
    """

    def __init__(
        self,
        location: str | os.PathLike[str],
        *,
        exclusive: bool = False,
        timeout: float | None = None,
        poll_interval: float = 0.05,
        cancel_event: threading.Event | None = None,
    ) -> None:
        self.location = Path(location).resolve(strict=False)
        self.lock_dir = environment_lock_path(location)
        self.exclusive = exclusive
        self.timeout = timeout
        self.poll_interval = poll_interval
        self.cancel_event = cancel_event
        self.token = uuid.uuid4().hex[:12]
        self.pid = os.getpid()
        self.created_at = _utc_now()
        self._files = _LeaseFiles()
        self._held = False

    @property
    def held(self) -> bool:
        return self._held

    def cancel(self) -> None:
        """Ask an in-progress acquisition wait to withdraw its request."""
        if self.cancel_event is None:
            self.cancel_event = threading.Event()
        self.cancel_event.set()

    def __enter__(self) -> EnvironmentLease:
        return self.acquire()

    def __exit__(self, exc_type: object, exc: object, tb: object) -> None:
        self.release()

    def acquire(self) -> EnvironmentLease:
        if self._held:
            return self
        try:
            self.lock_dir.mkdir(parents=True, exist_ok=True)
            (self.lock_dir / "holders").mkdir(exist_ok=True)
            (self.lock_dir / "requests").mkdir(exist_ok=True)
            if self.exclusive:
                self._acquire_writer()
            else:
                self._acquire_reader()
            self._held = True
            return self
        except BaseException:
            # If __enter__ raises, the with statement does not call __exit__.
            # Release any OS lock acquired before the timeout/cancellation so a
            # rejected request cannot leave writer.lock behind.
            self._held = True
            self.release()
            raise

    def release(self) -> None:
        if not self._held:
            return
        try:
            with self._control() if self.lock_dir.exists() else contextlib.nullcontext():
                files = self._files
                if files.holder_lock is not None:
                    with contextlib.suppress(Exception):
                        files.holder_lock.release()
                if files.writer_lock is not None:
                    with contextlib.suppress(Exception):
                        files.writer_lock.release()
                if files.request_lock is not None:
                    with contextlib.suppress(Exception):
                        files.request_lock.release()
                for path in (
                    files.holder_lock_path,
                    files.holder_json,
                    files.writer_json,
                    files.request_lock_path,
                    files.request_json,
                ):
                    if path is not None:
                        with contextlib.suppress(FileNotFoundError, OSError):
                            path.unlink()
                self._write_state_locked()
                self._remove_state_if_idle()
        finally:
            self._held = False

    def upgrade_to_exclusive(self) -> None:
        """Upgrade a shared lease, waiting for other shared users to finish."""
        if self.exclusive and self._held:
            return
        if not self._held:
            self.exclusive = True
            self.acquire()
            return

        # Remove our own holder first; otherwise the writer wait would wait on
        # this same lease.  There is deliberately no atomic upgrade: another
        # writer may win the race, but it publishes a new environment before we
        # repeat creation under our own exclusive lease.
        files = self._files
        with self._control():
            if files.holder_lock is not None:
                with contextlib.suppress(Exception):
                    files.holder_lock.release()
            for path in (files.holder_lock_path, files.holder_json):
                if path is not None:
                    with contextlib.suppress(FileNotFoundError, OSError):
                        path.unlink()
            self._write_state_locked()
            self._remove_state_if_idle()
        self._files.holder_lock = None
        self._files.holder_lock_path = None
        self._files.holder_json = None
        self.exclusive = True
        self._acquire_writer()

    def downgrade_to_shared(self) -> None:
        """Atomically replace an exclusive lease with a shared usage lease."""
        if not self.exclusive or not self._held:
            return
        with self._control():
            holder_lock_path = self.lock_dir / "holders" / f"{self.token}.lock"
            holder_json = self.lock_dir / "holders" / f"{self.token}.json"
            holder_lock = FileLock(str(holder_lock_path), timeout=0)
            holder_lock.acquire(blocking=False)
            self._files.holder_lock = holder_lock
            self._files.holder_lock_path = holder_lock_path
            self._files.holder_json = holder_json
            _write_json(holder_json, self._metadata("read"))

            writer = self._files.writer_lock
            if writer is not None:
                with contextlib.suppress(Exception):
                    writer.release()
            for path in (self._files.writer_json, self._files.request_json):
                if path is not None:
                    with contextlib.suppress(FileNotFoundError, OSError):
                        path.unlink()
            self._files.writer_lock = None
            self._files.writer_json = None
            self._files.request_json = None
            self.exclusive = False
            self._write_state_locked()

    def _check_cancelled(self, deadline: float | None) -> None:
        if self.cancel_event is not None and self.cancel_event.is_set():
            msg = f"Lease request for {self.location} was cancelled."
            raise EnvironmentLeaseTimeout(msg)
        if deadline is not None and time.monotonic() >= deadline:
            msg = f"Timed out waiting for environment lease for {self.location}."
            raise EnvironmentLeaseTimeout(msg)

    def _sleep(self, deadline: float | None) -> None:
        remaining = self.poll_interval
        if deadline is not None:
            remaining = min(remaining, max(0.0, deadline - time.monotonic()))
        if self.cancel_event is not None:
            # Event.wait returns True if set; the next check raises.
            self.cancel_event.wait(remaining)
        else:
            time.sleep(remaining)

    @contextlib.contextmanager
    def _control(self) -> Iterator[None]:
        lock = FileLock(str(self.lock_dir / "control.lock"), timeout=10)
        lock.acquire(timeout=10)
        try:
            yield
        finally:
            lock.release()

    def _metadata(self, mode: str) -> dict[str, Any]:
        return {
            "token": self.token,
            "pid": self.pid,
            "mode": mode,
            "envdir": str(self.location),
            "lock_dir": str(self.lock_dir),
            "created_at": self.created_at,
            "updated_at": _utc_now(),
        }

    def _acquire_reader(self) -> None:
        deadline = None if self.timeout is None else time.monotonic() + self.timeout
        while True:
            self._check_cancelled(deadline)
            self.lock_dir.mkdir(parents=True, exist_ok=True)
            with self._control():
                writer_lock_path = self.lock_dir / "writer.lock"
                if _lock_is_held(writer_lock_path):
                    busy = "writer"
                else:
                    # A dead/cancelled active writer cannot leave live writer
                    # metadata without also holding writer.lock.  Request files
                    # are owned by their writers and must not be removed here.
                    with contextlib.suppress(FileNotFoundError, OSError):
                        (self.lock_dir / "writer.json").unlink()
                    busy = None
                if busy is None:
                    lock_path = self.lock_dir / "holders" / f"{self.token}.lock"
                    json_path = self.lock_dir / "holders" / f"{self.token}.json"
                    lock = FileLock(str(lock_path), timeout=0)
                    lock.acquire(blocking=False)
                    self._files.holder_lock = lock
                    self._files.holder_lock_path = lock_path
                    self._files.holder_json = json_path
                    _write_json(json_path, self._metadata("read"))
                    self._write_state_locked()
                    return
            self._sleep(deadline)

    def _acquire_writer(self) -> None:
        deadline = None if self.timeout is None else time.monotonic() + self.timeout
        request_json = self.lock_dir / "requests" / f"{self.token}.json"
        request_lock_path = self.lock_dir / "requests" / f"{self.token}.lock"
        request_lock = FileLock(str(request_lock_path), timeout=0)
        request_lock.acquire(blocking=False)
        self._files.request_json = request_json
        self._files.request_lock = request_lock
        self._files.request_lock_path = request_lock_path
        _write_json(request_json, self._metadata("write-request"))
        writer_lock_path = self.lock_dir / "writer.lock"
        self._files.writer_lock_path = writer_lock_path
        self._files.writer_json = self.lock_dir / "writer.json"
        writer_lock = FileLock(str(writer_lock_path), timeout=0)

        while True:
            self._check_cancelled(deadline)
            try:
                writer_lock.acquire(blocking=False)
            except FileLockTimeout:
                self._sleep(deadline)
                continue
            self._files.writer_lock = writer_lock

            with self._control():
                # Do not delete requests/*.json here: another live writer may
                # already have published its request and be blocked on
                # writer.lock.  Each owner removes only its own request.
                active_readers = self._active_readers_locked()
                # Publish the waiting request even when the branch below keeps
                # waiting; state.json is an inspectable snapshot, not the lock.
                self._write_state_locked()
                if not active_readers:
                    request_lock = self._files.request_lock
                    if request_lock is not None:
                        with contextlib.suppress(Exception):
                            request_lock.release()
                    for request_path in (request_json, request_lock_path):
                        with contextlib.suppress(FileNotFoundError, OSError):
                            request_path.unlink()
                    self._files.request_json = None
                    self._files.request_lock = None
                    self._files.request_lock_path = None
                    writer_json = self.lock_dir / "writer.json"
                    self._files.writer_lock_path = writer_lock_path
                    _write_json(writer_json, self._metadata("write"))
                    self._write_state_locked()
                    return

            self._sleep(deadline)

    def _active_readers_locked(self) -> list[dict[str, Any]]:
        active: list[dict[str, Any]] = []
        holders = self.lock_dir / "holders"
        for lock_path in holders.glob("*.lock"):
            if _lock_is_held(lock_path):
                json_path = lock_path.with_suffix(".json")
                metadata = _read_json(json_path) or {"token": lock_path.stem}
                active.append(metadata)
                continue
            with contextlib.suppress(FileNotFoundError, OSError):
                lock_path.unlink()
            json_path = lock_path.with_suffix(".json")
            with contextlib.suppress(FileNotFoundError, OSError):
                json_path.unlink()
        return active

    def _active_requests_locked(self) -> list[dict[str, Any]]:
        active: list[dict[str, Any]] = []
        requests = self.lock_dir / "requests"
        for lock_path in requests.glob("*.lock"):
            json_path = lock_path.with_suffix(".json")
            if _lock_is_held(lock_path):
                metadata = _read_json(json_path) or {"token": lock_path.stem}
                metadata["alive"] = True
                active.append(metadata)
                continue
            with contextlib.suppress(FileNotFoundError, OSError):
                lock_path.unlink()
            with contextlib.suppress(FileNotFoundError, OSError):
                json_path.unlink()

        # A JSON request without its liveness lock is from an interrupted
        # writer or an older release; it cannot own the directory.
        for json_path in requests.glob("*.json"):
            lock_path = json_path.with_suffix(".lock")
            if not lock_path.exists():
                with contextlib.suppress(FileNotFoundError, OSError):
                    json_path.unlink()
        return active

    def _write_state_locked(self) -> None:
        readers = self._active_readers_locked()
        requests = self._active_requests_locked()
        writer = _read_json(self.lock_dir / "writer.json")
        state = {
            "version": 1,
            "envdir": str(self.location),
            "lock_dir": str(self.lock_dir),
            "updated_at": _utc_now(),
            "readers": readers,
            "writer": writer,
            "requests": requests,
        }
        _write_json(self.lock_dir / "state.json", state)

    def _remove_state_if_idle(self) -> None:
        try:
            has_holder = any((self.lock_dir / "holders").glob("*.json")) or any(
                (self.lock_dir / "holders").glob("*.lock")
            )
            has_request = any((self.lock_dir / "requests").glob("*.json"))
            has_writer = (self.lock_dir / "writer.json").exists()
        except OSError:
            return
        if not has_holder and not has_request and not has_writer:
            with contextlib.suppress(FileNotFoundError, OSError):
                (self.lock_dir / "state.json").unlink()


def inspect_environment(location: str | os.PathLike[str]) -> dict[str, Any]:
    """Return inspectable lease state for *location*.

    The result includes whether operating-system locks are actually live, so
    metadata left by a killed process cannot masquerade as an active lease.
    """
    lock_dir = environment_lock_path(location)
    result: dict[str, Any] = {
        "envdir": str(Path(location).resolve(strict=False)),
        "lock_dir": str(lock_dir),
        "exists": lock_dir.exists(),
        "readers": [],
        "writer": None,
        "writer_waiting": _lock_is_held(lock_dir / "writer.lock"),
        "requests": [],
    }
    if not lock_dir.exists():
        return result

    readers: list[dict[str, Any]] = []
    holders = lock_dir / "holders"
    if holders.exists():
        for lock_path in sorted(holders.glob("*.lock")):
            metadata = _read_json(lock_path.with_suffix(".json")) or {}
            metadata["token"] = metadata.get("token", lock_path.stem)
            metadata["alive"] = _lock_is_held(lock_path)
            readers.append(metadata)
    result["readers"] = readers

    writer = _read_json(lock_dir / "writer.json")
    if writer is not None:
        writer["alive"] = _lock_is_held(lock_dir / "writer.lock")
        result["writer"] = writer

    requests_dir = lock_dir / "requests"
    if requests_dir.exists():
        requests = []
        for json_path in sorted(requests_dir.glob("*.json")):
            data = _read_json(json_path) or {}
            lock_path = json_path.with_suffix(".lock")
            data["alive"] = lock_path.exists() and _lock_is_held(lock_path)
            requests.append(data)
        result["requests"] = requests
    result["mode"] = (
        "write"
        if result["writer"] and result["writer"].get("alive")
        else "write-waiting"
        if result["writer_waiting"]
        else "read"
        if any(reader.get("alive") for reader in readers)
        else "free"
    )
    return result


# Descriptive aliases for callers/tests that think in terms of an environment
# lock rather than the shared/exclusive lease implementation.
EnvironmentLock = EnvironmentLease
environment_lease = EnvironmentLease
