"""Tests for sqlparse.snapshot (versioned pure-data tree snapshots)."""

import json

import pytest

import sqlparse
from sqlparse import snapshot, sql
from sqlparse import tokens as T
from sqlparse.snapshot import SnapshotError

SAMPLES = [
    'select * from foo;',
    "SELECT a, b AS x, t.c FROM tbl t WHERE a > 10 AND b = 'x  y';",
    'insert into t (a, b) values (1, 2), (3, 4);',
    'select case when a = 1 then \'one\' else \'other\' end from t;',
    'select count(*) over (partition by x) from t  -- trailing comment\n',
    '  select  a\n  from\t\tt\nwhere x in (select y from z);  ',
    'BEGIN\n  SELECT 1;\nEND;',
    "select array[1, 2], m['k'] from m;",
    'with cte as (select 1 as one) select * from cte;',
    'select cast(a as date), interval \'2 hours\' from t;',
]


def flatten_pairs(tree):
    return [(tok.ttype, tok.value) for tok in tree.flatten()]


@pytest.mark.parametrize('sql_text', SAMPLES)
def test_roundtrip_structure(sql_text):
    original = sqlparse.parse(sql_text)[0]
    restored = snapshot.load(snapshot.dump(original))

    assert type(restored) is type(original)
    assert str(restored) == str(original) == sql_text
    assert flatten_pairs(restored) == flatten_pairs(original)
    assert [type(t) for t in restored.get_sublists()] == \
        [type(t) for t in original.get_sublists()]


@pytest.mark.parametrize('sql_text', SAMPLES)
def test_roundtrip_json(sql_text):
    original = sqlparse.parse(sql_text)[0]
    restored = snapshot.loads(snapshot.dumps(original))
    assert flatten_pairs(restored) == flatten_pairs(original)
    assert str(restored) == sql_text


def test_whitespace_is_preserved():
    sql_text = 'select  a ,\n\tb  from   foo  ;'
    original = sqlparse.parse(sql_text)[0]
    assert str(original) == sql_text  # sanity check for this sample
    restored = snapshot.loads(snapshot.dumps(original))
    assert str(restored) == sql_text
    ws = [t.value for t in restored.flatten() if t.is_whitespace]
    assert ws and '\n' in ws and '\t' in ws


def test_parent_navigation():
    original = sqlparse.parse('select a.b from t where a = 1;')[0]
    restored = snapshot.load(snapshot.dump(original))

    assert restored.parent is None
    for group in [restored, *restored.get_sublists()]:
        for child in group.tokens:
            assert child.parent is group

    where = restored.token_next_by(i=sql.Where)[1]
    assert where is not None
    comparison = where.token_next_by(i=sql.Comparison)[1]
    assert comparison.within(sql.Where)
    assert comparison.within(sql.Statement)
    assert comparison.is_child_of(where)
    assert comparison.has_ancestor(restored)


def test_formatting_matches_original():
    sql_text = 'select a, b from t where x = 1 and y in (select z from u);'
    original = sqlparse.parse(sql_text)[0]
    restored = snapshot.load(snapshot.dump(original))
    options = {'reindent': True, 'keyword_case': 'upper'}
    assert sqlparse.format(str(restored), **options) == \
        sqlparse.format(str(original), **options)


def test_get_token_at_offset_and_identifiers():
    sql_text = 'select alpha, beta.gamma from tbl;'
    original = sqlparse.parse(sql_text)[0]
    restored = snapshot.load(snapshot.dump(original))
    assert restored.get_token_at_offset(9).value == 'alpha'
    orig_ids = original.token_next_by(i=sql.IdentifierList)[1]
    rest_ids = restored.token_next_by(i=sql.IdentifierList)[1]
    assert [i.get_real_name() for i in rest_ids.get_identifiers()] == \
        [i.get_real_name() for i in orig_ids.get_identifiers()]


