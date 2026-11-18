import unittest
from contextlib import contextmanager
from datetime import datetime
from time import sleep
from unittest import mock

import pytest
from pybreaker import *
from tornado import gen, testing

import asyncio
import gc
import threading
import time
from types import MethodType

from tornado import locks


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


def _wait_until(predicate, timeout=2.0):
    """Wait until `predicate` returns a true value, or `timeout` seconds elapsed."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        sleep(0.005)
    return bool(predicate())


class CircuitBreakerHalfOpenConcurrencyTestCase(unittest.TestCase):
    """Tests for the half-open probe budget and round isolation."""

    def _opened_breaker(self, **kwargs):
        """Return a breaker that is open and whose reset timeout already elapsed."""
        breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01, **kwargs)
        breaker.open()
        sleep(0.02)
        return breaker

    def test_half_open_probe_limit_rejects_extra_calls(self):
        """CircuitBreaker: it should admit at most `half_open_max_calls` probes
        and reject the remaining calls instead of letting them all through.
        """
        breaker = self._opened_breaker(half_open_max_calls=2)

        state_lock = threading.Lock()
        all_probes_started = threading.Event()
        release_probes = threading.Event()
        counters = {"active": 0, "peak": 0}
        outcomes = []

        def probe():
            with state_lock:
                counters["active"] += 1
                counters["peak"] = max(counters["peak"], counters["active"])
                if counters["active"] == 2:
                    all_probes_started.set()
            release_probes.wait(5)
            with state_lock:
                counters["active"] -= 1
            return True

        def worker():
            try:
                breaker.call(probe)
                outcomes.append("success")
            except CircuitBreakerError:
                outcomes.append("rejected")

        threads = [threading.Thread(target=worker) for _ in range(6)]
        [t.start() for t in threads]

        assert all_probes_started.wait(5)
        assert _wait_until(lambda: outcomes.count("rejected") == 4)
        # The two probes hold the budget; the other four calls were rejected
        # without ever reaching the guarded operation.
        assert counters["peak"] == 2
        assert breaker._probes_in_flight == 2

        release_probes.set()
        [t.join(5) for t in threads]
        assert not any(t.is_alive() for t in threads)

        assert outcomes.count("rejected") == 4
        assert counters["peak"] == 2
        assert breaker.current_state == "closed"
        assert breaker._probes_in_flight == 0

    def test_half_open_probe_limit_counts_only_successful_round(self):
        """CircuitBreaker: a late success from a superseded round must not close
        the circuit, and `success_threshold` must only count probes of the
        current round.
        """
        breaker = self._opened_breaker(success_threshold=2, half_open_max_calls=2)

        late_probe_running = threading.Event()
        release_late_probe = threading.Event()
        outcomes = {}

        def late_probe():
            late_probe_running.set()
            release_late_probe.wait(5)
            return True

        def failing_probe():
            raise RuntimeError("probe failed")

        def call(func, name):
            try:
                outcomes[name] = breaker.call(func)
            except CircuitBreakerError as exc:
                outcomes[name] = exc

        late_thread = threading.Thread(target=call, args=(late_probe, "late"))
        late_thread.start()
        assert late_probe_running.wait(5)

        # The failing probe reopens the circuit, starting a new round.
        failing_thread = threading.Thread(target=call, args=(failing_probe, "failing"))
        failing_thread.start()
        failing_thread.join(5)
        assert not failing_thread.is_alive()

        assert breaker.current_state == "open"
        assert breaker.fail_counter == 1
        assert breaker.success_counter == 0
        assert isinstance(outcomes["failing"], CircuitBreakerError)

        # Releasing the probe admitted in the previous round yields a late
        # success, which must not close the circuit reopened in the meantime.
        release_late_probe.set()
        late_thread.join(5)
        assert not late_thread.is_alive()

        assert outcomes["late"] is True
        assert breaker.current_state == "open"
        assert breaker.fail_counter == 1
        assert breaker.success_counter == 0
        assert breaker._probes_in_flight == 0

        # Only the probes of the new round count towards the success threshold.
        sleep(0.02)
        assert breaker.call(lambda: True)
        assert breaker.current_state == "half-open"
        assert breaker.success_counter == 1

        assert breaker.call(lambda: True)
        assert breaker.current_state == "closed"
        assert breaker.success_counter == 0

    def test_interleaved_rounds_keep_state_and_counters_consistent(self):
        """CircuitBreaker: two rounds of probes interleaved with barriers must
        not share probe slots, state or counters.
        """
        breaker = self._opened_breaker(success_threshold=2, half_open_max_calls=2)

        # Both probes of the first round are genuinely admitted at once.
        first_round_admitted = threading.Barrier(2)
        release_stale_probe = threading.Event()
        second_round_running = threading.Event()
        release_second_round = threading.Event()
        outcomes = {}

        def first_round_success():
            first_round_admitted.wait(5)
            release_stale_probe.wait(5)
            return "first-round"

        def first_round_failure():
            first_round_admitted.wait(5)
            raise RuntimeError("probe failed")

        def second_round_success():
            second_round_running.set()
            release_second_round.wait(5)
            return "second-round"

        def call(func, name):
            try:
                outcomes[name] = breaker.call(func)
            except CircuitBreakerError as exc:
                outcomes[name] = exc

        stale_thread = threading.Thread(target=call, args=(first_round_success, "stale"))
        failing_thread = threading.Thread(target=call, args=(first_round_failure, "failing"))
        stale_thread.start()
        failing_thread.start()

        failing_thread.join(5)
        assert not failing_thread.is_alive()
        # The barrier proves both first-round probes were admitted concurrently.
        assert isinstance(outcomes["failing"], CircuitBreakerError)
        assert breaker.current_state == "open"
        assert breaker.fail_counter == 1
        assert breaker.success_counter == 0
        assert breaker._probes_in_flight == 0

        # Wait for the reset timeout and start the new round while the stale
        # probe is still blocked inside the guarded operation.
        sleep(0.02)
        current_thread = threading.Thread(target=call, args=(second_round_success, "current"))
        current_thread.start()
        assert second_round_running.wait(5)

        assert breaker.current_state == "half-open"
        assert breaker.success_counter == 0
        assert breaker._probes_in_flight == 1

        # The stale probe now succeeds while the new round is in flight: its
        # outcome must not close the circuit nor count as a round success.
        release_stale_probe.set()
        stale_thread.join(5)
        assert not stale_thread.is_alive()

        assert outcomes["stale"] == "first-round"
        assert breaker.current_state == "half-open"
        assert breaker.success_counter == 0
        assert breaker.fail_counter == 1
        assert breaker._probes_in_flight == 1

        # The current round keeps its own budget and success counter.
        release_second_round.set()
        current_thread.join(5)
        assert not current_thread.is_alive()
        assert outcomes["current"] == "second-round"
        assert breaker.current_state == "half-open"
        assert breaker.success_counter == 1
        assert breaker._probes_in_flight == 0

        # A second current-round success reaches the threshold and closes it.
        assert breaker.call(lambda: True)
        assert breaker.current_state == "closed"
        assert breaker.success_counter == 0
        assert breaker.fail_counter == 0

    def test_late_failure_does_not_affect_the_new_round(self):
        """CircuitBreaker: a late failure from a superseded round must neither
        count nor reopen the circuit of a newer round.
        """
        breaker = self._opened_breaker(success_threshold=1, half_open_max_calls=2)

        stale_probe_running = threading.Event()
        release_stale_probe = threading.Event()
        outcomes = {}

        def stale_probe():
            stale_probe_running.set()
            release_stale_probe.wait(5)
            raise RuntimeError("late failure")

        def call(func, name):
            try:
                outcomes[name] = breaker.call(func)
            except BaseException as exc:
                outcomes[name] = exc

        stale_thread = threading.Thread(target=call, args=(stale_probe, "stale"))
        stale_thread.start()
        assert stale_probe_running.wait(5)

        # A successful probe of the same round closes the circuit, starting a
        # new round while the first probe is still running.
        assert breaker.call(lambda: True) is True
        assert breaker.current_state == "closed"
        assert breaker.fail_counter == 0

        release_stale_probe.set()
        stale_thread.join(5)
        assert not stale_thread.is_alive()

        assert isinstance(outcomes["stale"], RuntimeError)
        # The stale failure was reported but must not reopen or count.
        assert breaker.current_state == "closed"
        assert breaker.fail_counter == 0
        assert breaker._probes_in_flight == 0

    def test_half_open_waiter_does_not_hold_probe_permit(self):
        """CircuitBreaker: a call waiting for a probe slot must not consume one,
        and must run once the slot is released.
        """
        breaker = self._opened_breaker(half_open_max_calls=1, half_open_wait_timeout=2)

        probe_running = threading.Event()
        release_probe = threading.Event()
        waiter_done = threading.Event()
        outcomes = {}

        def blocking_probe():
            probe_running.set()
            release_probe.wait(5)
            return "probe"

        probe_thread = threading.Thread(target=lambda: outcomes.__setitem__("probe", breaker.call(blocking_probe)))
        probe_thread.start()
        assert probe_running.wait(5)
        assert breaker._probes_in_flight == 1

        def waiting_call():
            try:
                outcomes["waiter"] = breaker.call(lambda: "waiter")
            except CircuitBreakerError as exc:
                outcomes["waiter"] = exc
            finally:
                waiter_done.set()

        waiter_thread = threading.Thread(target=waiting_call)
        waiter_thread.start()

        # The waiter is blocked, but the single probe slot is still held only
        # by the running probe.
        sleep(0.1)
        assert not waiter_done.is_set()
        assert breaker._probes_in_flight == 1

        release_probe.set()
        probe_thread.join(5)
        waiter_thread.join(5)
        assert not probe_thread.is_alive()
        assert not waiter_thread.is_alive()

        assert outcomes["probe"] == "probe"
        assert outcomes["waiter"] == "waiter"
        assert breaker.current_state == "closed"
        assert breaker._probes_in_flight == 0

    def test_half_open_waiter_times_out_without_leaking_permit(self):
        """CircuitBreaker: a call that gives up waiting must not keep a probe
        slot, and the slot must be reusable afterwards.
        """
        breaker = self._opened_breaker(half_open_max_calls=1, half_open_wait_timeout=0.1)

        probe_running = threading.Event()
        release_probe = threading.Event()
        outcomes = {}

        def blocking_probe():
            probe_running.set()
            release_probe.wait(5)
            return "probe"

        probe_thread = threading.Thread(target=lambda: outcomes.__setitem__("probe", breaker.call(blocking_probe)))
        probe_thread.start()
        assert probe_running.wait(5)

        started_at = time.monotonic()
        with pytest.raises(CircuitBreakerError):
            breaker.call(lambda: "waiter")
        assert time.monotonic() - started_at >= 0.1
        # Giving up waiting never takes the slot owned by the running probe.
        assert breaker._probes_in_flight == 1

        release_probe.set()
        probe_thread.join(5)
        assert not probe_thread.is_alive()
        assert outcomes["probe"] == "probe"
        assert breaker._probes_in_flight == 0

        # The single slot is available again.
        assert breaker.call(lambda: "after") == "after"
        assert breaker._probes_in_flight == 0

    def test_half_open_max_calls_validation(self):
        """CircuitBreaker: it should reject invalid half-open probe settings."""
        with pytest.raises(ValueError):
            CircuitBreaker(half_open_max_calls=0)
        with pytest.raises(ValueError):
            CircuitBreaker(half_open_wait_timeout=-1)

        breaker = CircuitBreaker()
        assert breaker.half_open_max_calls == 1
        assert breaker.half_open_wait_timeout is None

        breaker.half_open_max_calls = 3
        assert breaker.half_open_max_calls == 3
        with pytest.raises(ValueError):
            breaker.half_open_max_calls = 0

        breaker.half_open_wait_timeout = 1.5
        assert breaker.half_open_wait_timeout == 1.5
        breaker.half_open_wait_timeout = None
        assert breaker.half_open_wait_timeout is None
        with pytest.raises(ValueError):
            breaker.half_open_wait_timeout = -1


class CircuitBreakerLockScopeTestCase(unittest.TestCase):
    """Tests confirming the breaker lock is not held while user code runs."""

    def test_guarded_calls_run_concurrently(self):
        """CircuitBreaker: two guarded calls in the closed state must overlap
        instead of being serialized by the breaker lock.
        """
        breaker = CircuitBreaker()

        both_running = threading.Barrier(2, timeout=5)
        results = []

        def overlap():
            # Both threads must reach the barrier; a global lock around the
            # guarded operation would deadlock here.
            both_running.wait()
            return True

        threads = [
            threading.Thread(target=lambda: results.append(breaker.call(overlap))),
            threading.Thread(target=lambda: results.append(breaker.call(overlap))),
        ]
        [t.start() for t in threads]
        [t.join(5) for t in threads]

        assert not any(t.is_alive() for t in threads)
        assert results == [True, True]
        assert breaker.fail_counter == 0

    def test_guarded_call_does_not_hold_breaker_lock(self):
        """CircuitBreaker: another thread must be able to manage the breaker
        while a guarded call is executing.
        """
        breaker = CircuitBreaker(fail_max=1, reset_timeout=60)

        call_running = threading.Event()
        release_call = threading.Event()
        results = []

        def blocking():
            call_running.set()
            release_call.wait(5)
            return True

        call_thread = threading.Thread(target=lambda: results.append(breaker.call(blocking)))
        call_thread.start()
        assert call_running.wait(5)

        opener = threading.Thread(target=breaker.open)
        opener.start()
        opener.join(1)
        assert not opener.is_alive()
        assert breaker.current_state == "open"

        closer = threading.Thread(target=breaker.close)
        closer.start()
        closer.join(1)
        assert not closer.is_alive()
        assert breaker.current_state == "closed"

        # Let the blocked call finish: its outcome is late and must be ignored.
        breaker.open()
        release_call.set()
        call_thread.join(5)
        assert not call_thread.is_alive()
        assert results == [True]
        assert breaker.current_state == "open"
        # A late success from a superseded round must not reset the failure counter.
        assert breaker.fail_counter == 0
        assert breaker.success_counter == 0

    def test_abandoned_generator_returns_probe_permit(self):
        """CircuitBreaker: closing a guarded generator early must give its
        half-open probe slot back.
        """
        breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01, half_open_max_calls=1)
        breaker.open()
        sleep(0.02)

        @breaker
        def guarded():
            yield 1
            yield 2

        generator = guarded()
        assert next(generator) == 1
        assert breaker._probes_in_flight == 1

        generator.close()
        assert breaker._probes_in_flight == 0

        # The slot is free again, so a fresh probe is admitted.
        assert breaker.call(lambda: "after") == "after"
        assert breaker.current_state == "closed"


class CircuitBreakerAsyncHalfOpenTestCase(testing.AsyncTestCase):
    """Tests for the half-open probe budget on the Tornado call path."""

    @testing.gen_test
    def test_async_call_rejected_when_probe_budget_is_taken(self):
        """CircuitBreaker: asynchronous calls must not block the IOLoop waiting
        for a probe slot; they are rejected while another probe is in flight.
        """
        breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01, half_open_max_calls=1)
        breaker.open()
        sleep(0.02)

        probe_running = threading.Event()
        release_probe = threading.Event()
        calls = []

        def blocking_probe():
            probe_running.set()
            release_probe.wait(5)
            return "probe"

        probe_thread = threading.Thread(target=lambda: calls.append(breaker.call(blocking_probe)))
        probe_thread.start()
        assert probe_running.wait(5)
        assert breaker._probes_in_flight == 1

        @gen.coroutine
        def succeeding_async_call():
            raise gen.Return("async")

        with pytest.raises(CircuitBreakerError):
            yield breaker.call_async(succeeding_async_call)

        release_probe.set()
        probe_thread.join(5)
        assert not probe_thread.is_alive()
        assert calls == ["probe"]
        assert breaker.current_state == "closed"
        assert breaker._probes_in_flight == 0

    @testing.gen_test
    def test_async_recovery_probe_runs_once(self):
        """CircuitBreaker: a Tornado trial call must run the guarded operation
        once, and only its completed outcome may change the state.
        """
        breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01)
        breaker.open()
        sleep(0.02)

        calls = []

        @gen.coroutine
        def trial():
            calls.append("trial")
            yield gen.sleep(0.01)
            raise gen.Return("ok")

        assert (yield breaker.call_async(trial)) == "ok"
        assert calls == ["trial"]
        assert breaker.current_state == "closed"

    @testing.gen_test
    def test_async_trial_failure_reopens_breaker_once(self):
        """CircuitBreaker: a failing asynchronous trial opens the circuit and
        reports ``CircuitBreakerError``, executing the operation only once.
        """
        breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01)
        breaker.open()
        sleep(0.02)

        calls = []

        @gen.coroutine
        def trial():
            calls.append("trial")
            yield gen.sleep(0.01)
            raise RuntimeError("trial failed")

        with pytest.raises(CircuitBreakerError):
            yield breaker.call_async(trial)

        assert calls == ["trial"]
        assert breaker.current_state == "open"
        assert breaker.fail_counter == 1
        assert breaker._probes_in_flight == 0

    @testing.gen_test
    def test_async_cancellation_keeps_permit_until_operation_stops(self):
        """CircuitBreaker: cancelling a call must not free the probe slot while
        the guarded operation is still running, so the probe budget keeps
        reflecting the work in flight, and must not leak the slot afterwards.
        """
        breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01)
        breaker.open()
        sleep(0.02)

        started = locks.Event()
        finished = locks.Event()

        @gen.coroutine
        def slow_trial():
            started.set()
            try:
                yield gen.sleep(0.2)
            finally:
                finished.set()
            raise gen.Return(True)

        trial = breaker.call_async(slow_trial)
        yield started.wait()
        assert breaker._probes_in_flight == 1

        @gen.coroutine
        def quick_trial():
            raise gen.Return(True)

        trial.cancel()
        with pytest.raises(asyncio.CancelledError):
            yield trial

        # The operation is still running: its slot must stay taken so no other
        # call believes the service is ready for a second probe.
        assert not finished.is_set()
        assert breaker._probes_in_flight == 1

        with pytest.raises(CircuitBreakerError):
            yield breaker.call_async(quick_trial)

        while not finished.is_set():
            yield gen.sleep(0.01)
        yield gen.sleep(0.01)

        # The slot is returned once the operation stops, so it cannot leak.
        assert breaker._probes_in_flight == 0

        # The permit is free, so a trial call is admitted again.
        yield breaker.call_async(quick_trial)
        assert breaker._probes_in_flight == 0

    def test_async_success_counter_is_shared_with_async_round(self):
        """CircuitBreaker: asynchronous trials use the same round and success
        counter as synchronous ones.
        """
        breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01, success_threshold=2)
        breaker.open()
        sleep(0.02)

        assert breaker.call(lambda: True)
        assert breaker.current_state == "half-open"
        assert breaker.success_counter == 1

        assert breaker.call(lambda: True)
        assert breaker.current_state == "closed"
        assert breaker.success_counter == 0

    def test_unstarted_generator_returns_probe_permit(self):
        """CircuitBreaker: a guarded generator that is created but never
        iterated must give its half-open probe slot back when collected.
        """
        breaker = CircuitBreaker(fail_max=1, reset_timeout=0.01, half_open_max_calls=1)
        breaker.open()
        sleep(0.02)

        @breaker
        def guarded():
            yield 1

        generator = guarded()
        # Calling the guarded generator admitted a probe even before its body
        # started running.
        assert breaker._probes_in_flight == 1

        del generator
        gc.collect()

        assert breaker._probes_in_flight == 0
        assert breaker.fail_counter == 0
        assert breaker.success_counter == 0

        assert breaker.call(lambda: "after") == "after"
        assert breaker.current_state == "closed"


if __name__ == "__main__":
    unittest.main()
