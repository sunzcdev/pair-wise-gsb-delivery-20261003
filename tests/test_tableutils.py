from boltons.tableutils import (Table, FoldMarker, DepthLimitFoldMarker,
                                EntryLimitFoldMarker,
                                SharedReferenceFoldMarker, CycleFoldMarker)
from html import escape as html_escape


def test_table_lists():
    data_lists = [['id', 'name'],
                  [1, 'John Doe'],
                  [2, 'Dale Simmons']]
    t1 = Table(data_lists)
    assert set(t1.headers) == {'id', 'name'}
    assert len(t1) == 2
    assert 'John Doe' in repr(t1)

T2_REF_HTML = """<table>
<tr><th>id</th><td>1</td></tr>
<tr><th>name</th><td>John Doe</td></tr>
</table>"""

T3_REF_HTML = """<table>
<thead>
<tr><th>id</th><th>name</th></tr>
</thead>
<tbody>
<tr><td>1</td><td>John Doe</td></tr>
<tr><td>2</td><td>Dale Simmons</td></tr>
<tr><td>3</td><td>Kurt Rose</td></tr>
<tr><td>4</td><td>None</td></tr>
</tbody>
</table>"""


def test_table_dicts():
    data_dicts = [{'id': 1, 'name': 'John Doe'},
                  {'id': 2, 'name': 'Dale Simmons'}]
    t2 = Table.from_dict(data_dicts[0])
    t3 = Table.from_dict(data_dicts)
    t3.extend([[3, 'Kurt Rose'], [4]])

    assert set(t2.headers) == {'id', 'name'}
    assert len(t2) == 1
    # the sorted() stuff handles ordering differences between versions
    # TODO: should maybe change Table to sort the headers of dicts and such?
    assert sorted(t2.to_html()) == sorted(T2_REF_HTML)
    assert sorted(t3.to_html()) == sorted(T3_REF_HTML)
    assert t3.to_text()


def test_table_obj():
    class TestType:
        def __init__(self):
            self.greeting = 'hi'

    t4 = Table.from_object(TestType())
    assert len(t4) == 1
    assert 'greeting' in t4.headers



def test_to_text_empty_table():
    # used to raise ValueError from max() over an empty sequence
    assert Table([[]]).to_text() == ''


def test_to_text_none_headers():
    # used to raise AttributeError calling .center() on a None header
    t = Table([[None, 'b'], [1, 2]])
    lines = t.to_text().splitlines()
    header_cells = [c.strip() for c in lines[0].split(' | ')]
    # None headers render the same as None data cells
    assert header_cells[0] == 'None'


def test_to_text_no_headers():
    # used to raise IndexError; empty headers mean no header row or
    # separator rule, same as to_html
    t = Table([[], [1]])
    assert t.headers == []
    assert t.to_text() == '1'


def test_to_text_short_headers_padded():
    # used to raise IndexError; non-empty headers shorter than the
    # table width get padded with None cells, same as to_html
    t = Table([[1, 2]], headers=[])
    t.headers = ['a']
    lines = t.to_text().splitlines()
    header_cells = [c.strip() for c in lines[0].split(' | ')]
    assert header_cells == ['a', 'None']


def test_iterator_headers():
    # see #561
    t = Table([[1, 2]], headers=iter(['a', 'b']))
    assert t.headers == ['a', 'b']
    assert t.to_text().splitlines()[0] == 'a | b'

    t = Table.from_data([[1, 2]], headers=iter(['a', 'b']))
    assert t.headers == ['a', 'b']
    assert t.to_text().splitlines()[-1] == '1 | 2'


def test_headerless_extend_wider_row():
    # see #497
    t = Table([[1]], headers=[])
    t.extend([[2, 3]])
    assert t.to_text().splitlines() == ['1 | None', '2 |  3  ']


def test_to_html_bottom_metadata_after_table():
    # see #558
    t = Table([[1, 2]], headers=['a', 'b'], metadata={'k': 'v'})
    html = t.to_html(with_metadata='bottom')
    main_close = html.index('</table>')
    assert main_close < html.index('<br />') < html.index('<th>k</th>')
    assert html.endswith('</table>')
    assert html.count('<table>') == html.count('</table>') == 2


# ---------------------------------------------------------------------------
# bounded preview: depth/entry budgets, typed fold markers, cycles and
# shared references. See tableutils.FoldMarker.
# ---------------------------------------------------------------------------

