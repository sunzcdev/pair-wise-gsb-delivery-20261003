"""
Host identity: the explicit check plan reuse uses to decide whether an inventory
name that is still displayed the same way really points at the same machine.

``host.name`` is only an alias. Editing connector data such as ``ssh_hostname`` can
re-point an alias at a replacement machine while every display name in the inventory
stays identical. Plan reuse therefore verifies the *connector target* rather than the
name. Connectors declare which of their data keys describe the target through
``BaseConnector.identity_keys`` (or override ``BaseConnector.get_identity_target``).

Inventory data that does not describe the target - group membership, group data,
unrelated host data such as a human label - is deliberately excluded from the
identity, so annotating a host does not discard the results already confirmed for it.

An inventory can also state the identity explicitly, bypassing connector derivation,
with per-host ``host_identity`` data:

.. code:: python

    hosts = [
        ("web-01", {"host_identity": "rack-12-slot-4"}),
    ]

This is useful when the connector target cannot describe the machine, or when the
actual machine is tracked elsewhere. The key is read from *host* data only: a group
or global value would give every host in the group the same identity.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from hashlib import sha1
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from .host import Host

#: Host data key holding an explicit identity for the host. When set it wins over the
#: connector-derived target for the plan reuse identity check.
HOST_IDENTITY_DATA_KEY = "host_identity"

_UNSET = object()


def canonical_json(value: Any) -> str:
    """
    Return a stable JSON representation of ``value``, used to compare identity targets.

    Dict keys are sorted so equal targets serialise identically, and values that JSON
    cannot encode (paths, compiled patterns, arbitrary objects) fall back to ``repr`` so
    an identity can always be derived.
    """

    try:
        return json.dumps(value, sort_keys=True, separators=(",", ":"), default=repr)
    except (TypeError, ValueError):
        return repr(value)


@dataclass(frozen=True)
class HostIdentity:
    """
    The identity of a target machine as seen from the controller.

    ``name`` is the inventory alias (the display name), while ``connector`` and
    ``target`` describe where that alias actually points. Two identities match when
    their :attr:`fingerprint` matches - the same connector describing the same target -
    regardless of the alias used to look them up.
    """

    #: Inventory alias (display name) of the host.
    name: str
    #: Connector class name, eg ``SSHConnector``.
    connector: str
    #: Canonical JSON of the target description, eg ``{"ssh_hostname": "10.0.0.1"}``.
    target: str
    #: Where the target came from: ``"connector"`` or ``"host_identity"``.
    source: str = "connector"

    @property
    def target_data(self) -> Any:
        """
        The decoded target description, as returned by the connector.
        """

        try:
            return json.loads(self.target)
        except ValueError:
            return self.target

    @property
    def fingerprint(self) -> str:
        """
        Stable hash of the connector and target. Equal fingerprints mean the alias
        points at the same machine.
        """

        payload = f"{self.connector}\x00{self.target}"
        return sha1(payload.encode("utf-8")).hexdigest()

    def matches(self, other: HostIdentity) -> bool:
        """
        Return whether this identity points at the same machine as ``other``.
        """

        return self.fingerprint == other.fingerprint

    def describe(self) -> str:
        """
        Human readable form of the target, eg ``SSHConnector(ssh_hostname=web-01)``.
        """

        data = self.target_data
        if isinstance(data, dict):
            bits = ", ".join(f"{key}={value}" for key, value in data.items())
            return f"{self.connector}({bits})"
        return f"{self.connector}({data})"

    def as_dict(self) -> dict[str, Any]:
        """
        Serialise this identity for a plan reuse file or JSON report.
        """

        return {
            "name": self.name,
            "connector": self.connector,
            "target": self.target_data,
            "source": self.source,
            "fingerprint": self.fingerprint,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> HostIdentity:
        """
        Restore an identity written by :meth:`as_dict`.
        """

        return cls(
            name=data["name"],
            connector=data["connector"],
            target=canonical_json(data.get("target")),
            source=data.get("source", "connector"),
        )


def resolve_host_identity(host: Host) -> HostIdentity:
    """
    Derive the identity of ``host`` from explicit ``host_identity`` data or from its
    connector target.

    Connector data keys come from ``BaseConnector.identity_keys`` /
    ``BaseConnector.get_identity_target``. When a connector describes no target the
    inventory name is used, so hosts without a connector-level machine remain
    identifiable.
    """

    explicit = host.host_data.get(HOST_IDENTITY_DATA_KEY, _UNSET)
    connector_cls = host.connector_cls
    connector_name = getattr(connector_cls, "__name__", "connector")

    if explicit is not _UNSET and explicit is not None:
        return HostIdentity(
            name=host.name,
            connector=connector_name,
            target=canonical_json({"explicit": explicit}),
            source="host_identity",
        )

    target = connector_cls.get_identity_target(host)
    if not target:
        target = {"name": host.name}

    return HostIdentity(
        name=host.name,
        connector=connector_name,
        target=canonical_json(target),
    )
