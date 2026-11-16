#!/usr/bin/env python
from tests import unittest

import jmespath
from jmespath.normsearch import find_normalized, contains_normalized


class TestFindNormalized(unittest.TestCase):

    # --- basic folding / normalization ---

    def test_casefold_ascii(self):
        self.assertEqual(find_normalized('Hello World', 'hello'), [(0, 5)])

    def test_casefold_sharp_s_expansion(self):
        # 'ß' casefolds to 'ss': one original char, two key chars.
        self.assertEqual(find_normalized('Straße', 'STRASSE'), [(0, 6)])
        self.assertEqual(find_normalized('STRASSE', 'straße'), [(0, 7)])

    def test_expansion_position_points_at_original_char(self):
        # The 'ss' match inside 'aßb' must report the 'ß' interval.
        self.assertEqual(find_normalized('aßb', 'SS'), [(1, 2)])

    def test_partial_expansion_match(self):
        # Each 's' of the folded 'ss' maps back to the whole 'ß' span,
        # so both key-space hits report the same original interval.
        self.assertEqual(find_normalized('aßb', 's'), [(1, 2), (1, 2)])

    def test_nfc_composition_in_haystack(self):
        # Haystack uses e + combining acute (U+0301); needle precomposed.
        self.assertEqual(find_normalized('Café', 'é'), [(3, 5)])

    def test_nfc_composition_in_needle(self):
        # Reverse direction: precomposed haystack, decomposed needle.
        self.assertEqual(find_normalized('Café', 'café'), [(0, 4)])

    def test_combining_cluster_reports_whole_original_span(self):
        # 'é' is one key char produced from two original chars.
        matches = find_normalized('xéy', 'É')
        self.assertEqual(matches, [(1, 3)])
        self.assertEqual('xéy'[1:3], 'é')

    def test_hangul_jamo_composition(self):
        # L + V jamo compose across character boundaries.
        self.assertEqual(find_normalized('가', '가'), [(0, 2)])
        self.assertEqual(find_normalized('가', '가'), [(0, 1)])

    def test_final_sigma(self):
        self.assertEqual(find_normalized('ΣΣ', 'σς'), [(0, 2)])

    def test_turkish_capital_i_expansion(self):
        # 'İ' casefolds to 'i' + combining dot above.
        self.assertEqual(find_normalized('İstanbul', 'i̇stan'), [(0, 5)])

    def test_compatibility_form_is_opt_in(self):
        # Superscript two: casefold leaves it alone, NFKC folds to '2'.
        self.assertEqual(find_normalized('x²', '2'), [])
        self.assertEqual(find_normalized('x²', '2', form='NFKC'), [(1, 2)])

    # --- overlap rules ---

    def test_non_overlapping_default(self):
        self.assertEqual(find_normalized('aaaa', 'aa'), [(0, 2), (2, 4)])

    def test_overlapping(self):
        self.assertEqual(find_normalized('aaaa', 'aa', overlap=True),
                         [(0, 2), (1, 3), (2, 4)])

    def test_overlap_may_duplicate_original_intervals(self):
        # Two distinct key-space matches collapse onto the same
        # original interval; both are reported, in key-space order.
        self.assertEqual(find_normalized('ßß', 'sss', overlap=True),
                         [(0, 2), (0, 2)])
        self.assertEqual(find_normalized('ßß', 'sss'), [(0, 2)])

    # --- empty query / no match ---

    def test_empty_needle_matches_every_gap(self):
        self.assertEqual(find_normalized('ab', ''),
                         [(0, 0), (1, 1), (2, 2)])

    def test_empty_needle_empty_haystack(self):
        self.assertEqual(find_normalized('', ''), [(0, 0)])

    def test_no_match_returns_empty_list(self):
        self.assertEqual(find_normalized('abc', 'z'), [])
        self.assertEqual(find_normalized('', 'z'), [])

    def test_needle_longer_than_haystack(self):
        self.assertEqual(find_normalized('ab', 'abc'), [])

    # --- coordinates always refer to the original string ---

    def test_coordinates_slice_original_text(self):
        haystack = '  Straße  '
        for start, end in find_normalized(haystack, 'STRASSE'):
            self.assertEqual(haystack[start:end], 'Straße')

    def test_positions_after_expansion_shift_back(self):
        # Key space is longer than the original; reported indices must
        # still be valid original-string indices.
        haystack = 'ßxß'
        self.assertEqual(find_normalized(haystack, 'X'), [(1, 2)])
        self.assertEqual(find_normalized(haystack, 'ssxss'), [(0, 3)])

    # --- argument validation ---

    def test_type_errors(self):
        with self.assertRaises(TypeError):
            find_normalized(123, 'a')
        with self.assertRaises(TypeError):
            find_normalized('abc', b'a')

    def test_invalid_form(self):
        with self.assertRaises(ValueError):
            find_normalized('a', 'a', form='BOGUS')


class TestContainsNormalized(unittest.TestCase):

    def test_basic(self):
        self.assertTrue(contains_normalized('Straße', 'STRASSE'))
        self.assertFalse(contains_normalized('Straße', 'strausse'))

    def test_empty_needle_always_contained(self):
        # Mirrors Python's '' in s.
        self.assertTrue(contains_normalized('abc', ''))
        self.assertTrue(contains_normalized('', ''))

    def test_standard_contains_is_unchanged(self):
        # The new function is independent: JMESPath's contains() keeps
        # its exact, case-sensitive, non-normalizing semantics.
        self.assertIs(jmespath.search("contains(@, 'strasse')", 'Straße'),
                      False)
        self.assertIs(jmespath.search("contains(@, 'Straße')", 'Straße'),
                      True)
        self.assertTrue(contains_normalized('Straße', 'strasse'))


if __name__ == '__main__':
    unittest.main()