def test_depth_limit_emits_typed_marker_with_path():
    t = Table.from_data([{'a': {'b': {'c': 1}}}], max_depth=2)
    markers = t.fold_markers
    assert len(markers) == 1
    marker = markers[0]
    assert isinstance(marker, DepthLimitFoldMarker)
    assert isinstance(marker, FoldMarker)
    assert marker.type_name == 'dict'
    assert marker.path == (0, 'a', 'b')
    text = str(marker)
    assert 'depth-limit' in text and 'dict' in text and '$[0].a.b' in text
    # the marker is an object, never a plain string cell
    assert not isinstance(marker, str)
    # the unrendered level's data must not appear as a real cell: the
    # folded leaf value 1 is replaced by the marker object
    def _cells(table):
        for row in table:
            for cell in row:
                if isinstance(cell, Table):
                    yield from _cells(cell)
                else:
                    yield cell

    assert all(cell != 1 for cell in _cells(t))


def test_depth_limit_marker_in_both_renderers():
    t = Table.from_data([{'a': {'b': {'c': 1}}}], max_depth=2)
    marker = t.fold_markers[0]
    text_out = t.to_text()
    html_out = t.to_html(max_depth=2)
    assert str(marker) in text_out
    assert html_escape(str(marker), quote=True) in html_out
    assert 'fold-marker fold-depth-limit' in html_out


def test_entry_budget_dict_rows():
    t = Table.from_data([{'a': 1, 'b': 2, 'c': 3, 'd': 4}],
                        max_entries=2)
    markers = [m for m in t.fold_markers
               if isinstance(m, EntryLimitFoldMarker)]
    assert len(markers) == 1
    marker = markers[0]
    assert marker.omitted == 2
    assert marker.path == (0,)
    assert marker.type_name == 'dict'
    text = t.to_text()
    # in-budget entries remain real cells
    assert '1' in text and '2' in text
    # out-of-budget entries never show as cells
    assert '3' not in text and '4' not in text
    assert str(marker) in text
    assert html_escape(str(marker), quote=True) in t.to_html()


def test_entry_budget_headerless_sequence():
    t = Table.from_data([[1, 2, 3, 4, 5]], max_entries=3)
    marker = next(m for m in t.fold_markers
                  if isinstance(m, EntryLimitFoldMarker))
    assert marker.omitted == 2
    assert marker.type_name == 'list'
    assert marker.path == (0,)
    cells = [c for row in t for c in row if not isinstance(c, FoldMarker)]
    assert cells == [1, 2, 3]


def test_entry_budget_nested_container():
    t = Table.from_data([{'items': list(range(6))}],
                        max_depth=3, max_entries=4)
    # the nested list folds its own tail; the marker path points at it
    nested_markers = [m for m in t.fold_markers
                      if m.path == (0, 'items')]
    assert len(nested_markers) == 1
    marker = nested_markers[0]
    assert isinstance(marker, EntryLimitFoldMarker)
    assert marker.omitted == 2
    # its first four entries remain reachable as real nested cells
    nested = t[0][0]
    assert isinstance(nested, Table)
    flat = [c for row in nested for c in row
            if not isinstance(c, FoldMarker)]
    assert flat == [0, 1, 2, 3]


def test_entry_budget_pads_uneven_rows():
    rows = [{'a': 1, 'b': 2, 'c': 3}, {'a': 4}]
    t = Table.from_data(rows, max_entries=2)
    # every row is the same width so text rendering cannot IndexError
    assert all(len(row) == t._width for row in t)
    assert t.to_text()  # used to raise IndexError
    assert t.to_html()


def test_max_entries_validation():
    for bad in (0, -1, 1.5, True, '3'):
        try:
            Table.from_data([[1]], max_entries=bad)
        except ValueError:
            continue
        raise AssertionError('expected ValueError for %r' % (bad,))
    # None and positive ints are fine
    Table.from_data([[1]], max_entries=None)
    Table.from_data([[1]], max_entries=1)


