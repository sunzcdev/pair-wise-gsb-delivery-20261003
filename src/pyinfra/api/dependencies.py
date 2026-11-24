from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from graphlib import CycleError, TopologicalSorter
import time
from typing import TYPE_CHECKING, Any

from gevent.event import Event

from .exceptions import (
    CrossHostDependencyError,
    CrossHostDependencyFailed,
    CrossHostDependencyTimeout,
    UnresolvableCrossHostDependency,
)

if TYPE_CHECKING:
    from .state import State


class DependencyStatus(str, Enum):
    """Settlement state for one edge in the cross-host operation DAG."""

    PENDING = "pending"
    SATISFIED = "satisfied"
    FAILED = "failed"
    TIMED_OUT = "timed_out"
    UNRESOLVABLE = "unresolvable"
    BLOCKED = "blocked"


_BAD_EDGE_STATUSES = {
    DependencyStatus.FAILED,
    DependencyStatus.TIMED_OUT,
    DependencyStatus.UNRESOLVABLE,
}


@dataclass(frozen=True)
class OperationHandle:
    """
    Stable reference to one operation node on one host.

    An operation hash alone is not enough: identical operation calls can share a hash while
    host-specific ordering and recovery state remain per host.
    """

    host_name: str
    op_hash: str
    name: str | None = None

    @property
    def node(self) -> NodeRef:
        return NodeRef(self.host_name, self.op_hash)

    @classmethod
    def coerce(cls, value: Any) -> OperationHandle:
        if isinstance(value, OperationHandle):
            return value

        # ``add_op(..., host=host)`` returns ``{host: OperationMeta}``.
        if isinstance(value, dict):
            if len(value) != 1:
                raise TypeError(
                    "Cross-host dependency handles supplied as a dict must contain exactly "
                    f"one host/operation pair, got {len(value)}."
                )
            host, operation = next(iter(value.items()))
            return cls._from_host_and_operation(host, operation)

        if isinstance(value, (tuple, list)) and len(value) == 2:
            host, operation = value
            return cls._from_host_and_operation(host, operation)

        host = getattr(value, "_host", None)
        if host is not None:
            return cls._from_host_and_operation(host, value)

        raise TypeError(
            "Cross-host dependency must be an OperationHandle, (host, operation) pair, or "
            f"single-host add_op() result; got {type(value).__name__}."
        )

    @classmethod
    def coerce_many(cls, value: Any) -> list[OperationHandle]:
        """Coerce one target or a mapping/sequence of host-specific targets."""

        if isinstance(value, dict):
            return [cls._from_host_and_operation(host, op) for host, op in value.items()]

        if isinstance(value, (list, tuple)):
            # A two-item (host, operation_hash/meta) pair is one handle, not two handles.
            first = value[0] if value else None
            looks_like_host = isinstance(first, str) or (
                first is not None
                and hasattr(first, "name")
                and not isinstance(first, OperationHandle)
            )
            if len(value) == 2 and looks_like_host:
                return [cls.coerce(value)]
            return [cls.coerce(item) for item in value]

        return [cls.coerce(value)]

    @classmethod
    def _from_host_and_operation(cls, host: Any, operation: Any) -> OperationHandle:
        host_name = getattr(host, "name", host)
        if not isinstance(host_name, str) or not host_name:
            raise TypeError(f"Cross-host dependency host has no valid name: {host!r}")

        op_hash = getattr(operation, "_hash", None)
        if op_hash is None and isinstance(operation, str):
            op_hash = operation
        if not isinstance(op_hash, str) or not op_hash:
            raise TypeError(
                f"Cross-host dependency operation must have a non-empty '_hash'; got {operation!r}"
            )

        name = None
        names = getattr(getattr(operation, "operation_meta", None), "names", None)
        if names:
            name = ", ".join(sorted(names))
        return cls(host_name=host_name, op_hash=op_hash, name=name)

    def describe(self) -> str:
        label = self.name or "operation"
        return f"{label} [{self.op_hash[:12]}] on {self.host_name}"


@dataclass(frozen=True)
class NodeRef:
    host_name: str
    op_hash: str


@dataclass
class NodeSettlement:
    handle: OperationHandle
    status: DependencyStatus
    reason: str | None = None
    settled_at: float = field(default_factory=time.monotonic)


