from __future__ import annotations

import asyncio
import ssl
from collections.abc import Callable
from pathlib import Path
from typing import TypeAlias

import httpx2
import pytest

from tests.utils import run_server
from uvicorn.config import Config
from uvicorn.server import Server

DefaultFactory: TypeAlias = Callable[[], ssl.SSLContext]


async def app(scope, receive, send):
    assert scope["type"] == "http"
    await send({"type": "http.response.start", "status": 204, "headers": []})
    await send({"type": "http.response.body", "body": b"", "more_body": False})


@pytest.mark.anyio
async def test_run(
    tls_ca_ssl_context,
    tls_certificate_server_cert_path,
    tls_certificate_private_key_path,
    tls_ca_certificate_pem_path,
    unused_tcp_port: int,
):
    config = Config(
        app=app,
        loop="asyncio",
        limit_max_requests=1,
        ssl_keyfile=tls_certificate_private_key_path,
        ssl_certfile=tls_certificate_server_cert_path,
        ssl_ca_certs=tls_ca_certificate_pem_path,
        port=unused_tcp_port,
    )
    async with run_server(config):
        async with httpx2.AsyncClient(verify=tls_ca_ssl_context) as client:
            response = await client.get(f"https://127.0.0.1:{unused_tcp_port}")
    assert response.status_code == 204


@pytest.mark.anyio
async def test_run_chain(
    tls_ca_ssl_context,
    tls_certificate_key_and_chain_path,
    tls_ca_certificate_pem_path,
    unused_tcp_port: int,
):
    config = Config(
        app=app,
        loop="asyncio",
        limit_max_requests=1,
        ssl_certfile=tls_certificate_key_and_chain_path,
        ssl_ca_certs=tls_ca_certificate_pem_path,
        port=unused_tcp_port,
    )
    async with run_server(config):
        async with httpx2.AsyncClient(verify=tls_ca_ssl_context) as client:
            response = await client.get(f"https://127.0.0.1:{unused_tcp_port}")
    assert response.status_code == 204


@pytest.mark.anyio
async def test_run_chain_only(tls_ca_ssl_context, tls_certificate_key_and_chain_path, unused_tcp_port: int):
    config = Config(
        app=app,
        loop="asyncio",
        limit_max_requests=1,
        ssl_certfile=tls_certificate_key_and_chain_path,
        port=unused_tcp_port,
    )
    async with run_server(config):
        async with httpx2.AsyncClient(verify=tls_ca_ssl_context) as client:
            response = await client.get(f"https://127.0.0.1:{unused_tcp_port}")
    assert response.status_code == 204


@pytest.mark.anyio
async def test_run_password(
    tls_ca_ssl_context,
    tls_certificate_server_cert_path,
    tls_ca_certificate_pem_path,
    tls_certificate_private_key_encrypted_path,
    unused_tcp_port: int,
):
    config = Config(
        app=app,
        loop="asyncio",
        limit_max_requests=1,
        ssl_keyfile=tls_certificate_private_key_encrypted_path,
        ssl_certfile=tls_certificate_server_cert_path,
        ssl_keyfile_password="uvicorn password for the win",
        ssl_ca_certs=tls_ca_certificate_pem_path,
        port=unused_tcp_port,
    )
    async with run_server(config):
        async with httpx2.AsyncClient(verify=tls_ca_ssl_context) as client:
            response = await client.get(f"https://127.0.0.1:{unused_tcp_port}")
    assert response.status_code == 204


@pytest.mark.anyio
async def test_run_ssl_context_factory_default(
    tls_ca_ssl_context: ssl.SSLContext,
    tls_certificate_server_cert_path: str,
    tls_certificate_private_key_path: str,
    unused_tcp_port: int,
) -> None:
    """A factory that just delegates to the default factory should produce a working server."""

    def ssl_context_factory(config: Config, default_ssl_context_factory: DefaultFactory) -> ssl.SSLContext:
        return default_ssl_context_factory()

    config = Config(
        app=app,
        loop="asyncio",
        limit_max_requests=1,
        ssl_keyfile=tls_certificate_private_key_path,
        ssl_certfile=tls_certificate_server_cert_path,
        ssl_context_factory=ssl_context_factory,
        port=unused_tcp_port,
    )
    async with run_server(config):
        async with httpx2.AsyncClient(verify=tls_ca_ssl_context) as client:
            response = await client.get(f"https://127.0.0.1:{unused_tcp_port}")
    assert response.status_code == 204


