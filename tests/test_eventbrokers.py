from __future__ import annotations

import math
from collections.abc import AsyncGenerator
from contextlib import AsyncExitStack
from datetime import UTC, datetime
from logging import Logger

import pytest
from _pytest.logging import LogCaptureFixture
from anyio import CancelScope, create_memory_object_stream, fail_after

from apscheduler import Event, ReplayPositionExpired, ScheduleAdded
from apscheduler.abc import EventBroker
from apscheduler.eventbrokers.local import LocalEventBroker

pytestmark = pytest.mark.anyio


async def test_publish_subscribe(event_broker: EventBroker) -> None:
    send, receive = create_memory_object_stream[Event](2)
    with send, receive:
        event_broker.subscribe(send.send)
        event_broker.subscribe(send.send_nowait)
        event = ScheduleAdded(
            schedule_id="schedule1",
            task_id="task1",
            next_fire_time=datetime(2021, 9, 11, 12, 31, 56, 254867, UTC),
        )
        await event_broker.publish(event)

        with fail_after(3):
            event1 = await receive.receive()
            event2 = await receive.receive()

    assert event1 == event2
    assert isinstance(event1, ScheduleAdded)
    assert isinstance(event1.timestamp, datetime)
    assert event1.schedule_id == "schedule1"
    assert event1.task_id == "task1"
    assert event1.next_fire_time == datetime(2021, 9, 11, 12, 31, 56, 254867, UTC)


@pytest.mark.parametrize("callback_type", ["function", "callable_object"])
async def test_subscribe_coroutine_function(
    event_broker: EventBroker, callback_type: str
) -> None:
    send, receive = create_memory_object_stream[Event](1)

    async def callback(event: Event) -> None:
        await send.send(event)

    class AsyncCallable:
        async def __call__(self, event: Event) -> None:
            await send.send(event)

    with send, receive:
        if callback_type == "callable_object":
            event_broker.subscribe(AsyncCallable(), is_async=False)
        else:
            event_broker.subscribe(callback, is_async=False)
        event = Event()
        await event_broker.publish(event)

        with fail_after(10):
            assert await receive.receive() == event


async def test_subscribe_one_shot(event_broker: EventBroker) -> None:
    send, receive = create_memory_object_stream[Event](2)
    with send, receive:
        event_broker.subscribe(send.send, one_shot=True)
        event = ScheduleAdded(
            schedule_id="schedule1",
            task_id="task1",
            next_fire_time=datetime(2021, 9, 11, 12, 31, 56, 254867, UTC),
        )
        await event_broker.publish(event)
        event = ScheduleAdded(
            schedule_id="schedule2",
            task_id="task1",
            next_fire_time=datetime(2021, 9, 12, 8, 42, 11, 968481, UTC),
        )
        await event_broker.publish(event)

        with fail_after(3):
            received_event = await receive.receive()

        with pytest.raises(TimeoutError), fail_after(0.1):
            await receive.receive()

    assert isinstance(received_event, ScheduleAdded)
    assert received_event.schedule_id == "schedule1"
    assert received_event.task_id == "task1"


async def test_unsubscribe(event_broker: EventBroker) -> None:
    send, receive = create_memory_object_stream[Event]()
    with send, receive:
        subscription = event_broker.subscribe(send.send)
        await event_broker.publish(Event())
        with fail_after(3):
            await receive.receive()

        subscription.unsubscribe()
        await event_broker.publish(Event())
        with pytest.raises(TimeoutError), fail_after(0.1):
            await receive.receive()


async def test_publish_no_subscribers(
    event_broker: EventBroker, caplog: LogCaptureFixture
) -> None:
    await event_broker.publish(Event())
    assert not caplog.text


async def test_publish_exception(
    event_broker: EventBroker, caplog: LogCaptureFixture
) -> None:
    def bad_subscriber(event: Event) -> None:
        raise Exception("foo")

    timestamp = datetime.now(UTC)
    send, receive = create_memory_object_stream[Event]()
    with send, receive:
        event_broker.subscribe(bad_subscriber)
        event_broker.subscribe(send.send)
        await event_broker.publish(Event(timestamp=timestamp))

        received_event = await receive.receive()
        assert received_event.timestamp == timestamp
        assert "Error delivering Event" in caplog.text


async def test_cancel_start(raw_event_broker: EventBroker, logger: Logger) -> None:
    with CancelScope() as scope:
        scope.cancel()
        async with AsyncExitStack() as exit_stack:
            await raw_event_broker.start(exit_stack, logger)


async def test_cancel_stop(raw_event_broker: EventBroker, logger: Logger) -> None:
    with CancelScope() as scope:
        async with AsyncExitStack() as exit_stack:
            await raw_event_broker.start(exit_stack, logger)
            scope.cancel()


@pytest.fixture
async def started_local_broker(
    logger: Logger,
) -> AsyncGenerator[LocalEventBroker, None]:
    async with AsyncExitStack() as exit_stack:
        broker = LocalEventBroker(replay_buffer_size=3)
        await broker.start(exit_stack, logger)
        yield broker


async def test_local_read_events(started_local_broker: LocalEventBroker) -> None:
    broker = started_local_broker
    assert broker.current_position == 0

    events = [Event() for _ in range(3)]
    for event in events:
        await broker.publish(event)

    assert broker.current_position == 3
    position, replayed = broker.read_events(0)
    assert position == 3
    assert replayed == events

    # Reading again from the returned position yields nothing new
    position, replayed = broker.read_events(position)
    assert position == 3
    assert replayed == []


