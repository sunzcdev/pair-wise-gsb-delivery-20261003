"""
Grouped rolling rollout with per-group error budgets.

When an operation is executed with ``_batch_size`` set, its hosts are admitted in
fixed-size batches, independently for every inventory group (a *service group*).
Each group can be given its own error budget via ``_error_budget``: once the
number of failed hosts in a group reaches the budget, the group stops admitting
new batches. Hosts whose batch was already admitted still run and report their
real results, and other groups with budget remaining keep rolling independently.

Groups without an entry in the error budget mapping have an unlimited budget and
roll batch by batch without ever pausing. Hosts that belong to no inventory group
are collected under :data:`UNGROUPED_KEY` and likewise have an unlimited budget.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from .exceptions import PyinfraError

if TYPE_CHECKING:
    from .host import Host
    from .inventory import Inventory

# Key under which hosts belonging to no inventory group are rolled together.
UNGROUPED_KEY = "(ungrouped)"


@dataclass
class GroupRollout:
    """
    Rollout state for a single service group.

    Batches are built once, in inventory order, and never change. ``error_count``
    and ``paused`` accumulate across the whole deploy, so a group that exhausts its
    budget during one operation stays closed to new batches in later operations.
    """

    name: str | None
    batches: list[tuple[Host, ...]]
    budget: int | None
    error_count: int = 0
    paused: bool = False
    # Index of the next batch not yet admitted. Persists across operations: a batch
    # admitted in an earlier operation is not re-admitted, but its hosts still run
    # the later operations like every other active host.
    next_batch: int = 0
    _host_to_batch: dict[Host, int] = field(default_factory=dict)
    # Failed hosts already counted, so a host reported by several operations only
    # consumes the budget once.
    _counted_failures: set[Host] = field(default_factory=set)

    def __post_init__(self) -> None:
        for index, batch in enumerate(self.batches):
            for host in batch:
                self._host_to_batch[host] = index

    @property
    def host_count(self) -> int:
        return len(self._host_to_batch)

    def has_any_host(self, hosts: set[Host]) -> bool:
        """Whether any of ``hosts`` belongs to this group."""

        return any(host in self._host_to_batch for host in hosts)

    def record_errors(self, failed_hosts: set[Host]) -> int:
        """Account for newly failed hosts and pause if the budget is exhausted.

        Only failures on hosts this group owns are counted, and each host is
        counted at most once over the lifetime of the deploy.
        """

        new_failures = {
            host
            for host in failed_hosts
            if host in self._host_to_batch and host not in self._counted_failures
        }
        self._counted_failures.update(new_failures)
        self.error_count += len(new_failures)

        if self.budget is not None and self.error_count >= self.budget:
            self.paused = True

        return len(new_failures)

    def is_host_admitted(self, host: Host) -> bool:
        """Whether ``host`` belongs to a batch already admitted (or beyond)."""

        batch_index = self._host_to_batch.get(host)
        return batch_index is not None and batch_index < self.next_batch

    def is_host_unstarted(self, host: Host) -> bool:
        """Whether ``host`` is waiting in a batch that has never been admitted."""

        batch_index = self._host_to_batch.get(host)
        return batch_index is not None and batch_index >= self.next_batch

    def admitted_batches(self) -> list[tuple[Host, ...]]:
        """Batches admitted in earlier operations; their hosts re-run every op."""

        return [batch for index, batch in enumerate(self.batches) if index < self.next_batch]

    def unstarted_hosts(self) -> list[Host]:
        return [
            host
            for index, batch in enumerate(self.batches)
            if index >= self.next_batch
            for host in batch
        ]

    def unstarted_batches(self) -> list[tuple[Host, ...]]:
        return [batch for index, batch in enumerate(self.batches) if index >= self.next_batch]


class Rollout:
    """
    Holds the per-group :class:`GroupRollout` state for a whole deploy.

    The rollout is created lazily by the state on the first operation that sets
    ``_batch_size`` and reused afterwards, so group budgets accumulate across all
    rolling operations while non-rolling operations (``_serial``, default and
    ``no_wait``) behave exactly as before.
    """

    def __init__(
        self,
        inventory: Inventory,
        batch_size: int,
        error_budget: dict[str, int] | None = None,
    ):
        if not isinstance(batch_size, int) or isinstance(batch_size, bool) or batch_size < 1:
            raise PyinfraError("Rollout `_batch_size` must be a positive integer.")

        error_budget = error_budget or {}
        unknown_groups = set(error_budget) - set(inventory.groups)
        if unknown_groups:
            raise PyinfraError(
                "Rollout `_error_budget` names unknown inventory groups: "
                + ", ".join(sorted(unknown_groups)),
            )

        for group_name, budget in error_budget.items():
            if not isinstance(budget, int) or isinstance(budget, bool) or budget < 0:
                raise PyinfraError(
                    f"Rollout error budget for group {group_name!r} "
                    "must be a non-negative integer.",
                )

        self.batch_size = batch_size
        self.error_budget = dict(error_budget)

        # Assign every host to exactly one service group: the first inventory
        # group (in inventory definition order) it belongs to. Ungrouped hosts
        # share a synthetic group with an unlimited budget.
        group_to_hosts: dict[str | None, list[Host]] = {}
        group_names = list(inventory.groups)

        for host in inventory:
            chosen_group: str | None = None
            for group_name in group_names:
                if host in inventory.groups[group_name]:
                    chosen_group = group_name
                    break
            group_to_hosts.setdefault(chosen_group, []).append(host)

        self.groups: dict[str | None, GroupRollout] = {}
        for group_name, hosts in group_to_hosts.items():
            key = group_name if group_name is not None else UNGROUPED_KEY
            budget = error_budget.get(group_name) if group_name is not None else None
            batches = [
                tuple(hosts[offset : offset + batch_size])
                for offset in range(0, len(hosts), batch_size)
            ]
            self.groups[key] = GroupRollout(name=group_name, batches=batches, budget=budget)

        self._host_to_group: dict[Host, GroupRollout] = {
            host: group for group in self.groups.values() for host in group._host_to_batch
        }

    def get_group_for_host(self, host: Host) -> GroupRollout:
        return self._host_to_group[host]

    def is_paused(self, host: Host) -> bool:
        return self.get_group_for_host(host).paused

    def is_host_unstarted(self, host: Host) -> bool:
        return self.get_group_for_host(host).is_host_unstarted(host)

    def get_unstarted_batches(self) -> dict[str | None, list[tuple[Host, ...]]]:
        """Map of group name -> batches that have never admitted any host."""

        return {
            group.name: group.unstarted_batches()
            for group in self.groups.values()
            if group.next_batch < len(group.batches)
        }
