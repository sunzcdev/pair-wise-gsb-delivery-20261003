# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

from __future__ import annotations

import pytest

import structlog

from structlog import DropEvent
from structlog._base import BoundLoggerBase
from structlog.processors import (
    BRANCH_DROPPED,
    Branch,
    BranchDropped,
    BranchError,
    BranchOutputs,
    JSONRenderer,
    ProcessorBrancher,
)
from structlog.testing import ReturnLogger

from ..helpers import CustomError


def make_brancher(*branches, **kw):
    return ProcessorBrancher(list(branches), **kw)


def passthrough(logger, method_name, event_dict):
    return event_dict


def tag(name):
    def _tag(logger, method_name, event_dict):
        event_dict["branch"] = name
        return event_dict

    return _tag


class TestProcessorBrancher:
    def test_returns_outputs_per_branch(self):
        """
        Each branch's final return value is keyed by its branch name.
        """
        brancher = make_brancher(
            Branch("audit", [tag("audit")]),
            Branch("diagnostics", [tag("diagnostics")]),
        )

        out = brancher(None, "info", {"event": "e"})

        assert isinstance(out, BranchOutputs)
        assert out["audit"]["branch"] == "audit"
        assert out["diagnostics"]["branch"] == "diagnostics"

    def test_common_chain_runs_once(self):
        """
        Processors before the brancher execute exactly once and their output
        is shared (by value) with every branch.
        """
        calls = []

        def common(logger, method_name, event_dict):
            calls.append(event_dict["event"])
            event_dict["common"] = 42
            return event_dict

        brancher = ProcessorBrancher(
            [
                Branch("a", [passthrough]),
                Branch("b", [passthrough]),
                Branch("c", [passthrough]),
            ]
        )

        out = brancher(None, "info", common(None, "info", {"event": "e"}))

        assert calls == ["e"]
        assert all(out[name]["common"] == 42 for name in ("a", "b", "c"))

    def test_branches_do_not_rerun_common_processors(self):
        """
        Common processors never appear inside a branch, so a counter added by
        a common processor is not incremented again per branch.
        """

        def counter(logger, method_name, event_dict):
            event_dict["n"] = event_dict.get("n", 0) + 1
            return event_dict

        ed = counter(None, "info", {"event": "e"})
        brancher = ProcessorBrancher(
            [Branch("a", [counter]), Branch("b", [counter])]
        )

        out = brancher(None, "info", ed)

        # Common counter ran once (n=1); each branch runs its own copy of the
        # counter once, but never re-runs the common one.
        assert out["a"]["n"] == 2
        assert out["b"]["n"] == 2

    def test_branch_mutation_does_not_affect_other_branch(self):
        """
        Top-level changes in one branch (adding, removing, renaming keys) are
        isolated because every branch gets its own dict copy.
        """

        def auditor(logger, method_name, event_dict):
            event_dict.pop("debug_only", None)
            event_dict["audited"] = True
            return event_dict

        def diagnostician(logger, method_name, event_dict):
            event_dict["debug_only"] = "kept"
            return event_dict

        brancher = ProcessorBrancher(
            [Branch("audit", [auditor]), Branch("diag", [diagnostician])]
        )

        out = brancher(None, "info", {"event": "e", "debug_only": "raw"})

        assert "debug_only" not in out["audit"]
        assert out["audit"]["audited"] is True
        assert out["diag"]["debug_only"] == "kept"
        assert "audited" not in out["diag"]

    def test_input_event_dict_is_not_mutated(self):
        """
        The event dict handed to the brancher is copied and never modified.
        """
        ed = {"event": "e", "keep": True}

        def popper(logger, method_name, event_dict):
            event_dict.pop("keep", None)
            return event_dict

        ProcessorBrancher([Branch("a", [popper])])(None, "info", ed)

        assert ed == {"event": "e", "keep": True}

    def test_nested_values_are_shared_shallow_copy(self):
        """
        Only a shallow copy is made: nested mutable values are shared by
        reference, exactly as documented.
        """
        shared = []

        def mutates_nested(logger, method_name, event_dict):
            event_dict["items"].append("from-a")
            return event_dict

        brancher = ProcessorBrancher(
            [Branch("a", [mutates_nested]), Branch("b", [passthrough])]
        )

        out = brancher(None, "info", {"event": "e", "items": shared})

        # The nested list is the very same object across branches.
        assert out["a"]["items"] is shared
        assert out["b"]["items"] is shared
        assert out["b"]["items"] == ["from-a"]

    def test_branch_can_deepcopy_nested_value_itself(self):
        """
        A branch that needs isolated nested values copies them itself;
        structlog does not deep-copy business objects.
        """
        import copy

        shared = [1, 2]

        def isolated(logger, method_name, event_dict):
            event_dict["items"] = copy.deepcopy(event_dict["items"])
            event_dict["items"].append(3)
            return event_dict

        brancher = ProcessorBrancher(
            [Branch("a", [isolated]), Branch("b", [passthrough])]
        )

        out = brancher(None, "info", {"event": "e", "items": shared})

        assert out["a"]["items"] == [1, 2, 3]
        assert out["b"]["items"] == [1, 2]
        assert shared == [1, 2]

    def test_drop_event_only_drops_that_branch(self):
        """
        DropEvent in one branch is recorded as BRANCH_DROPPED but does not
        interrupt the other branch.
        """

        def dropper(logger, method_name, event_dict):
            raise DropEvent

        brancher = ProcessorBrancher(
            [Branch("dropped", [dropper]), Branch("kept", [tag("kept")])]
        )

        out = brancher(None, "info", {"event": "e"})

        assert out["dropped"] is BRANCH_DROPPED
        assert isinstance(out["dropped"], BranchDropped)
        assert out["kept"]["branch"] == "kept"

    def test_all_branches_dropped_raises_drop_event(self):
        """
        If every branch drops, the brancher itself raises DropEvent so that
        nothing reaches the wrapped logger.
        """

        def dropper(logger, method_name, event_dict):
            raise DropEvent

        brancher = ProcessorBrancher(
            [Branch("a", [dropper]), Branch("b", [dropper])]
        )

        with pytest.raises(DropEvent):
            brancher(None, "info", {"event": "e"})

    def test_error_raise_aborts_whole_event(self):
        """
        on_error='raise' (default) lets a real exception propagate and stops
        subsequent branches from running.
        """
        ran = []

        def failer(logger, method_name, event_dict):
            raise CustomError("boom")

        def recorder(logger, method_name, event_dict):
            ran.append(event_dict["event"])
            return event_dict

        brancher = ProcessorBrancher(
            [
                Branch("fail", [failer]),
                Branch("after", [recorder]),
            ]
        )

        with pytest.raises(CustomError, match="boom"):
            brancher(None, "info", {"event": "e"})

        assert ran == []

    def test_error_continue_records_failure_and_keeps_going(self):
        """
        on_error='continue' records a BranchError and still runs the remaining
        branches.
        """

        def failer(logger, method_name, event_dict):
            raise CustomError("boom")

        brancher = ProcessorBrancher(
            [
                Branch("fail", [failer]),
                Branch("ok", [tag("ok")]),
            ],
            on_error="continue",
        )

        out = brancher(None, "info", {"event": "e"})

        assert isinstance(out["fail"], BranchError)
        assert out["fail"].branch == "fail"
        assert isinstance(out["fail"].exception, CustomError)
        assert str(out["fail"].exception) == "boom"
        assert out["ok"]["branch"] == "ok"

    def test_error_continue_records_sink_failure(self):
        """
        A sink raising a real exception is subject to the same on_error
        policy.
        """

        def bad_sink(method_name, output):
            raise CustomError("sink boom")

        brancher = ProcessorBrancher(
            [Branch("a", [passthrough], bad_sink)],
            on_error="continue",
        )

        out = brancher(None, "info", {"event": "e"})

        assert isinstance(out["a"], BranchError)
        assert isinstance(out["a"].exception, CustomError)

    def test_sink_receives_method_name_and_output(self):
        """
        A branch sink is called with the wrapped method name and the branch's
        final output, after its whole chain has run.
        """
        received = []
        brancher = ProcessorBrancher(
            [
                Branch(
                    "audit",
                    [tag("audit"), JSONRenderer()],
                    lambda method_name, output: received.append(
                        (method_name, output)
                    ),
                )
            ]
        )

        brancher(None, "warning", {"event": "e"})

        assert len(received) == 1
        method_name, output = received[0]
        assert method_name == "warning"
        assert isinstance(output, str)
        assert '"branch": "audit"' in output

    def test_sink_failure_raise_aborts(self):
        """
        With on_error='raise', a failing sink aborts the whole event like a
        failing processor.
        """

        def bad_sink(method_name, output):
            raise CustomError("sink boom")

        brancher = ProcessorBrancher([Branch("a", [passthrough], bad_sink)])

        with pytest.raises(CustomError):
            brancher(None, "info", {"event": "e"})

    def test_drop_event_is_not_an_error(self):
        """
        DropEvent is never turned into a BranchError, regardless of on_error.
        """

        def dropper(logger, method_name, event_dict):
            raise DropEvent

        for on_error in ("raise", "continue"):
            brancher = ProcessorBrancher(
                [Branch("a", [dropper]), Branch("b", [passthrough])],
                on_error=on_error,
            )

            out = brancher(None, "info", {"event": "e"})

            assert out["a"] is BRANCH_DROPPED
            assert out["b"] == {"event": "e"}

    def test_renderer_outputs_are_independent(self):
        """
        Each branch may end in its own renderer and return its own serialized
        form.
        """
        brancher = ProcessorBrancher(
            [
                Branch("json", [tag("json"), JSONRenderer()]),
                Branch("dict", [tag("dict")]),
            ]
        )

        out = brancher(None, "info", {"event": "e"})

        assert isinstance(out["json"], str)
        assert '"branch": "json"' in out["json"]
        assert out["dict"]["branch"] == "dict"

    def test_requires_at_least_one_branch(self):
        with pytest.raises(ValueError, match="at least one Branch"):
            ProcessorBrancher([])

    def test_rejects_duplicate_branch_names(self):
        with pytest.raises(ValueError, match="unique"):
            ProcessorBrancher(
                [
                    Branch("same", [passthrough]),
                    Branch("same", [passthrough]),
                ]
            )

    def test_rejects_bad_on_error(self):
        with pytest.raises(ValueError, match="on_error"):
            ProcessorBrancher([Branch("a", [passthrough])], on_error="nope")

    def test_generator_chains_work_across_events(self):
        """
        Processor chains passed as generators are materialized, so the
        brancher keeps working for more than one event.
        """
        brancher = ProcessorBrancher(
            [
                Branch("a", (tag("a") for _ in range(1))),
                Branch("b", (tag("b") for _ in range(1))),
            ]
        )

        first = brancher(None, "info", {"event": "1"})
        second = brancher(None, "info", {"event": "2"})

        assert first["a"]["branch"] == "a"
        assert second["b"]["branch"] == "b"

    def test_branches_are_iterated_in_declaration_order(self):
        order = []

        def ordered(name):
            def _proc(logger, method_name, event_dict):
                order.append(name)
                return event_dict

            return _proc

        brancher = ProcessorBrancher(
            [
                Branch("first", [ordered("first")]),
                Branch("second", [ordered("second")]),
                Branch("third", [ordered("third")]),
            ]
        )

        brancher(None, "info", {"event": "e"})

        assert order == ["first", "second", "third"]


