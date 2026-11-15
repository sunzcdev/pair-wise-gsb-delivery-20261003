"""Optional parse explanations for the pure-Python Avro reader.

This module is private; the public entry points are ``ExplanationEvent`` and
the ``explain_callback`` / ``on_explain_error`` arguments of
:class:`fastavro.reader`, :class:`fastavro.block_reader` and
:func:`fastavro.schemaless_reader`.

Explanations only describe choices that were *actually executed* while
decoding a record:

* a writer field was matched to a reader field through an alias
  (``"field_alias"``);
* a reader field missing from the writer was filled with its default value
  (``"field_default"``);
* a union branch was selected (``"union_branch"``);
* a numeric/bytes value was promoted to the reader type (``"promotion"``).

Union branches that were never selected and writer fields that were skipped
because the reader has no matching field never produce events.

By design, explanation events never carry field values, only metadata about
the schema resolution decisions and the schema path / record index at which
they happened.
"""

from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional

# Event ``event_type`` values.
FIELD_ALIAS = "field_alias"
FIELD_DEFAULT = "field_default"
UNION_BRANCH = "union_branch"
PROMOTION = "promotion"

# Accepted values for the ``on_explain_error`` option.
RAISE = "raise"
IGNORE = "ignore"


@dataclass
class ExplanationEvent:
    """A single parse decision made while reading an Avro record.

    Instances are immutable and never contain decoded field values; only the
    metadata needed to understand *why* the output record looks the way it
    does.

    .. attribute:: event_type

        One of :data:`FIELD_ALIAS`, :data:`FIELD_DEFAULT`,
        :data:`UNION_BRANCH` or :data:`PROMOTION`.

    .. attribute:: record_index

        Zero-based index of the record within the input stream (across all
        blocks for a binary container). Always ``0`` for a schemaless read.

    .. attribute:: path

        Schema path at which the decision happened, e.g.
        ``["outer", "inner", "value"]``. Record and field names in the path
        are reader-schema names when a reader schema is supplied, otherwise
        writer-schema names. The path is a fresh list per event.

    .. attribute:: details

        Event-specific metadata (never field values):

        * ``field_alias`` -> ``{"writer_field", "reader_field", "alias"}``
        * ``field_default`` -> ``{"field"}``
        * ``union_branch`` -> ``{"writer_index", "writer_branch",
          "reader_branch"}``
        * ``promotion`` -> ``{"writer_type", "reader_type"}``
    """

    event_type: str
    record_index: int
    path: List[str] = field(default_factory=list)
    details: Dict[str, Any] = field(default_factory=dict)


# Signature of the callback supplied by the caller.
ExplainCallback = Callable[[ExplanationEvent], None]


def validate_explain_options(
    explain_callback: Optional[ExplainCallback],
    on_explain_error: str,
) -> None:
    """Validate the explanation options at the public entry points."""
    if explain_callback is not None and not callable(explain_callback):
        raise TypeError("explain_callback must be callable or None")
    if on_explain_error not in (RAISE, IGNORE):
        raise ValueError(
            "on_explain_error must be either 'raise' or 'ignore', got "
            f"{on_explain_error!r}"
        )


def union_branch_name(schema: Any) -> Any:
    """Return a stable, value-free description of a union branch."""
    if isinstance(schema, dict):
        return schema.get("name", schema.get("type"))
    return schema


class ExplanationCollector:
    """Tracks the schema path / record index and dispatches events.

    A collector is only ever created when the caller supplied an
    ``explain_callback``; without one the reader does no bookkeeping at all.
    """

    def __init__(
        self,
        callback: ExplainCallback,
        on_explain_error: str,
    ) -> None:
        self._callback = callback
        self._on_error = on_explain_error
        self.record_index = 0
        self._path: List[str] = []

    def begin_record(self) -> None:
        """Reset the path for a new top-level record and advance the index."""
        self._path = []
        self.record_index += 1

    def push(self, name: str) -> None:
        self._path.append(name)

    def pop(self) -> None:
        self._path.pop()

    def emit(self, event_type: str, details: Dict[str, Any]) -> None:
        """Dispatch one event to the caller's callback.

        The event reports a decision that has genuinely been executed. When
        ``on_explain_error`` is ``"raise"`` (the default) an exception raised
        by the callback propagates to the caller; when it is ``"ignore"`` the
        exception is swallowed so that normal data reading is unaffected.
        Either way, the decoded data itself is never modified by the
        explanation machinery.
        """
        event = ExplanationEvent(
            event_type=event_type,
            record_index=self.record_index - 1,
            path=list(self._path),
            details=dict(details),
        )
        try:
            self._callback(event)
        except Exception:
            if self._on_error == RAISE:
                raise
            # Explicitly requested by the caller via on_explain_error="ignore".

    # Convenience wrappers for the four event kinds.
    def emit_field_alias(
        self,
        writer_field: str,
        reader_field: str,
        alias: str,
    ) -> None:
        self.emit(
            FIELD_ALIAS,
            {
                "writer_field": writer_field,
                "reader_field": reader_field,
                "alias": alias,
            },
        )

    def emit_field_default(self, field_name: str) -> None:
        self.emit(FIELD_DEFAULT, {"field": field_name})

    def emit_union_branch(
        self,
        writer_index: int,
        writer_branch: Any,
        reader_branch: Any,
    ) -> None:
        self.emit(
            UNION_BRANCH,
            {
                "writer_index": writer_index,
                "writer_branch": union_branch_name(writer_branch),
                "reader_branch": union_branch_name(reader_branch),
            },
        )

    def emit_promotion(self, writer_type: str, reader_type: str) -> None:
        self.emit(
            PROMOTION,
            {"writer_type": writer_type, "reader_type": reader_type},
        )
