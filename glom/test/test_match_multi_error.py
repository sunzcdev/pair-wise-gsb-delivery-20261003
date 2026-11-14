"""Tests for Match's optional multi-error (collect) mode."""

import pytest

from glom import glom
from glom.matching import (
    Match, MatchError, Or, And, Optional, Required, Regex, Switch,
    MatchProblem, MultiMatchError, MatchReport, DEFAULT_MAX_PROBLEMS)


def test_collect_returns_report_and_problem_shape():
    report = Match({'id': int, 'name': str}).collect({'id': 'x', 'name': 1})
    assert isinstance(report, MatchReport)
    assert report.ok is False
    assert bool(report) is False
    assert len(report) == 2
    assert all(isinstance(p, MatchProblem) for p in report.problems)
    assert report.as_dicts() == [
        {'path': ['id'], 'expected': 'int', 'actual_type': 'str'},
        {'path': ['name'], 'expected': 'str', 'actual_type': 'int'},
    ]
    paths = [p.path for p in report]
    assert paths == [('id',), ('name',)]
    path, expected, actual_type = report.problems[0]
    assert (path, expected, actual_type) == (('id',), 'int', 'str')


def test_valid_target_produces_empty_truthy_report():
    report = Match({'id': int}).collect({'id': 1})
    assert report.ok is True
    assert bool(report) is True
    assert report.problems == []
    assert report.truncated is False
    assert report.as_dicts() == []


def test_sibling_fields_all_checked_in_order():
    spec = Match({'first': int, 'second': int, 'third': int})
    report = spec.collect({'first': 'a', 'second': 'b', 'third': 'c'})
    assert [p.path for p in report.problems] == [
        ('first',), ('second',), ('third',)]
    assert all(p.expected == 'int' and p.actual_type == 'str'
               for p in report.problems)


def test_nested_paths_include_indexes_and_keys():
    spec = Match({'items': [{'id': int}]})
    report = spec.collect(
        {'items': [{'id': 'x'}, {'id': 2}, {'id': 'y'}]})
    assert report.as_dicts() == [
        {'path': ['items', 0, 'id'], 'expected': 'int',
         'actual_type': 'str'},
        {'path': ['items', 2, 'id'], 'expected': 'int',
         'actual_type': 'str'},
    ]


def test_parent_type_error_does_not_fabricate_missing_children():
    report = Match({'a': int, 'b': str}).collect('not-a-dict')
    assert report.as_dicts() == [
        {'path': [], 'expected': 'dict', 'actual_type': 'str'}]

    report = Match({'items': [int]}).collect({'items': 5})
    assert report.as_dicts() == [
        {'path': ['items'], 'expected': 'list', 'actual_type': 'int'}]


def test_optional_and_defaults_keep_meaning():
    report = Match({Optional('x'): int, 'y': int}).collect({'y': 1})
    assert report.ok

    result = glom({}, Match({Optional('x', default=5): int}))
    assert result == {'x': 5}

    report = Match({Required(str): int}).collect({})
    assert report.problems[0].actual_type == 'missing'

    report = Match({'a': int, 'b': int}).collect({'a': 'x', 'b': 'y'})
    assert [p.actual_type for p in report.problems] == ['str', 'str']
    assert all(p.actual_type != 'missing' for p in report.problems)


def test_or_success_does_not_leak_branch_failures():
    spec = Match({'v': Or(int, str)})
    assert spec.collect({'v': 1}).ok
    assert spec.collect({'v': 's'}).ok

    report = Match(Or(int, str)).collect(1.5)
    assert report.as_dicts() == [
        {'path': [], 'expected': 'int or str', 'actual_type': 'float'}]

    report = Match({'v': Or(int, {'id': int})}).collect({'v': {'id': 'x'}})
    assert len(report.problems) == 1
    assert report.problems[0].path == ('v',)


def test_and_short_circuits_like_default_mode():
    report = Match(And(int, lambda x: x > 0)).collect('nope')
    assert len(report.problems) == 1
    assert Match(And(int, lambda x: x > 0)).collect(3).ok


def test_list_alternative_patterns():
    report = Match([int, str]).collect([1, 'a', 1.5])
    assert report.as_dicts() == [
        {'path': [2], 'expected': 'one of [int, str]',
         'actual_type': 'float'}]


def test_tuple_position_problems_and_length():
    report = Match((int, str)).collect(('x', 2))
    assert report.as_dicts() == [
        {'path': [0], 'expected': 'int', 'actual_type': 'str'},
        {'path': [1], 'expected': 'str', 'actual_type': 'int'},
    ]
    report = Match((int, str)).collect((1, 2, 3))
    assert report.problems[0].actual_type == 'tuple of length 3'


def test_max_problems_truncates_and_stops_traversal():
    spec = Match({'a': int, 'b': int, 'c': int})
    report = spec.collect({'a': 'x', 'b': 'y', 'c': 'z'}, max_problems=2)
    assert report.truncated is True
    assert len(report.problems) == 2

    report = Match(int).collect('x', max_problems=0)
    assert report.truncated is True
    assert report.problems == []

    report = spec.collect({'a': 'x', 'b': 'y', 'c': 'z'}, max_problems=None)
    assert report.truncated is False
    assert len(report.problems) == 3


def test_default_cap_is_applied():
    spec = Match({str: int})
    target = {str(i): 'bad' for i in range(DEFAULT_MAX_PROBLEMS + 5)}
    report = spec.collect(target)
    assert report.truncated is True
    assert len(report.problems) == DEFAULT_MAX_PROBLEMS


def test_multi_error_kwarg_raises_structured_error():
    with pytest.raises(MultiMatchError) as exc:
        glom({'a': 'x', 'b': 'y'},
             Match({'a': int, 'b': int}, multi_error=True))
    error = exc.value
    assert isinstance(error, MultiMatchError)
    assert len(error.problems) == 2
    assert error.truncated is False
    rendered = str(error)
    assert 'problem(s)' in rendered
    assert "at 'a'" in rendered


def test_default_raise_mode_unchanged():
    with pytest.raises(MatchError) as exc:
        glom({'a': 'x', 'b': 'y'}, Match({'a': int, 'b': int}))
    assert type(exc.value).__name__ == 'TypeMatchError'

    assert glom(1, Match(2, default=3)) == 3
    assert glom(1, Match(2, default=3, multi_error=True)) == 3


def test_sensitive_actual_values_never_in_report():
    secret = 'super-secret-token-value'
    report = Match({'password': int}).collect({'password': secret})
    rendered = str(report) + str(report.as_dicts())
    assert secret not in rendered
    assert report.problems[0].actual_type == 'str'

    rendered = str(Match('expected-literal').collect(secret))
    assert secret not in rendered


def test_regex_and_callable_failures_are_structured():
    assert Match(Regex('a+')).collect(3).problems[0].actual_type == 'mismatch'
    assert Match(len).collect([]).problems[0].expected == 'len'


def test_switch_does_not_leak_branch_failures():
    spec = Match(Switch([(int, 'n'), (str, 's')]))
    report = spec.collect(1.5)
    assert len(report.problems) == 1
    assert report.problems[0].path == ()
    assert spec.collect('x').problems


def test_problem_equality_and_repr():
    p = MatchProblem(('a', 0), 'int', 'str')
    assert p == MatchProblem(('a', 0), 'int', 'str')
    assert p != MatchProblem(('a', 1), 'int', 'str')
    assert repr(p) == (
        "MatchProblem(path=['a', 0], expected='int', actual_type='str')")
