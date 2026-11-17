import unittest
from contextlib import contextmanager
from datetime import datetime
from time import sleep
from unittest import mock

import pytest
from pybreaker import *
from pybreaker import UTC
from tornado import gen, testing


class CircuitBreakerStorageBasedTestCase:
    """Mix in to test against different storage backings. Depends on
    `self.breaker` and `self.breaker_kwargs`.
    """

    def __init__(self):
        self.breaker_kwargs = None

    def test_successful_call(self):
        """CircuitBreaker: it should keep the circuit closed after a successful
        call.
        """

        def func():
            return True

        assert self.breaker.call(func)
        assert self.breaker.fail_counter == 0
        assert self.breaker.current_state == "closed"

    def test_one_failed_call(self):
        """CircuitBreaker: it should keep the circuit closed after a few
        failures.
        """

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 1
        assert self.breaker.current_state == "closed"

    def test_one_successful_call_after_failed_call(self):
        """CircuitBreaker: it should keep the circuit closed after few mixed
        outcomes.
        """

        def suc():
            return True

        def err():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(err)
        assert self.breaker.fail_counter == 1

        assert self.breaker.call(suc)
        assert self.breaker.fail_counter == 0
        assert self.breaker.current_state == "closed"

    def test_several_failed_calls_setting_absent(self):
        """CircuitBreaker: it should open the circuit after many failures."""
        self.breaker = CircuitBreaker(fail_max=3, **self.breaker_kwargs)

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        with pytest.raises(NotImplementedError):
            self.breaker.call(func)

        # Circuit should open
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 3
        assert self.breaker.current_state == "open"

    def test_throw_new_error_on_trip_false(self):
        """CircuitBreaker: it should throw the original exception"""
        self.breaker = CircuitBreaker(fail_max=3, **self.breaker_kwargs, throw_new_error_on_trip=False)

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        with pytest.raises(NotImplementedError):
            self.breaker.call(func)

        # Circuit should be open
        assert self.breaker.fail_counter == 3
        assert self.breaker.current_state == "open"

        # Circuit should still be open and break
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 3
        assert self.breaker.current_state == "open"

    def test_throw_new_error_on_trip_true(self):
        """CircuitBreaker: it should throw a CircuitBreakerError exception"""
        self.breaker = CircuitBreaker(fail_max=3, **self.breaker_kwargs, throw_new_error_on_trip=True)

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        with pytest.raises(NotImplementedError):
            self.breaker.call(func)

        # Circuit should open
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 3
        assert self.breaker.current_state == "open"

    def test_traceback_in_circuitbreaker_error(self):
        """CircuitBreaker: it should open the circuit after many failures."""
        self.breaker = CircuitBreaker(fail_max=3, **self.breaker_kwargs)

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        with pytest.raises(NotImplementedError):
            self.breaker.call(func)

        # Circuit should open
        try:
            self.breaker.call(func)
            pytest.fail("CircuitBreakerError should throw")
        except CircuitBreakerError:
            import traceback

            assert "NotImplementedError" in traceback.format_exc()
        assert self.breaker.fail_counter == 3
        assert self.breaker.current_state == "open"

    def test_failed_call_after_timeout(self):
        """CircuitBreaker: it should half-open the circuit after timeout."""
        self.breaker = CircuitBreaker(fail_max=3, reset_timeout=0.5, **self.breaker_kwargs)

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        assert self.breaker.current_state == "closed"

        # Circuit should open
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 3

        # Wait for timeout
        sleep(0.6)

        # Circuit should open again
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 4
        assert self.breaker.current_state == "open"

    def test_successful_after_timeout(self):
        """CircuitBreaker: it should close the circuit when a call succeeds
        after timeout. The successful function should only be called once.
        """
        self.breaker = CircuitBreaker(fail_max=3, reset_timeout=1, **self.breaker_kwargs)

        suc = mock.MagicMock(return_value=True)

        def err():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(err)
        with pytest.raises(NotImplementedError):
            self.breaker.call(err)
        assert self.breaker.current_state == "closed"

        # Circuit should open
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(err)
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(suc)
        assert self.breaker.fail_counter == 3

        # Wait for timeout, at least a second since redis rounds to a second
        sleep(2)

        # Circuit should close again
        assert self.breaker.call(suc)
        assert self.breaker.fail_counter == 0
        assert self.breaker.current_state == "closed"
        assert suc.call_count == 1

    def test_failed_call_when_halfopen(self):
        """CircuitBreaker: it should open the circuit when a call fails in
        half-open state.
        """

        def fun():
            raise NotImplementedError

        self.breaker.half_open()
        assert self.breaker.fail_counter == 0
        assert self.breaker.current_state == "half-open"

        # Circuit should open
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(fun)
        assert self.breaker.fail_counter == 1
        assert self.breaker.current_state == "open"

    def test_successful_call_when_halfopen(self):
        """CircuitBreaker: it should close the circuit when a call succeeds in
        half-open state.
        """

        def fun():
            return True

        self.breaker.half_open()
        assert self.breaker.fail_counter == 0
        assert self.breaker.current_state == "half-open"

        # Circuit should open
        assert self.breaker.call(fun)
        assert self.breaker.fail_counter == 0
        assert self.breaker.current_state == "closed"

    def test_close(self):
        """CircuitBreaker: it should allow the circuit to be closed manually."""
        self.breaker = CircuitBreaker(fail_max=3, **self.breaker_kwargs)

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        with pytest.raises(NotImplementedError):
            self.breaker.call(func)

        # Circuit should open
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 3
        assert self.breaker.current_state == "open"

        # Circuit should close again
        self.breaker.close()
        assert self.breaker.fail_counter == 0
        assert self.breaker.current_state == "closed"

    def test_transition_events(self):
        """CircuitBreaker: it should call the appropriate functions on every
        state transition.
        """

        class Listener(CircuitBreakerListener):
            def __init__(self):
                self.out = ""

            def state_change(self, cb, old_state, new_state):
                assert cb
                if old_state:
                    self.out += old_state.name
                if new_state:
                    self.out += "->" + new_state.name
                self.out += ","

        listener = Listener()
        self.breaker = CircuitBreaker(listeners=(listener,), **self.breaker_kwargs)
        assert self.breaker.current_state == "closed"

        self.breaker.open()
        assert self.breaker.current_state == "open"

        self.breaker.half_open()
        assert self.breaker.current_state == "half-open"

        self.breaker.close()
        assert self.breaker.current_state == "closed"

        assert listener.out == "closed->open,open->half-open,half-open->closed,"

    def test_call_events(self):
        """CircuitBreaker: it should call the appropriate functions on every
        successful/failed call.
        """
        self.out = ""

        def suc():
            return True

        def err():
            raise NotImplementedError

        class Listener(CircuitBreakerListener):
            def __init__(self):
                self.out = ""

            def before_call(self, cb, func, *args, **kwargs):
                assert cb
                self.out += "-"

            def success(self, cb):
                assert cb
                self.out += "success"

            def failure(self, cb, exc):
                assert cb
                assert exc
                self.out += "failure"

        listener = Listener()
        self.breaker = CircuitBreaker(listeners=(listener,), **self.breaker_kwargs)

        assert self.breaker.call(suc)
        with pytest.raises(NotImplementedError):
            self.breaker.call(err)
        assert listener.out == "-success-failure"

    def test_generator(self):
        """CircuitBreaker: it should inspect generator values."""

        @self.breaker
        def suc(value):
            "Docstring"
            yield value

        @self.breaker
        def err(value):
            "Docstring"
            x = yield value
            raise NotImplementedError(x)

        s = suc(True)
        e = err(True)
        next(e)

        with pytest.raises(NotImplementedError):
            e.send(True)
        assert self.breaker.fail_counter == 1
        assert next(s)
        with pytest.raises((StopIteration, RuntimeError)):
            next(s)
        assert self.breaker.fail_counter == 0

    def test_contextmanager(self):
        """CircuitBreaker: it should catch in a with statement"""

        class Foo:
            @contextmanager
            @self.breaker
            def wrapper(self):
                try:
                    yield
                except NotImplementedError:
                    raise ValueError

            def foo(self):
                with self.wrapper():
                    raise NotImplementedError

        try:
            Foo().foo()
        except ValueError as e:
            assert isinstance(e, ValueError)


