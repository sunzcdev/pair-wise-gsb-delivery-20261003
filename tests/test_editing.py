"""Tests for the restricted range-editing API."""

import pytest

import sqlparse
from sqlparse import sql, tokens as T
from sqlparse.editing import EditOp, RangeEditor, check_structure, edit_range
from sqlparse.exceptions import RangeEditError


def leaf_spans(root):
    spans = []
    offset = 0
    for token in root.flatten():
        spans.append((offset, offset + len(token.value), token))
        offset += len(token.value)
    return spans


def span_of(root, value):
    matches = [span for span in leaf_spans(root) if span[2].value == value]
    assert len(matches) == 1
    return matches[0]


def test_replace_leaf_updates_text_and_value():
    parsed = sqlparse.parse("select a from t where x = 1")[0]
    start, end, _ = span_of(parsed, "1")

    RangeEditor(parsed).replace(start, end, "2")

    assert str(parsed) == "select a from t where x = 2"
    assert check_structure(parsed) == []


def test_replace_preserves_identity_of_unaffected_nodes():
    parsed = sqlparse.parse("select a from t where x = 1")[0]
    before = list(parsed.flatten())
    _, _, replaced = span_of(parsed, "1")

    start, end, _ = span_of(parsed, "1")
    RangeEditor(parsed).replace(start, end, "2")

    after = list(parsed.flatten())
    assert replaced not in after
    surviving = [token for token in after if token.value != "2"]
    assert all(token in before for token in surviving)
    # Group objects keep their identity too.
    where = parsed.tokens[-1]
    assert isinstance(where, sql.Where)


def test_replace_inside_nested_group_refreshes_ancestor_values():
    parsed = sqlparse.parse("select a from t where x = 1")[0]
    comparison = parsed.tokens[-1].tokens[-1]
    assert isinstance(comparison, sql.Comparison)

    start, end, _ = span_of(parsed, "1")
    RangeEditor(parsed).replace(start, end, "42")

    assert str(comparison) == "x = 42"
    assert comparison.value == "x = 42"
    assert parsed.tokens[-1].value == "where x = 42"
    assert check_structure(parsed) == []


def test_insert_at_boundary():
    parsed = sqlparse.parse("select a from t")[0]
    start, _, _ = span_of(parsed, "a")

    RangeEditor(parsed).insert(start, "b, ")

    assert str(parsed) == "select b, a from t"
    assert check_structure(parsed) == []


def test_insert_at_end_of_tree():
    parsed = sqlparse.parse("select a")[0]
    RangeEditor(parsed).insert(len(str(parsed)), " -- tail")
    assert str(parsed) == "select a -- tail"
    assert check_structure(parsed) == []


def test_insert_at_start_of_tree():
    parsed = sqlparse.parse("select a")[0]
    RangeEditor(parsed).insert(0, "/* hint */ ")
    assert str(parsed) == "/* hint */ select a"
    assert check_structure(parsed) == []


def test_delete_removes_leaf_tokens():
    parsed = sqlparse.parse("select a from t")[0]
    start, end, _ = span_of(parsed, "a")

    RangeEditor(parsed).delete(start, end)

    assert str(parsed) == "select  from t"
    assert check_structure(parsed) == []


def test_delete_via_empty_replacement():
    parsed = sqlparse.parse("select a from t")[0]
    start, end, _ = span_of(parsed, "a")

    RangeEditor(parsed).apply([EditOp.replace(start, end, "")])

    assert str(parsed) == "select  from t"


def test_parents_are_maintained_after_edit():
    parsed = sqlparse.parse("select a from t where x = 1")[0]
    start, end, _ = span_of(parsed, "1")
    RangeEditor(parsed).replace(start, end, "2")

    for token in parsed.flatten():
        assert token.parent is not None
        assert token in token.parent.tokens


def test_batch_applies_non_overlapping_edits():
    parsed = sqlparse.parse("select a from t")[0]
    a_start, a_end, _ = span_of(parsed, "a")

    edit_range(parsed, [
        EditOp.replace(0, 6, "SELECT"),
        EditOp.replace(a_start, a_end, "b"),
    ])

    assert str(parsed) == "SELECT b from t"
    assert check_structure(parsed) == []


def test_batch_rejects_overlapping_ranges():
    parsed = sqlparse.parse("select a from t")[0]
    before = str(parsed)
    ids_before = {id(token) for token in parsed.flatten()}

    with pytest.raises(RangeEditError, match="overlapping"):
        RangeEditor(parsed).apply([
            EditOp.replace(0, 7, "SELECT "),
            EditOp.replace(6, 8, "xx"),
        ])

    assert str(parsed) == before
    assert {id(token) for token in parsed.flatten()} == ids_before


