# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

from __future__ import annotations

import functools
import inspect
import json
import logging
import os
import pickle
import sys
import threading

from io import StringIO

import pytest

import structlog

from structlog import BoundLogger
from structlog._utils import get_processname
from structlog.exceptions import EventTooLargeError
from structlog.processors import (
    ByteBudget,
    CallsiteParameter,
    CallsiteParameterAdder,
    EventRenamer,
    ExceptionPrettyPrinter,
    JSONRenderer,
    StackInfoRenderer,
    UnicodeDecoder,
    UnicodeEncoder,
    _figure_out_exc_info,
    dict_tracebacks,
    format_exc_info,
)
from structlog.stdlib import ProcessorFormatter
from structlog.testing import CapturingLoggerFactory
from structlog.typing import EventDict, ExcInfo

from ..additional_frame import additional_frame


try:
    import simplejson
except ImportError:
    simplejson = None


class TestUnicodeEncoder:
    def test_encodes(self):
        """
        Unicode strings get encoded (as UTF-8 by default).
        """
        e = UnicodeEncoder()

        assert {"foo": b"b\xc3\xa4r"} == e(None, None, {"foo": "b\xe4r"})

    def test_passes_arguments(self):
        """
        Encoding options are passed into the encoding call.
        """
        e = UnicodeEncoder("latin1", "xmlcharrefreplace")

        assert {"foo": b"&#8211;"} == e(None, None, {"foo": "\u2013"})

    def test_bytes_nop(self):
        """
        If the string is already bytes, don't do anything.
        """
        e = UnicodeEncoder()

        assert {"foo": b"b\xc3\xa4r"} == e(None, None, {"foo": b"b\xc3\xa4r"})


class TestUnicodeDecoder:
    def test_decodes(self):
        """
        Byte strings get decoded (as UTF-8 by default).
        """
        ud = UnicodeDecoder()

        assert {"foo": "b\xe4r"} == ud(None, None, {"foo": b"b\xc3\xa4r"})

    def test_passes_arguments(self):
        """
        Encoding options are passed into the encoding call.
        """
        ud = UnicodeDecoder("utf-8", "ignore")

        assert {"foo": ""} == ud(None, None, {"foo": b"\xa1\xa4"})

    def test_bytes_nop(self):
        """
        If the value is already unicode, don't do anything.
        """
        ud = UnicodeDecoder()

        assert {"foo": "b\u2013r"} == ud(None, None, {"foo": "b\u2013r"})


class TestExceptionPrettyPrinter:
    def test_stdout_by_default(self):
        """
        If no file is supplied, use stdout.
        """
        epp = ExceptionPrettyPrinter()

        assert sys.stdout is epp._file

    def test_prints_exception(self, sio):
        """
        If there's an `exception` key in the event_dict, just print it out.
        This happens if `format_exc_info` was run before us in the chain.
        """
        epp = ExceptionPrettyPrinter(file=sio)
        try:
            raise ValueError
        except ValueError:
            ed = format_exc_info(None, None, {"exc_info": True})
        epp(None, None, ed)

        out = sio.getvalue()

        assert "test_prints_exception" in out
        assert "raise ValueError" in out

    def test_uses_exception_formatter(self, sio):
        """
        If an `exception_formatter` is passed, use that to render the
        exception rather than the default.
        """

        def formatter(exc_info: ExcInfo) -> str:
            return f"error: {exc_info}"

        epp = ExceptionPrettyPrinter(file=sio, exception_formatter=formatter)
        try:
            raise ValueError
        except ValueError as e:
            epp(None, None, {"exc_info": True})
            formatted = formatter(e)

        out = sio.getvalue()

        assert formatted in out

    def test_removes_exception_after_printing(self, sio):
        """
        After pretty printing `exception` is removed from the event_dict.
        """
        epp = ExceptionPrettyPrinter(sio)
        try:
            raise ValueError
        except ValueError:
            ed = format_exc_info(None, None, {"exc_info": True})

        assert "exception" in ed

        new_ed = epp(None, None, ed)

        assert "exception" not in new_ed

    def test_handles_exc_info(self, sio):
        """
        If `exc_info` is passed in, it behaves like `format_exc_info`.
        """
        epp = ExceptionPrettyPrinter(sio)
        try:
            raise ValueError
        except ValueError:
            epp(None, None, {"exc_info": True})

        out = sio.getvalue()

        assert "test_handles_exc_info" in out
        assert "raise ValueError" in out

    def test_removes_exc_info_after_printing(self, sio):
        """
        After pretty printing `exception` is removed from the event_dict.
        """
        epp = ExceptionPrettyPrinter(sio)
        try:
            raise ValueError
        except ValueError:
            ed = epp(None, None, {"exc_info": True})

        assert "exc_info" not in ed

    def test_nop_if_no_exception(self, sio):
        """
        If there is no exception, don't print anything.
        """
        epp = ExceptionPrettyPrinter(sio)
        epp(None, None, {})

        assert "" == sio.getvalue()

    def test_own_exc_info(self, sio):
        """
        If exc_info is a tuple, use it.
        """
        epp = ExceptionPrettyPrinter(sio)
        try:
            raise ValueError("XXX")
        except ValueError:
            ei = sys.exc_info()

        epp(None, None, {"exc_info": ei})

        assert "XXX" in sio.getvalue()

    def test_exception_on_py3(self, sio):
        """
        On Python 3, it's also legal to pass an Exception.
        """
        epp = ExceptionPrettyPrinter(sio)
        try:
            raise ValueError("XXX")
        except ValueError as e:
            epp(None, None, {"exc_info": e})

        assert "XXX" in sio.getvalue()


