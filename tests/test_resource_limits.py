"""Tests for the resource limits supported by the python backend."""

import io

import pytest

import ijson
from ijson import common


python_backend = ijson.get_backend('python')


class ChunkReader:
    def __init__(self, chunks):
        self.chunks = list(chunks)
        self.index = 0

    def read(self, size):
        if size == 0 or self.index == len(self.chunks):
            return b''
        chunk = self.chunks[self.index]
        self.index += 1
        return chunk


def _parse(data, **limits):
    return list(python_backend.basic_parse(io.BytesIO(data), **limits))


def _assert_limit_error(data, expected_name, expected_position, expected_limit,
                        **limits):
    with pytest.raises(common.JSONLimitError) as exc:
        _parse(data, **limits)
    error = exc.value
    assert error.limit_name == expected_name
    assert error.position == expected_position
    assert error.limit == expected_limit
    assert expected_name in str(error)


def test_string_length_counts_decoded_characters():
    assert _parse(b'"\\u0041\\u0042"', max_string_length=2) == [
        ('string', 'AB')
    ]
    assert _parse(b'"\\ud83d\\udca9"', max_string_length=1) == [
        ('string', '\U0001f4a9')
    ]
    _assert_limit_error(
        b'"\\ud83d\\udca9x"', 'max_string_length', 0, 1,
        max_string_length=1
    )


def test_string_length_is_checked_while_chunks_accumulate():
    reader = ChunkReader([b'"', b'a', b'b', b'c', b'd', b'"'])
    with pytest.raises(common.JSONLimitError):
        list(python_backend.basic_parse(reader, max_string_length=3))
    assert reader.index == 5


def test_utf8_fragments_and_escapes_do_not_break_string_limit():
    emoji = '\U0001f4a9'.encode('utf-8')
    reader = ChunkReader([b'"', emoji[:1], emoji[1:2], emoji[2:3], emoji[3:], b'"'])
    assert list(python_backend.basic_parse(reader, max_string_length=1)) == [
        ('string', '\U0001f4a9')
    ]

    reader = ChunkReader([b'"\\ud83d', b'\\udca9"'])
    assert list(python_backend.basic_parse(reader, max_string_length=1)) == [
        ('string', '\U0001f4a9')
    ]


def test_number_length_counts_input_token_characters():
    assert _parse(b'-1e+5', max_number_length=5) == [
        ('number', -100000)
    ]
    _assert_limit_error(
        b'-1e+5', 'max_number_length', 0, 4,
        max_number_length=4
    )
    assert _parse(b'true', max_number_length=1) == [('boolean', True)]


def test_number_length_is_checked_while_chunks_accumulate():
    reader = ChunkReader([b'1'] * 10)
    with pytest.raises(common.JSONLimitError):
        list(python_backend.basic_parse(reader, max_number_length=3))
    assert reader.index == 4


def test_root_container_has_depth_one():
    assert _parse(b'[]', max_depth=1) == [
        ('start_array', None), ('end_array', None)
    ]
    _assert_limit_error(b'[[]]', 'max_depth', 1, 1, max_depth=1)


def test_previous_events_are_kept_but_over_limit_token_is_not_emitted():
    events = ijson.sendable_list()
    coro = python_backend.basic_parse_coro(events, max_number_length=2)
    with pytest.raises(common.JSONLimitError):
        coro.send(b'[12,345]')
    assert events == [('start_array', None), ('number', 12)]


def test_other_backends_reject_limits():
    for backend_name in ijson.ALL_BACKENDS:
        if backend_name == 'python':
            continue
        try:
            backend = ijson.get_backend(backend_name)
        except ImportError:
            continue
        if backend.capabilities.resource_limits:
            continue
        with pytest.raises(ValueError, match='not supported'):
            backend.basic_parse(io.BytesIO(b'[]'), max_depth=1)