@pytest.mark.anyio
async def test_run_ssl_context_factory_custom(
    tls_ca_ssl_context: ssl.SSLContext,
    tls_certificate_server_cert_path: str,
    tls_certificate_private_key_path: str,
    unused_tcp_port: int,
) -> None:
    """A factory that builds its own SSLContext from scratch should work without ssl_keyfile/ssl_certfile."""

    def ssl_context_factory(config: Config, default_ssl_context_factory: DefaultFactory) -> ssl.SSLContext:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(tls_certificate_server_cert_path, tls_certificate_private_key_path)
        return ctx

    config = Config(
        app=app,
        loop="asyncio",
        limit_max_requests=1,
        ssl_context_factory=ssl_context_factory,
        port=unused_tcp_port,
    )
    async with run_server(config):
        async with httpx2.AsyncClient(verify=tls_ca_ssl_context) as client:
            response = await client.get(f"https://127.0.0.1:{unused_tcp_port}")
    assert response.status_code == 204


def test_ssl_context_factory_mutates_default(
    tls_certificate_server_cert_path: str,
    tls_certificate_private_key_path: str,
) -> None:
    """The factory can call the default and mutate the result (e.g., bump TLS minimum version)."""

    def ssl_context_factory(config: Config, default_ssl_context_factory: DefaultFactory) -> ssl.SSLContext:
        ctx = default_ssl_context_factory()
        ctx.minimum_version = ssl.TLSVersion.TLSv1_3
        return ctx

    config = Config(
        app=app,
        ssl_keyfile=tls_certificate_private_key_path,
        ssl_certfile=tls_certificate_server_cert_path,
        ssl_context_factory=ssl_context_factory,
    )
    config.load()
    assert config.is_ssl
    assert isinstance(config.ssl, ssl.SSLContext)
    assert config.ssl.minimum_version == ssl.TLSVersion.TLSv1_3


def test_default_ssl_context_factory_requires_ssl_certfile() -> None:
    """Calling `default_ssl_context_factory()` without `ssl_certfile` raises a clear error."""

    def ssl_context_factory(config: Config, default_ssl_context_factory: DefaultFactory) -> ssl.SSLContext:
        return default_ssl_context_factory()

    config = Config(app=app, ssl_context_factory=ssl_context_factory)
    with pytest.raises(RuntimeError, match="requires `ssl_certfile`"):
        config.load()


def test_ssl_context_factory_must_return_ssl_context() -> None:
    def bad_factory(config: Config, default_ssl_context_factory: DefaultFactory) -> object:
        return "not an SSLContext"

    config = Config(app=app, ssl_context_factory=bad_factory)  # type: ignore[arg-type]
    with pytest.raises(TypeError, match="must return an `ssl.SSLContext`"):
        config.load()


def test_ssl_ciphers_applied_when_set(
    tls_certificate_server_cert_path: str,
    tls_certificate_private_key_path: str,
) -> None:
    config = Config(
        app=app,
        ssl_keyfile=tls_certificate_private_key_path,
        ssl_certfile=tls_certificate_server_cert_path,
        ssl_ciphers="HIGH",
    )
    config.load()
    assert isinstance(config.ssl, ssl.SSLContext)


def test_is_ssl_true_when_only_factory_set() -> None:
    def ssl_context_factory(config: Config, default_ssl_context_factory: DefaultFactory) -> ssl.SSLContext:
        return ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)  # pragma: no cover

    config = Config(app=app, ssl_context_factory=ssl_context_factory)
    assert config.is_ssl is True


def test_reload_ssl_context_without_ssl() -> None:
    config = Config(app=app)
    server = Server(config)
    with pytest.raises(RuntimeError, match="SSL is not configured"):
        server.reload_ssl_context()


def test_reload_ssl_context_without_certfile() -> None:
    def ssl_context_factory(config: Config, default_ssl_context_factory: DefaultFactory) -> ssl.SSLContext:
        return ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)

    config = Config(app=app, ssl_context_factory=ssl_context_factory)
    config.load()
    server = Server(config)
    with pytest.raises(RuntimeError, match="`ssl_certfile`"):
        server.reload_ssl_context()


