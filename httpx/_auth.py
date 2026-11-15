from __future__ import annotations

import hashlib
import os
import re
import threading
import time
import typing
from base64 import b64encode
from urllib.request import parse_http_list

from ._exceptions import ProtocolError
from ._models import Cookies, Request, Response
from ._urls import URL
from ._utils import to_bytes, to_str, unquote

if typing.TYPE_CHECKING:  # pragma: no cover
    from hashlib import _Hash


__all__ = ["Auth", "BasicAuth", "DigestAuth", "FunctionAuth", "NetRCAuth"]


_DigestAuthOrigin = typing.Tuple[str, str, typing.Optional[int]]
_DigestAuthProtectionSpace = typing.Tuple[str, str, typing.Optional[int], bytes]


class _DigestAuthState:
    __slots__ = ("challenge", "nonce_count", "path")

    def __init__(self, challenge: _DigestAuthChallenge, path: str) -> None:
        self.challenge = challenge
        self.nonce_count = 1
        self.path = path


class Auth:
    """
    Base class for all authentication schemes.

    To implement a custom authentication scheme, subclass `Auth` and override
    the `.auth_flow()` method.

    If the authentication scheme does I/O such as disk access or network calls, or uses
    synchronization primitives such as locks, you should override `.sync_auth_flow()`
    and/or `.async_auth_flow()` instead of `.auth_flow()` to provide specialized
    implementations that will be used by `Client` and `AsyncClient` respectively.
    """

    requires_request_body = False
    requires_response_body = False

    def auth_flow(self, request: Request) -> typing.Generator[Request, Response, None]:
        """
        Execute the authentication flow.

        To dispatch a request, `yield` it:

        ```
        yield request
        ```

        The client will `.send()` the response back into the flow generator. You can
        access it like so:

        ```
        response = yield request
        ```

        A `return` (or reaching the end of the generator) will result in the
        client returning the last response obtained from the server.

        You can dispatch as many requests as is necessary.
        """
        yield request

    def sync_auth_flow(
        self, request: Request
    ) -> typing.Generator[Request, Response, None]:
        """
        Execute the authentication flow synchronously.

        By default, this defers to `.auth_flow()`. You should override this method
        when the authentication scheme does I/O and/or uses concurrency primitives.
        """
        if self.requires_request_body:
            request.read()

        flow = self.auth_flow(request)
        request = next(flow)

        while True:
            response = yield request
            if self.requires_response_body:
                response.read()

            try:
                request = flow.send(response)
            except StopIteration:
                break

    async def async_auth_flow(
        self, request: Request
    ) -> typing.AsyncGenerator[Request, Response]:
        """
        Execute the authentication flow asynchronously.

        By default, this defers to `.auth_flow()`. You should override this method
        when the authentication scheme does I/O and/or uses concurrency primitives.
        """
        if self.requires_request_body:
            await request.aread()

        flow = self.auth_flow(request)
        request = next(flow)

        while True:
            response = yield request
            if self.requires_response_body:
                await response.aread()

            try:
                request = flow.send(response)
            except StopIteration:
                break


class FunctionAuth(Auth):
    """
    Allows the 'auth' argument to be passed as a simple callable function,
    that takes the request, and returns a new, modified request.
    """

    def __init__(self, func: typing.Callable[[Request], Request]) -> None:
        self._func = func

    def auth_flow(self, request: Request) -> typing.Generator[Request, Response, None]:
        yield self._func(request)


class BasicAuth(Auth):
    """
    Allows the 'auth' argument to be passed as a (username, password) pair,
    and uses HTTP Basic authentication.
    """

    def __init__(self, username: str | bytes, password: str | bytes) -> None:
        self._auth_header = self._build_auth_header(username, password)

    def auth_flow(self, request: Request) -> typing.Generator[Request, Response, None]:
        request.headers["Authorization"] = self._auth_header
        yield request

    def _build_auth_header(self, username: str | bytes, password: str | bytes) -> str:
        userpass = b":".join((to_bytes(username), to_bytes(password)))
        token = b64encode(userpass).decode()
        return f"Basic {token}"


class NetRCAuth(Auth):
    """
    Use a 'netrc' file to lookup basic auth credentials based on the url host.
    """

    def __init__(self, file: str | None = None) -> None:
        # Lazily import 'netrc'.
        # There's no need for us to load this module unless 'NetRCAuth' is being used.
        import netrc

        self._netrc_info = netrc.netrc(file)

    def auth_flow(self, request: Request) -> typing.Generator[Request, Response, None]:
        auth_info = self._netrc_info.authenticators(request.url.host)
        if auth_info is None or not auth_info[2]:
            # The netrc file did not have authentication credentials for this host.
            yield request
        else:
            # Build a basic auth header with credentials from the netrc file.
            request.headers["Authorization"] = self._build_auth_header(
                username=auth_info[0], password=auth_info[2]
            )
            yield request

    def _build_auth_header(self, username: str | bytes, password: str | bytes) -> str:
        userpass = b":".join((to_bytes(username), to_bytes(password)))
        token = b64encode(userpass).decode()
        return f"Basic {token}"


