# -*- coding: utf-8 -*-
r"""Tests for the explicit strict_unicode mode.

The mode rejects unpaired UTF-16 surrogate code points on both the decode
side (escaped and literal, values and object keys) and the encode side
(where ensure_ascii must not be able to launder a lone surrogate into a
``\udXXX`` escape). It is implemented purely in Python: when enabled, the
C speedups are bypassed, so these tests run identically with and without
the extension built.
"""
from unittest import TestCase


import simplejson as json
import simplejson.decoder
import simplejson.encoder
import simplejson.scanner
from simplejson import JSONDecodeError
from simplejson.compat import b

HIGH = u'\ud834'
LOW = u'\udd20'
PAIR_ESCAPED = u'\\ud834\\udd20'
SUPPLEMENTARY = u'\U0001d120'  # character encoded by PAIR_ESCAPED


class TestStrictUnicodeDecode(TestCase):
    def test_default_remains_permissive(self):
        # Existing behavior must not change when the option is not given.
        self.assertEqual(json.loads('"\\ud834"'), HIGH)
        self.assertEqual(json.loads('"\\udd20"'), LOW)
        self.assertEqual(json.loads(u'"' + HIGH + u'"'), HIGH)
        self.assertEqual(json.loads('{"\\ud834": 1}'), {HIGH: 1})

    def test_explicit_opt_out_is_permissive(self):
        self.assertEqual(
            json.loads('"\\ud834"', strict_unicode=False), HIGH)
        self.assertEqual(
            json.loads(u'"' + LOW + u'"', strict_unicode=False), LOW)

    def test_escaped_lone_surrogates_rejected(self):
        for doc in (
            '"\\ud834"',          # lone high
            '"\\udd20"',          # lone low
            '"a\\ud834"',         # high after content
            '"\\udd20z"',         # low before content
            '"\\ud834\\ud834"',   # high followed by high
            '"\\ud834\\u0079"',   # high followed by non-surrogate escape
        ):
            with self.assertRaises(JSONDecodeError) as cm:
                json.loads(doc, strict_unicode=True)
            # Error points at the 'u' of the offending \u escape, matching
            # the existing "Invalid \uXXXX escape sequence" convention.
            self.assertEqual(cm.exception.pos, doc.index('\\u') + 1, doc)
            self.assertEqual(doc[cm.exception.pos], 'u')

    def test_valid_escaped_pair_combines(self):
        self.assertEqual(
            json.loads('"z' + PAIR_ESCAPED + 'x"', strict_unicode=True),
            u'z' + SUPPLEMENTARY + u'x')
        self.assertEqual(
            json.loads('"' + PAIR_ESCAPED + '"', strict_unicode=True),
            SUPPLEMENTARY)

    def test_adjacent_pairs_and_mixed_escape_sequences(self):
        two = SUPPLEMENTARY + SUPPLEMENTARY
        self.assertEqual(
            json.loads('"' + PAIR_ESCAPED + PAIR_ESCAPED + '"',
                       strict_unicode=True),
            two)
        # high-high-low: the first high is unpaired.
        with self.assertRaises(JSONDecodeError) as cm:
            json.loads('"\\ud834\\ud834\\udd20"', strict_unicode=True)
        self.assertEqual(cm.exception.pos, 2)
        # leading low before an otherwise valid pair.
        with self.assertRaises(JSONDecodeError) as cm:
            json.loads('"\\udd20\\ud834\\udd20"', strict_unicode=True)
        self.assertEqual(cm.exception.pos, 2)

    def test_pair_then_lone_reports_lone_position(self):
        # quote=0; the twelve pair-escape characters occupy 1..12. The
        # third escape's backslash is at 13 and its 'u' at 14.
        doc = '"' + PAIR_ESCAPED + '\\ud834"'
        self.assertEqual(doc[13], '\\')
        self.assertEqual(doc[14], 'u')
        with self.assertRaises(JSONDecodeError) as cm:
            json.loads(doc, strict_unicode=True)
        self.assertEqual(cm.exception.pos, 14)

    def test_literal_surrogates_rejected(self):
        for doc, expected_pos in (
            (u'"' + HIGH + u'"', 1),
            (u'"' + LOW + u'"', 1),
            (u'"ab' + HIGH + u'cd"', 3),
            (u'"ab' + LOW + u'cd"', 3),
        ):
            with self.assertRaises(JSONDecodeError) as cm:
                json.loads(doc, strict_unicode=True)
            self.assertEqual(cm.exception.pos, expected_pos, doc)

    def test_literal_supplementary_character_accepted(self):
        self.assertEqual(
            json.loads(u'"' + SUPPLEMENTARY + u'"', strict_unicode=True),
            SUPPLEMENTARY)

    def test_object_keys_are_validated(self):
        # '{', '"', then the escape's backslash at index 2 and 'u' at 3.
        doc = '{"\\ud834": 1}'
        self.assertEqual(doc[2], '\\')
        self.assertEqual(doc[3], 'u')
        with self.assertRaises(JSONDecodeError) as cm:
            json.loads(doc, strict_unicode=True)
        self.assertEqual(cm.exception.pos, 3)
        with self.assertRaises(JSONDecodeError):
            json.loads(u'{"' + LOW + u'": 1}', strict_unicode=True)
        # A valid key still works.
        self.assertEqual(
            json.loads('{"' + PAIR_ESCAPED + '": 1}', strict_unicode=True),
            {SUPPLEMENTARY: 1})

    def test_nested_strings_validated(self):
        with self.assertRaises(JSONDecodeError):
            json.loads('{"a": ["x", {"b": "\\udd20"}]}',
                       strict_unicode=True)

    def test_position_is_in_document_coordinates(self):
        doc = u'["ok",\n "\\ud834"]'
        with self.assertRaises(JSONDecodeError) as cm:
            json.loads(doc, strict_unicode=True)
        self.assertEqual(cm.exception.lineno, 2)
        self.assertEqual(cm.exception.colno, 4)
        # The reported position is the 'u' of the offending \u escape;
        # the backslash immediately precedes it.
        self.assertEqual(doc[cm.exception.pos], u'u')
        self.assertEqual(doc[cm.exception.pos - 1], u'\\')

    def test_strict_control_flag_is_independent(self):
        # Literal control char allowed with strict=False even in strict
        # unicode mode.
        self.assertEqual(
            json.loads('"a\x01b"', strict=False, strict_unicode=True),
            u'a\x01b')
        # strict=True still rejects the control character.
        with self.assertRaises(JSONDecodeError):
            json.loads('"a\x01b"', strict=True, strict_unicode=True)

    def test_bytes_input(self):
        self.assertEqual(
            json.loads(b('"' + PAIR_ESCAPED + '"'), strict_unicode=True),
            SUPPLEMENTARY)
        with self.assertRaises(JSONDecodeError):
            json.loads(b'"\\ud834"', strict_unicode=True)

    def test_decoder_instance_and_raw_scanstring(self):
        decoder = json.JSONDecoder(strict_unicode=True)
        self.assertTrue(decoder.strict_unicode)
        self.assertEqual(
            decoder.decode('"' + PAIR_ESCAPED + '"'), SUPPLEMENTARY)
        with self.assertRaises(JSONDecodeError):
            decoder.decode('"\\ud834"')
        # Direct scanner API keeps its old positional signature and gains
        # the keyword.
        result, end = simplejson.decoder.py_scanstring(
            '"' + PAIR_ESCAPED + '"', 1, None, True,
            strict_unicode=True)
        self.assertEqual(result, SUPPLEMENTARY)

    def test_uses_pure_python_scanner(self):
        decoder = json.JSONDecoder(strict_unicode=True)
        self.assertIs(decoder.parse_string,
                      simplejson.decoder.py_scanstring)
        # Functional smoke test of the wired scanner.
        self.assertEqual(
            decoder.decode('["x", {"y": 1}]'), ['x', {'y': 1}])

    def test_non_strict_decoder_keeps_configured_scanstring(self):
        decoder = json.JSONDecoder(strict_unicode=False)
        self.assertIs(decoder.parse_string,
                      simplejson.decoder.scanstring)


