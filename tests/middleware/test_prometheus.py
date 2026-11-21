from __future__ import annotations

import time
import urllib.request as request
from threading import Event, Thread

import dramatiq
from dramatiq import Worker
from dramatiq.middleware.prometheus import Prometheus, _run_exposition_server


def test_prometheus_middleware_exposes_metrics():
    # Given an instance of the exposition server
    thread = Thread(target=_run_exposition_server, daemon=True)
    thread.start()

    # When I give it time to boot up
    time.sleep(1)

    # And I request metrics via HTTP
    with request.urlopen("http://127.0.0.1:9191") as resp:
        # Then the response should be successful
        assert resp.getcode() == 200


def test_prometheus_middleware_tracks_the_local_work_queue(stub_broker):
    # Given a broker with the Prometheus middleware
    middleware = Prometheus()
    stub_broker.add_middleware(middleware)
    middleware.after_process_boot(stub_broker)

    started = Event()
    release = Event()

    @dramatiq.actor
    def block():
        started.set()
        release.wait(30)

    # And a single-threaded worker that claims messages faster than it can execute them
    worker = Worker(stub_broker, worker_threads=1, worker_timeout=10)
    worker.queue_prefetch = 8
    worker.start()
    labels = ("default", "block")
    try:
        # When one message is stuck executing inside the actor
        block.send()
        assert started.wait(10)

        # And a few more messages have been claimed into the worker's local work queue
        block.send()
        block.send()
        block.send()

        for _ in range(100):
            if middleware.inprogress_work_queue_messages.labels(*labels)._value.get() == 3:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("messages were not claimed into the local work queue")

        # Then the unstarted messages should be aggregated on the local-queue gauge
        assert middleware.inprogress_work_queue_messages.labels(*labels)._value.get() == 3
        # And the executing message should still be tracked by the existing in-progress metric
        assert middleware.inprogress_messages.labels(*labels)._value.get() == 1
        # And its local wait time should have been measured from the moment it entered the work queue
        wait_histogram = middleware.work_queue_wait_durations.labels(*labels)
        assert sum(bucket.get() for bucket in wait_histogram._buckets) == 1
    finally:
        # When the worker shuts down while the actor is still blocked
        worker.stop(timeout=1000)
        release.set()

    # Then the leftover messages should have been handed back to the broker
    assert middleware.inprogress_work_queue_messages.labels(*labels)._value.get() == 0
    assert middleware.total_requeued_work_queue_messages.labels(*labels)._value.get() == 3
