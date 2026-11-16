"""Tests for the pickle-free, versioned token-tree snapshots."""

import copy
import json

import pytest

import sqlparse
from sqlparse import filters, formatter, snapshot, sql
from sqlparse.engine import FilterStack
from sqlparse.exceptions import SQLParseError

SQL_SAMPLES = [
    "SELECT 1;",
    "SELECT  a.foo , b.bar  FROM t WHERE x = 1;",
    "-- leading comment\nSELECT id, name AS nm,\n"
    "       count(*) AS c\n"
    "  FROM schema.table AS t\n"
    " WHERE id IN (1, 2, 3)\n"
    "   AND name <> 'it''s'\n"
    " GROUP BY id /* inline */\n"
    " ORDER BY id DESC;\n",
    "SELECT (((1 + 2) * 3) - 4),\n"
    "       CASE WHEN a THEN b ELSE c END\n"
    "  FROM (SELECT x FROM y) AS z;",
    "BEGIN\n  FOR r IN (SELECT 1) LOOP\n    NULL;\n"
    "  END LOOP;\nEND;",
]

FORMAT_OPTIONS = [
    {},
    {"reindent": True},
    {"reindent": True, "keyword_case": "upper", "identifier_case": "lower"},
    {"reindent_aligned": True},
    {"strip_comments": True, "strip_whitespace": True},
    {"output_format": "python"},
    {"use_space_around_operators": True, "reindent": True},
]


def _format_statement(stmt, **options):
    """Run the statement-level formatter pipeline on an existing tree."""
    stack = FilterStack()
    options = formatter.validate_options(dict(options))
    stack = formatter.build_filter_stack(stack, options)
    stack.postprocess.append(filters.SerializerUnicode())
    for process in stack.stmtprocess:
        process.process(stmt)
    result = stmt
    for process in stack.postprocess:
        result = process.process(result)
    return result


def _group_chain(token):
    """Class names of the groups from the token up to the root."""
    names = []
    parent = token.parent
    while parent is not None:
        names.append(type(parent).__name__)
        parent = parent.parent
    return names


def _assert_trees_equal(original, restored):
    assert type(original) is type(restored)
    assert original.is_group == restored.is_group
    assert original.value == restored.value
    assert str(original) == str(restored)

    if not original.is_group:
        # Token type identity must survive the round-trip.
        assert restored.ttype is original.ttype
        assert restored.is_keyword == original.is_keyword
        assert restored.is_whitespace == original.is_whitespace
        assert restored.is_newline == original.is_newline
        assert restored.normalized == original.normalized
        return

    assert len(original.tokens) == len(restored.tokens)
    for original_child, restored_child in zip(original.tokens, restored.tokens):
        _assert_trees_equal(original_child, restored_child)
        # Parent navigation must point at the enclosing restored group.
        assert restored_child.parent is restored
        assert _group_chain(restored_child) == _group_chain(original_child)


@pytest.mark.parametrize("sql_text", SQL_SAMPLES)
def test_roundtrip_preserves_tree_and_whitespace(sql_text):
    parsed = sqlparse.parse(sql_text)
    assert len(parsed) == 1
    original = parsed[0]

    data = snapshot.tree_to_snapshot(original)

    # The snapshot is pure data: JSON must be able to carry it verbatim.
    encoded = json.dumps(data)
    restored = snapshot.snapshot_to_tree(json.loads(encoded))

    assert isinstance(restored, sql.Statement)
    assert restored.parent is None
    _assert_trees_equal(original, restored)

    # flatten() yields the same leaves and the raw text is unchanged,
    # including every space, tab and newline of the source.  The statement
    # splitter trims a single trailing newline from the last statement.
    expected = sql_text[:-1] if sql_text.endswith("\n") else sql_text
    assert [t.value for t in restored.flatten()] == \
        [t.value for t in original.flatten()]
    assert str(restored) == str(original) == expected
    assert "".join(t.value for t in restored.flatten()) == expected


