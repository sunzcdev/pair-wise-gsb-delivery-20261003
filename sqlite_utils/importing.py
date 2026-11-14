"""
Streaming helpers for inferring column types from a bounded sample.

The functions in this module deliberately keep the sample and the rest of the
input in a single iterator.  Callers can therefore inspect the first N rows to
choose a schema and then import those same rows plus all later rows without
reading the source twice.
"""

from __future__ import annotations

import enum
import itertools
import json
import re
from collections.abc import Callable, Iterable, Iterator, Mapping
from dataclasses import dataclass, field
from typing import Any

SQLITE_INTEGER_MIN = -(2**63)
SQLITE_INTEGER_MAX = 2**63 - 1

_INTEGER_RE = re.compile(r"[+-]?\d+")


class LateValuePolicy(str, enum.Enum):
    """
    What to do when a row after the sample does not fit the inferred type.

    ``REJECT`` skips the whole row.  ``ERROR_COLUMN`` writes the row, but
    moves offending values into the configured error column and sets the
    original columns to NULL.
    """

    REJECT = "reject"
    ERROR_COLUMN = "error-column"

    @classmethod
    def from_value(cls, value: "LateValuePolicy | str | None") -> "LateValuePolicy":
        if value is None:
            return cls.REJECT
        if isinstance(value, cls):
            return value
        normalized = str(value).strip().lower()
        aliases = {
            "reject": cls.REJECT,
            "skip": cls.REJECT,
            "error": cls.ERROR_COLUMN,
            "error-column": cls.ERROR_COLUMN,
            "error_column": cls.ERROR_COLUMN,
        }
        try:
            return aliases[normalized]
        except KeyError:
            raise ValueError(
                "late value policy must be one of: reject, error-column"
            ) from None


class SourceRow(dict):
    """A row dictionary that remembers where it came from in the input."""

    def __init__(
        self,
        values: Mapping[str, Any],
        source_row: int | None = None,
        source_name: str | None = None,
    ) -> None:
        super().__init__(values)
        self.source_row = source_row
        self.source_name = source_name


@dataclass(frozen=True)
class ColumnInference:
    """The type selected for one column, together with evidence for the choice."""

    name: str
    sqlite_type: str
    python_type: Any
    reason: str
    sample_count: int
    non_null_count: int
    missing_count: int
    null_count: int
    empty_count: int
    boolean_count: int
    integer_count: int
    float_count: int
    text_count: int
    bytes_count: int
    oversized_integer_count: int
    sample_row_numbers: tuple[int, ...] = ()

    @property
    def chosen_type(self) -> str:
        return self.sqlite_type

    @property
    def explanation(self) -> str:
        return self.reason

    def as_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "type": self.sqlite_type,
            "python_type": self.python_type,
            "reason": self.reason,
            "sample_count": self.sample_count,
            "non_null_count": self.non_null_count,
            "missing_count": self.missing_count,
            "null_count": self.null_count,
            "empty_count": self.empty_count,
            "boolean_count": self.boolean_count,
            "integer_count": self.integer_count,
            "float_count": self.float_count,
            "text_count": self.text_count,
            "bytes_count": self.bytes_count,
            "oversized_integer_count": self.oversized_integer_count,
            "sample_row_numbers": list(self.sample_row_numbers),
        }


