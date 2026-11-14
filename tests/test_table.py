import pytest

from parsel import Selector, SelectorList
from parsel.table import TableGrid, parse_table


def make_grid(body: str, query: str = "table") -> TableGrid:
    return parse_table(Selector(text=body).css(query))


class TestSimpleTable:
    def test_grid_dimensions_and_values(self):
        grid = make_grid(
            "<table>"
            "<tr><td>a</td><td>b</td></tr>"
            "<tr><td>c</td><td>d</td></tr>"
            "</table>"
        )
        assert grid.nrows == 2
        assert grid.ncols == 2
        assert grid.values() == [["a", "b"], ["c", "d"]]
        assert grid.diagnostics == []

    def test_source_coordinates(self):
        grid = make_grid(
            "<table>"
            "<tr><td>a</td><td>b</td></tr>"
            "<tr><td>c</td><td>d</td></tr>"
            "</table>"
        )
        assert [(cell.row, cell.col) for row in grid for cell in row if cell] == [
            (0, 0),
            (0, 1),
            (1, 0),
            (1, 1),
        ]

    def test_direct_rows_form_an_implicit_tbody(self):
        grid = make_grid("<table><tr><td>a</td></tr></table>")
        assert len(grid.groups) == 1
        group = grid.groups[0]
        assert group.kind == "tbody"
        assert group.implicit
        assert (group.start, group.end) == (0, 1)


class TestMixedSpans:
    HTML = (
        "<table>"
        '<tr><th colspan="2">A</th><th rowspan="2">B</th></tr>'
        "<tr><td>1</td><td>2</td></tr>"
        '<tr><td colspan="2">3</td><td>4</td></tr>'
        "</table>"
    )

    def test_grid_width_and_values(self):
        grid = make_grid(self.HTML)
        assert (grid.nrows, grid.ncols) == (3, 3)
        assert grid.values() == [
            ["A", "A", "B"],
            ["1", "2", "B"],
            ["3", "3", "4"],
        ]
        assert grid.diagnostics == []

    def test_covered_positions_keep_source_cell_identity(self):
        grid = make_grid(self.HTML)
        assert grid.rows[0][1] is grid.rows[0][0]
        assert grid.rows[1][2] is grid.rows[0][2]
        assert grid.rows[2][1] is grid.rows[2][0]
        # Origins keep their own coordinates everywhere they appear.
        assert (grid.rows[0][1].row, grid.rows[0][1].col) == (0, 0)
        assert (grid.rows[1][2].row, grid.rows[1][2].col) == (0, 2)
        assert grid.rows[0][0].colspan == 2
        assert grid.rows[0][2].rowspan == 2

    def test_cell_order_is_document_order(self):
        grid = make_grid(self.HTML)
        seen = []
        for row in grid:
            for cell in row:
                if cell is not None and cell not in seen:
                    seen.append(cell)
        assert [cell.text for cell in seen] == ["A", "B", "1", "2", "3", "4"]

    def test_values_with_placeholder_for_covered_positions(self):
        grid = make_grid(self.HTML)
        assert grid.values(repeat=False, placeholder=".") == [
            ["A", ".", "B"],
            ["1", "2", "."],
            ["3", ".", "4"],
        ]

    def test_header_cells(self):
        grid = make_grid(self.HTML)
        assert grid.rows[0][0].header
        assert grid.rows[0][2].header
        assert not grid.rows[1][0].header


class TestNestedTables:
    HTML = (
        '<table id="outer">'
        "<tr><td>o1</td><td>"
        '<table id="inner">'
        "<tr><td>i1</td><td>i2</td></tr>"
        "<tr><td>i3</td><td>i4</td></tr>"
        "</table>"
        "</td></tr>"
        "<tr><td>o2</td><td>o3</td></tr>"
        "</table>"
    )

    def test_nested_rows_are_not_counted(self):
        grid = make_grid(self.HTML, "#outer")
        assert (grid.nrows, grid.ncols) == (2, 2)
        assert grid.values() == [["o1", "i1 i2 i3 i4"], ["o2", "o3"]]
        assert grid.diagnostics == []

    def test_nested_table_can_be_parsed_on_its_own(self):
        selector = Selector(text=self.HTML)
        grid = parse_table(selector.css("#inner"))
        assert grid.values() == [["i1", "i2"], ["i3", "i4"]]
        assert grid.diagnostics == []

    def test_dom_is_not_modified(self):
        selector = Selector(text=self.HTML)
        before = selector.css("#outer").get()
        parse_table(selector.css("#outer"))
        assert selector.css("#outer").get() == before


