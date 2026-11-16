"""Restricted range editing of grouped token trees.

The helpers in this module edit a :class:`~sqlparse.sql.TokenList` in place
by replacing, inserting or deleting *whole leaf tokens*.  The tree is never
flattened back into a string and re-parsed, so the identity of every token
outside the edited range is preserved.

Edits are deliberately conservative:

* every offset must fall on a leaf-token boundary, an edit can never cut
  into the middle of a leaf token;
* an edit range must be contained within a single group, ranges crossing a
  group boundary are rejected instead of being "repaired";
* overlapping edit ranges in one batch are rejected before the tree is
  touched;
* the tree is never re-grouped.  Replacing ``foo(1)`` with ``bar(2)`` keeps
  the surrounding group structure, even if it no longer reflects the
  changed syntax.  Use :func:`check_structure` to inspect whether the
  cached structure is still consistent.

Every edit is validated before the tree is touched and application is
atomic: a failed batch never leaves a partially modified tree behind.
"""

from __future__ import annotations

from bisect import bisect_left
from dataclasses import dataclass, field

from sqlparse import sql
from sqlparse.exceptions import RangeEditError
from sqlparse.lexer import Lexer


@dataclass(frozen=True)
class EditOp:
    """A single leaf-token range edit.

    Offsets are character positions relative to the flattened value of the
    tree that is edited (the value returned by ``str(root)``).

    ``start`` and ``end`` delimit the affected half-open range
    ``[start, end)`` and must both fall on leaf-token boundaries.  For an
    insertion both are ``None`` and ``at`` gives the boundary at which the
    new tokens are inserted (``0`` inserts at the very beginning,
    ``len(str(root))`` at the very end).

    ``replacement`` is the text put in place of the edited range.  An empty
    string therefore turns the operation into a deletion.  It is lexed into
    leaf tokens and is *not* grouped.

    Alternatively ``tokens`` may hold pre-built leaf tokens that replace
    the range.  Such a token must not already be attached to a token tree.
    """

    start: int | None = None
    end: int | None = None
    replacement: str = ""
    at: int | None = None
    tokens: tuple[sql.Token, ...] | None = field(default=None,
                                                  compare=False)

    @classmethod
    def replace(cls, start: int, end: int, replacement: str) -> EditOp:
        """Replace the leaf tokens covering ``[start, end)``."""
        return cls(start=start, end=end, replacement=replacement)

    @classmethod
    def insert(cls, at: int, text: str) -> EditOp:
        """Insert ``text`` at the leaf boundary ``at``."""
        return cls(at=at, replacement=text)

    @classmethod
    def delete(cls, start: int, end: int) -> EditOp:
        """Delete the leaf tokens covering ``[start, end)``."""
        return cls(start=start, end=end, replacement="")

    @property
    def is_insert(self) -> bool:
        """``True`` if this operation inserts text at a single boundary."""
        return self.start is None and self.end is None

    def _boundaries(self) -> tuple[int, int]:
        if self.is_insert:
            if self.at is None:
                raise RangeEditError("insertion requires an 'at' boundary")
            return self.at, self.at
        if self.at is not None:
            raise RangeEditError(
                "an edit takes either 'at' or 'start'/'end', not both")
        return self.start, self.end


@dataclass
class _Plan:
    """A validated edit with the tree position it will be applied to."""

    start: int
    order: int
    is_insert: bool
    group: sql.TokenList
    index: int
    count: int
    new_tokens: tuple[sql.Token, ...]


