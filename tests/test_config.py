from __future__ import annotations

import os
import pickle
import socket
import ssl
import sys
from types import SimpleNamespace
from unittest.mock import Mock, NonCallableMock, patch

import pytest
from _pytest.monkeypatch import MonkeyPatch

import hypercorn.config
from hypercorn.config import Config, Sockets

access_log_format = "bob"
h11_max_incomplete_size = 4


def _check_standard_config(config: Config) -> None:
    assert config.access_log_format == access_log_format
    assert config.h11_max_incomplete_size == h11_max_incomplete_size
    assert config.bind == ["127.0.0.1:5555"]


def test_config_from_pyfile() -> None:
    path = os.path.join(os.path.dirname(__file__), "assets/config.py")
    config = Config.from_pyfile(path)
    _check_standard_config(config)


def test_config_from_object() -> None:
    sys.path.append(os.path.join(os.path.dirname(__file__)))

    config = Config.from_object("assets.config")
    _check_standard_config(config)


def test_ssl_config_from_pyfile() -> None:
    path = os.path.join(os.path.dirname(__file__), "assets/config_ssl.py")
    config = Config.from_pyfile(path)
    _check_standard_config(config)
    assert config.ssl_enabled


def test_config_from_toml() -> None:
    path = os.path.join(os.path.dirname(__file__), "assets/config.toml")
    config = Config.from_toml(path)
    _check_standard_config(config)


def test_create_ssl_context() -> None:
    path = os.path.join(os.path.dirname(__file__), "assets/config_ssl.py")
    config = Config.from_pyfile(path)
    context = config.create_ssl_context()

    # NOTE: In earlier versions of python context.options is equal to <Options.OP_NO_COMPRESSION: 0>
    #       hence the ANDing context.options with the specified ssl options results in
    #       "<Options.OP_NO_COMPRESSION: 0>", which as a Boolean value, is False.
    #
    #       To overcome this, instead of checking that the result in True, we will check that it is
    #        equal to "context.options".
    assert (
        context.options
        & (
            ssl.OP_NO_SSLv2
            | ssl.OP_NO_SSLv3
            | ssl.OP_NO_TLSv1
            | ssl.OP_NO_TLSv1_1
            | ssl.OP_NO_COMPRESSION
        )
        == context.options
    )


@pytest.mark.parametrize(
    "bind, expected_family, expected_binding",
    [
        ("127.0.0.1:5000", socket.AF_INET, ("127.0.0.1", 5000)),
        ("127.0.0.1", socket.AF_INET, ("127.0.0.1", 8000)),
        ("[::]:5000", socket.AF_INET6, ("::", 5000)),
        ("[::]", socket.AF_INET6, ("::", 8000)),
    ],
)
def test_create_sockets_ip(
    bind: str,
    expected_family: socket.AddressFamily,
    expected_binding: tuple[str, int],
    monkeypatch: MonkeyPatch,
) -> None:
    mock_socket = Mock()
    monkeypatch.setattr(socket, "socket", mock_socket)
    config = Config()
    config.bind = [bind]
    sockets = config.create_sockets()
    sock = sockets.insecure_sockets[0]
    mock_socket.assert_called_with(expected_family, socket.SOCK_STREAM)
    sock.setsockopt.assert_called_with(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)  # type: ignore
    sock.bind.assert_called_with(expected_binding)  # type: ignore
    sock.setblocking.assert_called_with(False)  # type: ignore
    sock.set_inheritable.assert_called_with(True)  # type: ignore


