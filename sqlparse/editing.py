"""Restricted range editing of grouped token trees.

The helpers in this module edit a :class:`~sqlparse.sql.TokenList` by
replacing, inserting or deleting *whole leaf tokens*.  They never flatten
the tree back into a string and re-parse it, so the identity of every
token outside the edited range is preserved.

Edits are deliberately conservative:

* an edit range must start and end on leaf-token boundaries, it can never
  cut into the middle of a leaf token;
* an edit range must be contained within a single group, ranges crossing a
  group boundary are rejected instead of being "repaired";
* overlapping edits in one batch are rejected;
* the tree is never re-grouped.  Replacing ``foo(1)`` with ``bar(2)`` keeps
  the surrounding group structure, even if it no longer reflects the
  changed syntax.  Use :func:`check_structure` to inspect whether the
  cached structure is still valid.

Every edit is validated before the tree is touched and application is
atomic: a failed batch never leaves a partially modified tree behind.
"""

from __future__ import annotations

import copy
from dataclasses import dataclass, field

from sqlparse import sql
from sqlparse.exceptions import RangeEditError
from sqlparse.lexer import Lexer


@dataclass(frozen=True)
class EditOp:
    """A single leaf-token range edit.

    Offsets are character positions relative to the flattened value of the
    tree that is edited (the value returned by ``str(root)``).

    ``start`` and ``end`` delimit the affected half-open range ``[start,
    end)`` and must both fall on leaf-token boundaries.  For an insertion
    both are ``None`` and ``at`` gives the boundary at which the new tokens
    are inserted (``0`` inserts at the very beginning, ``len(str(root))``
    at the very end).

    ``replacement`` is the text put in place of the edited range.  An
    empty string therefore turns the operation into a deletion.  It is
    lexed into leaf tokens and is *not* grouped.
    """

    start: int | None = None
    end: int | None = None
    replacement: str = ""
    at: int | None = None
    tokens: tuple[sql.Token, ...] | None = field(default=None,
                                                  compare=False)

    @classmethod
    def replace(cls, start: int, end: int, replacement: str) -> "EditOp":
        """Replace the leaf tokens covering ``[start, end)``."""
        return cls(start=start, end=end, replacement=replacement)

    @classmethod
    def insert(cls, at: int, text: str) -> "EditOp":
        """Insert ``text`` at the leaf boundary ``at``."""
        return cls(at=at, replacement=text)

    @classmethod
    def delete(cls, start: int, end: int) -> "EditOp":
        """Delete the leaf tokens covering ``[start, end)``."""
        return cls(start=start, end=end, replacement="")

    @property
    def is_insert(self) -> bool:
        return self.start is None and self.end is None

    def _boundaries(self) -> tuple[int, int]:
        if self.is_insert:
            if self.at is None:
                raise RangeEditError("insertion requires an 'at' boundary")
            return self.at, self.at
        return self.start, self.end


def _leaf_spans(root: sql.TokenList) -> list[tuple[int, int, sql.Token]]:
    """Return ``(start, end, token)`` for every leaf token in order."""
    spans = []
    offset = 0
    for token in root.flatten():
        length = len(token.value)
        spans.append((offset, offset + length, token))
        offset += length
    return spans


def _boundary_set(spans) -> set[int]:
    boundaries = {0}
    for start, end, _ in spans:
        boundaries.add(start)
        boundaries.add(end)
    return boundaries


def _insert_positions(root: sql.TokenList, spans, offset: int):
    """Map every enclosing group to its insert index at a boundary.

    A group is included when one of its own direct-child boundaries is at
    *offset*.  The deepest group in the returned mapping is the natural
    insertion target.
    """
    positions: dict[sql.TokenList, int] = {}
    for start, end, token in spans:
        if start != offset and end != offset:
            continue
        group = token.parent
        node = token
        while group is not None:
            direct_index = group.token_index(node)
            if start == offset:
                positions.setdefault(group, direct_index)
            if end == offset:
                positions.setdefault(group, direct_index + 1)
            node = group
            group = group.parent
    return positions


def _deepest_common_group(first: sql.Token, last: sql.Token) -> sql.TokenList:
    """Deepest group that directly contains both leaf tokens."""
    first_ancestors = []
    node = first.parent
    while node is not None:
        first_ancestors.append(node)
        node = node.parent

    node = last.parent
    while node is not None:
        if node in first_ancestors:
            return node
        node = node.parent
    raise RangeEditError("edited tokens do not share a group")


def _lex(text: str) -> list[sql.Token]:
    lexer = Lexer.get_default_instance()
    return [sql.Token(ttype, value)
            for ttype, value in lexer.get_tokens(text)]