class TestBoundLoggerIntegration:
    def test_works_in_bound_logger_chain(self):
        """
        The brancher plugs into the ordinary BoundLoggerBase processor chain
        unchanged; its BranchOutputs dict is delivered as **kwargs to the
        wrapped logger.
        """
        logger = ReturnLogger()
        bl = BoundLoggerBase(
            logger,
            [
                tag("common"),
                ProcessorBrancher(
                    [
                        Branch("audit", [tag("audit")]),
                        Branch("diag", [tag("diag")]),
                    ]
                ),
            ],
            {},
        )

        args, kw = bl._process_event("info", "e", {})

        assert args == ()
        assert set(kw) == {"audit", "diag"}
        assert kw["audit"]["branch"] == "audit"
        assert kw["diag"]["branch"] == "diag"

    def test_drop_event_silently_aborts_proxy(self, capsys):
        """
        When all branches drop, _proxy_to_logger returns None and never calls
        the wrapped logger -- the single-chain DropEvent contract still
        holds.
        """

        def dropper(logger, method_name, event_dict):
            raise DropEvent

        bl = BoundLoggerBase(
            ReturnLogger(),
            [ProcessorBrancher([Branch("a", [dropper])])],
            {},
        )

        assert bl._proxy_to_logger("info", "e") is None
        assert capsys.readouterr() == ("", "")

    def test_single_chain_interface_unchanged_without_brancher(self):
        """
        Loggers configured without a brancher behave exactly as before.
        """
        bl = structlog.BoundLogger(ReturnLogger(), [tag("only")], {"bound": 1})

        result = bl.info("e")

        assert result == ((), {"event": "e", "bound": 1, "branch": "only"})


class TestBranchDropped:
    def test_repr(self):
        assert repr(BRANCH_DROPPED) == "<BRANCH_DROPPED>"

    def test_singleton_pickle_roundtrip(self):
        import pickle

        assert pickle.loads(pickle.dumps(BRANCH_DROPPED)) is BRANCH_DROPPED

    def test_branch_error_is_a_tuple(self):
        err = BranchError("b", CustomError("x"))

        assert err.branch == "b"
        assert tuple(err) == ("b", err.exception)
