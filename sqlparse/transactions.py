#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Restricted transaction-control event scanner.

This module provides a best-effort, non-validating preview of the
transaction control flow of a SQL script.  It recognizes top-level
``BEGIN``/``COMMIT``/``ROLLBACK`` statements, ``SAVEPOINT`` definitions,
``RELEASE`` of savepoints and rollbacks to savepoints, together with the
savepoint name and the position of the statement in the source text.

The scan is deliberately restricted:

* Only the top-level tokens of each statement are inspected.  Keywords
  appearing inside procedure bodies (``BEGIN ... END`` blocks), string
  literals or comments are not reported as transaction events.
* Statements that cannot be identified as transaction control statements
  are reported as :attr:`TransactionEventType.UNKNOWN`.
* The optional ordering check flags savepoints that are released or
  rolled back to without a matching preceding ``SAVEPOINT`` definition.
  It performs a purely syntactic, sequential analysis; it does not
  connect to a database and does not claim to fully validate the SQL.

Transaction start syntax differs between dialects (e.g. ``BEGIN`` vs.
``START TRANSACTION``).  The recognized forms are selected explicitly
through :class:`TransactionScanConfig`.
"""

import enum
from dataclasses import dataclass, field

from sqlparse import engine
from sqlparse import tokens as T
from sqlparse.sql import Comment, Identifier, Statement
from sqlparse.utils import remove_quotes

__all__ = [
    "TransactionEventType",
    "TransactionEvent",
    "SavepointIssue",
    "TransactionReport",
    "TransactionScanConfig",
    "scan_transactions",
    "check_savepoint_order",
]


class TransactionEventType(enum.Enum):
    """Type of a transaction control event."""

    BEGIN = "BEGIN"
    COMMIT = "COMMIT"
    ROLLBACK = "ROLLBACK"
    SAVEPOINT = "SAVEPOINT"
    RELEASE = "RELEASE"
    ROLLBACK_TO_SAVEPOINT = "ROLLBACK_TO_SAVEPOINT"
    UNKNOWN = "UNKNOWN"


@dataclass
class TransactionEvent:
    """A single recognized transaction control event.

    ``name`` holds the savepoint name for ``SAVEPOINT``, ``RELEASE`` and
    ``ROLLBACK_TO_SAVEPOINT`` events and is ``None`` otherwise.
    ``offset`` is the 0-based character offset of the first significant
    token of the statement in the source text; ``line`` and ``column``
    are 1-based.  ``statement_index`` is the 0-based index of the
    statement in the script.
    """

    type: TransactionEventType
    name: str | None
    statement_index: int
    offset: int
    line: int
    column: int
    statement: Statement


@dataclass
class SavepointIssue:
    """A savepoint ordering problem found by :func:`check_savepoint_order`."""

    message: str
    name: str
    event: TransactionEvent


@dataclass
class TransactionReport:
    """Result of :func:`scan_transactions`."""

    events: list[TransactionEvent] = field(default_factory=list)
    issues: list[SavepointIssue] = field(default_factory=list)


@dataclass(frozen=True)
class TransactionScanConfig:
    """Configuration for :func:`scan_transactions`.

    ``transaction_starters`` is a tuple of keyword sequences.  A
    statement whose leading keywords start with one of these sequences
    is reported as a ``BEGIN`` event.  Use the factory classmethods for
    common dialects or pass custom sequences explicitly.
    """

    transaction_starters: tuple[tuple[str, ...], ...] = (("BEGIN",),)

    @classmethod
    def ansi(cls) -> "TransactionScanConfig":
        """ANSI/ISO style: ``BEGIN [WORK | TRANSACTION]``."""
        return cls(transaction_starters=(("BEGIN",),))

    @classmethod
    def postgresql(cls) -> "TransactionScanConfig":
        """PostgreSQL style: ``BEGIN`` and ``START TRANSACTION``."""
        return cls(transaction_starters=(("BEGIN",), ("START", "TRANSACTION")))

    @classmethod
    def mysql(cls) -> "TransactionScanConfig":
        """MySQL/MariaDB style: ``BEGIN`` and ``START TRANSACTION``."""
        return cls(transaction_starters=(("BEGIN",), ("START", "TRANSACTION")))

    @classmethod
    def sqlserver(cls) -> "TransactionScanConfig":
        """T-SQL style: only ``BEGIN TRAN[SACTION]`` starts a transaction.

        Bare ``BEGIN`` is excluded because it opens a statement block in
        T-SQL, not a transaction.
        """
        return cls(
            transaction_starters=(("BEGIN", "TRAN"), ("BEGIN", "TRANSACTION"))
        )


def _is_ignorable(token):
    if token.is_whitespace:
        return True
    if isinstance(token, Comment) or token.ttype in T.Comment:
        return True
    return token.ttype == T.Punctuation and token.value == ";"


def _significant_tokens(stmt):
    """Top-level tokens of *stmt* without whitespace, comments, semicolons."""
    return [tok for tok in stmt.tokens if not _is_ignorable(tok)]


def _identifier_name(token):
    """Return the unquoted name of an Identifier token, or None."""
    if not isinstance(token, Identifier):
        return None
    name = token.get_real_name()
    if name is None:
        name = token.value
    return remove_quotes(name)


def _classify(tokens, config):
    """Classify a statement from its significant top-level tokens.

    Returns a ``(TransactionEventType, name)`` tuple.
    """
    if not tokens:
        return TransactionEventType.UNKNOWN, None

    first = tokens[0]
    etype = TransactionEventType

    if first.ttype in T.Keyword:
        head = first.normalized
        if head == "SAVEPOINT":
            name = _identifier_name(tokens[1]) if len(tokens) > 1 else None
            return etype.SAVEPOINT, name
        if head == "COMMIT":
            return etype.COMMIT, None
        if head == "ROLLBACK":
            return _classify_rollback(tokens)
        if head == "RELEASE":
            # RELEASE is a keyword only with customized lexers.
            name = _identifier_name(tokens[-1]) if len(tokens) > 1 else None
            return etype.RELEASE, name
        if _matches_starter(tokens, config):
            return etype.BEGIN, None
        return etype.UNKNOWN, None

    if isinstance(first, Identifier) and (first.get_real_name() or "").upper() \
            == "RELEASE":
        return _classify_release(tokens)

    return etype.UNKNOWN, None


def _classify_rollback(tokens):
    etype = TransactionEventType
    for idx, token in enumerate(tokens[1:], start=1):
        if token.ttype in T.Keyword and token.normalized == "TO":
            # ROLLBACK [WORK] TO [SAVEPOINT] name
            for candidate in tokens[idx + 1:]:
                if isinstance(candidate, Identifier):
                    return etype.ROLLBACK_TO_SAVEPOINT, \
                        _identifier_name(candidate)
            return etype.ROLLBACK_TO_SAVEPOINT, None
    return etype.ROLLBACK, None


def _classify_release(tokens):
    etype = TransactionEventType
    first = tokens[0]
    if len(tokens) == 1:
        # RELEASE name  (grouped as a single aliased identifier)
        alias = first.get_name()
        if alias is not None and alias.upper() != "RELEASE":
            return etype.RELEASE, remove_quotes(alias)
        return etype.RELEASE, None
    # RELEASE [SAVEPOINT] name
    return etype.RELEASE, _identifier_name(tokens[-1])


def _matches_starter(tokens, config):
    words = []
    for token in tokens:
        if token.ttype in T.Keyword:
            words.append(token.normalized)
        elif isinstance(token, Identifier):
            # Dialect-specific words like TRAN in "BEGIN TRAN" are not
            # lexer keywords and are grouped as identifiers.
            name = token.get_real_name()
            if name is None:
                break
            words.append(remove_quotes(name).upper())
        else:
            break
    for starter in config.transaction_starters:
        pattern = tuple(word.upper() for word in starter)
        if tuple(words[: len(pattern)]) == pattern:
            return True
    return False


def _parse(source, encoding):
    stack = engine.FilterStack()
    stack.enable_grouping()
    return tuple(stack.run(source, encoding))


def _first_significant_offset(stmt):
    """Offset of the first significant token within the statement text."""
    lead = 0
    for token in stmt.tokens:
        if not _is_ignorable(token):
            break
        lead += len(str(token))
    return lead


def scan_transactions(source, config=None, encoding=None):
    """Scan *source* for transaction control events.

    :param source: A string containing one or more SQL statements.
    :param config: A :class:`TransactionScanConfig` selecting the
        recognized transaction start syntax.  Defaults to
        :meth:`TransactionScanConfig.ansi`.
    :param encoding: The encoding of the source (optional).
    :returns: A :class:`TransactionReport` with one
        :class:`TransactionEvent` per statement (unidentified statements
        are reported as ``UNKNOWN``) and the savepoint ordering issues
        found by :func:`check_savepoint_order`.

    The scan is syntactic and best-effort only; it does not connect to a
    database and does not fully validate the SQL.
    """
    if config is None:
        config = TransactionScanConfig.ansi()

    events = []
    cursor = 0
    for index, stmt in enumerate(_parse(source, encoding)):
        text = str(stmt)
        start = source.find(text, cursor)
        if start == -1:  # pragma: no cover - defensive fallback
            start = cursor
        cursor = start + len(text)

        offset = start + _first_significant_offset(stmt)
        line = source.count("\n", 0, offset) + 1
        column = offset - source.rfind("\n", 0, offset)

        event_type, name = _classify(_significant_tokens(stmt), config)
        events.append(
            TransactionEvent(
                type=event_type,
                name=name,
                statement_index=index,
                offset=offset,
                line=line,
                column=column,
                statement=stmt,
            )
        )

    return TransactionReport(events=events, issues=check_savepoint_order(events))


def check_savepoint_order(events):
    """Check *events* for savepoints used without a matching definition.

    Tracks savepoint definitions sequentially: ``RELEASE`` and
    ``ROLLBACK TO SAVEPOINT`` require a preceding ``SAVEPOINT`` with the
    same name (compared case-insensitively) in the currently active
    transaction.  ``RELEASE`` discards the named savepoint and all
    savepoints defined after it; ``ROLLBACK TO SAVEPOINT`` discards only
    later savepoints.  ``BEGIN``, ``COMMIT`` and ``ROLLBACK`` clear all
    active savepoints.

    :param events: An iterable of :class:`TransactionEvent` objects.
    :returns: A list of :class:`SavepointIssue` objects, one per
        unmatched savepoint reference.
    """
    issues = []
    active = []  # savepoint names, upper-cased, in definition order
    etype = TransactionEventType

    for event in events:
        if event.type in (etype.BEGIN, etype.COMMIT, etype.ROLLBACK):
            active.clear()
        elif event.type == etype.SAVEPOINT:
            if event.name is not None:
                key = event.name.upper()
                if key in active:
                    # Redefinition replaces the earlier savepoint.
                    active.remove(key)
                active.append(key)
        elif event.type in (etype.RELEASE, etype.ROLLBACK_TO_SAVEPOINT):
            if event.name is None:
                continue
            key = event.name.upper()
            if key not in active:
                verb = (
                    "RELEASE" if event.type == etype.RELEASE else "ROLLBACK TO"
                )
                issues.append(
                    SavepointIssue(
                        message=(
                            f"{verb} references savepoint {event.name!r} "
                            "without a matching preceding SAVEPOINT"
                        ),
                        name=event.name,
                        event=event,
                    )
                )
            elif event.type == etype.RELEASE:
                del active[active.index(key):]
            else:
                del active[active.index(key) + 1:]

    return issues
