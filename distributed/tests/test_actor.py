from __future__ import annotations

import asyncio
import operator
import sys
import threading
from time import sleep

import pytest

import dask

from distributed import (
    Actor,
    BaseActorFuture,
    Client,
    Event,
    Future,
    Nanny,
    as_completed,
    get_client,
    wait,
    worker_client,
)
from distributed.metrics import time
from distributed.utils import LateLoopEvent
from distributed.utils_test import (
    async_poll_for,
    cluster,
    double,
    gen_cluster,
    inc,
)
from distributed.worker import Worker, get_worker


class Counter:
    n = 0

    def __init__(self):
        self.n = 0

    def increment(self):
        self.n += 1
        return self.n

    async def ainc(self):
        self.n += 1
        return self.n

    def add(self, x):
        self.n += x
        return self.n


class List:
    L: list = []

    def __init__(self, dummy=None):
        self.L = []

    def append(self, x):
        self.L.append(x)


class ParameterServer:
    def __init__(self):
        self.data = {}

    def put(self, key, value):
        self.data[key] = value

    def get(self, key):
        return self.data[key]


@pytest.mark.parametrize("direct_to_workers", [True, False])
@gen_cluster()
async def test_client_actions(s, a, b, direct_to_workers):
    async with Client(
        s.address, asynchronous=True, direct_to_workers=direct_to_workers
    ) as c:
        counter = c.submit(Counter, workers=[a.address], actor=True)
        assert isinstance(counter, Future)
        counter = await counter
        assert counter._address
        assert hasattr(counter, "increment")
        assert hasattr(counter, "add")

        assert await counter.n == 0

        assert counter._address == a.address

        assert isinstance(a.state.actors[counter.key], Counter)
        assert s.tasks[counter.key].actor

        await asyncio.gather(counter.increment(), counter.increment())

        assert await counter.n == 2

        counter.add(10)
        while (await counter.n) != 10 + 2:
            await asyncio.sleep(0.01)


@pytest.mark.parametrize("separate_thread", [False, True])
@gen_cluster(client=True)
async def test_worker_actions(c, s, a, b, separate_thread):
    counter = c.submit(Counter, workers=[a.address], actor=True)
    a_address = a.address

    def f(counter):
        start = counter.n

        assert type(counter) is Actor
        assert counter._address == a_address

        future = counter.increment(separate_thread=separate_thread)
        assert isinstance(future, BaseActorFuture)
        assert "Future" in type(future).__name__
        end = future.result(timeout=1)
        assert end > start

    futures = [c.submit(f, counter, pure=False) for _ in range(10)]
    await c.gather(futures)

    counter = await counter
    assert await counter.n == 10


@gen_cluster(client=True)
async def test_Actor(c, s, a, b):
    counter = await c.submit(Counter, actor=True)

    assert counter._cls == Counter

    assert await counter.n == 0
    assert hasattr(counter, "increment")
    assert hasattr(counter, "add")

    assert not hasattr(counter, "abc")


@pytest.mark.xfail(
    reason="Tornado can pass things out of order"
    + "Should rely on sending small messages rather than rpc"
)
@gen_cluster(client=True)
async def test_linear_access(c, s, a, b):
    start = time()
    future = c.submit(sleep, 0.2)
    actor = c.submit(List, actor=True, dummy=future)
    actor = await actor

    for i in range(100):
        actor.append(i)

    while True:
        await asyncio.sleep(0.1)
        L = await actor.L
        if len(L) == 100:
            break

    L = await actor.L
    stop = time()
    assert L == tuple(range(100))

    assert stop - start > 0.2


@gen_cluster(client=True)
async def test_exceptions_create(c, s, a, b):
    class Foo:
        x = 0

        def __init__(self):
            raise ValueError("bar")

    with pytest.raises(ValueError) as info:
        await c.submit(Foo, actor=True)

    assert "bar" in str(info.value)


@gen_cluster(client=True)
async def test_exceptions_method(c, s, a, b):
    class Foo:
        def throw(self):
            1 / 0

    foo = await c.submit(Foo, actor=True)
    with pytest.raises(ZeroDivisionError):
        await foo.throw()


