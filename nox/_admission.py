"""Dynamic admission for runtime ``notify()`` requests in parallel mode.

The parallel scheduler runs every session in an isolated child process.  A
notification therefore cannot mutate the parent manifest directly.  Children
write :class:`Notification` records to a sidecar file; the parent feeds those
records to :class:`AdmissionLedger`.

The ledger is intentionally independent from process/IO concerns so that its
merge and cycle semantics can be exercised directly by interleaving callers.
"""

from __future__ import annotations

import dataclasses
import itertools
import threading
from collections import deque
from typing import Any

__all__ = [
    "AdmissionCycleError",
    "AdmissionLedger",
    "AdmissionRecord",
    "DynamicAdmission",
    "Notification",
    "session_label",
]


def session_label(session: object) -> str:
    """Return the most specific stable label for a session-like object."""
    signatures = getattr(session, "signatures", None)
    if signatures:
        return str(max(signatures, key=len))
    friendly_name = getattr(session, "friendly_name", None)
    if friendly_name:
        return str(friendly_name)
    name = getattr(session, "name", None)
    if name:
        return str(name)
    return repr(session)


@dataclasses.dataclass(frozen=True)
class Notification:
    """One ``notify()`` call received from a parallel worker."""

    sequence: int
    target: str
    source: str | None = None
    source_signatures: tuple[str, ...] = ()
    posargs: tuple[str, ...] = ()
    origin: str | None = None
    target_signature: str | None = None
    source_result: str | None = None
    source_ok: bool | None = None
    pid: int | None = None

    def to_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


class AdmissionCycleError(RuntimeError):
    """Raised when dynamic notifications close a cycle."""

    def __init__(
        self,
        cycle: list[object],
        origins: list[str | None],
    ) -> None:
        self.cycle = cycle
        self.cycle_labels = [session_label(item) for item in cycle]
        self.origins = list(origins)
        steps: list[str] = []
        for index, source in enumerate(self.cycle_labels[:-1]):
            target = self.cycle_labels[index + 1]
            origin = origins[index] if index < len(origins) else None
            steps.append(f"{source} -> {target}" + (f" ({origin})" if origin else ""))
        super().__init__("Dynamic notification cycle detected: " + "; ".join(steps))


@dataclasses.dataclass
class AdmissionRecord:
    """All notifications merged into one admitted session variant."""

    target: object
    notifications: list[Notification] = dataclasses.field(default_factory=list)

    @property
    def sources(self) -> list[str]:
        return list(dict.fromkeys(n.source for n in self.notifications if n.source))

    def to_dict(self) -> dict[str, Any]:
        target_label = session_label(self.target)
        return {
            "target": target_label,
            "variant": target_label,
            "admitted": True,
            "merged_notification_count": len(self.notifications),
            "sources": self.sources,
            "notifications": [item.to_dict() for item in self.notifications],
        }


class AdmissionLedger:
    """Thread-safe, idempotent merger for dynamic notifications."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._sequence = itertools.count(1)
        self.records: dict[object, AdmissionRecord] = {}
        self.order: list[object] = []
        self._edges: dict[object, set[object]] = {}
        self._edge_origins: dict[tuple[object, object], list[str]] = {}
        self.errors: list[AdmissionCycleError] = []

    def admit(
        self,
        target: object,
        *,
        source: object | None = None,
        posargs: object | None = None,
        origin: str | None = None,
        target_signature: str | None = None,
        source_signatures: object | None = None,
        source_result: str | None = None,
        source_ok: bool | None = None,
        pid: int | None = None,
    ) -> tuple[AdmissionRecord, bool]:
        """Admit *target*, merging a duplicate notification into its record.

        Returns the record and whether this is the first admission.  The lock
        makes the check/create pair atomic for interleaving workers.
        """
        posargs_tuple = tuple(str(arg) for arg in posargs or ())
        source_sig_tuple = tuple(str(arg) for arg in source_signatures or ())
        with self._lock:
            sequence = next(self._sequence)
            record = self.records.get(target)
            created = record is None
            if created:
                record = AdmissionRecord(target=target)
                self.records[target] = record
                self.order.append(target)

            notification = Notification(
                sequence=sequence,
                target=session_label(target),
                source=session_label(source) if source is not None else None,
                source_signatures=source_sig_tuple,
                posargs=posargs_tuple,
                origin=origin,
                target_signature=target_signature,
                source_result=source_result,
                source_ok=source_ok,
                pid=pid,
            )
            record.notifications.append(notification)

            if source is not None:
                self._edges.setdefault(source, set()).add(target)
                if origin:
                    self._edge_origins.setdefault((source, target), []).append(origin)
                cycle = self._find_cycle(target, source)
                if cycle is not None:
                    origins: list[str | None] = []
                    for index, item in enumerate(cycle[:-1]):
                        stored = self._edge_origins.get((item, cycle[index + 1]), [])
                        origins.append(stored[0] if stored else None)
                    error = AdmissionCycleError(cycle, origins)
                    self.errors.append(error)
                    raise error

            return record, created

    def _find_cycle(self, start: object, wanted: object) -> list[object] | None:
        """Find a path ``start -> ... -> wanted`` after adding ``wanted -> start``."""
        if start == wanted:
            return [wanted, start]
        queue: deque[object] = deque([start])
        parents: dict[object, object | None] = {start: None}
        while queue:
            current = queue.popleft()
            for nxt in self._edges.get(current, ()):  # type: ignore[arg-type]
                if nxt in parents:
                    continue
                parents[nxt] = current
                if nxt == wanted:
                    path = [nxt]
                    parent = current
                    while parent is not None:
                        path.append(parent)
                        parent = parents[parent]
                    path.reverse()
                    # path is start ... wanted.  Close the cycle back to start.
                    return [*path, start]
                queue.append(nxt)
        return None

    def snapshot(self) -> dict[str, dict[str, Any]]:
        with self._lock:
            return {
                session_label(target): self.records[target].to_dict()
                for target in self.order
            }


# Descriptive alias for callers that refer to this mechanism as dynamic admission.
DynamicAdmission = AdmissionLedger

