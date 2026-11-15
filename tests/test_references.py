"""Tests for Databook inter-sheet references."""

import unittest

import tablib
from tablib.exceptions import (
    InvalidMissingPolicy,
    SheetNotFound,
    SheetReferenced,
)
from tablib.references import MISSING_ERROR, MISSING_SKIP


class ReferenceTestCase(unittest.TestCase):
    def setUp(self):
        self.authors = tablib.Dataset(title='authors')
        self.authors.headers = ('id', 'name')
        self.authors.append((1, 'Ada'))
        self.authors.append((2, 'Bob'))

        self.books = tablib.Dataset(title='books')
        self.books.headers = ('isbn', 'author_id')
        self.books.append(('a-1', 1))
        self.books.append(('a-2', 2))

        self.book = tablib.Databook([self.authors, self.books])

    def add_ref(self, **kwargs):
        params = dict(
            source=self.books, source_columns=('author_id',),
            target=self.authors, target_columns=('id',),
            missing=MISSING_SKIP,
        )
        params.update(kwargs)
        return self.book.add_reference(**params)

    # -- registration --------------------------------------------------

    def test_add_reference_returns_reference(self):
        ref = self.add_ref()
        self.assertIn(ref, self.book.references)
        self.assertEqual(ref.source_columns, ('author_id',))
        self.assertEqual(ref.target_columns, ('id',))

    def test_missing_policy_is_mandatory_and_validated(self):
        with self.assertRaises(TypeError):
            self.book.add_reference(
                self.books, ('author_id',), self.authors, ('id',),
            )
        with self.assertRaises(InvalidMissingPolicy):
            self.add_ref(missing='whatever')

    def test_arity_mismatch_rejected(self):
        with self.assertRaises(ValueError):
            self.add_ref(source_columns=('author_id', 'isbn'))

    # -- verification --------------------------------------------------

    def test_verify_ok(self):
        self.add_ref()
        report = self.book.verify_references()
        self.assertTrue(report.ok)
        self.assertTrue(report)
        self.assertIn('OK', report.summary())

    def test_verify_is_read_only(self):
        self.add_ref()
        before = (self.authors.dict, self.books.dict, len(self.book.references))
        self.book.verify_references()
        after = (self.authors.dict, self.books.dict, len(self.book.references))
        self.assertEqual(before, after)

    def test_missing_table_reported(self):
        ref = self.add_ref()
        self.book._datasets.remove(self.authors)  # bypass remove_sheet guard
        report = self.book.verify_references()
        self.assertFalse(report.ok)
        self.assertEqual(
            [(issue.reference, issue.role) for issue in report.missing_tables],
            [(ref, 'target')],
        )
        self.assertIn('missing table', report.summary())

    def test_missing_column_reported(self):
        ref = self.add_ref()
        del self.authors['id']
        report = self.book.verify_references()
        self.assertEqual(
            [(i.reference, i.role, i.column) for i in report.missing_columns],
            [(ref, 'target', 'id')],
        )

    def test_duplicate_target_reported(self):
        self.authors.append((2, 'Bobby'))  # duplicate id=2
        ref = self.add_ref()
        report = self.book.verify_references()
        self.assertEqual(len(report.duplicate_targets), 1)
        issue = report.duplicate_targets[0]
        self.assertEqual(issue.reference, ref)
        self.assertEqual(issue.key, (2,))
        self.assertEqual(issue.count, 2)

    def test_dangling_reference_reported(self):
        self.books.append(('a-3', 99))
        ref = self.add_ref()
        report = self.book.verify_references()
        self.assertEqual(len(report.dangling), 1)
        issue = report.dangling[0]
        self.assertEqual(issue.reference, ref)
        self.assertEqual(issue.row, 2)
        self.assertEqual(issue.key, (99,))

    def test_values_compared_by_original_type(self):
        # '1' (str) must not match 1 (int), and 1.0 must not match 1.
        self.books.append(('a-3', '1'))
        self.books.append(('a-4', 1.0))
        self.add_ref()
        report = self.book.verify_references()
        self.assertEqual(
            [issue.key for issue in report.dangling],
            [('1',), (1.0,)],
        )

    def test_missing_value_skip_policy(self):
        self.books.append(('a-3', None))
        self.add_ref(missing=MISSING_SKIP)
        report = self.book.verify_references()
        self.assertTrue(report.ok)
        self.assertEqual(report.missing_values, [])
        self.assertEqual(report.dangling, [])

    def test_missing_value_error_policy(self):
        self.books.append(('a-3', None))
        ref = self.add_ref(missing=MISSING_ERROR)
        report = self.book.verify_references()
        self.assertFalse(report.ok)
        self.assertEqual(len(report.missing_values), 1)
        issue = report.missing_values[0]
        self.assertEqual(issue.reference, ref)
        self.assertEqual(issue.row, 2)
        self.assertEqual(issue.key, (None,))

    def test_composite_keys(self):
        cities = tablib.Dataset(title='cities')
        cities.headers = ('country', 'city')
        cities.append(('fr', 'paris'))
        trips = tablib.Dataset(title='trips')
        trips.headers = ('to_country', 'to_city')
        trips.append(('fr', 'paris'))
        trips.append(('fr', 'lyon'))
        book = tablib.Databook([cities, trips])
        book.add_reference(
            trips, ('to_country', 'to_city'),
            cities, ('country', 'city'),
            missing=MISSING_SKIP,
        )
        report = book.verify_references()
        self.assertEqual([i.key for i in report.dangling], [('fr', 'lyon')])

    def test_same_title_sheets_are_not_confused(self):
        # Two sheets sharing a title: references follow identity, not title.
        other = tablib.Dataset(title='authors')
        other.headers = ('id',)
        other.append(('x',))
        self.book.add_sheet(other)
        self.add_ref()
        report = self.book.verify_references()
        self.assertTrue(report.ok)

    # -- controlled rename ----------------------------------------------

    def test_rename_sheet_updates_reference_metadata(self):
        ref = self.add_ref()
        self.book.rename_sheet(self.authors, 'people')
        self.assertEqual(self.authors.title, 'people')
        self.assertEqual(ref.target_title, 'people')
        self.assertEqual(ref.source_title, 'books')
        self.assertTrue(self.book.verify_references().ok)

    def test_rename_sheet_unknown_sheet_rejected(self):
        stranger = tablib.Dataset(title='stranger')
        with self.assertRaises(SheetNotFound):
            self.book.rename_sheet(stranger, 'x')

    # -- controlled removal ----------------------------------------------

    def test_remove_referenced_sheet_refused(self):
        self.add_ref()
        with self.assertRaises(SheetReferenced):
            self.book.remove_sheet(self.authors)
        self.assertEqual(self.book.size, 2)

    def test_remove_sheet_with_unlink(self):
        self.add_ref()
        self.book.remove_sheet(self.authors, unlink=True)
        self.assertEqual(self.book.size, 1)
        self.assertEqual(self.book.references, ())

    def test_remove_unreferenced_sheet(self):
        self.add_ref()
        extra = tablib.Dataset(title='extra')
        self.book.add_sheet(extra)
        self.book.remove_sheet(extra)
        self.assertEqual(self.book.size, 2)
        self.assertEqual(len(self.book.references), 1)

    def test_remove_unknown_sheet_rejected(self):
        with self.assertRaises(SheetNotFound):
            self.book.remove_sheet(tablib.Dataset())

    def test_remove_reference(self):
        ref = self.add_ref()
        self.book.remove_reference(ref)
        self.assertEqual(self.book.references, ())
        with self.assertRaises(SheetNotFound):
            self.book.remove_reference(ref)

    def test_wipe_clears_references(self):
        self.add_ref()
        self.book.wipe()
        self.assertEqual(self.book.references, ())
        self.assertEqual(self.book.size, 0)


if __name__ == '__main__':
    unittest.main()