class _TreeIndex:
    """Leaf offsets and boundaries of a token tree.

    The index is built once per batch and keeps lookups proportional to
    the edit size instead of the whole tree size.
    """

    def __init__(self, root: sql.TokenList):
        self.root = root
        self.spans: list[tuple[int, int, sql.Token]] = []
        self._starts: list[int] = []
        self._at: dict[int, list[tuple[int, sql.Token]]] = {}
        self.boundaries = {0}
        offset = 0
        for token in root.flatten():
            start = offset
            offset += len(token.value)
            self.spans.append((start, offset, token))
            self._starts.append(start)
            self.boundaries.add(start)
            self.boundaries.add(offset)
            self._at.setdefault(start, []).append((0, token))
            self._at.setdefault(offset, []).append((1, token))
        self.length = offset

    def covering(self, start: int, end: int) -> list[sql.Token]:
        """Return the leaf tokens lying completely inside ``[start, end)``."""
        covered = []
        index = bisect_left(self._starts, start)
        while index < len(self.spans):
            lo, hi, token = self.spans[index]
            if lo >= end:
                break
            if hi <= end:
                covered.append(token)
            index += 1
        return covered

    def groups_at(self, offset: int) -> dict[sql.TokenList, int]:
        """Map groups of this tree to their insert index at *offset*.

        A group is included when one of its own direct children starts or
        ends at *offset*.  Groups outside this tree are ignored, so a stale
        parent chain never lets an edit escape the edited subtree.
        """
        positions: dict[sql.TokenList, int] = {}
        for kind, token in self._at.get(offset, ()):
            chain = []
            node = token
            group = token.parent
            while group is not None and group is not self.root:
                chain.append((group, node))
                node = group
                group = group.parent
            if group is not self.root:
                continue
            chain.append((self.root, node))
            for group, node in chain:
                index = _direct_index(group, node)
                positions.setdefault(group, index + kind)
        return positions


def _direct_index(group: sql.TokenList, node: sql.Token) -> int:
    for index, token in enumerate(group.tokens):
        if token is node:
            return index
    raise RangeEditError("token is not a direct child of its parent")


def _depth(group: sql.TokenList) -> int:
    depth = 0
    node = group.parent
    while node is not None:
        depth += 1
        node = node.parent
    return depth


def _depths(groups) -> dict[int, int]:
    """Map ``id(group)`` to its depth for the given groups and ancestors."""
    depths: dict[int, int] = {}
    for group in groups:
        chain = []
        node = group
        while node is not None and id(node) not in depths:
            chain.append(node)
            node = node.parent
        depth = depths[id(node)] if node is not None else -1
        for ancestor in reversed(chain):
            depth += 1
            depths[id(ancestor)] = depth
    return depths


def _deepest_common_group(first: sql.Token, last: sql.Token) -> sql.TokenList:
    """Deepest group that is an ancestor of both leaf tokens."""
    ancestors = set()
    node = first.parent
    while node is not None:
        ancestors.add(id(node))
        node = node.parent

    node = last.parent
    while node is not None:
        if id(node) in ancestors:
            return node
        node = node.parent
    raise RangeEditError("edited tokens do not share a group")


def _insert_target(index: _TreeIndex,
                   offset: int) -> tuple[sql.TokenList, int]:
    """Return the group and index at which text is inserted at *offset*.

    The literal text is the same for every candidate group.  Prefer the
    deepest group that has *interior* room at the boundary, so that text
    inside ``foo.bar`` lands between the name and the dot rather than
    before the whole identifier.  Otherwise the insertion goes to the root,
    which keeps text out of a group that merely ends at the boundary, such
    as a parenthesis just behind its closing bracket.
    """
    positions = index.groups_at(offset)
    if not positions:
        # The tree carries no leaf tokens, e.g. an empty group.
        return index.root, len(index.root.tokens)

    interior = {group: position for group, position in positions.items()
                if 0 < position < len(group.tokens)}
    if interior:
        group = max(interior, key=_depth)
        return group, interior[group]
    return index.root, positions.get(index.root, len(index.root.tokens))


def _lex(text: str) -> list[sql.Token]:
    lexer = Lexer.get_default_instance()
    return [sql.Token(ttype, value)
            for ttype, value in lexer.get_tokens(text)]


