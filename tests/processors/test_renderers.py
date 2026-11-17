# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

from __future__ import annotations

import datetime as dt
import json
import pickle

import pytest
import time_machine

from structlog.processors import (
    ExceptionRenderer,
    JSONRenderer,
    KeyValueRenderer,
    LogfmtRenderer,
    MaybeTimeStamper,
    TimeStamper,
    _json_fallback_handler,
    format_exc_info,
)
from structlog.threadlocal import wrap_dict

from ..helpers import CustomError


try:
    import simplejson
except ImportError:
    simplejson = None


class TestKeyValueRenderer:
    def test_sort_keys(self, event_dict):
        """
        Keys are sorted if sort_keys is set.
        """
        rv = KeyValueRenderer(sort_keys=True)(None, None, event_dict)

        assert r"a=<A(\o/)> b=[3, 4] x=7 y='test' z=(1, 2)" == rv

    def test_order_complete(self, event_dict):
        """
        Orders keys according to key_order.
        """
        rv = KeyValueRenderer(key_order=["y", "b", "a", "z", "x"])(
            None, None, event_dict
        )

        assert r"y='test' b=[3, 4] a=<A(\o/)> z=(1, 2) x=7" == rv

    def test_order_missing(self, event_dict):
        """
        Missing keys get rendered as None.
        """
        rv = KeyValueRenderer(key_order=["c", "y", "b", "a", "z", "x"])(
            None, None, event_dict
        )

        assert r"c=None y='test' b=[3, 4] a=<A(\o/)> z=(1, 2) x=7" == rv

    def test_order_missing_dropped(self, event_dict):
        """
        Missing keys get dropped
        """
        rv = KeyValueRenderer(
            key_order=["c", "y", "b", "a", "z", "x"], drop_missing=True
        )(None, None, event_dict)

        assert r"y='test' b=[3, 4] a=<A(\o/)> z=(1, 2) x=7" == rv

    def test_order_extra(self, event_dict):
        """
        Extra keys get sorted if sort_keys=True.
        """
        event_dict["B"] = "B"
        event_dict["A"] = "A"

        rv = KeyValueRenderer(
            key_order=["c", "y", "b", "a", "z", "x"], sort_keys=True
        )(None, None, event_dict)

        assert (
            r"c=None y='test' b=[3, 4] a=<A(\o/)> z=(1, 2) x=7 A='A' B='B'"
        ) == rv

    def test_order_sorted_missing_dropped(self, event_dict):
        """
        Keys get sorted if sort_keys=True and extras get dropped.
        """
        event_dict["B"] = "B"
        event_dict["A"] = "A"

        rv = KeyValueRenderer(
            key_order=["c", "y", "b", "a", "z", "x"],
            sort_keys=True,
            drop_missing=True,
        )(None, None, event_dict)

        assert r"y='test' b=[3, 4] a=<A(\o/)> z=(1, 2) x=7 A='A' B='B'" == rv

    def test_random_order(self, event_dict):
        """
        No special ordering doesn't blow up.
        """
        rv = KeyValueRenderer()(None, None, event_dict)

        assert isinstance(rv, str)

    @pytest.mark.parametrize("rns", [True, False])
    def test_repr_native_str(self, rns):
        """
        repr_native_str=False doesn't repr on native strings.
        """
        rv = KeyValueRenderer(repr_native_str=rns)(
            None, None, {"event": "哈", "key": 42, "key2": "哈"}
        )

        cnt = rv.count("哈")
        assert 2 == cnt