class CircuitBreakerConfigurationTestCase:
    """Tests for the CircuitBreaker class."""

    def test_default_state(self):
        """CircuitBreaker: it should get initial state from state_storage."""
        for state in (STATE_OPEN, STATE_CLOSED, STATE_HALF_OPEN):
            storage = CircuitMemoryStorage(state)
            breaker = CircuitBreaker(state_storage=storage)
            assert breaker.state.name == state

    def test_default_params(self):
        """CircuitBreaker: it should define smart defaults."""
        assert self.breaker.fail_counter == 0
        assert self.breaker.reset_timeout == 60
        assert self.breaker.fail_max == 5
        assert self.breaker.current_state == "closed"
        assert self.breaker.excluded_exceptions == ()
        assert self.breaker.listeners == ()
        assert self.breaker._state_storage.name == "memory"

    def test_new_with_custom_reset_timeout(self):
        """CircuitBreaker: it should support a custom reset timeout value."""
        self.breaker = CircuitBreaker(reset_timeout=30)
        assert self.breaker.fail_counter == 0
        assert self.breaker.reset_timeout == 30
        assert self.breaker.fail_max == 5
        assert self.breaker.excluded_exceptions == ()
        assert self.breaker.listeners == ()
        assert self.breaker._state_storage.name == "memory"

    def test_new_with_custom_fail_max(self):
        """CircuitBreaker: it should support a custom maximum number of
        failures.
        """
        self.breaker = CircuitBreaker(fail_max=10)
        assert self.breaker.fail_counter == 0
        assert self.breaker.reset_timeout == 60
        assert self.breaker.fail_max == 10
        assert self.breaker.excluded_exceptions == ()
        assert self.breaker.listeners == ()
        assert self.breaker._state_storage.name == "memory"

    def test_new_with_custom_excluded_exceptions(self):
        """CircuitBreaker: it should support a custom list of excluded
        exceptions.
        """
        self.breaker = CircuitBreaker(exclude=[Exception])
        assert self.breaker.fail_counter == 0
        assert self.breaker.reset_timeout == 60
        assert self.breaker.fail_max == 5
        assert (Exception,) == self.breaker.excluded_exceptions
        assert self.breaker.listeners == ()
        assert self.breaker._state_storage.name == "memory"

    def test_fail_max_setter(self):
        """CircuitBreaker: it should allow the user to set a new value for
        'fail_max'.
        """
        assert self.breaker.fail_max == 5
        self.breaker.fail_max = 10
        assert self.breaker.fail_max == 10

    def test_reset_timeout_setter(self):
        """CircuitBreaker: it should allow the user to set a new value for
        'reset_timeout'.
        """
        assert self.breaker.reset_timeout == 60
        self.breaker.reset_timeout = 30
        assert self.breaker.reset_timeout == 30

    def test_call_with_no_args(self):
        """CircuitBreaker: it should be able to invoke functions with no-args."""

        def func():
            return True

        assert self.breaker.call(func)

    def test_call_with_args(self):
        """CircuitBreaker: it should be able to invoke functions with args."""

        def func(arg1, arg2):
            return [arg1, arg2]

        assert [42, "abc"], self.breaker.call(func, 42 == "abc")

    def test_call_with_kwargs(self):
        """CircuitBreaker: it should be able to invoke functions with kwargs."""

        def func(**kwargs):
            return kwargs

        assert {"a": 1, "b": 2}, self.breaker.call(func, a=1, b=2)

    @testing.gen_test
    def test_call_async_with_no_args(self):
        """CircuitBreaker: it should be able to invoke async functions with no-args."""

        @gen.coroutine
        def func():
            return True

        ret = yield self.breaker.call(func)
        assert ret

    @testing.gen_test
    def test_call_async_with_args(self):
        """CircuitBreaker: it should be able to invoke async functions with args."""

        @gen.coroutine
        def func(arg1, arg2):
            return [arg1, arg2]

        ret = yield self.breaker.call(func, 42, "abc")
        assert [42, "abc"] == ret

    @testing.gen_test
    def test_call_async_with_kwargs(self):
        """CircuitBreaker: it should be able to invoke async functions with kwargs."""

        @gen.coroutine
        def func(**kwargs):
            return kwargs

        ret = yield self.breaker.call(func, a=1, b=2)
        assert {"a": 1, "b": 2} == ret

    def test_add_listener(self):
        """CircuitBreaker: it should allow the user to add a listener at a
        later time.
        """
        assert self.breaker.listeners == ()

        first = CircuitBreakerListener()
        self.breaker.add_listener(first)
        assert (first,) == self.breaker.listeners

        second = CircuitBreakerListener()
        self.breaker.add_listener(second)
        assert (first, second) == self.breaker.listeners

    def test_add_listeners(self):
        """CircuitBreaker: it should allow the user to add listeners at a
        later time.
        """
        first, second = CircuitBreakerListener(), CircuitBreakerListener()
        self.breaker.add_listeners(first, second)
        assert (first, second) == self.breaker.listeners

    def test_remove_listener(self):
        """CircuitBreaker: it should allow the user to remove a listener."""
        first = CircuitBreakerListener()
        self.breaker.add_listener(first)
        assert (first,) == self.breaker.listeners

        self.breaker.remove_listener(first)
        assert self.breaker.listeners == ()

    def test_excluded_exceptions(self):
        """CircuitBreaker: it should ignore specific exceptions."""
        self.breaker = CircuitBreaker(exclude=[LookupError])

        def err_1():
            raise NotImplementedError

        def err_2():
            raise LookupError

        def err_3():
            raise KeyError

        with pytest.raises(NotImplementedError):
            self.breaker.call(err_1)
        assert self.breaker.fail_counter == 1

        # LookupError is not considered a system error
        with pytest.raises(LookupError):
            self.breaker.call(err_2)
        assert self.breaker.fail_counter == 0

        with pytest.raises(NotImplementedError):
            self.breaker.call(err_1)
        assert self.breaker.fail_counter == 1

        # Should consider subclasses as well (KeyError is a subclass of
        # LookupError)
        with pytest.raises(KeyError):
            self.breaker.call(err_3)
        assert self.breaker.fail_counter == 0

    def test_excluded_callable_exceptions(self):
        """CircuitBreaker: it should ignore specific exceptions that return true from a filtering callable."""

        class TestException(Exception):
            def __init__(self, value):
                self.value = value

        filter_function = lambda e: type(e) == TestException and e.value == "good"
        self.breaker = CircuitBreaker(exclude=[filter_function])

        def err_1():
            raise TestException("bad")

        def err_2():
            raise TestException("good")

        def err_3():
            raise NotImplementedError

        with pytest.raises(TestException):
            self.breaker.call(err_1)
        assert self.breaker.fail_counter == 1

        with pytest.raises(TestException):
            self.breaker.call(err_2)
        assert self.breaker.fail_counter == 0

        with pytest.raises(NotImplementedError):
            self.breaker.call(err_3)
        assert self.breaker.fail_counter == 1

    def test_excluded_callable_and_types_exceptions(self):
        """CircuitBreaker: it should allow a mix of exclusions that includes both filter functions and types."""

        class TestException(Exception):
            def __init__(self, value):
                self.value = value

        filter_function = lambda e: type(e) == TestException and e.value == "good"
        self.breaker = CircuitBreaker(exclude=[filter_function, LookupError])

        def err_1():
            raise TestException("bad")

        def err_2():
            raise TestException("good")

        def err_3():
            raise NotImplementedError

        def err_4():
            raise LookupError

        with pytest.raises(TestException):
            self.breaker.call(err_1)
        assert self.breaker.fail_counter == 1

        with pytest.raises(TestException):
            self.breaker.call(err_2)
        assert self.breaker.fail_counter == 0

        with pytest.raises(NotImplementedError):
            self.breaker.call(err_3)
        assert self.breaker.fail_counter == 1

        with pytest.raises(LookupError):
            self.breaker.call(err_4)
        assert self.breaker.fail_counter == 0

    def test_add_excluded_exception(self):
        """CircuitBreaker: it should allow the user to exclude an exception at a
        later time.
        """
        assert self.breaker.excluded_exceptions == ()

        self.breaker.add_excluded_exception(NotImplementedError)
        assert (NotImplementedError,) == self.breaker.excluded_exceptions

        self.breaker.add_excluded_exception(Exception)
        assert (NotImplementedError, Exception) == self.breaker.excluded_exceptions

    def test_add_excluded_exceptions(self):
        """CircuitBreaker: it should allow the user to exclude exceptions at a
        later time.
        """
        self.breaker.add_excluded_exceptions(NotImplementedError, Exception)
        assert (NotImplementedError, Exception) == self.breaker.excluded_exceptions

    def test_remove_excluded_exception(self):
        """CircuitBreaker: it should allow the user to remove an excluded
        exception.
        """
        self.breaker.add_excluded_exception(NotImplementedError)
        assert (NotImplementedError,) == self.breaker.excluded_exceptions

        self.breaker.remove_excluded_exception(NotImplementedError)
        assert self.breaker.excluded_exceptions == ()

    def test_decorator(self):
        """CircuitBreaker: it should be a decorator."""

        @self.breaker
        def suc(value):
            "Docstring"
            return value

        @self.breaker
        def err(value):
            "Docstring"
            raise NotImplementedError

        assert suc.__doc__ == "Docstring"
        assert err.__doc__ == "Docstring"
        assert suc.__name__ == "suc"
        assert err.__name__ == "err"

        with pytest.raises(NotImplementedError):
            err(True)
        assert self.breaker.fail_counter == 1

        assert suc(True)
        assert self.breaker.fail_counter == 0

    @testing.gen_test
    def test_decorator_call_future(self):
        """CircuitBreaker: it should be a decorator."""

        @self.breaker(__pybreaker_call_async=True)
        @gen.coroutine
        def suc(value):
            "Docstring"
            raise gen.Return(value)

        @self.breaker(__pybreaker_call_async=True)
        @gen.coroutine
        def err(value):
            "Docstring"
            raise NotImplementedError

        assert suc.__doc__ == "Docstring"
        assert err.__doc__ == "Docstring"
        assert suc.__name__ == "suc"
        assert err.__name__ == "err"

        with pytest.raises(NotImplementedError):
            yield err(True)

        assert self.breaker.fail_counter == 1

        ret = yield suc(True)
        assert ret
        assert self.breaker.fail_counter == 0

    @mock.patch("pybreaker.HAS_TORNADO_SUPPORT", False)
    def test_no_tornado_raises(self):
        with pytest.raises(ImportError):

            def func():
                return True

            self.breaker(func, __pybreaker_call_async=True)

    def test_name(self):
        """CircuitBreaker: it should allow an optional name to be set and
        retrieved.
        """
        name = "test_breaker"
        self.breaker = CircuitBreaker(name=name)
        assert self.breaker.name == name

        name = "breaker_test"
        self.breaker.name = name
        assert self.breaker.name == name

    def test_success_threshold_default_behavior(self):
        """CircuitBreaker: it should maintain backward compatibility with default success_threshold=1."""
        self.breaker = CircuitBreaker(fail_max=3, reset_timeout=0.1, **self.breaker_kwargs)

        def fun():
            return True

        def err():
            raise NotImplementedError

        # Open the circuit
        for i in range(3):
            if i < 2:
                with pytest.raises(NotImplementedError):
                    self.breaker.call(err)
            else:
                with pytest.raises(CircuitBreakerError):
                    self.breaker.call(err)

        # Wait for timeout to enter half-open state
        sleep(0.2)

        # Single successful call should close the circuit (default behavior)
        assert self.breaker.call(fun)
        assert self.breaker.current_state == "closed"
        assert self.breaker.success_counter == 0  # Should be reset when closed

    def test_success_threshold_multiple_successes_required(self):
        """CircuitBreaker: it should require multiple successful calls before closing when success_threshold > 1."""
        self.breaker = CircuitBreaker(fail_max=3, reset_timeout=0.1, success_threshold=3, **self.breaker_kwargs)

        def fun():
            return True

        def err():
            raise NotImplementedError

        # Open the circuit
        for i in range(3):
            if i < 2:
                with pytest.raises(NotImplementedError):
                    self.breaker.call(err)
            else:
                with pytest.raises(CircuitBreakerError):
                    self.breaker.call(err)

        # Wait for timeout to enter half-open state
        sleep(0.2)

        # First successful call should not close the circuit
        assert self.breaker.call(fun)
        assert self.breaker.current_state == "half-open"
        assert self.breaker.success_counter == 1

        # Second successful call should not close the circuit
        assert self.breaker.call(fun)
        assert self.breaker.current_state == "half-open"
        assert self.breaker.success_counter == 2

        # Third successful call should close the circuit
        assert self.breaker.call(fun)
        assert self.breaker.current_state == "closed"
        assert self.breaker.success_counter == 0  # Should be reset when closed

    def test_success_threshold_failure_resets_counter(self):
        """CircuitBreaker: it should reset success counter when a failure occurs in half-open state."""
        self.breaker = CircuitBreaker(fail_max=3, reset_timeout=0.1, success_threshold=3, **self.breaker_kwargs)

        def fun():
            return True

        def err():
            raise NotImplementedError

        # Open the circuit
        for i in range(3):
            if i < 2:
                with pytest.raises(NotImplementedError):
                    self.breaker.call(err)
            else:
                with pytest.raises(CircuitBreakerError):
                    self.breaker.call(err)

        # Wait for timeout to enter half-open state
        sleep(0.2)

        # First successful call
        assert self.breaker.call(fun)
        assert self.breaker.current_state == "half-open"
        assert self.breaker.success_counter == 1

        # Second successful call
        assert self.breaker.call(fun)
        assert self.breaker.current_state == "half-open"
        assert self.breaker.success_counter == 2

        # Failure should reset success counter and open circuit
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(err)
        assert self.breaker.current_state == "open"
        assert self.breaker.success_counter == 0  # Should be reset when opened

    def test_success_threshold_property(self):
        """CircuitBreaker: it should support getting and setting success_threshold."""
        self.breaker = CircuitBreaker(success_threshold=5)
        assert self.breaker.success_threshold == 5

        self.breaker.success_threshold = 10
        assert self.breaker.success_threshold == 10

    def test_success_threshold_redis_storage(self):
        """CircuitBreaker: it should work correctly with Redis storage."""
        # This test requires Redis to be running
        try:
            import redis

            redis_client = redis.Redis()
            redis_client.ping()
        except (ImportError, redis.ConnectionError):
            pytest.skip("Redis not available")

        storage = CircuitRedisStorage(STATE_CLOSED, redis_client)
        self.breaker = CircuitBreaker(fail_max=3, reset_timeout=0.1, success_threshold=2, state_storage=storage)

        def fun():
            return True

        def err():
            raise NotImplementedError

        # Open the circuit
        for i in range(3):
            if i < 2:
                with pytest.raises(NotImplementedError):
                    self.breaker.call(err)
            else:
                with pytest.raises(CircuitBreakerError):
                    self.breaker.call(err)

        # Wait for timeout to enter half-open state
        sleep(0.2)

        # First successful call should not close the circuit
        assert self.breaker.call(fun)
        assert self.breaker.current_state == "half-open"
        assert self.breaker.success_counter == 1

        # Second successful call should close the circuit
        assert self.breaker.call(fun)
        assert self.breaker.current_state == "closed"
        assert self.breaker.success_counter == 0


