# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

import abc
import pickle
import threading
import warnings

from unittest.mock import call

import pytest

import structlog

from structlog._base import BoundLoggerBase
from structlog._config import (
    _BUILTIN_DEFAULT_CONTEXT_CLASS,
    _BUILTIN_DEFAULT_LOGGER_FACTORY,
    _BUILTIN_DEFAULT_PROCESSORS,
    _BUILTIN_DEFAULT_WRAPPER_CLASS,
    _CONFIG,
    BoundLoggerLazyProxy,
    MaterializedLoggerPolicy,
    configure,
    configure_once,
    get_logger,
    publish_config,
    wrap_logger,
)
from structlog.testing import ReturnLogger
from structlog.typing import BindableLogger

from .helpers import call_recorder, stub


@pytest.fixture(name="proxy")
def _proxy():
    """
    Returns a BoundLoggerLazyProxy constructed w/o parameters & None as logger.
    """
    return BoundLoggerLazyProxy(None)


class Wrapper(BoundLoggerBase):
    """
    Custom wrapper class for testing.
    """


def test_lazy_logger_is_not_detected_as_abstract_method():
    """
    If someone defines an attribute on an ABC with a logger, that logger is not
    detected as an abstract method.

    See https://github.com/hynek/structlog/issues/229
    """

    class Foo(metaclass=abc.ABCMeta):  # noqa: B024
        log = structlog.get_logger()

    Foo()


def test_lazy_logger_is_an_instance_of_bindable_logger():
    """
    The BoundLoggerLazyProxy returned by get_logger fulfills the BindableLogger
    protocol.

    See https://github.com/hynek/structlog/issues/560
    """
    assert isinstance(get_logger(), BindableLogger)


def test_lazy_logger_context_is_initial_values():
    """
    If a user asks for _context (e.g., using get_context) return
    initial_values.
    """
    logger = get_logger(context="a")

    assert {"context": "a"} == structlog.get_context(logger)


def test_default_context_class():
    """
    Default context class is dict.
    """
    assert dict is _BUILTIN_DEFAULT_CONTEXT_CLASS


class TestConfigure:
    def test_get_config_is_configured(self):
        """
        Return value of structlog.get_config() works as input for
        structlog.configure(). is_configured() reflects the state of
        configuration.
        """
        assert False is structlog.is_configured()

        structlog.configure(**structlog.get_config())

        assert True is structlog.is_configured()

        structlog.reset_defaults()

        assert False is structlog.is_configured()

    def test_configure_all(self, proxy):
        """
        All configurations are applied and land on the bound logger.
        """
        x = stub()
        configure(processors=[x], context_class=dict)
        b = proxy.bind()

        assert [x] == b._processors
        assert dict is b._context.__class__

    def test_reset(self, proxy):
        """
        Reset resets all settings to their default values.
        """
        x = stub()
        configure(processors=[x], context_class=dict, wrapper_class=Wrapper)

        structlog.reset_defaults()
        b = proxy.bind()

        assert [x] != b._processors
        assert _BUILTIN_DEFAULT_PROCESSORS == b._processors
        assert isinstance(b, _BUILTIN_DEFAULT_WRAPPER_CLASS)
        assert _BUILTIN_DEFAULT_CONTEXT_CLASS == b._context.__class__
        assert _BUILTIN_DEFAULT_LOGGER_FACTORY is _CONFIG.logger_factory

    def test_just_processors(self, proxy):
        """
        It's possible to only configure processors.
        """
        x = stub()
        configure(processors=[x])
        b = proxy.bind()

        assert [x] == b._processors
        assert _BUILTIN_DEFAULT_PROCESSORS != b._processors
        assert _BUILTIN_DEFAULT_CONTEXT_CLASS == b._context.__class__

    def test_just_context_class(self, proxy):
        """
        It's possible to only configure the context class.
        """
        configure(context_class=dict)
        b = proxy.bind()

        assert dict is b._context.__class__
        assert _BUILTIN_DEFAULT_PROCESSORS == b._processors

    def test_configure_sets_is_configured(self):
        """
        After configure() is_configured() returns True.
        """
        assert False is _CONFIG.is_configured

        configure()

        assert True is _CONFIG.is_configured

    def test_configures_logger_factory(self):
        """
        It's possible to configure the logger factory.
        """

        def f():
            pass

        configure(logger_factory=f)

        assert f is _CONFIG.logger_factory


