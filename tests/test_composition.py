from __future__ import annotations

import threading
import time
from threading import Condition, Event

import pytest

import dramatiq
from dramatiq import group, middleware, pipeline
from dramatiq.middleware import GroupCallbacks
from dramatiq.results import Results, ResultTimeout


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


def test_pipeline_run_returns_independent_handle(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # And a reusable pipeline template
    template = pipeline([add.message(1, 2), add.message(3)])

    # When I run it twice
    first = template.run()
    second = template.run()

    # Then I should get back a handle per run
    assert isinstance(first, dramatiq.pipeline_run)
    assert isinstance(second, dramatiq.pipeline_run)
    assert first is not second

    # And the handles should not share message ids, options or links
    assert [m.message_id for m in first.messages] != [m.message_id for m in second.messages]
    assert first.messages[0].options["pipe_target"]["message_id"] == first.messages[1].message_id
    assert second.messages[0].options["pipe_target"]["message_id"] == second.messages[1].message_id

    # And the template itself should be untouched by the runs
    assert "pipe_target" not in first.messages[-1].options
    assert template.messages[0].message_id not in {m.message_id for m in first.messages}

    # And each handle should resolve its own results
    assert first.get_result(block=True) == 6
    assert second.get_result(block=True) == 6
    assert [m.message_id for m in first.messages] != [m.message_id for m in second.messages]


def test_interleaved_runs_of_the_same_template_do_not_cross_talk(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # And a template whose first message takes its inputs per run
    template = pipeline([add.message(0, 0), add.message(3)])

    # When two runs with different inputs are interleaved
    first = template.run(args=(1, 2))
    second = template.run(args=(10, 20))
    stub_broker.join(add.queue_name)
    stub_worker.join()

    # Then the inputs should only have replaced the first message of each run
    assert first.messages[0].args == (1, 2)
    assert second.messages[0].args == (10, 20)
    assert first.messages[1].args == (3,)
    assert second.messages[1].args == (3,)

    # And the two runs should produce their own end results
    assert first.get_result(block=True) == 6
    assert second.get_result(block=True) == 33
    assert list(first.get_results()) == [3, 6]
    assert list(second.get_results()) == [30, 33]


def test_interleaved_processing_of_two_runs_does_not_cross_talk(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    # And an actor whose first step stalls run 1 until run 2 has reached it
    first_step_ran = Event()

    @dramatiq.actor(store_results=True)
    def begin(x):
        if x == 1:
            first_step_ran.wait(timeout=5)
        else:
            first_step_ran.set()
        return x

    @dramatiq.actor(store_results=True)
    def finish(x):
        return x * 10

    # And a nested template
    template = pipeline([begin.message(0), finish.message()])

    # When two runs are started and their processing is forced to
    # interleave (run 2's first step completes while run 1 is still
    # waiting on its first step)
    first = template.run(args=(1,))
    second = template.run(args=(2,))
    stub_broker.join(begin.queue_name)
    stub_broker.join(finish.queue_name)
    stub_worker.join()

    # Then each run should reach its own end result
    assert first.get_result(block=True) == 10
    assert second.get_result(block=True) == 20
    assert list(first.get_results()) == [1, 10]
    assert list(second.get_results()) == [2, 20]


def test_reusing_a_child_pipeline_across_templates_does_not_share_state(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # And a child pipeline that is reused by two different templates
    child = pipeline([add.message(3), add.message(4)])
    first_template = pipeline([add.message(1, 0), child])
    second_template = pipeline([add.message(2, 0), child])

    # When both templates are run
    first = first_template.run()
    second = second_template.run()
    stub_broker.join(add.queue_name)
    stub_worker.join()

    # Then the nested child should still have been flattened
    assert len(first.messages) == 3
    assert len(second.messages) == 3

    # And each template should keep its own first argument
    assert first.messages[0].args == (1, 0)
    assert second.messages[0].args == (2, 0)

    # And the shared child definition must not have been mutated
    assert child.messages[0].args == (3,)
    assert child.messages[0].options["pipe_target"]["message_id"] == child.messages[1].message_id

    # And the runs must reach their own end results
    assert first.get_result(block=True) == 8
    assert second.get_result(block=True) == 9


def test_querying_the_template_after_run_matches_the_last_run(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # And a template that is run twice with different inputs
    template = pipeline([add.message(0, 0), add.message(3)])
    template.run(args=(1, 2))
    template.run(args=(10, 20))

    # When I use the historical "run then query the same object" usage
    # Then it should refer to the most recent run
    assert template.get_result(block=True) == 33
    assert list(template.get_results()) == [30, 33]
    assert template.completed
    assert template.completed_count == 2


def test_pipeline_exposes_the_handle_of_its_last_run(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # And a template that hasn't run yet
    template = pipeline([add.message(1, 2), add.message(3)])
    assert template.last_run is None

    # When I run it
    run = template.run()

    # Then the template should expose that handle
    assert template.last_run is run

    # And a handle of an already-enqueued run cannot be composed again,
    # since that would reuse its message ids
    with pytest.raises(TypeError):
        pipeline([run, add.message(100)])


def test_composing_a_template_still_flattens_it(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I compose a template into another pipeline
    child = pipeline([add.message(1, 2), add.message(3)])
    composed = pipeline([child, add.message(100)])

    # Then the child should be flattened
    assert len(composed) == 3
    assert composed.messages[0].args == (1, 2)
    assert composed.messages[2].args == (100,)


def test_concurrent_runs_of_the_same_template_are_isolated(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # And a template that a service holds for a long time
    template = pipeline([add.message(0, 0), add.message(1)])

    # When two threads run it at the same time with different inputs
    runs = {}

    def run_and_wait(thread_id, args):
        runs[thread_id] = template.run(args=args)

    threads = [
        threading.Thread(target=run_and_wait, args=("a", (1, 2))),
        threading.Thread(target=run_and_wait, args=("b", (10, 20))),
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    stub_broker.join(add.queue_name)
    stub_worker.join()

    # Then both runs must have distinct message ids
    assert {m.message_id for m in runs["a"].messages}.isdisjoint({m.message_id for m in runs["b"].messages})

    # And each run should reach its own end result
    assert {runs["a"].get_result(block=True), runs["b"].get_result(block=True)} == {4, 31}


def test_group_run_queries_follow_the_run_handles(stub_broker, stub_worker, result_backend):
    # Given a result backend
    # And a broker with the results middleware
    stub_broker.add_middleware(Results(backend=result_backend))

    @dramatiq.actor(store_results=True)
    def add(x, y):
        return x + y

    # When I run a group containing the same pipeline template twice
    template = pipeline([add.message(1, 2), add.message(4)])
    g = group([template, template])
    g.run()

    # Then the group should read the results of the runs it enqueued
    assert list(g.get_results(block=True)) == [7, 7]
    assert g.completed
    assert g.completed_count == 2
