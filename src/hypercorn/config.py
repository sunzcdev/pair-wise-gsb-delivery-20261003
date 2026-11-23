from __future__ import annotations

import importlib
import importlib.util
import logging
import os
import socket
import stat
import sys
import types
import warnings
from collections.abc import Mapping
from dataclasses import dataclass, field
from ssl import (
    create_default_context,
    OP_NO_COMPRESSION,
    Purpose,
    SSLContext,
    TLSVersion,
    VerifyFlags,
    VerifyMode,
)
from time import time
from typing import Any
from wsgiref.handlers import format_date_time

if sys.version_info >= (3, 11):
    import tomllib
else:
    import tomli as tomllib

from .logging import Logger

BYTES = 1
OCTETS = 1
SECONDS = 1.0

FilePath = bytes | os.PathLike | str
SocketKind = int | socket.SocketKind


@dataclass
class Sockets:
    secure_sockets: list[socket.socket]
    insecure_sockets: list[socket.socket]
    quic_sockets: list[socket.socket]


@dataclass
class AltSvcAlternative:
    """An alternative service (a QUIC listener) and the entrypoint it serves.

    The entries are created from the actual bound sockets, so system
    assigned ports (``:0``) are advertised with the value the system
    chose. An alternative is only advertised whilst both its own socket
    and the entrypoint socket it is paired with are still listening.
    """

    address: tuple[str, int]
    entrypoint: tuple[str, int] | None = None
    socket: Any = field(default=None, repr=False, compare=False)
    entrypoint_socket: Any = field(default=None, repr=False, compare=False)
    closed: bool = field(default=False, compare=False)

    @property
    def valid(self) -> bool:
        return (
            not self.closed
            and _socket_listening(self.socket)
            and _socket_listening(self.entrypoint_socket)
        )

    def serves(self, server: tuple[str, int] | None) -> bool:
        """Whether this alternative can be reached from ``server``.

        ``server`` is the address the response is sent from. A paired
        alternative is only advertised on its own entrypoint, whilst an
        unpaired one (no entrypoint mapping) is advertised on any
        entrypoint that shares its host.
        """
        if server is None:
            return True
        if self.entrypoint is not None:
            return _addresses_compatible(self.entrypoint, server)
        return _hosts_compatible(self.address[0], server[0])


def _getsockname(sock: Any) -> Any:
    getsockname = getattr(sock, "getsockname", None)
    if getsockname is None:
        return None
    try:
        return getsockname()
    except OSError:
        return None


def _socket_address(name: Any) -> tuple[str, int] | None:
    if isinstance(name, (tuple, list)) and len(name) >= 2:
        host, port = name[0], name[1]
        if isinstance(host, str) and isinstance(port, int):
            return (host, port)
    return None


def _socket_listening(sock: Any) -> bool:
    if sock is None:
        return True
    try:
        fileno = sock.fileno()
    except (AttributeError, OSError):
        return True
    if not isinstance(fileno, int):
        # e.g. a test double, the close watcher is the only signal.
        return True
    return fileno != -1


def _hosts_compatible(left: str, right: str) -> bool:
    wildcards = {"", "0.0.0.0", "::"}
    return left in wildcards or right in wildcards or left == right


def _addresses_compatible(left: tuple[str, int], right: tuple[str, int]) -> bool:
    return left[1] == right[1] and _hosts_compatible(left[0], right[0])


def _socket_of(entrypoint: Any) -> Any:
    if entrypoint is None:
        return None
    if hasattr(entrypoint, "getsockname"):
        return entrypoint
    return getattr(entrypoint, "socket", None)


class SocketTypeError(Exception):
    def __init__(self, expected: SocketKind, actual: SocketKind) -> None:
        super().__init__(
            f'Unexpected socket type, wanted "{socket.SocketKind(expected)}" got '
            f'"{socket.SocketKind(actual)}"'
        )