class TestBoundLoggerLazyProxy:
    def test_repr(self):
        """
        repr reflects all attributes.
        """
        p = BoundLoggerLazyProxy(
            None,
            processors=[1, 2, 3],
            context_class=dict,
            initial_values={"foo": 42},
            logger_factory_args=(4, 5),
        )
        assert (
            "<BoundLoggerLazyProxy(logger=None, wrapper_class=None, "
            "processors=[1, 2, 3], "
            "context_class=<class 'dict'>, "
            "initial_values={'foo': 42}, "
            "logger_factory_args=(4, 5))>"
        ) == repr(p)

    def test_returns_bound_logger_on_bind(self, proxy):
        """
        bind gets proxied to the wrapped bound logger.
        """
        assert isinstance(proxy.bind(), BoundLoggerBase)

    def test_returns_bound_logger_on_new(self, proxy):
        """
        new gets proxied to the wrapped bound logger.
        """
        assert isinstance(proxy.new(), BoundLoggerBase)

    def test_returns_bound_logger_on_try_unbind(self, proxy):
        """
        try_unbind gets proxied to the wrapped bound logger.
        """
        assert isinstance(proxy.try_unbind(), BoundLoggerBase)

    def test_prefers_args_over_config(self):
        """
        Configuration can be overridden by passing arguments.
        """
        p = BoundLoggerLazyProxy(
            None, processors=[1, 2, 3], context_class=dict
        )
        b = p.bind()
        assert isinstance(b._context, dict)
        assert [1, 2, 3] == b._processors

        class Class:
            def __init__(self, *args, **kw):
                pass

            def update(self, *args, **kw):
                pass

        configure(processors=[4, 5, 6], context_class=Class)
        b = p.bind()

        assert not isinstance(b._context, Class)
        assert [1, 2, 3] == b._processors

    def test_falls_back_to_config(self, proxy):
        """
        Configuration is used if no arguments are passed.
        """
        b = proxy.bind()

        assert isinstance(b._context, _CONFIG.default_context_class)
        assert _CONFIG.default_processors == b._processors

    def test_bind_honors_initial_values(self):
        """
        Passed initial_values are merged on binds.
        """
        p = BoundLoggerLazyProxy(None, initial_values={"a": 1, "b": 2})
        b = p.bind()

        assert {"a": 1, "b": 2} == b._context

        b = p.bind(c=3)

        assert {"a": 1, "b": 2, "c": 3} == b._context

    def test_bind_binds_new_values(self, proxy):
        """
        Values passed to bind arrive in the context.
        """
        b = proxy.bind(c=3)

        assert {"c": 3} == b._context

    def test_unbind_unbinds_from_initial_values(self):
        """
        It's possible to unbind a value that came from initial_values.
        """
        p = BoundLoggerLazyProxy(None, initial_values={"a": 1, "b": 2})
        b = p.unbind("a")

        assert {"b": 2} == b._context

    def test_honors_wrapper_class(self):
        """
        Passed wrapper_class is used.
        """
        p = BoundLoggerLazyProxy(None, wrapper_class=Wrapper)
        b = p.bind()

        assert isinstance(b, Wrapper)

    def test_honors_wrapper_from_config(self, proxy):
        """
        Configured wrapper_class is used if not overridden.
        """
        configure(wrapper_class=Wrapper)

        b = proxy.bind()

        assert isinstance(b, Wrapper)

    def test_new_binds_only_initial_values_implicit_ctx_class(self, proxy):
        """
        new() doesn't clear initial_values if context_class comes from config.
        """
        proxy = BoundLoggerLazyProxy(None, initial_values={"a": 1, "b": 2})

        b = proxy.new(foo=42)

        assert {"a": 1, "b": 2, "foo": 42} == b._context

    def test_new_binds_only_initial_values_explicit_ctx_class(self, proxy):
        """
        new() doesn't clear initial_values if context_class is passed
        explicitly..
        """
        proxy = BoundLoggerLazyProxy(
            None, initial_values={"a": 1, "b": 2}, context_class=dict
        )
        b = proxy.new(foo=42)
        assert {"a": 1, "b": 2, "foo": 42} == b._context

    def test_rebinds_bind_method(self, proxy):
        """
        To save time, be rebind the bind method once the logger has been
        cached.
        """
        configure(cache_logger_on_first_use=True)

        bind = proxy.bind
        proxy.bind()

        assert bind != proxy.bind

    def test_does_not_cache_by_default(self, proxy):
        """
        Proxy's bind method doesn't change by default.
        """
        bind = proxy.bind

        proxy.bind()

        assert bind == proxy.bind

    @pytest.mark.parametrize("cache", [True, False])
    def test_argument_takes_precedence_over_configuration(self, cache):
        """
        Passing cache_logger_on_first_use as an argument overrides config.
        """
        configure(cache_logger_on_first_use=cache)

        proxy = BoundLoggerLazyProxy(None, cache_logger_on_first_use=not cache)
        bind = proxy.bind
        proxy.bind()

        if cache:
            assert bind == proxy.bind
        else:
            assert bind != proxy.bind

    def test_bind_doesnt_cache_logger(self):
        """
        Calling configure() changes BoundLoggerLazyProxys immediately.
        Previous uses of the BoundLoggerLazyProxy don't interfere.
        """

        class F:
            "New logger factory with a new attribute"

            def info(self, *args):
                return 5

        proxy = BoundLoggerLazyProxy(None)
        proxy.bind()
        configure(logger_factory=F)
        new_b = proxy.bind()

        assert new_b.info("test") == 5

    def test_emphemeral(self):
        """
        Calling an unknown method proxy creates a new wrapped bound logger
        first.
        """

        class Foo(BoundLoggerBase):
            def foo(self):
                return 42

        proxy = BoundLoggerLazyProxy(
            None, wrapper_class=Foo, cache_logger_on_first_use=False
        )
        assert 42 == proxy.foo()

    @pytest.mark.parametrize("proto", range(pickle.HIGHEST_PROTOCOL + 1))
    def test_pickle(self, proto):
        """
        Can be pickled and unpickled.
        """
        bllp = BoundLoggerLazyProxy(None)

        assert repr(bllp) == repr(pickle.loads(pickle.dumps(bllp, proto)))


