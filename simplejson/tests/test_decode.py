from __future__ import absolute_import
import decimal
from unittest import TestCase

import sys
import simplejson as json
from simplejson.compat import StringIO, b, binary_type
from simplejson import OrderedDict


class RecordingParser(list):
    def __call__(self, value):
        self.append(value)
        return 'parsed:' + value


class FalsyParser(RecordingParser):
    def __bool__(self):
        return False
    __nonzero__ = __bool__


class UncheckedParser(RecordingParser):
    def __bool__(self):
        raise AssertionError('parser truth value was checked')
    __nonzero__ = __bool__


class MisbehavingBytesSubtype(binary_type):
    def decode(self, encoding=None):
        return "bad decode"
    def __str__(self):
        return "bad __str__"
    def __bytes__(self):
        return b("bad __bytes__")

class TestDecode(TestCase):
    def check_parser_callback(self, parser_type, option, text, values,
                              allow_nan=False):
        for decode in (json.loads, json.JSONDecoder):
            parser = parser_type()
            kwargs = {option: parser, 'allow_nan': allow_nan}
            if decode is json.JSONDecoder:
                result = decode(**kwargs).decode(text)
            else:
                result = decode(text, **kwargs)
            self.assertEqual(result, ['parsed:' + value for value in values])
            self.assertEqual(list(parser), values)

    def test_parse_int_false_callable(self):
        for parser_type in (RecordingParser, FalsyParser):
            self.check_parser_callback(
                parser_type, 'parse_int', '[1, -2, 300]', ['1', '-2', '300'])

    def test_parse_float_false_callable(self):
        for parser_type in (RecordingParser, FalsyParser):
            self.check_parser_callback(
                parser_type, 'parse_float', '[1.5, -2e3]', ['1.5', '-2e3'])

    def test_parse_constant_false_callable(self):
        for parser_type in (RecordingParser, FalsyParser):
            self.check_parser_callback(
                parser_type, 'parse_constant', '[NaN, Infinity, -Infinity]',
                ['NaN', 'Infinity', '-Infinity'])

    def test_parse_constant_false_callable_with_allow_nan(self):
        for parser_type in (RecordingParser, FalsyParser, UncheckedParser):
            self.check_parser_callback(
                parser_type, 'parse_constant', '[NaN, Infinity, -Infinity]',
                ['NaN', 'Infinity', '-Infinity'], allow_nan=True)

    def test_parse_int_does_not_check_truth(self):
        self.check_parser_callback(
            UncheckedParser, 'parse_int', '[1, -2]', ['1', '-2'])

    def test_parse_float_does_not_check_truth(self):
        self.check_parser_callback(
            UncheckedParser, 'parse_float', '[1.5, -2e3]', ['1.5', '-2e3'])

    def test_parse_constant_does_not_check_truth(self):
        self.check_parser_callback(
            UncheckedParser, 'parse_constant', '[NaN, Infinity, -Infinity]',
            ['NaN', 'Infinity', '-Infinity'])

    def test_decimal(self):
        rval = json.loads('1.1', parse_float=decimal.Decimal)
        self.assertTrue(isinstance(rval, decimal.Decimal))
        self.assertEqual(rval, decimal.Decimal('1.1'))

    def test_float(self):
        rval = json.loads('1', parse_int=float)
        self.assertTrue(isinstance(rval, float))
        self.assertEqual(rval, 1.0)

    def test_decoder_optimizations(self):
        # Several optimizations were made that skip over calls to
        # the whitespace regex, so this test is designed to try and
        # exercise the uncommon cases. The array cases are already covered.
        rval = json.loads('{   "key"    :    "value"    ,  "k":"v"    }')
        self.assertEqual(rval, {"key":"value", "k":"v"})

    def test_empty_objects(self):
        s = '{}'
        self.assertEqual(json.loads(s), eval(s))
        s = '[]'
        self.assertEqual(json.loads(s), eval(s))
        s = '""'
        self.assertEqual(json.loads(s), eval(s))

    def test_object_pairs_hook(self):
        s = '{"xkd":1, "kcw":2, "art":3, "hxm":4, "qrt":5, "pad":6, "hoy":7}'
        p = [("xkd", 1), ("kcw", 2), ("art", 3), ("hxm", 4),
             ("qrt", 5), ("pad", 6), ("hoy", 7)]
        self.assertEqual(json.loads(s), eval(s))
        self.assertEqual(json.loads(s, object_pairs_hook=lambda x: x), p)
        self.assertEqual(json.load(StringIO(s),
                                   object_pairs_hook=lambda x: x), p)
        od = json.loads(s, object_pairs_hook=OrderedDict)
        self.assertEqual(od, OrderedDict(p))
        self.assertEqual(type(od), OrderedDict)
        # the object_pairs_hook takes priority over the object_hook
        self.assertEqual(json.loads(s,
                                    object_pairs_hook=OrderedDict,
                                    object_hook=lambda x: None),
                         OrderedDict(p))

    def check_keys_reuse(self, source, loads):
        rval = loads(source)
        (a, b), (c, d) = sorted(rval[0]), sorted(rval[1])
        self.assertIs(a, c)
        self.assertIs(b, d)

    def test_keys_reuse_str(self):
        s = u'[{"a_key": 1, "b_\xe9": 2}, {"a_key": 3, "b_\xe9": 4}]'.encode('utf8')
        self.check_keys_reuse(s, json.loads)

    def test_keys_reuse_unicode(self):
        s = u'[{"a_key": 1, "b_\xe9": 2}, {"a_key": 3, "b_\xe9": 4}]'
        self.check_keys_reuse(s, json.loads)

    def test_empty_strings(self):
        self.assertEqual(json.loads('""'), "")
        self.assertEqual(json.loads(u'""'), u"")
        self.assertEqual(json.loads('[""]'), [""])
        self.assertEqual(json.loads(u'[""]'), [u""])

    def test_raw_decode(self):
        cls = json.decoder.JSONDecoder
        self.assertEqual(
            ({'a': {}}, 9),
            cls().raw_decode("{\"a\": {}}"))
        # http://code.google.com/p/simplejson/issues/detail?id=85
        self.assertEqual(
            ({'a': {}}, 9),
            cls(object_pairs_hook=dict).raw_decode("{\"a\": {}}"))
        # https://github.com/simplejson/simplejson/pull/38
        self.assertEqual(
            ({'a': {}}, 11),
            cls().raw_decode(" \n{\"a\": {}}"))

    def test_bytes_decode(self):
        cls = json.decoder.JSONDecoder
        data = b('"\xe2\x82\xac"')
        self.assertEqual(cls().decode(data), u'\u20ac')
        self.assertEqual(cls(encoding='latin1').decode(data), u'\xe2\x82\xac')
        self.assertEqual(cls(encoding=None).decode(data), u'\u20ac')

        data = MisbehavingBytesSubtype(b('"\xe2\x82\xac"'))
        self.assertEqual(cls().decode(data), u'\u20ac')
        self.assertEqual(cls(encoding='latin1').decode(data), u'\xe2\x82\xac')
        self.assertEqual(cls(encoding=None).decode(data), u'\u20ac')

    def test_bounds_checking(self):
        # https://github.com/simplejson/simplejson/issues/98
        j = json.decoder.JSONDecoder()
        for i in [4, 5, 6, -1, -2, -3, -4, -5, -6]:
            self.assertRaises(ValueError, j.scan_once, '1234', i)
            self.assertRaises(ValueError, j.raw_decode, '1234', i)
        x, y = sorted(['128931233', '472389423'], key=id)
        diff = id(x) - id(y)
        self.assertRaises(ValueError, j.scan_once, y, diff)
        self.assertRaises(ValueError, j.raw_decode, y, i)

    def test_raw_decode_negative_idx(self):
        # Negative offsets are never normalized via Python's indexing, in
        # either mode, and the error keeps the requested coordinate.
        j = json.decoder.JSONDecoder()
        for strict in (False, True):
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode('1', -1, strict_idx=strict)
            self.assertEqual(cm.exception.pos, -1)
            self.assertEqual(cm.exception.msg, 'Expecting value')

    def test_raw_decode_idx_out_of_range(self):
        # An offset past the end of the buffer is an out-of-range argument,
        # not an empty document; it must not be clamped to len(s).
        j = json.decoder.JSONDecoder()
        for strict in (False, True):
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode('1234', 99, strict_idx=strict)
            self.assertEqual(cm.exception.pos, 4)
            self.assertEqual(cm.exception.msg, 'idx out of range')
        # idx == len(s) is still in range and reports "Expecting value"
        # (the buffer holds no more content at that coordinate).
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode('1', 1)
        self.assertEqual(cm.exception.pos, 1)
        self.assertEqual(cm.exception.msg, 'Expecting value')
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode('', 0)
        self.assertEqual(cm.exception.pos, 0)
        self.assertEqual(cm.exception.msg, 'Expecting value')

    def test_raw_decode_whitespace_only_tail(self):
        # The two modes keep "out of range" distinct from "only whitespace
        # left": strict mode fails at idx, the default mode first advances
        # past the whitespace and fails at the end of the buffer.
        j = json.decoder.JSONDecoder()
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode('1   ', 1)
        self.assertEqual(cm.exception.pos, 4)
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode('1   ', 1, strict_idx=True)
        self.assertEqual(cm.exception.pos, 1)
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode('   ', 0)
        self.assertEqual(cm.exception.pos, 3)
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode('   ', 0, strict_idx=True)
        self.assertEqual(cm.exception.pos, 0)
        # Trailing whitespace after a real value is left for the caller.
        self.assertEqual(j.raw_decode('1   '), (1, 1))

    def test_raw_decode_strict_rejects_leading_whitespace(self):
        # strict_idx makes the offset exact: no whitespace is consumed and
        # the failure points at idx instead of the value beyond it.
        j = json.decoder.JSONDecoder()
        self.assertEqual(j.raw_decode(' 1', 0), (1, 2))
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode(' 1', 0, strict_idx=True)
        self.assertEqual(cm.exception.pos, 0)
        # Every JSON whitespace character is covered.
        for ws in (' ', '\t', '\n', '\r'):
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode(ws + '1', 0, strict_idx=True)
            self.assertEqual(cm.exception.pos, 0)

    def test_raw_decode_bom_only_at_buffer_start(self):
        # A BOM is a buffer-level marker, so it is consumed only when idx
        # points at the very beginning -- and never in strict mode. A BOM
        # at a non-zero offset is ordinary invalid JSON content.
        j = json.decoder.JSONDecoder()
        # Unicode BOM (single code point) at the start.
        self.assertEqual(j.raw_decode(u'\ufeff1'), (1, 2))
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode(u'\ufeff1', 0, strict_idx=True)
        self.assertEqual(cm.exception.pos, 0)
        # UTF-8 BOM represented as three characters (text already decoded
        # from bytes elsewhere) at the start.
        self.assertEqual(j.raw_decode('\xef\xbb\xbf1'), (1, 4))
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode('\xef\xbb\xbf1', 0, strict_idx=True)
        self.assertEqual(cm.exception.pos, 0)
        # A BOM encountered mid-buffer must not be treated as a marker.
        for strict in (False, True):
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode(u'1 \ufeff2', 2, strict_idx=strict)
            self.assertEqual(cm.exception.pos, 2)
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode(u'1 \xef\xbb\xbf2', 2, strict_idx=strict)
            self.assertEqual(cm.exception.pos, 2)

    def test_raw_decode_does_not_skip_damaged_content(self):
        # Neither mode scans forward past broken or unrelated characters to
        # find a legal value further along the buffer.
        j = json.decoder.JSONDecoder()
        for strict in (False, True):
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode('x 1', 0, strict_idx=strict)
            self.assertEqual(cm.exception.pos, 0)

    def test_raw_decode_rejects_offsets_inside_tokens(self):
        # An offset into the interior of a token must fail at that offset
        # instead of being rescued by skipping to a later value. Cover the
        # backslash and the 'u' of a \uXXXX escape, and a string interior.
        j = json.decoder.JSONDecoder()
        doc = '["\\u0041"]'
        backslash = doc.index('\\')
        for strict in (False, True):
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode(doc, backslash, strict_idx=strict)
            self.assertEqual(cm.exception.pos, backslash)
        u_idx = doc.index('u')
        for strict in (False, True):
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode(doc, u_idx, strict_idx=strict)
            self.assertEqual(cm.exception.pos, u_idx)
        # Pointing at a character inside a string (past the opening quote).
        for strict in (False, True):
            with self.assertRaises(json.JSONDecodeError) as cm:
                j.raw_decode('"ab"', 1, strict_idx=strict)
            self.assertEqual(cm.exception.pos, 1)

    def test_raw_decode_chained_documents(self):
        # The end index returned for one document feeds the next raw_decode
        # call, and every coordinate stays relative to the original buffer.
        j = json.decoder.JSONDecoder()
        doc = '1 true  false null "x" [1, 2] {"k": 3}'
        expected = [1, True, False, None, 'x', [1, 2], {'k': 3}]
        idx = 0
        values = []
        while idx < len(doc):
            value, idx = j.raw_decode(doc, idx)
            values.append(value)
        self.assertEqual(values, expected)
        self.assertEqual(idx, len(doc))
        # The same loop works with exact offsets under strict_idx as long
        # as the caller skips the inter-document whitespace; the parser
        # itself never moves the start.
        idx = 0
        values = []
        while idx < len(doc):
            while idx < len(doc) and doc[idx] in ' \t\n\r':
                idx += 1
            if idx == len(doc):
                break
            value, idx = j.raw_decode(doc, idx, strict_idx=True)
            values.append(value)
        self.assertEqual(values, expected)
        self.assertEqual(idx, len(doc))

    def test_raw_decode_end_index_uses_original_coordinates(self):
        # BOM and leading whitespace shift the start, but the returned end
        # index remains an offset into the buffer the caller passed in.
        j = json.decoder.JSONDecoder()
        self.assertEqual(j.raw_decode('  [1]'), ([1], 5))
        self.assertEqual(j.raw_decode(u'\ufeff[1]'), ([1], 4))
        self.assertEqual(j.raw_decode('\xef\xbb\xbf[1]'), ([1], 6))
        value, end = j.raw_decode('xx[1]yy', 2)
        self.assertEqual(value, [1])
        self.assertEqual(end, 5)
        self.assertEqual('xx[1]yy'[end:], 'yy')

    def test_raw_decode_text_and_bytes_share_start_rules(self):
        # bytes are decoded to text first, so decode() applies raw_decode's
        # start rules to the decoded text with the same coordinates;
        # raw_decode itself still rejects bytes on Python 3.
        j = json.decoder.JSONDecoder()
        data = b'\xef\xbb\xbf  [1, 2]'
        text = data.decode('utf-8')
        self.assertEqual(j.decode(data), [1, 2])
        self.assertEqual(j.decode(text), [1, 2])
        if sys.version_info[0] >= 3:
            self.assertRaises(TypeError, j.raw_decode, data)
        # The decoded text view honors the same BOM/whitespace rules.
        self.assertEqual(j.raw_decode(text), ([1, 2], len(text)))
        with self.assertRaises(json.JSONDecodeError) as cm:
            j.raw_decode(text, 0, strict_idx=True)
        self.assertEqual(cm.exception.pos, 0)

    def test_array_hook(self):
        result = json.loads('[1, [2, 3], 4]', array_hook=tuple)
        self.assertEqual(result, (1, (2, 3), 4))
        self.assertIsInstance(result, tuple)

    def test_array_hook_empty(self):
        result = json.loads('[]', array_hook=tuple)
        self.assertEqual(result, ())
        self.assertIsInstance(result, tuple)

    def test_array_hook_nested_in_object(self):
        result = json.loads('{"a": [1, 2]}', array_hook=tuple)
        self.assertEqual(result, {"a": (1, 2)})

    def test_array_hook_with_object_hook(self):
        # Both hooks applied together: array_hook on arrays,
        # object_hook on objects
        def freeze_dict(d):
            return tuple(sorted(d.items()))
        s = '{"xkd": [[1], [2], [3]]}'
        result = json.loads(s, object_hook=freeze_dict, array_hook=tuple)
        self.assertIsInstance(result, tuple)
        # object_hook converts dict to sorted tuple of items
        xkd_value = dict(result)['xkd']
        self.assertIsInstance(xkd_value, tuple)
        for item in xkd_value:
            self.assertIsInstance(item, tuple)

    def test_array_hook_none(self):
        # array_hook=None should behave like no hook
        result = json.loads('[1, 2]', array_hook=None)
        self.assertEqual(result, [1, 2])
        self.assertIsInstance(result, list)

    def test_trailing_comma_unexpected_data(self):
        # Matches CPython's test_unexpected_data for trailing commas
        test_cases = [
            ('[42,]', 'Illegal trailing comma before end of array'),
            ('{"spam":42,}', 'Illegal trailing comma before end of object'),
            ('{"spam":42 , }', 'Illegal trailing comma before end of object'),
            ('[123  , ]', 'Illegal trailing comma before end of array'),
        ]
        for doc, expected_msg in test_cases:
            try:
                json.loads(doc)
                self.fail("Expected JSONDecodeError for %r" % (doc,))
            except json.JSONDecodeError as e:
                self.assertIn(expected_msg, str(e),
                    "Wrong error for %r: %s" % (doc, e))

    def test_trailing_comma_position(self):
        # The reported position is the comma itself, not the closing
        # bracket and not whatever whitespace happens to precede it.
        # Same positions as the stdlib json module. Runs on both the C
        # and the pure-Python scanner.
        test_cases = [
            ('[42,]', 3),
            ('[42, ]', 3),
            ('[42,\n]', 3),
            ('[123  , ]', 6),
            ('{"spam":42,}', 10),
            ('{"spam":42 , }', 11),
            ('{"spam":42,\n}', 10),
            ('[[1, 2],]', 7),
            ('{"a": {"b": 1,},}', 13),
        ]
        for doc, expected_pos in test_cases:
            try:
                json.loads(doc)
                self.fail("Expected JSONDecodeError for %r" % (doc,))
            except json.JSONDecodeError as e:
                self.assertEqual(doc[e.pos], ',',
                    "Position %d of %r is %r, not the comma"
                    % (e.pos, doc, doc[e.pos]))
                self.assertEqual(e.pos, expected_pos,
                    "Wrong position for %r: %d" % (doc, e.pos))

    def test_nonascii_digits_rejected(self):
        # Non-ASCII digits (e.g. fullwidth digits) must not be accepted
        # as JSON numbers. Matches CPython gh-125687.
        for num in [u"1\uff10", u"0.\uff10", u"0e\uff10"]:
            self.assertRaises(json.JSONDecodeError, json.loads, num)

    def test_bounded_int(self):
        # SJ-PT-23-03, limit quadratic number parsing per Python 3.11
        max_str_digits = getattr(sys, 'get_int_max_str_digits', lambda: 4300)()
        s = '1' + '0' * (max_str_digits - 1)
        self.assertEqual(json.loads(s), int(s))
        self.assertRaises(ValueError, json.loads, s + '0')