@pytest.fixture
def sir():
    return StackInfoRenderer()


class TestStackInfoRenderer:
    def test_removes_stack_info(self, sir):
        """
        The `stack_info` key is removed from `event_dict`.
        """
        ed = sir(None, None, {"stack_info": True})

        assert "stack_info" not in ed

    def test_adds_stack_if_asked(self, sir):
        """
        If `stack_info` is true, `stack` is added.
        """
        ed = sir(None, None, {"stack_info": True})

        assert "stack" in ed

    def test_renders_correct_stack(self, sir):
        """
        The rendered stack is correct.
        """
        ed = sir(None, None, {"stack_info": True})

        assert 'ed = sir(None, None, {"stack_info": True})' in ed["stack"]

    def test_additional_ignores(self):
        """
        Filtering of names works.
        """
        sir = StackInfoRenderer(["tests.additional_frame"])

        ed = additional_frame(
            functools.partial(sir, None, None, {"stack_info": True})
        )

        assert "additional_frame.py" not in ed["stack"]


class TestFigureOutExcInfo:
    @pytest.mark.parametrize("true_value", [True, 1, 1.1])
    def test_obtains_exc_info_on_True(self, true_value):
        """
        If the passed argument evaluates to True obtain exc_info ourselves.
        """
        try:
            0 / 0
        except Exception:
            assert sys.exc_info() == _figure_out_exc_info(true_value)
        else:
            pytest.fail("Exception not raised.")

    def test_py3_exception_no_traceback(self):
        """
        Exceptions without tracebacks are simply returned with None for
        traceback.
        """
        e = ValueError()

        assert (e.__class__, e, None) == _figure_out_exc_info(e)