class Config:
    _bind = ["127.0.0.1:8000"]
    _insecure_bind: list[str] = []
    _quic_bind: list[str] = []
    _quic_addresses: list[tuple] = []
    _alt_svc_alternatives: list[AltSvcAlternative] = []
    _log: Logger | None = None
    _root_path: str = ""

    access_log_format = '%(h)s %(l)s %(l)s %(t)s "%(r)s" %(s)s %(b)s "%(f)s" "%(a)s"'
    accesslog: logging.Logger | str | None = None
    alpn_protocols = ["h2", "http/1.1"]
    alt_svc_headers: list[str] = []
    application_path: str
    backlog = 100
    ca_certs: str | None = None
    certfile: str | None = None
    ciphers: str = "ECDHE+AESGCM"
    daemon = True
    debug = False
    dogstatsd_tags = ""
    errorlog: logging.Logger | str | None = "-"
    graceful_timeout: float = 3 * SECONDS
    read_timeout: int | None = None
    group: int | None = None
    h11_max_incomplete_size = 16 * 1024 * BYTES
    h11_pass_raw_headers = False
    h2_max_concurrent_streams = 100
    h2_max_header_list_size = 2**16
    h2_max_inbound_frame_size = 2**14 * OCTETS
    include_date_header = True
    include_server_header = True
    keep_alive_timeout = 5 * SECONDS
    keep_alive_max_requests = 1000
    keyfile: str | None = None
    keyfile_password: str | None = None
    logconfig: str | None = None
    logconfig_dict: dict | None = None
    logger_class = Logger
    loglevel: str = "INFO"
    max_app_queue_size: int = 10
    max_requests: int | None = None
    max_requests_jitter: int = 0
    pid_path: str | None = None
    server_names: list[str] = []
    shutdown_timeout = 60 * SECONDS
    ssl_handshake_timeout = 60 * SECONDS
    startup_timeout = 60 * SECONDS
    statsd_host: str | None = None
    statsd_prefix = ""
    umask: int | None = None
    use_reloader = False
    user: int | None = None
    verify_flags: VerifyFlags | None = None
    verify_mode: VerifyMode | None = None
    websocket_max_message_size = 16 * 1024 * 1024 * BYTES
    websocket_ping_interval: float | None = None
    worker_class = "asyncio"
    workers = 1
    wsgi_max_body_size = 16 * 1024 * 1024 * BYTES

    def set_cert_reqs(self, value: int) -> None:
        warnings.warn("Please use verify_mode instead", Warning)
        self.verify_mode = VerifyMode(value)

    cert_reqs = property(None, set_cert_reqs)

    @property
    def log(self) -> Logger:
        if self._log is None:
            self._log = self.logger_class(self)
        return self._log

    @property
    def bind(self) -> list[str]:
        return self._bind

    @bind.setter
    def bind(self, value: list[str] | str) -> None:
        if isinstance(value, str):
            self._bind = [value]
        else:
            self._bind = value

    @property
    def insecure_bind(self) -> list[str]:
        return self._insecure_bind

    @insecure_bind.setter
    def insecure_bind(self, value: list[str] | str) -> None:
        if isinstance(value, str):
            self._insecure_bind = [value]
        else:
            self._insecure_bind = value

    @property
    def quic_bind(self) -> list[str]:
        return self._quic_bind

    @quic_bind.setter
    def quic_bind(self, value: list[str] | str) -> None:
        if isinstance(value, str):
            self._quic_bind = [value]
        else:
            self._quic_bind = value

    @property
    def root_path(self) -> str:
        return self._root_path

    @root_path.setter
    def root_path(self, value: str) -> None:
        self._root_path = value.rstrip("/")

    def create_ssl_context(self) -> SSLContext | None:
        if not self.ssl_enabled:
            return None

        context = create_default_context(Purpose.CLIENT_AUTH)
        context.set_ciphers(self.ciphers)
        context.minimum_version = TLSVersion.TLSv1_2  # RFC 7540 Section 9.2: MUST be TLS >=1.2
        context.options = OP_NO_COMPRESSION  # RFC 7540 Section 9.2.1: MUST disable compression
        context.set_alpn_protocols(self.alpn_protocols)

        if self.certfile is not None and self.keyfile is not None:
            context.load_cert_chain(
                certfile=self.certfile,
                keyfile=self.keyfile,
                password=self.keyfile_password,
            )

        if self.ca_certs is not None:
            context.load_verify_locations(self.ca_certs)
        if self.verify_mode is not None:
            context.verify_mode = self.verify_mode
        if self.verify_flags is not None:
            context.verify_flags = self.verify_flags

        return context

    @property
    def ssl_enabled(self) -> bool:
        return self.certfile is not None and self.keyfile is not None

    def create_sockets(self) -> Sockets:
        if self.ssl_enabled:
            secure_sockets = self._create_sockets(self.bind)
            insecure_sockets = self._create_sockets(self.insecure_bind)
            quic_sockets = self._create_sockets(self.quic_bind, socket.SOCK_DGRAM)
            self._set_quic_addresses(quic_sockets, [*secure_sockets, *insecure_sockets])
        else:
            secure_sockets = []
            insecure_sockets = self._create_sockets(self.bind)
            quic_sockets = []
        return Sockets(secure_sockets, insecure_sockets, quic_sockets)

    def _set_quic_addresses(
        self,
        sockets: list[socket.socket],
        entrypoints: list[socket.socket] | None = None,
    ) -> None:
        """Declare the QUIC listeners as alternatives for the entrypoints.

        Each QUIC socket is paired with the entrypoint (the secure bind)
        it can serve. The pairing uses the actual bound addresses, so a
        system assigned port is advertised with the value the system
        chose rather than the requested ``:0``. The listeners are also
        watched, such that a closed or failed listener is no longer
        advertised.
        """
        self._quic_addresses = []
        self._alt_svc_alternatives = []
        candidates = list(entrypoints or [])
        paired: set[int] = set()
        for index, sock in enumerate(sockets):
            name = sock.getsockname()
            address = _socket_address(name)
            if address is not None:
                entrypoint, entrypoint_socket = self._pair_entrypoint(
                    address, candidates, index, paired
                )
                alternative = AltSvcAlternative(
                    address=address,
                    entrypoint=entrypoint,
                    socket=sock,
                    entrypoint_socket=entrypoint_socket,
                )
                self._watch_alternative(alternative)
                self._quic_addresses.append(name)
                self._alt_svc_alternatives.append(alternative)
            else:
                warnings.warn(
                    f'Cannot create a alt-svc header for the QUIC socket with address "{name}"',
                    Warning,
                )

    def _pair_entrypoint(
        self,
        address: tuple[str, int],
        entrypoints: list[Any],
        index: int,
        paired: set[int],
    ) -> tuple[tuple[str, int] | None, Any]:
        for candidate_index, entrypoint in enumerate(entrypoints):
            if candidate_index in paired:
                continue
            entrypoint_address = _socket_address(_getsockname(entrypoint))
            if entrypoint_address is not None and _hosts_compatible(
                entrypoint_address[0], address[0]
            ):
                paired.add(candidate_index)
                return entrypoint_address, _socket_of(entrypoint)
        if index < len(entrypoints) and index not in paired:
            entrypoint = entrypoints[index]
            entrypoint_address = _socket_address(_getsockname(entrypoint))
            if entrypoint_address is not None:
                paired.add(index)
                return entrypoint_address, _socket_of(entrypoint)
        return None, None

    def _watch_alternative(self, alternative: AltSvcAlternative) -> None:
        sockets = {
            id(alternative.socket): alternative.socket,
            id(alternative.entrypoint_socket): alternative.entrypoint_socket,
        }
        for sock in sockets.values():
            if sock is None:
                continue
            close = getattr(sock, "close", None)
            if close is None or not callable(close):
                continue

            def watcher(
                *args: Any,
                _alternative: AltSvcAlternative = alternative,
                _close: Any = close,
                **kwargs: Any,
            ) -> Any:
                _alternative.closed = True
                return _close(*args, **kwargs)

            try:
                sock.close = watcher
            except (AttributeError, TypeError):
                # Builtin sockets cannot be patched, the closed state is
                # instead read from the socket when responding.
                pass

    def mark_listener_failed(self, sock: Any) -> None:
        """Stop advertising the alternative(s) served by ``sock``.

        Used when a listener cannot be started (e.g. the datagram
        endpoint errors), the address is then no longer advertised.
        """
        address = _socket_address(_getsockname(sock))
        for alternative in self._alt_svc_alternatives:
            if alternative.socket is sock or (
                address is not None and alternative.address == address
            ):
                alternative.closed = True

    def _valid_alternatives(self, server: tuple[str, int] | None) -> list[AltSvcAlternative]:
        alternatives = self._alt_svc_alternatives
        if not alternatives and self._quic_addresses:
            # Addresses declared without their listeners cannot be
            # watched or paired, so they are taken as they are.
            alternatives = [
                AltSvcAlternative(address=address)
                for address in map(_socket_address, self._quic_addresses)
                if address is not None
            ]
        return [
            alternative
            for alternative in alternatives
            if alternative.valid and alternative.serves(server)
        ]

    def attach_sockets(self, sockets: Sockets) -> None:
        """Pair the alternatives with this process' listeners.

        When workers are used the config is pickled, which drops the
        socket references, so the alternatives are re-paired with the
        sockets handed to the worker.
        """
        if self.alt_svc_headers or not sockets.quic_sockets:
            return
        if self._alt_svc_alternatives and all(
            alternative.socket is not None for alternative in self._alt_svc_alternatives
        ):
            return
        self._set_quic_addresses(
            sockets.quic_sockets, [*sockets.secure_sockets, *sockets.insecure_sockets]
        )

    def __getstate__(self) -> dict:
        state = dict(self.__dict__)
        alternatives = state.get("_alt_svc_alternatives", [])
        if alternatives:
            # Sockets are process specific (and passed to the workers
            # separately), so only the declared addresses are pickled.
            state["_alt_svc_alternatives"] = [
                AltSvcAlternative(address=a.address, entrypoint=a.entrypoint)
                for a in alternatives
            ]
        return state

    def __setstate__(self, state: dict) -> None:
        self.__dict__.update(state)

    def _create_sockets(
        self, binds: list[str], type_: int = socket.SOCK_STREAM
    ) -> list[socket.socket]:
        sockets: list[socket.socket] = []
        for bind in binds:
            binding: Any = None
            if bind.startswith("unix:"):
                sock = socket.socket(socket.AF_UNIX, type_)
                binding = bind[5:]
                try:
                    if stat.S_ISSOCK(os.stat(binding).st_mode):
                        os.remove(binding)
                except FileNotFoundError:
                    pass
            elif bind.startswith("fd://"):
                sock = socket.socket(fileno=int(bind[5:]))
                actual_type = sock.getsockopt(socket.SOL_SOCKET, socket.SO_TYPE)
                if actual_type != type_:
                    raise SocketTypeError(type_, actual_type)
            else:
                bind = bind.replace("[", "").replace("]", "")
                try:
                    value = bind.rsplit(":", 1)
                    host, port = value[0], int(value[1])
                except (ValueError, IndexError):
                    host, port = bind, 8000
                sock = socket.socket(socket.AF_INET6 if ":" in host else socket.AF_INET, type_)

                if type_ == socket.SOCK_STREAM:
                    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)

                if self.workers > 1:
                    try:
                        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
                    except AttributeError:
                        pass
                binding = (host, port)

            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)

            if bind.startswith("unix:"):
                if self.umask is not None:
                    current_umask = os.umask(self.umask)
                sock.bind(binding)
                if self.user is not None and self.group is not None:
                    os.chown(binding, self.user, self.group)
                if self.umask is not None:
                    os.umask(current_umask)
            elif bind.startswith("fd://"):
                pass
            else:
                sock.bind(binding)

            sock.setblocking(False)
            try:
                sock.set_inheritable(True)
            except AttributeError:
                pass
            sockets.append(sock)
        return sockets

    def response_headers(
        self, protocol: str, server: tuple[str, int] | None = None
    ) -> list[tuple[bytes, bytes]]:
        headers = []
        if self.include_date_header:
            headers.append((b"date", format_date_time(time()).encode("ascii")))
        if self.include_server_header:
            headers.append((b"server", f"hypercorn-{protocol}".encode("ascii")))

        for alt_svc_header in self.alt_svc_headers:
            headers.append((b"alt-svc", alt_svc_header.encode()))
        if len(self.alt_svc_headers) == 0:
            alternatives = self._valid_alternatives(server)
            if alternatives:
                from aioquic.h3.connection import H3_ALPN

                for version in H3_ALPN:
                    for alternative in alternatives:
                        port = alternative.address[1]
                        headers.append(
                            (b"alt-svc", b'%s=":%d"; ma=3600' % (version.encode(), port))
                        )

        return headers

    def set_statsd_logger_class(self, statsd_logger: type[Logger]) -> None:
        if self.logger_class == Logger and self.statsd_host is not None:
            self.logger_class = statsd_logger

    @classmethod
    def from_mapping(
        cls: type[Config], mapping: Mapping[str, Any] | None = None, **kwargs: Any
    ) -> Config:
        """Create a configuration from a mapping.

        This allows either a mapping to be directly passed or as
        keyword arguments, for example,

        .. code-block:: python

            config = {'keep_alive_timeout': 10}
            Config.from_mapping(config)
            Config.from_mapping(keep_alive_timeout=10)

        Arguments:
            mapping: Optionally a mapping object.
            kwargs: Optionally a collection of keyword arguments to
                form a mapping.
        """
        mappings: dict[str, Any] = {}
        if mapping is not None:
            mappings.update(mapping)
        mappings.update(kwargs)
        config = cls()
        for key, value in mappings.items():
            try:
                setattr(config, key, value)
            except AttributeError:
                pass

        return config

    @classmethod
    def from_pyfile(cls: type[Config], filename: FilePath) -> Config:
        """Create a configuration from a Python file.

        .. code-block:: python

            Config.from_pyfile('hypercorn_config.py')

        Arguments:
            filename: The filename which gives the path to the file.
        """
        file_path = os.fspath(filename)
        spec = importlib.util.spec_from_file_location("module.name", file_path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return cls.from_object(module)

    @classmethod
    def from_toml(cls: type[Config], filename: FilePath) -> Config:
        """Load the configuration values from a TOML formatted file.

        This allows configuration to be loaded as so

        .. code-block:: python

            Config.from_toml('config.toml')

        Arguments:
            filename: The filename which gives the path to the file.
        """
        file_path = os.fspath(filename)
        with open(file_path, "rb") as file_:
            data = tomllib.load(file_)
        return cls.from_mapping(data)

    @classmethod
    def from_object(cls: type[Config], instance: object | str) -> Config:
        """Create a configuration from a Python object.

        This can be used to reference modules or objects within
        modules for example,

        .. code-block:: python

            Config.from_object('module')
            Config.from_object('module.instance')
            from module import instance
            Config.from_object(instance)

        are valid.

        Arguments:
            instance: Either a str referencing a python object or the
                object itself.

        """
        if isinstance(instance, str):
            try:
                instance = importlib.import_module(instance)
            except ImportError:
                path, config = instance.rsplit(".", 1)
                module = importlib.import_module(path)
                instance = getattr(module, config)

        mapping = {
            key: getattr(instance, key)
            for key in dir(instance)
            if not isinstance(getattr(instance, key), types.ModuleType) and not key.startswith("__")
        }
        return cls.from_mapping(mapping)
