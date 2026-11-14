"""Tests for the ijson.basic_parse method"""

import itertools
import sys
import threading
from decimal import Decimal

import pytest
from ijson import common, utils

from .test_base import ARRAY_JSON, EMBEDDED_NUL_JSON, ARRAY_JSON_EVENTS, INCOMPLETE_JSONS, INCOMPLETE_JSON_TOKENS, INVALID_JSONS, JSON, JSON_EVENTS, SCALAR_JSON, SURROGATE_PAIRS_JSON, STRINGS_JSON


def _raises_json_error(adaptor, json, **kwargs):
    with pytest.raises(common.JSONError):
        adaptor.basic_parse(json, **kwargs)


def _raises_incomplete_json_error(adaptor, json):
    with pytest.raises(common.IncompleteJSONError):
        adaptor.basic_parse(json)


def test_basic_parse(adaptor):
    assert JSON_EVENTS == adaptor.basic_parse(JSON)


def test_basic_parse_threaded(adaptor):
    thread = threading.Thread(target=test_basic_parse, args=(adaptor,))
    thread.start()
    thread.join()


def test_basic_parse_array(adaptor):
    assert ARRAY_JSON_EVENTS == adaptor.basic_parse(ARRAY_JSON)


def test_basic_parse_array_threaded(adaptor):
    thread = threading.Thread(target=test_basic_parse_array, args=(adaptor,))
    thread.start()
    thread.join()


def test_scalar(adaptor):
    assert [('number', 0)] == adaptor.basic_parse(SCALAR_JSON)


def test_strings(adaptor):
    events = adaptor.basic_parse(STRINGS_JSON)
    strings = [value for event, value in events if event == 'string']
    assert ['', '"', '\\', '\\\\', '\b\f\n\r\t'] == strings
    assert ('map_key', 'special\t') in events


def test_surrogate_pairs(adaptor):
    event = adaptor.basic_parse(SURROGATE_PAIRS_JSON)[0]
    parsed_string = event[1]
    assert '💩' == parsed_string


def test_embedded_nul(adaptor):
    events = adaptor.basic_parse(EMBEDDED_NUL_JSON)
    assert ('map_key', 'a\x00b') in events
    assert ('string', 'c\x00d') in events


def _get_numbers(adaptor, json, use_float):
    events = adaptor.basic_parse(json, use_float=use_float)
    return [value for event, value in events if event == 'number']


@pytest.mark.parametrize(
    "json, expected_float_type, expected_numbers, use_float",
    (
        (b'[1, 1.0, 1E2]', Decimal, [1, Decimal("1.0"), Decimal("1e2")], False),
        (b'[1, 1.0, 1E2]', float, [1, 1., 100.], True),
        (b'1e400', Decimal, [Decimal('1e400')], False),
        (b'1e-400', Decimal, [Decimal('1e-400')], False),
        (b'1e-400', float, [0], True),
    )
)
def test_numbers(adaptor, json, expected_float_type, expected_numbers, use_float):
    """Check that numbers are correctly parsed"""
    numbers = _get_numbers(adaptor, json, use_float=use_float)
    float_types = set(type(number) for number in numbers)
    float_types -= {int}
    assert 1 == len(float_types)
    assert expected_float_type == next(iter(float_types))
    assert expected_numbers == numbers


def test_32bit_ints(adaptor):
    """Test for 64-bit integers support when using use_float=true"""
    past32bits = 2 ** 32 + 1
    past32bits_as_json = ('%d' % past32bits).encode('utf8')
    if adaptor.backend.capabilities.int64:
        parsed_number = _get_numbers(adaptor, past32bits_as_json, use_float=True)[0]
        assert past32bits == parsed_number
    else:
        _raises_json_error(adaptor, past32bits_as_json, use_float=True)


@pytest.mark.parametrize("json", (b'1e400', b'-1e400'))
def test_max_double(adaptor, json):
    """Check that numbers whose magnitude exceeds MAX_DOUBLE (usually ~1e308) cannot be represented"""
    _raises_json_error(adaptor, json, use_float=True)


@pytest.mark.skipif(not hasattr(sys, "get_int_max_str_digits"),
                    reason="no int max str digits limit")
def test_int_exceeding_max_str_digits(adaptor):
    """Check that integers beyond sys.get_int_max_str_digits() raise int()'s ValueError on all backends"""
    json = b'[%s]' % (b'1' * (sys.get_int_max_str_digits() + 1))
    with pytest.raises(ValueError, match="integer string conversion"):
        adaptor.basic_parse(json)


