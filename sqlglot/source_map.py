from __future__ import annotations

import enum
import typing as t
from dataclasses import dataclass

if t.TYPE_CHECKING:
    from sqlglot.expressions.core import Expr

SOURCE_META_KEY: str = "sqlglot.source"
"""Meta key holding the originating source range ``(start, end)`` of a parsed node."""


class SourceMapKind(enum.Enum):
    """
    Describes how a node relates back to the original SQL text.

    Attributes:
        IDENTITY: the node maps to a single, contiguous source fragment.
        COMPOSITE: the node is generated from multiple source fragments, which may be
            non-contiguous, so each fragment is kept as its own range instead of being
            merged into one imprecise span.
        SYNTHETIC: the node has no direct source (e.g. it was introduced by an
            optimization or built programmatically).
    """

    IDENTITY = "identity"
    COMPOSITE = "composite"
    SYNTHETIC = "synthetic"


@dataclass(frozen=True)
class SourceRange:
    """
    A half-open range (``start`` inclusive, ``end`` exclusive) over the original SQL text.

    Offsets are Unicode code point indices, i.e. Python string indices. The same unit is
    used across comments, non-ASCII characters and every statement of a multi-statement
    input, so ranges stay valid when slicing the original string directly.
    """

    start: int
    end: int
    line: int
    col: int

    @property
    def length(self) -> int:
        return self.end - self.start


@dataclass(frozen=True)
class SourceMap:
    """
    The source mapping of a single expression node.

    Attributes:
        expression: the mapped node.
        kind: how the node relates to the source (see `SourceMapKind`).
        ranges: the originating source fragments, in ascending order. Empty for
            synthetic nodes, a single range for identity nodes, and one range per
            distinct fragment for composite nodes.
    """

    expression: Expr
    kind: SourceMapKind
    ranges: tuple[SourceRange, ...] = ()

    @property
    def start(self) -> int | None:
        return self.ranges[0].start if self.ranges else None

    @property
    def end(self) -> int | None:
        return self.ranges[-1].end if self.ranges else None


def merge_ranges(ranges: t.Iterable[SourceRange]) -> tuple[SourceRange, ...]:
    """
    Merges overlapping or touching ranges while preserving genuine gaps.

    A gap between two ranges means they came from distinct, non-contiguous fragments;
    such fragments are kept separate instead of being hidden inside one large range.
    """
    ordered = sorted(ranges, key=lambda r: (r.start, r.end))
    merged: list[SourceRange] = []
    for rng in ordered:
        if merged and rng.start <= merged[-1].end:
            last = merged[-1]
            if rng.end > last.end:
                merged[-1] = SourceRange(last.start, rng.end, last.line, last.col)
        else:
            merged.append(rng)
    return tuple(merged)


def source_map(expression: Expr) -> SourceMap:
    """
    Computes the source mapping of a single expression node.

    Nodes tagged while parsing report their own contiguous range (identity). Nodes built
    by transformations derive their mapping from the tagged descendants: a single merged
    range is an identity, several ranges denote a composite, and the absence of any tagged
    descendant marks the node synthetic. Newly constructed nodes never inherit a neighbor's
    position because their origin lives in a dedicated meta key, separate from positions.

    Example:
        >>> import sqlglot
        >>> from sqlglot import exp
        >>> tree = sqlglot.parse_one("SELECT a + 1 FROM t", track_sources=True)
        >>> source_map(tree.find(exp.Add)).kind
        <SourceMapKind.COMPOSITE: 'composite'>
    """
    own = expression.meta_get(SOURCE_META_KEY)
    if own is not None:
        return SourceMap(
            expression=expression,
            kind=SourceMapKind.IDENTITY,
            ranges=(_identity_range(expression, own),),
        )

    ranges = merge_ranges(
        _identity_range(node, node.meta_get(SOURCE_META_KEY))
        for node in expression.walk()
        if node.meta_get(SOURCE_META_KEY) is not None
    )

    if not ranges:
        return SourceMap(expression=expression, kind=SourceMapKind.SYNTHETIC)

    kind = SourceMapKind.IDENTITY if len(ranges) == 1 else SourceMapKind.COMPOSITE
    return SourceMap(expression=expression, kind=kind, ranges=ranges)


def build_source_map(expression: Expr) -> dict[int, SourceMap]:
    """
    Builds a source mapping for every node in the tree, keyed by ``id(node)``.

    The mapping is derived on demand and references the tree's nodes directly, so it stays
    valid after copying the AST (the copy has its own node ids and carries the same meta).
    """
    return {id(node): source_map(node) for node in expression.walk()}


def _identity_range(node: Expr, span: tuple[int, int]) -> SourceRange:
    start, end = span
    return SourceRange(start, end, node.meta_get("line", 1), node.meta_get("col", 1))
