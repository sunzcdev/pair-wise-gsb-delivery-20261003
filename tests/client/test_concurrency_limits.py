from __future__ import annotations

import threading
import time
import typing

import anyio
import pytest

import httpx


def wait_for_waiters(client: httpx.Client, origin: tuple[str, str, int]) -> None:
    deadline = time.monotonic() + 2.0
    while time.monotonic() < deadline:
        with client._origin_limiter._lock:  # type: ignore[attr-defined]
            states = client._origin_limiter._states  # type: ignore[attr-defined]
            state = states.get(origin)
            if state is not None and len(state.waiters) == 2:
                return
        time.sleep(0.001)
    raise AssertionError("Timed out waiting for queued requests.")


def test_limits_origin_concurrency_configuration():
    limits = httpx.Limits(max_connections_per_origin=2)
    assert limits.max_connections_per_origin == 2
    assert limits.max_concurrent_requests_per_origin == 2
    assert limits.max_concurrency_per_origin == 2
    assert "max_connections_per_origin=2" in repr(limits)
    assert limits == httpx.Limits(max_concurrent_requests_per_origin=2)

    with pytest.raises(ValueError):
        httpx.Limits(max_connections_per_origin=0)
    with pytest.raises(ValueError):
        httpx.Limits(
            max_connections_per_origin=1,
            max_concurrent_requests_per_origin=2,
        )


def test_client_origin_concurrency_configuration():
    transport = httpx.MockTransport(lambda request: httpx.Response(200))
    client = httpx.Client(
        transport=transport,
        max_concurrent_requests_per_origin=2,
    )
    assert client.max_connections_per_origin == 2
    assert client.max_concurrent_requests_per_origin == 2
    assert client.max_concurrency_per_origin == 2
    client.close()

    with pytest.raises(ValueError):
        httpx.Client(
            transport=transport,
            max_connections_per_origin=1,
            max_concurrency_per_origin=2,
        )


class BlockingStream(httpx.SyncByteStream):
    def __init__(self, release: threading.Event, closed: threading.Event) -> None:
        self._release = release
        self._closed = closed

    def __iter__(self) -> typing.Iterator[bytes]:
        yield b"first"
        self._release.wait(2.0)
        yield b"second"

    def close(self) -> None:
        self._closed.set()


def test_same_origin_is_fifo_and_other_origins_progress():
    release = threading.Event()
    closed = threading.Event()
    entries: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        entries.append(f"{request.url.host}{request.url.path}")
        if request.url.host == "a.test" and request.url.path == "/1":
            return httpx.Response(200, stream=BlockingStream(release, closed))
        return httpx.Response(200, content=b"ok")

    client = httpx.Client(
        transport=httpx.MockTransport(handler),
        limits=httpx.Limits(max_connections_per_origin=1),
    )
    results: dict[str, bytes] = {}

    def run_first() -> None:
        results["first"] = client.get("http://a.test/1").content

    first = threading.Thread(target=run_first)
    first.start()
    while entries != ["a.test/1"]:
        time.sleep(0.001)

    second = threading.Thread(
        target=lambda: results.setdefault(
            "second", client.get("http://a.test/2").content
        )
    )
    third = threading.Thread(
        target=lambda: results.setdefault(
            "third", client.get("http://a.test/3").content
        )
    )
    other = threading.Thread(
        target=lambda: results.setdefault(
            "other", client.get("http://b.test/1").content
        )
    )
    second.start()
    third.start()
    other.start()

    wait_for_waiters(client, ("http", "a.test", 80))
    other.join(2.0)
    assert results["other"] == b"ok"
    assert "a.test/2" not in entries

    release.set()
    for thread in (first, second, third):
        thread.join(2.0)
        assert not thread.is_alive()

    assert results["first"] == b"firstsecond"
    assert closed.is_set()
    assert entries == ["a.test/1", "b.test/1", "a.test/2", "a.test/3"]
    client.close()


def test_closing_stream_releases_permit():
    entered = threading.Event()

    def handler(request: httpx.Request) -> httpx.Response:
        entered.set()
        return httpx.Response(
            200,
            stream=BlockingStream(threading.Event(), threading.Event()),
        )

    client = httpx.Client(
        transport=httpx.MockTransport(handler),
        limits=httpx.Limits(max_connections_per_origin=1),
    )
    with client.stream("GET", "http://a.test/") as response:
        assert next(response.iter_raw()) == b"first"
    assert entered.is_set()

    def handler_ok(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=b"ok")

    client._transport = httpx.MockTransport(handler_ok)  # type: ignore[assignment]
    assert client.get("http://a.test/").content == b"ok"
    client.close()


def test_transport_and_response_hook_failures_release_permit():
    attempts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        if attempts == 1:
            raise httpx.ConnectError("boom")
        return httpx.Response(200, content=b"ok")

    client = httpx.Client(
        transport=httpx.MockTransport(handler),
        limits=httpx.Limits(max_connections_per_origin=1),
    )
    with pytest.raises(httpx.ConnectError):
        client.get("http://a.test/")
    assert client.get("http://a.test/").content == b"ok"

    def failing_hook(response: httpx.Response) -> None:
        raise RuntimeError("hook")

    client.event_hooks = {"response": [failing_hook]}
    with pytest.raises(RuntimeError, match="hook"):
        client.get("http://a.test/")

    client.event_hooks = {"response": []}
    assert client.get("http://a.test/").content == b"ok"
    client.close()


