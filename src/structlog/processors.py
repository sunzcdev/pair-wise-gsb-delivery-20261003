# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

"""
Processors useful regardless of the logging framework.
"""

from __future__ import annotations


__lazy_modules__ = ["datetime", "logging", "threading"]

import datetime as dt
import enum
import json
import logging
import operator
import os
import sys
import threading
import time

from collections.abc import Callable, Collection, Mapping, Sequence
from types import FrameType, TracebackType
from typing import (
    Any,
    ClassVar,
    Final,
    NamedTuple,
    TextIO,
    cast,
)

from ._frames import (
    _find_first_app_frame_and_name,
    _format_exception,
    _format_stack,
)
from ._log_levels import NAME_TO_LEVEL, add_log_level
from ._utils import get_processname
from .contextvars import _ASYNC_CALLING_THREAD
from .exceptions import EventTooLargeError
from .tracebacks import ExceptionDictTransformer
from .typing import (
    EventDict,
    ExceptionTransformer,
    ExcInfo,
    WrappedLogger,
)


__all__ = [
    "NAME_TO_LEVEL",  # some people rely on it being here
    "ByteBudget",
    "CallsiteParameter",
    "CallsiteParameterAdder",
    "EventRenamer",
    "ExceptionPrettyPrinter",
    "JSONRenderer",
    "KeyValueRenderer",
    "LogfmtRenderer",
    "StackInfoRenderer",
    "TimeStamper",
    "UnicodeDecoder",
    "UnicodeEncoder",
    "add_log_level",
    "dict_tracebacks",
    "format_exc_info",
]


class KeyValueRenderer:
    """
    Render ``event_dict`` as a list of ``Key=repr(Value)`` pairs.

    Args:
        sort_keys: Whether to sort keys when formatting.

        key_order:
            List of keys that should be rendered in this exact order.  Missing
            keys will be rendered as ``None``, extra keys depending on
            *sort_keys* and the dict class.

        drop_missing:
            When ``True``, extra keys in *key_order* will be dropped rather
            than rendered as ``None``.

        repr_native_str:
            When ``True``, :func:`repr()` is also applied to native strings.

    .. versionadded:: 0.2.0 *key_order*
    .. versionadded:: 16.1.0 *drop_missing*
    .. versionadded:: 17.1.0 *repr_native_str*
    """

    def __init__(
        self,
        sort_keys: bool = False,
        key_order: Sequence[str] | None = None,
        drop_missing: bool = False,
        repr_native_str: bool = True,
    ):
        self._ordered_items = _items_sorter(sort_keys, key_order, drop_missing)

        if repr_native_str is True:
            self._repr = repr
        else:

            def _repr(inst: Any) -> str:
                if isinstance(inst, str):
                    return inst

                return repr(inst)

            self._repr = _repr

    def __call__(
        self, _: WrappedLogger, __: str, event_dict: EventDict
    ) -> str:
        return " ".join(
            k + "=" + self._repr(v) for k, v in self._ordered_items(event_dict)
        )


class LogfmtRenderer:
    """
    Render ``event_dict`` using the logfmt_ format.

    .. _logfmt: https://brandur.org/logfmt

    Args:
        sort_keys: Whether to sort keys when formatting.

        key_order:
            List of keys that should be rendered in this exact order. Missing
            keys are rendered with empty values, extra keys depending on
            *sort_keys* and the dict class.

        drop_missing:
            When ``True``, extra keys in *key_order* will be dropped rather
            than rendered with empty values.

        bool_as_flag:
            When ``True``, render ``{"flag": True}`` as ``flag``, instead of
            ``flag=true``. ``{"flag": False}`` is always rendered as
            ``flag=false``.

    Raises:
        ValueError: If a key contains non-printable or whitespace characters.

    .. versionadded:: 21.5.0
    """

    bool_as_flag: bool

    def __init__(
        self,
        sort_keys: bool = False,
        key_order: Sequence[str] | None = None,
        drop_missing: bool = False,
        bool_as_flag: bool = True,
    ):
        self._ordered_items = _items_sorter(sort_keys, key_order, drop_missing)
        self.bool_as_flag = bool_as_flag

    def __call__(
        self, _: WrappedLogger, __: str, event_dict: EventDict
    ) -> str:
        elements: list[str] = []
        for key, value in self._ordered_items(event_dict):
            if any(c <= " " for c in key):
                msg = f'Invalid key: "{key}"'
                raise ValueError(msg)

            if value is None:
                elements.append(f"{key}=")
                continue

            if isinstance(value, bool):
                if self.bool_as_flag and value:
                    elements.append(f"{key}")
                    continue
                value = "true" if value else "false"

            value = str(value)
            backslashes_need_escaping = (
                " " in value or "=" in value or '"' in value
            )
            if backslashes_need_escaping and "\\" in value:
                value = value.replace("\\", "\\\\")

            value = value.replace('"', '\\"').replace("\n", "\\n")

            if backslashes_need_escaping:
                value = f'"{value}"'

            elements.append(f"{key}={value}")

        return " ".join(elements)


def _items_sorter(
    sort_keys: bool,
    key_order: Sequence[str] | None,
    drop_missing: bool,
) -> Callable[[EventDict], list[tuple[str, object]]]:
    """
    Return a function to sort items from an ``event_dict``.

    See `KeyValueRenderer` for an explanation of the parameters.
    """
    # Use an optimized version for each case.
    if key_order and sort_keys:

        def ordered_items(event_dict: EventDict) -> list[tuple[str, Any]]:
            items = []
            for key in key_order:
                value = event_dict.pop(key, None)
                if value is not None or not drop_missing:
                    items.append((key, value))

            items += sorted(event_dict.items())

            return items

    elif key_order:

        def ordered_items(event_dict: EventDict) -> list[tuple[str, Any]]:
            items = []
            for key in key_order:
                value = event_dict.pop(key, None)
                if value is not None or not drop_missing:
                    items.append((key, value))

            items += event_dict.items()

            return items

    elif sort_keys:

        def ordered_items(event_dict: EventDict) -> list[tuple[str, Any]]:
            return sorted(event_dict.items())

    else:
        ordered_items = operator.methodcaller(  # type: ignore[assignment]
            "items"
        )

    return ordered_items