@gen_cluster(client=True)
async def test_gc(c, s, a, b):
    actor = c.submit(Counter, actor=True)
    await wait(actor)
    del actor

    while a.state.actors or b.state.actors:
        await asyncio.sleep(0.01)


@gen_cluster(client=True)
async def test_track_dependencies(c, s, a, b):
    actor = c.submit(Counter, actor=True)
    await wait(actor)
    x = c.submit(sleep, 0.5)
    y = c.submit(lambda x, y: x, x, actor)
    del actor

    await asyncio.sleep(0.3)

    assert a.state.actors or b.state.actors


@gen_cluster(client=True)
async def test_future(c, s, a, b):
    counter = c.submit(Counter, actor=True, workers=[a.address])
    assert isinstance(counter, Future)
    await wait(counter)
    assert isinstance(a.state.actors[counter.key], Counter)

    counter = await counter
    assert isinstance(counter, Actor)
    assert counter._address

    await asyncio.sleep(0.1)
    assert counter.key in c.futures  # don't lose future


@gen_cluster(client=True)
async def test_future_dependencies(c, s, a, b):
    counter = c.submit(Counter, actor=True, workers=[a.address])

    def f(a):
        assert isinstance(a, Actor)
        assert a._cls == Counter

    x = c.submit(f, counter, workers=[b.address])
    await x

    assert {ts.key for ts in s.tasks[x.key].dependencies} == {counter.key}
    assert {ts.key for ts in s.tasks[counter.key].dependents} == {x.key}

    y = c.submit(f, counter, workers=[a.address], pure=False)
    await y

    assert {ts.key for ts in s.tasks[y.key].dependencies} == {counter.key}
    assert {ts.key for ts in s.tasks[counter.key].dependents} == {x.key, y.key}


def test_sync(client):
    counter = client.submit(Counter, actor=True)
    counter = counter.result()

    assert counter.n == 0

    future = counter.increment()
    n = future.result()
    assert n == 1
    assert counter.n == 1

    assert future.result() == future.result()

    assert "ActorFuture" in repr(future)
    assert "distributed.actor" not in repr(future)


def test_timeout(client):
    class Waiter:
        def __init__(self):
            self.event = LateLoopEvent()

        async def set(self):
            self.event.set()

        async def wait(self):
            return await self.event.wait()

    event = client.submit(Waiter, actor=True).result()
    future = event.wait()

    with pytest.raises(asyncio.TimeoutError):
        future.result(timeout="0.001s")

    event.set().result()
    assert future.result() is True


@gen_cluster(client=True)
async def test_failed_worker(c, s, a, b):
    counter = await c.submit(Counter, actor=True, workers=[a.address])
    assert counter._future.status == "finished"
    await a.close()

    # Avoid race condition tested below in test_failed_worker_before_future_update
    while counter._future.status == "finished":
        await asyncio.sleep(0.01)

    with pytest.raises(RuntimeError, match="Worker holding Actor was lost"):
        await counter.increment()


@gen_cluster(client=True, config={"distributed.comm.timeouts.connect": "1s"})
async def test_failed_worker_before_future_update(c, s, a, b):
    """Test race condition where the worker failed but the information
    has not yet reached the Client
    """
    counter = await c.submit(Counter, actor=True, workers=[a.address])
    assert counter._future.status == "finished"
    close_fut = asyncio.Task(a.close())

    # Wait until the worker has already shut down its RPC channels, but
    # the client has definitely not been notified yet.
    while a.address in s.workers:
        await asyncio.sleep(0)
    assert counter._future.status == "finished"

    with pytest.raises(OSError, match="Unable to contact Actor's worker"):
        await counter.increment()

    # Wait for the scheduler to send back handshakes to the worker.
    # In parallel, the scheduler sends the notification to the client, which
    # may or may not have arrived by the time Worker.close() finishes.
    await close_fut


@gen_cluster(client=True)
async def bench(c, s, a, b):
    counter = await c.submit(Counter, actor=True)

    for _ in range(1000):
        await counter.increment()


