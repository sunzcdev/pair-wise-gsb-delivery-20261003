"""Tests for the original-input error locations reported by the python backend"""

import io

import pytest

from ijson import common
from ijson.backends import python as python_backend
from ijson.backends.python import ERROR_CONTEXT_SIZE


def parse(data, **kwargs):
    return list(python_backend.basic_parse(io.BytesIO(data), **kwargs))


def test_syntax_error_location():
    data = '{\r\n"kéy": "值",\r\n  bad\r\n}'.encode('utf-8')
    with pytest.raises(common.JSONError) as exc_info:
        parse(data)
    error = exc_info.value
    assert not isinstance(error, common.IncompleteJSONError)
    assert error.pos == data.index(b'bad')
    assert error.line == 3
    assert error.column == 3


def test_location_is_buf_size_independent():
    data = '{\r\n"kéy": "值",\r\n  bad\r\n}'.encode('utf-8')
    locations = []
    for buf_size in (1, 2, 3, 5, 16, 64 * 1024):
        with pytest.raises(common.JSONError) as exc_info:
            parse(data, buf_size=buf_size)
        error = exc_info.value
        locations.append((error.pos, error.line, error.column))
    assert len(set(locations)) == 1


def test_incomplete_error_points_at_end_of_input():
    data = b'{"a": [1, 2'
    with pytest.raises(common.IncompleteJSONError) as exc_info:
        parse(data, buf_size=3)
    error = exc_info.value
    assert error.pos == len(data)
    assert error.line == 1
    assert error.column == len(data) + 1


def test_incomplete_string_error():
    data = b'{"a": "abc'
    with pytest.raises(common.IncompleteJSONError) as exc_info:
        parse(data, buf_size=2)
    assert exc_info.value.pos == len(data)


def test_incomplete_error_multiline():
    data = b'{\n  "a": 1,\n  "b": [1,\n'
    with pytest.raises(common.IncompleteJSONError) as exc_info:
        parse(data, buf_size=4)
    error = exc_info.value
    assert error.pos == len(data)
    assert error.line == 4
    assert error.column == 1


def test_multibyte_characters_do_not_shift_byte_offset():
    prefix = '["日本語", "é", '
    data = (prefix + '?]').encode('utf-8')
    with pytest.raises(common.JSONError) as exc_info:
        parse(data, buf_size=2)
    error = exc_info.value
    assert error.pos == data.index(b'?')
    assert error.line == 1
    assert error.column == len(prefix) + 1


def test_escape_spanning_chunks_does_not_shift_location():
    data = b'["ab\\\\", ?]'
    with pytest.raises(common.JSONError) as exc_info:
        parse(data, buf_size=3)
    assert exc_info.value.pos == data.index(b'?')


def test_error_context_is_bounded():
    data = b'[' + b'1,' * 1000 + b'?]'
    with pytest.raises(common.JSONError) as exc_info:
        parse(data)
    error = exc_info.value
    assert error.context is not None
    assert len(error.context) <= 2 * ERROR_CONTEXT_SIZE
    assert '?' in error.context


def test_error_context_can_be_disabled():
    with pytest.raises(common.JSONError) as exc_info:
        parse(b'[?]', error_context=False)
    error = exc_info.value
    assert error.context is None
    assert error.pos == 1
    assert error.line == 1
    assert error.column == 2


def test_error_context_contents():
    data = b'[1, 2, ?]'
    with pytest.raises(common.JSONError) as exc_info:
        parse(data)
    assert exc_info.value.context == data.decode('utf-8')


def test_invalid_utf8_error_location():
    data = b'{"a": "\xff"}'
    with pytest.raises(common.IncompleteJSONError) as exc_info:
        parse(data, buf_size=4)
    assert exc_info.value.pos == data.index(b'\xff')


def test_additional_data_error_location():
    with pytest.raises(common.JSONError) as exc_info:
        parse(b'{} {}')
    error = exc_info.value
    assert error.pos == 3
    assert error.line == 1
    assert error.column == 4


def test_errors_remain_catchable_as_before():
    with pytest.raises(common.JSONError):
        parse(b'[?]')
    with pytest.raises(common.IncompleteJSONError):
        parse(b'[1,')
    with pytest.raises(common.JSONError):
        parse(b'[1,')


def test_normal_events_unchanged():
    events = parse(b'{"a": [1, true, null, "x"]}')
    assert events == [
        ('start_map', None),
        ('map_key', 'a'),
        ('start_array', None),
        ('number', 1),
        ('boolean', True),
        ('null', None),
        ('string', 'x'),
        ('end_array', None),
        ('end_map', None),
    ]
