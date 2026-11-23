"""Unit tests for zeroconf._services.registry."""

from __future__ import annotations

import socket
import unittest

import zeroconf as r
from zeroconf import ServiceInfo

from .. import make_service_info


class TestServiceRegistry(unittest.TestCase):
    def test_only_register_once(self):
        type_ = "_test-srvc-type._tcp.local."
        name = "xxxyyy"
        registration_name = f"{name}.{type_}"

        desc = {"path": "/healthz/"}
        info = make_service_info(type_, registration_name, properties=desc)

        registry = r.ServiceRegistry()
        registry.async_add(info)
        self.assertRaises(r.ServiceNameAlreadyRegistered, registry.async_add, info)
        registry.async_remove(info)
        registry.async_add(info)

    def test_register_same_server(self):
        type_ = "_test-srvc-type._tcp.local."
        name = "xxxyyy"
        name2 = "xxxyyy2"
        registration_name = f"{name}.{type_}"
        registration_name2 = f"{name2}.{type_}"

        desc = {"path": "/healthz/"}
        info = make_service_info(type_, registration_name, properties=desc, server="same.local.")
        info2 = make_service_info(type_, registration_name2, properties=desc, server="same.local.")
        registry = r.ServiceRegistry()
        registry.async_add(info)
        registry.async_add(info2)
        assert registry.async_get_infos_server("same.local.") == [info, info2]
        registry.async_remove(info)
        assert registry.async_get_infos_server("same.local.") == [info2]
        registry.async_remove(info2)
        assert registry.async_get_infos_server("same.local.") == []

    def test_unregister_multiple_times(self):
        """Verify we can unregister a service multiple times.

        In production unregister_service and unregister_all_services
        may happen at the same time during shutdown. We want to treat
        this as non-fatal since its expected to happen and it is unlikely
        that the callers know about each other.
        """
        type_ = "_test-srvc-type._tcp.local."
        name = "xxxyyy"
        registration_name = f"{name}.{type_}"

        desc = {"path": "/healthz/"}
        info = make_service_info(type_, registration_name, properties=desc)

        registry = r.ServiceRegistry()
        registry.async_add(info)
        self.assertRaises(r.ServiceNameAlreadyRegistered, registry.async_add, info)
        registry.async_remove(info)
        registry.async_remove(info)

    def test_lookups(self):
        type_ = "_test-srvc-type._tcp.local."
        name = "xxxyyy"
        registration_name = f"{name}.{type_}"

        desc = {"path": "/healthz/"}
        info = make_service_info(type_, registration_name, properties=desc)

        registry = r.ServiceRegistry()
        registry.async_add(info)

        assert registry.async_get_service_infos() == [info]
        assert registry.async_get_info_name(registration_name) == info
        assert registry.async_get_infos_type(type_) == [info]
        assert registry.async_get_infos_server("spare-rig.local.") == [info]
        assert registry.async_get_types() == [type_]

    def test_lookups_upper_case_by_lower_case(self):
        type_ = "_test-SRVC-type._tcp.local."
        name = "Xxxyyy"
        registration_name = f"{name}.{type_}"

        desc = {"path": "/healthz/"}
        info = make_service_info(type_, registration_name, properties=desc, server="SPARE-RIG.local.")

        registry = r.ServiceRegistry()
        registry.async_add(info)

        assert registry.async_get_service_infos() == [info]
        assert registry.async_get_info_name(registration_name.lower()) == info
        assert registry.async_get_infos_type(type_.lower()) == [info]
        assert registry.async_get_infos_server("spare-rig.local.") == [info]
        assert registry.async_get_types() == [type_.lower()]

    def test_empty_buckets_are_removed_when_last_entry_is_removed(self):
        type_ = "_test-srvc-type._tcp.local."
        registration_name = f"xxxyyy.{type_}"
        desc = {"path": "/healthz/"}
        info = make_service_info(type_, registration_name, properties=desc)

        registry = r.ServiceRegistry()
        registry.async_add(info)
        registry.async_remove(info)

        assert type_.lower() not in registry.types
        assert "spare-rig.local." not in registry.servers
        assert registry.async_get_types() == []

    def test_bulk_remove_preserves_order_of_survivors(self):
        type_ = "_test-srvc-type._tcp.local."
        server = "shared.local."
        desc = {"path": "/healthz/"}
        infos = [
            make_service_info(type_, f"svc{i}.{type_}", properties=desc, server=server) for i in range(20)
        ]

        registry = r.ServiceRegistry()
        for info in infos:
            registry.async_add(info)

        # Remove every other entry in one bulk call.
        to_remove = [infos[i] for i in range(0, 20, 2)]
        registry.async_remove(to_remove)

        expected = [infos[i] for i in range(1, 20, 2)]
        assert registry.async_get_infos_type(type_) == expected
        assert registry.async_get_infos_server(server) == expected

    def test_bulk_remove_then_readd_under_same_key(self):
        """Re-adding after the bucket was deleted must rebuild it cleanly."""
        type_ = "_test-srvc-type._tcp.local."
        server = "spare-rig.local."
        desc = {"path": "/healthz/"}
        info = make_service_info(type_, f"only.{type_}", properties=desc, server=server)

        registry = r.ServiceRegistry()
        registry.async_add(info)
        registry.async_remove(info)
        assert type_.lower() not in registry.types
        registry.async_add(info)
        assert registry.async_get_infos_type(type_) == [info]
        assert registry.async_get_infos_server(server) == [info]

    def test_update_replaces_indexed_info(self):
        type_ = "_test-srvc-type._tcp.local."
        server = "spare-rig.local."
        registration_name = f"xxxyyy.{type_}"
        info = make_service_info(type_, registration_name, properties={"path": "/healthz/"}, server=server)
        updated = ServiceInfo(
            type_,
            registration_name,
            81,
            0,
            0,
            {"path": "/healthz/"},
            server,
            addresses=[socket.inet_aton("10.7.4.3")],
        )

        registry = r.ServiceRegistry()
        registry.async_add(info)
        registry.async_update(updated)

        assert registry.async_get_infos_type(type_)[0] is updated
        assert registry.async_get_infos_server(server)[0] is updated

    def test_per_interface_server_references(self):
        type_ = "_test-srvc-type._tcp.local."
        server = "shared.local."
        first_iface = ("127.0.0.1", 0)
        second_iface = ("10.0.0.2", 0)
        info = make_service_info(type_, f"one.{type_}", server=server)
        info2 = make_service_info(type_, f"two.{type_}", server=server)

        registry = r.ServiceRegistry()
        registry.async_add(info)
        registry.async_add(info2)
        for interface_key in (first_iface, second_iface):
            registry.async_increment_server_reference(info, interface_key)
            registry.async_increment_server_reference(info2, interface_key)

        # Removing one reference keeps both interfaces live.
        registry.async_decrement_server_reference(info, first_iface)
        assert registry.async_get_server_reference_interfaces(server) == {first_iface, second_iface}
        assert not registry.async_is_server_reference_drained(info, first_iface)

        # The first interface loses its remaining reference with info2.
        registry.async_decrement_server_reference(info2, first_iface)
        assert first_iface not in registry.async_get_server_reference_interfaces(server)
        assert registry.async_is_server_reference_drained(info2, first_iface)
        # Further decrements on a drained interface stay no-ops.
        registry.async_decrement_server_reference(info, first_iface)
        assert registry.async_is_server_reference_drained(info, first_iface)

        # Re-adding a reference revives the drained interface.
        registry.async_increment_server_reference(info2, first_iface)
        assert not registry.async_is_server_reference_drained(info2, first_iface)
        assert first_iface in registry.async_get_server_reference_interfaces(server)

    def test_decrement_seeds_unseeded_interface_from_registry(self):
        type_ = "_test-srvc-type._tcp.local."
        server = "implicit.local."
        interface_key = ("127.0.0.1", 0)
        info = make_service_info(type_, f"one.{type_}", server=server)
        info2 = make_service_info(type_, f"two.{type_}", server=server)

        registry = r.ServiceRegistry()
        registry.async_add(info)
        registry.async_add(info2)

        # An interface with no explicit bookkeeping implicitly references
        # every registered instance, so one removal leaves one reference.
        registry.async_decrement_server_reference(info, interface_key)
        assert registry.async_get_server_reference_interfaces(server) == {interface_key}
        registry.async_decrement_server_reference(info2, interface_key)
        assert registry.async_get_server_reference_interfaces(server) == set()
        assert registry.async_is_server_reference_drained(info2, interface_key)