class TestRowspanZero:
    def test_scope_is_limited_to_the_owning_row_group(self):
        grid = make_grid(
            "<table>"
            "<thead><tr><th>h1</th><th>h2</th></tr></thead>"
            "<tbody>"
            '<tr><td rowspan="0">x</td><td>1</td></tr>'
            "<tr><td>2</td></tr>"
            "</tbody>"
            "<tfoot><tr><td>f1</td><td>f2</td></tr></tfoot>"
            "</table>"
        )
        assert (grid.nrows, grid.ncols) == (4, 2)
        cell = grid.rows[1][0]
        assert cell is grid.rows[2][0]
        assert cell.rowspan == 2  # the two tbody rows, not the tfoot row
        assert grid.values() == [
            ["h1", "h2"],
            ["x", "1"],
            ["x", "2"],
            ["f1", "f2"],
        ]
        assert grid.diagnostics == []

    def test_scope_in_an_implicit_row_group(self):
        grid = make_grid(
            "<table>"
            '<tr><td rowspan="0">a</td><td>1</td></tr>'
            "<tbody><tr><td>b</td><td>2</td></tr></tbody>"
            "</table>"
        )
        # The implicit tbody has a single row, so rowspan="0" spans 1 row.
        assert grid.values() == [["a", "1"], ["b", "2"]]
        assert grid.diagnostics == []


class TestInvalidSpans:
    def test_non_integer_and_out_of_range_values_fall_back_to_one(self):
        grid = make_grid(
            "<table>"
            '<tr><td rowspan="x">a</td><td colspan="0">b</td>'
            '<td colspan="-3">c</td></tr>'
            "<tr><td>d</td><td>e</td><td>f</td></tr>"
            "</table>"
        )
        assert grid.values() == [["a", "b", "c"], ["d", "e", "f"]]
        assert [d.kind for d in grid.diagnostics] == [
            "invalid-rowspan",
            "invalid-colspan",
            "invalid-colspan",
        ]
        assert [(d.row, d.col) for d in grid.diagnostics] == [(0, 0), (0, 1), (0, 2)]

    def test_rowspan_is_clipped_at_the_row_group_boundary(self):
        grid = make_grid(
            "<table><tbody>"
            '<tr><td rowspan="5">a</td></tr>'
            "<tr><td>b</td></tr>"
            "</tbody></table>"
        )
        cell = grid.rows[0][0]
        assert cell.rowspan == 2
        assert cell is grid.rows[1][0]
        assert grid.values() == [["a", None], ["a", "b"]]
        kinds = [d.kind for d in grid.diagnostics]
        assert kinds.count("invalid-rowspan") == 1
        assert kinds.count("missing-cell") == 1

    def test_spans_are_clamped_to_the_html_limits(self):
        grid = make_grid(
            "<table><tr>"
            '<td colspan="2000">a</td><td rowspan="70000">b</td>'
            "</tr></table>"
        )
        assert grid.rows[0][0].colspan == 1000
        assert grid.rows[0][1000].rowspan == 1  # clipped to the single row
        assert [d.kind for d in grid.diagnostics] == [
            "invalid-colspan",
            "invalid-rowspan",
            "invalid-rowspan",
        ]


class TestOverlap:
    HTML = (
        "<table>"
        '<tr><td rowspan="2">a</td><td>b</td></tr>'
        '<tr><td rowspan="2">c</td></tr>'
        '<tr><td colspan="2">d</td></tr>'
        "</table>"
    )

    def test_earlier_cell_keeps_the_contested_slot(self):
        grid = make_grid(self.HTML)
        assert grid.values() == [["a", "b"], ["a", "c"], ["d", "c"]]
        assert grid.rows[2][0].text == "d"
        assert grid.rows[2][1].text == "c"

    def test_overlap_is_diagnosed(self):
        grid = make_grid(self.HTML)
        assert len(grid.diagnostics) == 1
        diagnostic = grid.diagnostics[0]
        assert diagnostic.kind == "overlap"
        assert (diagnostic.row, diagnostic.col) == (2, 1)
        assert "(2, 0)" in diagnostic.message  # the overlapping cell origin
        assert "(1, 1)" in diagnostic.message  # the cell keeping the slot