class TestLogfmtRenderer:
    def test_sort_keys(self, event_dict):
        """
        Keys are sorted if sort_keys is set.
        """
        rv = LogfmtRenderer(sort_keys=True)(None, None, event_dict)

        assert r'a=<A(\o/)> b="[3, 4]" x=7 y=test z="(1, 2)"' == rv

    def test_order_complete(self, event_dict):
        """
        Orders keys according to key_order.
        """
        rv = LogfmtRenderer(key_order=["y", "b", "a", "z", "x"])(
            None, None, event_dict
        )

        assert r'y=test b="[3, 4]" a=<A(\o/)> z="(1, 2)" x=7' == rv

    def test_order_missing(self, event_dict):
        """
        Missing keys get rendered as None.
        """
        rv = LogfmtRenderer(key_order=["c", "y", "b", "a", "z", "x"])(
            None, None, event_dict
        )

        assert r'c= y=test b="[3, 4]" a=<A(\o/)> z="(1, 2)" x=7' == rv

    def test_order_missing_dropped(self, event_dict):
        """
        Missing keys get dropped
        """
        rv = LogfmtRenderer(
            key_order=["c", "y", "b", "a", "z", "x"], drop_missing=True
        )(None, None, event_dict)

        assert r'y=test b="[3, 4]" a=<A(\o/)> z="(1, 2)" x=7' == rv

    def test_order_extra(self, event_dict):
        """
        Extra keys get sorted if sort_keys=True.
        """
        event_dict["B"] = "B"
        event_dict["A"] = "A"

        rv = LogfmtRenderer(
            key_order=["c", "y", "b", "a", "z", "x"], sort_keys=True
        )(None, None, event_dict)

        assert (
            r'c= y=test b="[3, 4]" a=<A(\o/)> z="(1, 2)" x=7 A=A B=B'
        ) == rv

    def test_order_sorted_missing_dropped(self, event_dict):
        """
        Keys get sorted if sort_keys=True and extras get dropped.
        """
        event_dict["B"] = "B"
        event_dict["A"] = "A"

        rv = LogfmtRenderer(
            key_order=["c", "y", "b", "a", "z", "x"],
            sort_keys=True,
            drop_missing=True,
        )(None, None, event_dict)

        assert r'y=test b="[3, 4]" a=<A(\o/)> z="(1, 2)" x=7 A=A B=B' == rv

    def test_random_order(self, event_dict):
        """
        No special ordering doesn't blow up.
        """
        rv = LogfmtRenderer()(None, None, event_dict)

        assert isinstance(rv, str)

    def test_empty_event_dict(self):
        """
        Empty event dict renders as empty string.
        """
        rv = LogfmtRenderer()(None, None, {})

        assert "" == rv

    def test_bool_as_flag(self):
        """
        If activated, render ``{"a": True}`` as ``a`` instead of ``a=true``.
        """
        event_dict = {"a": True, "b": False}

        rv_abbrev = LogfmtRenderer(bool_as_flag=True)(None, None, event_dict)
        assert r"a b=false" == rv_abbrev

        rv_no_abbrev = LogfmtRenderer(bool_as_flag=False)(
            None, None, event_dict
        )
        assert r"a=true b=false" == rv_no_abbrev

    def test_reference_format(self):
        """
        Test rendering according to example at
        https://pkg.go.dev/github.com/kr/logfmt
        """
        event_dict = {
            "foo": "bar",
            "a": 14,
            "baz": "hello kitty",
            "cool%story": "bro",
            "f": True,
            "%^asdf": True,
        }

        rv = LogfmtRenderer()(None, None, event_dict)
        assert 'foo=bar a=14 baz="hello kitty" cool%story=bro f %^asdf' == rv

    def test_equal_sign_or_space_in_value(self):
        """
        Values with equal signs are always quoted.
        """
        event_dict = {
            "without": "somevalue",
            "withequal": "some=value",
            "withspace": "some value",
        }

        rv = LogfmtRenderer()(None, None, event_dict)
        assert (
            r'without=somevalue withequal="some=value" withspace="some value"'
            == rv
        )

    def test_invalid_key(self):
        """
        Keys cannot contain space characters.
        """
        event_dict = {"invalid key": "somevalue"}

        with pytest.raises(ValueError, match='Invalid key: "invalid key"'):
            LogfmtRenderer()(None, None, event_dict)

    def test_newline_in_value(self):
        """
        Newlines in values are escaped.
        """
        event_dict = {"with_newline": "some\nvalue"}

        rv = LogfmtRenderer()(None, None, event_dict)

        assert r"with_newline=some\nvalue" == rv

    @pytest.mark.parametrize(
        ("raw", "escaped"),
        [
            # Slash by itself does not need to be escaped.
            (r"a\slash", r"a\slash"),
            # A quote requires quoting, and escaping the quote.
            ('a"quote', r'"a\"quote"'),
            # If anything triggers quoting of the string, then the slash must
            # be escaped.
            (
                r'a\slash with space or a"quote',
                r'"a\\slash with space or a\"quote"',
            ),
            (
                r"I want to render this \"string\" with logfmtrenderer",
                r'"I want to render this \\\"string\\\" with logfmtrenderer"',
            ),
        ],
    )
    def test_escaping(self, raw, escaped):
        """
        Backslashes and quotes are escaped.
        """
        rv = LogfmtRenderer()(None, None, {"key": raw})

        assert f"key={escaped}" == rv