@dataclass(frozen=True)
class TypeInference:
    """The result of inferring a complete row schema from an input sample."""

    columns: tuple[ColumnInference, ...]
    sample_row_count: int
    sample_size: int
    empty_is_null: bool = False
    parse_numbers_in_strings: bool = True

    @property
    def types(self) -> dict[str, str]:
        return {column.name: column.sqlite_type for column in self.columns}

    @property
    def python_types(self) -> dict[str, Any]:
        return {column.name: column.python_type for column in self.columns}

    @property
    def reasons(self) -> dict[str, str]:
        return {column.name: column.reason for column in self.columns}

    @property
    def explanations(self) -> dict[str, str]:
        return self.reasons

    def __iter__(self) -> Iterator[str]:
        return iter(self.types)

    def __len__(self) -> int:
        return len(self.columns)

    def __contains__(self, column_name: object) -> bool:
        return any(column.name == column_name for column in self.columns)

    def __getitem__(self, column_name: str) -> ColumnInference:
        for column in self.columns:
            if column.name == column_name:
                return column
        raise KeyError(column_name)

    def get(self, column_name: str) -> ColumnInference | None:
        try:
            return self[column_name]
        except KeyError:
            return None

    def as_dict(self) -> dict[str, Any]:
        return {
            "sample_size": self.sample_size,
            "sample_row_count": self.sample_row_count,
            "empty_is_null": self.empty_is_null,
            "parse_numbers_in_strings": self.parse_numbers_in_strings,
            "columns": [column.as_dict() for column in self.columns],
            "types": self.types,
            "reasons": self.reasons,
        }

    def with_overrides(self, overrides: Mapping[str, Any]) -> "TypeInference":
        """Return a copy with explicitly selected column types replacing guesses."""
        if not overrides:
            return self
        override_types = {
            name: _sqlite_type_name(value) for name, value in overrides.items()
        }
        updated = []
        seen = set()
        for column in self.columns:
            seen.add(column.name)
            if column.name not in override_types:
                updated.append(column)
                continue
            sqlite_type = override_types[column.name]
            updated.append(
                ColumnInference(
                    name=column.name,
                    sqlite_type=sqlite_type,
                    python_type=_python_type_for(sqlite_type),
                    reason="explicitly selected with --type",
                    sample_count=column.sample_count,
                    non_null_count=column.non_null_count,
                    missing_count=column.missing_count,
                    null_count=column.null_count,
                    empty_count=column.empty_count,
                    boolean_count=column.boolean_count,
                    integer_count=column.integer_count,
                    float_count=column.float_count,
                    text_count=column.text_count,
                    bytes_count=column.bytes_count,
                    oversized_integer_count=column.oversized_integer_count,
                    sample_row_numbers=column.sample_row_numbers,
                )
            )
        for name, sqlite_type in override_types.items():
            if name in seen:
                continue
            updated.append(
                ColumnInference(
                    name=name,
                    sqlite_type=sqlite_type,
                    python_type=_python_type_for(sqlite_type),
                    reason="explicitly selected with --type",
                    sample_count=0,
                    non_null_count=0,
                    missing_count=self.sample_row_count,
                    null_count=0,
                    empty_count=0,
                    boolean_count=0,
                    integer_count=0,
                    float_count=0,
                    text_count=0,
                    bytes_count=0,
                    oversized_integer_count=0,
                )
            )
        return TypeInference(
            columns=tuple(updated),
            sample_row_count=self.sample_row_count,
            sample_size=self.sample_size,
            empty_is_null=self.empty_is_null,
            parse_numbers_in_strings=self.parse_numbers_in_strings,
        )


@dataclass(frozen=True)
class ValueMismatch:
    """A single value that cannot be represented using the selected type."""

    source_row: int | None
    column: str
    value: Any
    expected_type: str
    reason: str

    def as_dict(self) -> dict[str, Any]:
        return {
            "source_row": self.source_row,
            "column": self.column,
            "value": self.value,
            "expected_type": self.expected_type,
            "reason": self.reason,
        }


@dataclass
class _ColumnStats:
    sample_count: int = 0
    non_null_count: int = 0
    missing_count: int = 0
    null_count: int = 0
    empty_count: int = 0
    boolean_count: int = 0
    integer_count: int = 0
    float_count: int = 0
    text_count: int = 0
    bytes_count: int = 0
    oversized_integer_count: int = 0
    sample_row_numbers: list[int] = field(default_factory=list)