def _replacement_tokens(edit: EditOp) -> tuple[sql.Token, ...]:
    if edit.tokens is not None:
        tokens = tuple(edit.tokens)
        for token in tokens:
            if not isinstance(token, sql.Token):
                raise RangeEditError(
                    "replacement tokens must be sqlparse Token instances")
            if token.is_group:
                raise RangeEditError(
                    "replacement tokens must be leaf tokens, not groups")
            if token.parent is not None:
                raise RangeEditError(
                    "replacement token is already attached to a token tree")
        return tokens

    if not isinstance(edit.replacement, str):
        raise RangeEditError("edit replacement must be a string")
    return tuple(_lex(edit.replacement))


def _normalize_ranges(edits) -> list[tuple[int, int]]:
    """Return the raw ranges of *edits*, rejecting malformed offsets."""
    ranges = []
    for edit in edits:
        start, end = edit._boundaries()
        if not isinstance(start, int) or not isinstance(end, int):
            raise RangeEditError("edit offsets must be integers")
        if start < 0 or end < 0:
            raise RangeEditError("edit offsets must not be negative")
        if start > end:
            raise RangeEditError("edit start must not be greater than end")
        ranges.append((start, end))
    return ranges


def _validate_boundaries(ranges, boundaries, length) -> None:
    for start, end in ranges:
        if end > length:
            raise RangeEditError(
                "edit range extends beyond the end of the token tree")
        if start not in boundaries or end not in boundaries:
            raise RangeEditError(
                "edit boundary does not align with a leaf token; "
                "splitting a leaf token is not supported")


def _reject_overlaps(ranges) -> None:
    ordered = sorted(ranges)
    for (_, prev_end), (next_start, _) in zip(ordered, ordered[1:]):
        if next_start < prev_end:
            raise RangeEditError(
                "overlapping edit ranges are not allowed in one batch")


