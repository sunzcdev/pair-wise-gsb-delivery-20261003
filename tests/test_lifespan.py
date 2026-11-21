import asyncio
import logging

import pytest

from uvicorn.config import Config
from uvicorn.lifespan.off import LifespanOff
from uvicorn.lifespan.on import LifespanOn


def test_lifespan_on():
    startup_complete = False
    shutdown_complete = False

    async def app(scope, receive, send):
        nonlocal startup_complete, shutdown_complete
        message = await receive()
        assert message["type"] == "lifespan.startup"
        startup_complete = True
        await send({"type": "lifespan.startup.complete"})
        message = await receive()
        assert message["type"] == "lifespan.shutdown"
        shutdown_complete = True
        await send({"type": "lifespan.shutdown.complete"})

    async def test():
        config = Config(app=app, lifespan="on")
        lifespan = LifespanOn(config)

        assert not startup_complete
        assert not shutdown_complete
        await lifespan.startup()
        assert startup_complete
        assert not shutdown_complete
        await lifespan.shutdown()
        assert startup_complete
        assert shutdown_complete

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


def test_lifespan_off():
    async def app(scope, receive, send):
        pass  # pragma: no cover

    async def test():
        config = Config(app=app, lifespan="off")
        lifespan = LifespanOff(config)

        await lifespan.startup()
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


def test_lifespan_auto():
    startup_complete = False
    shutdown_complete = False

    async def app(scope, receive, send):
        nonlocal startup_complete, shutdown_complete
        message = await receive()
        assert message["type"] == "lifespan.startup"
        startup_complete = True
        await send({"type": "lifespan.startup.complete"})
        message = await receive()
        assert message["type"] == "lifespan.shutdown"
        shutdown_complete = True
        await send({"type": "lifespan.shutdown.complete"})

    async def test():
        config = Config(app=app, lifespan="auto")
        lifespan = LifespanOn(config)

        assert not startup_complete
        assert not shutdown_complete
        await lifespan.startup()
        assert startup_complete
        assert not shutdown_complete
        await lifespan.shutdown()
        assert startup_complete
        assert shutdown_complete

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


def test_lifespan_auto_with_error():
    async def app(scope, receive, send):
        assert scope["type"] == "http"

    async def test():
        config = Config(app=app, lifespan="auto")
        lifespan = LifespanOn(config)

        await lifespan.startup()
        assert lifespan.error_occurred
        assert not lifespan.should_exit
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


def test_lifespan_on_with_error():
    async def app(scope, receive, send):
        if scope["type"] != "http":
            raise RuntimeError()

    async def test():
        config = Config(app=app, lifespan="on")
        lifespan = LifespanOn(config)

        await lifespan.startup()
        assert lifespan.error_occurred
        assert lifespan.should_exit
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


@pytest.mark.parametrize("mode", ("auto", "on"))
@pytest.mark.parametrize("raise_exception", (True, False))
def test_lifespan_with_failed_startup(mode, raise_exception, caplog):
    async def app(scope, receive, send):
        message = await receive()
        assert message["type"] == "lifespan.startup"
        await send({"type": "lifespan.startup.failed", "message": "the lifespan event failed"})
        if raise_exception:
            # App should be able to re-raise an exception if startup failed.
            raise RuntimeError()

    async def test():
        config = Config(app=app, lifespan=mode)
        lifespan = LifespanOn(config)

        await lifespan.startup()
        assert lifespan.startup_failed
        assert lifespan.error_occurred is raise_exception
        assert lifespan.should_exit
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()
    error_messages = [
        record.message for record in caplog.records if record.name == "uvicorn.error" and record.levelname == "ERROR"
    ]
    assert "the lifespan event failed" in error_messages.pop(0)
    assert "Application startup failed. Exiting." in error_messages.pop(0)


def test_lifespan_scope_asgi3app():
    async def asgi3app(scope, receive, send):
        assert scope == {
            "type": "lifespan",
            "asgi": {"version": "3.0", "spec_version": "2.0"},
            "state": {},
        }

    async def test():
        config = Config(app=asgi3app, lifespan="on")
        lifespan = LifespanOn(config)

        await lifespan.startup()
        assert not lifespan.startup_failed
        assert not lifespan.error_occurred
        assert not lifespan.should_exit
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