def test_snapshot_is_pure_data():
    data = snapshot.dump(sqlparse.parse('select 1;')[0])
    assert data['version'] == snapshot.SNAPSHOT_VERSION
    # must be JSON round-trippable without custom encoders
    assert json.loads(json.dumps(data)) == data


def test_dump_rejects_non_tokenlist():
    with pytest.raises(SnapshotError):
        snapshot.dump(sql.Token(T.Keyword, 'select'))
    with pytest.raises(SnapshotError):
        snapshot.dump('select 1')


def test_dump_rejects_unknown_group_class():
    class Custom(sql.TokenList):
        pass

    with pytest.raises(SnapshotError):
        snapshot.dump(Custom([sql.Token(T.Keyword, 'select')]))


def test_dump_rejects_inconsistent_group_value():
    group = sql.Where([sql.Token(T.Keyword, 'where')])
    group.value = 'tampered'
    with pytest.raises(SnapshotError, match='does not match'):
        snapshot.dump(group)


def make_snapshot(**tree_kwargs):
    tree = {
        'type': 'group',
        'class': 'Statement',
        'value': 'select 1',
        'children': [
            {'type': 'token', 'ttype': 'Keyword.DML', 'value': 'select'},
            {'type': 'token', 'ttype': 'Text.Whitespace', 'value': ' '},
            {'type': 'token', 'ttype': 'Number.Integer', 'value': '1'},
        ],
    }
    tree.update(tree_kwargs)
    return {'version': snapshot.SNAPSHOT_VERSION, 'tree': tree}


def test_load_minimal_valid():
    stmt = snapshot.load(make_snapshot())
    assert isinstance(stmt, sql.Statement)
    assert str(stmt) == 'select 1'
    assert [t.ttype for t in stmt.flatten()] == [
        T.Keyword.DML, T.Whitespace, T.Number.Integer]
    assert list(stmt.flatten())[1].ttype is T.Text.Whitespace


def test_load_rejects_bad_version():
    for version in (0, 2, '1', 1.0, True, None):
        with pytest.raises(SnapshotError):
            snapshot.load({'version': version, 'tree': {}})


def test_load_rejects_bad_document_shape():
    with pytest.raises(SnapshotError):
        snapshot.load([])
    with pytest.raises(SnapshotError):
        snapshot.load({'version': snapshot.SNAPSHOT_VERSION})
    with pytest.raises(SnapshotError):
        snapshot.load({**make_snapshot(), 'extra': 1})


def test_load_rejects_unknown_group():
    tree = make_snapshot()
    tree['tree']['class'] = 'Evil'
    tree['tree']['children'] = []
    tree['tree']['value'] = ''
    with pytest.raises(SnapshotError, match='Unknown group class'):
        snapshot.load(tree)


def test_load_rejects_unknown_node_type():
    tree = make_snapshot()
    tree['tree']['children'][0] = {'type': 'weird', 'value': 'x'}
    with pytest.raises(SnapshotError, match='Unknown snapshot node type'):
        snapshot.load(tree)


def test_load_rejects_value_mismatch():
    tree = make_snapshot()
    tree['tree']['value'] = 'select 2'
    with pytest.raises(SnapshotError, match='does not match'):
        snapshot.load(tree)


def test_load_rejects_mismatch_in_nested_group():
    tree = make_snapshot(value='select (1)', children=[
        {'type': 'token', 'ttype': 'Keyword.DML', 'value': 'select'},
        {'type': 'token', 'ttype': 'Whitespace', 'value': ' '},
        {'type': 'group', 'class': 'Parenthesis', 'value': '(2)',
         'children': [
             {'type': 'token', 'ttype': 'Punctuation', 'value': '('},
             {'type': 'token', 'ttype': 'Number.Integer', 'value': '1'},
             {'type': 'token', 'ttype': 'Punctuation', 'value': ')'},
         ]},
    ])
    with pytest.raises(SnapshotError, match='does not match'):
        snapshot.load(tree)