def test_snapshot_is_versioned_pure_data():
    stmt = sqlparse.parse("SELECT a FROM b;")[0]
    data = snapshot.tree_to_snapshot(stmt)

    assert data["version"] == snapshot.SNAPSHOT_VERSION
    root = data["tree"]
    assert root["type"] == "group"
    assert root["group"] == "Statement"
    assert root["value"] == "SELECT a FROM b;"
    assert isinstance(root["children"], list)

    leaf = next(
        node for node in _iter_nodes(root) if node["type"] == "token"
    )
    assert leaf == {
        "type": "token",
        "ttype": ["Keyword", "DML"],
        "value": "SELECT",
    }

    def is_plain(value):
        if isinstance(value, dict):
            return all(isinstance(k, str) and is_plain(v)
                       for k, v in value.items())
        if isinstance(value, list):
            return all(is_plain(item) for item in value)
        return isinstance(value, (str, int, bool)) or value is None

    assert is_plain(data)


def _iter_nodes(node):
    yield node
    if node.get("type") == "group":
        for child in node["children"]:
            yield from _iter_nodes(child)


@pytest.mark.parametrize("sql_text", SQL_SAMPLES)
@pytest.mark.parametrize("options", FORMAT_OPTIONS)
def test_formatting_matches_original_tree(sql_text, options):
    original_output = _format_statement(
        sqlparse.parse(sql_text)[0], **options
    )

    data = snapshot.tree_to_snapshot(sqlparse.parse(sql_text)[0])
    restored = snapshot.snapshot_to_tree(copy.deepcopy(data))
    restored_output = _format_statement(restored, **options)

    assert restored_output == original_output


def test_multiple_statements_roundtrip():
    sql_text = "SELECT 1; SELECT 2;"
    originals = sqlparse.parse(sql_text)

    payload = {
        "version": snapshot.SNAPSHOT_VERSION,
        "statements": [
            snapshot.tree_to_snapshot(stmt)["tree"] for stmt in originals
        ],
    }
    restored = [
        snapshot.snapshot_to_tree(
            {"version": snapshot.SNAPSHOT_VERSION, "tree": tree}
        )
        for tree in json.loads(json.dumps(payload))["statements"]
    ]
    assert [str(stmt) for stmt in restored] == \
        [str(stmt) for stmt in originals]
    assert "".join(str(stmt) for stmt in restored) == sql_text


def test_parent_navigation_after_restore():
    original = sqlparse.parse(
        "SELECT f(x) FROM (SELECT y FROM t) AS sub;"
    )[0]
    restored = snapshot.snapshot_to_tree(
        snapshot.tree_to_snapshot(original)
    )

    original_leaves = list(original.flatten())
    restored_leaves = list(restored.flatten())
    assert len(original_leaves) == len(restored_leaves)

    for original_leaf, restored_leaf in zip(original_leaves, restored_leaves):
        assert _group_chain(restored_leaf) == _group_chain(original_leaf)
        assert restored_leaf.is_child_of(restored_leaf.parent)
        assert restored_leaf.has_ancestor(restored)

    original_func = next(
        token for token in original.flatten()
        if token.within(sql.Function)
    )
    restored_func = next(
        token for token in restored.flatten() if token.within(sql.Function)
    )
    assert restored_func.within(sql.Function)
    assert _group_chain(restored_func) == _group_chain(original_func)


def test_serialize_rejects_non_tokens():
    with pytest.raises(SQLParseError):
        snapshot.tree_to_snapshot("SELECT 1;")
    with pytest.raises(SQLParseError):
        snapshot.tree_to_snapshot({"not": "a token"})


def test_load_rejects_unsupported_version():
    data = snapshot.tree_to_snapshot(sqlparse.parse("SELECT 1;")[0])
    data["version"] = 999
    with pytest.raises(SQLParseError, match="Unsupported snapshot version"):
        snapshot.snapshot_to_tree(data)