def test_cycle_uses_cycle_marker():
    cyclic = []
    cyclic.append(cyclic)
    t = Table.from_data([[cyclic]], max_depth=6)
    markers = [m for m in t.fold_markers if isinstance(m, CycleFoldMarker)]
    assert len(markers) == 1
    marker = markers[0]
    assert marker.type_name == 'list'
    assert marker.cycle_path == (0, 0)
    assert marker.path == (0, 0, 0)
    assert 'cycle' in str(marker)
    # rendering terminates instead of recursing forever
    assert t.to_html(max_depth=6)
    assert t.to_text()


def test_shared_reference_uses_distinct_marker():
    shared = {'x': 1}
    t = Table.from_data([{'k1': shared, 'k2': shared}], max_depth=2)
    shared_markers = [m for m in t.fold_markers
                      if isinstance(m, SharedReferenceFoldMarker)]
    cycle_markers = [m for m in t.fold_markers
                     if isinstance(m, CycleFoldMarker)]
    assert len(shared_markers) == 1
    assert cycle_markers == []
    marker = shared_markers[0]
    assert marker.first_path == (0, 'k1')
    assert marker.path == (0, 'k2')
    assert 'shared-reference' in str(marker)
    # first occurrence is still rendered in full as a nested table
    assert isinstance(t[0][0], Table)


def test_cycle_and_shared_markers_are_distinct_types():
    cyclic = []
    cyclic.append(cyclic)
    t = Table.from_data([[cyclic]], max_depth=4)
    assert any(isinstance(m, CycleFoldMarker) for m in t.fold_markers)
    assert not any(isinstance(m, SharedReferenceFoldMarker)
                   for m in t.fold_markers)

    shared = {}
    t2 = Table.from_data([[shared, shared]], max_depth=2)
    assert any(isinstance(m, SharedReferenceFoldMarker)
               for m in t2.fold_markers)
    assert not any(isinstance(m, CycleFoldMarker)
                   for m in t2.fold_markers)


def test_html_and_text_follow_same_preview_plan():
    shared = {'x': 1}
    data = [{'k1': shared, 'k2': shared, 'k3': {'y': [1, 2, 3, 4]}}]
    t = Table.from_data(data, max_depth=3, max_entries=2)
    html_out = t.to_html(max_depth=3)
    text_out = t.to_text()
    assert t.fold_markers  # plan actually folded something
    for marker in t.fold_markers:
        rendered = str(marker)
        assert rendered in text_out
        assert html_escape(rendered, quote=True) in html_out
    # the markers reachable from cells equal the collected registry
    collected = list(t.fold_markers)
    assert len(collected) == len({id(m) for m in collected})


def test_markers_are_never_plain_cell_values():
    # even data that textually resembles a fold marker stays a real string
    fake = '<fold:depth-limit dict at $[0]>'
    t = Table.from_data([[fake]])
    assert t[0][0] == fake
    assert isinstance(t[0][0], str)
    assert t.fold_markers == []


def test_input_data_is_not_modified():
    import copy

    dict_rows = [{'a': 1, 'b': 2, 'c': 3, 'd': 4}]
    snapshot = copy.deepcopy(dict_rows)
    Table.from_data(dict_rows, max_entries=2)
    assert dict_rows == snapshot

    list_rows = [[1, 2, 3, 4, 5]]
    snapshot = copy.deepcopy(list_rows)
    Table.from_data(list_rows, max_entries=3)
    assert list_rows == snapshot

    # default (unbounded) import also leaves short rows untouched now
    headed = [['h1', 'h2'], [1]]
    snapshot = copy.deepcopy(headed)
    Table.from_data(headed)
    assert headed == snapshot


def test_object_property_accessed_once():
    class Probed:
        def __init__(self):
            self.count = 0

        @property
        def probe(self):
            self.count += 1
            return 'value-%d' % self.count

    obj = Probed()
    t = Table.from_data([obj], max_depth=2, max_entries=10)
    assert obj.count == 1
    # rendering never re-invokes the property
    t.to_html(max_depth=2)
    t.to_text()
    assert obj.count == 1


def test_within_budget_output_matches_legacy():
    data = [{'id': 1, 'meta': {'a': 1, 'b': [1, 2, 3]}}]
    legacy = Table.from_data(data, max_depth=3)
    bounded = Table.from_data(data, max_depth=3, max_entries=100)
    assert bounded.fold_markers == []
    assert legacy.to_html(max_depth=3) == bounded.to_html(max_depth=3)
    assert legacy.to_text() == bounded.to_text()


