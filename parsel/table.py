"""HTML table extraction into a normalized rectangular grid.

The :func:`parse_table` function expands ``rowspan`` and ``colspan``
attributes of an HTML table into a rectangular grid. Every grid position
points to the *source* :class:`TableCell` that covers it, so positions
covered by a spanning cell keep the identity of that cell; callers decide
whether to repeat its value or use a placeholder via
:meth:`TableGrid.values`.

Only the direct row groups (``thead``, ``tbody``, ``tfoot``) and direct
``tr`` children of the selected table are taken into account; rows that
belong to nested tables are never included. Consecutive direct ``tr``
children are treated as a single implicit ``tbody`` row group.

Policies for malformed tables (each one reported through
:attr:`TableGrid.diagnostics`):

* non-integer, negative or zero ``colspan`` values, and non-integer or
  negative ``rowspan`` values, are treated as ``1``
  (``invalid-colspan`` / ``invalid-rowspan``);
* ``rowspan="0"`` extends the cell to the last row of its *own row
  group*, as defined by the HTML standard;
* a ``rowspan`` that would cross the boundary of its row group is
  clipped to the end of that group (``invalid-rowspan``);
* spans are clamped to the limits of the HTML standard (1000 columns,
  65534 rows) (``invalid-colspan`` / ``invalid-rowspan``);
* when a cell would cover a slot already covered by a previously placed
  cell, the earlier cell keeps the slot and the overlapping slot of the
  later cell is skipped (``overlap``);
* grid positions not covered by any cell (ragged rows) are filled with
  ``None`` (``missing-cell``).

The source DOM is never modified.

>>> from parsel import Selector
>>> from parsel.table import parse_table
>>> selector = Selector(text='''
... <table>
...   <tr><th>Name</th><th>Price</th></tr>
...   <tr><td rowspan="2">Apple</td><td>1.0</td></tr>
...   <tr><td>1.5</td></tr>
... </table>
... ''')
>>> grid = parse_table(selector.css("table"))
>>> grid.values()
[['Name', 'Price'], ['Apple', '1.0'], ['Apple', '1.5']]
>>> grid.values(repeat=False, placeholder="")
[['Name', 'Price'], ['Apple', '1.0'], ['', '1.5']]
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Literal, TypeAlias, cast

from .selector import Selector, SelectorList

if TYPE_CHECKING:
    from collections.abc import Iterator

__all__ = [
    "DiagnosticKind",
    "TableCell",
    "TableDiagnostic",
    "TableGrid",
    "TableRowGroup",
    "parse_table",
]

DiagnosticKind: TypeAlias = Literal[
    "invalid-rowspan",
    "invalid-colspan",
    "overlap",
    "missing-cell",
]

#: Maximum accepted ``colspan`` value, as defined by the HTML standard.
MAX_COLSPAN = 1000
#: Maximum accepted ``rowspan`` value, as defined by the HTML standard.
MAX_ROWSPAN = 65534

_RowGroupKind: TypeAlias = Literal["thead", "tbody", "tfoot"]


@dataclass
class TableCell:
    """A source cell (``th`` or ``td``) of a parsed table.

    The same :class:`TableCell` object appears in every grid position
    covered by the cell, so ``row`` and ``col`` always hold the
    coordinates of the top-left (origin) position of the cell.
    """

    #: Selector for the cell element.
    selector: Selector
    #: Row index of the origin of the cell within the grid.
    row: int
    #: Column index of the origin of the cell within the grid.
    col: int
    #: Number of grid rows covered by the cell.
    rowspan: int
    #: Number of grid columns covered by the cell.
    colspan: int
    #: Whether the cell is a ``th`` (header) cell.
    header: bool

    @property
    def text(self) -> str:
        """Whitespace-normalized text of the cell.

        Text nodes are stripped and joined with a single space. Text of
        nested tables, if any, is included.
        """
        return " ".join(
            part
            for text in self.selector.xpath(".//text()").getall()
            for part in text.split()
        )


@dataclass
class TableRowGroup:
    """A row group of a parsed table.

    Direct ``tr`` children of the table are reported as consecutive
    implicit ``tbody`` groups (``implicit`` is ``True``).
    """

    #: One of ``"thead"``, ``"tbody"`` or ``"tfoot"``.
    kind: _RowGroupKind
    #: Index of the first grid row of the group.
    start: int
    #: Index one past the last grid row of the group.
    end: int
    #: Whether the group is an implicit ``tbody`` (direct ``tr`` children).
    implicit: bool = False


@dataclass
class TableDiagnostic:
    """A non-fatal issue found while parsing a table."""

    #: One of ``"invalid-rowspan"``, ``"invalid-colspan"``, ``"overlap"``
    #: or ``"missing-cell"``.
    kind: DiagnosticKind
    #: Human-readable description of the issue.
    message: str
    #: Grid row the issue relates to, if any.
    row: int | None = None
    #: Grid column the issue relates to, if any.
    col: int | None = None


@dataclass
class TableGrid:
    """A rectangular grid of table cells.

    ``rows[r][c]`` is the source :class:`TableCell` covering position
    ``(r, c)``, or ``None`` if no cell covers it. Positions covered by a
    spanning cell reference the same :class:`TableCell` object as the
    origin position of that cell.
    """

    #: Grid rows; each position holds its source cell or ``None``.
    rows: list[list[TableCell | None]]
    #: Row groups of the table, in document order.
    groups: list[TableRowGroup]
    #: Non-fatal issues found while parsing the table.
    diagnostics: list[TableDiagnostic]

    @property
    def nrows(self) -> int:
        """Number of grid rows."""
        return len(self.rows)

    @property
    def ncols(self) -> int:
        """Number of grid columns."""
        return len(self.rows[0]) if self.rows else 0

    @property
    def header_rows(self) -> list[int]:
        """Indices of the grid rows that belong to a ``thead`` group."""
        return [
            row
            for group in self.groups
            if group.kind == "thead"
            for row in range(group.start, group.end)
        ]

    def cell(self, row: int, col: int) -> TableCell | None:
        """Return the source cell covering ``(row, col)``, or ``None``."""
        return self.rows[row][col]

    def values(self, *, repeat: bool = True, placeholder: Any = None) -> list[list[Any]]:
        """Return the grid as a list of rows of cell texts.

        If ``repeat`` is ``True`` (default), positions covered by a
        spanning cell repeat the text of the source cell; otherwise they
        are replaced with ``placeholder``. Positions not covered by any
        cell always use ``placeholder``.
        """
        return [
            [
                cell.text
                if cell is not None and (repeat or (cell.row == r and cell.col == c))
                else placeholder
                for c, cell in enumerate(row)
            ]
            for r, row in enumerate(self.rows)
        ]

    def __iter__(self) -> Iterator[list[TableCell | None]]:
        return iter(self.rows)


def _parse_span(
    raw: str | None,
    *,
    attribute: Literal["rowspan", "colspan"],
    row: int,
    col: int,
    diagnostics: list[TableDiagnostic],
) -> int:
    """Return a validated span value, reporting issues in ``diagnostics``.

    ``rowspan="0"`` is returned as ``0``; scoping it to its row group is
    left to the caller.
    """
    kind: DiagnosticKind = (
        "invalid-rowspan" if attribute == "rowspan" else "invalid-colspan"
    )
    if raw is None:
        return 1
    try:
        value = int(raw.strip())
    except ValueError:
        diagnostics.append(
            TableDiagnostic(
                kind,
                f"{attribute}={raw!r} is not an integer, using 1",
                row,
                col,
            )
        )
        return 1
    if attribute == "rowspan" and value == 0:
        return 0
    if value < 1:
        diagnostics.append(
            TableDiagnostic(
                kind,
                f"{attribute}={value} is smaller than 1, using 1",
                row,
                col,
            )
        )
        return 1
    limit = MAX_ROWSPAN if attribute == "rowspan" else MAX_COLSPAN
    if value > limit:
        diagnostics.append(
            TableDiagnostic(
                kind,
                f"{attribute}={value} exceeds the maximum of {limit}, clamped",
                row,
                col,
            )
        )
        return limit
    return value


def parse_table(table: Selector | SelectorList[Selector]) -> TableGrid:
    """Parse a ``table`` element into a normalized rectangular grid.

    ``table`` must be a :class:`~parsel.selector.Selector` (or a
    :class:`~parsel.selector.SelectorList` with a single element) matching
    a ``table`` element. Only the direct row groups and rows of that
    table are processed; rows of nested tables are ignored. The source
    DOM is not modified.

    See the :mod:`parsel.table` module documentation for the policies
    applied to malformed tables.
    """
    if isinstance(table, SelectorList):
        if len(table) != 1:
            raise ValueError(
                f"parse_table() expects a single <table> element, "
                f"got {len(table)}"
            )
        table = table[0]
    if not isinstance(table, Selector):
        raise TypeError(
            f"parse_table() expects a Selector, got {type(table).__name__!r}"
        )
    tag = getattr(table.root, "tag", None)
    if not isinstance(tag, str) or tag.lower() != "table":
        raise ValueError("parse_table() expects a selector for a <table> element")

    # Collect row groups in document order. Direct ``tr`` children are
    # merged into consecutive implicit ``tbody`` groups. Only direct
    # children are considered, so rows of nested tables are excluded.
    groups: list[tuple[_RowGroupKind, bool, list[Selector]]] = []
    for child in table.xpath("./thead|./tbody|./tfoot|./tr"):
        child_tag = str(getattr(child.root, "tag", "")).lower()
        if child_tag == "tr":
            if groups and groups[-1][1]:
                groups[-1][2].append(child)
            else:
                groups.append(("tbody", True, [child]))
        else:
            kind = cast("_RowGroupKind", child_tag)
            groups.append((kind, False, list(child.xpath("./tr"))))

    grid: list[list[TableCell | None]] = []
    diagnostics: list[TableDiagnostic] = []
    row_groups: list[TableRowGroup] = []

    for kind, implicit, row_selectors in groups:
        start = len(grid)
        row_count = len(row_selectors)
        for index, row_selector in enumerate(row_selectors):
            row = start + index
            while len(grid) <= row:
                grid.append([])
            for cell_selector in row_selector.xpath("./th|./td"):
                header = str(getattr(cell_selector.root, "tag", "")).lower() == "th"
                # First grid column of this row not covered by a cell
                # from a previous row.
                slots = grid[row]
                col = 0
                while col < len(slots) and slots[col] is not None:
                    col += 1
                rowspan = _parse_span(
                    cell_selector.attrib.get("rowspan"),
                    attribute="rowspan",
                    row=row,
                    col=col,
                    diagnostics=diagnostics,
                )
                colspan = _parse_span(
                    cell_selector.attrib.get("colspan"),
                    attribute="colspan",
                    row=row,
                    col=col,
                    diagnostics=diagnostics,
                )
                # A cell never spans past its own row group: rowspan="0"
                # means "to the end of the row group" and larger values
                # are clipped.
                group_remaining = row_count - index
                if rowspan == 0:
                    rowspan = group_remaining
                elif rowspan > group_remaining:
                    diagnostics.append(
                        TableDiagnostic(
                            "invalid-rowspan",
                            f"rowspan={rowspan} crosses the boundary of its "
                            f"row group, clipped to {group_remaining}",
                            row,
                            col,
                        )
                    )
                    rowspan = group_remaining
                cell = TableCell(
                    selector=cell_selector,
                    row=row,
                    col=col,
                    rowspan=rowspan,
                    colspan=colspan,
                    header=header,
                )
                for r in range(row, row + rowspan):
                    while len(grid) <= r:
                        grid.append([])
                    r_slots = grid[r]
                    for c in range(col, col + colspan):
                        if c < len(r_slots) and r_slots[c] is not None:
                            existing = r_slots[c]
                            assert existing is not None
                            diagnostics.append(
                                TableDiagnostic(
                                    "overlap",
                                    f"cell at ({row}, {col}) overlaps the "
                                    f"cell originating at "
                                    f"({existing.row}, {existing.col}), "
                                    f"which keeps slot ({r}, {c})",
                                    r,
                                    c,
                                )
                            )
                            continue
                        while len(r_slots) <= c:
                            r_slots.append(None)
                        r_slots[c] = cell
        row_groups.append(
            TableRowGroup(kind=kind, start=start, end=start + row_count, implicit=implicit)
        )

    # Normalize the grid to a rectangle, reporting uncovered positions.
    ncols = max((len(slots) for slots in grid), default=0)
    for row, slots in enumerate(grid):
        for col in range(ncols):
            if col < len(slots):
                if slots[col] is None:
                    diagnostics.append(
                        TableDiagnostic(
                            "missing-cell",
                            f"no cell covers position ({row}, {col})",
                            row,
                            col,
                        )
                    )
            else:
                slots.append(None)
                diagnostics.append(
                    TableDiagnostic(
                        "missing-cell",
                        f"no cell covers position ({row}, {col})",
                        row,
                        col,
                    )
                )

    return TableGrid(rows=grid, groups=row_groups, diagnostics=diagnostics)