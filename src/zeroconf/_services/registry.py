"""A pure python implementation of multicast DNS service discovery.

Licensed under LGPL-2.1-or-later; see COPYING for details. This file is
part of a continuously modified work; modification dates are recorded
in the project's git history.
"""

from __future__ import annotations

from .._exceptions import ServiceNameAlreadyRegistered
from .info import ServiceInfo

_str = str
_ServiceInfo = ServiceInfo
_ServiceIndex = dict[str, dict[str, ServiceInfo]]
_ServerReferenceIndex = dict[str, dict[tuple[str, int], int]]
_DrainedInterfaces = dict[str, set[tuple[str, int]]]


class ServiceRegistry:
    """A registry to keep track of services.

    The registry must only be accessed from
    the event loop as it is not thread safe.
    """

    __slots__ = ("_server_drained", "_server_references", "_services", "has_entries", "servers", "types")

    def __init__(
        self,
    ) -> None:
        """Create the ServiceRegistry class."""
        self._services: dict[str, ServiceInfo] = {}
        self.types: _ServiceIndex = {}
        self.servers: _ServiceIndex = {}
        self._server_references: _ServerReferenceIndex = {}
        self._server_drained: _DrainedInterfaces = {}
        self.has_entries: bool = False

    def async_add(self, info: ServiceInfo) -> None:
        """Add a new service to the registry."""
        self._add(info)

    def async_decrement_server_reference(
        self, info: ServiceInfo, interface_key: tuple[str, int]
    ) -> None:
        """Drop one per-interface reference to an info's server.

        An interface without an explicit count implicitly references every
        registered instance, so the first decrement seeds it from the current
        number of infos on the server before subtracting one. Interfaces that
        already reached zero stay drained instead of being reseeded.
        """
        assert info.server_key is not None
        drained = self._server_drained.setdefault(info.server_key, set())
        if interface_key in drained:
            return
        interface_buckets = self._server_references.setdefault(info.server_key, {})
        if interface_key not in interface_buckets:
            interface_buckets[interface_key] = len(self.servers.get(info.server_key, {}))
        count = interface_buckets[interface_key]
        if count <= 1:
            del interface_buckets[interface_key]
            drained.add(interface_key)
            if not interface_buckets:
                del self._server_references[info.server_key]
        else:
            interface_buckets[interface_key] = count - 1

    def async_get_info_name(self, name: str) -> ServiceInfo | None:
        """Return all ServiceInfo for the name."""
        return self._services.get(name)

    def async_get_infos_server(self, server: str) -> list[ServiceInfo]:
        """Return all ServiceInfo matching server."""
        return self._async_get_by_index(self.servers, server)

    def async_get_infos_type(self, type_: str) -> list[ServiceInfo]:
        """Return all ServiceInfo matching type."""
        return self._async_get_by_index(self.types, type_)

    def async_get_server_reference_interfaces(self, server: str) -> set[tuple[str, int]]:
        """Return the interfaces that still hold a reference to the server."""
        interface_buckets = self._server_references.get(server)
        return set(interface_buckets) if interface_buckets else set()

    def async_get_service_infos(self) -> list[ServiceInfo]:
        """Return all ServiceInfo."""
        return list(self._services.values())

    def async_get_types(self) -> list[str]:
        """Return all types."""
        return list(self.types)

    def async_increment_server_reference(
        self, info: ServiceInfo, interface_key: tuple[str, int]
    ) -> None:
        """Add one per-interface reference to an info's server."""
        assert info.server_key is not None
        drained = self._server_drained.get(info.server_key)
        if drained is not None:
            drained.discard(interface_key)
            if not drained:
                del self._server_drained[info.server_key]
        interface_buckets = self._server_references.setdefault(info.server_key, {})
        interface_buckets[interface_key] = interface_buckets.get(interface_key, 0) + 1

    def async_is_server_reference_drained(
        self, info: ServiceInfo, interface_key: tuple[str, int]
    ) -> bool:
        """Return whether the interface already withdrew the info's server address."""
        assert info.server_key is not None
        drained = self._server_drained.get(info.server_key)
        return bool(drained and interface_key in drained)

    def async_remove(self, info: list[ServiceInfo] | ServiceInfo) -> None:
        """Remove a new service from the registry."""
        self._remove(info if isinstance(info, list) else [info])

    def async_update(self, info: ServiceInfo) -> None:
        """Update new service in the registry."""
        self._remove([info])
        self._add(info)

    def _add(self, info: ServiceInfo) -> None:
        """Add a new service under the lock."""
        assert info.server_key is not None, "ServiceInfo must have a server"
        if info.key in self._services:
            raise ServiceNameAlreadyRegistered

        info.async_clear_cache()
        self._services[info.key] = info
        # insertion order matters: async_get_infos_type/server return registration order
        self.types.setdefault(info.type.lower(), {})[info.key] = info
        self.servers.setdefault(info.server_key, {})[info.key] = info
        self.has_entries = True

    def _async_get_by_index(self, records: _ServiceIndex, key: _str) -> list[_ServiceInfo]:
        """Return all ServiceInfo matching the index."""
        record_infos = records.get(key)
        if record_infos is None:
            return []
        return list(record_infos.values())

    def _remove(self, infos: list[_ServiceInfo]) -> None:
        """Remove a services under the lock."""
        for info in infos:
            old_service_info = self._services.get(info.key)
            if old_service_info is None:
                continue
            assert old_service_info.server_key is not None
            type_key = old_service_info.type.lower()
            server_key = old_service_info.server_key
            type_bucket = self.types[type_key]
            del type_bucket[info.key]
            if not type_bucket:
                del self.types[type_key]
            server_bucket = self.servers[server_key]
            del server_bucket[info.key]
            if not server_bucket:
                del self.servers[server_key]
            del self._services[info.key]

        self.has_entries = bool(self._services)