class TestStrictUnicodeEncode(TestCase):
    def test_default_remains_permissive(self):
        self.assertEqual(json.dumps(HIGH), '"\\ud834"')
        self.assertEqual(json.dumps(LOW), '"\\udd20"')
        self.assertEqual(
            json.dumps(HIGH, ensure_ascii=False), u'"' + HIGH + u'"')

    def test_lone_surrogates_rejected_both_ascii_modes(self):
        for value in (
            HIGH, LOW, u'a' + HIGH, HIGH + u'b',
            u'x' + LOW + u'y',
        ):
            for kwargs in ({}, {'ensure_ascii': False}):
                with self.assertRaises(ValueError):
                    json.dumps(value, strict_unicode=True, **kwargs)

    def test_ensure_ascii_cannot_launder(self):
        # The whole point: ensure_ascii=True would otherwise emit the lone
        # surrogate as "\ud834". In strict mode that must be a ValueError.
        with self.assertRaises(ValueError) as cm:
            json.dumps(HIGH, strict_unicode=True, ensure_ascii=True)
        self.assertIn('surrogate', str(cm.exception).lower())

    def test_supplementary_characters_round_trip(self):
        self.assertEqual(
            json.dumps(SUPPLEMENTARY, strict_unicode=True),
            '"' + PAIR_ESCAPED + '"')
        self.assertEqual(
            json.dumps(SUPPLEMENTARY, strict_unicode=True,
                       ensure_ascii=False),
            u'"' + SUPPLEMENTARY + u'"')
        self.assertEqual(
            json.dumps([u'x' + SUPPLEMENTARY + u'y'],
                       strict_unicode=True),
            '["x' + PAIR_ESCAPED + 'y"]')

    def test_nested_values_and_keys(self):
        self.assertEqual(
            json.dumps({'k': [SUPPLEMENTARY]}, strict_unicode=True),
            '{"k": ["' + PAIR_ESCAPED + '"]}')
        with self.assertRaises(ValueError):
            json.dumps({HIGH: 1}, strict_unicode=True)
        with self.assertRaises(ValueError):
            json.dumps({'a': [LOW]}, strict_unicode=True)
        with self.assertRaises(ValueError):
            json.dumps({'a': {LOW: 1}}, strict_unicode=True)

    def test_encoder_instance_and_iterencode(self):
        enc = json.JSONEncoder(strict_unicode=True)
        self.assertTrue(enc.strict_unicode)
        self.assertEqual(
            ''.join(enc.iterencode(SUPPLEMENTARY)),
            '"' + PAIR_ESCAPED + '"')
        with self.assertRaises(ValueError):
            enc.encode(HIGH)
        enc_nonascii = json.JSONEncoder(strict_unicode=True,
                                        ensure_ascii=False)
        self.assertEqual(
            enc_nonascii.encode(SUPPLEMENTARY),
            u'"' + SUPPLEMENTARY + u'"')

    def test_html_encoder_inherits_mode(self):
        enc = simplejson.encoder.JSONEncoderForHTML(strict_unicode=True)
        self.assertEqual(
            enc.encode({'a': u'<' + SUPPLEMENTARY + u'>'}),
            '{"a": "\\u003c' + PAIR_ESCAPED + '\\u003e"}')
        with self.assertRaises(ValueError):
            enc.encode(HIGH)

    def test_uses_pure_python_iterencoder(self):
        # With the C factory present it must be bypassed; the resulting
        # output must still be correct for a range of structures.
        enc = json.JSONEncoder(strict_unicode=True)
        payload = {
            'list': [1, SUPPLEMENTARY, None, True],
            'str': u'ok',
            'nest': {'k': [SUPPLEMENTARY]},
        }
        encoded = ''.join(enc.iterencode(payload))
        self.assertEqual(json.loads(encoded), json.loads(json.dumps(
            payload, ensure_ascii=False)))

    def test_non_surrogate_output_unchanged(self):
        payload = {'a': 1, 'b': [u'x', None, 3.5], 'c': True}
        self.assertEqual(
            json.dumps(payload, strict_unicode=True),
            json.dumps(payload))
        self.assertEqual(
            json.dumps(payload, strict_unicode=True, ensure_ascii=False),
            json.dumps(payload, ensure_ascii=False))

    def test_bytes_and_str_subclasses(self):
        class MyStr(str):
            pass

        self.assertEqual(
            json.dumps(MyStr(SUPPLEMENTARY), strict_unicode=True),
            '"' + PAIR_ESCAPED + '"')
        with self.assertRaises(ValueError):
            json.dumps(MyStr(HIGH), strict_unicode=True)
        # UTF-8 bytes are decoded before the surrogate check, so a
        # supplementary character encoded as UTF-8 serializes normally.
        utf8 = SUPPLEMENTARY.encode('utf-8')
        self.assertEqual(
            json.dumps(utf8, strict_unicode=True),
            '"' + PAIR_ESCAPED + '"')
        self.assertEqual(
            json.dumps([utf8], strict_unicode=True),
            '["' + PAIR_ESCAPED + '"]')
        # A non-UTF-8 source encoding keeps working.
        self.assertEqual(
            json.dumps(u'\xe9'.encode('latin-1'), encoding='latin-1',
                       strict_unicode=True),
            '"\\u00e9"')

    def test_empty_values(self):
        self.assertEqual(json.dumps('', strict_unicode=True), '""')
        self.assertEqual(json.dumps([], strict_unicode=True), '[]')
        self.assertEqual(json.dumps({}, strict_unicode=True), '{}')
        self.assertEqual(json.loads('""', strict_unicode=True), '')

    def test_adjacent_pairs_and_mixed_sequences(self):
        two = SUPPLEMENTARY + SUPPLEMENTARY
        self.assertEqual(
            json.dumps(two, strict_unicode=True),
            '"' + PAIR_ESCAPED + PAIR_ESCAPED + '"')
        for bad in (SUPPLEMENTARY + HIGH, HIGH + SUPPLEMENTARY,
                    SUPPLEMENTARY + LOW, LOW + SUPPLEMENTARY,
                    HIGH + HIGH, LOW + LOW, LOW + HIGH):
            with self.assertRaises(ValueError):
                json.dumps(bad, strict_unicode=True)

    def test_error_names_code_point(self):
        try:
            json.dumps(LOW, strict_unicode=True)
        except ValueError as exc:
            self.assertIn('0xdd20', str(exc))
        else:
            self.fail('ValueError expected')


