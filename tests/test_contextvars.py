# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

import asyncio
import inspect
import secrets

import pytest

import structlog

from structlog.contextvars import (
    _CONTEXT_VARS,
    TraceContext,
    TraceContextStatus,
    bind_contextvars,
    bind_trace_context,
    bound_contextvars,
    bound_trace_context,
    clear_contextvars,
    get_contextvars,
    get_merged_contextvars,
    merge_contextvars,
    parse_trace_context,
    reset_contextvars,
    unbind_contextvars,
    unbind_trace_context,
)


@pytest.fixture(autouse=True)
def _clear_contextvars():
    """
    Make sure all tests start with a clean slate.
    """
    clear_contextvars()


class TestContextvars:
    async def test_bind(self):
        """
        Binding a variable causes it to be included in the result of
        merge_contextvars.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            bind_contextvars(a=1)
            return merge_contextvars(None, None, {"b": 2})

        assert {"a": 1, "b": 2} == await event_loop.create_task(coro())

    async def test_multiple_binds(self):
        """
        Multiple calls to bind_contextvars accumulate values instead of
        replacing them. But they override redefined ones.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            bind_contextvars(a=1, c=3)
            bind_contextvars(c=333, d=4)
            return merge_contextvars(None, None, {"b": 2})

        assert {
            "a": 1,
            "b": 2,
            "c": 333,
            "d": 4,
        } == await event_loop.create_task(coro())

    async def test_reset(self):
        """
        reset_contextvars allows resetting contexvars to
        previously-set values.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            bind_contextvars(a=1)

            assert {"a": 1} == get_contextvars()

            await event_loop.create_task(nested_coro())

        async def nested_coro():
            tokens = bind_contextvars(a=2, b=3)

            assert {"a": 2, "b": 3} == get_contextvars()

            reset_contextvars(**tokens)

            assert {"a": 1} == get_contextvars()

        await event_loop.create_task(coro())

    async def test_nested_async_bind(self):
        """
        Context is passed correctly between "nested" concurrent operations.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            bind_contextvars(a=1)
            return await event_loop.create_task(nested_coro())

        async def nested_coro():
            bind_contextvars(c=3)
            return merge_contextvars(None, None, {"b": 2})

        assert {"a": 1, "b": 2, "c": 3} == await event_loop.create_task(coro())

    async def test_merge_works_without_bind(self):
        """
        merge_contextvars returns values as normal even when there has
        been no previous calls to bind_contextvars.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            return merge_contextvars(None, None, {"b": 2})

        assert {"b": 2} == await event_loop.create_task(coro())

    async def test_merge_overrides_bind(self):
        """
        Variables included in merge_contextvars override previously
        bound variables.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            bind_contextvars(a=1)
            return merge_contextvars(None, None, {"a": 111, "b": 2})

        assert {"a": 111, "b": 2} == await event_loop.create_task(coro())

    async def test_clear(self):
        """
        The context-local context can be cleared, causing any previously bound
        variables to not be included in merge_contextvars's result.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            bind_contextvars(a=1)
            clear_contextvars()
            return merge_contextvars(None, None, {"b": 2})

        assert {"b": 2} == await event_loop.create_task(coro())

    async def test_clear_without_bind(self):
        """
        The context-local context can be cleared, causing any previously bound
        variables to not be included in merge_contextvars's result.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            clear_contextvars()
            return merge_contextvars(None, None, {})

        assert {} == await event_loop.create_task(coro())

    async def test_unbind(self):
        """
        Unbinding a previously bound variable causes it to be removed from the
        result of merge_contextvars.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            bind_contextvars(a=1)
            unbind_contextvars("a")
            return merge_contextvars(None, None, {"b": 2})

        assert {"b": 2} == await event_loop.create_task(coro())

    async def test_unbind_not_bound(self):
        """
        Unbinding a not bound variable causes doesn't raise an exception.
        """
        event_loop = asyncio.get_running_loop()

        async def coro():
            # Since unbinding means "setting to Ellipsis", we have to make
            # some effort to ensure that the ContextVar never existed.
            unbind_contextvars("a" + secrets.token_hex())

            return merge_contextvars(None, None, {"b": 2})

        assert {"b": 2} == await event_loop.create_task(coro())

    async def test_parallel_binds(self):
        """
        Binding a variable causes it to be included in the result of
        merge_contextvars.
        """
        event_loop = asyncio.get_running_loop()
        coro1_bind = asyncio.Event()
        coro2_bind = asyncio.Event()

        bind_contextvars(c=3)

        async def coro1():
            bind_contextvars(a=1)

            coro1_bind.set()
            await coro2_bind.wait()

            return merge_contextvars(None, None, {"b": 2})

        async def coro2():
            bind_contextvars(a=2)

            await coro1_bind.wait()
            coro2_bind.set()

            return merge_contextvars(None, None, {"b": 2})

        coro1_task = event_loop.create_task(coro1())
        coro2_task = event_loop.create_task(coro2())

        assert {"a": 1, "b": 2, "c": 3} == await coro1_task
        assert {"a": 2, "b": 2, "c": 3} == await coro2_task

    def test_get_only_gets_structlog_without_deleted(self):
        """
        get_contextvars returns only the structlog-specific key-values with
        the prefix removed. Deleted keys (= Ellipsis) are ignored.
        """
        bind_contextvars(a=1, b=2)
        unbind_contextvars("b")
        _CONTEXT_VARS["foo"] = "bar"

        assert {"a": 1} == get_contextvars()

    def test_get_merged_merges_context(self):
        """
        get_merged_contextvars merges a bound context into the copy.
        """
        bind_contextvars(x=1)
        log = structlog.get_logger().bind(y=2)

        assert {"x": 1, "y": 2} == get_merged_contextvars(log)


class TestBoundContextvars:
    def test_cleanup(self):
        """
        Bindings are cleaned up
        """
        with bound_contextvars(x=42, y="foo"):
            assert {"x": 42, "y": "foo"} == get_contextvars()

        assert {} == get_contextvars()

    def test_cleanup_conflict(self):
        """
        Overwritten keys are restored after the clean up
        """
        bind_contextvars(x="original", z="unrelated")
        with bound_contextvars(x=42, y="foo"):
            assert {"x": 42, "y": "foo", "z": "unrelated"} == get_contextvars()

        assert {"x": "original", "z": "unrelated"} == get_contextvars()

    def test_preserve_independent_bind(self):
        """
        New bindings inside bound_contextvars are preserved after the clean up
        """
        with bound_contextvars(x=42):
            bind_contextvars(y="foo")
            assert {"x": 42, "y": "foo"} == get_contextvars()

        assert {"y": "foo"} == get_contextvars()

    def test_nesting_works(self):
        """
        bound_contextvars binds and unbinds even when nested
        """
        with bound_contextvars(l1=1):
            assert {"l1": 1} == get_contextvars()

            with bound_contextvars(l2=2):
                assert {"l1": 1, "l2": 2} == get_contextvars()

            assert {"l1": 1} == get_contextvars()

        assert {} == get_contextvars()

    def test_as_decorator(self):
        """
        bound_contextvars can be used as a decorator and it preserves the
        name, signature and documentation of the wrapped function.
        """

        @bound_contextvars(x=42)
        def wrapped(arg1):
            """Wrapped documentation"""
            bind_contextvars(y=arg1)
            assert {"x": 42, "y": arg1} == get_contextvars()

        wrapped(23)

        assert "wrapped" == wrapped.__name__
        assert "(arg1)" == str(inspect.signature(wrapped))
        assert "Wrapped documentation" == wrapped.__doc__


TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
SPAN_ID = "00f067aa0ba902b7"
TRACEPARENT = f"00-{TRACE_ID}-{SPAN_ID}-01"


class TestParseTraceContext:
    def test_valid(self):
        """
        A valid sampled traceparent is parsed into its components.
        """
        ctx = parse_trace_context(TRACEPARENT)

        assert TraceContextStatus.SAMPLED is ctx.status
        assert TRACE_ID == ctx.trace_id
        assert SPAN_ID == ctx.span_id
        assert 1 == ctx.trace_flags
        assert () == ctx.tracestate
        assert ctx.sampled is True
        assert ctx.error is None

    def test_not_sampled(self):
        """
        A clear sampling flag of zero is distinguishable from a missing or
        invalid header.
        """
        ctx = parse_trace_context(f"00-{TRACE_ID}-{SPAN_ID}-00")

        assert TraceContextStatus.NOT_SAMPLED is ctx.status
        assert ctx.sampled is False
        assert TRACE_ID == ctx.trace_id
        assert SPAN_ID == ctx.span_id
        assert ctx.error is None

    def test_unknown_flag_bits_do_not_change_sampling(self):
        """
        Only the least significant bit is the sampled flag; other bits must not
        break parsing.
        """
        ctx = parse_trace_context(f"00-{TRACE_ID}-{SPAN_ID}-02")

        assert TraceContextStatus.NOT_SAMPLED is ctx.status
        assert 2 == ctx.trace_flags

    @pytest.mark.parametrize("traceparent", [None, "", "   ", "\t"])
    def test_missing(self, traceparent):
        """
        A missing or blank header is reported as missing and yields no IDs.
        """
        ctx = parse_trace_context(traceparent)

        assert TraceContextStatus.MISSING is ctx.status
        assert ctx.trace_id is None
        assert ctx.span_id is None
        assert ctx.sampled is False
        assert ctx.error is None

    @pytest.mark.parametrize(
        ("traceparent", "error"),
        [
            ("00", "malformed"),
            ("00-abc", "malformed"),
            (f"00-{TRACE_ID}-{SPAN_ID}", "malformed"),
            ("00-abc-abc-01", "invalid_trace_id"),
            (f"00-{TRACE_ID.upper()}-{SPAN_ID}-01", "invalid_trace_id"),
            (f"00-{'g' * 32}-{SPAN_ID}-01", "invalid_trace_id"),
            (f"00-{'0' * 32}-{SPAN_ID}-01", "zero_trace_id"),
            (f"00-{TRACE_ID}-{'0' * 16}-01", "zero_span_id"),
            (f"00-{TRACE_ID}-{SPAN_ID.upper()}-01", "invalid_span_id"),
            (f"00-{TRACE_ID}-abc-01", "invalid_span_id"),
            (f"00-{TRACE_ID}-{SPAN_ID}-1", "invalid_trace_flags"),
            (f"00-{TRACE_ID}-{SPAN_ID}-zz", "invalid_trace_flags"),
            (f"00-{TRACE_ID}-{SPAN_ID}-01-", "malformed"),
            (f"ff-{TRACE_ID}-{SPAN_ID}-01", "invalid_version"),
            (f"FF-{TRACE_ID}-{SPAN_ID}-01", "invalid_version"),
            (f"0-{TRACE_ID}-{SPAN_ID}-01", "invalid_version"),
            (f"zz-{TRACE_ID}-{SPAN_ID}-01", "invalid_version"),
            (f"00-{TRACE_ID}-{SPAN_ID}-01-extra", "trailing_data"),
        ],
    )
    def test_invalid(self, traceparent, error):
        """
        Invalid headers are reported as invalid, name the reason, and -- most
        importantly -- do not surface the inbound values as trusted IDs.
        """
        ctx = parse_trace_context(traceparent)

        assert TraceContextStatus.INVALID is ctx.status
        assert error == ctx.error
        assert ctx.trace_id is None
        assert ctx.span_id is None
        assert ctx.sampled is False

    def test_version_other_than_zero_with_extra_fields(self):
        """
        Future versions may append fields to the header.
        """
        ctx = parse_trace_context(f"01-{TRACE_ID}-{SPAN_ID}-01-extra")

        assert TraceContextStatus.SAMPLED is ctx.status
        assert TRACE_ID == ctx.trace_id

    @pytest.mark.parametrize("traceparent", [42, b"00", ["00"]])
    def test_non_string_is_invalid(self, traceparent):
        """
        Non-string values don't blow up or get coerced into IDs.
        """
        ctx = parse_trace_context(traceparent)

        assert TraceContextStatus.INVALID is ctx.status
        assert ctx.trace_id is None

    def test_headers_mapping(self):
        """
        A mapping of headers is searched case-insensitively for both headers.
        """
        ctx = parse_trace_context(
            {"TraceParent": TRACEPARENT, "TraceState": "congo=a"}
        )

        assert TraceContextStatus.SAMPLED is ctx.status
        assert TRACE_ID == ctx.trace_id
        assert (("congo", "a"),) == ctx.tracestate

    def test_empty_headers_mapping(self):
        """
        A mapping without trace headers is just a missing header.
        """
        ctx = parse_trace_context({"accept": "*/*"})

        assert TraceContextStatus.MISSING is ctx.status

    def test_headers_mapping_and_tracestate_conflict(self):
        """
        Passing both a mapping and a raw tracestate is a programming error.
        """
        with pytest.raises(ValueError, match="tracestate"):
            parse_trace_context({"traceparent": TRACEPARENT}, "congo=a")

    def test_tracestate(self):
        """
        Valid tracestate members are kept in order and as key-value pairs.
        """
        ctx = parse_trace_context(
            TRACEPARENT, " congo=t61rcWkgMzE , rojo=00f067aa0ba902b7 "
        )

        assert TraceContextStatus.SAMPLED is ctx.status
        assert (
            ("congo", "t61rcWkgMzE"),
            ("rojo", "00f067aa0ba902b7"),
        ) == ctx.tracestate
        assert ctx.error is None

    def test_tracestate_with_tenant_and_system(self):
        """
        The multi-tenant ``tenant-id@system-id`` form is accepted.
        """
        ctx = parse_trace_context(TRACEPARENT, "t1@sys=value,1abc@x=y")

        assert (("t1@sys", "value"), ("1abc@x", "y")) == ctx.tracestate

    @pytest.mark.parametrize(
        "tracestate",
        [
            "congo",  # no key-value pair
            "=value",  # empty key
            "congo=",  # empty value
            "congo=a=b",  # too many equals signs
            "congo=a,",  # empty trailing member
            "congo=a,congo=b",  # duplicate keys
            "CONGO=a",  # keys must be lowercase
            "1abc=a",  # simple keys can't start with a digit
            "congo= a",  # leading space in value
            "congo=" + "a" * 257,  # value too long
            ",".join(f"k{i}=v" for i in range(33)),  # too many members
            "a@=b",  # empty system
            "@sys=b",  # empty tenant
            "a@1sys=b",  # system must start with a letter
        ],
    )
    def test_invalid_tracestate_only_drops_tracestate(self, tracestate):
        """
        A malformed tracestate doesn't invalidate a good traceparent, but it is
        dropped instead of being passed on verbatim.
        """
        ctx = parse_trace_context(TRACEPARENT, tracestate)

        assert TraceContextStatus.SAMPLED is ctx.status
        assert TRACE_ID == ctx.trace_id
        assert () == ctx.tracestate
        assert "invalid_tracestate" == ctx.error

    def test_tracestate_is_ignored_without_traceparent(self):
        """
        Without a traceparent there's no trace to attach a tracestate to.
        """
        ctx = parse_trace_context(None, "congo=a")

        assert TraceContextStatus.MISSING is ctx.status
        assert () == ctx.tracestate

    def test_as_event_dict_sampled(self):
        """
        The correlation fields contain the validated IDs and the state.
        """
        ctx = parse_trace_context(TRACEPARENT, "congo=a")

        assert {
            "trace_context_status": "sampled",
            "trace_id": TRACE_ID,
            "span_id": SPAN_ID,
            "trace_flags": 1,
            "trace_sampled": True,
            "tracestate": "congo=a",
        } == ctx.as_event_dict()

    def test_as_event_dict_missing(self):
        """
        A missing header is observable, but produces no fake IDs.
        """
        assert {"trace_context_status": "missing"} == (
            parse_trace_context(None).as_event_dict()
        )

    def test_as_event_dict_invalid(self):
        """
        An invalid header is observable, but produces no fake IDs.
        """
        assert {
            "trace_context_status": "invalid",
            "trace_context_error": "zero_trace_id",
        } == parse_trace_context(f"00-{'0' * 32}-{SPAN_ID}-01").as_event_dict()

    def test_as_event_dict_not_sampled(self):
        """
        Explicitly unsampled traces keep their IDs, but say they're unsampled.
        """
        event_dict = parse_trace_context(
            f"00-{TRACE_ID}-{SPAN_ID}-00"
        ).as_event_dict()

        assert "not_sampled" == event_dict["trace_context_status"]
        assert event_dict["trace_sampled"] is False
        assert TRACE_ID == event_dict["trace_id"]

    def test_trace_context_is_frozen(self):
        """
        TraceContext values are immutable.
        """
        ctx = parse_trace_context(TRACEPARENT)

        with pytest.raises(AttributeError):
            ctx.trace_id = "nope"


class TestBindTraceContext:
    def test_binds_correlation_fields(self):
        """
        Binding a traceparent adds its correlation fields to the context.
        """
        bind_trace_context(TRACEPARENT, "congo=a")

        assert {
            "trace_context_status": "sampled",
            "trace_id": TRACE_ID,
            "span_id": SPAN_ID,
            "trace_flags": 1,
            "trace_sampled": True,
            "tracestate": "congo=a",
        } == get_contextvars()

    def test_accepts_parsed_context(self):
        """
        A TraceContext can be bound as-is.
        """
        bind_trace_context(parse_trace_context(TRACEPARENT))

        assert TRACE_ID == get_contextvars()["trace_id"]

    def test_accepts_parsed_context_with_prefix(self):
        """
        key_prefix is applied to all trace fields.
        """
        bind_trace_context(
            parse_trace_context(TRACEPARENT), key_prefix="http_"
        )

        assert TRACE_ID == get_contextvars()["http_trace_id"]

    def test_parsed_context_and_tracestate_conflict(self):
        """
        Passing both a parsed context and a raw tracestate is a programming
        error and not silently resolved.
        """
        with pytest.raises(ValueError, match="tracestate"):
            bind_trace_context(parse_trace_context(TRACEPARENT), "congo=a")

    def test_accepts_headers_mapping(self):
        """
        A mapping of headers is looked up case-insensitively and bound.
        """
        bind_trace_context(
            {
                "TraceParent": TRACEPARENT,
                "TRACESTATE": "congo=a",
                "User-Agent": "curl",
            }
        )

        assert {
            "trace_context_status": "sampled",
            "trace_id": TRACE_ID,
            "span_id": SPAN_ID,
            "trace_flags": 1,
            "trace_sampled": True,
            "tracestate": "congo=a",
        } == get_contextvars()

    def test_headers_mapping_without_headers_is_missing(self):
        """
        A headers mapping without trace headers is reported as missing
        instead of failing.
        """
        bind_trace_context({"User-Agent": "curl"})

        assert {"trace_context_status": "missing"} == get_contextvars()

    def test_repeated_tracestate_headers_are_joined(self):
        """
        The specification allows splitting tracestate over several header
        lines, in order.
        """
        bind_trace_context(
            {
                "traceparent": TRACEPARENT,
                "tracestate": ["congo=a", "rojo=b"],
            }
        )

        assert "congo=a,rojo=b" == get_contextvars()["tracestate"]

    @pytest.mark.parametrize(
        "headers",
        [
            {"traceparent": [TRACEPARENT, TRACEPARENT]},
            {"traceparent": ""},
        ],
    )
    def test_untrustworthy_traceparent_headers(self, headers):
        """
        Repeated or empty traceparent headers are not trusted.
        """
        bind_trace_context(headers)

        assert "trace_id" not in get_contextvars()

    def test_mapping_and_tracestate_conflict(self):
        """
        Passing both a mapping and a raw tracestate is a programming error.
        """
        with pytest.raises(ValueError, match="tracestate"):
            bind_trace_context({"traceparent": TRACEPARENT}, "congo=a")

    def test_invalid_does_not_bind_inbound_ids(self):
        """
        An invalid inbound header never becomes a trusted trace_id -- only the
        failure is observable.
        """
        bind_trace_context(f"00-{TRACE_ID.upper()}-{SPAN_ID}-01")

        assert {
            "trace_context_status": "invalid",
            "trace_context_error": "invalid_trace_id",
        } == get_contextvars()

    def test_missing_overrides_previous_trace(self):
        """
        A request without traceparent must not inherit the previous trace.
        """
        bind_trace_context(TRACEPARENT)
        bind_trace_context(None)

        assert {"trace_context_status": "missing"} == get_contextvars()

    def test_keeps_business_fields(self):
        """
        bind_contextvars keeps binding arbitrary fields and bind_trace_context
        only manages its own keys.
        """
        bind_contextvars(user_id=42, request_id="r-1")

        bind_trace_context(TRACEPARENT)

        assert {
            "user_id": 42,
            "request_id": "r-1",
            "trace_context_status": "sampled",
            "trace_id": TRACE_ID,
            "span_id": SPAN_ID,
            "trace_flags": 1,
            "trace_sampled": True,
        } == get_contextvars()

    def test_no_global_trace_format_enforcement(self):
        """
        Regular bind_contextvars neither validates nor rewrites trace-like
        fields.
        """
        bind_contextvars(trace_id="not-a-valid-trace-id", span_id=1234)

        assert {
            "trace_id": "not-a-valid-trace-id",
            "span_id": 1234,
        } == get_contextvars()

    def test_reset_restores_previous_trace(self):
        """
        The returned tokens restore the previous trace context exactly --
        including fields that were absent before.
        """
        bind_contextvars(user_id=1)
        tokens = bind_trace_context(TRACEPARENT, "congo=a")

        reset_contextvars(**tokens)

        assert {"user_id": 1} == get_contextvars()

    def test_reset_restores_overridden_trace(self):
        """
        Tokens restore the trace context that was bound before.
        """
        bind_trace_context(TRACEPARENT)
        tokens = bind_trace_context(None)

        assert {"trace_context_status": "missing"} == get_contextvars()

        reset_contextvars(**tokens)

        assert "sampled" == get_contextvars()["trace_context_status"]
        assert TRACE_ID == get_contextvars()["trace_id"]

    def test_merges_into_event_dict(self):
        """
        The bound fields are ordinary context-local values and get merged by
        merge_contextvars.
        """
        bind_trace_context(TRACEPARENT)

        event_dict = merge_contextvars(None, None, {"event": "hi"})

        assert TRACE_ID == event_dict["trace_id"]
        assert SPAN_ID == event_dict["span_id"]
        assert "sampled" == event_dict["trace_context_status"]
        assert "hi" == event_dict["event"]

    def test_unbind_trace_context(self):
        """
        unbind_trace_context removes only trace fields.
        """
        bind_contextvars(user_id=1)
        bind_trace_context(TRACEPARENT)

        unbind_trace_context()

        assert {"user_id": 1} == get_contextvars()

    def test_unbind_trace_context_with_prefix(self):
        """
        unbind_trace_context honors the prefix.
        """
        bind_trace_context(TRACEPARENT, key_prefix="http_")

        unbind_trace_context(key_prefix="http_")

        assert {} == get_contextvars()


class TestBoundTraceContext:
    def test_restores_context(self):
        """
        Leaving the context manager removes the trace fields again.
        """
        with bound_trace_context(TRACEPARENT) as ctx:
            assert TRACE_ID == get_contextvars()["trace_id"]
            assert isinstance(ctx, TraceContext)
            assert ctx.status is TraceContextStatus.SAMPLED

        assert {} == get_contextvars()

    def test_nested_entry_points_restore_outer_context(self):
        """
        A nested entry point gets its own trace and the outer one is restored
        afterwards.
        """
        outer = f"00-{'1' * 32}-{'2' * 16}-01"
        inner = f"00-{'3' * 32}-{'4' * 16}-01"

        with bound_trace_context(outer):
            assert "1" * 32 == get_contextvars()["trace_id"]

            with bound_trace_context(inner):
                assert "3" * 32 == get_contextvars()["trace_id"]

            assert "1" * 32 == get_contextvars()["trace_id"]

        assert {} == get_contextvars()

    def test_nesting_missing_inner(self):
        """
        An inner entry point without a header doesn't inherit the outer trace
        and doesn't wipe it either.
        """
        with bound_trace_context(TRACEPARENT):
            with bound_trace_context(None):
                assert {"trace_context_status": "missing"} == (
                    get_contextvars()
                )

            assert TRACE_ID == get_contextvars()["trace_id"]

    def test_preserves_business_fields(self):
        """
        Business fields bound inside and outside survive.
        """
        bind_contextvars(user_id=1)

        with bound_trace_context(TRACEPARENT):
            bind_contextvars(order_id=2)
            assert 1 == get_contextvars()["user_id"]
            assert TRACE_ID == get_contextvars()["trace_id"]

        assert {"user_id": 1, "order_id": 2} == get_contextvars()

    def test_restores_on_exception(self):
        """
        The context is restored even if the block raises.
        """
        bind_trace_context(TRACEPARENT)

        with pytest.raises(RuntimeError), bound_trace_context(None):
            raise RuntimeError("boom")

        assert TRACE_ID == get_contextvars()["trace_id"]

    def test_as_decorator(self):
        """
        bound_trace_context works as a decorator and preserves metadata.
        """

        @bound_trace_context(TRACEPARENT)
        def wrapped(arg1):
            """Wrapped documentation"""
            assert TRACE_ID == get_contextvars()["trace_id"]

            return arg1

        assert 23 == wrapped(23)
        assert {} == get_contextvars()

        assert "wrapped" == wrapped.__name__
        assert "(arg1)" == str(inspect.signature(wrapped))
        assert "Wrapped documentation" == wrapped.__doc__

    async def test_isolated_between_tasks(self):
        """
        Concurrent tasks don't see each other's trace contexts.
        """
        event_loop = asyncio.get_running_loop()
        task_a_ready = asyncio.Event()
        task_b_ready = asyncio.Event()

        async def task(trace_id, ready, wait_for):
            traceparent = f"00-{trace_id}-{SPAN_ID}-01"

            with bound_trace_context(traceparent):
                bind_contextvars(branch=trace_id[:1])

                ready.set()
                # Both tasks have bound their trace context before either
                # reads it back, so they truly interleave.
                await wait_for.wait()

                return get_contextvars()

        task_a = event_loop.create_task(
            task("1" * 32, task_a_ready, task_b_ready)
        )
        task_b = event_loop.create_task(
            task("2" * 32, task_b_ready, task_a_ready)
        )

        ctx_a = await task_a
        ctx_b = await task_b

        assert "1" * 32 == ctx_a["trace_id"]
        assert "1" == ctx_a["branch"]
        assert "2" * 32 == ctx_b["trace_id"]
        assert "2" == ctx_b["branch"]

        assert {} == get_contextvars()