@pytest.mark.parametrize(
    "json", [
        sign + prefix + suffix
        for sign, prefix, suffix in itertools.product(
            (b'', b'-'),
            (b'00', b'01', b'001'),
            (b'', b'.0', b'e0', b'E0')
        )
    ]
)
def test_invalid_leading_zeros(adaptor, json):
    """Check leading zeros are invalid"""
    if not adaptor.backend.capabilities.invalid_leading_zeros_detection:
        return
    _raises_json_error(adaptor, json)


@pytest.mark.parametrize("json", (b'1e', b'0.1e', b'0E'))
def test_incomplete_exponents(adaptor, json):
    """incomplete exponents are invalid JSON"""
    _raises_json_error(adaptor, json)


@pytest.mark.parametrize("json", (b'1.', b'.1', b'-.1', b'1.e5', b'1.E5'))
def test_incomplete_fractions(adaptor, json):
    """incomplete fractions are invalid JSON"""
    _raises_json_error(adaptor, json)


@pytest.mark.parametrize("json", (b'+1', b'+0', b'+01', b'+.5', b'+1e5'))
def test_leading_plus_sign(adaptor, json):
    """numbers with a leading plus sign are invalid JSON"""
    _raises_json_error(adaptor, json)


def test_incomplete(adaptor):
    for json in INCOMPLETE_JSONS:
        _raises_incomplete_json_error(adaptor, json)


def test_incomplete_tokens(adaptor):
    if not adaptor.backend.capabilities.incomplete_json_tokens_detection:
        return
    for json in INCOMPLETE_JSON_TOKENS:
        _raises_incomplete_json_error(adaptor, json)


def test_invalid(adaptor):
    for json in INVALID_JSONS:
        if not adaptor.backend.capabilities.incomplete_json_tokens_detection and json == INVALID_JSON_WITH_DANGLING_JUNK:
            continue
        _raises_json_error(adaptor, json)


def test_comments(adaptor):
    json = b'{"a": 2 /* a comment */}'
    if adaptor.backend.capabilities.c_comments:
        events = adaptor.basic_parse(json, allow_comments=True)
        assert events is not None
    else:
        with pytest.raises(ValueError):
            adaptor.basic_parse(json, allow_comments=True)


@pytest.mark.parametrize("json, expected", (
    # line comments
    (b'// header\n1', [('number', 1)]),
    (b'1 // line comment to EOF', [('number', 1)]),
    (b'{"a": 1, // tail\n "b": 2}',
     [('start_map', None), ('map_key', 'a'), ('number', 1),
      ('map_key', 'b'), ('number', 2), ('end_map', None)]),
    (b'[1// c\r\n, 2// c\n]',
     [('start_array', None), ('number', 1), ('number', 2),
      ('end_array', None)]),
    # block comments
    (b'/* before */[1, /* middle */ 2]/* after */',
     [('start_array', None), ('number', 1), ('number', 2),
      ('end_array', None)]),
    (b'/**/1/**/', [('number', 1)]),
    (b'/* multi\nline\r\ncomment */null', [('null', None)]),
    # comments are allowed wherever whitespace is
    (b'/*a*/{/*b*/"k"/*c*/:/*d*/[/*e*/1/*f*/]/*g*/}/*h*/',
     [('start_map', None), ('map_key', 'k'), ('start_array', None),
      ('number', 1), ('end_array', None), ('end_map', None)]),
))
def test_comments_allowed(adaptor, json, expected):
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    assert expected == adaptor.basic_parse(json, allow_comments=True)


def test_comments_disabled_by_default(adaptor):
    """Comments must be rejected unless explicitly enabled"""
    for json in (b'1 // c', b'1 /* c */', b'// c\n1', b'/* c */1'):
        _raises_json_error(adaptor, json)
        _raises_json_error(adaptor, json, allow_comments=False)


def test_comments_inside_strings_are_literal(adaptor):
    """Slashes and comment-like text inside strings is parsed verbatim"""
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    json = b'["// not a comment", "/* neither */", "a/b", "escaped\\/slash"]'
    events = adaptor.basic_parse(json, allow_comments=True)
    strings = [value for event, value in events if event == 'string']
    assert ['// not a comment', '/* neither */', 'a/b', 'escaped/slash'] == strings
    # the same string content must parse identically without the option
    assert events == adaptor.basic_parse(json)


def test_comments_do_not_join_numbers(adaptor):
    """A comment can't glue two number fragments into a single number"""
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    # two complete values separated by a comment are two values, not one
    events = adaptor.basic_parse(
        b'1/* comment */2', allow_comments=True, multiple_values=True)
    assert [('number', 1), ('number', 2)] == events
    # a comment inside a single number leaves it invalid
    for json in (b'1/* c */2', b'-/* c */1', b'1./* c */5', b'1e/* c */2'):
        with pytest.raises(common.JSONError):
            adaptor.basic_parse(json, allow_comments=True)


