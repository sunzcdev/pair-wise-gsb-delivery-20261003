"""Test restricted (closed-scope) matching."""
import soupsieve as sv
from bs4 import BeautifulSoup
from . import util


class TestRestricted(util.TestCase):
    """Test the explicit restricted mode that closes matching around a record root."""

    # Two adjacent record roots under a common wrapper, each with its own internal structure.
    MARKUP = """
    <section id="wrap">
    <div class="record" id="r1">
        <p class="lead" id="r1-lead">lead1</p>
        <p id="r1-p2">p2-1</p>
        <p id="r1-p3">p3-1</p>
        <span id="r1-span"><b id="r1-b">b1</b></span>
    </div>
    <div class="record" id="r2">
        <p class="lead" id="r2-lead">lead2</p>
        <p id="r2-p2">p2-2</p>
        <p id="r2-p3">p3-2</p>
    </div>
    </section>
    """

    def _soup(self):
        """Parse the shared markup."""

        return self.soup(self.MARKUP, 'html.parser')

    def test_restricted_select_stays_inside_record(self):
        """An ancestor combinator cannot leave the record root in restricted mode."""

        soup = self._soup()
        r1 = soup.select_one('#r1')

        # Normal mode keeps the original DOM relations and walks up to `section`.
        self.assertEqual(
            [el['id'] for el in sv.select('section p', r1)],
            ['r1-lead', 'r1-p2', 'r1-p3']
        )
        # Restricted mode stops at the record root, so `section p` matches nothing inside it.
        self.assertEqual(sv.select('section p', r1, scope=r1), [])
        # A selector satisfied entirely inside the subtree still matches.
        self.assertEqual(
            [el['id'] for el in sv.select('div p', r1, scope=r1)],
            ['r1-lead', 'r1-p2', 'r1-p3']
        )

    def test_scope_points_at_record_root(self):
        """:scope refers to the explicit record root in restricted mode."""

        soup = self._soup()
        r1 = soup.select_one('#r1')

        self.assertTrue(sv.match(':scope', r1, scope=r1))
        self.assertFalse(sv.match(':scope', soup.select_one('#r1-lead'), scope=r1))
        self.assertEqual(
            [el['id'] for el in sv.select(':scope > p', r1, scope=r1)],
            ['r1-lead', 'r1-p2', 'r1-p3']
        )
        self.assertTrue(sv.match(':scope > p', soup.select_one('#r1-p2'), scope=r1))

    def test_adjacent_records_are_isolated(self):
        """Two adjacent records that mutually satisfy a condition are isolated in restricted mode."""

        soup = self._soup()
        r1 = soup.select_one('#r1')
        r2 = soup.select_one('#r2')
        r2_lead = soup.select_one('#r2-lead')

        # In normal mode the selector chain can walk out of r2 to the preceding record root r1.
        self.assertTrue(sv.match('.record + .record > p', r2_lead))
        # In restricted mode the sibling/ancestor walk stops at r2, so r1 cannot participate.
        self.assertFalse(sv.match('.record + .record > p', r2_lead, scope=r2))
        # The same node still matches a condition satisfied purely inside its own record.
        self.assertTrue(sv.match(':scope > p', r2_lead, scope=r2))
        # r1 cannot be matched while scoped to r2.
        self.assertFalse(sv.match('div', r1, scope=r2))

    def test_internal_relative_selector_still_matches(self):
        """Adjacent and general sibling combinators inside the record keep working."""

        soup = self._soup()
        r1 = soup.select_one('#r1')
        r2 = soup.select_one('#r2')

        # The adjacent sibling condition is met inside each record.
        self.assertTrue(sv.match('p.lead + p', soup.select_one('#r1-p2'), scope=r1))
        self.assertTrue(sv.match('p.lead + p', soup.select_one('#r2-p2'), scope=r2))
        # General sibling inside the record works.
        self.assertTrue(sv.match('p.lead ~ p', soup.select_one('#r1-p3'), scope=r1))
        # A general sibling that would have to reach the other record does not.
        self.assertFalse(
            sv.match('.record ~ .record p', soup.select_one('#r2-p2'), scope=r2)
        )

    def test_has_works_inside_subtree(self):
        """:has and its relative combinators work normally within the record root."""

        soup = self._soup()
        r1 = soup.select_one('#r1')
        r2 = soup.select_one('#r2')

        self.assertEqual(
            [el['id'] for el in sv.select('span:has(b)', r1, scope=r1)],
            ['r1-span']
        )
        self.assertEqual(
            [el['id'] for el in sv.select('p:has(~ p)', r1, scope=r1)],
            ['r1-lead', 'r1-p2']
        )
        self.assertEqual(
            [el['id'] for el in sv.select('p:has(+ p)', r2, scope=r2)],
            ['r2-lead', 'r2-p2']
        )

    def test_has_cannot_cross_boundary(self):
        """:has cannot reach a sibling of the record root in restricted mode."""

        soup = self._soup()
        r1 = soup.select_one('#r1')

        # Normal mode sees the following sibling record r2.
        self.assertEqual(
            [el['id'] for el in sv.select('.record:has(+ .record)', soup)],
            ['r1']
        )
        # Restricted mode cannot look past the record root to its sibling.
        self.assertEqual(sv.select('.record:has(+ .record)', r1, scope=r1), [])

    def test_nth_is_computed_per_record(self):
        """nth selectors count among the siblings inside the record, not the whole document."""

        soup = self._soup()
        r1 = soup.select_one('#r1')
        r2 = soup.select_one('#r2')

        self.assertEqual(
            [el['id'] for el in sv.select('p:nth-child(2)', r1, scope=r1)],
            ['r1-p2']
        )
        self.assertEqual(
            [el['id'] for el in sv.select('p:nth-of-type(1)', r2, scope=r2)],
            ['r2-lead']
        )
        self.assertEqual(
            [el['id'] for el in sv.select('p:nth-last-of-type(1)', r1, scope=r1)],
            ['r1-p3']
        )

    def test_closest_respects_boundary(self):
        """closest never returns an ancestor above the record root in restricted mode."""

        soup = self._soup()
        r1 = soup.select_one('#r1')
        b = soup.select_one('#r1-b')

        self.assertEqual(sv.closest('section', b)['id'], 'wrap')
        self.assertIsNone(sv.closest('section', b, scope=r1))
        self.assertEqual(sv.closest('span', b, scope=r1)['id'], 'r1-span')
        self.assertEqual(sv.closest('div', b, scope=r1)['id'], 'r1')

    def test_out_of_scope_target_rejected(self):
        """A node outside of the record root never matches in restricted mode."""

        soup = self._soup()
        r1 = soup.select_one('#r1')

        self.assertFalse(sv.match('p', soup.select_one('#r2-lead'), scope=r1))
        self.assertTrue(sv.match('p', soup.select_one('#r2-lead')))

    def test_compiled_object_shares_boundary(self):
        """The compiled SoupSieve object applies the same boundary across its methods."""

        soup = self._soup()
        r1 = soup.select_one('#r1')
        r2 = soup.select_one('#r2')
        pattern = sv.compile('p.lead + p')

        self.assertTrue(pattern.match(soup.select_one('#r2-p2')))
        self.assertTrue(pattern.match(soup.select_one('#r2-p2'), r2))
        self.assertFalse(pattern.match(soup.select_one('#r2-lead'), r2))
        self.assertEqual(
            [el['id'] for el in pattern.select(r2, scope=r2)],
            ['r2-p2']
        )
        self.assertEqual(pattern.select_one(r2, scope=r2)['id'], 'r2-p2')
        self.assertEqual(
            [el['id'] for el in pattern.filter(r2, scope=r2)],
            ['r2-p2']
        )
        # The same compiled selector used on another record is isolated from it.
        self.assertEqual(pattern.select(r1, scope=r1)[0]['id'], 'r1-p2')

    def test_restricted_select_one_iselect_filter(self):
        """All selection entry points honor the same boundary."""

        soup = self._soup()
        r2 = soup.select_one('#r2')

        self.assertEqual(sv.select_one('p', r2, scope=r2)['id'], 'r2-lead')
        self.assertEqual(
            [el['id'] for el in sv.iselect('p', r2, scope=r2)],
            ['r2-lead', 'r2-p2', 'r2-p3']
        )
        self.assertEqual(
            [el['id'] for el in sv.filter('p', r2, scope=r2)],
            ['r2-lead', 'r2-p2', 'r2-p3']
        )
        # Nothing from the adjacent record leaks through a universal selector.
        self.assertEqual(
            [el['id'] for el in sv.select('*', r2, scope=r2)],
            ['r2-lead', 'r2-p2', 'r2-p3']
        )

    def test_restricted_mode_without_cache(self):
        """The boundary is enforced on the non-cached code path as well."""

        soup = self._soup()
        r1 = soup.select_one('#r1')
        r2 = soup.select_one('#r2')

        self.assertFalse(
            sv.match(
                '.record + .record > p',
                soup.select_one('#r2-lead'),
                scope=r2,
                flags=sv.NOCACHE
            )
        )
        self.assertTrue(
            sv.match('p.lead ~ p', soup.select_one('#r1-p3'), scope=r1, flags=sv.NOCACHE)
        )
        self.assertEqual(
            [el['id'] for el in sv.select('p:has(+ p)', r2, scope=r2, flags=sv.NOCACHE)],
            ['r2-lead', 'r2-p2']
        )

    def test_normal_mode_is_unchanged(self):
        """Without an explicit scope, matching follows the original document-wide DOM relations."""

        soup = self._soup()
        r1 = soup.select_one('#r1')

        self.assertEqual(
            [el['id'] for el in sv.select('section p', r1)],
            ['r1-lead', 'r1-p2', 'r1-p3']
        )
        self.assertTrue(sv.match('section p', soup.select_one('#r1-p3')))
        self.assertEqual(sv.closest('section', soup.select_one('#r1-b'))['id'], 'wrap')

    def test_scope_equal_to_target(self):
        """Using the matched element itself as the record root keeps it and its subtree in scope."""

        soup = self._soup()
        r1 = soup.select_one('#r1')

        # A record root is a valid match for itself, and descendants resolve relative to it.
        self.assertTrue(sv.match('div.record', r1, scope=r1))
        self.assertTrue(sv.match(':scope', r1, scope=r1))
        self.assertEqual(
            [el['id'] for el in sv.select(':scope b', r1, scope=r1)],
            ['r1-b']
        )

    def test_invalid_scope_type(self):
        """A scope that is not a tag is rejected like any other invalid input."""

        soup = self._soup()

        with self.assertRaises(TypeError):
            sv.match('p', soup.select_one('#r1-p2'), scope='not a tag')
        with self.assertRaises(TypeError):
            sv.select('p', soup, scope=123)
        with self.assertRaises(TypeError):
            sv.closest('p', soup.select_one('#r1-p2'), scope=object())

    def test_detached_record_root(self):
        """A detached subtree is its own closed scope with no outside relations."""

        root = BeautifulSoup(
            '<div id="rec"><p class="lead" id="a"></p><p id="b"></p></div>',
            'html.parser'
        ).div
        p_b = root.select_one('#b')

        self.assertTrue(sv.match('p.lead + p', p_b, scope=root))
        self.assertTrue(sv.match(':scope > p', p_b, scope=root))
        self.assertEqual(
            [el['id'] for el in sv.select('p', root, scope=root)],
            ['a', 'b']
        )
        # There is no ancestor above the detached root to match.
        self.assertIsNone(sv.closest('section', root, scope=root))

    def test_document_scope(self):
        """Scoping to the document node uses the document element as the record root."""

        soup = self._soup()
        r2_lead = soup.select_one('#r2-lead')
        # With `html.parser` the fragment's outermost element is the document element.
        doc_root = soup.find(True)

        # The document element is the boundary, so the whole tree is inside and normal relations hold.
        self.assertTrue(sv.match('section p', r2_lead, scope=soup))
        self.assertEqual(
            [el['id'] for el in sv.select('p', soup, scope=soup)],
            ['r1-lead', 'r1-p2', 'r1-p3', 'r2-lead', 'r2-p2', 'r2-p3']
        )
        self.assertTrue(sv.match(':scope', doc_root, scope=soup))
        # `:scope` is the document element, not the document node itself.
        self.assertFalse(sv.match(':scope', soup, scope=soup))
