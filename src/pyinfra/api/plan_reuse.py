"""
Plan reuse: carry confirmed facts and confirmed step results across a regenerated
plan, gated by an explicit host identity check.

A plan is generated once (prepare), reviewed, then executed. When the same deploy is
re-planned later - after the inventory was edited, or on a fresh run against the same
targets - discarding work already confirmed for a target that has not changed is both
wasteful and, if done by name alone, unsafe.

The reuse rule is explicit here:

+ A stored record is bound to a :class:`~pyinfra.api.identity.HostIdentity`, not to the
  inventory display name. The identity is derived from the *actual connector target*
  (see :func:`resolve_host_identity`), so an alias that now points at a replacement
  machine is a different identity even though ``host.name`` is unchanged.
+ A host inherits a stored record only when the stored identity **matches** the identity
  resolved for it now. Otherwise the record is refused and the host starts from an empty
  plan - it never borrows the old result.
+ Group membership and group data are *not* part of the identity. Adding a group label
  (or editing a group note) that does not change operation behaviour leaves the identity
  matching, so the host still resolves its own confirmed record.

Usage
-----

.. code:: python

    from pyinfra.api.plan_reuse import PlanReuseStore

    store = PlanReuseStore.from_dict(payload)      # a previously saved plan
    snapshot = store.snapshot(state)               # resolve against the current state

    # ... run the deploy ...

    saved = store.to_dict(state)                   # facts/steps newly confirmed
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

from pyinfra import logger

from .identity import HostIdentity, resolve_host_identity

if TYPE_CHECKING:
    from .host import Host
    from .state import State

#: Version of the on-disk plan reuse format.
PLAN_REUSE_VERSION = 1


@dataclass
class ConfirmedStep:
    """
    A step (operation) that a previous plan confirmed against a target.

    ``success`` mirrors the operation result; ``commands`` is the number of commands the
    step ran, so a reused step can report whether it changed anything without replaying.
    """

    success: bool
    did_change: bool
    commands: int = 0

    def as_dict(self) -> dict[str, Any]:
        return {
            "success": self.success,
            "did_change": self.did_change,
            "commands": self.commands,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> ConfirmedStep:
        return cls(
            success=bool(data.get("success")),
            did_change=bool(data.get("did_change")),
            commands=int(data.get("commands") or 0),
        )


@dataclass
class HostPlanRecord:
    """
    Everything confirmed for a single target in a previous plan.

    ``identity`` binds the record to the machine, not the inventory name. ``facts`` maps
    fact cache keys to their confirmed values, and ``steps`` maps operation hashes to the
    confirmed step result.
    """

    identity: HostIdentity
    facts: dict[str, Any] = field(default_factory=dict)
    steps: dict[str, ConfirmedStep] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        return {
            "identity": self.identity.as_dict(),
            "facts": self.facts,
            "steps": {op_hash: step.as_dict() for op_hash, step in self.steps.items()},
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> HostPlanRecord:
        return cls(
            identity=HostIdentity.from_dict(data["identity"]),
            facts=dict(data.get("facts") or {}),
            steps={
                op_hash: ConfirmedStep.from_dict(step)
                for op_hash, step in (data.get("steps") or {}).items()
            },
        )


@dataclass
class ReusedHost:
    """
    The outcome of matching a stored record against the current inventory.

    ``reason`` is ``"match"`` when the identity matched, otherwise one of:

    + ``"identity_changed"`` - same display name, different connector target
    + ``"new_host"`` - no stored record existed for this display name
    """

    host: Host
    identity: HostIdentity
    reason: str
    record: HostPlanRecord | None = None

    @property
    def reused(self) -> bool:
        return self.reason == "match" and self.record is not None


class PlanReuseSnapshot:
    """
    Per-run view over a :class:`PlanReuseStore`, resolved once against a state.

    Every host in the state's inventory is resolved to a :class:`ReusedHost`, recorded in
    :attr:`resolved` keyed by display name. Facts and confirmed steps are only served for
    hosts whose identity matched, so a refused host has nothing to borrow.
    """

    def __init__(self, store: PlanReuseStore, state: State):
        self.store = store
        self.state = state
        self.resolved: dict[str, ReusedHost] = {}

        for host in state.inventory:
            self.resolved[host.name] = self._resolve_host(host)

    def _resolve_host(self, host: Host) -> ReusedHost:
        identity = resolve_host_identity(host)

        if not self.store.enabled:
            return ReusedHost(host=host, identity=identity, reason="new_host")

        record = self.store.records.get(host.name)
        if record is None:
            return ReusedHost(host=host, identity=identity, reason="new_host")

        if not record.identity.matches(identity):
            logger.debug(
                "Plan reuse refused for %s: identity changed from %s to %s",
                host.name,
                record.identity.describe(),
                identity.describe(),
            )
            # Keep the record so the refusal can report the old target, but ``reused``
            # stays False so nothing from it is served.
            return ReusedHost(
                host=host,
                identity=identity,
                reason="identity_changed",
                record=record,
            )

        return ReusedHost(host=host, identity=identity, reason="match", record=record)

    def facts_for(self, host: Host) -> dict[str, Any]:
        """
        Confirmed facts for ``host``, empty when the identity did not match.
        """

        reused = self.resolved.get(host.name)
        if reused is None or not reused.reused:
            return {}
        if reused.record is None:  # pragma: no cover - reused implies a record
            return {}
        return dict(reused.record.facts)

    def confirmed_step(self, host: Host, op_hash: str) -> ConfirmedStep | None:
        """
        The confirmed step result for ``op_hash`` on ``host``, or ``None`` when the
        identity did not match or the step was not confirmed (only successful, completed
        steps are ever stored).
        """

        reused = self.resolved.get(host.name)
        if reused is None or not reused.reused or reused.record is None:
            return None
        step = reused.record.steps.get(op_hash)
        if step is None or not step.success:
            return None
        return step

    def as_dict(self) -> dict[str, Any]:
        """
        Report every host's identity decision, for machine readable plan output.

        Each entry carries ``reused`` and the ``reason``. A refused host also carries its
        ``previous_identity`` so the caller can see the old target next to the new one; a
        reused host lists the facts and steps it inherited.
        """

        report: dict[str, Any] = {}
        for name, reused in self.resolved.items():
            entry: dict[str, Any] = {
                "reused": reused.reused,
                "reason": reused.reason,
                "identity": reused.identity.as_dict(),
            }
            if reused.reason == "identity_changed" and reused.record is not None:
                entry["previous_identity"] = reused.record.identity.as_dict()
            if reused.reused and reused.record is not None:
                entry["facts"] = sorted(reused.record.facts)
                entry["steps"] = sorted(reused.record.steps)
            report[name] = entry
        return report


class PlanReuseStore:
    """
    A saved plan: one :class:`HostPlanRecord` per inventory name, keyed by the name the
    record was confirmed under. Whether that name still points at the same machine is
    decided per run by :class:`PlanReuseSnapshot`.
    """

    def __init__(self, records: dict[str, HostPlanRecord] | None = None, enabled: bool = True):
        self.records = records or {}
        self.enabled = enabled

    def snapshot(self, state: State) -> PlanReuseSnapshot:
        """
        Resolve this store against ``state``, producing a per-run snapshot.
        """

        return PlanReuseSnapshot(self, state)

    def update_from_state(self, state: State) -> None:
        """
        Record the facts/steps confirmed by ``state`` against each host's *current*
        identity.

        Only identity-matched hosts keep their previous facts/steps; a host whose identity
        changed starts a fresh record bound to the new target, so a later reload compares
        against the machine actually used.
        """

        snapshot = self.snapshot(state)

        for host in state.inventory:
            reused = snapshot.resolved[host.name]
            existing = reused.record if reused.reused else None

            facts = dict(existing.facts) if existing is not None else {}
            steps = dict(existing.steps) if existing is not None else {}

            new_facts = getattr(state, "confirmed_facts", {}).get(host.name, {})
            for cache_key, value in new_facts.items():
                facts[cache_key] = value

            for op_hash in state.get_op_order():
                if op_hash not in state.ops[host]:
                    continue
                op_meta = state.get_op_data_for_host(host, op_hash).operation_meta
                if not op_meta.is_complete():
                    continue
                # A step that just failed is no longer confirmed on this machine, so drop
                # any earlier confirmation: it must run again next time.
                if not op_meta.did_succeed(_raise_if_not_complete=False):
                    steps.pop(op_hash, None)
                    continue
                steps[op_hash] = ConfirmedStep(
                    success=True,
                    did_change=op_meta.did_change(),
                    commands=op_meta.command_count,
                )

            self.records[host.name] = HostPlanRecord(
                identity=reused.identity,
                facts=facts,
                steps=steps,
            )

    def to_dict(self, state: State | None = None) -> dict[str, Any]:
        """
        Serialise this store, optionally refreshed from ``state`` first.
        """

        if state is not None:
            self.update_from_state(state)

        return {
            "version": PLAN_REUSE_VERSION,
            "records": {name: record.as_dict() for name, record in self.records.items()},
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None) -> PlanReuseStore:
        """
        Load a store from the output of :meth:`to_dict`.

        A missing or malformed payload yields a disabled store, so callers can pass the
        raw result of reading an optional file.
        """

        if not data:
            return cls(enabled=False)

        version = data.get("version")
        if version != PLAN_REUSE_VERSION:
            logger.warning("Ignoring plan reuse data with unknown version: %r", version)
            return cls(enabled=False)

        records = {
            name: HostPlanRecord.from_dict(record)
            for name, record in (data.get("records") or {}).items()
        }
        return cls(records=records, enabled=True)