@gen_cluster(client=True)
async def test_numpy_roundtrip(c, s, a, b):
    np = pytest.importorskip("numpy")

    server = await c.submit(ParameterServer, actor=True)

    x = np.random.random(1000)
    await server.put("x", x)

    y = await server.get("x")

    assert (x == y).all()


@gen_cluster(client=True)
async def test_numpy_roundtrip_getattr(c, s, a, b):
    np = pytest.importorskip("numpy")

    counter = await c.submit(Counter, actor=True)

    x = np.random.random(1000)

    await counter.add(x)

    y = await counter.n

    assert (x == y).all()


@gen_cluster(client=True)
async def test_repr(c, s, a, b):
    counter = await c.submit(Counter, actor=True)

    assert "Counter" in repr(counter)
    assert "Actor" in repr(counter)
    assert counter.key in repr(counter)
    assert "distributed.actor" not in repr(counter)


@gen_cluster(client=True)
async def test_dir(c, s, a, b):
    counter = await c.submit(Counter, actor=True)

    d = set(dir(counter))

    for attr in dir(Counter):
        if not attr.startswith("_"):
            assert attr in d


@gen_cluster(client=True)
async def test_many_computations(c, s, a, b):
    counter = await c.submit(Counter, actor=True)

    def add(n, counter):
        for _ in range(n):
            counter.increment().result()

    futures = c.map(add, range(10), counter=counter)
    done = c.submit(lambda x: None, futures)

    while not done.done():
        assert (
            len([ws for ws in s.workers.values() if ws.processing])
            <= a.state.nthreads + b.state.nthreads
        )
        await asyncio.sleep(0.01)

    await done


@gen_cluster(client=True, nthreads=[("127.0.0.1", 5)] * 2)
async def test_thread_safety(c, s, a, b):
    class Unsafe:
        def __init__(self):
            self.n = 0

        def f(self):
            assert self.n == 0
            self.n += 1

            for _ in range(20):
                sleep(0.002)
                assert self.n == 1
            self.n = 0

    unsafe = await c.submit(Unsafe, actor=True)

    futures = [unsafe.f() for i in range(10)]
    await c.gather(futures)


@gen_cluster(client=True)
async def test_Actors_create_dependencies(c, s, a, b):
    counter = await c.submit(Counter, actor=True)
    future = c.submit(lambda x: None, counter)
    await wait(future)
    assert s.tasks[future.key].dependencies == {s.tasks[counter.key]}


@gen_cluster(client=True)
async def test_load_balance(c, s, a, b):
    class Foo:
        def __init__(self, x):
            pass

    b = c.submit(operator.mul, "b", 1000000)
    await wait(b)
    [ws] = s.tasks[b.key].who_has

    x = await c.submit(Foo, b, actor=True)
    y = await c.submit(Foo, b, actor=True)
    assert x.key != y.key  # actors assumed not pure

    assert s.tasks[x.key].who_has == {ws}  # first went to best match
    assert s.tasks[x.key].who_has != s.tasks[y.key].who_has  # second load balanced


@gen_cluster(client=True, nthreads=[("127.0.0.1", 1)] * 5)
async def test_load_balance_map(c, s, *workers):
    class Foo:
        def __init__(self, x, y=None):
            pass

    b = c.submit(operator.mul, "b", 1000000)
    await wait(b)

    actors = c.map(Foo, range(10), y=b, actor=True)
    await wait(actors)

    assert all(len(w.state.actors) == 2 for w in workers)


@gen_cluster(client=True, nthreads=[("127.0.0.1", 1)] * 4, Worker=Nanny)
async def bench_param_server(c, s, *workers):
    np = pytest.importorskip("numpy")
    import dask.array as da

    x = da.random.random((500000, 1000), chunks=(1000, 1000))
    x = x.persist()
    await wait(x)

    class ParameterServer:
        data = None

        def __init__(self, n):
            self.data = np.random.random(n)

        def update(self, x):
            self.data += x
            self.data /= 2

        def get_data(self):
            return self.data

    def f(block, ps=None):
        start = time()
        params = ps.get_data(separate_thread=False).result()
        stop = time()
        update = (block - params).mean(axis=0)
        ps.update(update, separate_thread=False)
        print(format_time(stop - start))
        return np.array([[stop - start]])

    from distributed.utils import format_time

    start = time()
    ps = await c.submit(ParameterServer, x.shape[1], actor=True)
    y = x.map_blocks(f, ps=ps, dtype=x.dtype)
    # result = await c.compute(y.mean())
    await wait(y.persist())
    end = time()
    print(format_time(end - start))