class TestSpeedupsFallback(TestCase):
    """The C acceleration is bypassed whenever strict_unicode is enabled."""

    def test_scanner_factory_bypasses_c(self):
        original = simplejson.scanner.c_make_scanner

        def fail_if_called(context):
            raise AssertionError('C scanner used in strict mode')

        simplejson.scanner.c_make_scanner = fail_if_called
        try:
            strict_decoder = json.JSONDecoder(strict_unicode=True)
            # Rebuild through the patched factory function.
            strict_decoder.scan_once = simplejson.scanner.make_scanner(
                strict_decoder)
            self.assertEqual(
                strict_decoder.decode('"' + PAIR_ESCAPED + '"'),
                SUPPLEMENTARY)
        finally:
            simplejson.scanner.c_make_scanner = original

    def test_encoder_bypasses_c_make_encoder(self):
        original = simplejson.encoder.c_make_encoder

        def fail_if_called(*args, **kwargs):
            raise AssertionError('C encoder used in strict mode')

        simplejson.encoder.c_make_encoder = fail_if_called
        try:
            enc = simplejson.encoder.JSONEncoder(strict_unicode=True)
            self.assertEqual(
                ''.join(enc.iterencode(SUPPLEMENTARY)),
                '"' + PAIR_ESCAPED + '"')
            with self.assertRaises(ValueError):
                ''.join(enc.iterencode(HIGH))
        finally:
            simplejson.encoder.c_make_encoder = original