class RangeEditor:
    """Apply restricted leaf-token range edits to a token tree."""

    def __init__(self, root: sql.TokenList):
        self.root = root

    def apply(self, edits) -> None:
        """Apply a batch of :class:`EditOp` instances atomically.

        All edits are validated first.  If any edit is invalid, or the
        batch contains overlapping ranges, :class:`RangeEditError` is
        raised and the tree is left completely untouched.
        """
        edits = tuple(edits)
        if not edits:
            return

        spans = _leaf_spans(self.root)
        boundaries = _boundary_set(spans)
        total_length = spans[-1][1] if spans else 0

        raw_ranges = sorted(edit._boundaries() for edit in edits)
        for (prev_start, prev_end), (next_start, next_end) in zip(
                raw_ranges, raw_ranges[1:]):
            if next_start < prev_end:
                raise RangeEditError(
                    "overlapping edit ranges are not allowed in one batch")

        planned = [self._plan(edit, spans, boundaries, total_length)
                   for edit in edits]

        snapshot = copy.deepcopy(self.root.tokens)
        try:
            for plan in sorted(planned, key=lambda item: item[0],
                               reverse=True):
                self._execute(plan)
        except Exception:
            self.root.tokens = snapshot
            self._reattach(self.root)
            self._refresh_values(self.root)
            raise

    def replace(self, start: int, end: int, replacement: str) -> None:
        """Replace the leaf tokens covering ``[start, end)``."""
        self.apply([EditOp.replace(start, end, replacement)])

    def insert(self, at: int, text: str) -> None:
        """Insert ``text`` at the leaf boundary ``at``."""
        self.apply([EditOp.insert(at, text)])

    def delete(self, start: int, end: int) -> None:
        """Delete the leaf tokens covering ``[start, end)``."""
        self.apply([EditOp.delete(start, end)])

    def _plan(self, edit, spans, boundaries, total_length):
        start, end = edit._boundaries()
        if start < 0 or end < 0:
            raise RangeEditError("edit offsets must not be negative")
        if start > end:
            raise RangeEditError("edit start must not be greater than end")
        if end > total_length:
            raise RangeEditError(
                "edit range extends beyond the end of the token tree")
        for boundary in (start, end):
            if boundary not in boundaries:
                raise RangeEditError(
                    "edit boundary does not align with a leaf token; "
                    "splitting a leaf token is not supported")

        if edit.is_insert:
            positions = _insert_positions(self.root, spans, start)
            if not positions:
                # Empty tree or an insertion at the very end.
                group = self.root
                index = len(group.tokens)
            else:
                group = self._deepest(positions)
                index = positions[group]
            affected = []
        else:
            affected = [token for lo, hi, token in spans
                        if lo >= start and hi <= end]
            if not affected:
                raise RangeEditError("edit range does not cover any token")
            group = _deepest_common_group(affected[0], affected[-1])
            if not self._same_direct_group(affected, group):
                raise RangeEditError(
                    "edit range crosses a group boundary; restructuring "
                    "across groups is not supported")
            indices = {group.token_index(token) for token in affected}
            if indices != set(range(min(indices), max(indices) + 1)):
                raise RangeEditError(
                    "edit range crosses a group boundary; restructuring "
                    "across groups is not supported")
            index = min(indices)

        new_tokens = tuple(edit.tokens) if edit.tokens is not None else (
            tuple(_lex(edit.replacement)))
        for token in new_tokens:
            if token.is_group:
                raise RangeEditError(
                    "replacement tokens must be leaf tokens, not groups")
        return start, end, group, index, affected, list(new_tokens), edit

    @staticmethod
    def _same_direct_group(affected, group) -> bool:
        return all(token.parent is group for token in affected)

    @staticmethod
    def _deepest(positions: dict[sql.TokenList, int]) -> sql.TokenList:
        deepest = None
        depth = -1
        for group in positions:
            group_depth = 0
            node = group
            while node is not None:
                group_depth += 1
                node = node.parent
            if group_depth > depth:
                depth = group_depth
                deepest = group
        return deepest

    def _execute(self, plan) -> None:
        start, end, group, index, affected, new_tokens, edit = plan
        for token in new_tokens:
            token.parent = group
        if edit.is_insert:
            group.tokens[index:index] = new_tokens
        else:
            stop = index + len(affected)
            group.tokens[index:stop] = new_tokens
        self._refresh_values(group)

    @staticmethod
    def _reattach(group: sql.TokenList) -> None:
        for token in group.tokens:
            token.parent = group
            if token.is_group:
                RangeEditor._reattach(token)

    @staticmethod
    def _refresh_values(group: sql.TokenList) -> None:
        node = group
        while node is not None:
            node.value = "".join(token.value for token in node.flatten())
            node = node.parent


def check_structure(root: sql.TokenList) -> list[str]:
    """Inspect the cached structure of a token tree.

    Edits never re-group a tree, so a group's cached ``value`` or its
    parent links can become stale.  This returns a list of human-readable
    problems; an empty list means the tree is structurally consistent.
    It does not re-parse or re-group anything.
    """
    problems: list[str] = []

    def visit(group: sql.TokenList) -> None:
        for token in group.tokens:
            if token.parent is not group:
                problems.append(
                    f"token {token!r} has a stale parent reference")
            if token.is_group:
                visit(token)
        expected = "".join(token.value for token in group.flatten())
        if group.value != expected:
            problems.append(
                f"{type(group).__name__} value is stale: "
                f"expected {expected!r}, cached {group.value!r}")

    visit(root)
    return problems


def edit_range(root: sql.TokenList, edits) -> None:
    """Convenience wrapper around :class:`RangeEditor`."""
    RangeEditor(root).apply(edits)