class UnicodeEncoder:
    """
    Encode unicode values in ``event_dict``.

    Args:
        encoding: Encoding to encode to (default: ``"utf-8"``).

        errors:
            How to cope with encoding errors (default ``"backslashreplace"``).

    Just put it in the processor chain before the renderer.

    .. note:: Not very useful in a Python 3-only world.
    """

    _encoding: str
    _errors: str

    def __init__(
        self, encoding: str = "utf-8", errors: str = "backslashreplace"
    ) -> None:
        self._encoding = encoding
        self._errors = errors

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> EventDict:
        for key, value in event_dict.items():
            if isinstance(value, str):
                event_dict[key] = value.encode(self._encoding, self._errors)

        return event_dict


class UnicodeDecoder:
    """
    Decode byte string values in ``event_dict``.

    Args:
        encoding: Encoding to decode from (default: ``"utf-8"``).

        errors: How to cope with encoding errors (default: ``"replace"``).

    Useful to prevent ``b"abc"`` being rendered as as ``'b"abc"'``.

    Just put it in the processor chain before the renderer.

    .. versionadded:: 15.4.0
    """

    _encoding: str
    _errors: str

    def __init__(
        self, encoding: str = "utf-8", errors: str = "replace"
    ) -> None:
        self._encoding = encoding
        self._errors = errors

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> EventDict:
        for key, value in event_dict.items():
            if isinstance(value, bytes):
                event_dict[key] = value.decode(self._encoding, self._errors)

        return event_dict


class JSONRenderer:
    """
    Render the ``event_dict`` using ``serializer(event_dict, **dumps_kw)``.

    Args:
        dumps_kw:
            Are passed unmodified to *serializer*.  If *default* is passed, it
            will disable support for ``__structlog__``-based serialization.

        serializer:
            A :func:`json.dumps`-compatible callable that will be used to
            format the string.  This can be used to use alternative JSON
            encoders (default: :func:`json.dumps`).

            .. seealso:: :doc:`performance` for examples.

    .. versionadded:: 0.2.0 Support for ``__structlog__`` serialization method.
    .. versionadded:: 15.4.0 *serializer* parameter.
    .. versionadded:: 18.2.0
       Serializer's *default* parameter can be overwritten now.
    """

    def __init__(
        self,
        serializer: Callable[..., str | bytes] = json.dumps,
        **dumps_kw: Any,
    ) -> None:
        dumps_kw.setdefault("default", _json_fallback_handler)
        self._dumps_kw = dumps_kw
        self._dumps = serializer

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> str | bytes:
        """
        The return type of this depends on the return type of self._dumps.
        """
        return self._dumps(event_dict, **self._dumps_kw)


def _json_fallback_handler(obj: Any) -> Any:
    """
    Serialize custom datatypes and pass the rest to __structlog__ & repr().
    """
    # circular imports :(
    from structlog.threadlocal import _ThreadLocalDictWrapper

    if isinstance(obj, _ThreadLocalDictWrapper):
        return obj._dict

    try:
        return obj.__structlog__()
    except AttributeError:
        return repr(obj)


_NOT_FOUND: Final = object()

_MAX_PLAN_ROUNDS: Final = 4


