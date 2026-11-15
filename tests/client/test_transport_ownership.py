from __future__ import annotations

import typing

import pytest

import httpx


def hello_world(request: httpx.Request) -> httpx.Response:
    return httpx.Response(200, text="Hello, world!")


class CloseCountingTransport(httpx.BaseTransport):
    """
    A sync transport double that records how many times it is closed or
    driven through the context manager protocol.
    """

    def __init__(self, *, fail_enter: bool = False) -> None:
        self.close_count = 0
        self.enter_count = 0
        self.exit_count = 0
        self.fail_enter = fail_enter

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        return hello_world(request)

    def close(self) -> None:
        self.close_count += 1

    def __enter__(self) -> "CloseCountingTransport":
        self.enter_count += 1
        if self.fail_enter:
            raise RuntimeError("boom on enter")
        return self

    def __exit__(self, *args: typing.Any) -> None:
        self.exit_count += 1
        self.close()


class AsyncCloseCountingTransport(httpx.AsyncBaseTransport):
    """
    An async transport double that records how many times it is closed or
    driven through the async context manager protocol.
    """

    def __init__(self, *, fail_enter: bool = False) -> None:
        self.close_count = 0
        self.enter_count = 0
        self.exit_count = 0
        self.fail_enter = fail_enter

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        return hello_world(request)

    async def aclose(self) -> None:
        self.close_count += 1

    async def __aenter__(self) -> "AsyncCloseCountingTransport":
        self.enter_count += 1
        if self.fail_enter:
            raise RuntimeError("boom on enter")
        return self

    async def __aexit__(self, *args: typing.Any) -> None:
        self.exit_count += 1
        await self.aclose()


# ---------------------------------------------------------------------------
# Synchronous behaviour
# ---------------------------------------------------------------------------


def test_explicitly_borrowed_transport_is_not_closed_by_client() -> None:
    transport = CloseCountingTransport()

    # Two clients share one explicitly borrowed transport. Neither may close
    # it, since its lifetime stays with the caller.
    client_a = httpx.Client(transport=transport, owner=False)
    client_b = httpx.Client(transport=transport, owner=False)
    client_a.close()
    client_b.close()

    assert transport.close_count == 0


def test_provided_transport_is_owned_by_default() -> None:
    transport = CloseCountingTransport()

    client = httpx.Client(transport=transport)
    client.close()

    # The historical behaviour is preserved: a passed-in transport is closed
    # unless borrowing is requested explicitly.
    assert transport.close_count == 1


def test_client_created_transport_is_owned_by_default() -> None:
    # With no explicit transport the client instantiates one itself and must
    # therefore take ownership of it without any extra flag.
    client = httpx.Client()
    transport = client._transport

    assert client._owns_transport(transport)

    client.close()
    assert client.is_closed


def test_borrowed_mount_is_not_closed() -> None:
    transport = CloseCountingTransport()
    mounted = CloseCountingTransport()

    client = httpx.Client(
        transport=transport,
        owner=False,
        mounts=[httpx.Mount("all://example.org", mounted, owner=False)],
    )
    client.close()

    assert transport.close_count == 0
    assert mounted.close_count == 0


def test_owned_mount_is_closed() -> None:
    mounted = CloseCountingTransport()
    main = CloseCountingTransport()

    client = httpx.Client(
        transport=main,
        mounts=[httpx.Mount("all://example.org", mounted)],
    )
    client.close()

    assert mounted.close_count == 1
    assert main.close_count == 1


def test_same_transport_mounted_more_than_once_closes_once() -> None:
    shared = CloseCountingTransport()

    client = httpx.Client(
        transport=shared,
        mounts=[
            httpx.Mount("http://a.example", shared),
            httpx.Mount("http://b.example", shared),
        ],
    )
    client.close()

    # The very same object is the main transport and two mounts, but ownership
    # is tracked by identity so it must be closed exactly once.
    assert shared.close_count == 1


def test_shared_transport_between_two_clients_only_owner_closes() -> None:
    shared = CloseCountingTransport()

    owner_client = httpx.Client(transport=shared)
    borrower_client = httpx.Client(transport=shared, owner=False)

    borrower_client.close()
    assert shared.close_count == 0

    owner_client.close()
    assert shared.close_count == 1


def test_context_manager_enters_and_exits_only_owned() -> None:
    owned = CloseCountingTransport()
    borrowed = CloseCountingTransport()

    with httpx.Client(
        transport=owned,
        mounts=[httpx.Mount("all://example.org", borrowed, owner=False)],
    ):
        pass

    assert owned.enter_count == 1
    assert owned.exit_count == 1
    assert owned.close_count == 1
    assert borrowed.enter_count == 0
    assert borrowed.exit_count == 0
    assert borrowed.close_count == 0