class TestCallsiteParameterAdder:
    parameter_strings = {
        "pathname",
        "filename",
        "module",
        "func_name",
        "lineno",
        "thread",
        "thread_name",
        "process",
        "process_name",
    }

    # Exclude QUAL_NAME and QUAL_MODULE from the general set to keep parity with
    # stdlib LogRecord-derived parameters. QUAL_NAME and QUAL_MODULE are tested
    # separately.
    _all_parameters = {
        p
        for p in set(CallsiteParameter)
        if p
        not in (CallsiteParameter.QUAL_NAME, CallsiteParameter.QUAL_MODULE)
    }

    def test_all_parameters(self) -> None:
        """
        All callsite parameters are included in ``self.parameter_strings`` and
        the dictionary returned by ``self.get_callsite_parameters`` contains
        keys for all callsite parameters.
        """

        assert self.parameter_strings == {
            member.value for member in self._all_parameters
        }
        assert self.parameter_strings == self.get_callsite_parameters().keys()

    @pytest.mark.skipif(
        sys.version_info < (3, 11), reason="QUAL_NAME requires Python 3.11+"
    )
    def test_qual_name_structlog(self) -> None:
        """
        QUAL_NAME is added for structlog-originated events on Python 3.11+.
        """
        processor = CallsiteParameterAdder(
            parameters={CallsiteParameter.QUAL_NAME}
        )
        event_dict: EventDict = {"event": "msg"}
        actual = processor(None, None, event_dict)

        assert actual["qual_name"].endswith(
            f"{self.__class__.__name__}.test_qual_name_structlog"
        )

    def test_qual_module_structlog(self) -> None:
        """
        QUAL_MODULE is added for structlog-originated events.
        """
        processor = CallsiteParameterAdder(
            parameters={CallsiteParameter.QUAL_MODULE}
        )
        event_dict: EventDict = {"event": "msg"}
        actual = processor(None, None, event_dict)

        assert "tests.processors.test_processors" == actual["qual_module"]

    def test_qual_name_logging_origin_absent(self) -> None:
        """
        QUAL_NAME is not sourced from stdlib LogRecord and remains absent
        (because it doesn't exist).
        """
        processor = CallsiteParameterAdder(
            parameters={CallsiteParameter.QUAL_NAME}
        )
        record = logging.LogRecord(
            "name",
            logging.INFO,
            __file__,
            0,
            "message",
            None,
            None,
            "func",
        )
        event_dict: EventDict = {
            "event": "message",
            "_record": record,
            "_from_structlog": False,
        }
        actual = processor(None, None, event_dict)

        assert "qual_name" not in actual

    @pytest.mark.asyncio
    @pytest.mark.parametrize(
        ("wrapper_class", "method_name"),
        [
            (structlog.stdlib.BoundLogger, "ainfo"),
            (structlog.stdlib.AsyncBoundLogger, "info"),
        ],
    )
    async def test_async(self, wrapper_class, method_name) -> None:
        """
        Callsite information for async invocations are correct.
        Thread information is now correctly captured before async bridge.
        """
        string_io = StringIO()

        class StringIOLogger(structlog.PrintLogger):
            def __init__(self):
                super().__init__(file=string_io)

        processor = self.make_processor(None, ["concurrent", "threading"])
        structlog.configure(
            processors=[processor, JSONRenderer()],
            logger_factory=StringIOLogger,
            wrapper_class=wrapper_class,
            cache_logger_on_first_use=True,
        )

        logger = structlog.stdlib.get_logger()

        # Capture thread info before async call
        expected_thread = threading.get_ident()
        expected_thread_name = threading.current_thread().name

        callsite_params = self.get_callsite_parameters()
        await getattr(logger, method_name)("baz")
        logger_params = json.loads(string_io.getvalue())

        assert expected_thread == logger_params["thread"]
        assert expected_thread_name == logger_params["thread_name"]

        # Remove thread info from comparison for remaining params
        for key in ["thread", "thread_name"]:
            callsite_params.pop(key)
            logger_params.pop(key)

        assert {"event": "baz", **callsite_params} == logger_params

    @pytest.mark.asyncio
    async def test_async_native_logger(self) -> None:
        """
        Callsite thread information for native async invocations is correct.
        """
        cf = CapturingLoggerFactory()
        structlog.configure(
            processors=[
                CallsiteParameterAdder(
                    parameters=[
                        CallsiteParameter.THREAD,
                        CallsiteParameter.THREAD_NAME,
                    ]
                ),
            ],
            logger_factory=cf,
            wrapper_class=structlog._native.BoundLoggerFilteringAtInfo,
            cache_logger_on_first_use=True,
        )

        logger = structlog.get_logger()

        expected_thread = threading.get_ident()
        expected_thread_name = threading.current_thread().name

        await logger.ainfo("test native async")

        captured = cf.logger.calls[0].kwargs

        assert expected_thread == captured["thread"]
        assert expected_thread_name == captured["thread_name"]

    def test_additional_ignores(self) -> None:
        """
        Stack frames from modules with names that start with values in
        `additional_ignores` are ignored when determining the callsite.
        """
        test_message = "test message"
        additional_ignores = ["tests.additional_frame"]
        processor = self.make_processor(None, additional_ignores)
        event_dict: EventDict = {"event": test_message}

        # Warning: the next two lines must appear exactly like this to make
        # line numbers match.
        callsite_params = self.get_callsite_parameters(1)
        actual = processor(None, None, event_dict)

        expected = {"event": test_message, **callsite_params}

        assert expected == actual

    @pytest.mark.parametrize("origin", ["logging", "structlog"])
    @pytest.mark.parametrize(
        "parameter_strings",
        [
            None,
            *[{parameter} for parameter in parameter_strings],
            set(),
            parameter_strings,
            {"pathname", "filename"},
            {"module", "func_name"},
        ],
    )
    def test_processor(
        self,
        origin: str,
        parameter_strings: set[str] | None,
    ):
        """
        The correct callsite parameters are added to event dictionaries.
        """
        test_message = "test message"
        processor = self.make_processor(parameter_strings)
        if origin == "structlog":
            event_dict: EventDict = {"event": test_message}
            callsite_params = self.get_callsite_parameters()
            actual = processor(None, None, event_dict)
        elif origin == "logging":
            callsite_params = self.get_callsite_parameters()
            record = logging.LogRecord(
                "name",
                logging.INFO,
                callsite_params["pathname"],
                callsite_params["lineno"],
                test_message,
                None,
                None,
                callsite_params["func_name"],
            )
            event_dict: EventDict = {
                "event": test_message,
                "_record": record,
                "_from_structlog": False,
            }
            actual = processor(None, None, event_dict)
        else:
            pytest.fail(f"invalid origin {origin}")
        actual = {
            key: value
            for key, value in actual.items()
            if not key.startswith("_")
        }
        callsite_params = self.filter_parameter_dict(
            callsite_params, parameter_strings
        )
        expected = {"event": test_message, **callsite_params}

        assert expected == actual

    @pytest.mark.parametrize(
        "setup",
        ["common-without-pre", "common-with-pre", "shared", "everywhere"],
    )
    @pytest.mark.parametrize("origin", ["logging", "structlog"])
    @pytest.mark.parametrize(
        "parameter_strings",
        [
            None,
            *[{parameter} for parameter in parameter_strings],
            set(),
            parameter_strings,
            {"pathname", "filename"},
            {"module", "func_name"},
        ],
    )
    def test_e2e(
        self,
        setup: str,
        origin: str,
        parameter_strings: set[str] | None,
    ) -> None:
        """
        Logging output contains the correct callsite parameters.
        """
        logger = logging.Logger(sys._getframe().f_code.co_name)
        string_io = StringIO()
        handler = logging.StreamHandler(string_io)
        processors = [self.make_processor(parameter_strings)]
        if setup == "common-without-pre":
            common_processors = processors
            formatter = ProcessorFormatter(
                processors=[*processors, JSONRenderer()]
            )
        elif setup == "common-with-pre":
            common_processors = processors
            formatter = ProcessorFormatter(
                foreign_pre_chain=processors,
                processors=[JSONRenderer()],
            )
        elif setup == "shared":
            common_processors = []
            formatter = ProcessorFormatter(
                processors=[*processors, JSONRenderer()],
            )
        elif setup == "everywhere":
            common_processors = processors
            formatter = ProcessorFormatter(
                foreign_pre_chain=processors,
                processors=[*processors, JSONRenderer()],
            )
        else:
            pytest.fail(f"invalid setup {setup}")
        handler.setFormatter(formatter)
        handler.setLevel(0)
        logger.addHandler(handler)
        logger.setLevel(0)

        test_message = "test message"
        if origin == "logging":
            callsite_params = self.get_callsite_parameters()
            logger.info(test_message)
        elif origin == "structlog":
            ctx = {}
            bound_logger = BoundLogger(
                logger,
                [*common_processors, ProcessorFormatter.wrap_for_formatter],
                ctx,
            )
            callsite_params = self.get_callsite_parameters()
            bound_logger.info(test_message)
        else:
            pytest.fail(f"invalid origin {origin}")

        callsite_params = self.filter_parameter_dict(
            callsite_params, parameter_strings
        )
        actual = {
            key: value
            for key, value in json.loads(string_io.getvalue()).items()
            if not key.startswith("_")
        }
        expected = {"event": test_message, **callsite_params}

        assert expected == actual

    def test_pickeable_callsite_parameter_adder(self) -> None:
        """
        An instance of ``CallsiteParameterAdder`` can be pickled.  This
        functionality may be used to propagate structlog configurations to
        subprocesses.
        """
        pickle.dumps(CallsiteParameterAdder())

    @classmethod
    def make_processor(
        cls,
        parameter_strings: set[str] | None,
        additional_ignores: list[str] | None = None,
    ) -> CallsiteParameterAdder:
        """
        Creates a ``CallsiteParameterAdder`` with parameters matching the
        supplied *parameter_strings* values and with the supplied
        *additional_ignores* values.

        Args:
            parameter_strings:
                Strings for which corresponding ``CallsiteParameters`` should
                be included in the resulting ``CallsiteParameterAdded``.

            additional_ignores:
                Used as *additional_ignores* for the resulting
                ``CallsiteParameterAdded``.
        """
        if parameter_strings is None:
            return CallsiteParameterAdder(
                parameters=cls._all_parameters,
                additional_ignores=additional_ignores,
            )

        parameters = cls.filter_parameters(parameter_strings)
        return CallsiteParameterAdder(
            parameters=parameters,
            additional_ignores=additional_ignores,
        )

    @classmethod
    def filter_parameters(
        cls, parameter_strings: set[str] | None
    ) -> set[CallsiteParameter]:
        """
        Returns a set containing all ``CallsiteParameter`` members with values
        that are in ``parameter_strings``.

        Args:
            parameter_strings:
                The parameters strings for which corresponding
                ``CallsiteParameter`` members should be returned. If this value
                is `None` then all ``CallsiteParameter`` will be returned.
        """
        if parameter_strings is None:
            return cls._all_parameters
        return {
            parameter
            for parameter in cls._all_parameters
            if parameter.value in parameter_strings
        }

    @classmethod
    def filter_parameter_dict(
        cls, input: dict[str, object], parameter_strings: set[str] | None
    ) -> dict[str, object]:
        """
        Returns a dictionary that is equivalent to *input* but with all keys
        not in *parameter_strings* removed.

        Args:
            parameter_strings:
                The keys to keep in the dictionary, if this value is ``None``
                then all keys matching ``cls.parameter_strings`` are kept.
        """
        if parameter_strings is None:
            parameter_strings = cls.parameter_strings
        return {
            key: value
            for key, value in input.items()
            if key in parameter_strings
        }

    @classmethod
    def get_callsite_parameters(cls, offset: int = 1) -> dict[str, object]:
        """
        This function creates dictionary of callsite parameters for the line
        that is ``offset`` lines after the invocation of this function.

        Args:
            offset:
                The amount of lines after the invocation of this function that
                callsite parameters should be generated for.
        """
        frame_info = inspect.stack()[1]
        frame_traceback = inspect.getframeinfo(frame_info[0])
        return {
            "pathname": frame_traceback.filename,
            "filename": os.path.basename(frame_traceback.filename),
            "module": os.path.splitext(
                os.path.basename(frame_traceback.filename)
            )[0],
            "func_name": frame_info.function,
            "lineno": frame_info.lineno + offset,
            "thread": threading.get_ident(),
            "thread_name": threading.current_thread().name,
            "process": os.getpid(),
            "process_name": get_processname(),
        }