class ByteBudget:
    """
    Render *event_dict* so that it fits into a UTF-8 byte budget.

    Unlike most processors, this one is a *terminal* processor: it wraps the
    renderer that produces the final output and returns its result.  That is
    the only way to charge for the bytes that actually reach the log sink --
    including JSON escaping (``"`` becomes ``\\"``), non-ASCII text (a single
    CJK character is three UTF-8 bytes, six if it is ASCII-escaped), and
    structured exception trees.

    Small events that already fit are passed through untouched: the wrapped
    *renderer* is called exactly once and its result is returned as-is.

    If an event is over budget, values are shrunk *structurally* -- never by
    slicing the rendered string -- and the event is rendered again.  The log
    sink therefore never sees half a JSON document or half a UTF-8 sequence:
    strings are cut on character boundaries and their container is
    re-serialized.  Fields are shrunk in *trim_order* (default: the order they
    appear in *event_dict*) and only as much as necessary; *required_fields*
    are never touched.

    Every truncation is recorded under *facts_key* as a compact mapping of
    field name to the number of omitted bytes.  The in-string *marker* repeats
    that number inline, so the fact survives even if the facts field itself
    has to give way.

    The *renderer* is called a hard-bounded number of times -- at most
    *max_passes* -- and is **never** used as a binary-search oracle:
    renderers may have side effects, so probing them over and over to discover
    a size would run them for an unpredictable number of times.

    Args:
        renderer:
            A processor returning the final ``str`` or ``bytes``, for example
            `JSONRenderer`.  It is called as
            ``renderer(logger, method_name, event_dict)``.

        budget_bytes:
            Maximum number of bytes (after final encoding) per event.

        required_fields:
            Keys that must never be truncated.  They're charged against the
            budget; if they -- together with the truncation facts -- can't fit,
            `structlog.exceptions.EventTooLargeError` is raised instead of
            emitting an over-budget or invalid event.

        trim_order:
            Order in which fields are considered for truncation.  Keys that are
            missing or listed in *required_fields* are skipped.  Keys of
            *event_dict* not mentioned here are considered afterwards, largest
            first, so a big payload absorbs the hit before a tiny field is
            touched.

        facts_key:
            Key under which truncation facts are attached.  Any pre-existing
            value under that key is replaced.  If ``None``, no facts are
            attached.

        marker:
            Template for the in-string marker that replaces omitted text.
            ``{omitted}`` is replaced with the number of omitted bytes.

        container_marker:
            Template for leaf values that can't be shrunk structurally (e.g.
            ``bytes``).  ``{omitted}`` and ``{type}`` are replaced with the
            number of omitted bytes and the value's type name.

        max_passes:
            Upper bound on the number of times *renderer* is called.

    Raises:
        structlog.exceptions.EventTooLargeError:
            If the event can't be brought within *budget_bytes* without
            touching *required_fields*.

        TypeError:
            If *renderer* returns something other than ``str`` or
            ``bytes``-like.

    .. versionadded:: 26.2.0
    """

    __slots__ = (
        "_budget",
        "_container_marker",
        "_facts_key",
        "_marker",
        "_max_passes",
        "_renderer",
        "_required",
        "_trim_order",
    )

    def __init__(
        self,
        renderer: Callable[[WrappedLogger, str, EventDict], str | bytes],
        budget_bytes: int,
        required_fields: Collection[str] = ("event",),
        trim_order: Sequence[str] | None = None,
        facts_key: str | None = "truncated",
        marker: str = "…<truncated {omitted} bytes>…",
        container_marker: str = "<truncated {omitted}-byte {type}>",
        max_passes: int = 3,
    ) -> None:
        if budget_bytes < 0:
            msg = "budget_bytes must not be negative."
            raise ValueError(msg)
        if max_passes < 1:
            msg = "max_passes must be at least 1."
            raise ValueError(msg)

        self._renderer = renderer
        self._budget = budget_bytes
        self._required = frozenset(required_fields)
        self._trim_order = tuple(trim_order) if trim_order is not None else ()
        self._facts_key = facts_key
        self._marker = marker
        self._container_marker = container_marker
        self._max_passes = max_passes

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> str | bytes:
        calls = 1
        rendered = self._render(logger, name, event_dict)
        size = _encoded_size(rendered)

        if size <= self._budget:
            return rendered

        # Over budget: work on a copy so the caller's dict -- and the values
        # inside it -- stay untouched.
        work: EventDict = dict(event_dict)
        # Untouched originals, so repeated passes shrink from the source
        # instead of stacking markers.
        originals: dict[str, Any] = {}
        # key -> cumulative estimated bytes to remove from its original.
        reductions: dict[str, int] = {}
        # key -> cumulative omitted payload bytes
        trimmed_values: dict[str, int] = {}

        # Plan against our own estimate -- it's cheap and doesn't touch the
        # renderer -- then confirm by rendering.  If the confirmed size still
        # misses, add the measured deficit on top and re-plan, all within the
        # hard *max_passes* renderer-call bound.
        deficit = 0
        while calls < self._max_passes:
            self._plan(work, originals, reductions, trimmed_values, deficit)

            rendered = self._render(logger, name, work)
            calls += 1
            size = _encoded_size(rendered)

            if size <= self._budget:
                break

            deficit += size - self._budget

        if size > self._budget:
            msg = (
                f"Event is {size} bytes but the budget is "
                f"{self._budget} bytes and it can't be reduced further "
                f"without truncating required fields "
                f"({', '.join(sorted(self._required)) or 'none'})."
            )
            raise EventTooLargeError(msg)

        return rendered

    def _render(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> str | bytes:
        rendered = self._renderer(logger, name, event_dict)
        _encoded_size(rendered)  # validate the type early

        return rendered

    def _plan(
        self,
        work: EventDict,
        originals: dict[str, Any],
        reductions: dict[str, int],
        trimmed_values: dict[str, int],
        deficit: int,
    ) -> None:
        """
        Trim *work* (recording *trimmed_values*) until its estimate fits.

        *deficit* is added on top of the estimate's own overshoot, which lets
        the caller compensate for a renderer that produces slightly more bytes
        than the estimate predicted.  The truncation facts are kept in *work*
        while planning, so they're paid for too.
        """
        for _ in range(_MAX_PLAN_ROUNDS):
            if self._facts_key is not None and trimmed_values:
                work[self._facts_key] = _facts(trimmed_values)

            excess = _estimate_bytes(work) - self._budget + deficit
            if excess <= 0:
                return

            if self._trim_pass(
                work, originals, reductions, trimmed_values, excess
            ):
                continue

            # Structural trimming can't free anything anymore (all leaf
            # strings are at their floor, the rest is structural keys).  Fold
            # the largest remaining field into a single marker to close the
            # gap, and let the next round re-check.
            if not self._fold_pass(work, trimmed_values):
                return

        if self._facts_key is not None and trimmed_values:
            work[self._facts_key] = _facts(trimmed_values)

    def _fold_pass(
        self, work: EventDict, trimmed_values: dict[str, int]
    ) -> bool:
        """
        Replace the single largest non-required field with a fold marker.

        Used only when structural trimming has stalled.  Returns whether
        anything was folded.
        """
        best_key: str | None = None
        best_size = 0

        for key in self._candidate_keys(work):
            size = _estimate_bytes(work[key])
            if size > best_size:
                best_key, best_size = key, size

        if best_key is None:
            return False

        payload = _payload_bytes(work[best_key])
        marker = self._container_marker.format(
            omitted=payload, type=type(work[best_key]).__name__
        )

        if _estimate_bytes(marker) >= best_size:
            return False

        work[best_key] = marker
        _record(trimmed_values, best_key, payload)

        return True

    def _trim_pass(
        self,
        work: EventDict,
        originals: dict[str, Any],
        reductions: dict[str, int],
        trimmed_values: dict[str, int],
        excess: int,
    ) -> bool:
        """
        Shrink non-required fields in declaration order until *excess* bytes
        (by estimate) are gone.

        Each field is always shrunk from its *original* value -- tracked in
        *originals* -- with the *cumulative* reduction tracked in
        *reductions*, so successive passes never undo or double-apply work.

        Returns whether anything was actually changed.
        """
        trimmed = False
        for key in self._candidate_keys(work):
            if excess <= 0:
                break

            if key not in originals:
                if key not in work:
                    continue
                originals[key] = work[key]

            original_value = originals[key]
            original = _estimate_bytes(original_value)

            wanted = reductions.get(key, 0) + excess
            new_value, kept, omitted = self._shrink(original_value, wanted)

            removed = original - kept
            if removed <= reductions.get(key, 0):
                continue

            # Never make a field bigger than it currently is.  This matters
            # after `_fold_pass` collapsed it: re-deriving from the untouched
            # original could otherwise grow the folded value again.
            if _estimate_bytes(new_value) >= _estimate_bytes(work[key]):
                continue

            # Only trim if it's a net win once the field's entry in the facts
            # mapping is paid for.  Otherwise we'd shrink something by 2 bytes
            # and grow the facts by 12.
            if key not in trimmed_values:
                fact_cost = len(key) + 8  # "key": 123,
                if removed <= fact_cost:
                    continue

            work[key] = new_value
            excess -= removed - reductions.get(key, 0)
            reductions[key] = removed
            _record(trimmed_values, key, omitted)
            trimmed = True

        return trimmed

    def _candidate_keys(self, event_dict: EventDict) -> list[str]:
        """
        Return the keys eligible for trimming, in priority order.

        Keys named in *trim_order* come first, in the order declared there.
        The remaining keys follow, largest first (insertion order breaks ties).
        "Largest first" is what keeps a big payload from surviving while a
        tiny, unrelated field gets wiped: the big field absorbs the hit.
        """
        candidates = [
            key
            for key in self._trim_order
            if key in event_dict
            and key not in self._required
            and key != self._facts_key
        ]

        rest = [
            key
            for key in event_dict
            if key not in candidates
            and key not in self._required
            and key != self._facts_key
        ]
        rest.sort(key=lambda key: -_estimate_bytes(event_dict[key]))

        return candidates + rest

    def _shrink(
        self, value: Any, excess: int
    ) -> tuple[Any, int, int]:
        """
        Shrink *value* by roughly *excess* bytes, structurally.

        Recurses into containers (never touching keys), cuts strings on
        character boundaries, and drops trailing sequence items once children
        are at their floor.  If that isn't enough, the caller's `_fold_pass`
        falls back to a coarse fold.

        Returns ``(new_value, estimated_size, omitted_content_bytes)``.  The
        last item is measured in the original value's UTF-8 payload bytes and
        matches the number the inline markers report.  The original value --
        including nested containers -- is never mutated.
        """
        return self._shrink_inner(value, excess, set())

    def _shrink_inner(
        self, value: Any, excess: int, seen: set[int]
    ) -> tuple[Any, int, int]:
        original = _estimate_bytes(value)

        if isinstance(value, str):
            new, omitted = _truncate_str(value, excess, self._marker)

            return new, min(_estimate_bytes(new), original), omitted

        identity = id(value)
        if identity in seen:
            # Circular reference: leave it to the renderer.
            return value, original, 0

        if isinstance(value, Mapping):
            seen.add(identity)
            try:
                new_dict = dict(value)
                remaining = excess
                omitted = 0
                for key in list(new_dict):
                    if remaining <= 0:
                        break
                    old_item = new_dict[key]
                    new_item, item_size, item_omitted = self._shrink_inner(
                        old_item, remaining, seen
                    )
                    reduction = _estimate_bytes(old_item) - item_size
                    if reduction > 0:
                        new_dict[key] = new_item
                        remaining -= reduction
                        omitted += item_omitted

                return (
                    new_dict,
                    min(_estimate_bytes(new_dict), original),
                    omitted,
                )
            finally:
                seen.discard(identity)

        if isinstance(value, (list, tuple)):
            seen.add(identity)
            try:
                remaining = excess
                omitted = 0
                items = []
                for item in value:
                    if remaining <= 0:
                        items.append(item)
                        continue

                    new_item, item_size, item_omitted = self._shrink_inner(
                        item, remaining, seen
                    )
                    reduction = _estimate_bytes(item) - item_size
                    if reduction > 0:
                        remaining -= reduction
                        omitted += item_omitted
                    items.append(new_item)

                # The children are at their floor but we still need room:
                # drop trailing items -- the coarse equivalent of max_frames.
                while remaining > 0 and items:
                    dropped = items.pop()
                    remaining -= _estimate_bytes(dropped)
                    omitted += _payload_bytes(dropped)

                new_sequence: Any = (
                    tuple(items) if isinstance(value, tuple) else items
                )

                return (
                    new_sequence,
                    min(_estimate_bytes(new_sequence), original),
                    omitted,
                )
            finally:
                seen.discard(identity)

        # Opaque leaf (bytes etc.): replace with a marker -- but only if that
        # actually makes the value smaller.
        payload = _payload_bytes(value)
        marker = self._container_marker.format(
            omitted=payload, type=type(value).__name__
        )
        marker_size = _estimate_bytes(marker)

        if marker_size < original:
            return marker, marker_size, payload

        return value, original, 0


def _record(
    trimmed_values: dict[str, int], key: str, omitted: int
) -> None:
    """
    Accumulate how many payload bytes were dropped from *key*.
    """
    trimmed_values[key] = max(trimmed_values.get(key, 0), omitted)


def _facts(trimmed_values: dict[str, int]) -> dict[str, int]:
    """
    Turn the internal bookkeeping into the public truncation facts.

    The shape is deliberately compact -- ``{field: omitted_bytes}`` -- because
    the facts are charged against the very budget they document.

    The number matches the one the field's inline marker shows, so a consumer
    can trust either one.
    """
    return dict(trimmed_values)


def _encoded_size(rendered: Any) -> int:
    """
    Return the number of bytes *rendered* occupies once encoded.

    This is the authoritative billing function: it encodes with UTF-8, so
    multi-byte characters and escaped sequences are charged their real cost.

    Raises:
        TypeError: If *rendered* is neither ``str`` nor ``bytes``-like.
    """
    if isinstance(rendered, str):
        return len(rendered.encode("utf-8"))
    if isinstance(rendered, (bytes, bytearray)):
        return len(rendered)

    msg = (
        "ByteBudget's renderer must return str, bytes, or bytearray -- got "
        f"{type(rendered).__name__}."
    )
    raise TypeError(msg)


def _escaped_len(value: str) -> int:
    """
    Upper bound on the length of *value* once JSON-escaped and UTF-8 encoded.

    Non-ASCII characters count as six bytes (``\\uXXXX``), which is at least
    their UTF-8 length, so this never underestimates a JSON renderer's output.
    """
    return len(json.dumps(value, ensure_ascii=True).encode("utf-8")) - 2


def _estimate_bytes(value: Any, seen: set[int] | None = None) -> int:
    """
    Estimate how many bytes *value* occupies once rendered and encoded.

    The estimate is renderer-agnostic and deliberately errs on the high side
    for non-ASCII text (it assumes ``\\uXXXX`` escaping).  Shrinking a value
    reduces this estimate by at least the requested amount -- see
    :func:`_truncate_str` -- which is what makes a single trimming pass
    usually sufficient.  It never slices or truncates anything and guards
    against reference cycles.
    """
    if seen is None:
        seen = set()

    if isinstance(value, str):
        return _escaped_len(value) + 2  # + surrounding quotes

    if isinstance(value, (bytes, bytearray)):
        # The fallback handler uses repr(); be generous.
        return len(value) * 4 + 4

    if value is None or isinstance(value, (bool, int, float)):
        return len(json.dumps(value))

    identity = id(value)
    if identity in seen:
        # Circular reference.  Renderers either fail or emit a short marker.
        return 2

    if isinstance(value, Mapping):
        seen.add(identity)
        try:
            total = 2  # braces
            for key, item in value.items():
                total += (
                    _estimate_bytes(key, seen)
                    + 2  # colon + space
                    + _estimate_bytes(item, seen)
                    + 2  # comma + space
                )
            return total
        finally:
            seen.discard(identity)

    if isinstance(value, (list, tuple, set, frozenset)):
        seen.add(identity)
        try:
            total = 2 + 2 * max(0, len(value) - 1)  # brackets + ", "
            for item in value:
                total += _estimate_bytes(item, seen)
            return total
        finally:
            seen.discard(identity)

    try:
        return _escaped_len(str(value))
    except Exception:  # noqa: BLE001
        return 0


def _payload_bytes(value: Any, seen: set[int] | None = None) -> int:
    """
    Return the UTF-8 payload bytes of *value*, ignoring structural overhead.

    Unlike :func:`_estimate_bytes`, this is *exact* for strings and bytes --
    the two leaf types that make up the bulk of real log payloads.  It's used
    for the reported truncation facts, so the number in a field's marker and
    in the facts mapping add up to the original payload.
    """
    if seen is None:
        seen = set()

    if isinstance(value, str):
        return _raw_len(value)

    if isinstance(value, (bytes, bytearray)):
        return len(value)

    if value is None or isinstance(value, (bool, int, float)):
        return len(_safe_str(value).encode("utf-8", "backslashreplace"))

    identity = id(value)
    if identity in seen:
        return 0

    if isinstance(value, Mapping):
        seen.add(identity)
        try:
            return sum(
                _payload_bytes(k, seen) + _payload_bytes(v, seen)
                for k, v in value.items()
            )
        finally:
            seen.discard(identity)

    if isinstance(value, (list, tuple, set, frozenset)):
        seen.add(identity)
        try:
            return sum(_payload_bytes(item, seen) for item in value)
        finally:
            seen.discard(identity)

    return len(_safe_str(value).encode("utf-8", "backslashreplace"))


def _raw_len(value: str) -> int:
    """
    Return the UTF-8 length of *value*, tolerating lone surrogates.
    """
    try:
        return len(value.encode("utf-8"))
    except UnicodeEncodeError:
        return len(value.encode("utf-8", "backslashreplace"))

def _safe_str(value: Any) -> str:
    try:
        return str(value)
    except Exception:  # noqa: BLE001
        return f"<{type(value).__name__}>"


def _cut_chars(value: str, keep: int) -> str:
    """
    Keep *keep* bytes worth of *value*: half from the front, half from the
    back.

    *keep* is a UTF-8 byte budget and is consumed on character boundaries only,
    so the result is always a valid sequence of code points.
    """
    if keep <= 0:
        return ""
    if len(value) <= keep:  # fast path; ASCII-ish short strings
        return value

    encoded = value.encode("utf-8", "surrogatepass")
    if len(encoded) <= keep:
        return value

    head_size = keep // 2
    tail_size = keep - head_size

    head = encoded[:head_size].decode("utf-8", "ignore")
    tail = encoded[len(encoded) - tail_size :].decode("utf-8", "ignore")

    return head + tail


def _fit_str(value: str, target: int, marker: str) -> tuple[str, int]:
    """
    Return the "largest" variant of *value* whose estimate is at most *target*.

    The variant keeps a prefix and a suffix of *value* and appends *marker*.
    The cut is on UTF-8 character boundaries.

    Returns ``(variant, omitted_utf8_bytes)`` where *omitted_utf8_bytes* is the
    payload byte count removed -- exactly the number rendered into the marker.
    """
    if _estimate_bytes(value) <= target:
        return value, 0

    if target <= 2:  # two quotes
        return "", _raw_len(value)

    original_bytes = _raw_len(value)

    low, high = 0, original_bytes
    best: tuple[str, int] = ("", original_bytes)
    found = False

    while low <= high:
        mid = (low + high) // 2
        kept = _cut_chars(value, mid)
        omitted = original_bytes - _raw_len(kept)
        candidate = kept + marker.format(omitted=omitted)

        if _estimate_bytes(candidate) <= target:
            best = (candidate, omitted)
            found = True
            low = mid + 1
        else:
            high = mid - 1

    if found:
        return best

    # Not even the marker plus one character fits: try the bare marker, then
    # a plain prefix/suffix -- whichever fits -- so the result stays valid.
    rendered_marker = marker.format(omitted=original_bytes)
    if _estimate_bytes(rendered_marker) <= target:
        return rendered_marker, original_bytes

    low, high = 0, original_bytes
    bare: tuple[str, int] = ("", original_bytes)

    while low <= high:
        mid = (low + high) // 2
        kept = _cut_chars(value, mid)
        omitted = original_bytes - _raw_len(kept)
        candidate = kept

        if _estimate_bytes(candidate) <= target:
            bare = (candidate, omitted)
            low = mid + 1
        else:
            high = mid - 1

    return bare


def _truncate_str(value: str, excess: int, marker: str) -> tuple[str, int]:
    """
    Shrink *value* so its :func:`_estimate_bytes` drops by at least *excess*.

    Returns ``(new_value, omitted_utf8_bytes)``, where the byte count is the
    one rendered into the marker.  Returns the value unchanged (and zero
    omitted) if it can't usefully shrink -- for instance when the marker would
    be larger than the value itself.  The cut is on character boundaries.
    """
    original = _estimate_bytes(value)
    if original <= 2:
        return value, 0

    new_value, omitted = _fit_str(value, original - excess, marker)

    if omitted <= 0 or _estimate_bytes(new_value) >= original:
        return value, 0

    return new_value, omitted


class ExceptionRenderer:
    """
    Replace an ``exc_info`` field with an ``exception`` field which is rendered
    by *exception_formatter*.

    The contents of the ``exception`` field depends on the return value of the
    *exception_formatter* that is passed:

    - The default produces a formatted string via Python's built-in traceback
      formatting (this is :obj:`.format_exc_info`).
    - If you pass a :class:`~structlog.tracebacks.ExceptionDictTransformer`, it
      becomes a list of stack dicts that can be serialized to JSON.

    If *event_dict* contains the key ``exc_info``, there are three possible
    behaviors:

    1. If the value is a tuple, render it into the key ``exception``.
    2. If the value is an Exception render it into the key ``exception``.
    3. If the value true but no tuple, obtain exc_info ourselves and render
       that.

    If there is no ``exc_info`` key, the *event_dict* is not touched. This
    behavior is analog to the one of the stdlib's logging.

    Args:
        exception_formatter:
            A callable that is used to format the exception from the
            ``exc_info`` field into the ``exception`` field.

    .. seealso::
        :doc:`exceptions` for a broader explanation of *structlog*'s exception
        features.

    .. versionadded:: 22.1.0
    """

    format_exception: ExceptionTransformer

    def __init__(
        self,
        exception_formatter: ExceptionTransformer = _format_exception,
    ) -> None:
        self.format_exception = exception_formatter

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> EventDict:
        exc_info = _figure_out_exc_info(event_dict.pop("exc_info", None))
        if exc_info:
            event_dict["exception"] = self.format_exception(exc_info)

        return event_dict


format_exc_info: Final = ExceptionRenderer()
"""
Replace an ``exc_info`` field with an ``exception`` string field using Python's
built-in traceback formatting.

If *event_dict* contains the key ``exc_info``, there are three possible
behaviors:

1. If the value is a tuple, render it into the key ``exception``.
2. If the value is an Exception render it into the key ``exception``.
3. If the value is true but no tuple, obtain exc_info ourselves and render
   that.

If there is no ``exc_info`` key, the *event_dict* is not touched. This behavior
is analog to the one of the stdlib's logging.

.. seealso::
    :doc:`exceptions` for a broader explanation of *structlog*'s exception
    features.
"""

dict_tracebacks: Final = ExceptionRenderer(ExceptionDictTransformer())
"""
Replace an ``exc_info`` field with an ``exception`` field containing structured
tracebacks suitable for, e.g., JSON output.

It is a shortcut for :class:`ExceptionRenderer` with a
:class:`~structlog.tracebacks.ExceptionDictTransformer`.

The treatment of the ``exc_info`` key is identical to `format_exc_info`.

.. versionadded:: 22.1.0

.. seealso::
    :doc:`exceptions` for a broader explanation of *structlog*'s exception
    features.
"""


class TimeStamper:
    """
    Add a timestamp to ``event_dict``.

    Args:
        fmt:
            strftime format string, or ``"iso"`` for `ISO 8601
            <https://en.wikipedia.org/wiki/ISO_8601>`_, or `None` for a `UNIX
            timestamp <https://en.wikipedia.org/wiki/Unix_time>`_.

        utc: Whether timestamp should be in UTC or local time.

        key: Target key in *event_dict* for added timestamps.

    .. versionchanged:: 19.2.0 Can be pickled now.
    """

    __slots__ = ("_stamper", "fmt", "key", "utc")

    fmt: str | None
    utc: bool
    key: str

    def __init__(
        self,
        fmt: str | None = None,
        utc: bool = True,
        key: str = "timestamp",
    ) -> None:
        self.fmt, self.utc, self.key = fmt, utc, key

        self._stamper = _make_stamper(fmt, utc, key)

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> EventDict:
        return self._stamper(event_dict)

    def __getstate__(self) -> dict[str, Any]:
        return {"fmt": self.fmt, "utc": self.utc, "key": self.key}

    def __setstate__(self, state: dict[str, Any]) -> None:
        self.fmt = state["fmt"]
        self.utc = state["utc"]
        self.key = state["key"]

        self._stamper = _make_stamper(**state)


def _make_stamper(
    fmt: str | None, utc: bool, key: str
) -> Callable[[EventDict], EventDict]:
    """
    Create a stamper function.
    """
    if fmt is None and not utc:
        msg = "UNIX timestamps are always UTC."
        raise ValueError(msg)

    now: Callable[[], dt.datetime]

    if utc:

        def now() -> dt.datetime:
            return dt.datetime.now(tz=dt.timezone.utc)

    else:

        def now() -> dt.datetime:
            # We don't need the TZ for our own formatting. We add it only for
            # user-defined formats later.
            return dt.datetime.now()  # noqa: DTZ005

    if fmt is None:

        def stamper_unix(event_dict: EventDict) -> EventDict:
            event_dict[key] = time.time()

            return event_dict

        return stamper_unix

    if fmt.upper() == "ISO":

        def stamper_iso_local(event_dict: EventDict) -> EventDict:
            event_dict[key] = now().isoformat()
            return event_dict

        def stamper_iso_utc(event_dict: EventDict) -> EventDict:
            event_dict[key] = now().isoformat().replace("+00:00", "Z")
            return event_dict

        if utc:
            return stamper_iso_utc

        return stamper_iso_local

    def stamper_fmt_local(event_dict: EventDict) -> EventDict:
        event_dict[key] = now().astimezone().strftime(fmt)
        return event_dict

    def stamper_fmt_utc(event_dict: EventDict) -> EventDict:
        event_dict[key] = now().strftime(fmt)
        return event_dict

    if utc:
        return stamper_fmt_utc

    return stamper_fmt_local


class MaybeTimeStamper:
    """
    A timestamper that only adds a timestamp if there is none.

    This allows you to overwrite the ``timestamp`` key in the event dict for
    example when the event is coming from another system.

    It takes the same arguments as `TimeStamper`.

    .. versionadded:: 23.2.0
    """

    __slots__ = ("stamper",)

    stamper: TimeStamper

    def __init__(
        self,
        fmt: str | None = None,
        utc: bool = True,
        key: str = "timestamp",
    ):
        self.stamper = TimeStamper(fmt=fmt, utc=utc, key=key)

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> EventDict:
        if self.stamper.key not in event_dict:
            return self.stamper(logger, name, event_dict)

        return event_dict


def _figure_out_exc_info(v: Any) -> ExcInfo | None:
    """
    Try to convert *v* into an ``exc_info`` tuple.

    Return ``None`` if *v* does not represent an exception or if there is no
    current exception.
    """
    if isinstance(v, BaseException):
        return (v.__class__, v, v.__traceback__)

    if isinstance(v, tuple) and len(v) == 3:
        has_type = isinstance(v[0], type) and issubclass(v[0], BaseException)
        has_exc = isinstance(v[1], BaseException)
        has_tb = v[2] is None or isinstance(v[2], TracebackType)
        if has_type and has_exc and has_tb:
            return v

    if v:
        result = sys.exc_info()
        if result == (None, None, None):
            return None
        return cast(ExcInfo, result)

    return None


class ExceptionPrettyPrinter:
    """
    Pretty print exceptions rendered by *exception_formatter* and remove them
    from the ``event_dict``.

    Args:
        file: Target file for output (default: ``sys.stdout``).
        exception_formatter:
            A callable that is used to format the exception from the
            ``exc_info`` field into the ``exception`` field.

    This processor is mostly for development and testing so you can read
    exceptions properly formatted.

    It behaves like `format_exc_info`, except that it removes the exception data
    from the event dictionary after printing it using the passed
    *exception_formatter*, which defaults to Python's built-in traceback formatting.

    It's tolerant to having `format_exc_info` in front of itself in the
    processor chain but doesn't require it.  In other words, it handles both
    ``exception`` as well as ``exc_info`` keys.

    .. versionadded:: 0.4.0

    .. versionchanged:: 16.0.0
       Added support for passing exceptions as ``exc_info`` on Python 3.

    .. versionchanged:: 25.4.0
       Fixed *exception_formatter* so that it overrides the default if set.
    """

    format_exception: ExceptionTransformer

    def __init__(
        self,
        file: TextIO | None = None,
        exception_formatter: ExceptionTransformer = _format_exception,
    ) -> None:
        self.format_exception = exception_formatter
        if file is not None:
            self._file = file
        else:
            self._file = sys.stdout

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> EventDict:
        exc = event_dict.pop("exception", None)
        if exc is None:
            exc_info = _figure_out_exc_info(event_dict.pop("exc_info", None))
            if exc_info:
                exc = self.format_exception(exc_info)

        if exc:
            print(exc, file=self._file)

        return event_dict


class StackInfoRenderer:
    """
    Add stack information with key ``stack`` if ``stack_info`` is `True`.

    Useful when you want to attach a stack dump to a log entry without
    involving an exception and works analogously to the *stack_info* argument
    of the Python standard library logging.

    Args:
        additional_ignores:
            By default, stack frames coming from *structlog* are ignored. With
            this argument you can add additional names that are ignored, before
            the stack starts being rendered. They are matched using
            ``startswith()``, so they don't have to match exactly. The names
            are used to find the first relevant name, therefore once a frame is
            found that doesn't start with *structlog* or one of
            *additional_ignores*, **no filtering** is applied to subsequent
            frames.

    .. versionadded:: 0.4.0
    .. versionadded:: 22.1.0  *additional_ignores*
    """

    __slots__ = ("_additional_ignores",)

    def __init__(self, additional_ignores: list[str] | None = None) -> None:
        self._additional_ignores = additional_ignores

    def __call__(
        self, logger: WrappedLogger, name: str, event_dict: EventDict
    ) -> EventDict:
        if event_dict.pop("stack_info", None):
            event_dict["stack"] = _format_stack(
                _find_first_app_frame_and_name(self._additional_ignores)[0]
            )

        return event_dict


class CallsiteParameter(enum.Enum):
    """
    Callsite parameters that can be added to an event dictionary with the
    `structlog.processors.CallsiteParameterAdder` processor class.

    The string values of the members of this enum will be used as the keys for
    the callsite parameters in the event dictionary.

    .. versionadded:: 21.5.0

    .. versionadded:: 25.5.0
       `QUAL_NAME` parameter.

    .. versionadded:: 26.1.0
       `QUAL_MODULE` parameter.
    """

    #: The full path to the python source file of the callsite.
    PATHNAME = "pathname"
    #: The basename part of the full path to the python source file of the
    #: callsite.
    FILENAME = "filename"
    #: The python module the callsite was in. This mimics the module attribute
    #: of `logging.LogRecord` objects and will be the basename, without
    #: extension, of the full path to the python source file of the callsite.
    MODULE = "module"
    #: The fully qualified import name of the module of the callsite.
    QUAL_MODULE = "qual_module"
    #: The name of the function that the callsite was in.
    FUNC_NAME = "func_name"
    #: The qualified name of the callsite (includes scope and class names).
    #: Requires Python 3.11+.
    QUAL_NAME = "qual_name"
    #: The line number of the callsite.
    LINENO = "lineno"
    #: The ID of the thread the callsite was executed in.
    THREAD = "thread"
    #: The name of the thread the callsite was executed in.
    THREAD_NAME = "thread_name"
    #: The ID of the process the callsite was executed in.
    PROCESS = "process"
    #: The name of the process the callsite was executed in.
    PROCESS_NAME = "process_name"


def _get_callsite_pathname(module: str, frame: FrameType) -> Any:
    return frame.f_code.co_filename


def _get_callsite_filename(module: str, frame: FrameType) -> Any:
    return os.path.basename(frame.f_code.co_filename)


def _get_callsite_module(module: str, frame: FrameType) -> Any:
    return os.path.splitext(os.path.basename(frame.f_code.co_filename))[0]


def _get_callsite_func_name(module: str, frame: FrameType) -> Any:
    return frame.f_code.co_name


def _get_callsite_qual_name(module: str, frame: FrameType) -> Any:
    return frame.f_code.co_qualname  # will crash on Python <3.11


def _get_callsite_qual_module(module: str, frame: FrameType) -> Any:
    return module


def _get_callsite_lineno(module: str, frame: FrameType) -> Any:
    return frame.f_lineno


def _get_callsite_thread(module: str, frame: FrameType) -> Any:
    thread_info = _ASYNC_CALLING_THREAD.get(None)
    if thread_info is None:
        return threading.get_ident()

    return thread_info[0]


def _get_callsite_thread_name(module: str, frame: FrameType) -> Any:
    thread_info = _ASYNC_CALLING_THREAD.get(None)
    if thread_info is None:
        return threading.current_thread().name

    return thread_info[1]


def _get_callsite_process(module: str, frame: FrameType) -> Any:
    return os.getpid()


def _get_callsite_process_name(module: str, frame: FrameType) -> Any:
    return get_processname()


class CallsiteParameterAdder:
    """
    Adds parameters of the callsite that an event dictionary originated from to
    the event dictionary. This processor can be used to enrich events
    dictionaries with information such as the function name, line number and
    filename that an event dictionary originated from.

    If the event dictionary has an embedded `logging.LogRecord` object and did
    not originate from *structlog* then the callsite information will be
    determined from the `logging.LogRecord` object. For event dictionaries
    without an embedded `logging.LogRecord` object the callsite will be
    determined from the stack trace, ignoring all intra-structlog calls, calls
    from the `logging` module, and stack frames from modules with names that
    start with values in ``additional_ignores``, if it is specified.

    The keys used for callsite parameters in the event dictionary are the
    string values of `CallsiteParameter` enum members.

    Args:
        parameters:
            A collection of `CallsiteParameter` values that should be added to
            the event dictionary.

        additional_ignores:
            Additional names with which a stack frame's module name must not
            start for it to be considered when determening the callsite.

    .. note::

        When used with `structlog.stdlib.ProcessorFormatter` the most efficient
        configuration is to either use this processor in ``foreign_pre_chain``
        of `structlog.stdlib.ProcessorFormatter` and in ``processors`` of
        `structlog.configure`, or to use it in ``processors`` of
        `structlog.stdlib.ProcessorFormatter` without using it in
        ``processors`` of `structlog.configure` and ``foreign_pre_chain`` of
        `structlog.stdlib.ProcessorFormatter`.

    .. versionadded:: 21.5.0
    """

    _handlers: ClassVar[
        dict[CallsiteParameter, Callable[[str, FrameType], Any]]
    ] = {
        # We can't use lambda functions here because they are not pickleable.
        CallsiteParameter.PATHNAME: _get_callsite_pathname,
        CallsiteParameter.FILENAME: _get_callsite_filename,
        CallsiteParameter.MODULE: _get_callsite_module,
        CallsiteParameter.QUAL_MODULE: _get_callsite_qual_module,
        CallsiteParameter.FUNC_NAME: _get_callsite_func_name,
        CallsiteParameter.QUAL_NAME: _get_callsite_qual_name,
        CallsiteParameter.LINENO: _get_callsite_lineno,
        CallsiteParameter.THREAD: _get_callsite_thread,
        CallsiteParameter.THREAD_NAME: _get_callsite_thread_name,
        CallsiteParameter.PROCESS: _get_callsite_process,
        CallsiteParameter.PROCESS_NAME: _get_callsite_process_name,
    }
    _record_attribute_map: ClassVar[dict[CallsiteParameter, str]] = {
        CallsiteParameter.PATHNAME: "pathname",
        CallsiteParameter.FILENAME: "filename",
        CallsiteParameter.MODULE: "module",
        CallsiteParameter.FUNC_NAME: "funcName",
        CallsiteParameter.LINENO: "lineno",
        CallsiteParameter.THREAD: "thread",
        CallsiteParameter.THREAD_NAME: "threadName",
        CallsiteParameter.PROCESS: "process",
        CallsiteParameter.PROCESS_NAME: "processName",
    }

    _all_parameters: ClassVar[set[CallsiteParameter]] = set(CallsiteParameter)

    class _RecordMapping(NamedTuple):
        event_dict_key: str
        record_attribute: str

    __slots__ = ("_active_handlers", "_additional_ignores", "_record_mappings")

    def __init__(
        self,
        parameters: Collection[CallsiteParameter] = _all_parameters,
        additional_ignores: list[str] | None = None,
    ) -> None:
        if additional_ignores is None:
            additional_ignores = []
        # Ignore stack frames from the logging module. They will occur if this
        # processor is used in ProcessorFormatter, and additionally the logging
        # module should not be logging using structlog.
        self._additional_ignores = ["logging", *additional_ignores]
        self._active_handlers: list[
            tuple[CallsiteParameter, Callable[[str, FrameType], Any]]
        ] = []
        self._record_mappings: list[CallsiteParameterAdder._RecordMapping] = []
        for parameter in parameters:
            self._active_handlers.append(
                (parameter, self._handlers[parameter])
            )
            if (
                record_attr := self._record_attribute_map.get(parameter)
            ) is not None:
                self._record_mappings.append(
                    self._RecordMapping(
                        parameter.value,
                        record_attr,
                    )
                )

    def __call__(
        self, logger: logging.Logger, name: str, event_dict: EventDict
    ) -> EventDict:
        record: logging.LogRecord | None = event_dict.get("_record")
        from_structlog: bool = event_dict.get("_from_structlog", False)

        # If the event dictionary has a record, but it comes from structlog,
        # then the callsite parameters of the record will not be correct.
        if record is not None and not from_structlog:
            for mapping in self._record_mappings:
                event_dict[mapping.event_dict_key] = record.__dict__[
                    mapping.record_attribute
                ]

            return event_dict

        frame, module = _find_first_app_frame_and_name(
            additional_ignores=self._additional_ignores
        )
        for parameter, handler in self._active_handlers:
            event_dict[parameter.value] = handler(module, frame)

        return event_dict


class EventRenamer:
    r"""
    Rename the ``event`` key in event dicts.

    This is useful if you want to use consistent log message keys across
    platforms and/or use the ``event`` key for something custom.

    .. warning::

       It's recommended to put this processor right before the renderer, since
       some processors may rely on the presence and meaning of the ``event``
       key.

    Args:
        to: Rename ``event_dict["event"]`` to ``event_dict[to]``

        replace_by:
            Rename ``event_dict[replace_by]`` to ``event_dict["event"]``.
            *replace_by* missing from ``event_dict`` is handled gracefully.

    .. versionadded:: 22.1.0

    See also the :ref:`rename-event` recipe.
    """

    to: str
    replace_by: str | None

    def __init__(self, to: str, replace_by: str | None = None):
        self.to = to
        self.replace_by = replace_by

    def __call__(
        self, logger: logging.Logger, name: str, event_dict: EventDict
    ) -> EventDict:
        event = event_dict.pop("event")
        event_dict[self.to] = event

        if self.replace_by is not None:
            replace_by = event_dict.pop(self.replace_by, None)
            if replace_by is not None:
                event_dict["event"] = replace_by

        return event_dict