class TestJSONRenderer:
    def test_renders_json(self, event_dict):
        """
        Renders a predictable JSON string.
        """
        rv = JSONRenderer(sort_keys=True)(None, None, event_dict)

        assert (
            r'{"a": "<A(\\o/)>", "b": [3, 4], "x": 7, '
            r'"y": "test", "z": '
            r"[1, 2]}"
        ) == rv

    def test_FallbackEncoder_handles_ThreadLocalDictWrapped_dicts(self):
        """
        Our fallback handling handles properly ThreadLocalDictWrapper values.
        """
        with pytest.deprecated_call():
            d = wrap_dict(dict)

        s = json.dumps(d({"a": 42}), default=_json_fallback_handler)

        assert '{"a": 42}' == s

    def test_FallbackEncoder_falls_back(self):
        """
        The fallback handler uses repr if it doesn't know the type.
        """
        s = json.dumps(
            {"date": dt.date(1980, 3, 25)},
            default=_json_fallback_handler,
        )

        assert '{"date": "datetime.date(1980, 3, 25)"}' == s

    def test_serializer(self):
        """
        A custom serializer is used if specified.
        """
        jr = JSONRenderer(serializer=lambda obj, **kw: {"a": 42})
        obj = object()

        assert {"a": 42} == jr(None, None, obj)

    def test_custom_fallback(self):
        """
        A custom fallback handler can be used.
        """
        jr = JSONRenderer(default=lambda x: repr(x)[::-1])
        d = {"date": dt.date(1980, 3, 25)}

        assert '{"date": ")52 ,3 ,0891(etad.emitetad"}' == jr(None, None, d)

    @pytest.mark.skipif(simplejson is None, reason="simplejson is missing.")
    def test_simplejson(self, event_dict):
        """
        Integration test with simplejson.
        """
        jr = JSONRenderer(serializer=simplejson.dumps)

        assert {
            "a": "<A(\\o/)>",
            "b": [3, 4],
            "x": 7,
            "y": "test",
            "z": [1, 2],
        } == json.loads(jr(None, None, event_dict))