def test_load_rejects_non_mapping_input():
    with pytest.raises(SQLParseError, match="top-level object"):
        snapshot.snapshot_to_tree(["not", "a", "mapping"])
    with pytest.raises(SQLParseError, match="top-level object"):
        snapshot.snapshot_to_tree("nope")


def test_load_rejects_missing_root_tree():
    with pytest.raises(SQLParseError, match="root 'tree'"):
        snapshot.snapshot_to_tree({"version": snapshot.SNAPSHOT_VERSION})


def test_load_rejects_unknown_group_category():
    data = snapshot.tree_to_snapshot(sqlparse.parse("SELECT 1;")[0])
    data["tree"]["group"] = "os.system"
    with pytest.raises(SQLParseError, match="Unknown group category"):
        snapshot.snapshot_to_tree(data)

    # Only the static registry may resolve classes: no dynamic import.
    data["tree"]["group"] = "__import__"
    with pytest.raises(SQLParseError, match="Unknown group category"):
        snapshot.snapshot_to_tree(data)


def test_load_rejects_illegal_parent_child_structures():
    base = snapshot.tree_to_snapshot(sqlparse.parse("SELECT 1;")[0])

    bad_children = copy.deepcopy(base)
    bad_children["tree"]["children"] = []
    with pytest.raises(SQLParseError, match="non-empty list"):
        snapshot.snapshot_to_tree(bad_children)

    bad_child = copy.deepcopy(base)
    bad_child["tree"]["children"] = ["not-a-node"]
    with pytest.raises(SQLParseError, match="node mappings"):
        snapshot.snapshot_to_tree(bad_child)

    bad_value = copy.deepcopy(base)
    bad_value["tree"]["value"] = 42
    with pytest.raises(SQLParseError, match="'value' must be a string"):
        snapshot.snapshot_to_tree(bad_value)

    bad_leaf_value = copy.deepcopy(base)
    leaf = next(
        node for node in _iter_nodes(bad_leaf_value["tree"])
        if node["type"] == "token"
    )
    leaf["value"] = ["SELECT"]
    with pytest.raises(SQLParseError, match="'value' must be a string"):
        snapshot.snapshot_to_tree(bad_leaf_value)

    bad_node_type = copy.deepcopy(base)
    bad_node_type["tree"]["type"] = "pickle"
    with pytest.raises(SQLParseError, match="unknown node type"):
        snapshot.snapshot_to_tree(bad_node_type)

    bad_ttype = copy.deepcopy(base)
    leaf = next(
        node for node in _iter_nodes(bad_ttype["tree"])
        if node["type"] == "token"
    )
    leaf["ttype"] = []
    with pytest.raises(SQLParseError, match="non-empty list"):
        snapshot.snapshot_to_tree(bad_ttype)

    bad_ttype_name = copy.deepcopy(base)
    leaf = next(
        node for node in _iter_nodes(bad_ttype_name["tree"])
        if node["type"] == "token"
    )
    leaf["ttype"] = ["not an identifier"]
    with pytest.raises(SQLParseError, match="token type component"):
        snapshot.snapshot_to_tree(bad_ttype_name)


def test_load_rejects_shared_subtrees():
    data = snapshot.tree_to_snapshot(sqlparse.parse("SELECT 1;")[0])
    shared = data["tree"]["children"][0]
    data["tree"]["children"].append(shared)
    with pytest.raises(SQLParseError, match="duplicate node reference"):
        snapshot.snapshot_to_tree(data)


def test_load_rejects_inconsistent_group_value():
    data = snapshot.tree_to_snapshot(sqlparse.parse("SELECT 1;")[0])
    original_value = data["tree"]["value"]
    data["tree"]["value"] = original_value + " tampered"

    with pytest.raises(SQLParseError, match="does not match"):
        snapshot.snapshot_to_tree(data)

    # Tampering with a nested group is detected as well.
    deep = snapshot.tree_to_snapshot(
        sqlparse.parse("SELECT (1 + 2) FROM t;")[0]
    )
    nested = next(
        node for node in _iter_nodes(deep["tree"])
        if node.get("group") == "Parenthesis"
    )
    nested["value"] += "x"
    with pytest.raises(SQLParseError, match="does not match"):
        snapshot.snapshot_to_tree(deep)