class CircuitBreakerTestCase(
    testing.AsyncTestCase,
    CircuitBreakerStorageBasedTestCase,
    CircuitBreakerConfigurationTestCase,
):
    """Tests for the CircuitBreaker class."""

    def setUp(self):
        super(CircuitBreakerTestCase, self).setUp()
        self.breaker_kwargs = {}
        self.breaker = CircuitBreaker()

    def test_create_new_state__bad_state(self):
        with pytest.raises(ValueError):
            self.breaker._create_new_state("foo")

    @mock.patch("pybreaker.CircuitOpenState")
    def test_notify_not_called_on_init(self, open_state):
        storage = CircuitMemoryStorage("open")
        breaker = CircuitBreaker(state_storage=storage)
        open_state.assert_called_once_with(breaker, prev_state=None, notify=False)

    @mock.patch("pybreaker.CircuitOpenState")
    def test_notify_called_on_state_change(self, open_state):
        storage = CircuitMemoryStorage("closed")
        breaker = CircuitBreaker(state_storage=storage)
        prev_state = breaker.state
        breaker.state = "open"
        open_state.assert_called_once_with(breaker, prev_state=prev_state, notify=True)

    def test_failure_count_not_reset_during_creation(self):
        for state in (STATE_OPEN, STATE_CLOSED, STATE_HALF_OPEN):
            storage = CircuitMemoryStorage(state)
            storage.increment_counter()

            breaker = CircuitBreaker(state_storage=storage)
            assert breaker.state.name == state
            assert breaker.fail_counter == 1

    def test_state_opened_at_not_reset_during_creation(self):
        for state in (STATE_OPEN, STATE_CLOSED, STATE_HALF_OPEN):
            storage = CircuitMemoryStorage(state)
            now = datetime.now()
            storage.opened_at = now

            breaker = CircuitBreaker(state_storage=storage)
            assert breaker.state.name == state
            assert storage.opened_at == now