async def test_local_read_events_expired(
    started_local_broker: LocalEventBroker,
) -> None:
    broker = started_local_broker
    for _ in range(5):
        await broker.publish(Event())

    # The buffer only retains the last 3 events (positions 3-5), so positions
    # 0 and 1 have expired from the window
    with pytest.raises(ReplayPositionExpired):
        broker.read_events(0)

    with pytest.raises(ReplayPositionExpired):
        broker.read_events(1)

    # Position 2 is still covered: events 3-5 are returned
    position, replayed = broker.read_events(2)
    assert position == 5
    assert len(replayed) == 3


async def test_local_read_events_position_in_future(
    started_local_broker: LocalEventBroker,
) -> None:
    with pytest.raises(ValueError, match="ahead of the current position"):
        started_local_broker.read_events(1)


async def test_local_read_events_publish_sandwiched(
    started_local_broker: LocalEventBroker,
) -> None:
    """Publishes sandwiched between read-backs must not go unnoticed."""
    broker = started_local_broker
    event1 = Event()
    await broker.publish(event1)

    position, replayed = broker.read_events(0)
    assert replayed == [event1]

    # Two publishes happen while the receiver is processing the first batch
    event2, event3 = Event(), Event()
    await broker.publish(event2)
    await broker.publish(event3)

    # The follow-up read returns exactly the sandwiched events, in order
    position, replayed = broker.read_events(position)
    assert position == 3
    assert replayed == [event2, event3]

    # If the window overflows before the next read, the gap is detectable
    for _ in range(4):
        await broker.publish(Event())

    with pytest.raises(ReplayPositionExpired):
        broker.read_events(position)


async def test_local_stream_events(started_local_broker: LocalEventBroker) -> None:
    """Catch-up and live events must be handed off in order, without duplicates."""
    broker = started_local_broker
    event1 = Event()
    await broker.publish(event1)
    token = broker.current_position

    # Buffer the live events delivered through a regular subscription
    send, receive = create_memory_object_stream[Event](math.inf)
    with send, receive:
        broker.subscribe(send.send_nowait)

        received: list[Event] = []
        stream = broker.stream_events(since=token)
        # Two publishes sandwiched into the catch-up phase: they are in the
        # replay window and also delivered to the live subscription
        event2, event3 = Event(), Event()
        await broker.publish(event2)
        await broker.publish(event3)

        async for event in stream:
            received.append(event)
            if len(received) == 2:
                break

        await stream.aclose()

        # The stream yields the sandwiched events exactly once, in order
        assert received == [event2, event3]

        # The regular subscription received the two events independently
        with fail_after(3):
            assert [await receive.receive() for _ in range(2)] == [event2, event3]


async def test_local_stream_events_expired(
    started_local_broker: LocalEventBroker,
) -> None:
    broker = started_local_broker
    for _ in range(4):
        await broker.publish(Event())

    with pytest.raises(ReplayPositionExpired):
        broker.stream_events(since=0)


async def test_local_stream_events_live_only(
    started_local_broker: LocalEventBroker,
) -> None:
    broker = started_local_broker
    await broker.publish(Event())

    stream = broker.stream_events()
    event = Event()
    await broker.publish(event)
    with fail_after(3):
        assert await stream.__anext__() is event

    await stream.aclose()


async def test_local_replay_disabled(logger: Logger) -> None:
    async with AsyncExitStack() as exit_stack:
        broker = LocalEventBroker(replay_buffer_size=0)
        await broker.start(exit_stack, logger)
        await broker.publish(Event())

        position, replayed = broker.read_events(broker.current_position)
        assert position == 1
        assert replayed == []

        with pytest.raises(ReplayPositionExpired):
            broker.read_events(0)


def test_asyncpg_broker_from_async_engine() -> None:
    pytest.importorskip("asyncpg", reason="asyncpg is not installed")
    from sqlalchemy import URL
    from sqlalchemy.ext.asyncio import create_async_engine

    from apscheduler.eventbrokers.asyncpg import AsyncpgEventBroker

    url = URL(
        "postgresql+asyncpg",
        "myuser",
        "c /%@",
        "localhost",
        7654,
        "dbname",
        {"opt1": "foo", "opt2": "bar"},
    )
    engine = create_async_engine(url)
    broker = AsyncpgEventBroker.from_async_sqla_engine(engine)
    assert isinstance(broker, AsyncpgEventBroker)
    assert broker.dsn == (
        "postgresql://myuser:c %2F%25%40@localhost:7654/dbname?opt1=foo&opt2=bar"
    )


def test_psycopg_broker_from_async_engine() -> None:
    pytest.importorskip(
        "psycopg", exc_type=ImportError, reason="psycopg is not installed"
    )
    from sqlalchemy import URL
    from sqlalchemy.ext.asyncio import create_async_engine

    from apscheduler.eventbrokers.psycopg import PsycopgEventBroker

    url = URL(
        "postgresql+psycopg",
        "myuser",
        "c /%@",
        "localhost",
        7654,
        "dbname",
        {"opt1": "foo", "opt2": "bar"},
    )
    engine = create_async_engine(url)
    broker = PsycopgEventBroker.from_async_sqla_engine(engine)
    assert isinstance(broker, PsycopgEventBroker)
    assert broker.conninfo == (
        "postgresql://myuser:c %2F%25%40@localhost:7654/dbname?opt1=foo&opt2=bar"
    )