class TestRenameKey:
    def test_rename_once(self):
        """
        Renaming event to something else works.
        """
        assert {"msg": "hi", "foo": "bar"} == EventRenamer("msg")(
            None, None, {"event": "hi", "foo": "bar"}
        )

    def test_rename_twice(self):
        """
        Renaming both from and to `event` works.
        """
        assert {
            "msg": "hi",
            "event": "fabulous",
            "foo": "bar",
        } == EventRenamer("msg", "_event")(
            None, None, {"event": "hi", "foo": "bar", "_event": "fabulous"}
        )

    def test_replace_by_key_is_optional(self):
        """
        The key that is renamed to `event` doesn't have to exist.
        """
        assert {"msg": "hi", "foo": "bar"} == EventRenamer("msg", "missing")(
            None, None, {"event": "hi", "foo": "bar"}
        )

class TestByteBudget:
    """
    Tests for the `ByteBudget` processor.

    The budget is billed on the *final* encoded output, so escaping, CJK text
    and structured exceptions all count.
    """

    def _render(self, budget, event_dict, **kw):
        bb = ByteBudget(JSONRenderer(), budget_bytes=budget, **kw)

        return bb(None, "info", event_dict)

    def test_small_event_is_untouched_and_rendered_once(self):
        """
        An in-budget event is passed through verbatim and the renderer runs
        exactly once.
        """
        calls = []

        def renderer(logger, name, event_dict):
            calls.append(dict(event_dict))

            return json.dumps(event_dict)

        bb = ByteBudget(renderer, budget_bytes=1000)
        event_dict = {"event": "hi", "n": 1}
        out = bb(None, "info", event_dict)

        assert '{"event": "hi", "n": 1}' == out
        assert 1 == len(calls)
        assert {"event": "hi", "n": 1} == event_dict

    def test_fitting_event_has_no_facts(self):
        """
        No truncation facts are attached if nothing was truncated.
        """
        out = self._render(1000, {"event": "hi"})

        assert {"event": "hi"} == json.loads(out)

    def test_over_budget_event_stays_valid_and_within_budget(self):
        """
        Trimming never hands half a JSON document to the sink and never
        exceeds the budget.
        """
        out = self._render(
            200, {"event": "boom", "msg": "x" * 500, "other": "y" * 500}
        )

        assert len(out.encode()) <= 200
        assert isinstance(json.loads(out), dict)

    def test_cjk_is_billed_by_encoded_bytes(self):
        """
        Multi-byte characters are charged their UTF-8 (escaped) size.
        """
        out = self._render(400, {"event": "e", "big": "中文" * 100})

        assert len(out.encode()) <= 400
        # Three UTF-8 bytes per character -- six if \u-escaped.
        assert 600 == len(("中文" * 100).encode())

    def test_escapes_are_billed(self):
        """
        Characters that have to be escaped count by their escaped size.
        """
        out = self._render(200, {"event": "q", "x": '"\\\n' * 100})

        assert len(out.encode()) <= 200
        assert isinstance(json.loads(out), dict)

    def test_truncation_char_boundaries(self):
        """
        Truncation happens on character boundaries; no lone surrogates or
        replacement characters leak into the output.
        """
        out = self._render(200, {"event": "e", "big": "中文" * 100})
        value = json.loads(out)["big"]

        assert value == value.encode("utf-8").decode("utf-8")
        assert "�" not in value

    def test_truncation_facts_are_attached(self):
        """
        Truncated fields are reported with the number of bytes dropped, and
        the value itself says how much it lost.
        """
        out = self._render(200, {"event": "e", "big": "x" * 5000})
        data = json.loads(out)

        assert {"big"} == set(data["truncated"])
        assert 0 < data["truncated"]["big"] <= 5000
        assert f"{data['truncated']['big']}" in data["big"]

    def test_required_fields_are_never_truncated(self):
        """
        Required fields are preserved verbatim; the rest absorbs the hit.
        """
        out = self._render(
            300,
            {"event": "keep-me-intact", "a": "x" * 2000},
            required_fields=("event",),
        )
        data = json.loads(out)

        assert "keep-me-intact" == data["event"]
        assert "a" in data["truncated"]

    def test_required_field_over_budget_is_rejected(self):
        """
        If required fields alone blow the budget, an explicit error is raised
        instead of emitting an over-budget or invalid event.
        """
        with pytest.raises(EventTooLargeError) as ei:
            self._render(10, {"event": "x" * 500})

        assert "budget" in str(ei.value)

    def test_required_field_over_budget_is_a_value_error(self):
        """
        EventTooLargeError is a ValueError so broad handlers can catch it.
        """
        assert issubclass(EventTooLargeError, ValueError)

    def test_facts_are_paid_for(self):
        """
        The truncation facts themselves are charged against the budget.
        """
        out = self._render(
            250,
            {"event": "e", "a": "x" * 4000, "b": "y" * 4000},
        )
        facts = json.loads(out)["truncated"]

        assert len(out.encode()) <= 250
        assert {"a", "b"} == set(facts)

    def test_trimming_is_skipped_when_not_a_net_win(self):
        """
        A tiny field isn't trimmed if doing so would cost more bytes in
        truncation facts than it saves.  Such an event is rejected instead of
        being grown into a worse, over-budget shape.
        """
        event_dict = {"event": "e", **{f"k{i}": "ab" for i in range(40)}}

        with pytest.raises(EventTooLargeError):
            self._render(400, event_dict)

    def test_trimming_fact_overhead_is_accounted(self):
        """
        With enough room, many small fields are simply kept, not trimmed.
        """
        event_dict = {"event": "e", **{f"k{i}": "ab" for i in range(40)}}
        out = self._render(600, event_dict)
        data = json.loads(out)

        assert len(out.encode()) <= 600
        assert "truncated" not in data


        """
        Fields are trimmed in the declared order.
        """
        out = self._render(
            260,
            {"event": "e", "a": "A" * 300, "b": "B" * 300},
            trim_order=["b", "a"],
        )
        facts = json.loads(out)["truncated"]

        # "b" is first in the trim order, so it loses at least as much.
        assert facts["b"] >= facts["a"]

    def test_only_first_fields_trimmed_when_enough(self):
        """
        Later fields are left alone if earlier ones free enough room.
        """
        out = self._render(
            400,
            {"event": "e", "first": "x" * 5000, "second": "keep"},
            trim_order=["first"],
        )
        data = json.loads(out)

        assert "keep" == data["second"]
        assert "first" in data["truncated"]

    def test_large_fields_absorb_the_hit(self):
        """
        Without an explicit order, the big payload is trimmed while a small
        field is left alone -- we don't wipe tiny fields just to save a byte.
        """
        out = self._render(
            300, {"event": "e", "small": "keepme", "big": "x" * 5000}
        )
        data = json.loads(out)

        assert "keepme" == data["small"]
        assert "big" in data["truncated"]
        assert "small" not in data["truncated"]

    def test_omitted_bytes_are_exact(self):
        """
        The reported omitted-byte count plus the retained content equals the
        original payload.
        """
        original = "中文" * 100
        out = self._render(400, {"event": "e", "big": original})
        data = json.loads(out)

        marker_start = data["big"].rindex("…<truncated ")
        kept = data["big"][:marker_start]

        assert len(original.encode()) == len(kept.encode()) + (
            data["truncated"]["big"]
        )

    def test_nested_values_are_trimmed_structurally(self):
        """
        Nested containers stay valid and are shrunk field by field.
        """
        out = self._render(
            250, {"event": "e", "payload": {"deep": ["z" * 300, "k" * 300]}}
        )
        data = json.loads(out)

        assert len(out.encode()) <= 250
        assert isinstance(data["payload"]["deep"], list)

    def test_structured_exception_is_trimmed(self):
        """
        A structured exception tree counts towards the budget and is trimmed
        to a still-valid JSON value.
        """
        event_dict: EventDict = {"event": "boom", "exc_info": True}
        try:
            raise ValueError("x" * 500)
        except ValueError:
            dict_tracebacks(None, "info", event_dict)

        out = ByteBudget(JSONRenderer(), budget_bytes=300)(
            None, "info", event_dict
        )
        data = json.loads(out)

        assert len(out.encode()) <= 300
        assert "exception" in data["truncated"]

    def test_renderer_call_count_is_bounded(self):
        """
        The renderer is called at most max_passes times, even for over-budget
        events -- it's never used as a size oracle.
        """
        calls = []

        def renderer(logger, name, event_dict):
            calls.append(1)

            return json.dumps(event_dict)

        bb = ByteBudget(renderer, budget_bytes=200, max_passes=3)
        bb(None, "info", {"event": "e", "a": "x" * 5000})

        assert len(calls) <= 3

    def test_renderer_call_count_with_side_effects(self):
        """
        A side-effecting renderer that ignores trimming can't cause unbounded
        repeats: we give up after max_passes and reject.
        """
        calls = []

        def renderer(logger, name, event_dict):
            calls.append(1)

            return "x" * 1000

        bb = ByteBudget(renderer, budget_bytes=100, max_passes=2)

        with pytest.raises(EventTooLargeError):
            bb(None, "info", {"event": "e"})

        assert 2 == len(calls)

    def test_original_event_dict_and_values_are_not_mutated(self):
        """
        The caller's event dict and the containers it holds are untouched.
        """
        payload = {"deep": ["z" * 300, "k" * 300]}
        event_dict = {"event": "e", "payload": payload}
        snapshot = json.dumps(event_dict, sort_keys=True)

        self._render(200, event_dict)

        assert snapshot == json.dumps(event_dict, sort_keys=True)
        assert 300 == len(event_dict["payload"]["deep"][0])

    def test_bytes_renderer_is_supported(self):
        """
        A renderer returning bytes works and is billed by its byte length.
        """
        bb = ByteBudget(
            lambda logger, name, event_dict: json.dumps(event_dict).encode(),
            budget_bytes=100,
        )
        out = bb(None, "info", {"event": "e", "a": "x" * 500})

        assert isinstance(out, bytes)
        assert len(out) <= 100

    def test_non_str_renderer_result_raises(self):
        """
        A renderer that doesn't return str/bytes is rejected clearly.
        """
        bb = ByteBudget(lambda logger, name, event_dict: event_dict, 100)

        with pytest.raises(TypeError, match="must return str, bytes"):
            bb(None, "info", {"event": "e"})

    def test_facts_key_none_attaches_nothing(self):
        """
        facts_key=None disables the truncation facts.
        """
        out = self._render(
            200, {"event": "e", "a": "x" * 5000}, facts_key=None
        )

        assert "truncated" not in json.loads(out)

    def test_negative_budget_rejected(self):
        """
        A negative budget is a configuration error.
        """
        with pytest.raises(ValueError, match="budget_bytes"):
            ByteBudget(JSONRenderer(), budget_bytes=-1)

    def test_zero_max_passes_rejected(self):
        """
        max_passes must allow at least one renderer call.
        """
        with pytest.raises(ValueError, match="max_passes"):
            ByteBudget(JSONRenderer(), budget_bytes=100, max_passes=0)

    def test_cycle_does_not_hang(self):
        """
        Self-referential values don't loop forever in the size estimator.
        """
        event_dict: EventDict = {"event": "e", "big": "x" * 500}
        event_dict["self"] = event_dict

        def renderer(logger, name, ed):
            try:
                return json.dumps(ed)
            except ValueError:
                return '{"event": "e"}'

        bb = ByteBudget(renderer, budget_bytes=100)
        bb(None, "info", event_dict)  # must terminate


class TestByteBudgetIntegration:
    def test_end_to_end_with_bound_logger(self):
        """
        ByteBudget works as the terminal processor in a real logger chain and
        what reaches the sink is valid, in-budget JSON.
        """
        captured = []

        def renderer(logger, name, event_dict):
            out = JSONRenderer()(logger, name, event_dict)
            captured.append(out)

            return out

        structlog.configure(
            processors=[ByteBudget(renderer, budget_bytes=200)],
        )
        log = structlog.get_logger()
        out = log.info("boom", a="x" * 1000)

        assert 2 == len(captured)  # one measure, one confirmation
        assert len(captured[-1].encode()) <= 200
        assert isinstance(json.loads(captured[-1]), dict)