@gen_cluster(client=True)
async def test_compute(c, s, a, b):
    @dask.delayed
    def f(n, counter):
        assert isinstance(counter, Actor)
        for _ in range(n):
            counter.increment().result()

    @dask.delayed
    def check(counter, blanks):
        return counter.n

    counter = dask.delayed(Counter)()
    values = [f(i, counter) for i in range(5)]
    final = check(counter, values)

    result = await c.compute(final, actors=counter)
    assert result == 0 + 1 + 2 + 3 + 4

    while a.data or b.data:
        await asyncio.sleep(0.01)


def test_compute_sync(client):
    @dask.delayed
    def f(n, counter):
        assert isinstance(counter, Actor), type(counter)
        for _ in range(n):
            counter.increment().result()

    @dask.delayed
    def check(counter, blanks):
        return counter.n

    counter = dask.delayed(Counter)()
    values = [f(i, counter) for i in range(5)]
    final = check(counter, values)

    result = final.compute(actors=counter)
    assert result == 0 + 1 + 2 + 3 + 4

    def check(dask_worker):
        return len(dask_worker.data) + len(dask_worker.state.actors)

    start = time()
    while any(client.run(check).values()):
        sleep(0.01)
        assert time() < start + 30


@pytest.mark.skipif(sys.version_info.minor == 11, reason="Profiler disabled")
@gen_cluster(
    client=True,
    nthreads=[("127.0.0.1", 1)],
    config={
        "distributed.worker.profile.enabled": True,
        "distributed.worker.profile.interval": "1ms",
    },
)
async def test_actors_in_profile(c, s, a):
    class Sleeper:
        def sleep(self, time):
            sleep(time)

    sleeper = await c.submit(Sleeper, actor=True)

    for _ in range(5):
        await sleeper.sleep(0.200)
        if (
            list(a.profile_recent["children"])[0].startswith("sleep")
            or "Sleeper.sleep" in a.profile_keys
        ):
            return
    assert False, list(a.profile_keys)


@gen_cluster(client=True)
async def test_waiter(c, s, a, b):
    class Waiter:
        def __init__(self):
            self.event = LateLoopEvent()

        async def set(self):
            self.event.set()

        async def wait(self):
            await self.event.wait()

    waiter = await c.submit(Waiter, actor=True)

    futures = [waiter.wait() for _ in range(5)]  # way more than we have actor threads

    await asyncio.sleep(0.1)
    assert not any(future.done() for future in futures)

    await waiter.set()

    await c.gather(futures)


@gen_cluster(client=True, client_kwargs=dict(set_as_default=False))
# ^ NOTE: without `set_as_default=False`, `get_client()` within worker would return
# the same client instance the test is using (because it's all one process).
# Even with this, both workers will share the same client instance.
async def test_worker_actor_handle_is_weakref(c, s, a, b):
    counter = c.submit(Counter, actor=True, workers=[a.address])

    await c.submit(lambda _: None, counter, workers=[b.address])

    del counter

    start = time()
    while a.state.actors or b.data:
        await asyncio.sleep(0.1)
        assert time() < start + 30


def test_worker_actor_handle_is_weakref_sync(client):
    workers = list(client.run(lambda: None))
    counter = client.submit(Counter, actor=True, workers=[workers[0]])

    client.submit(lambda _: None, counter, workers=[workers[1]]).result()

    del counter

    def check(dask_worker):
        return len(dask_worker.data) + len(dask_worker.state.actors)

    start = time()
    while any(client.run(check).values()):
        sleep(0.01)
        assert time() < start + 30