class DigestAuth(Auth):
    _ALGORITHM_TO_HASH_FUNCTION: dict[str, typing.Callable[[bytes], _Hash]] = {
        "MD5": hashlib.md5,
        "MD5-SESS": hashlib.md5,
        "SHA": hashlib.sha1,
        "SHA-SESS": hashlib.sha1,
        "SHA-256": hashlib.sha256,
        "SHA-256-SESS": hashlib.sha256,
        "SHA-512": hashlib.sha512,
        "SHA-512-SESS": hashlib.sha512,
    }
    # One authenticated request, plus one retry for a stale challenge.
    _MAX_AUTH_ATTEMPTS = 2

    def __init__(self, username: str | bytes, password: str | bytes) -> None:
        self._username = to_bytes(username)
        self._password = to_bytes(password)
        self._challenge_states: dict[
            _DigestAuthProtectionSpace, _DigestAuthState
        ] = {}
        self._challenge_lock = threading.Lock()

    def auth_flow(self, request: Request) -> typing.Generator[Request, Response, None]:
        state = self._get_preemptive_challenge(request)
        sent_preemptively = state is not None
        auth_attempts = 0
        if state is not None:
            request.headers["Authorization"] = self._build_auth_header(request, state)
            auth_attempts = 1

        response = yield request

        while True:
            if (
                response.status_code != 401
                or "www-authenticate" not in response.headers
            ):
                # If the response is not a 401 then we don't
                # need to build an authenticated request.
                return

            for auth_header in response.headers.get_list("www-authenticate"):
                if auth_header.lower().startswith("digest "):
                    break
            else:
                # If the response does not include a 'WWW-Authenticate: Digest ...'
                # header, then we don't need to build an authenticated request.
                return

            challenge = self._parse_challenge(request, response, auth_header)
            challenge_request = response.request or request
            state = self._store_challenge(challenge_request, challenge)

            if auth_attempts >= self._MAX_AUTH_ATTEMPTS:
                return

            if auth_attempts > 0 and not challenge.stale:
                # A non-stale challenge after sending credentials means that the
                # credentials were rejected. The exception is a preemptive
                # request, where the cached challenge may simply have expired.
                if not (sent_preemptively and auth_attempts == 1):
                    return

            request.headers["Authorization"] = self._build_auth_header(request, state)
            if response.cookies:
                Cookies(response.cookies).set_cookie_header(request=request)
            response = yield request
            auth_attempts += 1
            sent_preemptively = False

    def _parse_challenge(
        self, request: Request, response: Response, auth_header: str
    ) -> _DigestAuthChallenge:
        """
        Returns a challenge from a Digest WWW-Authenticate header.
        These take the form of:
        `Digest realm="realm@host.com",qop="auth,auth-int",nonce="abc",opaque="xyz"`
        """
        scheme, _, fields = auth_header.partition(" ")

        # This method should only ever have been called with a Digest auth header.
        assert scheme.lower() == "digest"

        header_dict: dict[str, str] = {}
        for field in parse_http_list(fields):
            key, value = field.strip().split("=", 1)
            header_dict[key.strip().lower()] = unquote(value.strip())

        try:
            realm = header_dict["realm"].encode()
            nonce = header_dict["nonce"].encode()
            algorithm = header_dict.get("algorithm", "MD5").strip()
            opaque = header_dict["opaque"].encode() if "opaque" in header_dict else None
            qop = header_dict["qop"].encode() if "qop" in header_dict else None
            stale = header_dict.get("stale", "false").strip().lower() == "true"
            if algorithm.upper() not in self._ALGORITHM_TO_HASH_FUNCTION:
                message = f'Unsupported Digest algorithm "{algorithm}"'
                raise ProtocolError(message, request=request)
            return _DigestAuthChallenge(
                realm=realm,
                nonce=nonce,
                algorithm=algorithm,
                opaque=opaque,
                qop=qop,
                stale=stale,
            )
        except (KeyError, IndexError, TypeError, ValueError) as exc:
            message = "Malformed Digest WWW-Authenticate header"
            raise ProtocolError(message, request=request) from exc

    def _build_auth_header(
        self, request: Request, state: _DigestAuthState
    ) -> str:
        with self._challenge_lock:
            challenge = state.challenge
            nonce_count = state.nonce_count
            state.nonce_count += 1

        hash_func = self._ALGORITHM_TO_HASH_FUNCTION.get(challenge.algorithm.upper())
        if hash_func is None:
            message = f'Unsupported Digest algorithm "{challenge.algorithm}"'
            raise ProtocolError(message, request=request)

        def digest(data: bytes) -> bytes:
            return hash_func(data).hexdigest().encode()

        A1 = b":".join((self._username, challenge.realm, self._password))

        path = request.url.raw_path
        A2 = b":".join((request.method.encode(), path))
        # TODO: implement auth-int
        HA2 = digest(A2)

        nc_value = b"%08x" % nonce_count
        cnonce = self._get_client_nonce(nonce_count, challenge.nonce)

        HA1 = digest(A1)
        if challenge.algorithm.lower().endswith("-sess"):
            HA1 = digest(b":".join((HA1, challenge.nonce, cnonce)))

        qop = self._resolve_qop(challenge.qop, request=request)
        if qop is None:
            # Following RFC 2069
            digest_data = [HA1, challenge.nonce, HA2]
        else:
            # Following RFC 2617/7616
            digest_data = [HA1, challenge.nonce, nc_value, cnonce, qop, HA2]

        format_args = {
            "username": self._username,
            "realm": challenge.realm,
            "nonce": challenge.nonce,
            "uri": path,
            "response": digest(b":".join(digest_data)),
            "algorithm": challenge.algorithm.encode(),
        }
        if challenge.opaque:
            format_args["opaque"] = challenge.opaque
        if qop:
            format_args["qop"] = b"auth"
            format_args["nc"] = nc_value
            format_args["cnonce"] = cnonce

        return "Digest " + self._get_header_value(format_args)

    def _get_preemptive_challenge(
        self, request: Request
    ) -> _DigestAuthState | None:
        origin = _get_origin(request.url)
        path = request.url.path
        with self._challenge_lock:
            matching_states = [
                state
                for protection_space, state in self._challenge_states.items()
                if protection_space[:3] == origin
            ]

            if not matching_states:
                return None

            # Prefer the challenge whose request path most closely encloses this
            # request. If several realms exist for the same origin then this avoids
            # arbitrarily using a challenge for a different protected path.
            best_state = None
            for state in matching_states:
                if _path_is_in_scope(path, state.path):
                    if best_state is None or len(state.path) >= len(best_state.path):
                        best_state = state
            return best_state if best_state is not None else matching_states[-1]

    def _store_challenge(
        self, request: Request, challenge: _DigestAuthChallenge
    ) -> _DigestAuthState:
        protection_space = _get_protection_space(request, challenge)
        with self._challenge_lock:
            state = self._challenge_states.get(protection_space)
            if state is None or state.challenge.nonce != challenge.nonce:
                state = _DigestAuthState(challenge, request.url.path)
            else:
                state.challenge = challenge
                state.path = request.url.path

            # Keep recently used challenges last so that they take priority when
            # a request could match more than one protection space.
            self._challenge_states.pop(protection_space, None)
            self._challenge_states[protection_space] = state

        return state

    def _get_client_nonce(self, nonce_count: int, nonce: bytes) -> bytes:
        s = str(nonce_count).encode()
        s += nonce
        s += time.ctime().encode()
        s += os.urandom(8)

        return hashlib.sha1(s).hexdigest()[:16].encode()

    def _get_header_value(self, header_fields: dict[str, bytes]) -> str:
        NON_QUOTED_FIELDS = ("algorithm", "qop", "nc")
        QUOTED_TEMPLATE = '{}="{}"'
        NON_QUOTED_TEMPLATE = "{}={}"

        header_value = ""
        for i, (field, value) in enumerate(header_fields.items()):
            if i > 0:
                header_value += ", "
            template = (
                QUOTED_TEMPLATE
                if field not in NON_QUOTED_FIELDS
                else NON_QUOTED_TEMPLATE
            )
            header_value += template.format(field, to_str(value))

        return header_value

    def _resolve_qop(self, qop: bytes | None, request: Request) -> bytes | None:
        if qop is None:
            return None
        qops = re.split(b", ?", qop)
        if b"auth" in qops:
            return b"auth"

        if qops == [b"auth-int"]:
            raise NotImplementedError("Digest auth-int support is not yet implemented")

        message = f'Unexpected qop value "{qop!r}" in digest auth'
        raise ProtocolError(message, request=request)


class _DigestAuthChallenge(typing.NamedTuple):
    realm: bytes
    nonce: bytes
    algorithm: str
    opaque: bytes | None
    qop: bytes | None
    stale: bool = False


def _get_origin(url: URL) -> _DigestAuthOrigin:
    return (url.scheme, url.host, url.port)


def _get_protection_space(
    request: Request, challenge: _DigestAuthChallenge
) -> _DigestAuthProtectionSpace:
    return (*_get_origin(request.url), challenge.realm)


def _path_is_in_scope(path: str, challenge_path: str) -> bool:
    if path == challenge_path or challenge_path == "/":
        return True
    return path.startswith(challenge_path.rstrip("/") + "/")