import logging

import fakeredis
from redis.exceptions import RedisError


class CircuitBreakerRedisTestCase(unittest.TestCase, CircuitBreakerStorageBasedTestCase):
    """Tests for the CircuitBreaker class."""

    def setUp(self):
        self.redis = fakeredis.FakeStrictRedis()
        self.breaker_kwargs = {"state_storage": CircuitRedisStorage("closed", self.redis)}
        self.breaker = CircuitBreaker(**self.breaker_kwargs)

    def tearDown(self):
        self.redis.flushall()

    def test_namespace(self):
        self.redis.flushall()
        self.breaker_kwargs = {"state_storage": CircuitRedisStorage("closed", self.redis, namespace="my_app")}
        self.breaker = CircuitBreaker(**self.breaker_kwargs)

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        keys = self.redis.keys()
        assert len(keys) == 3  # fail_counter, success_counter, state
        assert keys[0].decode("utf-8").startswith("my_app")
        assert keys[1].decode("utf-8").startswith("my_app")
        assert keys[2].decode("utf-8").startswith("my_app")

    def test_fallback_state(self):
        logger = logging.getLogger("pybreaker")
        logger.setLevel(logging.FATAL)
        self.breaker_kwargs = {
            "state_storage": CircuitRedisStorage("closed", self.redis, fallback_circuit_state="open")
        }
        self.breaker = CircuitBreaker(**self.breaker_kwargs)

        def func(k):
            raise RedisError()

        with mock.patch.object(self.redis, "get", new=func):
            state = self.breaker.state
            assert state.name == "open"

    def test_missing_state(self):
        """CircuitBreakerRedis: If state on Redis is missing, it should set the
        fallback circuit state and reset the fail counter to 0.
        """
        self.breaker_kwargs = {
            "state_storage": CircuitRedisStorage("closed", self.redis, fallback_circuit_state="open")
        }
        self.breaker = CircuitBreaker(**self.breaker_kwargs)

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 1

        with mock.patch.object(self.redis, "get", new=lambda k: None):
            state = self.breaker.state
            assert state.name == "open"
            assert self.breaker.fail_counter == 0

    def test_cluster_mode(self):
        self.redis.flushall()

        storage = CircuitRedisStorage(STATE_OPEN, self.redis, namespace="my_app", cluster_mode=True)
        breaker_kwargs = {"state_storage": storage}

        now = datetime.now()
        storage.opened_at = now

        now_str = now.strftime("%Y-%m-%d-%H:%M:%S")
        opened_at = storage.opened_at.strftime("%Y-%m-%d-%H:%M:%S")

        breaker = CircuitBreaker(**breaker_kwargs)
        assert breaker.state.name == STATE_OPEN
        assert opened_at == now_str


