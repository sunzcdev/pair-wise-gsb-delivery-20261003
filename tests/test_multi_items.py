"""Tests for the ijson.multi_items method"""

import collections
import io

import pytest

from ijson import common
from ijson.common import ObjectBuilder

from .test_base import JSON, JSON_OBJECT


def test_multi_items_single_root_selector(adaptor):
    assert [("", JSON_OBJECT)] == adaptor.multi_items(JSON, [""])


def test_multi_items_mixed_scalars_and_containers(adaptor):
    json = b'{"a": 1, "b": {"x": 2}, "list": [10, 20, 30]}'
    prefixes = ["a", "b", "list.item"]
    expected = [
        ("a", 1),
        ("b", {"x": 2}),
        ("list.item", 10),
        ("list.item", 20),
        ("list.item", 30),
    ]
    assert expected == adaptor.multi_items(json, prefixes)


def test_multi_items_results_in_document_order(adaptor):
    # Selectors are given out of order; results must still follow the document.
    json = b'{"first": 1, "second": 2, "third": 3}'
    prefixes = ["third", "first", "second"]
    expected = [("first", 1), ("second", 2), ("third", 3)]
    assert expected == adaptor.multi_items(json, prefixes)


def test_multi_items_array_elements(adaptor):
    json = b'{"docs": [{"id": 1}, {"id": 2}, {"id": 3}]}'
    expected = [
        ("docs.item", {"id": 1}),
        ("docs.item", {"id": 2}),
        ("docs.item", {"id": 3}),
    ]
    assert expected == adaptor.multi_items(json, ["docs.item"])


def test_multi_items_sibling_selectors(adaptor):
    json = b'{"a": {"x": 1}, "b": {"y": 2}}'
    expected = [("a", {"x": 1}), ("b", {"y": 2})]
    assert expected == adaptor.multi_items(json, ["b", "a"])


def test_multi_items_parent_and_child_container(adaptor):
    # The child object is emitted on its own and also kept inside the parent.
    json = b'{"parent": {"child": {"leaf": 5}}}'
    expected = [
        ("parent.child", {"leaf": 5}),
        ("parent", {"child": {"leaf": 5}}),
    ]
    assert expected == adaptor.multi_items(json, ["parent", "parent.child"])


def test_multi_items_child_scalar_kept_inside_parent(adaptor):
    # A scalar selector nested inside a matched container is not emitted twice:
    # it is delivered as part of the enclosing object.
    json = b'{"parent": {"x": 1, "y": 2}}'
    assert [("parent", {"x": 1, "y": 2})] == adaptor.multi_items(
        json, ["parent", "parent.x", "parent.y"]
    )


def test_multi_items_duplicate_selectors_collapse(adaptor):
    json = b'{"a": 1, "b": 2}'
    assert [("a", 1), ("b", 2)] == adaptor.multi_items(
        json, ["a", "b", "a", "b"]
    )


def test_multi_items_scalar_at_root(adaptor):
    assert [("", 42)] == adaptor.multi_items(b"42", [""])


def test_multi_items_root_array(adaptor):
    assert [("", [1, 2, 3])] == adaptor.multi_items(b"[1, 2, 3]", [""])


def test_multi_items_no_match(adaptor):
    assert [] == adaptor.multi_items(b'{"a": 1}', ["nope"])


@pytest.mark.parametrize(
    "json, prefixes, expected",
    (
        (b'{"0.1": 0, "0": {"1": 1}}', ["0.1"], [("0.1", 0), ("0.1", 1)]),
        (
            b'{"0.1": [{"a.b": 0}]}',
            ["0.1.item.a.b"],
            [("0.1.item.a.b", 0)],
        ),
        (b'{"abc.def": 0}', ["abc"], []),
        (b'{"a": {"": {"b": 1}}}', ["a."], [("a.", {"b": 1})]),
    ),
)
def test_multi_items_dotted_and_empty_names(adaptor, json, prefixes, expected):
    assert expected == adaptor.multi_items(json, prefixes)


def test_multi_items_map_type(adaptor):
    result = adaptor.multi_items(b'{"o": {"a": 1}}', ["o"],
                                 map_type=collections.OrderedDict)
    prefix, value = result[0]
    assert prefix == "o"
    assert isinstance(value, collections.OrderedDict)


def test_multi_items_invalid_json_in_ignored_branch_raises(adaptor):
    # Filtering must not skip JSON legality checks on unmatched branches.
    bad_json = b'{"huge": [1, 2, ], "keep": 7}'
    with pytest.raises(common.JSONError):
        adaptor.multi_items(bad_json, ["keep"])


def test_multi_items_incomplete_json_raises(adaptor):
    with pytest.raises(common.IncompleteJSONError):
        adaptor.multi_items(b'{"keep": {"a": 1}', ["keep"])


def test_multi_items_multiple_values(adaptor):
    if not adaptor.backend.capabilities.multiple_values:
        with pytest.raises(ValueError):
            adaptor.multi_items(b"", [""], multiple_values=True)
        return
    json = b'{"a": 1}{"a": 2}'
    with pytest.raises(common.JSONError):
        adaptor.multi_items(json, ["a"])
    expected = [("a", 1), ("a", 2)]
    assert expected == adaptor.multi_items(
        json, ["a"], multiple_values=True
    )


@pytest.mark.parametrize(
    "prefixes",
    (
        [],
        "single-string",
        b"bytes",
        [1, 2],
        ["ok", 3],
    ),
)
def test_multi_items_invalid_prefixes(backend, prefixes):
    # Validation happens before reading, so even an unusable source is fine.
    class NoRead:
        def read(self, size=-1):
            raise AssertionError("source must not be read")

    with pytest.raises(ValueError):
        list(backend.multi_items_gen(NoRead(), prefixes))
    with pytest.raises(ValueError):
        backend.multi_items_coro(_UnusedTarget(), prefixes)


class _UnusedTarget:
    def send(self, value):
        raise AssertionError("no result should be produced")


def test_multi_items_only_matched_branches_build_objects(backend, monkeypatch):
    # Memory is bounded by concurrently matched, unfinished objects: the large
    # unmatched branch must never instantiate an ObjectBuilder.
    created = []
    original_init = ObjectBuilder.__init__

    def tracking_init(self, *args, **kwargs):
        created.append(self)
        original_init(self, *args, **kwargs)

    monkeypatch.setattr(ObjectBuilder, "__init__", tracking_init)
    json = (
        b'{"huge": {"a": [1, 2, 3], "b": {"c": 9}}, '
        b'"sel": {"x": 1}, "sel2": [4, 5]}'
    )
    result = list(backend.multi_items(json, ["sel", "sel2"]))
    assert result == [
        ("sel", {"x": 1}),
        ("sel2", [4, 5]),
    ]
    assert len(created) == 2


def test_common_multi_items_is_deprecated(backend):
    events = backend.parse(io.BytesIO(b'{"a": 1, "b": [10]}'))
    with pytest.warns(DeprecationWarning):
        result = list(common.multi_items(events, ["a", "b.item"]))
    assert result == [("a", 1), ("b.item", 10)]