def test_worker_actor_handle_is_weakref_from_compute_sync(client):
    workers = list(client.run(lambda: None))

    with dask.annotate(workers=workers[0]):
        counter = dask.delayed(Counter)()
    with dask.annotate(workers=workers[1]):
        intermediate = dask.delayed(lambda c: None)(counter)
    with dask.annotate(workers=workers[0]):
        final = dask.delayed(lambda x, c: x)(intermediate, counter)

    final.compute(actors=counter, optimize_graph=False)

    def worker_tasks_running(dask_worker):
        return len(dask_worker.data) + len(dask_worker.state.actors)

    start = time()
    while any(client.run(worker_tasks_running).values()):
        sleep(0.01)
        assert time() < start + 30


def test_one_thread_deadlock(loop):
    class UsesCounter:
        # An actor whose method argument is another actor

        def do_inc(self, ac):
            return ac.increment().result()

    with cluster(nworkers=1) as (cl, _), Client(cl["address"], loop=loop) as client:
        ac = client.submit(Counter, actor=True).result()
        ac2 = client.submit(UsesCounter, actor=True).result()

        assert ac2.do_inc(ac).result() == 1


def test_one_thread_deadlock_timeout(loop):
    class UsesCounter:
        # An actor whose method argument is another actor

        def do_inc(self, ac):
            # ac.increment() returns an EagerActorFuture and so the timeout
            # cannot expire
            return ac.increment().result(timeout=0.001)

    with cluster(nworkers=1) as (cl, _), Client(cl["address"], loop=loop) as client:
        ac = client.submit(Counter, actor=True).result()
        ac2 = client.submit(UsesCounter, actor=True).result()

        assert ac2.do_inc(ac).result() == 1


def test_one_thread_deadlock_sync_client(loop):
    class UsesCounter:
        # An actor whose method argument is another actor

        def do_inc(self, ac):
            return get_client().sync(ac.increment)

    with cluster(nworkers=1) as (cl, _), Client(cl["address"], loop=loop) as client:
        ac = client.submit(Counter, actor=True).result()
        ac2 = client.submit(UsesCounter, actor=True).result()

        assert ac2.do_inc(ac).result() == 1


@gen_cluster(client=True, nthreads=[("127.0.0.1", 1)])
async def test_async_deadlock(client, s, a):
    class UsesCounter:
        # An actor whose method argument is another actor

        async def ado_inc(self, ac):
            return await ac.ainc()

    ac = await client.submit(Counter, actor=True)
    ac2 = await client.submit(UsesCounter, actor=True)

    assert (await ac2.ado_inc(ac)) == 1


def test_exception(loop):
    class MyException(Exception):
        pass

    class Broken:
        def method(self):
            raise MyException

        @property
        def prop(self):
            raise MyException

    with cluster(nworkers=2) as (cl, w), Client(cl["address"], loop=loop) as client:
        ac = client.submit(Broken, actor=True).result()
        acfut = ac.method()
        with pytest.raises(MyException):
            acfut.result()

        with pytest.raises(MyException):
            ac.prop


@gen_cluster(client=True)
async def test_exception_async(client, s, a, b):
    class MyException(Exception):
        pass

    class Broken:
        def method(self):
            raise MyException

        @property
        def prop(self):
            raise MyException

    ac = await client.submit(Broken, actor=True)
    acfut = ac.method()
    with pytest.raises(MyException):
        await acfut

    with pytest.raises(MyException):
        await ac.prop


def test_as_completed(client):
    ac = client.submit(Counter, actor=True).result()
    futures = [ac.increment() for _ in range(10)]
    max = 0

    for future in as_completed(futures):
        value = future.result()
        if value > max:
            max = value

    assert all(future.done() for future in futures)
    assert max == 10


@gen_cluster(client=True)
async def test_actor_future_awaitable(client, s, a, b):
    ac = await client.submit(Counter, actor=True)
    futures = [ac.increment() for _ in range(10)]

    assert all([isinstance(future, BaseActorFuture) for future in futures])

    out = await asyncio.gather(*futures)
    assert all([future.done() for future in futures])
    assert max(out) == 10


@gen_cluster(client=True)
async def test_actor_future_awaitable_deadlock(client, s, a, b):
    ac = await client.submit(Counter, actor=True)
    f = ac.increment()

    async def coro():
        return await f

    assert await asyncio.gather(coro(), coro()) == [1, 1]