class TestJSONRendererExplicitMode:
    """
    Explicit mode: only registered adapters run, never __structlog__/repr().
    """

    def test_registered_adapter_is_used(self):
        """
        Registered types are converted by their adapter.
        """
        jr = JSONRenderer(type_adapters={dt.date: str})

        rv = jr(None, None, {"date": dt.date(1980, 3, 25)})

        assert '{"date": "1980-03-25"}' == rv

    def test_unregistered_type_gets_type_marker(self):
        """
        Unregistered objects are replaced by a type marker; __structlog__
        and __repr__ are not called.
        """

        class Sneaky:
            def __structlog__(self):  # pragma: no cover
                raise AssertionError("__structlog__ must not be called")

            def __repr__(self):  # pragma: no cover
                raise AssertionError("__repr__ must not be called")

        jr = JSONRenderer(type_adapters={})

        rv = jr(None, None, {"obj": Sneaky()})

        assert "Sneaky" in rv
        assert "unregistered-type" in rv

    def test_adapter_priority_exact_type_beats_base(self):
        """
        An exact type registration wins over a base class registration.
        """

        class Base:
            pass

        class Child(Base):
            pass

        jr = JSONRenderer(
            type_adapters={Base: lambda o: "base", Child: lambda o: "child"}
        )

        rv = jr(None, None, {"obj": Child()})

        assert '{"obj": "child"}' == rv

    def test_adapter_priority_mro(self):
        """
        Without an exact registration, the nearest registered ancestor in
        the MRO applies.
        """

        class Base:
            pass

        class Middle(Base):
            pass

        class Child(Middle):
            pass

        jr = JSONRenderer(
            type_adapters={
                Base: lambda o: "base",
                Middle: lambda o: "middle",
            }
        )

        rv = jr(None, None, {"obj": Child()})

        assert '{"obj": "middle"}' == rv

    def test_adapter_error_marker_and_no_retry(self):
        """
        A failing adapter produces a dedicated marker and is called exactly
        once.
        """
        calls = 0

        class Boom:
            pass

        def bad_adapter(obj):
            nonlocal calls
            calls += 1
            raise RuntimeError("kaputt")

        jr = JSONRenderer(type_adapters={Boom: bad_adapter})

        rv = jr(None, None, {"obj": Boom()})

        assert 1 == calls
        assert "adapter-error" in rv
        assert "RuntimeError" in rv
        assert "kaputt" not in rv

    def test_circular_container_gets_marker(self):
        """
        Circular containers are replaced by a marker instead of recursing
        forever.
        """
        jr = JSONRenderer(type_adapters={})
        circle = [1, 2]
        circle.append(circle)

        rv = jr(None, None, {"circle": circle})

        assert '{"circle": [1, 2, "<structlog:circular-reference>"]}' == rv

    def test_adapter_result_is_checked_for_cycles(self):
        """
        Containers produced by an adapter are still checked for cycles.
        """

        class Node:
            pass

        node = Node()
        holder = {"node": node}
        holder["self"] = holder

        jr = JSONRenderer(type_adapters={Node: lambda o: holder})

        rv = jr(None, None, {"obj": node})

        assert "circular-reference" in rv

    def test_adapter_returning_itself_terminates(self):
        """
        An adapter returning the object itself does not loop forever.
        """

        class Ouroboros:
            pass

        jr = JSONRenderer(type_adapters={Ouroboros: lambda o: o})

        rv = jr(None, None, {"obj": Ouroboros()})

        assert "circular-reference" in rv

    def test_max_items_marker(self):
        """
        Containers exceeding max_items are replaced by a marker.
        """
        jr = JSONRenderer(type_adapters={}, max_items=2)

        rv = jr(None, None, {"ok": [1, 2], "big": [1, 2, 3]})

        assert '{"ok": [1, 2], "big": "<structlog:max-items-exceeded>"}' == rv

    def test_max_items_applies_to_adapter_results(self):
        """
        Containers produced by adapters are size-checked too.
        """

        class Wide:
            pass

        jr = JSONRenderer(
            type_adapters={Wide: lambda o: [1, 2, 3]}, max_items=2
        )

        rv = jr(None, None, {"obj": Wide()})

        assert '{"obj": "<structlog:max-items-exceeded>"}' == rv

    def test_max_depth_marker(self):
        """
        Structures nested deeper than max_depth are replaced by a marker.
        """
        jr = JSONRenderer(type_adapters={}, max_depth=1)

        rv = jr(None, None, {"deep": [[["too deep"]]]})

        assert '{"deep": ["<structlog:max-depth-exceeded>"]}' == rv

    def test_does_not_mutate_event_dict_or_objects(self):
        """
        The original event dict and contained objects are left untouched.
        """

        class Thing:
            def __init__(self):
                self.value = 42

        thing = Thing()
        nested = {"items": [1, 2], "thing": thing}
        event_dict = {"nested": nested}

        jr = JSONRenderer(type_adapters={Thing: lambda o: {"v": o.value}})
        rv = jr(None, None, event_dict)

        assert {"nested": {"items": [1, 2], "thing": thing}} == event_dict
        assert 42 == thing.value
        assert '{"nested": {"items": [1, 2], "thing": {"v": 42}}}' == rv

    def test_nested_unregistered_inside_adapter_result(self):
        """
        Unregistered objects inside adapter results get type markers.
        """

        class Registered:
            pass

        class Unregistered:
            pass

        jr = JSONRenderer(
            type_adapters={Registered: lambda o: {"inner": Unregistered()}}
        )

        rv = jr(None, None, {"obj": Registered()})

        assert "unregistered-type" in rv
        assert "Unregistered" in rv

    def test_limits_without_type_adapters_raise(self):
        """
        max_depth/max_items without type_adapters are a configuration error.
        """
        with pytest.raises(ValueError, match="require explicit mode"):
            JSONRenderer(max_depth=3)

        with pytest.raises(ValueError, match="require explicit mode"):
            JSONRenderer(max_items=3)

    def test_non_native_dict_keys(self):
        """
        Non-string keys are converted; unconvertible ones become markers.
        """
        jr = JSONRenderer(type_adapters={dt.date: str})

        rv = jr(None, None, {dt.date(1980, 3, 25): "day", (1, 2): "tuple"})

        assert (
            '{"1980-03-25": "day", "<structlog:unregistered-type builtins.tuple>": "tuple"}'
            == rv
        )