class TestFunctions:
    def test_wrap_passes_args(self):
        """
        wrap_logger propagates all arguments to the wrapped bound logger.
        """
        logger = object()
        p = wrap_logger(logger, processors=[1, 2, 3], context_class=dict)

        assert logger is p._logger
        assert [1, 2, 3] == p._processors
        assert dict is p._context_class

    def test_empty_processors(self):
        """
        An empty list is a valid value for processors so it must be preserved.
        """
        # We need to do a bind such that we get an actual logger and not just
        # a lazy proxy.
        logger = wrap_logger(object(), processors=[]).new()

        assert [] == logger._processors

    def test_wrap_returns_proxy(self):
        """
        wrap_logger always returns a lazy proxy.
        """
        assert isinstance(wrap_logger(None), BoundLoggerLazyProxy)

    def test_configure_once_issues_warning_on_repeated_call(self):
        """
        configure_once raises a warning when it's after configuration.
        """
        with warnings.catch_warnings(record=True) as warns:
            configure_once()

        assert 0 == len(warns)

        with warnings.catch_warnings(record=True) as warns:
            configure_once()

        assert 1 == len(warns)
        assert RuntimeWarning is warns[0].category
        assert "Repeated configuration attempted." == warns[0].message.args[0]

    def test_get_logger_configures_according_to_config(self):
        """
        get_logger returns a correctly configured bound logger.
        """
        b = get_logger().bind()

        assert isinstance(
            b._logger, _BUILTIN_DEFAULT_LOGGER_FACTORY().__class__
        )
        assert _BUILTIN_DEFAULT_PROCESSORS == b._processors
        assert isinstance(b, _BUILTIN_DEFAULT_WRAPPER_CLASS)
        assert _BUILTIN_DEFAULT_CONTEXT_CLASS == b._context.__class__

    def test_get_logger_passes_positional_arguments_to_logger_factory(self):
        """
        Ensure `get_logger` passes optional positional arguments through to
        the logger factory.
        """
        factory = call_recorder(lambda *args: object())
        configure(logger_factory=factory)

        get_logger("test").bind(x=42)

        assert [call("test")] == factory.call_args_list