def test_default_import_unchanged_without_budgets():
    t = Table.from_data([{'id': 1, 'name': 'John Doe'}])
    assert t.fold_markers == []
    ref = ('<table>\n<tr><th>id</th><td>1</td></tr>\n'
           '<tr><th>name</th><td>John Doe</td></tr>\n</table>')
    assert ''.join(sorted(t.to_html())) == ''.join(sorted(ref))


def test_from_helpers_accept_max_entries():
    data = {'a': 1, 'b': 2, 'c': 3}
    t = Table.from_dict(data, max_entries=2)
    assert any(isinstance(m, EntryLimitFoldMarker) for m in t.fold_markers)

    t = Table.from_list([[1, 2, 3, 4]], max_entries=2)
    assert any(isinstance(m, EntryLimitFoldMarker) for m in t.fold_markers)

    class Obj:
        pass
    obj = Obj()
    obj.a, obj.b, obj.c = 1, 2, 3
    t = Table.from_object(obj, max_entries=2)
    assert any(isinstance(m, EntryLimitFoldMarker) for m in t.fold_markers)


def test_fold_markers_collected_from_nested_tables():
    t = Table.from_data([{'a': {'b': {'c': 1, 'd': 2, 'e': 3}}}],
                        max_depth=3, max_entries=1)
    # the deepest dict exceeds the per-container budget; its marker is
    # surfaced on the root table's fold_markers registry
    paths = {m.path: m for m in t.fold_markers}
    assert (0, 'a', 'b') in paths
    assert isinstance(paths[(0, 'a', 'b')], EntryLimitFoldMarker)


def test_depth_one_keeps_scalar_cells_but_guards_unsafe_graphs():
    # entry budget without nesting keeps in-budget cells exactly like legacy
    class Obj:
        def __init__(self):
            self.alpha = 2
            self.zeta = 1

    legacy = Table.from_data([Obj()])
    bounded = Table.from_data([Obj()], max_entries=10)
    assert legacy.headers == bounded.headers
    assert legacy[0] == bounded[0]

    # a nested container at depth 1 stays a raw scalar cell
    nested = {'nested': 1}
    t = Table.from_data([{'a': nested, 'b': 2, 'c': 3}], max_entries=2)
    assert t[0][0] == nested

    # but a self-referential cell cannot be kept scalar and is folded
    cyclic = []
    cyclic.append(cyclic)
    t = Table.from_data([[cyclic]], max_entries=5)
    assert any(isinstance(m, CycleFoldMarker) for m in t.fold_markers)
    assert t.to_text() and t.to_html()

    # and a shared object's second occurrence is marked even at depth 1
    shared = {'x': 1}
    t = Table.from_data([[shared, shared]], max_entries=5)
    assert any(isinstance(m, SharedReferenceFoldMarker)
               for m in t.fold_markers)

    # a cycle nested inside an otherwise scalar depth-1 cell stays scalar
    # (Python's own repr handles it) while the hazard is still recorded
    inner = []
    inner.append(inner)
    t = Table.from_data([{'loopy': inner, 'safe': 1}], max_entries=10)
    assert t[0][0] == inner  # scalar cell preserved
    assert any(isinstance(m, CycleFoldMarker) for m in t.fold_markers)
    assert t.to_text() and t.to_html()


def test_combined_depth_and_entry_budgets():
    t = Table.from_data([{'a': {'b': {'c': 1, 'd': 2}}}],
                        max_depth=2, max_entries=1)
    kinds = {type(m) for m in t.fold_markers}
    # depth runs out before the inner dict can be expanded
    assert DepthLimitFoldMarker in kinds
    assert t.to_text() and t.to_html(max_depth=2)


def test_fold_marker_equality_and_path_formatting():
    m1 = DepthLimitFoldMarker('dict', (0, 'a'))
    m2 = DepthLimitFoldMarker('dict', (0, 'a'))
    m3 = DepthLimitFoldMarker('list', (0, 'a'))
    assert m1 == m2
    assert m1 != m3
    assert hash(m1) == hash(m2)
    assert str(m1) == '<fold:depth-limit dict at $[0].a>'
    entry = EntryLimitFoldMarker('list', (1,), 3)
    assert entry.omitted == 3
    assert '3 more entries' in str(entry)
    single = EntryLimitFoldMarker('list', (1,), 1)
    assert '1 more entry' in str(single)