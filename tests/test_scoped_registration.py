"""Unit tests for scoped (per-interface) service registration."""

from __future__ import annotations

import asyncio
import socket
from typing import cast
from unittest.mock import Mock, patch

import pytest

from zeroconf import NonUniqueNameException, ScopedRegistration, Zeroconf, _core, const
from zeroconf._dns import DNSPointer
from zeroconf._protocol.outgoing import DNSOutgoing
from zeroconf._transport import _WrappedTransport
from zeroconf.asyncio import AsyncZeroconf

from . import _wait_for_start, make_service_info


def _make_sender(address: str) -> _WrappedTransport:
    """Build a fake sender bound to ``address`` with a Mock transport."""
    return _WrappedTransport(
        transport=cast("asyncio.DatagramTransport", Mock()),
        is_ipv6=False,
        sock=cast("socket.socket", Mock()),
        fileno=0,
        sock_name=(address, 5353),
    )


def _capture_sends(zc: Zeroconf, sends: list, on_send=None) -> None:
    """Patch zc.async_send to record (out, transport) pairs and run a hook."""
    real_async_send = zc.async_send

    def capture_send(out, addr=None, port=const._MDNS_PORT, v6_flow_scope=(), transport=None):
        sends.append((out, transport))
        if on_send is not None:
            on_send(out, transport)
        real_async_send(out, addr, port, v6_flow_scope, transport)

    patch.object(zc, "async_send", capture_send).start()


@pytest.mark.asyncio
async def test_scoped_registration_replays_probing_after_conflict(quick_timing: None) -> None:
    """A conflict on one scoped interface renames and replays probing on all of them."""
    aiozc = AsyncZeroconf(interfaces=["127.0.0.1"])
    zc = aiozc.zeroconf
    await zc.async_wait_for_start()
    sender_a = _make_sender("10.0.0.1")
    sender_b = _make_sender("10.0.0.2")
    sender_c = _make_sender("10.0.0.3")  # up, but not in the registration scope
    zc.engine.senders = [sender_a, sender_b, sender_c]

    type_ = "_scoped-conflict._tcp.local."
    registration_name = f"xxxyyy.{type_}"
    info = make_service_info(type_, registration_name)

    sends: list[tuple[DNSOutgoing, _WrappedTransport]] = []

    def inject_conflict(out: DNSOutgoing, transport: _WrappedTransport) -> None:
        # Interface B's segment legitimately conflicts with the first
        # candidate; interface A's segment stays silent.
        if (
            transport is sender_b
            and out.is_query()
            and out.authorities
            and out.authorities[0].alias == registration_name
        ):
            zc.cache.async_add_records(
                [DNSPointer(type_, const._TYPE_PTR, const._CLASS_IN, 120, registration_name)]
            )

    _capture_sends(zc, sends, inject_conflict)
    try:
        registration = await aiozc.async_register_service_scoped(
            info, ["10.0.0.1", "10.0.0.2"], allow_name_change=True
        )
    finally:
        patch.stopall()
        await aiozc.async_close()

    final_name = f"xxxyyy-2.{type_}"
    assert isinstance(registration, ScopedRegistration)
    assert registration.info is info
    assert info.name == final_name
    assert registration.interfaces == (("10.0.0.1", 0), ("10.0.0.2", 0))

    # Nothing was ever sent on the out-of-scope interface.
    assert all(transport is not sender_c for _, transport in sends)

    probes = [(out, transport) for out, transport in sends if out.is_query()]
    # The first candidate was probed on both in-scope interfaces before
    # the conflict arrived.
    first_round = [transport for out, transport in probes if out.authorities[0].alias == registration_name]
    assert first_round == [sender_a, sender_b]
    # The conflict replayed a full probing round of the next candidate on
    # both in-scope interfaces.
    replayed = [transport for out, transport in probes if out.authorities[0].alias == final_name]
    assert replayed == [sender_a, sender_b] * _core._REGISTER_BROADCASTS

    announcements = [(out, transport) for out, transport in sends if out.is_response()]
    assert {transport for _, transport in announcements} == {sender_a, sender_b}
    announced_names = {record.name for out, _ in announcements for record, _ in out.answers} | {
        record.alias
        for out, _ in announcements
        for record, _ in out.answers
        if isinstance(record, DNSPointer)
    }
    # Only the final name is announced; the rejected candidate that was
    # probed on the wire never appears in an announcement.
    assert final_name in announced_names
    assert registration_name not in announced_names