def test_lifespan_scope_asgi2app():
    def asgi2app(scope):
        assert scope == {
            "type": "lifespan",
            "asgi": {"version": "2.0", "spec_version": "2.0"},
            "state": {},
        }

        async def asgi(receive, send):
            pass

        return asgi

    async def test():
        config = Config(app=asgi2app, lifespan="on")
        lifespan = LifespanOn(config)

        await lifespan.startup()
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


@pytest.mark.parametrize("mode", ("auto", "on"))
@pytest.mark.parametrize("raise_exception", (True, False))
def test_lifespan_with_failed_shutdown(mode, raise_exception, caplog):
    async def app(scope, receive, send):
        message = await receive()
        assert message["type"] == "lifespan.startup"
        await send({"type": "lifespan.startup.complete"})
        message = await receive()
        assert message["type"] == "lifespan.shutdown"
        await send({"type": "lifespan.shutdown.failed", "message": "the lifespan event failed"})

        if raise_exception:
            # App should be able to re-raise an exception if startup failed.
            raise RuntimeError()

    async def test():
        config = Config(app=app, lifespan=mode)
        lifespan = LifespanOn(config)

        await lifespan.startup()
        assert not lifespan.startup_failed
        await lifespan.shutdown()
        assert lifespan.shutdown_failed
        assert lifespan.error_occurred is raise_exception
        assert lifespan.should_exit

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    error_messages = [
        record.message for record in caplog.records if record.name == "uvicorn.error" and record.levelname == "ERROR"
    ]
    assert "the lifespan event failed" in error_messages.pop(0)
    assert "Application shutdown failed. Exiting." in error_messages.pop(0)
    loop.close()


def test_lifespan_state():
    async def app(scope, receive, send):
        message = await receive()
        assert message["type"] == "lifespan.startup"
        await send({"type": "lifespan.startup.complete"})
        scope["state"]["foo"] = 123
        message = await receive()
        assert message["type"] == "lifespan.shutdown"
        await send({"type": "lifespan.shutdown.complete"})

    async def test():
        config = Config(app=app, lifespan="on")
        lifespan = LifespanOn(config)

        await lifespan.startup()
        assert lifespan.state == {"foo": 123}
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


async def _run_lifespan(app, *, lifespan_mode, timeout):
    config = Config(app=app, lifespan=lifespan_mode, timeout_lifespan_startup=timeout)
    lifespan = LifespanOn(config)
    await lifespan.startup()
    return lifespan


def test_lifespan_startup_timeout_with_unresponsive_app(caplog):
    """An app that takes the startup event but never replies fails startup.

    The failure must be reported under ``lifespan="auto"`` as well, instead of
    being mistaken for an application that does not implement the lifespan.
    """
    startup_received = False
    cancelled = False

    async def app(scope, receive, send):
        nonlocal startup_received, cancelled
        if scope["type"] != "lifespan":
            return
        message = await receive()
        assert message["type"] == "lifespan.startup"
        startup_received = True
        try:
            # Block forever without ever sending lifespan.startup.complete.
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            cancelled = True
            raise

    async def test():
        lifespan = await _run_lifespan(app, lifespan_mode="auto", timeout=0.1)
        assert startup_received
        assert lifespan.startup_timeout
        assert lifespan.startup_failed
        assert lifespan.should_exit
        # The application task must have received an explicit cancellation.
        assert cancelled
        assert lifespan.main_lifespan_task.cancelled()
        # The timeout cleanup already owns shutdown, so shutdown must not enter
        # the shutdown phase a second time.
        await lifespan.shutdown()
        assert lifespan.shutdown_started

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()

    error_messages = [
        record.message for record in caplog.records if record.name == "uvicorn.error" and record.levelname == "ERROR"
    ]
    assert any("startup exceeded the configured timeout" in message for message in error_messages)
    assert "Application startup failed. Exiting." not in error_messages
    # An unresponsive but lifespan-aware app must not be reported as unsupported.
    assert not any("lifespan' protocol appears unsupported" in message for message in error_messages)