def test_unterminated_block_comment(adaptor):
    """An unterminated block comment is reported as truncated content"""
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    for json in (b'1 /* never ends', b'/* never ends', b'{"a": 1}/* nope',
                 b'/*x*', b'1 /*/ still open'):
        with pytest.raises(common.IncompleteJSONError):
            adaptor.basic_parse(json, allow_comments=True)


def test_empty_and_comment_only_inputs(adaptor):
    """Inputs without any JSON value are incomplete, even with comments"""
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    for json in (b'', b'   ', b'// only a comment', b'/* only a comment */',
                 b' // c\n '):
        with pytest.raises(common.IncompleteJSONError):
            adaptor.basic_parse(json, allow_comments=True)


def test_opening_star_does_not_close_block_comment(adaptor):
    """The '*' opening a block comment can't simultaneously close it"""
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    # '/*/' is still an open block comment; the real close comes later
    events = adaptor.basic_parse(
        b'1 /*/ not closed yet */ 2', allow_comments=True,
        multiple_values=True)
    assert [('number', 1), ('number', 2)] == events
    with pytest.raises(common.IncompleteJSONError):
        adaptor.basic_parse(b'1 /*/', allow_comments=True)


@pytest.mark.pull_only
@pytest.mark.parametrize("buf_size", [1, 2, 3, 5, 7, 16, 64 * 1024])
def test_comment_boundaries_across_chunks(adaptor, buf_size):
    """Comment delimiters split across input chunks are handled correctly"""
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    json = (b'/* head */ {"a": [1, /* mid */ 2],'
            b' "b": "// literal"} // tail\n')
    expected = [
        ('start_map', None), ('map_key', 'a'),
        ('start_array', None), ('number', 1), ('number', 2),
        ('end_array', None), ('map_key', 'b'), ('string', '// literal'),
        ('end_map', None),
    ]
    result = adaptor.basic_parse(json, allow_comments=True, buf_size=buf_size)
    assert expected == result


def test_comments_are_streamed_without_buffering_whole_input(backend):
    """A long comment must not force the whole input to be buffered"""
    if not backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    # tens of thousands of small chunks forming one long block comment; a
    # whole-input implementation (e.g. regex substitution) would have to keep
    # every chunk alive before seeing the closing delimiter
    chunks = [b'/* padding '] * 5_000
    events = utils.sendable_list()
    coro = backend.basic_parse_coro(events, allow_comments=True)
    for chunk in chunks:
        coro.send(chunk)
    with pytest.raises(common.IncompleteJSONError):
        coro.close()


def test_lone_slash_is_not_a_comment(adaptor):
    """A slash that doesn't start a comment is still invalid JSON"""
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    _raises_json_error(adaptor, b'1 / 2', allow_comments=True)


def test_comment_positions_are_original(adaptor):
    """Removing comments must not shift the positions reported for lexemes"""
    if adaptor.backend.backend_name != 'python':
        pytest.skip("position introspection targets the python lexer")
    if not adaptor.backend.capabilities.c_comments:
        pytest.skip("backend does not support comments")
    plain = b'[1, 2]'
    commented = b'[/* x */1, /* y */ 2]'

    # compare against the low-level lexer positions directly
    plain_positions = _lexer_positions(
        adaptor.backend, plain, allow_comments=False)
    commented_positions = _lexer_positions(
        adaptor.backend, commented, allow_comments=True)
    assert plain_positions == commented_positions


def _lexer_positions(backend, json, allow_comments):
    from ijson import utils
    from ijson.backends.python import utf8_encoder
    sink = utils.sendable_list()
    lexer = backend.CommentableLexer if allow_comments else backend.Lexer
    coro = utf8_encoder(lexer(backend.parse_value(sink, False, False)))
    for index in range(len(json)):
        coro.send(json[index:index + 1])
    coro.close()
    return [pos for pos, symbol in sink if symbol is not None]


def test_multiple_values_raises_if_not_supported(adaptor):
    """Test that setting multiple_values raises if not supported"""
    if not adaptor.backend.capabilities.multiple_values:
        with pytest.raises(ValueError):
            adaptor.basic_parse("", multiple_values=True)


def test_multiple_values(adaptor):
    """Test that multiple_values are supported"""
    multiple_json = JSON + JSON + JSON
    with pytest.raises(common.JSONError):
        adaptor.basic_parse(multiple_json)
    with pytest.raises(common.JSONError):
        adaptor.basic_parse(multiple_json, multiple_values=False)
    result = adaptor.basic_parse(multiple_json, multiple_values=True)
    assert JSON_EVENTS + JSON_EVENTS + JSON_EVENTS == result