def _paren_group(inner):
    """Build a structurally valid Parenthesis snapshot around *inner*."""
    children = [
        {"type": "token", "ttype": ["Punctuation"], "value": "("},
        inner,
        {"type": "token", "ttype": ["Punctuation"], "value": ")"},
    ]
    return {
        "type": "group",
        "group": "Parenthesis",
        "value": "".join(child["value"] for child in children),
        "children": children,
    }


def test_load_depth_boundaries():
    leaf = {"type": "token", "ttype": ["Number", "Integer"], "value": "1"}

    # Exactly MAX_SNAPSHOT_DEPTH nested group levels must round-trip.
    node = leaf
    for _ in range(snapshot.MAX_SNAPSHOT_DEPTH):
        node = _paren_group(node)
    restored = snapshot.snapshot_to_tree(
        {"version": snapshot.SNAPSHOT_VERSION, "tree": node}
    )
    assert restored.value == "(" * snapshot.MAX_SNAPSHOT_DEPTH + "1" + \
        ")" * snapshot.MAX_SNAPSHOT_DEPTH

    # One additional level must fail with a clean, non-recursion error.
    too_deep = _paren_group(node)
    with pytest.raises(SQLParseError, match="nesting too deep"):
        snapshot.snapshot_to_tree(
            {"version": snapshot.SNAPSHOT_VERSION, "tree": too_deep}
        )


def test_load_does_not_mutate_input():
    data = snapshot.tree_to_snapshot(sqlparse.parse("SELECT 1;")[0])
    blob = json.dumps(data)
    snapshot.snapshot_to_tree(copy.deepcopy(data))
    # Rejected payloads must not leave helper keys behind either.
    bad = copy.deepcopy(data)
    bad["tree"]["children"].append(bad["tree"]["children"][0])
    with pytest.raises(SQLParseError):
        snapshot.snapshot_to_tree(bad)
    assert json.dumps(data) == blob


def test_load_rejects_excessive_ttype_nesting():
    data = snapshot.tree_to_snapshot(sqlparse.parse("SELECT 1;")[0])
    leaf = next(
        node for node in _iter_nodes(data["tree"]) if node["type"] == "token"
    )
    leaf["ttype"] = ["Name"] * (snapshot.MAX_TTYPE_DEPTH + 1)
    with pytest.raises(SQLParseError, match="Token type nesting too deep"):
        snapshot.snapshot_to_tree(data)


def test_load_enforces_node_count_limit(monkeypatch):
    monkeypatch.setattr(snapshot, "MAX_SNAPSHOT_NODES", 3)
    data = snapshot.tree_to_snapshot(sqlparse.parse("SELECT 1;")[0])
    with pytest.raises(SQLParseError, match="Snapshot too large"):
        snapshot.snapshot_to_tree(data)


def test_leaf_snapshot_roundtrip():
    token = sql.Token(sqlparse.tokens.Name, "  col  ")
    data = snapshot.tree_to_snapshot(token)
    restored = snapshot.snapshot_to_tree(data)

    assert isinstance(restored, sql.Token)
    assert not restored.is_group
    assert restored.ttype is sqlparse.tokens.Name
    assert restored.value == "  col  "
    assert restored.parent is None


def test_no_dynamic_import_used():
    # The registry only exposes the concrete groups declared in sqlparse.sql.
    assert snapshot._GROUP_REGISTRY["Statement"] is sql.Statement
    assert snapshot._GROUP_REGISTRY["Parenthesis"] is sql.Parenthesis
    assert "Token" not in snapshot._GROUP_REGISTRY
    for cls in snapshot._GROUP_REGISTRY.values():
        assert issubclass(cls, sql.TokenList)