@pytest.mark.anyio
async def test_ssl_certificate_reload(
    tmp_path: Path,
    tls_certificate_authority,  # noqa: ANN001
    unused_tcp_port: int,
) -> None:
    """Rotate the TLS certificate without restarting: one successful reload and one failing reload."""
    ca = tls_certificate_authority
    cert_a = ca.issue_cert("localhost", "127.0.0.1", "::1")
    cert_b = ca.issue_cert("localhost", "127.0.0.1", "::1")

    cert_a_path = tmp_path / "cert-a.pem"
    key_a_path = tmp_path / "key-a.pem"
    cert_b_path = tmp_path / "cert-b.pem"
    key_b_path = tmp_path / "key-b.pem"
    ca_path = tmp_path / "ca.pem"
    cert_a_path.write_bytes(cert_a.cert_chain_pems[0].bytes())
    key_a_path.write_bytes(cert_a.private_key_pem.bytes())
    cert_b_path.write_bytes(cert_b.cert_chain_pems[0].bytes())
    key_b_path.write_bytes(cert_b.private_key_pem.bytes())
    ca_path.write_bytes(ca.cert_pem.bytes())

    cert_a_der = ssl.PEM_cert_to_DER_cert(cert_a.cert_chain_pems[0].bytes().decode())
    cert_b_der = ssl.PEM_cert_to_DER_cert(cert_b.cert_chain_pems[0].bytes().decode())
    assert cert_a_der != cert_b_der

    def ssl_context_factory(config: Config, default_ssl_context_factory: DefaultFactory) -> ssl.SSLContext:
        ctx = default_ssl_context_factory()
        ctx.set_alpn_protocols(["http/1.1"])
        return ctx

    config = Config(
        app=app,
        loop="asyncio",
        port=unused_tcp_port,
        ssl_keyfile=str(key_a_path),
        ssl_certfile=str(cert_a_path),
        ssl_cert_reqs=ssl.CERT_OPTIONAL,
        ssl_ca_certs=str(ca_path),
        ssl_context_factory=ssl_context_factory,
    )

    client_ctx = ssl.create_default_context(ssl.Purpose.SERVER_AUTH)
    ca.configure_trust(client_ctx)
    client_ctx.set_alpn_protocols(["http/1.1"])

    async def open_connection() -> tuple[asyncio.StreamReader, asyncio.StreamWriter]:
        return await asyncio.open_connection("127.0.0.1", unused_tcp_port, ssl=client_ctx)

    def peer_cert_der(writer: asyncio.StreamWriter) -> bytes:
        ssl_object = writer.get_extra_info("ssl_object")
        return ssl_object.getpeercert(binary_form=True)

    def selected_alpn_protocol(writer: asyncio.StreamWriter) -> str:
        return writer.get_extra_info("ssl_object").selected_alpn_protocol()

    async def roundtrip(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        writer.write(b"GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
        await writer.drain()
        response = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), timeout=5)
        assert response.startswith(b"HTTP/1.1 204")

    async with run_server(config) as server:
        live_context = config.ssl
        assert live_context is not None
        assert live_context.verify_mode == ssl.CERT_OPTIONAL

        # Existing connection, established with certificate A.
        reader1, writer1 = await open_connection()
        assert peer_cert_der(writer1) == cert_a_der
        assert selected_alpn_protocol(writer1) == "http/1.1"
        await roundtrip(reader1, writer1)

        # Successful rotation to certificate B.
        config.ssl_certfile = str(cert_b_path)
        config.ssl_keyfile = str(key_b_path)
        server.reload_ssl_context()
        # The live context is updated in place: ALPN and client-certificate
        # verification settings are preserved.
        assert config.ssl is live_context
        assert config.ssl.verify_mode == ssl.CERT_OPTIONAL

        # The existing connection still works and keeps certificate A.
        await roundtrip(reader1, writer1)
        assert peer_cert_der(writer1) == cert_a_der

        # New connections get certificate B, and ALPN is still negotiated.
        reader2, writer2 = await open_connection()
        assert peer_cert_der(writer2) == cert_b_der
        assert selected_alpn_protocol(writer2) == "http/1.1"
        await roundtrip(reader2, writer2)

        # Failing rotation: certificate B combined with the mismatched key A.
        config.ssl_certfile = str(cert_b_path)
        config.ssl_keyfile = str(key_a_path)
        with pytest.raises(ssl.SSLError):
            server.reload_ssl_context()

        # The previous listener keeps serving certificate B, and existing
        # connections are unaffected.
        await roundtrip(reader1, writer1)
        await roundtrip(reader2, writer2)
        reader3, writer3 = await open_connection()
        assert peer_cert_der(writer3) == cert_b_der
        await roundtrip(reader3, writer3)

        for writer in (writer1, writer2, writer3):
            writer.close()