class SampleResult:
    """
    A replayed input stream plus the inference derived from its sample.

    Iterating this object yields the sampled rows first, followed by every row
    that had not yet been consumed.  The source iterator is never restarted.
    """

    def __init__(
        self,
        sample: list[dict[str, Any]],
        remaining: Iterator[dict[str, Any]],
        inference: TypeInference,
    ) -> None:
        self.sample = sample
        self.remaining = remaining
        self.inference = inference
        self.types = inference.types
        self.reasons = inference.reasons

    def __iter__(self) -> Iterator[dict[str, Any]]:
        return itertools.chain(self.sample, self.remaining)


def take_sample(
    rows: Iterable[dict[str, Any]], sample_size: int
) -> tuple[list[dict[str, Any]], Iterator[dict[str, Any]]]:
    """
    Consume at most ``sample_size`` rows and return them with the live remainder.

    The remainder starts immediately after the last sampled row, so combining
    ``sample`` with ``remaining`` replays the input exactly once.
    """
    if sample_size < 1:
        raise ValueError("sample_size must be at least 1")
    iterator = iter(rows)
    sample: list[dict[str, Any]] = []
    for _ in range(sample_size):
        try:
            sample.append(next(iterator))
        except StopIteration:
            break
    return sample, iterator


def infer_column_types(
    rows: Iterable[Mapping[str, Any]],
    *,
    empty_is_null: bool = False,
    parse_numbers_in_strings: bool = True,
) -> TypeInference:
    """
    Infer SQLite column types from an already bounded iterable of rows.

    This function intentionally has no ``sample_size`` argument: passing an
    unbounded input would read it all.  Use :func:`sample_and_infer` when you
    need a bounded sample that can still be replayed for the actual import.
    """
    stats: dict[str, _ColumnStats] = {}
    row_count = 0
    for row_count, row in enumerate(rows, start=1):
        source_row = _source_row_number(row, row_count)
        for column_name in _all_columns(stats, row):
            column_stats = stats.get(column_name)
            if column_stats is None:
                # This column did not exist on earlier rows, so those rows
                # effectively had the field missing.
                column_stats = _ColumnStats(
                    sample_count=row_count - 1,
                    missing_count=row_count - 1,
                )
                stats[column_name] = column_stats
            column_stats.sample_count += 1
            column_stats.sample_row_numbers.append(source_row)
            if column_name not in row:
                column_stats.missing_count += 1
                continue
            _observe_value(
                column_stats,
                row[column_name],
                empty_is_null=empty_is_null,
                parse_numbers_in_strings=parse_numbers_in_strings,
            )
    columns = tuple(
        _infer_one_column(name, column_stats, row_count, empty_is_null)
        for name, column_stats in stats.items()
    )
    return TypeInference(
        columns=columns,
        sample_row_count=row_count,
        sample_size=row_count,
        empty_is_null=empty_is_null,
        parse_numbers_in_strings=parse_numbers_in_strings,
    )


def sample_and_infer(
    rows: Iterable[dict[str, Any]],
    sample_size: int,
    *,
    empty_is_null: bool = False,
    parse_numbers_in_strings: bool = True,
) -> SampleResult:
    """Take a bounded sample, infer types from it, and replay it for import."""
    sample, remaining = take_sample(rows, sample_size)
    inference = infer_column_types(
        sample,
        empty_is_null=empty_is_null,
        parse_numbers_in_strings=parse_numbers_in_strings,
    )
    return SampleResult(sample, remaining, inference)