class OtherWrapper(BoundLoggerBase):
    """
    Another custom wrapper class for testing.
    """


class TestPublishConfig:
    def test_publishes_whole_config_atomically(self, proxy):
        """
        processors, wrapper_class, context_class, and logger_factory switch
        together: a lazy proxy materializes a chain that consistently
        belongs to the published generation.
        """
        x = stub()
        publish_config(
            processors=[x],
            wrapper_class=Wrapper,
            context_class=dict,
            logger_factory=lambda: "wrapped",
            cache_logger_on_first_use=True,
        )

        b = proxy.bind()

        assert [x] == b._processors
        assert isinstance(b, Wrapper)
        assert "wrapped" is b._logger
        assert dict is b._context.__class__

    def test_returns_monotonically_increasing_generations(self):
        """
        Each publish returns a generation id higher than the previous one.
        """
        gen1 = publish_config(processors=[stub()])
        gen2 = publish_config(processors=[stub()])

        assert isinstance(gen1, int)
        assert gen2 > gen1

    def test_sets_is_configured(self):
        """
        publish_config marks structlog as configured.
        """
        assert False is structlog.is_configured()

        publish_config(processors=[stub()])

        assert True is structlog.is_configured()

    def test_get_config_reflects_published_config(self):
        """
        get_config returns the published values.
        """
        x = stub()
        publish_config(processors=[x], wrapper_class=Wrapper)

        config = structlog.get_config()

        assert [x] == config["processors"]
        assert Wrapper is config["wrapper_class"]

    @pytest.mark.parametrize(
        "kwargs",
        [
            {"processors": "not-an-iterable-of-callables"},
            {"processors": [stub(), 42]},
            {"wrapper_class": 42},
            {"context_class": 42},
            {"logger_factory": 42},
            {"cache_logger_on_first_use": "yes"},
        ],
    )
    def test_validation_failure_keeps_old_config(self, kwargs):
        """
        If validation fails, the previous configuration stays fully in
        effect and the error is raised.
        """
        x = stub()
        configure(processors=[x], wrapper_class=Wrapper)
        before = structlog.get_config()

        with pytest.raises(TypeError):
            publish_config(**kwargs)

        assert before == structlog.get_config()

    def test_invalid_materialized_loggers_keeps_old_config(self):
        """
        An invalid materialized_loggers policy raises and keeps the old
        configuration.
        """
        configure(processors=[stub()])
        before = structlog.get_config()

        with pytest.raises(ValueError, match="bogus"):
            publish_config(
                processors=[stub()], materialized_loggers="bogus"
            )

        assert before == structlog.get_config()

    def test_caller_processor_list_is_not_modified(self):
        """
        The processor list passed by the caller is never modified -- not by
        publishing, not by disabling the cache, and not by refreshing
        materialized loggers.
        """
        procs = [stub(), stub()]
        original = procs[:]

        publish_config(
            processors=procs,
            cache_logger_on_first_use=True,
            materialized_loggers=MaterializedLoggerPolicy.REFRESH,
        )
        proxy = BoundLoggerLazyProxy(None)
        proxy.bind()

        # Force a refresh of the materialized logger.
        publish_config(
            processors=[stub()],
            cache_logger_on_first_use=False,
            materialized_loggers=MaterializedLoggerPolicy.REFRESH,
        )
        proxy.bind()

        assert original == procs

    def test_published_generation_is_immune_to_caller_mutation(self):
        """
        Mutating the caller's list after publish_config does not leak into
        the published generation.
        """
        x = stub()
        procs = [x]
        publish_config(processors=procs)

        procs.clear()

        assert [x] == BoundLoggerLazyProxy(None).bind()._processors

    def test_materialized_loggers_keep_old_generation_by_default(self):
        """
        Already-materialized (cached) loggers keep serving the generation
        they were built from unless told otherwise.
        """
        configure(cache_logger_on_first_use=True)
        proxy = BoundLoggerLazyProxy(None)
        b1 = proxy.bind()

        publish_config(processors=[stub()], wrapper_class=Wrapper)
        b2 = proxy.bind()

        assert b1 is b2
        assert not isinstance(b2, Wrapper)

    @pytest.mark.parametrize(
        "policy", [MaterializedLoggerPolicy.REFRESH, "refresh"]
    )
    def test_materialized_loggers_refresh_if_policy_says_so(self, policy):
        """
        If the published generation carries the REFRESH policy, cached
        loggers re-assemble themselves from the new generation on next use.
        """
        configure(cache_logger_on_first_use=True)
        proxy = BoundLoggerLazyProxy(None)
        b1 = proxy.bind()

        x = stub()
        publish_config(
            processors=[x], wrapper_class=Wrapper, materialized_loggers=policy
        )
        b2 = proxy.bind()

        assert b1 is not b2
        assert isinstance(b2, Wrapper)
        assert [x] == b2._processors

        # The refreshed logger is cached again.
        assert b2 is proxy.bind()

    def test_refresh_keeps_explicit_proxy_arguments(self):
        """
        A refreshed proxy still prefers its explicit arguments over the
        published configuration.
        """
        y = stub()
        proxy = BoundLoggerLazyProxy(
            None, processors=[y], cache_logger_on_first_use=True
        )
        proxy.bind()

        publish_config(
            processors=[stub()],
            materialized_loggers=MaterializedLoggerPolicy.REFRESH,
        )
        b = proxy.bind()

        assert [y] == b._processors

    def test_lazy_proxy_binds_complete_generation(self):
        """
        A lazy proxy that materializes after a publish uses only components
        of the new generation -- never a mix with the previous one.
        """
        old = stub()
        configure(processors=[old], wrapper_class=OtherWrapper)
        proxy = BoundLoggerLazyProxy(None)

        new = stub()
        publish_config(processors=[new], wrapper_class=Wrapper)
        b = proxy.bind()

        assert [new] == b._processors
        assert isinstance(b, Wrapper)

    def test_concurrent_first_bind_never_mixes_generations(self):
        """
        Two threads materializing lazy loggers for the first time while
        another thread publishes configurations never end up with a chain
        that mixes components from different generations.
        """
        p_old, p_new = stub(), stub()
        configure(processors=[p_old], wrapper_class=OtherWrapper)

        barrier = threading.Barrier(3)
        results = []
        errors = []

        def materialize():
            try:
                barrier.wait()
                for _ in range(200):
                    results.append(BoundLoggerLazyProxy(None).bind())
            except Exception as e:  # noqa: BLE001
                errors.append(e)

        threads = [threading.Thread(target=materialize) for _ in range(2)]
        for t in threads:
            t.start()

        barrier.wait()
        for _ in range(200):
            publish_config(processors=[p_new], wrapper_class=Wrapper)
            publish_config(processors=[p_old], wrapper_class=OtherWrapper)

        for t in threads:
            t.join()

        assert not errors
        assert results

        for b in results:
            if isinstance(b, Wrapper):
                assert [p_new] == b._processors
            else:
                assert isinstance(b, OtherWrapper)
                assert [p_old] == b._processors

    def test_concurrent_cached_first_bind_never_mixes_generations(self):
        """
        Two threads racing the *first* cached bind of the same lazy proxy
        both end up with a complete chain from exactly one generation.
        """
        p_old, p_new = stub(), stub()
        configure(
            processors=[p_old],
            wrapper_class=OtherWrapper,
            cache_logger_on_first_use=True,
        )
        proxy = BoundLoggerLazyProxy(None)

        barrier = threading.Barrier(2)
        results = []

        def first_bind():
            barrier.wait()
            results.append(proxy.bind())

        t1 = threading.Thread(target=first_bind)
        t2 = threading.Thread(target=first_bind)

        t1.start()
        publish_config(processors=[p_new], wrapper_class=Wrapper)
        t2.start()

        t1.join()
        t2.join()

        assert 2 == len(results)
        for b in results:
            if isinstance(b, Wrapper):
                assert [p_new] == b._processors
            else:
                assert isinstance(b, OtherWrapper)
                assert [p_old] == b._processors