def test_lifespan_startup_timeout_with_immediate_exit(caplog):
    """A legacy app that rejects the lifespan scope is still handled by auto mode.

    Such an application raises instead of consuming the startup event. It must be
    reported as unsupported (so the server keeps running) rather than failing with
    a startup timeout, even when a timeout is configured.
    """

    async def app(scope, receive, send):
        # A legacy application that does not implement the lifespan raises as
        # soon as it is handed a lifespan scope, before consuming any event.
        assert scope["type"] == "http"

    async def test():
        lifespan = await _run_lifespan(app, lifespan_mode="auto", timeout=0.1)
        assert lifespan.error_occurred
        assert not lifespan.startup_timeout
        assert not lifespan.startup_failed
        assert not lifespan.should_exit
        await lifespan.shutdown()

    caplog.set_level(logging.INFO, logger="uvicorn.error")
    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()

    info_messages = [
        record.message for record in caplog.records if record.name == "uvicorn.error" and record.levelname == "INFO"
    ]
    assert any("lifespan' protocol appears unsupported" in message for message in info_messages)


def test_lifespan_startup_timeout_with_timely_reply():
    """An app that replies within the timeout starts up and shuts down normally."""
    startup_complete = False
    shutdown_complete = False

    async def app(scope, receive, send):
        nonlocal startup_complete, shutdown_complete
        if scope["type"] != "lifespan":
            return
        message = await receive()
        assert message["type"] == "lifespan.startup"
        await asyncio.sleep(0.01)
        await send({"type": "lifespan.startup.complete"})
        startup_complete = True
        message = await receive()
        assert message["type"] == "lifespan.shutdown"
        await send({"type": "lifespan.shutdown.complete"})
        shutdown_complete = True

    async def test():
        lifespan = await _run_lifespan(app, lifespan_mode="on", timeout=5)
        assert startup_complete
        assert not lifespan.startup_timeout
        assert not lifespan.startup_failed
        assert not lifespan.should_exit
        await lifespan.shutdown()
        assert shutdown_complete

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


def test_lifespan_startup_timeout_reports_failure_in_on_mode(caplog):
    """Under ``lifespan="on"`` the unresponsive app also fails startup."""

    async def app(scope, receive, send):
        if scope["type"] != "lifespan":
            return
        await receive()
        await asyncio.Event().wait()

    async def test():
        lifespan = await _run_lifespan(app, lifespan_mode="on", timeout=0.1)
        assert lifespan.startup_failed
        assert lifespan.should_exit
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()

    error_messages = [
        record.message for record in caplog.records if record.name == "uvicorn.error" and record.levelname == "ERROR"
    ]
    assert any("startup exceeded the configured timeout" in message for message in error_messages)


def test_lifespan_without_startup_timeout_keeps_waiting_forever():
    """With the default ``None`` timeout an unresponsive app keeps waiting.

    No timeout is armed, so the startup must remain pending for as long as the
    application holds off on replying, and only complete once it does reply.
    """
    release_app = asyncio.Event()

    async def app(scope, receive, send):
        if scope["type"] != "lifespan":
            return
        message = await receive()
        assert message["type"] == "lifespan.startup"
        await release_app.wait()
        await send({"type": "lifespan.startup.complete"})
        message = await receive()
        assert message["type"] == "lifespan.shutdown"
        await send({"type": "lifespan.shutdown.complete"})

    async def test():
        config = Config(app=app, lifespan="on")
        assert config.timeout_lifespan_startup is None
        lifespan = LifespanOn(config)

        startup_task = asyncio.create_task(lifespan.startup())
        # Let the startup begin and make sure it never completes on its own.
        await asyncio.sleep(0.1)
        assert not startup_task.done()

        # Let the application reply: the startup then completes normally.
        release_app.set()
        await asyncio.wait_for(startup_task, timeout=1)
        assert not lifespan.should_exit
        await lifespan.shutdown()

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()


def test_lifespan_shutdown_is_not_entered_twice():
    """Repeated shutdown calls after a timeout never send a second shutdown event."""

    async def app(scope, receive, send):
        if scope["type"] != "lifespan":
            return
        await receive()
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            raise

    async def test():
        lifespan = await _run_lifespan(app, lifespan_mode="on", timeout=0.1)
        assert lifespan.shutdown_started
        queue_size_before = lifespan.receive_queue.qsize()
        await lifespan.shutdown()
        await lifespan.shutdown()
        # No extra shutdown event may have been queued by the guard.
        assert lifespan.receive_queue.qsize() == queue_size_before

    loop = asyncio.new_event_loop()
    loop.run_until_complete(test())
    loop.close()