def apply_inferred_types(
    rows: Iterable[dict[str, Any]],
    inference: TypeInference,
    *,
    empty_is_null: bool | None = None,
    late_value_policy: LateValuePolicy | str | None = None,
    error_column: str | None = None,
    source_row_start: int = 1,
    on_reject: Callable[[list[ValueMismatch]], None] | None = None,
) -> Iterator[dict[str, Any]]:
    """
    Coerce rows to the inferred types and apply the late-value policy.

    The sample rows are expected to be part of ``rows`` too.  This is what
    makes the sample a replay rather than a separate pass over the source.
    """
    if late_value_policy is None:
        policy = (
            LateValuePolicy.ERROR_COLUMN if error_column else LateValuePolicy.REJECT
        )
    else:
        policy = LateValuePolicy.from_value(late_value_policy)
    if policy == LateValuePolicy.ERROR_COLUMN and not error_column:
        raise ValueError("error_column is required for the error-column policy")
    if empty_is_null is None:
        empty_is_null = inference.empty_is_null

    inferred_columns = {column.name: column for column in inference.columns}
    if error_column and error_column in inferred_columns:
        if inferred_columns[error_column].sample_count:
            raise ValueError(
                f"error_column {error_column!r} conflicts with an input column"
            )
        # An existing table can already have the destination error column.
        inferred_columns.pop(error_column)

    for fallback_row_number, row in enumerate(rows, start=source_row_start):
        source_row = _source_row_number(row, fallback_row_number)
        output = dict(row)
        mismatches: list[ValueMismatch] = []
        for column_name, column_inference in inferred_columns.items():
            if column_name not in row:
                continue
            original_value = row[column_name]
            compatible, normalized, reason = _coerce_value(
                original_value,
                column_inference.sqlite_type,
                empty_is_null=empty_is_null,
            )
            if compatible:
                output[column_name] = normalized
            else:
                output[column_name] = None
                mismatches.append(
                    ValueMismatch(
                        source_row=source_row,
                        column=column_name,
                        value=original_value,
                        expected_type=column_inference.sqlite_type,
                        reason=reason,
                    )
                )

        if not mismatches:
            yield output
            continue

        if policy == LateValuePolicy.REJECT:
            if on_reject is not None:
                on_reject(mismatches)
            continue

        assert error_column is not None
        payload: list[dict[str, Any]] = [mismatch.as_dict() for mismatch in mismatches]
        output[error_column] = payload[0] if len(payload) == 1 else payload
        yield output


def _all_columns(
    stats: dict[str, _ColumnStats], row: Mapping[str, Any]
) -> list[str]:
    columns = list(stats)
    for column_name in row:
        if column_name not in stats:
            columns.append(column_name)
    return columns


def _observe_value(
    stats: _ColumnStats,
    value: Any,
    *,
    empty_is_null: bool,
    parse_numbers_in_strings: bool,
) -> None:
    if value is None:
        stats.null_count += 1
        return
    stats.non_null_count += 1

    if isinstance(value, str) and value == "":
        stats.empty_count += 1
        if empty_is_null:
            stats.null_count += 1
            stats.non_null_count -= 1
            return
        stats.text_count += 1
        return

    if isinstance(value, bool):
        stats.boolean_count += 1
        return
    if isinstance(value, int):
        if SQLITE_INTEGER_MIN <= value <= SQLITE_INTEGER_MAX:
            stats.integer_count += 1
        else:
            stats.oversized_integer_count += 1
        return
    if isinstance(value, float):
        stats.float_count += 1
        return
    if isinstance(value, (bytes, bytearray, memoryview)):
        stats.bytes_count += 1
        return
    if isinstance(value, str):
        if parse_numbers_in_strings:
            integer_value = _parse_integer(value)
            if integer_value is not None:
                if SQLITE_INTEGER_MIN <= integer_value <= SQLITE_INTEGER_MAX:
                    stats.integer_count += 1
                else:
                    stats.oversized_integer_count += 1
                return
            if _parse_float(value) is not None:
                stats.float_count += 1
                return
        stats.text_count += 1
        return
    stats.text_count += 1