def test_load_rejects_leaf_with_children():
    tree = make_snapshot()
    tree['tree']['children'][0] = {
        'type': 'token', 'ttype': 'Keyword.DML', 'value': 'select',
        'children': []}
    with pytest.raises(SnapshotError, match='Malformed token node'):
        snapshot.load(tree)


def test_load_rejects_group_without_children_list():
    tree = make_snapshot()
    tree['tree']['children'] = None
    with pytest.raises(SnapshotError, match='children must be a list'):
        snapshot.load(tree)


def test_load_rejects_non_dict_node():
    tree = make_snapshot()
    tree['tree']['children'][0] = 'select'
    with pytest.raises(SnapshotError, match='must be a mapping'):
        snapshot.load(tree)


def test_load_rejects_nested_statement():
    inner = make_snapshot()['tree']
    inner['value'] = 'select 1'
    tree = make_snapshot(value='select 1', children=[inner])
    with pytest.raises(SnapshotError, match='only allowed at the root'):
        snapshot.load(tree)


def test_load_rejects_token_root():
    with pytest.raises(SnapshotError, match='root must be a group'):
        snapshot.load({
            'version': snapshot.SNAPSHOT_VERSION,
            'tree': {'type': 'token', 'ttype': 'Keyword', 'value': 'x'},
        })


@pytest.mark.parametrize('ttype', [
    '', '.', 'Keyword..DML', 'Keyword.DM L', '__class__', 'Keyword._x',
    'Keyword/DML', 42, None,
])
def test_load_rejects_invalid_ttype(ttype):
    tree = make_snapshot()
    tree['tree']['children'][0]['ttype'] = ttype
    with pytest.raises(SnapshotError, match='Invalid token type'):
        snapshot.load(tree)


def test_load_rejects_too_deep():
    node = {'type': 'token', 'ttype': 'Number.Integer', 'value': '1'}
    value = '1'
    for _ in range(snapshot.MAX_SNAPSHOT_DEPTH + 1):
        node = {'type': 'group', 'class': 'Operation',
                'value': value, 'children': [node]}
    with pytest.raises(SnapshotError, match='depth limit'):
        snapshot.load({'version': snapshot.SNAPSHOT_VERSION, 'tree': node})


def test_load_rejects_too_many_nodes(monkeypatch):
    monkeypatch.setattr(snapshot, 'MAX_SNAPSHOT_NODES', 2)
    with pytest.raises(SnapshotError, match='node limit'):
        snapshot.load(make_snapshot())


def test_loads_rejects_invalid_json():
    with pytest.raises(SnapshotError):
        snapshot.loads('{not json')
    with pytest.raises(SnapshotError):
        snapshot.loads('42')


def test_loads_rejects_deeply_nested_json():
    doc = '{"type":"token","ttype":"Number.Integer","value":"1"}'
    for _ in range(2000):
        doc = ('{"type":"group","class":"Operation","value":"1",'
               f'"children":[{doc}]}}')
    with pytest.raises(SnapshotError):
        snapshot.loads(doc)


def test_restored_tree_supports_grouping_api():
    sql_text = "select f(a, b) as res from t1.t2 where x = 'y';"
    original = sqlparse.parse(sql_text)[0]
    restored = snapshot.load(snapshot.dump(original))

    outer = restored.token_next_by(i=sql.Identifier)[1]
    func = outer.token_next_by(i=sql.Function)[1]
    assert outer.get_alias() == 'res'
    assert [str(p) for p in func.get_parameters()] == ['a', 'b']

    tbl = next(g for g in restored.get_sublists()
               if isinstance(g, sql.Identifier) and g.value == 't1.t2')
    assert tbl.get_parent_name() == 't1'
    assert tbl.get_real_name() == 't2'
    assert restored.get_type() == 'SELECT'
    assert restored.token_first().normalized == 'SELECT'