@pytest.mark.asyncio
async def test_scoped_registration_interface_leaving_reduces_coverage(quick_timing: None) -> None:
    """An interface leaving mid-probe drops out of the round's coverage."""
    aiozc = AsyncZeroconf(interfaces=["127.0.0.1"])
    zc = aiozc.zeroconf
    await zc.async_wait_for_start()
    sender_a = _make_sender("10.0.0.1")
    sender_b = _make_sender("10.0.0.2")
    zc.engine.senders = [sender_a, sender_b]

    type_ = "_scoped-leave._tcp.local."
    info = make_service_info(type_, f"xxxyyy.{type_}")

    sends: list[tuple[DNSOutgoing, _WrappedTransport]] = []

    def drop_b(out: DNSOutgoing, transport: _WrappedTransport) -> None:
        # Interface B leaves after its first probe went out.
        if transport is sender_b and out.is_query():
            zc.engine.senders = [sender_a]

    _capture_sends(zc, sends, drop_b)
    try:
        registration = await aiozc.async_register_service_scoped(info, ["10.0.0.1", "10.0.0.2"])
    finally:
        patch.stopall()
        await aiozc.async_close()

    assert registration.interfaces == (("10.0.0.1", 0),)
    # Only the first probe round reached B; everything after went to A alone.
    assert len([1 for out, transport in sends if out.is_query() and transport is sender_b]) == 1
    assert all(transport is sender_a for out, transport in sends if out.is_response())


@pytest.mark.asyncio
async def test_scoped_registration_requires_an_available_interface(quick_timing: None) -> None:
    """A scope matching no current sender raises ValueError."""
    aiozc = AsyncZeroconf(interfaces=["127.0.0.1"])
    zc = aiozc.zeroconf
    await zc.async_wait_for_start()
    zc.engine.senders = [_make_sender("10.0.0.1")]
    type_ = "_scoped-empty._tcp.local."
    info = make_service_info(type_, f"xxxyyy.{type_}")
    with pytest.raises(ValueError, match="None of the requested interfaces"):
        await aiozc.async_register_service_scoped(info, ["10.9.9.9"])
    await aiozc.async_close()


@pytest.mark.asyncio
async def test_scoped_registration_conflict_raises_without_allow_name_change(quick_timing: None) -> None:
    """A legitimate conflict on a scoped interface raises NonUniqueNameException."""
    aiozc = AsyncZeroconf(interfaces=["127.0.0.1"])
    zc = aiozc.zeroconf
    await zc.async_wait_for_start()
    sender_a = _make_sender("10.0.0.1")
    sender_b = _make_sender("10.0.0.2")
    zc.engine.senders = [sender_a, sender_b]

    type_ = "_scoped-strict._tcp.local."
    registration_name = f"xxxyyy.{type_}"
    info = make_service_info(type_, registration_name)

    sends: list[tuple[DNSOutgoing, _WrappedTransport]] = []

    def inject_conflict(out: DNSOutgoing, transport: _WrappedTransport) -> None:
        if transport is sender_b and out.is_query():
            zc.cache.async_add_records(
                [DNSPointer(type_, const._TYPE_PTR, const._CLASS_IN, 120, registration_name)]
            )

    _capture_sends(zc, sends, inject_conflict)
    try:
        with pytest.raises(NonUniqueNameException):
            await aiozc.async_register_service_scoped(info, ["10.0.0.1", "10.0.0.2"])
    finally:
        patch.stopall()
        await aiozc.async_close()

    assert zc.registry.async_get_info_name(registration_name) is None


def test_register_service_scoped_sync(quick_timing: None) -> None:
    """The sync scoped registration returns the ScopedRegistration."""
    zc = Zeroconf(interfaces=["127.0.0.1"])
    _wait_for_start(zc)
    zc.engine.senders = [_make_sender("10.0.0.1")]
    type_ = "_scoped-sync._tcp.local."
    info = make_service_info(type_, f"xxxyyy.{type_}")
    registration = zc.register_service_scoped(info, ["10.0.0.1"])
    assert isinstance(registration, ScopedRegistration)
    assert registration.info is info
    assert registration.interfaces == (("10.0.0.1", 0),)
    zc.close()