class TestMissingCells:
    def test_short_rows_are_padded(self):
        grid = make_grid(
            "<table>"
            "<tr><td>a</td><td>b</td></tr>"
            "<tr><td>c</td></tr>"
            "</table>"
        )
        assert (grid.nrows, grid.ncols) == (2, 2)
        assert grid.rows[1][1] is None
        assert grid.values() == [["a", "b"], ["c", None]]
        assert grid.values(placeholder="") == [["a", "b"], ["c", ""]]
        assert [d.kind for d in grid.diagnostics] == ["missing-cell"]
        assert (grid.diagnostics[0].row, grid.diagnostics[0].col) == (1, 1)

    def test_uncovered_interior_positions_are_reported(self):
        grid = make_grid(
            "<table>"
            '<tr><td rowspan="2">a</td><td>b</td><td rowspan="2">c</td></tr>'
            "<tr></tr>"
            "</table>"
        )
        assert grid.values() == [["a", "b", "c"], ["a", None, "c"]]
        assert [d.kind for d in grid.diagnostics] == ["missing-cell"]
        assert (grid.diagnostics[0].row, grid.diagnostics[0].col) == (1, 1)


class TestRowGroups:
    def test_groups_and_header_rows(self):
        grid = make_grid(
            "<table>"
            "<thead><tr><th>h</th></tr></thead>"
            "<tbody><tr><td>a</td></tr><tr><td>b</td></tr></tbody>"
            "<tfoot><tr><td>f</td></tr></tfoot>"
            "</table>"
        )
        assert [(g.kind, g.start, g.end, g.implicit) for g in grid.groups] == [
            ("thead", 0, 1, False),
            ("tbody", 1, 3, False),
            ("tfoot", 3, 4, False),
        ]
        assert grid.header_rows == [0]
        assert grid.values() == [["h"], ["a"], ["b"], ["f"]]

    def test_consecutive_direct_rows_merge_into_one_implicit_group(self):
        grid = make_grid(
            "<table>"
            "<tr><td>a</td></tr>"
            "<tr><td>b</td></tr>"
            "<tbody><tr><td>c</td></tr></tbody>"
            "<tr><td>d</td></tr>"
            "</table>"
        )
        assert [(g.kind, g.start, g.end, g.implicit) for g in grid.groups] == [
            ("tbody", 0, 2, True),
            ("tbody", 2, 3, False),
            ("tbody", 3, 4, True),
        ]
        assert grid.values() == [["a"], ["b"], ["c"], ["d"]]


class TestCellText:
    def test_text_is_whitespace_normalized(self):
        grid = make_grid("<table><tr><td>  a\n b\t c  </td></tr></table>")
        assert grid.rows[0][0].text == "a b c"


class TestInputValidation:
    def test_non_selector_input_is_rejected(self):
        with pytest.raises(TypeError, match="expects a Selector"):
            parse_table("<table></table>")

    def test_non_table_element_is_rejected(self):
        with pytest.raises(ValueError, match="<table> element"):
            parse_table(Selector(text="<div>x</div>").css("div"))

    def test_empty_selection_is_rejected(self):
        with pytest.raises(ValueError, match="single <table> element"):
            parse_table(Selector(text="<p>a</p>").css("table"))

    def test_multiple_tables_are_rejected(self):
        with pytest.raises(ValueError, match="single <table> element"):
            parse_table(
                Selector(text="<table></table><table></table>").css("table")
            )

    def test_selector_list_with_a_single_table_is_accepted(self):
        selector_list = Selector(text="<table><tr><td>a</td></tr></table>").css(
            "table"
        )
        assert isinstance(selector_list, SelectorList)
        assert parse_table(selector_list).values() == [["a"]]