import threading
from types import MethodType


class ConflictInjectingMemoryStorage(CircuitMemoryStorage):
    """Shared test double used to arrange version conflicts and retries.

    It behaves exactly like the memory storage, except a configurable number
    of conditional operations deliberately fail with a version conflict
    before succeeding. Because the same instance is shared between several
    `CircuitBreaker` objects, it emulates another breaker instance winning
    the version race.
    """

    def __init__(self, state: str):
        super().__init__(state)
        self.conflict_once = 0
        self.conditional_calls = 0
        self.snapshot_reads = 0
        self.snapshot_error: Exception | None = None
        self.cas_error: Exception | None = None
        self.counter_error: Exception | None = None

    def get_snapshot(self):
        self.snapshot_reads += 1
        if self.snapshot_error is not None:
            raise self.snapshot_error
        return super().get_snapshot()

    def compare_and_swap_state(self, expected_version, new_state, opened_at, fail_counter, success_counter):
        self.conditional_calls += 1
        if self.cas_error is not None:
            raise self.cas_error
        if self.conflict_once > 0:
            self.conflict_once -= 1
            # Another writer moved the version before our CAS is applied.
            with self._lock:
                self._version += 1
            raise CircuitBreakerStorageVersionConflictError(self.name, expected_version)
        return super().compare_and_swap_state(
            expected_version, new_state, opened_at, fail_counter, success_counter
        )

    def conditional_increment_counter(self, expected_version):
        self.conditional_calls += 1
        if self.counter_error is not None:
            raise self.counter_error
        if self.conflict_once > 0:
            self.conflict_once -= 1
            with self._lock:
                self._version += 1
            raise CircuitBreakerStorageVersionConflictError(self.name, expected_version)
        return super().conditional_increment_counter(expected_version)

    def conditional_increment_success_counter(self, expected_version, reset_fail_counter=False):
        self.conditional_calls += 1
        if self.counter_error is not None:
            raise self.counter_error
        if self.conflict_once > 0:
            self.conflict_once -= 1
            with self._lock:
                self._version += 1
            raise CircuitBreakerStorageVersionConflictError(self.name, expected_version)
        return super().conditional_increment_success_counter(expected_version, reset_fail_counter)

    def conditional_reset_counter(self, expected_version):
        self.conditional_calls += 1
        if self.counter_error is not None:
            raise self.counter_error
        if self.conflict_once > 0:
            self.conflict_once -= 1
            with self._lock:
                self._version += 1
            raise CircuitBreakerStorageVersionConflictError(self.name, expected_version)
        return super().conditional_reset_counter(expected_version)


class CircuitMemoryStorageConditionalTestCase(unittest.TestCase):
    """Tests for the version-conditional protocol on the memory storage."""

    def setUp(self):
        self.storage = CircuitMemoryStorage(STATE_CLOSED)

    def test_supports_conditional_updates(self):
        assert self.storage.supports_conditional_updates is True

    def test_snapshot_is_consistent_and_bumps_version(self):
        snapshot = self.storage.get_snapshot()
        assert snapshot.state == STATE_CLOSED
        assert snapshot.fail_counter == 0
        assert snapshot.success_counter == 0
        assert snapshot.opened_at is None
        assert snapshot.version == 0

        next_snapshot = self.storage.conditional_increment_counter(snapshot.version)
        assert next_snapshot.fail_counter == 1
        assert next_snapshot.version == 1
        # The returned snapshot reflects the whole state consistently.
        assert next_snapshot.state == snapshot.state

    def test_compare_and_swap_state_commits_whole_round_atomically(self):
        opened = datetime.now(UTC)
        snapshot = self.storage.get_snapshot()
        new_snapshot = self.storage.compare_and_swap_state(
            snapshot.version, STATE_OPEN, opened_at=opened, fail_counter=5, success_counter=0
        )
        assert new_snapshot.state == STATE_OPEN
        assert new_snapshot.opened_at == opened
        assert new_snapshot.fail_counter == 5
        assert new_snapshot.success_counter == 0
        assert new_snapshot.version == snapshot.version + 1

        # The independent views agree with the committed consistent snapshot.
        assert self.storage.state == STATE_OPEN
        assert self.storage.opened_at == opened
        assert self.storage.counter == 5

    def test_stale_version_is_rejected(self):
        stale = self.storage.get_snapshot()
        # Some other writer advances the state in between.
        self.storage.increment_counter()
        with pytest.raises(CircuitBreakerStorageVersionConflictError):
            self.storage.conditional_increment_counter(stale.version)
        # The rejected attempt must not have mutated anything.
        assert self.storage.counter == 1

    def test_stale_compare_and_swap_leaves_no_partial_data(self):
        stale = self.storage.get_snapshot()
        self.storage.compare_and_swap_state(
            self.storage.get_snapshot().version, STATE_OPEN, opened_at=datetime.now(UTC),
            fail_counter=3, success_counter=0,
        )
        with pytest.raises(CircuitBreakerStorageVersionConflictError):
            self.storage.compare_and_swap_state(
                stale.version, STATE_CLOSED, opened_at=None, fail_counter=0, success_counter=0
            )
        # State, counters and opened moment are untouched after the rejection.
        current = self.storage.get_snapshot()
        assert current.state == STATE_OPEN
        assert current.fail_counter == 3
        assert current.success_counter == 0
        assert current.opened_at is not None


