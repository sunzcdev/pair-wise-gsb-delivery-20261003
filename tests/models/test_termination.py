import typing

import pytest

import httpx


def streaming_body() -> typing.Iterator[bytes]:
    yield b"Hello, "
    yield b"world!"


async def async_streaming_body() -> typing.AsyncIterator[bytes]:
    yield b"Hello, "
    yield b"world!"


class FailingSyncStream(httpx.SyncByteStream):
    def __iter__(self) -> typing.Iterator[bytes]:
        yield b"partial"
        raise httpx.ReadError("boom")

    def close(self) -> None:
        pass


class FailingAsyncStream(httpx.AsyncByteStream):
    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        yield b"partial"
        raise httpx.ReadError("boom")

    async def aclose(self) -> None:
        pass


class MalformedGzipSyncStream(httpx.SyncByteStream):
    def __iter__(self) -> typing.Iterator[bytes]:
        yield b"not really gzip"

    def close(self) -> None:
        pass


def test_termination_reason_is_none_before_settling():
    response = httpx.Response(200, content=streaming_body())
    assert response.termination_reason is None


def test_termination_hook_called_on_complete():
    events = []
    response = httpx.Response(200, content=streaming_body())
    response.add_termination_hook(lambda **kwargs: events.append(kwargs))

    response.read()

    assert len(events) == 1
    assert events[0]["response"] is response
    assert events[0]["reason"] == httpx.ResponseTerminationReason.COMPLETE
    assert events[0]["num_bytes"] == 13
    assert events[0]["exception"] is None
    assert response.termination_reason == "complete"


def test_termination_hook_called_for_empty_body():
    events = []
    response = httpx.Response(200)
    response.add_termination_hook(lambda **kwargs: events.append(kwargs))

    assert len(events) == 1
    assert events[0]["reason"] == httpx.ResponseTerminationReason.COMPLETE
    assert events[0]["num_bytes"] == 0


def test_termination_hook_fires_immediately_when_already_settled():
    events = []
    response = httpx.Response(200, content=b"abc")

    response.add_termination_hook(lambda **kwargs: events.append(kwargs))

    assert len(events) == 1
    assert events[0]["reason"] == "complete"
    assert events[0]["num_bytes"] == 3


def test_termination_hook_only_called_once_when_read_then_close():
    events = []
    response = httpx.Response(200, content=streaming_body())
    response.add_termination_hook(lambda **kwargs: events.append(kwargs))

    response.read()
    response.close()

    assert len(events) == 1


def test_termination_hook_called_on_early_close():
    events = []
    response = httpx.Response(200, content=streaming_body())
    response.add_termination_hook(lambda **kwargs: events.append(kwargs))

    response.close()

    assert len(events) == 1
    assert events[0]["reason"] == httpx.ResponseTerminationReason.CLOSED_EARLY
    assert events[0]["num_bytes"] == 0


def test_termination_hook_called_on_early_close_after_partial_read():
    events = []
    response = httpx.Response(200, content=streaming_body())
    response.add_termination_hook(lambda **kwargs: events.append(kwargs))

    next(response.iter_raw())
    response.close()

    assert len(events) == 1
    assert events[0]["reason"] == httpx.ResponseTerminationReason.CLOSED_EARLY
    assert events[0]["num_bytes"] == 7


def test_termination_hook_called_on_read_failure():
    events = []
    response = httpx.Response(200, stream=FailingSyncStream())
    response.add_termination_hook(lambda **kwargs: events.append(kwargs))

    with pytest.raises(httpx.ReadError):
        response.read()

    assert len(events) == 1
    assert events[0]["reason"] == httpx.ResponseTerminationReason.FAILED
    assert isinstance(events[0]["exception"], httpx.ReadError)
    assert events[0]["num_bytes"] == 7
    assert response.termination_reason == "failed"


def test_termination_hook_preserves_decoding_error():
    events = []
    response = httpx.Response(
        200,
        headers={"content-encoding": "gzip"},
        stream=MalformedGzipSyncStream(),
    )
    response.add_termination_hook(lambda **kwargs: events.append(kwargs))

    with pytest.raises(httpx.DecodingError):
        response.read()

    assert len(events) == 1
    assert events[0]["reason"] == httpx.ResponseTerminationReason.FAILED
    assert isinstance(events[0]["exception"], httpx.DecodingError)


def test_termination_hook_exception_propagates():
    def failing_hook(**kwargs: typing.Any) -> None:
        raise ValueError("hook failure")

    response = httpx.Response(200, content=streaming_body())
    response.add_termination_hook(failing_hook)

    with pytest.raises(ValueError, match="hook failure"):
        response.read()


@pytest.mark.anyio
async def test_async_termination_hook_called_on_complete():
    events = []

    async def hook(**kwargs: typing.Any) -> None:
        events.append(kwargs)

    response = httpx.Response(200, content=async_streaming_body())
    await response.aadd_termination_hook(hook)

    await response.aread()

    assert len(events) == 1
    assert events[0]["reason"] == "complete"
    assert events[0]["num_bytes"] == 13


@pytest.mark.anyio
async def test_async_termination_hook_called_on_early_close():
    events = []

    async def hook(**kwargs: typing.Any) -> None:
        events.append(kwargs)

    response = httpx.Response(200, content=async_streaming_body())
    await response.aadd_termination_hook(hook)

    await response.aclose()

    assert len(events) == 1
    assert events[0]["reason"] == "closed_early"


@pytest.mark.anyio
async def test_async_termination_hook_called_on_failure():
    events = []

    async def hook(**kwargs: typing.Any) -> None:
        events.append(kwargs)

    response = httpx.Response(200, stream=FailingAsyncStream())
    await response.aadd_termination_hook(hook)

    with pytest.raises(httpx.ReadError):
        await response.aread()

    assert len(events) == 1
    assert events[0]["reason"] == "failed"
    assert isinstance(events[0]["exception"], httpx.ReadError)
    assert events[0]["num_bytes"] == 7