class TestTimeStamper:
    def test_disallows_non_utc_unix_timestamps(self):
        """
        A asking for a UNIX timestamp with a timezone that's not UTC raises a
        ValueError.
        """
        with pytest.raises(ValueError, match="UNIX timestamps are always UTC"):
            TimeStamper(utc=False)

    def test_inserts_utc_unix_timestamp_by_default(self):
        """
        Per default a float UNIX timestamp is used.
        """
        ts = TimeStamper()
        d = ts(None, None, {})

        assert isinstance(d["timestamp"], float)

    @time_machine.travel("1980-03-25 16:00:00")
    def test_local(self):
        """
        Timestamp in local timezone work. Due to historic reasons, the default
        format does not include a timezone.
        """
        ts = TimeStamper(fmt="iso", utc=False)
        d = ts(None, None, {})

        assert "1980-03-25T16:00:00" == d["timestamp"]

    @time_machine.travel("1980-03-25 16:00:00")
    def test_formats(self):
        """
        The fmt string is respected.
        """
        ts = TimeStamper(fmt="%Y")
        d = ts(None, None, {})

        assert "1980" == d["timestamp"]

    @time_machine.travel(
        dt.datetime(1980, 3, 25, 16, 0, 0, tzinfo=dt.timezone.utc)
    )
    def test_inserts_formatted_utc(self):
        """
        The fmt string in UTC timezone works.
        """

        ts = TimeStamper(fmt="%Y-%m-%d %H:%M:%S %Z")
        d = ts(None, None, {})

        assert "1980-03-25 16:00:00 UTC" == d["timestamp"]

    @time_machine.travel("1980-03-25 16:00:00")
    def test_inserts_formatted_local(self):
        """
        The fmt string in local timezone works.
        """
        local_tz = dt.datetime.now().astimezone().tzname()
        ts = TimeStamper(fmt="%Y-%m-%d %H:%M:%S %Z", utc=False)
        d = ts(None, None, {})

        assert f"1980-03-25 16:00:00 {local_tz}" == d["timestamp"]

    @time_machine.travel("1980-03-25 16:00:00")
    def test_tz_aware(self):
        """
        The timestamp that is used for formatting is timezone-aware.
        """
        ts = TimeStamper(fmt="%z")
        d = ts(None, None, {})

        assert "" == dt.datetime.now().strftime("%z")  # noqa: DTZ005
        assert "" != d["timestamp"]

    @time_machine.travel(
        dt.datetime(1980, 3, 25, 16, 0, 0, tzinfo=dt.timezone.utc)
    )
    def test_adds_Z_to_iso(self):
        """
        stdlib's isoformat is buggy, so we fix it.
        """
        ts = TimeStamper(fmt="iso", utc=True)
        d = ts(None, None, {})

        assert "1980-03-25T16:00:00Z" == d["timestamp"]

    @time_machine.travel("1980-03-25 16:00:00")
    def test_key_can_be_specified(self):
        """
        Timestamp is stored with the specified key.
        """
        ts = TimeStamper(fmt="%m", key="month")
        d = ts(None, None, {})

        assert "03" == d["month"]

    @time_machine.travel("1980-03-25 16:00:00", tick=False)
    @pytest.mark.parametrize("fmt", [None, "%Y"])
    @pytest.mark.parametrize("utc", [True, False])
    @pytest.mark.parametrize("key", [None, "other-key"])
    @pytest.mark.parametrize("proto", range(pickle.HIGHEST_PROTOCOL + 1))
    def test_pickle(self, fmt, utc, key, proto):
        """
        TimeStamper is serializable.
        """
        # UNIX timestamps must be UTC.
        if fmt is None and not utc:
            pytest.skip()

        ts = TimeStamper()

        assert ts(None, None, {}) == pickle.loads(pickle.dumps(ts, proto))(
            None, None, {}
        )

    def test_apply_time_machine_after_instantiation(self):
        """
        Freezing time after instantiation of TimeStamper works.
        """
        ts = TimeStamper(fmt="iso", utc=False)

        # Simulate a different local time by traveling to a different timestamp
        # after the stamper was created.
        with time_machine.travel("1980-03-25 17:00:00"):
            d = ts(None, None, {})

            assert "1980-03-25T17:00:00" == d["timestamp"]