class CircuitBreakerConditionalModeTestCase(unittest.TestCase):
    """Tests for a breaker driving storage through version-conditional ops."""

    def setUp(self):
        self.storage = ConflictInjectingMemoryStorage(STATE_CLOSED)
        self.breaker = CircuitBreaker(fail_max=3, state_storage=self.storage, conditional_updates=True)

    def test_conditional_mode_flag(self):
        assert self.breaker.conditional_updates is True

    def test_legacy_mode_does_not_use_conditional_ops(self):
        storage = ConflictInjectingMemoryStorage(STATE_CLOSED)
        breaker = CircuitBreaker(fail_max=3, state_storage=storage)
        assert breaker.conditional_updates is False

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            breaker.call(func)
        assert storage.conditional_calls == 0

    def test_failure_round_uses_single_conditional_operation(self):
        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 1
        assert self.storage.conditional_calls == 1

    def test_conflict_is_retried_and_update_is_not_lost(self):
        # Arrange one conflicting writer before the conditional update lands.
        self.storage.conflict_once = 1

        def func():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            self.breaker.call(func)
        assert self.breaker.fail_counter == 1
        # One rejected attempt plus the successful retry, and an extra
        # snapshot read to re-apply the decision against the new version.
        assert self.storage.conditional_calls == 2
        assert self.storage.snapshot_reads >= 2

    def test_exhausted_retries_raise_version_conflict(self):
        # Every conditional attempt conflicts forever.
        self.storage.conflict_once = 10**6

        def func():
            raise NotImplementedError

        with pytest.raises(CircuitBreakerStorageVersionConflictError):
            self.breaker.call(func)
        # Bounded retries stop rather than looping indefinitely.
        assert self.storage.conditional_calls == MAX_CONDITIONAL_ATTEMPTS

    def test_shared_storage_does_not_lose_updates_between_breakers(self):
        other_breaker = CircuitBreaker(fail_max=3, state_storage=self.storage, conditional_updates=True)

        def func():
            raise NotImplementedError

        def fail(breaker):
            with pytest.raises(NotImplementedError):
                breaker.call(func)

        # First breaker hits an arranged conflict, then both keep failing.
        self.storage.conflict_once = 1
        fail(self.breaker)
        fail(other_breaker)
        # The third failure trips the shared circuit.
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)

        # Both breakers observe the same shared, consistent state.
        assert self.breaker.fail_counter == 3
        assert other_breaker.fail_counter == 3
        assert self.breaker.current_state == STATE_OPEN
        assert other_breaker.current_state == STATE_OPEN
        assert self.storage.get_snapshot().fail_counter == 3

    def test_trip_is_one_atomic_conditional_round(self):
        def func():
            raise NotImplementedError

        # A conflict injected on the tripping round must be retried, and the
        # trip (state + opened moment + counters) still lands atomically.
        self.storage.conflict_once = 1
        for _ in range(2):
            with pytest.raises(NotImplementedError):
                self.breaker.call(func)
        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)

        snapshot = self.storage.get_snapshot()
        assert snapshot.state == STATE_OPEN
        assert snapshot.fail_counter == 3
        assert snapshot.success_counter == 0
        assert snapshot.opened_at is not None

    def test_success_in_half_open_closes_in_one_round(self):
        self.breaker.half_open()

        def func():
            return True

        assert self.breaker.call(func)
        snapshot = self.storage.get_snapshot()
        assert snapshot.state == STATE_CLOSED
        assert snapshot.fail_counter == 0
        assert snapshot.success_counter == 0
        assert snapshot.opened_at is None

    def test_success_threshold_accumulates_then_closes(self):
        breaker = CircuitBreaker(
            fail_max=3, success_threshold=3, state_storage=self.storage, conditional_updates=True
        )
        breaker.half_open()

        def func():
            return True

        breaker.call(func)
        assert self.storage.get_snapshot().success_counter == 1
        assert breaker.current_state == STATE_HALF_OPEN
        breaker.call(func)
        assert self.storage.get_snapshot().success_counter == 2
        assert breaker.current_state == STATE_HALF_OPEN
        breaker.call(func)
        snapshot = self.storage.get_snapshot()
        assert snapshot.state == STATE_CLOSED
        assert snapshot.success_counter == 0

    def test_failed_trial_call_reopens_atomically(self):
        self.breaker.half_open()

        def func():
            raise NotImplementedError

        with pytest.raises(CircuitBreakerError):
            self.breaker.call(func)
        snapshot = self.storage.get_snapshot()
        assert snapshot.state == STATE_OPEN
        assert snapshot.opened_at is not None
        assert snapshot.success_counter == 0

    def test_storage_error_is_distinct_from_dependency_error(self):
        class DependencyError(Exception):
            pass

        class HardStorageError(CircuitBreakerStorageError):
            pass

        # The storage fails while committing the round; this must surface as a
        # storage error, never as the dependency's exception, and no partial
        # round may be committed.
        self.storage.counter_error = HardStorageError("backend down")
        before = self.storage.get_snapshot()

        def func():
            raise DependencyError

        with pytest.raises(HardStorageError):
            self.breaker.call(func)

        after = self.storage.get_snapshot()
        assert after.state == before.state
        assert after.fail_counter == before.fail_counter
        assert after.version == before.version

    def test_snapshot_read_error_aborts_before_the_dependency_runs(self):
        class HardStorageError(CircuitBreakerStorageError):
            pass

        # Make the cached state stale so that `state` must consult the shared
        # storage (via a snapshot read) before the call can proceed.
        self.storage.state = STATE_OPEN
        self.storage.snapshot_error = HardStorageError("cannot read state")
        called = False

        def func():
            nonlocal called
            called = True
            return True

        with pytest.raises(HardStorageError):
            self.breaker.call(func)
        assert called is False

    def test_manual_transitions_are_conditional(self):
        opened = self.breaker.open()
        assert opened is True
        assert self.storage.get_snapshot().state == STATE_OPEN
        self.breaker.half_open()
        assert self.storage.get_snapshot().state == STATE_HALF_OPEN
        self.breaker.close()
        snapshot = self.storage.get_snapshot()
        assert snapshot.state == STATE_CLOSED
        assert snapshot.fail_counter == 0
        assert snapshot.success_counter == 0

    def test_remote_storage_is_explicitly_rejected(self):
        import fakeredis

        redis_storage = CircuitRedisStorage(STATE_CLOSED, fakeredis.FakeStrictRedis())
        assert redis_storage.supports_conditional_updates is False
        with pytest.raises(CircuitBreakerStorageUnsupportedError):
            CircuitBreaker(state_storage=redis_storage, conditional_updates=True)
        # The rejected mode never touched any conditional operation.
        with pytest.raises(CircuitBreakerStorageUnsupportedError):
            redis_storage.get_snapshot()
        with pytest.raises(CircuitBreakerStorageUnsupportedError):
            redis_storage.compare_and_swap_state(0, STATE_OPEN, None, 1, 0)