@dataclass
class CrossHostDependency:
    """
    One explicit edge: ``target`` may run only after ``source`` has succeeded.
    """

    source: OperationHandle
    target: OperationHandle
    timeout: float | None = None
    name: str | None = None
    status: DependencyStatus = DependencyStatus.PENDING
    reason: str | None = None
    deadline: float | None = field(default=None, repr=False)

    def describe(self) -> str:
        prefix = f"{self.name}: " if self.name else ""
        return f"{prefix}{self.source.describe()} -> {self.target.describe()}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "source": {"host": self.source.host_name, "op_hash": self.source.op_hash},
            "target": {"host": self.target.host_name, "op_hash": self.target.op_hash},
            "timeout": self.timeout,
            "status": self.status.value,
            "reason": self.reason,
        }


class CrossHostDependencyGraph:
    """
    Mutable cross-host edge registry and settlement state for one :class:`State` run.

    The graph only coordinates explicit edges. It never inserts barriers for hosts or
    operations that have no registered edge, so unrelated host streams remain runnable.
    """

    def __init__(self, edges: list[CrossHostDependency] | None = None) -> None:
        self.edges: list[CrossHostDependency] = edges or []
        self.settlements: dict[NodeRef, NodeSettlement] = {}
        self._event = Event()
        self._armed = False

    def add_edge(
        self,
        source: OperationHandle | Any,
        target: OperationHandle | Any,
        *,
        timeout: float | None = None,
        name: str | None = None,
    ) -> CrossHostDependency:
        if self._armed:
            raise RuntimeError("Cannot add cross-host dependencies after a run has started.")
        self._validate_timeout(timeout)
        source_handle = OperationHandle.coerce(source)
        target_handle = OperationHandle.coerce(target)
        edge = CrossHostDependency(
            source=source_handle,
            target=target_handle,
            timeout=timeout,
            name=name,
        )
        self.edges.append(edge)
        return edge

    def require(
        self,
        target: OperationHandle | Any,
        sources: list[OperationHandle | Any] | tuple[OperationHandle | Any, ...],
        *,
        timeout: float | None = None,
        name: str | None = None,
    ) -> list[CrossHostDependency]:
        """Require every source operation to succeed before ``target`` starts."""

        if not isinstance(sources, (list, tuple)) or not sources:
            raise TypeError("'sources' must be a non-empty list or tuple of operation handles.")
        target_handles = OperationHandle.coerce_many(target)
        source_handles = [OperationHandle.coerce(source) for source in sources]
        edges: list[CrossHostDependency] = []
        for target_handle in target_handles:
            for i, source_handle in enumerate(source_handles):
                edge_name = name
                if name and len(source_handles) > 1:
                    edge_name = f"{name} ({i + 1}/{len(source_handles)})"
                edges.append(
                    self.add_edge(
                        source_handle,
                        target_handle,
                        timeout=timeout,
                        name=edge_name,
                    )
                )
        return edges

    @staticmethod
    def _validate_timeout(timeout: float | None) -> None:
        if timeout is None:
            return
        if isinstance(timeout, bool) or not isinstance(timeout, (int, float)):
            raise TypeError("Cross-host dependency timeout must be a number of seconds or None.")
        if timeout <= 0:
            raise ValueError("Cross-host dependency timeout must be greater than zero.")

    def arm(self, state: State | None = None, *, now: float | None = None) -> None:
        """
        Prepare deadlines and validate references.

        Already satisfied edges are preserved so an explicit recovery can reuse evidence from
        a genuine success. Pending edges receive a fresh bounded deadline.
        """

        current_time = time.monotonic() if now is None else now
        for edge in self.edges:
            if edge.status is DependencyStatus.PENDING and edge.timeout is not None:
                edge.deadline = current_time + edge.timeout
            if state is not None and edge.status is DependencyStatus.PENDING:
                reason = self._missing_reference_reason(state, edge)
                if reason is not None:
                    edge.status = DependencyStatus.UNRESOLVABLE
                    edge.reason = reason
                    self.settlements[edge.source.node] = NodeSettlement(
                        edge.source,
                        DependencyStatus.UNRESOLVABLE,
                        reason,
                        current_time,
                    )
        if state is not None:
            self.validate_dag(state)
        self._armed = True
        self._notify()

    def validate_dag(self, state: State) -> list[NodeRef]:
        """
        Topologically validate local host ordering plus explicit cross-host edges.

        Returns a ready-order of host-specific operation nodes. Execution still uses host
        streams and edge events so unrelated nodes do not become a global barrier.
        """

        ts: TopologicalSorter = TopologicalSorter()

        for host in state.inventory:
            previous: NodeRef | None = None
            for op_hash in host.op_hash_order:
                node = NodeRef(host.name, op_hash)
                if previous is None:
                    ts.add(node)
                else:
                    ts.add(node, previous)
                previous = node

        for edge in self.edges:
            ts.add(edge.target.node, edge.source.node)

        try:
            ts.prepare()
        except CycleError as error:
            cycle = " -> ".join(f"{node.host_name}:{node.op_hash[:8]}" for node in error.args[1])
            raise CrossHostDependencyError(
                f"Cycle detected in host/cross-host operation DAG: {cycle}"
            ) from error

        order: list[NodeRef] = []
        while ts.is_active():
            ready = list(ts.get_ready())
            ts.done(*ready)
            order.extend(ready)
        return order

    @staticmethod
    def _missing_reference_reason(state: State, edge: CrossHostDependency) -> str | None:
        for role, handle in (("source", edge.source), ("target", edge.target)):
            host = state.inventory.hosts.get(handle.host_name)
            if host is None:
                return f"{role} host {handle.host_name!r} is not in the inventory"
            if handle.op_hash not in host.op_hash_order:
                return (
                    f"{role} operation {handle.op_hash[:12]!r} is not planned on host "
                    f"{handle.host_name!r}"
                )
            if handle.op_hash not in state.ops.get(host, {}):
                return (
                    f"{role} operation {handle.op_hash[:12]!r} has no operation data on host "
                    f"{handle.host_name!r}"
                )
        return None

    def settle_node(
        self,
        handle: OperationHandle,
        success: bool,
        reason: str | None = None,
    ) -> NodeSettlement:
        node = handle.node
        existing = self.settlements.get(node)
        if existing is not None and existing.status in {
            DependencyStatus.SATISFIED,
            DependencyStatus.FAILED,
        }:
            return existing

        status = DependencyStatus.SATISFIED if success else DependencyStatus.FAILED
        settlement = NodeSettlement(handle, status, reason)
        self.settlements[node] = settlement

        for edge in self._outgoing(node):
            if edge.status is not DependencyStatus.PENDING:
                continue
            edge.status = status
            edge.reason = reason or (None if success else f"{handle.describe()} settled as failed")
        self._notify()
        return settlement

    def expire_edges(self, *, now: float | None = None) -> list[CrossHostDependency]:
        """Transition pending edges past their deadlines to timed-out settlements."""

        current_time = time.monotonic() if now is None else now
        newly_timed_out: list[CrossHostDependency] = []
        for edge in self.edges:
            if (
                edge.status is DependencyStatus.PENDING
                and edge.deadline is not None
                and edge.deadline <= current_time
            ):
                edge.status = DependencyStatus.TIMED_OUT
                timeout = edge.timeout
                edge.reason = (
                    f"{edge.source.describe()} did not settle within {timeout:g}s; "
                    "the edge is not satisfied and the dependent operation will not start"
                )
                self.settlements.setdefault(
                    edge.source.node,
                    NodeSettlement(edge.source, DependencyStatus.TIMED_OUT, edge.reason),
                )
                newly_timed_out.append(edge)
        if newly_timed_out:
            self._notify()
        return newly_timed_out

    def wait_for_target(self, target: OperationHandle | NodeRef) -> None:
        """Block only the calling target host stream until its incoming edges settle."""

        node = target.node if isinstance(target, OperationHandle) else target
        incoming = self._incoming(node)
        if not incoming:
            return

        while True:
            self.expire_edges()
            bad = [edge for edge in incoming if edge.status in _BAD_EDGE_STATUSES]
            if bad:
                raise self._exception_for_bad_edges(bad)

            if all(edge.status is DependencyStatus.SATISFIED for edge in incoming):
                return

            wait_timeout = self._next_wait_timeout(incoming)

            # Clear and re-check in this order to avoid losing a settlement notification.
            self._event.clear()
            self.expire_edges()
            bad = [edge for edge in incoming if edge.status in _BAD_EDGE_STATUSES]
            if bad:
                raise self._exception_for_bad_edges(bad)
            if all(edge.status is DependencyStatus.SATISFIED for edge in incoming):
                return

            self._event.wait(wait_timeout)

    def block_target(self, handle: OperationHandle, reason: str) -> NodeSettlement:
        """Record that a dependent target was intentionally not started."""

        settlement = NodeSettlement(handle, DependencyStatus.BLOCKED, reason)
        self.settlements[handle.node] = settlement
        self._notify()
        return settlement

    def recovery_plan(self) -> dict[str, list[dict[str, Any]]]:
        """
        Explain which edges may be reused and which must be established again.

        Only explicitly successful edges appear as satisfied. Failed, timed-out and
        unresolvable edges are never promoted by recovery.
        """

        return {
            "satisfied": [
                edge.to_dict() for edge in self.edges if edge.status is DependencyStatus.SATISFIED
            ],
            "retry": [
                edge.to_dict()
                for edge in self.edges
                if edge.status
                in {
                    DependencyStatus.PENDING,
                    DependencyStatus.TIMED_OUT,
                    DependencyStatus.UNRESOLVABLE,
                }
            ],
            "failed": [
                edge.to_dict() for edge in self.edges if edge.status is DependencyStatus.FAILED
            ],
        }

    def recover(self, *, reset_failed: bool = False) -> dict[str, list[dict[str, Any]]]:
        """
        Prepare an explicit recovery attempt.

        Timed-out/unreachable and still-pending edges are reset and must be proved by a real
        source success in the recovered run. Satisfied edges are retained. Known failed edges
        remain failed unless an operator explicitly requests ``reset_failed=True`` after fixing
        the cause.
        """

        plan = self.recovery_plan()
        retry_statuses = {
            DependencyStatus.PENDING,
            DependencyStatus.TIMED_OUT,
            DependencyStatus.UNRESOLVABLE,
        }
        if reset_failed:
            retry_statuses.add(DependencyStatus.FAILED)

        retry_nodes = set()
        blocked_targets = set()
        for edge in self.edges:
            if edge.status in retry_statuses:
                edge.status = DependencyStatus.PENDING
                edge.reason = None
                edge.deadline = None
                retry_nodes.add(edge.source.node)
                blocked_targets.add(edge.target.node)

        for node in retry_nodes:
            settlement = self.settlements.get(node)
            if settlement is not None and settlement.status is not DependencyStatus.SATISFIED:
                self.settlements.pop(node, None)

        for node in blocked_targets:
            settlement = self.settlements.get(node)
            if settlement is not None and settlement.status is DependencyStatus.BLOCKED:
                self.settlements.pop(node, None)

        self._armed = False
        self._notify()
        return plan

    def resume(self, *, reset_failed: bool = False) -> dict[str, list[dict[str, Any]]]:
        """Alias for :meth:`recover` using operational resume vocabulary."""

        return self.recover(reset_failed=reset_failed)

    @property
    def bad_edges(self) -> list[CrossHostDependency]:
        return [edge for edge in self.edges if edge.status in _BAD_EDGE_STATUSES]

    def raise_if_unsatisfied(self) -> None:
        bad = self.bad_edges
        if bad:
            raise self._exception_for_bad_edges(bad)

    def status_report(self) -> list[dict[str, Any]]:
        return [edge.to_dict() for edge in self.edges]

    def settlement_report(self) -> dict[str, dict[str, Any]]:
        return {
            f"{node.host_name}:{node.op_hash}": {
                "status": settlement.status.value,
                "reason": settlement.reason,
            }
            for node, settlement in self.settlements.items()
        }

    def _incoming(self, node: NodeRef) -> list[CrossHostDependency]:
        return [edge for edge in self.edges if edge.target.node == node]

    def _outgoing(self, node: NodeRef) -> list[CrossHostDependency]:
        return [edge for edge in self.edges if edge.source.node == node]

    def _next_wait_timeout(self, edges: list[CrossHostDependency]) -> float | None:
        deadlines = [edge.deadline for edge in edges if edge.deadline is not None]
        if not deadlines:
            return None
        return max(0.0, min(deadlines) - time.monotonic())

    def _notify(self) -> None:
        self._event.set()

    @staticmethod
    def _exception_for_bad_edges(
        edges: list[CrossHostDependency],
    ) -> CrossHostDependencyError:
        lines = ["Cross-host operation dependency is not satisfied:"]
        for edge in edges:
            lines.append(f"  - {edge.describe()}")
            lines.append(f"      status={edge.status.value}; {edge.reason or 'no reason provided'}")
        message = "\n".join(lines)

        if any(edge.status is DependencyStatus.TIMED_OUT for edge in edges):
            return CrossHostDependencyTimeout(message)
        if any(edge.status is DependencyStatus.UNRESOLVABLE for edge in edges):
            return UnresolvableCrossHostDependency(message)
        return CrossHostDependencyFailed(message)