def _infer_one_column(
    name: str,
    stats: _ColumnStats,
    row_count: int,
    empty_is_null: bool,
) -> ColumnInference:
    if stats.bytes_count:
        sqlite_type = "BLOB"
    elif stats.oversized_integer_count:
        sqlite_type = "TEXT"
    elif stats.text_count:
        sqlite_type = "TEXT"
    elif stats.float_count:
        sqlite_type = "REAL"
    elif stats.integer_count or stats.boolean_count:
        sqlite_type = "INTEGER"
    else:
        sqlite_type = "TEXT"

    reason_parts: list[str] = []
    if stats.non_null_count == 0:
        reason_parts.append(
            f"no non-null values in the {row_count}-row sample; TEXT is the safe default"
        )
    elif sqlite_type == "TEXT":
        if stats.oversized_integer_count:
            reason_parts.append(
                "{} integer value(s) are outside SQLite's signed 64-bit range; "
                "TEXT preserves the exact value".format(
                    stats.oversized_integer_count
                )
            )
        elif stats.text_count and not stats.integer_count and not stats.float_count:
            reason_parts.append(
                "{} text value(s) in the sample require TEXT".format(stats.text_count)
            )
        else:
            reason_parts.append(
                "the sample mixes text with another value type; TEXT avoids lossy conversion"
            )
    elif sqlite_type == "BLOB":
        if stats.bytes_count == stats.non_null_count:
            reason_parts.append(
                "all {} non-null sampled value(s) are bytes; BLOB preserves binary data".format(
                    stats.bytes_count
                )
            )
        else:
            reason_parts.append(
                "the sample contains bytes; BLOB stores other values as UTF-8 bytes"
            )
    elif sqlite_type == "REAL":
        reason_parts.append(
            "the sample contains {} floating-point value(s); REAL preserves fractions".format(
                stats.float_count
            )
        )
    elif sqlite_type == "INTEGER":
        if stats.boolean_count and stats.integer_count:
            reason_parts.append(
                "the sample contains integers and {} boolean value(s); both map to INTEGER 0/1".format(
                    stats.boolean_count
                )
            )
        elif stats.boolean_count:
            reason_parts.append(
                "all {} sampled value(s) are booleans; SQLite stores them as INTEGER 0/1".format(
                    stats.boolean_count
                )
            )
        else:
            reason_parts.append(
                "all {} non-null sampled value(s) are integers within SQLite's signed 64-bit range".format(
                    stats.integer_count
                )
            )

    if stats.missing_count:
        reason_parts.append(
            "{} missing field(s) are treated as NULL and do not affect the choice".format(
                stats.missing_count
            )
        )
    if stats.null_count:
        reason_parts.append(
            "{} NULL value(s) do not affect the choice".format(stats.null_count)
        )
    if stats.empty_count:
        if empty_is_null:
            reason_parts.append(
                "{} empty string(s) were treated as NULL".format(stats.empty_count)
            )
        else:
            reason_parts.append(
                "{} empty string(s) were kept as text".format(stats.empty_count)
            )

    return ColumnInference(
        name=name,
        sqlite_type=sqlite_type,
        python_type=_python_type_for(sqlite_type),
        reason="; ".join(reason_parts),
        sample_count=stats.sample_count,
        non_null_count=stats.non_null_count,
        missing_count=stats.missing_count,
        null_count=stats.null_count,
        empty_count=stats.empty_count,
        boolean_count=stats.boolean_count,
        integer_count=stats.integer_count,
        float_count=stats.float_count,
        text_count=stats.text_count,
        bytes_count=stats.bytes_count,
        oversized_integer_count=stats.oversized_integer_count,
        sample_row_numbers=tuple(stats.sample_row_numbers),
    )