@gen_cluster(client=True)
async def test_serialize_with_pickle(c, s, a, b):
    class Foo:
        def __init__(self):
            self.actor = get_client().submit(Counter, actor=True).result()

        def __getstate__(self):
            return self.actor

        def __setstate__(self, state):
            self.actor = state

    future = c.submit(Foo, workers=a.address)
    foo = await future
    assert isinstance(foo.actor, Actor)


@gen_cluster(client=True)
async def test_worker_client_async(c, s, a, b):
    class Actor:
        async def demo(self, x):
            with worker_client() as c:
                x = c.submit(inc, x)
                y = c.submit(double, x)
                return await x + await y

    actor = await c.submit(Actor, actor=True)
    assert await actor.demo(10) == 10 + 1 + (10 + 1) * 2


@gen_cluster(client=True)
async def test_worker_client_separate_thread(c, s, a, b):
    class Actor:
        def demo(self, x):
            with worker_client() as c:
                x = c.submit(inc, x)
                y = c.submit(double, x)
                return x.result() + y.result()

    actor = await c.submit(Actor, actor=True)
    assert await actor.demo(10, separate_thread=True) == 10 + 1 + (10 + 1) * 2


@gen_cluster(client=True)
async def test_get_worker(c, s, a, b):
    class Actor:
        # There's not much you can do with a worker in a synchronous function
        # running on the worker event loop.
        def demo(self):
            return get_worker().address

    actor = await c.submit(Actor, actor=True, workers=[a.address])
    assert await actor.demo() == a.address


@gen_cluster(client=True, nthreads=[("", 1)])
async def test_actor_worker_host_leaves_gracefully(c, s, a):
    # see also test_actor_worker_host_dies
    async with Worker(s.address, nthreads=1) as b:
        counter = await c.submit(
            Counter, actor=True, workers=[b.address], allow_other_workers=True
        )

        enter_ev = Event()
        wait_ev = Event()

        def foo(counter, enter_ev, wait_ev):
            enter_ev.set()
            wait_ev.wait()

        fut = c.submit(
            foo,
            counter,
            enter_ev,
            wait_ev,
            workers=[a.address],
            allow_other_workers=True,
        )

        await enter_ev.wait()
    await wait_ev.set()
    with pytest.raises(RuntimeError, match="Worker holding Actor was lost"):
        await fut.result()


@gen_cluster(client=True, nthreads=[("", 1)])
async def test_actor_worker_host_dies(c, s, a):
    # see also test_actor_worker_host_leaves_gracefully
    async with Worker(s.address, nthreads=1) as b:
        counter = await c.submit(
            Counter, actor=True, workers=[b.address], allow_other_workers=True
        )

        enter_ev = Event()
        wait_ev = Event()

        def foo(counter, enter_ev, wait_ev):
            enter_ev.set()
            wait_ev.wait()

        fut = c.submit(
            foo,
            counter,
            enter_ev,
            wait_ev,
            workers=[a.address],
            allow_other_workers=True,
        )

        await enter_ev.wait()
        # Simulate the worker going down
        s.stream_comms[b.address].close()
        await b.finished()
        await wait_ev.set()
        with pytest.raises(RuntimeError, match="Worker holding Actor was lost"):
            await fut.result()


class BlockingActor:
    """An actor whose method blocks until an external gate is released.

    The blocking method is synchronous, so it runs in the worker's dedicated actor
    thread and leaves the worker's event loop free to handle other connections.
    """

    def __init__(self):
        self.calls = 0
        self.entered = threading.Event()
        self.gate = threading.Event()
        self.settled = threading.Event()

    def blocking(self):
        self.calls += 1
        self.entered.set()
        self.gate.wait()
        self.settled.set()
        return "blocking-result"

    def ping(self):
        self.calls += 1
        return "pong"