def test_adjacent_ranges_are_allowed():
    parsed = sqlparse.parse("select a")[0]
    RangeEditor(parsed).apply([
        EditOp.replace(0, 6, "SELECT"),
        EditOp.replace(6, 7, "  "),
    ])
    assert str(parsed) == "SELECT  a"


def test_splitting_a_leaf_token_is_rejected():
    parsed = sqlparse.parse("select a")[0]
    start, _, token = span_of(parsed, "select")
    middle = start + 3  # inside the "select" leaf token

    with pytest.raises(RangeEditError, match="leaf token"):
        RangeEditor(parsed).replace(middle, start + len(token.value), "X")

    assert str(parsed) == "select a"


def test_crossing_group_boundary_is_rejected():
    parsed = sqlparse.parse("select a from t")[0]
    a_start, _, _ = span_of(parsed, "a")
    from_start, from_end, _ = span_of(parsed, "from")

    # "a" lives inside an Identifier while "from" is a Statement child.
    with pytest.raises(RangeEditError, match="group boundary"):
        RangeEditor(parsed).replace(a_start, from_end, "z")
    assert str(parsed) == "select a from t"


def test_out_of_range_offset_is_rejected():
    parsed = sqlparse.parse("select a")[0]
    length = len(str(parsed))
    with pytest.raises(RangeEditError, match="beyond the end"):
        RangeEditor(parsed).delete(length - 1, length + 2)


def test_negative_and_reversed_ranges_are_rejected():
    parsed = sqlparse.parse("select a")[0]
    with pytest.raises(RangeEditError):
        RangeEditor(parsed).delete(-1, 2)
    with pytest.raises(RangeEditError, match="start must not be greater"):
        RangeEditor(parsed).delete(4, 2)


def test_failed_batch_leaves_no_half_tree():
    parsed = sqlparse.parse("select a from t")[0]
    before = str(parsed)
    ids_before = {id(token) for token in parsed.flatten()}

    with pytest.raises(RangeEditError):
        RangeEditor(parsed).apply([
            EditOp.replace(0, 6, "SELECT"),
            EditOp.delete(100, 102),  # out of range
        ])

    assert str(parsed) == before
    assert {id(token) for token in parsed.flatten()} == ids_before
    assert check_structure(parsed) == []


def test_no_auto_regrouping():
    # Replacing a function name leaf does not rebuild surrounding groups.
    parsed = sqlparse.parse("select foo(1)")[0]
    function = parsed.tokens[2]
    assert isinstance(function, sql.Function)
    start, end, name_token = span_of(parsed, "foo")
    RangeEditor(parsed).replace(start, end, "bar")

    # The Function group keeps its identity even though only text changed.
    assert parsed.tokens[2] is function
    assert isinstance(parsed.tokens[2], sql.Function)
    assert str(parsed) == "select bar(1)"
    assert name_token not in list(parsed.flatten())


def test_check_structure_detects_stale_value():
    parsed = sqlparse.parse("select a")[0]
    assert check_structure(parsed) == []
    parsed.value = "stale"
    problems = check_structure(parsed)
    assert problems and "stale" in problems[0]


def test_check_structure_detects_stale_parent():
    parsed = sqlparse.parse("select a")[0]
    leaf = next(iter(parsed.flatten()))
    leaf.parent = None
    problems = check_structure(parsed)
    assert any("parent" in problem for problem in problems)


def test_explicit_replacement_tokens_are_used():
    parsed = sqlparse.parse("select a")[0]
    start, end, _ = span_of(parsed, "a")
    custom = sql.Token(T.Name, "col")

    RangeEditor(parsed).apply(
        [EditOp(start, end, tokens=(custom,))])

    assert str(parsed) == "select col"
    assert custom.parent is not None


def test_grouped_replacement_tokens_are_rejected():
    parsed = sqlparse.parse("select a")[0]
    start, end, _ = span_of(parsed, "a")
    group = sql.Identifier([sql.Token(T.Name, "col")])

    with pytest.raises(RangeEditError, match="leaf tokens"):
        RangeEditor(parsed).apply(
            [EditOp(start, end, tokens=(group,))])


def test_empty_batch_is_a_noop():
    parsed = sqlparse.parse("select a")[0]
    before = str(parsed)
    RangeEditor(parsed).apply([])
    assert str(parsed) == before


def test_insert_into_empty_group():
    empty = sql.Statement([])
    RangeEditor(empty).insert(0, "select 1")
    assert str(empty) == "select 1"
    assert check_structure(empty) == []
    for token in empty.flatten():
        assert token.parent is not None


def test_removed_tokens_are_detached_from_tree():
    parsed = sqlparse.parse("select a from t")[0]
    start, end, removed = span_of(parsed, "a")
    RangeEditor(parsed).replace(start, end, "b")
    assert removed not in list(parsed.flatten())
    # The removed leaf is no longer referenced by its old group.
    assert all(removed not in group.tokens
               for group in [parsed] + list(parsed.get_sublists()))
