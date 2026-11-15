import unittest
import simplejson as json

dct1 = {
    'key1': 'value1'
}

dct2 = {
    'key2': 'value2',
    'd1': dct1
}

dct3 = {
    'key2': 'value2',
    'd1': json.dumps(dct1)
}

dct4 = {
    'key2': 'value2',
    'd1': json.RawJSON(json.dumps(dct1))
}


class TestRawJson(unittest.TestCase):

    def test_normal_str(self):
        self.assertNotEqual(json.dumps(dct2), json.dumps(dct3))

    def test_raw_json_str(self):
        self.assertEqual(json.dumps(dct2), json.dumps(dct4))
        self.assertEqual(dct2, json.loads(json.dumps(dct4)))

    def test_list(self):
        self.assertEqual(
            json.dumps([dct2]),
            json.dumps([json.RawJSON(json.dumps(dct2))]))
        self.assertEqual(
            [dct2],
            json.loads(json.dumps([json.RawJSON(json.dumps(dct2))])))

    def test_direct(self):
        self.assertEqual(
            json.dumps(dct2),
            json.dumps(json.RawJSON(json.dumps(dct2))))
        self.assertEqual(
            dct2,
            json.loads(json.dumps(json.RawJSON(json.dumps(dct2)))))


class TestValidatedRawJson(unittest.TestCase):

    def test_valid_values(self):
        for fragment in (
            '{"key1": "value1"}',
            '[1, 2.5, "three", null, true, false]',
            '"a string"',
            '42',
            '-3.25e10',
            'true',
            'false',
            'null',
        ):
            raw = json.ValidatedRawJSON(fragment)
            self.assertEqual(json.loads(fragment), json.loads(json.dumps(raw)))

    def test_surrounding_whitespace_allowed(self):
        raw = json.ValidatedRawJSON('  \t\r\n {"a": 1} \n ')
        self.assertEqual(json.loads(json.dumps({'d': raw})), {'d': {'a': 1}})

    def test_verbatim_embedding(self):
        fragment = '{ "a" : 1.50 , "b" : [ 1,2 ] }'
        raw = json.ValidatedRawJSON(fragment)
        # The original text is preserved byte-for-byte, not re-serialized
        self.assertEqual(json.dumps(raw), fragment)
        self.assertEqual(json.dumps({'d': raw}), '{"d": %s}' % (fragment,))
        self.assertEqual(json.dumps([raw]), '[%s]' % (fragment,))

    def test_embed_in_object_and_array(self):
        raw = json.ValidatedRawJSON(json.dumps(dct1))
        self.assertEqual(json.dumps(dct2), json.dumps({'key2': 'value2', 'd1': raw}))
        self.assertEqual(json.dumps([dct2]), json.dumps([json.ValidatedRawJSON(json.dumps(dct2))]))
        self.assertEqual([dct2], json.loads(json.dumps([json.ValidatedRawJSON(json.dumps(dct2))])))

    def test_rejects_empty_and_truncated(self):
        for fragment in ('', '   ', '{', '{"a":', '[1,', '"unterminated', 'tru'):
            self.assertRaises(json.JSONDecodeError, json.ValidatedRawJSON, fragment)

    def test_rejects_trailing_second_value(self):
        for fragment in ('{} []', '1 2', '"a" "b"', 'true false', '{"a": 1} 2'):
            self.assertRaises(json.JSONDecodeError, json.ValidatedRawJSON, fragment)

    def test_rejects_nonstandard_numbers(self):
        for fragment in ('NaN', 'Infinity', '-Infinity', '[NaN]', '{"x": Infinity}'):
            self.assertRaises(json.JSONDecodeError, json.ValidatedRawJSON, fragment)

    def test_rejects_malformed_numbers(self):
        for fragment in ('01', '1.', '.5', '+1', '1e', '0x10'):
            self.assertRaises(json.JSONDecodeError, json.ValidatedRawJSON, fragment)

    def test_rejects_invalid_json(self):
        for fragment in ("{'a': 1}", '[1, 2,]', '{"a": 1,}', 'undefined'):
            self.assertRaises(json.JSONDecodeError, json.ValidatedRawJSON, fragment)

    def test_bytes_input(self):
        raw = json.ValidatedRawJSON(b'{"a": 1}')
        self.assertEqual(raw.encoded_json, '{"a": 1}')
        self.assertEqual(json.dumps({'d': raw}), '{"d": {"a": 1}}')

    def test_invalid_type(self):
        for fragment in (None, 1, 1.5, [], {}, object()):
            self.assertRaises(TypeError, json.ValidatedRawJSON, fragment)

    def test_immutable_snapshot(self):
        # Content cannot be changed by mutating the source object
        # after validation
        buf = bytearray(b'[1, 2]')
        raw = json.ValidatedRawJSON(buf)
        buf[:] = b'["hacked", 3]'
        self.assertEqual(raw.encoded_json, '[1, 2]')
        self.assertEqual(json.dumps(raw), '[1, 2]')

    def test_str_subclass_normalized(self):
        class SneakyStr(str):
            pass
        raw = json.ValidatedRawJSON(SneakyStr('{"a": 1}'))
        self.assertEqual(type(raw.encoded_json), str)
        self.assertEqual(raw.encoded_json, '{"a": 1}')

    def test_subclass_of_raw_json(self):
        # ValidatedRawJSON is a RawJSON, so the encoders (C and pure
        # Python) emit it through the existing verbatim path
        self.assertTrue(issubclass(json.ValidatedRawJSON, json.RawJSON))
        self.assertIsInstance(json.ValidatedRawJSON('1'), json.RawJSON)
