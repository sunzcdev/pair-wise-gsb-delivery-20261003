from __future__ import annotations

import collections
import io
import sys
import typing

from .._models import Request, Response
from .._types import SyncByteStream
from .base import BaseTransport

if typing.TYPE_CHECKING:
    from _typeshed import OptExcInfo  # pragma: no cover
    from _typeshed.wsgi import WSGIApplication  # pragma: no cover


__all__ = ["WSGITransport"]


class WSGIByteStream(SyncByteStream):
    """
    A byte stream wrapping the iterable returned by a WSGI application.

    This supports applications that defer calling `start_response` until
    the first iteration, and applications that mix `write()` calls with a
    returned iterable, delivering output in the order the application
    produced it. The application iterable is closed once it is exhausted,
    if the client closes the response early, or if iteration fails.
    """

    def __init__(
        self,
        result: typing.Iterable[bytes],
        write_buffer: collections.deque[bytes],
    ) -> None:
        self._close = getattr(result, "close", None)
        self._app_iter = iter(result)
        self._write_buffer = write_buffer
        self._pending: collections.deque[bytes] = collections.deque()
        self._exhausted = False

    def _drain_writes(self) -> None:
        while self._write_buffer:
            self._pending.append(self._write_buffer.popleft())

    def pull(self) -> bool:
        """
        Advance the application iterable by one step, queueing any output
        in the order the application produced it.

        Returns `False` once the application iterable is exhausted.
        """
        if self._exhausted:
            return False
        try:
            chunk = next(self._app_iter)
        except StopIteration:
            self._exhausted = True
            self._drain_writes()
            self.close()
            return False
        except BaseException:
            self._exhausted = True
            self._close_suppressing_errors()
            raise
        # Anything the application passed to `write()` before yielding
        # this chunk must be delivered first.
        self._drain_writes()
        self._pending.append(chunk)
        return True

    def __iter__(self) -> typing.Iterator[bytes]:
        try:
            while True:
                while self._pending:
                    chunk = self._pending.popleft()
                    if chunk:
                        yield chunk
                # Note that `pull()` may queue trailing `write()` output
                # when the application iterable is exhausted, so the
                # queued chunks are checked again before finishing.
                if not self.pull() and not self._pending:
                    break
        except BaseException:
            # Either the application iterable raised, or the client
            # stopped reading the response early. Make sure the application
            # iterable is closed, without letting a failing `close()`
            # mask an exception that is already in flight.
            self._close_suppressing_errors()
            raise

    def close(self) -> None:
        close, self._close = self._close, None
        if close is not None:
            close()

    def _close_suppressing_errors(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class WSGITransport(BaseTransport):
    """
    A custom transport that handles sending requests directly to an WSGI app.
    The simplest way to use this functionality is to use the `app` argument.

    ```
    client = httpx.Client(app=app)
    ```

    Alternatively, you can setup the transport instance explicitly.
    This allows you to include any additional configuration arguments specific
    to the WSGITransport class:

    ```
    transport = httpx.WSGITransport(
        app=app,
        script_name="/submount",
        remote_addr="1.2.3.4"
    )
    client = httpx.Client(transport=transport)
    ```

    Arguments:

    * `app` - The WSGI application.
    * `raise_app_exceptions` - Boolean indicating if exceptions in the application
       should be raised. Default to `True`. Can be set to `False` for use cases
       such as testing the content of a client 500 response.
    * `script_name` - The root path on which the WSGI application should be mounted.
    * `remote_addr` - A string indicating the client IP of incoming requests.
    ```
    """

    def __init__(
        self,
        app: WSGIApplication,
        raise_app_exceptions: bool = True,
        script_name: str = "",
        remote_addr: str = "127.0.0.1",
        wsgi_errors: typing.TextIO | None = None,
    ) -> None:
        self.app = app
        self.raise_app_exceptions = raise_app_exceptions
        self.script_name = script_name
        self.remote_addr = remote_addr
        self.wsgi_errors = wsgi_errors

    def handle_request(self, request: Request) -> Response:
        request.read()
        wsgi_input = io.BytesIO(request.content)

        port = request.url.port or {"http": 80, "https": 443}[request.url.scheme]
        environ = {
            "wsgi.version": (1, 0),
            "wsgi.url_scheme": request.url.scheme,
            "wsgi.input": wsgi_input,
            "wsgi.errors": self.wsgi_errors or sys.stderr,
            "wsgi.multithread": True,
            "wsgi.multiprocess": False,
            "wsgi.run_once": False,
            "REQUEST_METHOD": request.method,
            "SCRIPT_NAME": self.script_name,
            "PATH_INFO": request.url.path,
            "QUERY_STRING": request.url.query.decode("ascii"),
            "SERVER_NAME": request.url.host,
            "SERVER_PORT": str(port),
            "SERVER_PROTOCOL": "HTTP/1.1",
            "REMOTE_ADDR": self.remote_addr,
        }
        for header_key, header_value in request.headers.raw:
            key = header_key.decode("ascii").upper().replace("-", "_")
            if key not in ("CONTENT_TYPE", "CONTENT_LENGTH"):
                key = "HTTP_" + key
            environ[key] = header_value.decode("ascii")

        seen_status = None
        seen_response_headers = None
        seen_exc_info = None
        write_buffer: collections.deque[bytes] = collections.deque()

        def start_response(
            status: str,
            response_headers: list[tuple[str, str]],
            exc_info: OptExcInfo | None = None,
        ) -> typing.Callable[[bytes], typing.Any]:
            nonlocal seen_status, seen_response_headers, seen_exc_info
            seen_status = status
            seen_response_headers = response_headers
            seen_exc_info = exc_info
            return write_buffer.append

        result = self.app(environ, start_response)
        stream = WSGIByteStream(result, write_buffer)

        # WSGI applications may defer calling `start_response` until the
        # first iteration. Pull from the application until the response
        # headers are available, buffering any body chunks produced along
        # the way for later delivery.
        while seen_status is None and stream.pull():
            pass

        assert seen_status is not None
        assert seen_response_headers is not None
        if seen_exc_info and seen_exc_info[0] and self.raise_app_exceptions:
            try:
                stream.close()
            except Exception:
                pass  # Don't let a failing `close()` mask the app exception.
            raise seen_exc_info[1]

        status_code = int(seen_status.split()[0])
        headers = [
            (key.encode("ascii"), value.encode("ascii"))
            for key, value in seen_response_headers
        ]

        return Response(status_code, headers=headers, stream=stream)
