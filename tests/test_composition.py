from __future__ import annotations

import time
from threading import Condition, Event

import pytest

import dramatiq
from dramatiq import group, middleware, pipeline
from dramatiq.middleware import GroupCallbacks
from dramatiq.results import GroupResult, Results, ResultStatus, ResultTimeout


def test_messages_can_be_piped(stub_broker):
    # Given an actor that adds two numbers together
    @dramatiq.actor
    def add(x, y):
        return x + y

    # When I pipe some messages intended for that actor together
    pipe = add.message(1, 2) | add.message(3) | add.message(4)

    # Then I should get back a pipeline object
    assert isinstance(pipe, pipeline)

    # And each message in the pipeline should reference the next message in line
    assert pipe.messages[0].options["pipe_target"] == pipe.messages[1].asdict()
    assert pipe.messages[1].options["pipe_target"] == pipe.messages[2].asdict()
    assert "pipe_target" not in pipe.messages[2].options


def test_pipelines_flatten_child_pipelines(stub_broker):
    # Given an actor that adds two numbers together
    @dramatiq.actor
    def add(x, y):
        return x + y

    # When I pipe a message intended for that actor and another pipeline together
    pipe = pipeline([add.message(1, 2), add.message(3) | add.message(4), add.message(5)])

    # Then the inner pipeline should be flattened into the outer pipeline
    assert len(pipe) == 4
    assert pipe.messages[0].args == (1, 2)
    assert pipe.messages[1].args == (3,)
    assert pipe.messages[2].args == (4,)
    assert pipe.messages[3].args == (5,)


