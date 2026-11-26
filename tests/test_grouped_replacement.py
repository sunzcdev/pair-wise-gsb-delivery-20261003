from __future__ import annotations

import asyncio
import socket
from unittest.mock import AsyncMock, patch

import pytest

from zeroconf import (
    DNSOutgoing,
    DNSPointer,
    InterfaceUpdateRolledBack,
    ServiceInfo,
    ServiceListener,
    Zeroconf,
    _engine,
    const,
)
from zeroconf._engine import _WrappedTransport
from zeroconf._listener import AsyncListener
from zeroconf.asyncio import AsyncServiceBrowser, AsyncZeroconf


def _bind_endpoint(address: str) -> socket.socket:
    endpoint = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    endpoint.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    endpoint.bind((address, 0))
    return endpoint


def _ptr_response(name: str) -> bytes:
    out = DNSOutgoing(const._FLAGS_QR_RESPONSE)
    out.add_answer_at_time(
        DNSPointer(
            "_http._tcp.local.",
            const._TYPE_PTR,
            const._CLASS_IN,
            120,
            name,
        ),
        0,
    )
    return out.packets()[0]


def _protocol_for(engine: _engine.AsyncEngine, sender: _WrappedTransport) -> AsyncListener:
    return next(
        protocol
        for protocol in engine.protocols
        if protocol.transport is not None and protocol.transport.transport is sender.transport
    )


@pytest.mark.asyncio
@pytest.mark.usefixtures("quick_timing")
async def test_grouped_replace_rolls_back_and_keeps_unchanged_nic(
    aiozc: AsyncZeroconf,
) -> None:
    zc = aiozc.zeroconf
    await zc.async_wait_for_start()
    engine = zc.engine

    await engine._async_wrap_socket(_bind_endpoint("127.0.0.9"), is_sender=True)
    unchanged = next(w for w in engine.senders if w.interface_key == ("127.0.0.1", 0))
    unchanged_protocol = _protocol_for(engine, unchanged)
    old_nic = next(w for w in engine.senders if w.interface_key == ("127.0.0.9", 0))

    failing: set[str] = {"127.0.0.3"}
    joined: set[str] = set()
    stands: dict[str, socket.socket] = {}

    def fake_add(
        listen_socket: socket.socket | None,
        interface: str,
        apple_p2p: bool = False,
        unicast: bool = False,
    ) -> socket.socket | None:
        if interface in failing:
            return None
        endpoint = _bind_endpoint(interface)
        stands[interface] = endpoint
        joined.add(interface)
        return endpoint

    def fake_drop(listen_socket: socket.socket | None, interface: str) -> None:
        joined.discard(interface)

    events: list[tuple[str, str]] = []

    class Listener(ServiceListener):
        def add_service(self, zc: Zeroconf, type_: str, name: str) -> None:
            events.append((type_, name))

        def remove_service(self, zc: Zeroconf, type_: str, name: str) -> None:
            pass

        def update_service(self, zc: Zeroconf, type_: str, name: str) -> None:
            pass

    browser = AsyncServiceBrowser(zc, "_http._tcp.local.", Listener())
    info = ServiceInfo(
        "_test._tcp.local.",
        "Replaced._test._tcp.local.",
        addresses=[b"\x7f\x00\x00\x01"],
        port=80,
        server="replaced.local.",
    )
    zc.registry.async_add(info)

    try:
        with (
            patch.object(_engine, "add_interface", side_effect=fake_add),
            patch.object(_engine, "drop_multicast_member", side_effect=fake_drop),
            patch.object(zc, "_async_broadcast_service", new_callable=AsyncMock) as announce,
        ):
            with pytest.raises(InterfaceUpdateRolledBack):
                await aiozc.async_update_interfaces(["127.0.0.1", "127.0.0.2", "127.0.0.3"], replace=True)
            await asyncio.sleep(0)

            keys = {w.interface_key for w in engine.senders}
            assert keys == {("127.0.0.1", 0), ("127.0.0.9", 0)}
            assert stands["127.0.0.2"].fileno() == -1
            assert joined == set()
            assert next(w for w in engine.senders if w.interface_key == ("127.0.0.1", 0)) is unchanged
            assert not unchanged.transport.is_closing()
            assert not old_nic.transport.is_closing()
            assert browser in zc.record_manager.listeners
            announce.assert_not_called()
            assert zc._interfaces == ["127.0.0.1"]

            unchanged_protocol.datagram_received(
                _ptr_response("Before._http._tcp.local."),
                ("127.0.0.1", const._MDNS_PORT),
            )
            await asyncio.sleep(0)
            assert events == [("_http._tcp.local.", "Before._http._tcp.local.")]

            failing.clear()
            await aiozc.async_update_interfaces(["127.0.0.1", "127.0.0.2", "127.0.0.3"], replace=True)
            await asyncio.sleep(0)

            keys = {w.interface_key for w in engine.senders}
            assert keys == {
                ("127.0.0.1", 0),
                ("127.0.0.2", 0),
                ("127.0.0.3", 0),
            }
            assert old_nic.transport.is_closing()
            assert zc._interfaces == ["127.0.0.1", "127.0.0.2", "127.0.0.3"]
            announce.assert_awaited_once()

            assert next(w for w in engine.senders if w.interface_key == ("127.0.0.1", 0)) is unchanged
            assert _protocol_for(engine, unchanged) is unchanged_protocol

            unchanged_protocol.datagram_received(
                _ptr_response("After._http._tcp.local."),
                ("127.0.0.1", const._MDNS_PORT),
            )
            await asyncio.sleep(0)
            assert events == [
                ("_http._tcp.local.", "Before._http._tcp.local."),
                ("_http._tcp.local.", "After._http._tcp.local."),
            ]
    finally:
        await browser.async_cancel()


@pytest.mark.asyncio
async def test_atomic_alias_is_supported(aiozc: AsyncZeroconf) -> None:
    await aiozc.async_update_interfaces(["127.0.0.1"], atomic=True)


def test_sync_grouped_replace_noop(zc: Zeroconf) -> None:
    zc.update_interfaces(["127.0.0.1"], replace=True)