class CircuitBreakerConditionalConcurrencyTestCase(unittest.TestCase):
    """Shared-storage conflict/retry behavior across breaker instances."""

    def _start_threads(self, target, n):
        threads = [threading.Thread(target=target) for _ in range(n)]
        [t.start() for t in threads]
        [t.join() for t in threads]
        return threads

    def test_concurrent_failures_never_overwrite_the_counter(self):
        storage = ConflictInjectingMemoryStorage(STATE_CLOSED)
        breakers = [
            CircuitBreaker(fail_max=10000, reset_timeout=60, state_storage=storage, conditional_updates=True)
            for _ in range(4)
        ]

        class SpecificException(Exception):
            pass

        def trigger_error(index):
            breaker = breakers[index]
            for _ in range(250):
                try:
                    breaker.call(_raise_specific)
                except SpecificException:
                    pass
                except CircuitBreakerError:
                    pass

        def _raise_specific():
            raise SpecificException

        threads = [threading.Thread(target=trigger_error, args=(i,)) for i in range(4)]
        [t.start() for t in threads]
        [t.join() for t in threads]
        # 4 threads x 250 failures, none may be lost to a read-then-write race.
        assert storage.get_snapshot().fail_counter == 1000

    def test_conflicting_writers_both_commit_their_rounds(self):
        storage = ConflictInjectingMemoryStorage(STATE_CLOSED)
        breaker_a = CircuitBreaker(fail_max=20, state_storage=storage, conditional_updates=True)
        breaker_b = CircuitBreaker(fail_max=20, state_storage=storage, conditional_updates=True)

        def fail():
            raise NotImplementedError

        # Every operation of breaker A conflicts once, simulating breaker B
        # winning the race each time.
        storage.conflict_once = 5
        for _ in range(5):
            with pytest.raises(NotImplementedError):
                breaker_a.call(fail)
            with pytest.raises(NotImplementedError):
                breaker_b.call(fail)

        assert storage.get_snapshot().fail_counter == 10
        assert breaker_a.fail_counter == 10
        assert breaker_b.fail_counter == 10

    def test_second_breaker_observes_remote_open_and_rejects_calls(self):
        storage = ConflictInjectingMemoryStorage(STATE_CLOSED)
        breaker_a = CircuitBreaker(fail_max=2, reset_timeout=60, state_storage=storage, conditional_updates=True)
        breaker_b = CircuitBreaker(fail_max=2, reset_timeout=60, state_storage=storage, conditional_updates=True)

        def fail():
            raise NotImplementedError

        with pytest.raises(NotImplementedError):
            breaker_a.call(fail)
        with pytest.raises(CircuitBreakerError):
            breaker_a.call(fail)

        # Breaker B never made a call, yet its stale cached 'closed' state is
        # refreshed from the shared snapshot and the call is rejected without
        # running the dependency.
        called = []

        def dependency():
            called.append(True)
            return True

        with pytest.raises(CircuitBreakerError):
            breaker_b.call(dependency)
        assert called == []
        assert breaker_b.current_state == STATE_OPEN

    def test_half_open_transition_is_single_winner_under_conflict(self):
        storage = ConflictInjectingMemoryStorage(STATE_OPEN)
        storage.opened_at = datetime.now(UTC)
        breaker = CircuitBreaker(fail_max=2, reset_timeout=0.01, state_storage=storage,
                                 conditional_updates=True)
        sleep(0.02)

        changes = []

        class StateListener(CircuitBreakerListener):
            def state_change(self, cb, old_state, new_state):
                changes.append((old_state.name if old_state else None, new_state.name))

        breaker.add_listener(StateListener())
        # Arrange a conflict exactly when the open -> half-open CAS runs.
        storage.conflict_once = 1

        def succeed():
            return True

        # The conflict is retried against the newer version and the trial
        # call still succeeds exactly once.
        assert breaker.call(succeed)
        assert storage.get_snapshot().state == STATE_CLOSED
        assert ("open", "half-open") in changes
        assert ("half-open", "closed") in changes
        # No duplicated half-open notifications after retrying the CAS.
        assert changes.count(("open", "half-open")) == 1