class TestMaybeTimeStamper:
    def test_overwrite(self):
        """
        If there is a timestamp, leave it.
        """
        mts = MaybeTimeStamper()

        assert {"timestamp": 42} == mts(None, None, {"timestamp": 42})

    def test_overwrite_custom_key(self):
        """
        If there is a timestamp with a custom key, leave it.
        """
        mts = MaybeTimeStamper(key="timestamp2")

        assert {"timestamp2": 42} == mts(None, None, {"timestamp2": 42})

    def test_none(self):
        """
        If there is no timestamp, add one.
        """
        mts = MaybeTimeStamper()

        assert "timestamp" in mts(None, None, {})


class TestFormatExcInfo:
    def test_custom_formatter(self):
        """
        The exception formatter can be changed.
        """
        formatter = ExceptionRenderer(lambda _: "There is no exception!")

        try:
            raise CustomError("test")
        except CustomError as e:
            exc = e

        assert formatter(None, None, {"exc_info": exc}) == {
            "exception": "There is no exception!"
        }

    @pytest.mark.parametrize("ei", [False, None, ""])
    def test_nop(self, ei):
        """
        If exc_info is falsey, only remove the key.
        """
        assert {} == ExceptionRenderer()(None, None, {"exc_info": ei})

    def test_nop_missing(self):
        """
        If event dict doesn't contain exc_info, do nothing.
        """
        assert {} == ExceptionRenderer()(None, None, {})

    def test_formats_tuple(self):
        """
        If exc_info is an arbitrary 3-tuple, it is not used.
        """
        formatter = ExceptionRenderer(lambda exc_info: exc_info)
        d = formatter(None, None, {"exc_info": (None, None, 42)})

        assert {} == d

    def test_gets_exc_info_on_bool(self):
        """
        If exc_info is True, it is obtained using sys.exc_info().
        """
        # monkeypatching sys.exc_info makes currently pytest return 1 on
        # success.
        try:
            raise ValueError("test")
        except ValueError:
            d = ExceptionRenderer()(None, None, {"exc_info": True})

        assert "exc_info" not in d
        assert 'raise ValueError("test")' in d["exception"]
        assert "ValueError: test" in d["exception"]

    def test_exception(self):
        """
        Passing exceptions as exc_info is valid.
        """
        formatter = ExceptionRenderer(lambda exc_info: exc_info)

        try:
            raise ValueError("test")
        except ValueError as e:
            exc = e
        else:
            pytest.fail("Exception not raised.")

        assert {
            "exception": (ValueError, exc, exc.__traceback__)
        } == formatter(None, None, {"exc_info": exc})

    def test_exception_without_traceback(self):
        """
        If an Exception is missing a traceback, render it anyway.
        """
        rv = ExceptionRenderer()(
            None, None, {"exc_info": Exception("no traceback!")}
        )

        assert {"exception": "Exception: no traceback!"} == rv

    def test_format_exception(self):
        """
        "format_exception" is the "ExceptionRenderer" with default settings.
        """
        try:
            raise ValueError("test")
        except ValueError as e:
            a = format_exc_info(None, None, {"exc_info": e})
            b = ExceptionRenderer()(None, None, {"exc_info": e})

        assert a == b

    @pytest.mark.parametrize("ei", [True, (None, None, None)])
    def test_no_exception(self, ei):
        """
        A missing exception does not blow up.
        """
        assert {} == format_exc_info(None, None, {"exc_info": ei})