@gen_cluster(nthreads=[("", 2)])
async def test_actor_in_flight_call_survives_client_disconnect(s, a):
    """A method that has already started must be allowed to settle if its submitting
    client disconnects, and the actor must outlive that client while the call runs.
    """
    async with Client(s.address, asynchronous=True) as c1:
        actor = await c1.submit(BlockingActor, actor=True, workers=[a.address])
        key = actor.key

        call = actor.blocking()
        obj = a.state.actors[key]
        await async_poll_for(obj.entered.is_set)

        # The call is genuinely in flight on both the worker and the scheduler
        assert a.state.actor_call_in_flight(key)
        assert s.tasks[key].actor_calls_in_flight == 1

        await c1.close()

    # Give the scheduler time to process the client removal
    await async_poll_for(lambda: key not in s.clients)
    await asyncio.sleep(0.2)

    # Neither the worker object nor the scheduler task may be torn down mid-call
    assert key in a.state.actors
    assert key in s.tasks
    assert s.tasks[key].state == "memory"
    assert s.tasks[key].actor_calls_in_flight == 1
    assert not obj.settled.is_set()

    # Settle the method directly on the worker; it must run to completion undisturbed
    obj.gate.set()
    await async_poll_for(obj.settled.is_set)

    # The detached client's future can no longer deliver the result over its dead
    # connection, but the method itself has settled and the bookkeeping is cleared.
    await async_poll_for(lambda: not a.state.actor_call_in_flight(key))
    assert call.done()


@gen_cluster(nthreads=[("", 2)])
async def test_actor_reachable_by_other_client_after_holder_disconnects(s, a):
    """If another client still wants the actor, it keeps using the same remote object
    after the original holder disconnects while a method is in flight.
    """
    async with Client(s.address, asynchronous=True) as c1:
        actor = await c1.submit(BlockingActor, actor=True, workers=[a.address])
        key = actor.key
        address = actor._address
        actor.blocking()
        obj = a.state.actors[key]
        await async_poll_for(obj.entered.is_set)

        # A second client acquires its own handle to the very same actor object and
        # registers scheduler-side interest, becoming a real holder.
        async with Client(s.address, asynchronous=True) as c2:
            other = Actor(BlockingActor, address, key)
            other._client = c2
            other._future = Future(key, c2)
            c2._send_to_scheduler({"op": "client-desires-keys", "keys": [key]})
            await async_poll_for(
                lambda: bool(s.tasks[key].who_wants)
                and len(s.tasks[key].who_wants) == 2
            )

            await c1.close()
            await async_poll_for(
                lambda: bool(s.tasks[key].who_wants)
                and len(s.tasks[key].who_wants) == 1
            )
            await asyncio.sleep(0.2)

            # c2 is now the sole holder; the actor is still alive and reachable even
            # though a method is in flight and the original holder is gone.
            assert key in a.state.actors
            assert key in s.tasks
            assert s.tasks[key].actor_calls_in_flight == 1

            # Settle the in-flight method directly on the worker; it must run to
            # completion undisturbed rather than be cancelled by the disconnect
            obj.gate.set()
            await async_poll_for(obj.settled.is_set)

            # Now that the single actor thread is free, c2 drives the same object again
            assert await other.ping() == "pong"
            assert obj.calls == 2  # blocking + ping

        # c2 disconnects with no call running: destruction is now allowed
    await async_poll_for(lambda: key not in s.tasks and key not in a.state.actors)
    assert not a.state.actor_calls_in_flight


@gen_cluster(client=True, nthreads=[("", 2)])
async def test_actor_destroyed_only_after_holders_and_calls_gone(c, s, a):
    """Actual destruction must wait until there are neither holders nor in-flight
    calls, but it must happen promptly once both conditions are met.
    """
    actor = await c.submit(BlockingActor, actor=True, workers=[a.address])
    key = actor.key

    actor.blocking()
    obj = a.state.actors[key]
    await async_poll_for(obj.entered.is_set)

    # Drop every client reference while the method is still running
    del actor
    c._dec_ref(key)
    await asyncio.sleep(0.3)
    assert key in a.state.actors  # protected by the in-flight call
    assert key in s.tasks

    obj.gate.set()
    await async_poll_for(lambda: key not in a.state.actors and key not in s.tasks)

    assert key not in a.state.actors
    assert not a.state.actor_calls_in_flight
    assert obj.settled.is_set()