def _coerce_value(
    value: Any, sqlite_type: str, *, empty_is_null: bool
) -> tuple[bool, Any, str]:
    if value is None:
        return True, None, ""
    if isinstance(value, str) and value == "" and empty_is_null:
        return True, None, ""

    if sqlite_type == "INTEGER":
        if isinstance(value, bool):
            return True, int(value), ""
        if isinstance(value, int):
            if SQLITE_INTEGER_MIN <= value <= SQLITE_INTEGER_MAX:
                return True, value, ""
            return (
                False,
                None,
                "integer is outside SQLite's signed 64-bit range",
            )
        parsed_integer = _parse_integer(value)
        if parsed_integer is not None:
            if SQLITE_INTEGER_MIN <= parsed_integer <= SQLITE_INTEGER_MAX:
                return True, parsed_integer, ""
            return (
                False,
                None,
                "integer is outside SQLite's signed 64-bit range",
            )
        return False, None, "value is not an integer"

    if sqlite_type in ("REAL", "FLOAT", "NUMERIC"):
        if isinstance(value, bool):
            return True, float(value), ""
        if isinstance(value, int):
            if SQLITE_INTEGER_MIN <= value <= SQLITE_INTEGER_MAX:
                return True, float(value), ""
            return (
                False,
                None,
                "integer is outside SQLite's signed 64-bit range",
            )
        if isinstance(value, float):
            return True, value, ""
        parsed_float = _parse_float(value)
        if parsed_float is not None:
            return True, parsed_float, ""
        return False, None, "value is not a number"

    if sqlite_type == "BLOB":
        if isinstance(value, bytes):
            return True, value, ""
        if isinstance(value, (bytearray, memoryview)):
            return True, bytes(value), ""
        if isinstance(value, (dict, list, tuple)):
            return (
                True,
                json.dumps(value, default=repr, ensure_ascii=False).encode("utf-8"),
                "",
            )
        if isinstance(value, bool):
            return True, str(int(value)).encode("utf-8"), ""
        if isinstance(value, str):
            return True, value.encode("utf-8"), ""
        if isinstance(value, (int, float)):
            return True, str(value).encode("utf-8"), ""
        return False, None, "value cannot be encoded as bytes"

    if sqlite_type == "ANY":
        return True, value, ""

    # TEXT is deliberately broad enough to preserve integers that did not fit
    # in INTEGER, but bytes cannot be represented as text without corruption.
    if isinstance(value, bytes) or isinstance(value, (bytearray, memoryview)):
        return False, None, "bytes cannot be stored in a TEXT column"
    if isinstance(value, (dict, list, tuple)):
        return True, value, ""
    if isinstance(value, str):
        return True, value, ""
    if isinstance(value, bool):
        return True, str(int(value)), ""
    return True, str(value), ""


def _parse_integer(value: Any) -> int | None:
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, int):
        return value
    if not isinstance(value, str):
        return None
    stripped = value.strip()
    if not stripped or not _INTEGER_RE.fullmatch(stripped):
        return None
    try:
        return int(stripped)
    except ValueError:
        return None


def _parse_float(value: Any) -> float | None:
    if isinstance(value, bool):
        return float(value)
    if isinstance(value, (int, float)):
        return float(value)
    if not isinstance(value, str):
        return None
    stripped = value.strip()
    if not stripped:
        return None
    try:
        return float(stripped)
    except ValueError:
        return None


def _source_row_number(row: Mapping[str, Any], fallback: int) -> int:
    source_row = getattr(row, "source_row", None)
    return source_row if isinstance(source_row, int) else fallback


def _sqlite_type_name(value: Any) -> str:
    if isinstance(value, str):
        normalized = value.upper()
        if normalized == "FLOAT":
            return "REAL"
        return normalized
    python_types = {
        str: "TEXT",
        int: "INTEGER",
        bool: "INTEGER",
        float: "REAL",
        bytes: "BLOB",
        dict: "TEXT",
        list: "TEXT",
        tuple: "TEXT",
        type(None): "TEXT",
    }
    return python_types.get(value, "TEXT")


def _python_type_for(sqlite_type: str) -> Any:
    if sqlite_type == "INTEGER":
        return int
    if sqlite_type in ("REAL", "FLOAT", "NUMERIC"):
        return float
    if sqlite_type == "BLOB":
        return bytes
    return str


# Friendly aliases for callers that prefer result-oriented names.
ColumnTypeInference = ColumnInference
InferenceResult = TypeInference