class RangeEditor:
    """Apply restricted leaf-token range edits to a token tree."""

    def __init__(self, root: sql.TokenList):
        if not isinstance(root, sql.TokenList):
            raise TypeError("root must be a TokenList instance")
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
        for edit in edits:
            if not isinstance(edit, EditOp):
                raise RangeEditError("edits must be EditOp instances")

        # Overlap is checked on the raw ranges, before any tree index is
        # built, so a conflicting batch is rejected as early as possible.
        ranges = _normalize_ranges(edits)
        _reject_overlaps(ranges)

        index = _TreeIndex(self.root)
        _validate_boundaries(ranges, index.boundaries, index.length)

        plans = [self._plan(order, edit, index)
                 for order, edit in enumerate(edits)]

        seen = set()
        for plan in plans:
            for token in plan.new_tokens:
                if id(token) in seen:
                    raise RangeEditError(
                        "replacement tokens must not be reused in one batch")
                seen.add(id(token))

        # Apply from the end of the tree towards its beginning.  Edits at
        # higher offsets never invalidate the positions computed for lower
        # ones.  A non-empty replacement is applied before an insertion that
        # starts at the same boundary so the insertion ends up in front of
        # the replacement text.
        ordered = sorted(
            plans,
            key=lambda plan: (
                plan.start,
                0 if plan.is_insert else 1,
                plan.order,
            ),
            reverse=True,
        )

        undo = []
        dirty = {}
        try:
            for plan in ordered:
                self._execute(plan, undo, dirty)
            # Cached values are refreshed once per batch, from the deepest
            # changed group upwards.  Refreshing after every edit would
            # repeat the work for the ancestors shared by several edits.
            self._refresh_values(dirty.values())
        except BaseException:
            self._rollback(undo)
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

    def _plan(self, order, edit: EditOp, index: _TreeIndex) -> _Plan:
        start, end = edit._boundaries()
        new_tokens = _replacement_tokens(edit)

        if edit.is_insert:
            group, position = _insert_target(index, start)
            return _Plan(start=start, order=order, is_insert=True,
                         group=group, index=position, count=0,
                         new_tokens=new_tokens)

        covered = index.covering(start, end)
        if not covered:
            raise RangeEditError("edit range does not cover any token")

        group = _deepest_common_group(covered[0], covered[-1])
        if any(token.parent is not group for token in covered):
            raise RangeEditError(
                "edit range crosses a group boundary; restructuring "
                "across groups is not supported")
        position = _direct_index(group, covered[0])
        stop = position + len(covered)
        if any(child is not expected for child, expected in
               zip(group.tokens[position:stop], covered)):
            raise RangeEditError(
                "edit range crosses a group boundary; restructuring "
                "across groups is not supported")

        return _Plan(start=start, order=order, is_insert=False,
                     group=group, index=position, count=len(covered),
                     new_tokens=new_tokens)

    def _execute(self, plan: _Plan, undo: list, dirty: dict) -> None:
        group = plan.group
        position = plan.index
        count = 0 if plan.is_insert else plan.count
        removed = group.tokens[position:position + count]
        parents = [token.parent for token in removed]
        # A slice assignment is a single operation and keeps the identity of
        # the child list intact.  The inverse is recorded right afterwards,
        # so a failure at any later point can be rolled back.
        group.tokens[position:position + count] = plan.new_tokens
        undo.append((group, position, removed, parents,
                     len(plan.new_tokens)))
        for token in plan.new_tokens:
            token.parent = group
        for token in removed:
            token.parent = None
        node = group
        while node is not None:
            dirty[id(node)] = node
            node = node.parent

    @staticmethod
    def _refresh_values(groups) -> None:
        # Recompute cached values from the deepest group upwards so that
        # every parent sees the updated values of its children.  A group's
        # value is the concatenation of its children's values; that keeps
        # this linear in the tree size instead of rescanning leaf subtrees.
        # Depths are resolved once so the sort never re-walks a chain.
        groups = list(groups)
        depths = _depths(groups)
        ordered = sorted(groups, key=lambda node: depths[id(node)],
                         reverse=True)
        for node in ordered:
            node.value = "".join(token.value for token in node.tokens)

    def _rollback(self, undo) -> None:
        dirty = {}
        for group, position, removed, parents, inserted in reversed(undo):
            dropped = group.tokens[position:position + inserted]
            group.tokens[position:position + inserted] = removed
            for token in dropped:
                token.parent = None
            for token, parent in zip(removed, parents):
                token.parent = parent
            node = group
            while node is not None:
                dirty[id(node)] = node
                node = node.parent
        self._refresh_values(dirty.values())


def check_structure(root: sql.TokenList) -> list[str]:
    """Inspect the cached structure of a token tree.

    Edits never re-group a tree, so a group's cached ``value`` or its
    parent links can become stale.  This returns a list of human-readable
    problems; an empty list means the tree is structurally consistent.
    It does not re-parse or re-group anything.
    """
    if not isinstance(root, sql.TokenList):
        raise TypeError("root must be a TokenList instance")

    problems: list[str] = []
    # Iterative post-order walk: a group is checked after its subgroups so
    # that every real value is computed exactly once.
    stack = [(root, False)]
    real_values: dict[int, str] = {}
    seen: set[int] = set()
    while stack:
        group, loaded = stack.pop()
        if not loaded:
            if id(group) in seen:
                problems.append(
                    f"{type(group).__name__} is referenced more than once")
                continue
            seen.add(id(group))
            stack.append((group, True))
            for token in group.tokens:
                if token.is_group:
                    stack.append((token, False))
            continue

        for token in group.tokens:
            if token.parent is not group:
                problems.append(
                    f"token {token!r} has a stale parent reference")
        expected = "".join(
            real_values.get(id(token), token.value)
            if token.is_group else token.value
            for token in group.tokens)
        real_values[id(group)] = expected
        if group.value != expected:
            problems.append(
                f"{type(group).__name__} value is stale: "
                f"expected {expected!r}, cached {group.value!r}")
    return problems


def edit_range(root: sql.TokenList, edits) -> None:
    """Apply *edits* to *root*, see :class:`RangeEditor`."""
    RangeEditor(root).apply(edits)
