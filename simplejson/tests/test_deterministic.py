# -*- coding: utf-8 -*-
from __future__ import absolute_import
from unittest import TestCase
from decimal import Decimal
from collections import namedtuple

import simplejson as json
from simplejson.compat import StringIO, PY3
from simplejson.raw_json import RawJSON

DET = dict(deterministic=True, ensure_ascii=False)

# U+1F600 GRINNING FACE (outside the BMP, UTF-16: D83D DE00) and
# U+E000 (BMP private use). Code point order puts U+E000 first;
# UTF-16 code unit order puts the surrogate lead D83D first.
NON_BMP = u'\U0001F600'
BMP_HIGH = u''
CAFE = u'café 中文 ' + NON_BMP


class TestDeterministic(TestCase):
    def test_compact_utf8_output(self):
        obj = {'b': [1, 2.5, u'x'], 'a': {'y': True, 'z': None}}
        self.assertEqual(
            json.dumps(obj, **DET),
            u'{"a":{"y":true,"z":null},"b":[1,2.5,"x"]}')

    def test_utf16_key_order_differs_from_sort_keys(self):
        obj = {NON_BMP: 1, BMP_HIGH: 2}
        deterministic = json.dumps(obj, **DET)
        sorted_keys = json.dumps(
            obj, sort_keys=True, ensure_ascii=False,
            separators=(',', ':'))
        self.assertEqual(
            deterministic,
            u'{"%s":1,"%s":2}' % (NON_BMP, BMP_HIGH))
        self.assertEqual(
            sorted_keys,
            u'{"%s":2,"%s":1}' % (BMP_HIGH, NON_BMP))
        self.assertNotEqual(deterministic, sorted_keys)

    def test_bmp_characters_are_literal(self):
        self.assertEqual(
            json.dumps(CAFE, **DET),
            u'"%s"' % (CAFE,))

    def test_mandatory_escapes(self):
        self.assertEqual(
            json.dumps(u'"\\\n\t', **DET),
            u'"\\"\\\\\\n\\t"')

    def test_unpaired_surrogates_are_escaped(self):
        # An unpaired surrogate has no UTF-8 representation, so
        # deterministic mode emits a lowercase \uXXXX escape to
        # keep the output well-formed UTF-8 text.
        self.assertEqual(
            json.dumps(u'a\ud800b\udfff', **DET),
            u'"a\\ud800b\\udfff"')
        self.assertEqual(
            json.dumps({u'\ud800': 1}, **DET),
            u'{"\\ud800":1}')

    def test_surrogate_sort_order(self):
        # U+D800 (as a lone surrogate code unit) sorts before U+E000
        # in UTF-16 code unit order.
        obj = {BMP_HIGH: 1, u'\ud800': 2}
        self.assertEqual(
            json.dumps(obj, **DET),
            u'{"\\ud800":2,"%s":1}' % (BMP_HIGH,))

    def test_negative_zero(self):
        self.assertEqual(json.dumps(-0.0, **DET), '0')
        self.assertEqual(json.dumps(0.0, **DET), '0')
        self.assertEqual(json.dumps([-0.0, 0.0, 0], **DET), '[0,0,0]')

    def test_decimal_negative_zero_is_preserved(self):
        # Decimal explicitly preserves a signed zero; str() of a
        # finite Decimal is the deterministic representation.
        self.assertEqual(json.dumps(Decimal('-0'), **DET), '-0')
        self.assertEqual(json.dumps(Decimal('1.10'), **DET), '1.10')

    def test_integer_boundaries(self):
        limit = 2 ** 53
        self.assertEqual(json.dumps(limit, **DET), '9007199254740992')
        self.assertEqual(json.dumps(-limit, **DET), '-9007199254740992')
        self.assertRaises(ValueError, json.dumps, limit + 1, **DET)
        self.assertRaises(ValueError, json.dumps, -limit - 1, **DET)

    def test_non_finite_numbers_rejected(self):
        for value in (float('nan'), float('inf'), float('-inf'),
                      Decimal('NaN'), Decimal('sNaN'),
                      Decimal('Infinity'), Decimal('-Infinity')):
            self.assertRaises(ValueError, json.dumps, value, **DET)

    def test_decimal_requires_use_decimal(self):
        self.assertRaises(
            TypeError, json.dumps, Decimal('1.1'),
            use_decimal=False, **DET)

    def test_only_string_keys(self):
        for key in (1, 1.5, True, None, (1, 2)):
            self.assertRaises(
                TypeError, json.dumps, {key: 'x'}, **DET)
        if PY3:
            self.assertRaises(
                TypeError, json.dumps, {b'bytes': 'x'}, **DET)

    def test_custom_objects_rejected(self):
        self.assertRaises(TypeError, json.dumps, object(), **DET)
        self.assertRaises(TypeError, json.dumps, set([1]), **DET)
        self.assertRaises(
            TypeError, json.dumps, RawJSON('{"a":1}'), **DET)

        class ForJSON(object):
            def for_json(self):
                return {'a': 1}
        # for_json is not consulted in deterministic mode
        self.assertRaises(TypeError, json.dumps, ForJSON(), **DET)

    def test_namedtuple_encodes_as_array(self):
        Point = namedtuple('Point', ['x', 'y'])
        self.assertEqual(json.dumps(Point(1, 2), **DET), '[1,2]')

    def test_circular_reference(self):
        lst = [1]
        lst.append(lst)
        self.assertRaises(ValueError, json.dumps, lst, **DET)
        dct = {}
        dct['self'] = dct
        self.assertRaises(ValueError, json.dumps, dct, **DET)

    def test_conflicting_options_rejected(self):
        conflicts = [
            dict(),  # ensure_ascii defaults to True
            dict(ensure_ascii=True),
            dict(sort_keys=True),
            dict(item_sort_key=lambda kv: kv[0]),
            dict(indent=2),
            dict(separators=(',', ':')),
            dict(skipkeys=True),
            dict(allow_nan=True),
            dict(ignore_nan=True),
            dict(default=str),
            dict(for_json=True),
            dict(bigint_as_string=True),
            dict(int_as_string_bitcount=32),
            dict(iterable_as_array=True),
            dict(tuple_as_array=False),
            dict(namedtuple_as_object=False),
            dict(encoding='latin-1'),
        ]
        for kw in conflicts:
            kw['deterministic'] = True
            kw.setdefault('ensure_ascii', True)
            self.assertRaises(TypeError, json.dumps, {'a': 1}, **kw)

    def test_conflict_error_names_options(self):
        try:
            json.dumps({'a': 1}, deterministic=True, sort_keys=True)
        except TypeError as exc:
            msg = str(exc)
        else:
            self.fail('expected TypeError')
        self.assertIn('sort_keys', msg)
        self.assertIn('ensure_ascii', msg)

    def test_sort_keys_does_not_silently_override(self):
        # A plain sort_keys/item_sort_key must never change the
        # deterministic key order; the combination is an error.
        obj = {NON_BMP: 1, BMP_HIGH: 2}
        self.assertRaises(
            TypeError, json.dumps, obj,
            deterministic=True, ensure_ascii=False, sort_keys=True)
        self.assertRaises(
            TypeError, json.dumps, obj,
            deterministic=True, ensure_ascii=False,
            item_sort_key=lambda kv: kv[0])

    def test_encoder_class(self):
        encoder = json.JSONEncoder(deterministic=True, ensure_ascii=False)
        self.assertEqual(
            encoder.encode({NON_BMP: 1, BMP_HIGH: 2}),
            u'{"%s":1,"%s":2}' % (NON_BMP, BMP_HIGH))
        self.assertEqual(
            ''.join(encoder.iterencode([1, u'x'])),
            u'[1,"x"]')

    def test_dump_to_file(self):
        out = StringIO()
        json.dump({'b': 1, 'a': [True]}, out, **DET)
        self.assertEqual(out.getvalue(), u'{"a":[true],"b":1}')

    def test_round_trip(self):
        obj = {u'key': [1, -0.0, CAFE, {u' nested ': None}]}
        self.assertEqual(
            json.loads(json.dumps(obj, **DET)),
            {u'key': [1, 0.0, CAFE, {u' nested ': None}]})

    def test_str_subclass_key(self):
        class S(str):
            pass
        self.assertEqual(json.dumps({S('a'): 1}, **DET), '{"a":1}')
