import anyio
import pytest

from tests._async.test_warmup import CountingAsyncBackend, HTTP_RESPONSE

import httpcore


@pytest.mark.anyio
async def test_warmup_cancelled_owner_aborts_establishment():
    """
    Cancelling the warmup owner aborts the connection establishment and
    removes the partial connection from the pool.
    """
    gate = anyio.Event()

    class GatedBackend(CountingAsyncBackend):
        async def connect_tcp(self, *args, **kwargs):
            self.connects += 1
            await gate.wait()
            return await super().connect_tcp(*args, **kwargs)

    network_backend = GatedBackend(HTTP_RESPONSE)

    async with httpcore.AsyncConnectionPool(
        network_backend=network_backend,
    ) as pool:
        with anyio.move_on_after(0.05):
            await pool.awarmup("https://example.com/")

        gate.set()
        await anyio.sleep(0.05)

        # The cancelled warmup leaves no connection behind it.
        assert pool.connections == []
        assert network_backend.writes == 0


@pytest.mark.anyio
async def test_joined_warmup_cancellation_leaves_establishment_running():
    """
    Cancelling a caller that merely joined an in-flight warmup only cancels
    its own wait; the owner's connection establishment continues and the
    connection ends up in the pool.
    """
    gate = anyio.Event()

    class GatedBackend(CountingAsyncBackend):
        async def connect_tcp(self, *args, **kwargs):
            await gate.wait()
            return await super().connect_tcp(*args, **kwargs)

    network_backend = GatedBackend(HTTP_RESPONSE)

    async with httpcore.AsyncConnectionPool(
        network_backend=network_backend,
    ) as pool:

        async def owner() -> None:
            await pool.awarmup("https://example.com/")

        async def joining_caller() -> None:
            with anyio.move_on_after(0.05):
                await pool.awarmup("https://example.com/")

        async with anyio.create_task_group() as task_group:
            task_group.start_soon(owner)
            await anyio.sleep(0.02)
            task_group.start_soon(joining_caller)
            await anyio.sleep(0.1)
            gate.set()

        assert network_backend.connects == 1
        assert network_backend.writes == 0
        assert len(pool.connections) == 1


@pytest.mark.anyio
async def test_request_arriving_during_warmup_joins_and_reuses_connection():
    """
    A real request that arrives while a warmup for its origin is in flight
    waits for the warmup and then reuses its single established connection.
    """
    gate = anyio.Event()

    class GatedBackend(CountingAsyncBackend):
        async def connect_tcp(self, *args, **kwargs):
            await gate.wait()
            return await super().connect_tcp(*args, **kwargs)

    network_backend = GatedBackend(HTTP_RESPONSE)

    async with httpcore.AsyncConnectionPool(
        network_backend=network_backend,
    ) as pool:

        async def warm() -> None:
            await pool.awarmup("https://example.com/")

        async def request() -> None:
            response = await pool.request("GET", "https://example.com/")
            assert response.status == 200
            assert response.content == b"Hello, world!"

        async with anyio.create_task_group() as task_group:
            task_group.start_soon(warm)
            await anyio.sleep(0.02)
            task_group.start_soon(request)
            await anyio.sleep(0.05)
            gate.set()

        # The warmup and the request shared one connection.
        assert network_backend.connects == 1
        assert network_backend.handshakes == 1
        assert len(pool.connections) == 1