class CircuitBreakerThreadsTestCase(unittest.TestCase):
    """Tests to reproduce common synchronization errors on CircuitBreaker class."""

    def setUp(self):
        self.breaker = CircuitBreaker(fail_max=3000, reset_timeout=1)

    def _start_threads(self, target, n):
        """Starts `n` threads that calls `target` and waits for them to finish."""
        threads = [threading.Thread(target=target) for i in range(n)]
        [t.start() for t in threads]
        [t.join() for t in threads]

    def _mock_function(self, obj, func):
        """Replaces a bounded function in `self.breaker` by another."""
        setattr(obj, func.__name__, MethodType(func, self.breaker))

    def test_fail_thread_safety(self):
        """CircuitBreaker: it should compute a failed call atomically to
        avoid race conditions.
        """

        # Create a specific exception to avoid masking other errors
        class SpecificException(Exception):
            pass

        @self.breaker
        def err():
            raise SpecificException

        def trigger_error():
            for n in range(500):
                try:
                    err()
                except SpecificException:
                    pass

        def _inc_counter(self):
            c = self._state_storage._fail_counter
            sleep(0.00005)
            self._state_storage._fail_counter = c + 1

        self._mock_function(self.breaker, _inc_counter)
        self._start_threads(trigger_error, 3)
        assert self.breaker.fail_counter == 1500

    def test_success_thread_safety(self):
        """CircuitBreaker: it should compute a successful call atomically
        to avoid race conditions.
        """

        @self.breaker
        def suc():
            return True

        def trigger_success():
            for n in range(500):
                suc()

        class SuccessListener(CircuitBreakerListener):
            def success(self, cb):
                c = 0
                if hasattr(cb, "_success_counter"):
                    c = cb._success_counter
                sleep(0.00005)
                cb._success_counter = c + 1

        self.breaker.add_listener(SuccessListener())
        self._start_threads(trigger_success, 3)
        assert self.breaker._success_counter == 1500

    def test_half_open_thread_safety(self):
        """CircuitBreaker: it should allow only one trial call when the
        circuit is half-open.
        """
        self.breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01)

        self.breaker.open()
        sleep(0.01)

        @self.breaker
        def err():
            raise Exception

        def trigger_failure():
            try:
                err()
            except:
                pass

        class StateListener(CircuitBreakerListener):
            def __init__(self):
                self._count = 0

            def before_call(self, cb, fun, *args, **kwargs):
                sleep(0.00005)

            def state_change(self, cb, old_state, new_state):
                if new_state.name == "half-open":
                    self._count += 1

        state_listener = StateListener()
        self.breaker.add_listener(state_listener)

        self._start_threads(trigger_failure, 5)
        assert state_listener._count == 1

    def test_fail_max_thread_safety(self):
        """CircuitBreaker: it should not allow more failed calls than
        'fail_max' setting.
        """

        @self.breaker
        def err():
            raise Exception

        def trigger_error():
            for i in range(2000):
                try:
                    err()
                except:
                    pass

        class SleepListener(CircuitBreakerListener):
            def before_call(self, cb, func, *args, **kwargs):
                sleep(0.00005)

        self.breaker.add_listener(SleepListener())
        self._start_threads(trigger_error, 3)
        assert self.breaker.fail_max == self.breaker.fail_counter


class CircuitBreakerRedisConcurrencyTestCase(unittest.TestCase):
    """Tests to reproduce common concurrency between different machines
    connecting to redis. This is simulated locally using threads.
    """

    def setUp(self):
        self.redis = fakeredis.FakeStrictRedis()
        self.breaker_kwargs = {
            "fail_max": 3000,
            "reset_timeout": 20,
            "state_storage": CircuitRedisStorage("closed", self.redis),
        }
        self.breaker = CircuitBreaker(**self.breaker_kwargs)

    def tearDown(self):
        self.redis.flushall()

    def _start_threads(self, target, n):
        """Starts `n` threads that calls `target` and waits for them to finish."""
        threads = [threading.Thread(target=target) for i in range(n)]
        [t.start() for t in threads]
        [t.join() for t in threads]

    def _mock_function(self, obj, func):
        """Replaces a bounded function in `self.breaker` by another."""
        setattr(obj, func.__name__, MethodType(func, self.breaker))

    def test_fail_thread_safety(self):
        """CircuitBreaker: it should compute a failed call atomically to
        avoid race conditions.
        """

        # Create a specific exception to avoid masking other errors
        class SpecificException(Exception):
            pass

        @self.breaker
        def err():
            raise SpecificException

        def trigger_error():
            for n in range(500):
                try:
                    err()
                except SpecificException:
                    pass

        def _inc_counter(self):
            sleep(0.00005)
            self._state_storage.increment_counter()

        self._mock_function(self.breaker, _inc_counter)
        self._start_threads(trigger_error, 3)
        assert self.breaker.fail_counter == 1500

    def test_success_thread_safety(self):
        """CircuitBreaker: it should compute a successful call atomically
        to avoid race conditions.
        """

        @self.breaker
        def suc():
            return True

        def trigger_success():
            for n in range(500):
                suc()

        class SuccessListener(CircuitBreakerListener):
            def success(self, cb):
                c = 0
                if hasattr(cb, "_success_counter"):
                    c = cb._success_counter
                sleep(0.00005)
                cb._success_counter = c + 1

        self.breaker.add_listener(SuccessListener())
        self._start_threads(trigger_success, 3)
        assert self.breaker._success_counter == 1500

    def test_half_open_thread_safety(self):
        """CircuitBreaker: it should allow only one trial call when the
        circuit is half-open.
        """
        self.breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01)

        self.breaker.open()
        sleep(0.01)

        @self.breaker
        def err():
            raise Exception

        def trigger_failure():
            try:
                err()
            except:
                pass

        class StateListener(CircuitBreakerListener):
            def __init__(self):
                self._count = 0

            def before_call(self, cb, fun, *args, **kwargs):
                sleep(0.00005)

            def state_change(self, cb, old_state, new_state):
                if new_state.name == "half-open":
                    self._count += 1

        state_listener = StateListener()
        self.breaker.add_listener(state_listener)

        self._start_threads(trigger_failure, 5)
        assert state_listener._count == 1

    def test_fail_max_thread_safety(self):
        """CircuitBreaker: it should not allow more failed calls than 'fail_max'
        setting. Note that with Redis, where we have separate systems
        incrementing the counter, we can get concurrent updates such that the
        counter is greater than the 'fail_max' by the number of systems. To
        prevent this, we'd need to take out a lock amongst all systems before
        trying the call.
        """

        @self.breaker
        def err():
            raise Exception

        def trigger_error():
            for i in range(2000):
                try:
                    err()
                except:
                    pass

        class SleepListener(CircuitBreakerListener):
            def before_call(self, cb, func, *args, **kwargs):
                sleep(0.00005)

        self.breaker.add_listener(SleepListener())
        num_threads = 3
        self._start_threads(trigger_error, num_threads)
        assert self.breaker.fail_counter < self.breaker.fail_max + num_threads


class CircuitBreakerContextManagerTestCase(unittest.TestCase):
    """Tests for the CircuitBreaker class, when used as a context manager."""

    def test_calling(self):
        """Test that the CircuitBreaker calling() API returns a context manager and works as expected."""

        class TestError(Exception):
            pass

        breaker = CircuitBreaker(fail_max=2, reset_timeout=0.01)
        mock_fn = mock.MagicMock()

        def _do_raise():
            with breaker.calling():
                raise TestError

        def _do_succeed():
            with breaker.calling():
                mock_fn()

        self.assertRaises(TestError, _do_raise)
        self.assertRaises(CircuitBreakerError, _do_raise)
        assert breaker.fail_counter == 2
        assert breaker.current_state == "open"

        # Still fails while circuit breaker is open:
        self.assertRaises(CircuitBreakerError, _do_succeed)
        mock_fn.assert_not_called()

        sleep(0.01)

        _do_succeed()
        mock_fn.assert_called_once()
        assert breaker.fail_counter == 0
        assert breaker.current_state == "closed"


if __name__ == "__main__":
    unittest.main()