def test_create_sockets_unix(monkeypatch: MonkeyPatch) -> None:
    mock_socket = Mock()
    monkeypatch.setattr(socket, "socket", mock_socket)
    monkeypatch.setattr(os, "chown", Mock())
    config = Config()
    config.bind = ["unix:/tmp/hypercorn.sock"]
    sockets = config.create_sockets()
    sock = sockets.insecure_sockets[0]
    mock_socket.assert_called_with(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.setsockopt.assert_called_with(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)  # type: ignore
    sock.bind.assert_called_with("/tmp/hypercorn.sock")  # type: ignore
    sock.setblocking.assert_called_with(False)  # type: ignore
    sock.set_inheritable.assert_called_with(True)  # type: ignore


def test_create_sockets_fd(monkeypatch: MonkeyPatch) -> None:
    mock_sock_class = Mock(
        return_value=NonCallableMock(
            **{"getsockopt.return_value": socket.SOCK_STREAM}  # type: ignore
        )
    )
    monkeypatch.setattr(socket, "socket", mock_sock_class)
    config = Config()
    config.bind = ["fd://2"]
    sockets = config.create_sockets()
    sock = sockets.insecure_sockets[0]
    mock_sock_class.assert_called_with(fileno=2)
    sock.getsockopt.assert_called_with(socket.SOL_SOCKET, socket.SO_TYPE)  # type: ignore
    sock.setsockopt.assert_called_with(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)  # type: ignore
    sock.setblocking.assert_called_with(False)  # type: ignore
    sock.set_inheritable.assert_called_with(True)  # type: ignore


def test_create_sockets_multiple(monkeypatch: MonkeyPatch) -> None:
    mock_socket = Mock()
    monkeypatch.setattr(socket, "socket", mock_socket)
    monkeypatch.setattr(os, "chown", Mock())
    config = Config()
    config.bind = ["127.0.0.1", "unix:/tmp/hypercorn.sock"]
    sockets = config.create_sockets()
    assert len(sockets.insecure_sockets) == 2


def test_response_headers(monkeypatch: MonkeyPatch) -> None:
    monkeypatch.setattr(hypercorn.config, "time", lambda: 1_512_229_395)
    config = Config()
    assert config.response_headers("test") == [
        (b"date", b"Sat, 02 Dec 2017 15:43:15 GMT"),
        (b"server", b"hypercorn-test"),
    ]
    config.include_server_header = False
    assert config.response_headers("test") == [(b"date", b"Sat, 02 Dec 2017 15:43:15 GMT")]


class FakeSocket:
    def __init__(self, address: tuple[str, int]) -> None:
        self.address = address
        self.closed = False

    def getsockname(self) -> tuple[str, int]:
        return self.address

    def fileno(self) -> int:
        return -1 if self.closed else 1

    def close(self) -> None:
        self.closed = True


def _alt_svc_headers(config: Config, server: tuple[str, int] | None = None) -> list[bytes]:
    with patch.dict(sys.modules, {"aioquic.h3.connection": SimpleNamespace(H3_ALPN=["h3"])}):
        return [
            value for name, value in config.response_headers("h2", server) if name == b"alt-svc"
        ]


def test_alt_svc_uses_actual_bound_ports() -> None:
    config = Config()
    entrypoints = [FakeSocket(("127.0.0.1", 43118)), FakeSocket(("127.0.0.2", 43119))]
    quic = [FakeSocket(("127.0.0.1", 43118)), FakeSocket(("127.0.0.2", 43119))]
    config._set_quic_addresses(quic, entrypoints)  # type: ignore[arg-type]

    assert _alt_svc_headers(config, ("127.0.0.1", 43118)) == [b'h3=":43118"; ma=3600']
    assert _alt_svc_headers(config, ("127.0.0.2", 43119)) == [b'h3=":43119"; ma=3600']


def test_alt_svc_closed_listener_is_withdrawn() -> None:
    config = Config()
    entrypoints = [FakeSocket(("127.0.0.1", 43118)), FakeSocket(("127.0.0.2", 43119))]
    quic = [FakeSocket(("127.0.0.1", 43118)), FakeSocket(("127.0.0.2", 43119))]
    config._set_quic_addresses(quic, entrypoints)  # type: ignore[arg-type]
    assert len(_alt_svc_headers(config)) == 2

    quic[1].close()
    assert _alt_svc_headers(config, ("127.0.0.1", 43118)) == [b'h3=":43118"; ma=3600']
    assert _alt_svc_headers(config, ("127.0.0.2", 43119)) == []

    entrypoints[0].close()
    assert _alt_svc_headers(config, ("127.0.0.1", 43118)) == []


def test_alt_svc_explicit_headers_are_unchanged() -> None:
    config = Config()
    config.alt_svc_headers = ['h3=":443"; ma=60']
    quic = [FakeSocket(("127.0.0.1", 43118))]
    config._set_quic_addresses(quic)  # type: ignore[arg-type]
    quic[0].close()

    assert _alt_svc_headers(config) == [b'h3=":443"; ma=60']


def test_alt_svc_unknown_entrypoint_advertises_all() -> None:
    config = Config()
    entrypoints = [FakeSocket(("127.0.0.1", 43118)), FakeSocket(("127.0.0.2", 43119))]
    quic = [FakeSocket(("127.0.0.1", 43118)), FakeSocket(("127.0.0.2", 43119))]
    config._set_quic_addresses(quic, entrypoints)  # type: ignore[arg-type]

    assert _alt_svc_headers(config) == [
        b'h3=":43118"; ma=3600',
        b'h3=":43119"; ma=3600',
    ]


def test_alt_svc_is_picklable_and_reattached() -> None:
    config = Config()
    config.application_path = "module:app"
    entrypoints = [FakeSocket(("127.0.0.1", 43118))]
    quic = [FakeSocket(("127.0.0.1", 43118))]
    config._set_quic_addresses(quic, entrypoints)  # type: ignore[arg-type]

    restored = pickle.loads(pickle.dumps(config))
    assert restored._alt_svc_alternatives[0].address == ("127.0.0.1", 43118)
    assert restored._alt_svc_alternatives[0].socket is None

    restored.attach_sockets(Sockets([], [], quic))  # type: ignore[arg-type]
    assert _alt_svc_headers(restored, ("127.0.0.1", 43118)) == [b'h3=":43118"; ma=3600']


def test_alt_svc_failed_listener_is_withdrawn() -> None:
    config = Config()
    entrypoints = [FakeSocket(("127.0.0.1", 43118)), FakeSocket(("127.0.0.2", 43119))]
    quic = [FakeSocket(("127.0.0.1", 43118)), FakeSocket(("127.0.0.2", 43119))]
    config._set_quic_addresses(quic, entrypoints)  # type: ignore[arg-type]

    config.mark_listener_failed(quic[1])
    assert _alt_svc_headers(config, ("127.0.0.1", 43118)) == [b'h3=":43118"; ma=3600']
    assert _alt_svc_headers(config, ("127.0.0.2", 43119)) == []


def test_alt_svc_no_quic_sockets() -> None:
    assert _alt_svc_headers(Config()) == []