def test_enter_failure_closes_owned_and_keeps_borrowed() -> None:
    owned = CloseCountingTransport(fail_enter=True)
    borrowed = CloseCountingTransport()

    with pytest.raises(RuntimeError, match="boom on enter"):
        with httpx.Client(
            transport=owned,
            mounts=[httpx.Mount("all://example.org", borrowed, owner=False)],
        ):
            pass  # pragma: no cover

    assert owned.close_count == 1
    assert borrowed.close_count == 0


def test_init_failure_closes_owned_transport() -> None:
    transport = CloseCountingTransport()

    with pytest.raises(ValueError):
        httpx.Client(
            transport=transport,
            # An invalid mount key raises while building URLPattern, after the
            # main transport has already been registered.
            mounts=[httpx.Mount("not-a-url-pattern", CloseCountingTransport())],
        )

    assert transport.close_count == 1


def test_init_failure_leaves_borrowed_transport_open() -> None:
    transport = CloseCountingTransport()

    with pytest.raises(ValueError):
        httpx.Client(
            transport=transport,
            owner=False,
            mounts=[httpx.Mount("not-a-url-pattern", CloseCountingTransport())],
        )

    assert transport.close_count == 0


def test_mapping_mounts_remain_owned() -> None:
    mounted = CloseCountingTransport()

    client = httpx.Client(mounts={"all://": mounted})
    client.close()

    # The historical mapping API keeps its ownership semantics.
    assert mounted.close_count == 1


def test_invalid_mount_entry_type() -> None:
    with pytest.raises(TypeError):
        httpx.Client(
            mounts=[("all://", CloseCountingTransport())]  # type: ignore[list-item]
        )


# ---------------------------------------------------------------------------
# Asynchronous behaviour
# ---------------------------------------------------------------------------


@pytest.mark.anyio
async def test_async_explicitly_borrowed_transport_is_not_closed() -> None:
    transport = AsyncCloseCountingTransport()

    client_a = httpx.AsyncClient(transport=transport, owner=False)
    client_b = httpx.AsyncClient(transport=transport, owner=False)
    await client_a.aclose()
    await client_b.aclose()

    assert transport.close_count == 0


@pytest.mark.anyio
async def test_async_provided_transport_is_owned_by_default() -> None:
    transport = AsyncCloseCountingTransport()

    client = httpx.AsyncClient(transport=transport)
    await client.aclose()

    assert transport.close_count == 1


@pytest.mark.anyio
async def test_async_same_transport_mounted_more_than_once_closes_once() -> None:
    shared = AsyncCloseCountingTransport()

    client = httpx.AsyncClient(
        transport=shared,
        mounts=[
            httpx.Mount("http://a.example", shared),
            httpx.Mount("http://b.example", shared),
        ],
    )
    await client.aclose()

    assert shared.close_count == 1


@pytest.mark.anyio
async def test_async_context_manager_enters_and_exits_only_owned() -> None:
    owned = AsyncCloseCountingTransport()
    borrowed = AsyncCloseCountingTransport()

    async with httpx.AsyncClient(
        transport=owned,
        mounts=[httpx.Mount("all://example.org", borrowed, owner=False)],
    ):
        pass

    assert owned.enter_count == 1
    assert owned.exit_count == 1
    assert owned.close_count == 1
    assert borrowed.enter_count == 0
    assert borrowed.exit_count == 0
    assert borrowed.close_count == 0


@pytest.mark.anyio
async def test_async_enter_failure_closes_owned_and_keeps_borrowed() -> None:
    owned = AsyncCloseCountingTransport(fail_enter=True)
    borrowed = AsyncCloseCountingTransport()

    with pytest.raises(RuntimeError, match="boom on enter"):
        async with httpx.AsyncClient(
            transport=owned,
            mounts=[httpx.Mount("all://example.org", borrowed, owner=False)],
        ):
            pass  # pragma: no cover

    assert owned.close_count == 1
    assert borrowed.close_count == 0


@pytest.mark.anyio
async def test_async_init_failure_closes_owned_transport() -> None:
    transport = AsyncCloseCountingTransport()

    with pytest.raises(ValueError):
        httpx.AsyncClient(
            transport=transport,
            mounts=[httpx.Mount("not-a-url-pattern", AsyncCloseCountingTransport())],
        )

    assert transport.close_count == 1


@pytest.mark.anyio
async def test_async_init_failure_leaves_borrowed_transport_open() -> None:
    transport = AsyncCloseCountingTransport()

    with pytest.raises(ValueError):
        httpx.AsyncClient(
            transport=transport,
            owner=False,
            mounts=[httpx.Mount("not-a-url-pattern", AsyncCloseCountingTransport())],
        )

    assert transport.close_count == 0


@pytest.mark.anyio
async def test_async_owned_mount_is_closed() -> None:
    mounted = AsyncCloseCountingTransport()

    client = httpx.AsyncClient(
        mounts=[httpx.Mount("all://example.org", mounted)]
    )
    await client.aclose()

    assert mounted.close_count == 1