def test_cross_origin_redirect_closes_previous_response_first():
    closed = threading.Event()

    class RedirectStream(httpx.SyncByteStream):
        def __iter__(self) -> typing.Iterator[bytes]:
            yield b"redirect"

        def close(self) -> None:
            closed.set()

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "a.test":
            return httpx.Response(
                302,
                headers={"Location": "http://b.test/"},
                stream=RedirectStream(),
            )
        assert closed.is_set()
        return httpx.Response(200, content=b"ok")

    client = httpx.Client(
        transport=httpx.MockTransport(handler),
        follow_redirects=True,
        limits=httpx.Limits(max_connections_per_origin=1),
    )
    assert client.get("http://a.test/").content == b"ok"
    client.close()


def test_sync_close_wakes_queued_request():
    release = threading.Event()
    entered = threading.Event()

    def handler(request: httpx.Request) -> httpx.Response:
        entered.set()
        return httpx.Response(200, stream=BlockingStream(release, threading.Event()))

    client = httpx.Client(
        transport=httpx.MockTransport(handler),
        limits=httpx.Limits(max_connections_per_origin=1),
    )
    first = threading.Thread(target=lambda: client.get("http://a.test/1"))
    first.start()
    assert entered.wait(2.0)

    errors: list[BaseException] = []

    def run_second() -> None:
        try:
            client.get("http://a.test/2")
        except BaseException as exc:
            errors.append(exc)

    second = threading.Thread(target=run_second)
    second.start()
    time.sleep(0.05)
    client.close()
    second.join(2.0)
    assert not second.is_alive()
    assert len(errors) == 1
    assert isinstance(errors[0], RuntimeError)
    assert "closed" in str(errors[0])

    release.set()
    first.join(2.0)
    assert not first.is_alive()


class AsyncBlockingStream(httpx.AsyncByteStream):
    def __init__(self, release: anyio.Event) -> None:
        self._release = release

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        yield b"first"
        await self._release.wait()
        yield b"second"

    async def aclose(self) -> None:
        pass


async def wait_for_async_waiters(
    client: httpx.AsyncClient, origin: tuple[str, str, int]
) -> None:
    with anyio.fail_after(2.0):
        while True:
            with client._origin_limiter._lock:  # type: ignore[attr-defined]
                states = client._origin_limiter._states  # type: ignore[attr-defined]
                state = states.get(origin)
                if state is not None and len(state.waiters) == 2:
                    return
            await anyio.sleep(0.001)


@pytest.mark.anyio
async def test_async_queue_cancellation_preserves_fifo():
    release = anyio.Event()
    entries: list[str] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        entries.append(request.url.path)
        if request.url.path == "/1":
            return httpx.Response(200, stream=AsyncBlockingStream(release))
        return httpx.Response(200, content=b"ok")

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler),
        limits=httpx.Limits(max_connections_per_origin=1),
    ) as client:
        results: list[str] = []
        cancel_scope = anyio.CancelScope()

        async def run_second() -> None:
            with cancel_scope:
                results.append((await client.get("http://a.test/2")).text)

        async with anyio.create_task_group() as task_group:
            task_group.start_soon(
                lambda: _run_async_request(client, "/1", results)
            )
            while entries != ["/1"]:
                await anyio.sleep(0.001)

            task_group.start_soon(run_second)
            task_group.start_soon(lambda: _run_async_request(client, "/3", results))
            await wait_for_async_waiters(client, ("http", "a.test", 80))

            cancel_scope.cancel()
            await anyio.sleep(0)

            release.set()

        assert sorted(results) == ["1", "3"]
        assert entries == ["/1", "/3"]


async def _run_async_request(
    client: httpx.AsyncClient, path: str, results: list[str]
) -> None:
    response = await client.get(f"http://a.test{path}")
    results.append(path.strip("/"))


@pytest.mark.anyio
async def test_async_close_wakes_queued_request():
    release = anyio.Event()
    entered = anyio.Event()

    async def handler(request: httpx.Request) -> httpx.Response:
        entered.set()
        return httpx.Response(200, stream=AsyncBlockingStream(release))

    client = httpx.AsyncClient(
        transport=httpx.MockTransport(handler),
        limits=httpx.Limits(max_connections_per_origin=1),
    )
    errors: list[BaseException] = []

    async def run_first() -> None:
        await client.get("http://a.test/1")

    async def run_second() -> None:
        try:
            await client.get("http://a.test/2")
        except BaseException as exc:
            errors.append(exc)

    async with anyio.create_task_group() as task_group:
        task_group.start_soon(run_first)
        await entered.wait()
        task_group.start_soon(run_second)
        await anyio.sleep(0.05)
        await client.aclose()
        await anyio.sleep(0.05)
        release.set()

    assert len(errors) == 1
    assert isinstance(errors[0], RuntimeError)
    assert "closed" in str(errors[0])


@pytest.mark.anyio
async def test_async_failure_releases_permit():
    attempts = 0

    async def handler(request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        if attempts == 1:
            raise httpx.ConnectError("boom")
        return httpx.Response(200, content=b"ok")

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler),
        limits=httpx.Limits(max_connections_per_origin=1),
    ) as client:
        with pytest.raises(httpx.ConnectError):
            await client.get("http://a.test/")
        assert (await client.get("http://a.test/")).content == b"ok"
