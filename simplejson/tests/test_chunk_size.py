# -*- coding: utf-8 -*-
from __future__ import absolute_import
from unittest import TestCase

import simplejson as json
from simplejson import JSONEncoder
from simplejson.compat import StringIO, text_type


def _escape_spans(text):
    """Return (start, end) spans of every JSON escape sequence in text."""
    spans = []
    i = 0
    n = len(text)
    while i < n:
        if text[i] == '\\':
            if text[i + 1:i + 2] == 'u':
                spans.append((i, i + 6))
                i += 6
            else:
                spans.append((i, i + 2))
                i += 2
        else:
            i += 1
    return spans


def _assert_boundaries_respect_escapes(testcase, chunks):
    joined = ''.join(chunks)
    boundaries = set()
    pos = 0
    for chunk in chunks[:-1]:
        pos += len(chunk)
        boundaries.add(pos)
    for start, end in _escape_spans(joined):
        for boundary in boundaries:
            testcase.assertFalse(
                start < boundary < end,
                'chunk boundary %d splits escape %r at %d:%d' % (
                    boundary, joined[start:end], start, end))
    return joined


class TestChunkSize(TestCase):
    def test_string_content_is_segmented(self):
        value = 'a' * 10000
        chunks = list(JSONEncoder(chunk_size=100).iterencode(value))
        self.assertGreater(len(chunks), 10)
        self.assertEqual(''.join(chunks), json.dumps(value))
        # budget + slack for one atomic escape sequence
        self.assertLessEqual(max(len(c) for c in chunks), 100 + 11)

    def test_chunk_boundaries_never_split_escapes(self):
        value = ('x' * 37 + '\n' + 'y' * 13 + '"\\' + 'z' * 5 +
                 '\t' + 'é' * 11 + '\U0001F600' + 'w' * 29)
        for chunk_size in (1, 2, 3, 5, 6, 7, 11, 12, 13, 64):
            for ensure_ascii in (True, False):
                enc = JSONEncoder(chunk_size=chunk_size,
                                  ensure_ascii=ensure_ascii)
                chunks = list(enc.iterencode(value))
                joined = _assert_boundaries_respect_escapes(self, chunks)
                self.assertEqual(
                    joined, json.dumps(value, ensure_ascii=ensure_ascii))

    def test_surrogate_pair_is_atomic(self):
        value = '\U0001F600'
        expect = json.dumps(value)  # "\ud83d\ude00"
        for chunk_size in (1, 2, 4, 6, 8, 11):
            chunks = list(JSONEncoder(chunk_size=chunk_size).iterencode(value))
            self.assertEqual(''.join(chunks), expect)
            self.assertIn('\\ud83d\\ude00', ''.join(chunks))
            # the pair is emitted as one indivisible token
            self.assertTrue(any('\\ud83d\\ude00' in c for c in chunks),
                            (chunk_size, chunks))
            _assert_boundaries_respect_escapes(self, chunks)

    def test_matches_unchunked_with_indent_and_ensure_ascii(self):
        obj = {
            'k' * 40: ['v' * 90, 1, 2.5, None, True, {'n' * 40: 'é' * 30}],
            'z': '\tπ\U0001F600' * 20,
            'empty': '',
        }
        for chunk_size in (1, 2, 3, 7, 16, 1000):
            for ensure_ascii in (True, False):
                for indent in (None, 0, 2, '\t'):
                    kw = dict(ensure_ascii=ensure_ascii, indent=indent,
                              sort_keys=True)
                    chunks = list(
                        JSONEncoder(chunk_size=chunk_size, **kw)
                        .iterencode(obj))
                    self.assertEqual(''.join(chunks), json.dumps(obj, **kw))
                    _assert_boundaries_respect_escapes(self, chunks)

    def test_dict_keys_are_chunked(self):
        obj = {'k' * 500: 1}
        chunks = list(JSONEncoder(chunk_size=32).iterencode(obj))
        self.assertGreater(len(chunks), 5)
        self.assertEqual(''.join(chunks), json.dumps(obj))
        self.assertLessEqual(max(len(c) for c in chunks), 32 + 11)

    def test_bytes_values_are_chunked(self):
        chunks = list(JSONEncoder(chunk_size=4).iterencode([b'hello world']))
        self.assertEqual(''.join(chunks), '["hello world"]')
        self.assertGreater(len(chunks), 1)

    def test_str_subclass_normalized(self):
        class S(text_type):
            pass
        chunks = list(JSONEncoder(chunk_size=3).iterencode([S('abcdef')]))
        self.assertEqual(''.join(chunks), '["abcdef"]')

    def test_for_json_called_once_despite_chunking(self):
        calls = []

        class W(object):
            def for_json(self):
                calls.append(1)
                return 'x' * 1000

        enc = JSONEncoder(chunk_size=10, for_json=True)
        out = ''.join(enc.iterencode([W()]))
        self.assertEqual(out, '["' + 'x' * 1000 + '"]')
        self.assertEqual(len(calls), 1)

    def test_default_called_once_despite_chunking(self):
        calls = []

        def default(o):
            calls.append(1)
            return 'y' * 500

        enc = JSONEncoder(chunk_size=6, default=default)
        out = ''.join(enc.iterencode([object()]))
        self.assertEqual(out, '["' + 'y' * 500 + '"]')
        self.assertEqual(len(calls), 1)

    def test_close_mid_iteration_releases_state(self):
        calls = []

        class W(object):
            def for_json(self):
                calls.append(1)
                return ['x' * 1000]

        enc = JSONEncoder(chunk_size=8, for_json=True)
        gen = enc.iterencode([W()])
        for chunk in gen:
            if calls:
                break
        self.assertEqual(len(calls), 1)
        gen.close()
        # closing did not error, did not re-run for_json, and the
        # encoder is fully reusable afterwards
        self.assertEqual(len(calls), 1)
        out = ''.join(enc.iterencode([W()]))
        self.assertEqual(out, '[["' + 'x' * 1000 + '"]]')
        self.assertEqual(len(calls), 2)

    def test_circular_reference_still_detected(self):
        lst = []
        lst.append(lst)
        self.assertRaises(
            ValueError,
            list, JSONEncoder(chunk_size=4).iterencode(lst))

    def test_empty_and_tiny_strings(self):
        for chunk_size in (1, 2, 3):
            enc = JSONEncoder(chunk_size=chunk_size)
            self.assertEqual(''.join(enc.iterencode('')), '""')
            self.assertEqual(''.join(enc.iterencode('\n')), '"\\n"')
            self.assertEqual(''.join(enc.iterencode('a')), '"a"')

    def test_encode_matches_unchunked(self):
        obj = ['é' * 200, {'k': '\U0001F600' * 50}]
        for ensure_ascii in (True, False):
            self.assertEqual(
                JSONEncoder(chunk_size=8,
                            ensure_ascii=ensure_ascii).encode(obj),
                JSONEncoder(ensure_ascii=ensure_ascii).encode(obj))

    def test_dump_writes_chunked(self):
        obj = {'k' * 100: 'v' * 100}
        sio = StringIO()
        json.dump(obj, sio, chunk_size=16)
        self.assertEqual(sio.getvalue(), json.dumps(obj))

    def test_dumps_matches_unchunked(self):
        obj = ['x' * 300]
        self.assertEqual(json.dumps(obj, chunk_size=16), json.dumps(obj))

    def test_invalid_chunk_size(self):
        for bad in (0, -1, -100, 1.5, '10', True):
            self.assertRaises(ValueError, JSONEncoder, chunk_size=bad)

    def test_chunk_size_attribute(self):
        self.assertIsNone(JSONEncoder().chunk_size)
        self.assertEqual(JSONEncoder(chunk_size=10).chunk_size, 10)