def test_pipe_ignore_applies_to_receiving_message(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def return_args(*args):
        return args

    # When I compose pipe of three messages with pipe_ignore option on second message
    pipe = (
        return_args.message(1) | return_args.message_with_options(pipe_ignore=True, args=(2,)) | return_args.message(3)
    )

    # And then run and wait for it to complete
    pipe.run()
    stub_broker.join(return_args.queue_name)
    results = list(pipe.get_results())

    # The then result of the first message should NOT be passed as
    # argument to the second message and the result of the second
    # message should be passed as argument to the third message.
    assert results == [[1], [2], [3, [2]]]


def test_pipeline_results_can_be_retrieved(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that adds two numbers together and stores the result
    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I pipe some messages intended for that actor together and run the pipeline
    pipe = add.message(1, 2) | (add.message(3) | add.message(4))
    pipe.run()

    # Then the pipeline result should be the sum of 1, 2, 3 and 4
    assert pipe.get_result(block=True) == 10

    # And I should be able to retrieve individual results
    assert list(pipe.get_results()) == [3, 6, 10]


def test_pipeline_results_respect_timeouts(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that waits some amount of time then doubles that amount
    @dramatiq.actor(store_results=True)
    def wait(n):
        time.sleep(n)
        return n * 2

    # When I pipe some messages intended for that actor together and run the pipeline
    pipe = wait.message(1) | wait.message() | wait.message()
    pipe.run()

    # And get the results with a lower timeout than the tasks can complete in
    # Then a ResultTimeout error should be raised
    with pytest.raises(ResultTimeout):
        for _ in pipe.get_results(block=True, timeout=1000):
            pass


def test_pipelines_expose_completion_stats(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that waits some amount of time
    condition = Condition()

    @dramatiq.actor(store_results=True)
    def wait(n):
        time.sleep(n)
        with condition:
            condition.notify_all()
            return n

    # When I pipe some messages intended for that actor together and run the pipeline
    pipe = wait.message(1) | wait.message()
    pipe.run()

    # Then every time a job in the pipeline completes, the completed_count should increase
    for count in range(1, len(pipe) + 1):
        with condition:
            condition.wait(2)
            time.sleep(0.1)  # give the worker time to set the result
            assert pipe.completed_count == count

    # Finally, completed should be true
    assert pipe.completed


def test_pipelines_can_be_incomplete(stub_broker, result_backend):
    # Given that I am not running a worker
    # And I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    # And I have an actor that does nothing
    @dramatiq.actor(store_results=True)
    def do_nothing():
        return None

    # And I've run a pipeline
    pipe = do_nothing.message() | do_nothing.message_with_options(pipe_ignore=True)
    pipe.run()

    # When I check if the pipeline has completed
    # Then it should return False
    assert not pipe.completed


def test_groups_execute_jobs_in_parallel(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    # And I have an actor that sleeps for 100ms
    @dramatiq.actor(store_results=True)
    def wait():
        time.sleep(0.1)

    # When I group multiple of these actors together and run them
    t = time.monotonic()
    g = group([wait.message() for _ in range(5)])
    g.run()

    # And wait on the group to complete
    results = list(g.get_results(block=True))

    # Then the total elapsed time should be less than 500ms
    assert time.monotonic() - t <= 0.5

    # And I should get back as many results as there were jobs in the group
    assert len(results) == len(g)

    # And the group should be completed
    assert g.completed


def test_groups_execute_inner_groups(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    # And I have an actor that sleeps for 100ms
    @dramatiq.actor(store_results=True)
    def wait():
        time.sleep(0.1)

    # When I group multiple groups inside one group and run it
    t = time.monotonic()
    g = group(group(wait.message() for _ in range(2)) for _ in range(3))
    g.run()

    # And wait on the group to complete
    results = list(g.get_results(block=True))

    # Then the total elapsed time should be less than 500ms
    assert time.monotonic() - t <= 0.5

    # And I should get back 3 results each with 2 results inside it
    assert results == [[None, None]] * 3

    # And the group should be completed
    assert g.completed


def test_groups_can_time_out(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    # And I have an actor that sleeps for 300ms
    @dramatiq.actor(store_results=True)
    def wait():
        time.sleep(0.3)

    # When I group a few jobs together and run it
    g = group(wait.message() for _ in range(2))
    g.run()

    # And wait for the group to complete with a timeout
    # Then a ResultTimeout error should be raised
    with pytest.raises(ResultTimeout):
        g.wait(timeout=100)

    # And the group should not be completed
    assert not g.completed


def test_groups_expose_completion_stats(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that waits some amount of time
    condition = Condition()

    @dramatiq.actor(store_results=True)
    def wait(n):
        time.sleep(n)
        with condition:
            condition.notify_all()
            return n

    # When I group messages of varying durations together and run the group
    g = group(wait.message(n) for n in range(1, 4))
    g.run()

    # Then every time a job in the group completes, the completed_count should increase
    for count in range(1, len(g) + 1):
        with condition:
            condition.wait(5)
            time.sleep(0.1)  # give the worker time to set the result
            assert g.completed_count == count

    # Finally, completed should be true
    assert g.completed


def test_groups_completion_count_works_for_tasks_finishing_out_of_order(stub_broker, stub_worker, result_backend):
    # Regression test for https://github.com/Bogdanp/dramatiq/issues/452

    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that waits some amount of time, before notifying the Condition
    condition = Condition()

    @dramatiq.actor(store_results=True)
    def wait(n):
        time.sleep(n)
        with condition:
            condition.notify_all()
            return n

    # When I group messages of varying durations together and run the group.
    # NOTE the durations ensure the jobs finish in a different order to what they are in the group.
    g = group(wait.message(t) for t in [3.5, 1, 2.2])
    g.run()

    # Then every time a job in the group completes, the completed_count should increase by 1
    for count in [1, 2, 3]:
        with condition:
            condition.wait(5)
            time.sleep(0.1)  # give the worker time to set the result
            assert g.completed_count == count

    # Finally, completed should be true
    assert g.completed


def test_pipeline_does_not_continue_to_next_actor_when_message_is_marked_as_failed(stub_broker, stub_worker):
    # Given that I have an actor that fails messages
    class FailMessageMiddleware(middleware.Middleware):
        def after_process_message(self, broker, message, *, result=None, exception=None):
            message.fail()

    stub_broker.add_middleware(FailMessageMiddleware())

    has_run = False

    @dramatiq.actor
    def do_nothing():
        pass

    @dramatiq.actor
    def should_never_run():
        nonlocal has_run
        has_run = True

    # When I pipe some messages intended for that actor together and run the pipeline
    pipe = do_nothing.message_with_options(pipe_ignore=True) | should_never_run.message()
    pipe.run()

    stub_broker.join(should_never_run.queue_name, timeout=10 * 1000, fail_fast=False)
    stub_worker.join()

    # Then the second message in the pipe should never have run
    assert not has_run


def test_pipeline_respects_own_delay(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that adds two numbers together
    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I pipe some messages intended for that actor together and run the pipeline with a delay
    pipe = add.message(1, 2) | add.message(3)
    pipe.run(delay=10000)

    # And get the results with a lower timeout than the the pipeline is delayed by
    # Then a ResultTimeout error should be raised
    with pytest.raises(ResultTimeout):
        for _ in pipe.get_results(block=True, timeout=100):
            pass


def test_pipeline_respects_delay_of_first_message(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that adds two numbers together
    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I pipe some messages intended for that actor together, where first message is delayed and run the pipeline
    pipe = add.message_with_options(args=(1, 2), delay=10000) | add.message(3)
    pipe.run()

    # And get the results with a lower timeout than the first message's delay
    # Then a ResultTimeout error should be raised
    with pytest.raises(ResultTimeout):
        for _ in pipe.get_results(block=True, timeout=100):
            pass


def test_pipeline_respects_delay_of_second_message(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that adds two numbers together
    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I pipe some messages intended for that actor together, where second message is delayed and run the pipeline
    pipe = add.message(1, 2) | add.message_with_options(args=(3,), delay=10000)
    pipe.run()

    # And get the results with a lower timeout than the second message's delay
    # Then a ResultTimeout error should be raised
    with pytest.raises(ResultTimeout):
        for _ in pipe.get_results(block=True, timeout=100):
            pass


def test_pipeline_respects_bigger_of_first_messages_and_pipelines_delay(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that adds two numbers together
    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I pipe some messages intended for that actor together, first of which is delayed
    # And the pipeline is delayed with a bigger value than the first message, and run the pipeline
    pipe = add.message_with_options(args=(1, 2), delay=100) | add.message(3)
    pipe.run(delay=10000)

    # And get the results with a higher timeout than first message's delay, but lower than pipeline's delay
    # Then a ResultTimeout error should be raised
    with pytest.raises(ResultTimeout):
        for _ in pipe.get_results(block=True, timeout=300):
            pass


def test_groups_can_have_completion_callbacks(stub_broker, stub_worker, rate_limiter_backend):
    # Given that I have a rate limiter backend
    # And I've added the GroupCallbacks middleware to my broker
    stub_broker.add_middleware(GroupCallbacks(rate_limiter_backend))

    do_nothing_times = []
    finalize_times = []
    finalized = Event()

    @dramatiq.actor
    def do_nothing():
        do_nothing_times.append(time.monotonic())

    @dramatiq.actor
    def finalize(n):
        assert n == 42
        finalize_times.append(time.monotonic())
        finalized.set()

    # When I group together some messages with a completion callback
    g = group(do_nothing.message() for n in range(5))
    g.add_completion_callback(finalize.message(42))
    g.run()

    # And wait for the callback to be callled
    finalized.wait(timeout=30)

    # Then all the messages in the group should run
    assert len(do_nothing_times) == 5

    # And the callback
    assert len(finalize_times) == 1

    # And the callback should run after all the messages
    assert sorted(do_nothing_times)[-1] <= finalize_times[0]


def test_groups_with_completion_callbacks_fail_unless_group_callbacks_is_set_up(stub_broker, stub_worker):
    # Given that I haven't set up GroupCallbacks
    @dramatiq.actor
    def do_nothing():
        pass

    @dramatiq.actor
    def finalize(n):
        pass

    # When I group together some messages with a completion callback
    g = group(do_nothing.message() for n in range(5))
    g.add_completion_callback(finalize.message(42))

    # And run the group
    # Then a RuntimeError should be raised
    with pytest.raises(RuntimeError):
        g.run()


def test_groups_of_pipelines_can_have_completion_callbacks(stub_broker, stub_worker, rate_limiter_backend):
    # Given that I have a rate limiter backend
    # And I've added the GroupCallbacks middleware to my broker
    stub_broker.add_middleware(GroupCallbacks(rate_limiter_backend))

    do_nothing_times = []
    finalize_times = []
    finalized = Event()

    @dramatiq.actor
    def do_nothing(_):
        do_nothing_times.append(time.monotonic())

    @dramatiq.actor
    def finalize(n):
        assert n == 42
        finalize_times.append(time.monotonic())
        finalized.set()

    # When I group together some messages with a completion callback
    g = group([do_nothing.message(1) | do_nothing.message(), do_nothing.message(1)])
    g.add_completion_callback(finalize.message(42))
    g.run()

    # And wait for the callback to be callled
    finalized.wait(timeout=30)

    # Then all the messages in the group should run
    assert len(do_nothing_times) == 3

    # And the callback
    assert len(finalize_times) == 1

    # And the callback should run after all the messages
    assert sorted(do_nothing_times)[-1] <= finalize_times[0]


def test_iter_results_yields_results_in_completion_order_with_their_index_paths(
    stub_broker, stub_worker, result_backend
):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that waits some amount of time before returning its argument
    @dramatiq.actor(store_results=True)
    def wait(n):
        time.sleep(n)
        return n

    # When I group two jobs whose durations make them finish in the reverse
    # order of the one they were submitted in
    g = group([wait.message(0.4), wait.message(0.05)])
    g.run()

    # And iterate the results as they complete
    results = list(g.iter_results(block=True, timeout=10_000))

    # Then I should get one GroupResult per job
    assert len(results) == 2
    assert all(isinstance(result, GroupResult) for result in results)

    # And the job that finished first (the second one in the group) should
    # be yielded first even though it was submitted last
    assert [result.index_path for result in results] == [(1,), (0,)]
    assert [result.value for result in results] == [0.05, 0.4]
    assert all(result.complete for result in results)


def test_iter_results_does_not_hold_back_completed_results_behind_a_slow_first_job(
    stub_broker, stub_worker, result_backend
):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def wait(n):
        time.sleep(n)
        return n

    # When the first job in the group is the slow one
    g = group([wait.message(0.3), wait.message(0.05)])
    g.run()

    # Then the fast, second job's result should be available well before the
    # slow, first job finishes
    iterator = g.iter_results(block=True, timeout=10_000)
    first = next(iterator)
    assert first.index_path == (1,)
    assert first.value == 0.05

    second = next(iterator)
    assert second.index_path == (0,)
    assert second.value == 0.3


def test_iter_results_reports_missing_results_when_not_blocking(stub_broker, result_backend):
    # Given that I have a result backend but no worker
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def do_work():
        return 42

    # When I run a group without processing any of its messages
    g = group(do_work.message() for _ in range(2))
    g.run()

    # And iterate the results without blocking
    results = list(g.iter_results())

    # Then every job should be reported exactly once as missing, rather
    # than a ResultMissing error being raised
    assert len(results) == 2
    assert [result.index_path for result in results] == [(0,), (1,)]
    assert all(result.missing for result in results)
    assert all(result.status is ResultStatus.MISSING for result in results)
    assert all(result.value is None for result in results)
    assert all(result.error is None for result in results)


def test_iter_results_uses_a_single_shared_deadline(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor that takes longer than the deadline I'm willing to wait
    @dramatiq.actor(store_results=True)
    def wait():
        time.sleep(0.4)
        return 42

    # When I run the group and iterate with a timeout shorter than the job
    g = group([wait.message(), wait.message()])
    g.run()

    started = time.monotonic()
    results = list(g.iter_results(block=True, timeout=100))
    elapsed = time.monotonic() - started

    # Then the whole iteration should stop once the single deadline expires
    assert elapsed < 1.0

    # And every unfinished job should be reported as missing instead of
    # the deadline causing a ResultTimeout to be raised
    assert len(results) == 2
    assert [result.index_path for result in results] == [(0,), (1,)]
    assert all(result.missing for result in results)


def test_iter_results_distinguishes_failures_missing_and_none(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True, max_retries=0)
    def fails():
        raise RuntimeError("boom")

    @dramatiq.actor(store_results=True)
    def returns_none():
        return None

    @dramatiq.actor(store_results=True)
    def slow():
        time.sleep(0.3)
        return "done"

    # When I group a failing job together with a job that legitimately
    # returns None and wait for both to settle
    settled = group([fails.message(), returns_none.message()])
    settled.run()
    results = list(settled.iter_results(block=True, timeout=10_000))
    by_path = {result.index_path: result for result in results}
    assert set(by_path) == {(0,), (1,)}

    # The failed job is reported as a failure carrying the stored exception
    failed = by_path[(0,)]
    assert failed.failed
    assert failed.status is ResultStatus.FAILED
    assert failed.error is not None
    assert failed.error.orig_exc_type == "RuntimeError"
    assert failed.error.orig_exc_msg == "boom"
    assert failed.value is None

    # The None result is complete and distinct from a missing result
    none_result = by_path[(1,)]
    assert none_result.complete
    assert none_result.value is None
    assert not none_result.missing

    # And a job that has not finished by the deadline is reported once as
    # missing rather than raising an error
    pending = group([slow.message()])
    pending.run()
    timed_out = list(pending.iter_results(block=True, timeout=100))
    assert len(timed_out) == 1
    missing = timed_out[0]
    assert missing.index_path == (0,)
    assert missing.missing
    assert missing.status is ResultStatus.MISSING
    assert missing.error is None


def test_iter_results_flattens_nested_groups_and_keeps_full_index_paths(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I build nested groups and run them
    g = group(
        [
            group([add.message(1, 1), add.message(2, 2)]),
            group([add.message(3, 3)]),
        ]
    )
    g.run()

    results = list(g.iter_results(block=True, timeout=10_000))
    by_path = {result.index_path: result.value for result in results}

    # Then every leaf should be locatable through its full index path
    assert by_path == {(0, 0): 2, (0, 1): 4, (1, 0): 6}
    assert all(result.complete for result in results)


def test_iter_results_locates_pipeline_ends_via_their_index_path(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I group pipelines together with plain messages
    g = group(
        [
            add.message(1, 1) | add.message(10),
            add.message(2, 2),
        ]
    )
    g.run()

    results = list(g.iter_results(block=True, timeout=10_000))
    by_path = {result.index_path: result for result in results}

    # Then the pipeline is a single leaf whose value is the result of the
    # terminal message in the chain
    assert set(by_path) == {(0,), (1,)}
    assert by_path[(0,)].complete
    assert by_path[(0,)].value == 12
    assert by_path[(1,)].value == 4


def test_iter_results_does_not_change_submission_order_entrypoint(stub_broker, stub_worker, result_backend):
    # Given that I have a result backend
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def wait(n):
        time.sleep(n)
        return n

    # Then the existing get_results entrypoint must still yield in the
    # original submission order, waiting on each job in turn
    ordered = group([wait.message(0.3), wait.message(0.05)])
    ordered.run()
    assert list(ordered.get_results(block=True, timeout=10_000)) == [0.3, 0.05]

    # And the new entrypoint, consumed while its jobs are still running,
    # surfaces the job that finishes first ahead of the slower one even
    # though that job was submitted last
    streamed = group([wait.message(0.3), wait.message(0.05)])
    streamed.run()
    first = next(streamed.iter_results(block=True, timeout=10_000))
    assert first.index_path == (1,)
    assert first.value == 0.05
