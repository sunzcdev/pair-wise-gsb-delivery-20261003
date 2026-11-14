"""Tests for the ijson.items method"""

import collections
import sys

import pytest

from .test_base import ARRAY_JSON, ARRAY_JSON_OBJECT, EMPTY_MEMBER_TEST_CASES, JSON, JSON_OBJECT

from ijson import DuplicateKeyError, DuplicateKeyPolicy, JSONError


DUPLICATE_JSON = (
    b'{"a": 1, "a": 2, "n": {"x": 1, "x": 2}, '
    b'"arr": [{"k": 1, "k": 2}, {"k": 9}]}'
)
DUPLICATE_KEEP_FIRST = {
    "a": 1,
    "n": {"x": 1},
    "arr": [{"k": 1}, {"k": 9}],
}


@pytest.mark.parametrize("policy", [
    DuplicateKeyPolicy.KEEP_FIRST,
    "keep_first",
    "KEEP_FIRST",
])
def test_items_duplicate_keys_keep_first(adaptor, policy):
    result = adaptor.items(DUPLICATE_JSON, "", duplicate_keys=policy)
    assert result == [DUPLICATE_KEEP_FIRST]


def test_items_duplicate_keys_keep_last_is_default(adaptor):
    default = adaptor.items(DUPLICATE_JSON, "")
    explicit = adaptor.items(
        DUPLICATE_JSON, "", duplicate_keys=DuplicateKeyPolicy.KEEP_LAST
    )
    assert default == explicit
    assert default == [{
        "a": 2,
        "n": {"x": 2},
        "arr": [{"k": 2}, {"k": 9}],
    }]


@pytest.mark.parametrize("policy", [DuplicateKeyPolicy.REJECT, "reject"])
def test_items_duplicate_keys_reject(adaptor, policy):
    with pytest.raises(DuplicateKeyError):
        adaptor.items(DUPLICATE_JSON, "", duplicate_keys=policy)


def test_items_duplicate_keys_uses_decoded_key(adaptor):
    json = b'{"a": 1, "\\u0061": 2}'
    assert adaptor.items(json, "", duplicate_keys=DuplicateKeyPolicy.KEEP_FIRST) == [{"a": 1}]
    with pytest.raises(DuplicateKeyError):
        adaptor.items(json, "", duplicate_keys=DuplicateKeyPolicy.REJECT)


def test_items_keep_first_validates_ignored_value(adaptor):
    bad_json = b'{"a": 1, "a": [1, 2,]}'
    with pytest.raises(JSONError):
        adaptor.items(bad_json, "", duplicate_keys=DuplicateKeyPolicy.KEEP_FIRST)


def test_items_duplicate_keys_with_custom_map_type(adaptor):
    result = adaptor.items(
        DUPLICATE_JSON, "", map_type=collections.OrderedDict,
        duplicate_keys=DuplicateKeyPolicy.KEEP_FIRST,
    )[0]
    assert isinstance(result, collections.OrderedDict)
    assert result == DUPLICATE_KEEP_FIRST
    with pytest.raises(DuplicateKeyError):
        adaptor.items(
            DUPLICATE_JSON, "", map_type=collections.OrderedDict,
            duplicate_keys=DuplicateKeyPolicy.REJECT,
        )

def test_items(adaptor):
    assert [JSON_OBJECT] == adaptor.items(JSON, '')


def test_items_array(adaptor):
    assert [ARRAY_JSON_OBJECT] == adaptor.items(ARRAY_JSON, '')


def test_items_twodictlevels(adaptor):
    json = b'{"meta":{"view":{"columns":[{"id": -1}, {"id": -2}]}}}'
    ids = adaptor.items(json, 'meta.view.columns.item.id')
    assert 2 == len(ids)
    assert [-2,-1], sorted(ids)


@pytest.mark.parametrize(
    "json, prefix, expected_items",
    (
        (b'{"0.1": 0}', '0.1', [0]),
        (b'{"0.1": [{"a.b": 0}]}', '0.1.item.a.b', [0]),
        (b'{"0.1": 0, "0": {"1": 1}}', '0.1', [0, 1]),
        (b'{"abc.def": 0}', 'abc.def', [0]),
        (b'{"abc.def": 0}', 'abc', []),
        (b'{"abc.def": 0}', 'def', []),
    )
)
def test_items_with_dotted_name(adaptor, json, prefix, expected_items):
    assert expected_items == adaptor.items(json, prefix)


def test_map_type(adaptor):
    obj = adaptor.items(JSON, '')[0]
    assert isinstance(obj, dict)
    obj = adaptor.items(JSON, '', map_type=collections.OrderedDict)[0]
    assert isinstance(obj, collections.OrderedDict)


@pytest.mark.parametrize("test_case", [
    pytest.param(value, id=name) for name, value in EMPTY_MEMBER_TEST_CASES.items()
])
def test_items_empty_member(adaptor, test_case):
    assert test_case.items == adaptor.items(test_case.json, test_case.prefix)


def test_multiple_values_raises_if_not_supported(adaptor):
    """Test that setting multiple_values raises if not supported"""
    if not adaptor.backend.capabilities.multiple_values:
        with pytest.raises(ValueError):
            adaptor.items("", "", multiple_values=True)


def test_multiple_values(adaptor):
    """Test that multiple_values are supported"""
    multiple_json = JSON + JSON + JSON
    with pytest.raises(JSONError):
        adaptor.items(multiple_json, "")
    with pytest.raises(JSONError):
        adaptor.items(multiple_json, "", multiple_values=False)
    result = adaptor.items(multiple_json, "", multiple_values=True)
    assert [JSON_OBJECT, JSON_OBJECT, JSON_OBJECT] == result


def test_coro_needs_input_with_three_elements(backend):
    int_element_parse_events = list(backend.parse(b'0'))
    # all good
    assert [0] == list(backend.items(int_element_parse_events, ''))
    # one more element in event
    with pytest.raises(ValueError, match="too many values"):
        next(backend.items((event + ('extra dummy',) for event in int_element_parse_events), ''))
    # one less
    with pytest.raises(ValueError, match="not enough values"):
        next(backend.items((event[:-1] for event in int_element_parse_events), ''))
    # not an iterable
    with pytest.raises(TypeError, match="cannot unpack"):
        next(backend.items([None], ''))


def test_user_events(backend):
    """
    User-provided events work correct -- yajl2_c used to fail with event names
    that weren't generated by itself.
    """
    embedded_empty_list_events = [
        ('', 'start_array', None),
        ('item', 'start_array', None),
        ('item', 'end_array', None),
        ('', 'end_array', None),
    ]
    assert [[]] == list(backend.items(embedded_empty_list_events, 'item'))


@pytest.mark.skipif(not hasattr(sys, "get_int_max_str_digits"),
                    reason="no int max str digits limit")
def test_items_int_exceeding_max_str_digits(adaptor):
    """Check that items raises int()'s ValueError for integers beyond sys.get_int_max_str_digits()"""
    json = b'[%s]' % (b'1' * (sys.get_int_max_str_digits() + 1))
    with pytest.raises(ValueError, match="integer string conversion"):
        adaptor.items(json, 'item')
