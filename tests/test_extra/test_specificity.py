"""Test static specificity analysis of compiled selectors."""
import soupsieve as sv
from .. import util
import pickle


class TestSpecificity(util.TestCase):
    """Test specificity calculations on compiled selectors."""

    def test_basic(self):
        """Test basic selector weights."""

        self.assertEqual(sv.compile('div').specificity(), [(0, 0, 1)])
        self.assertEqual(sv.compile('*').specificity(), [(0, 0, 0)])
        self.assertEqual(sv.compile('#id').specificity(), [(1, 0, 0)])
        self.assertEqual(sv.compile('.class').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile('div.class#id').specificity(), [(1, 1, 1)])

    def test_selector_list(self):
        """Test that specificity is reported per selector in the list, in order."""

        self.assertEqual(
            sv.compile('div, #id, .class, *').specificity(),
            [(0, 0, 1), (1, 0, 0), (0, 1, 0), (0, 0, 0)]
        )

    def test_combinators(self):
        """Test that combinators add no weight, but each compound selector counts."""

        self.assertEqual(sv.compile('div > p').specificity(), [(0, 0, 2)])
        self.assertEqual(sv.compile('ul li a.nav').specificity(), [(0, 1, 3)])
        self.assertEqual(sv.compile('#a + #b ~ #c').specificity(), [(3, 0, 0)])

    def test_attributes(self):
        """Test that attribute selectors count once, regardless of their value."""

        self.assertEqual(sv.compile('[href]').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile('[type=checkbox]').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile('[class~=a][class~=b]').specificity(), [(0, 2, 0)])

    def test_namespaces(self):
        """Test that a namespace prefix is not counted as an extra element."""

        namespaces = {'ns': 'http://www.w3.org/1999/xhtml', 'xhtml': 'http://www.w3.org/1999/xhtml'}
        self.assertEqual(sv.compile('ns|div', namespaces=namespaces).specificity(), [(0, 0, 1)])
        self.assertEqual(sv.compile('ns|*', namespaces=namespaces).specificity(), [(0, 0, 0)])
        self.assertEqual(sv.compile('*|div').specificity(), [(0, 0, 1)])
        self.assertEqual(sv.compile('[ns|attr=value]', namespaces=namespaces).specificity(), [(0, 1, 0)])

    def test_where(self):
        """Test that `:where()` and its contents have zero weight."""

        self.assertEqual(sv.compile(':where(div)').specificity(), [(0, 0, 0)])
        self.assertEqual(sv.compile(':where(#id, .class, div)').specificity(), [(0, 0, 0)])
        self.assertEqual(sv.compile(':where(#id) .class').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile('div:where(:where(#id))').specificity(), [(0, 0, 1)])

    def test_is_not_has(self):
        """Test that `:is()`, `:not()`, and `:has()` take on their most specific candidate."""

        self.assertEqual(sv.compile(':is(div, #id)').specificity(), [(1, 0, 0)])
        self.assertEqual(sv.compile(':not(.class, #id)').specificity(), [(1, 0, 0)])
        self.assertEqual(sv.compile('div:has(> span, #id)').specificity(), [(1, 0, 1)])
        self.assertEqual(sv.compile(':is(:is(div, #id))').specificity(), [(1, 0, 0)])
        # A `:where()` candidate contributes zero to the maximum.
        self.assertEqual(sv.compile(':is(:where(#id), span)').specificity(), [(0, 0, 1)])

    def test_nth(self):
        """Test `nth` pseudo-classes, including the `of S` clause."""

        self.assertEqual(sv.compile(':nth-child(2n)').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile(':nth-of-type(odd)').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile(':nth-child(2n of .class, #id)').specificity(), [(1, 1, 0)])
        self.assertEqual(sv.compile(':nth-child(2n of :where(#id))').specificity(), [(0, 1, 0)])

    def test_pseudo_class_flags(self):
        """Test pseudo-classes stored as flags count once each."""

        self.assertEqual(sv.compile(':root').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile(':empty').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile(':scope').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile(':defined').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile(':dir(ltr)').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile(':lang(en)').specificity(), [(0, 1, 0)])
        self.assertEqual(sv.compile(':lang(en, fr)').specificity(), [(0, 1, 0)])

    def test_contains(self):
        """Test that the contains pseudo-class counts as a pseudo-class."""

        self.assertEqual(sv.compile(':-soup-contains(text)').specificity(), [(0, 1, 0)])

    def test_custom_selectors(self):
        """Test that custom pseudo-classes are scored by their expanded semantics."""

        custom = {':--header': 'h1, h2#id'}
        self.assertEqual(sv.compile(':--header', custom=custom).specificity(), [(1, 0, 1)])

        custom = {':--zero': ':where(#id) div'}
        self.assertEqual(sv.compile(':--zero', custom=custom).specificity(), [(0, 0, 1)])

        custom = {':--inner': '.class', ':--outer': 'article:--inner'}
        self.assertEqual(sv.compile(':--outer', custom=custom).specificity(), [(0, 1, 1)])

    def test_expanded_pseudo_classes(self):
        """Test that pseudo-classes that expand to selectors are scored by their expansion."""

        # `:link` expands to `html|*:is(a, area)[href]`
        self.assertEqual(sv.compile(':link').specificity(), [(0, 1, 1)])
        self.assertEqual(sv.compile(':checked').specificity(), [(0, 2, 1)])

    def test_no_match(self):
        """Test that selectors which match nothing have no weight."""

        self.assertEqual(sv.compile('p:hover').specificity(), [(0, 0, 0)])
        self.assertEqual(sv.compile(':is(p, :hover)').specificity(), [(0, 0, 1)])
        # Forgiven empty slots in `:is()` contribute nothing.
        self.assertEqual(sv.compile(':is(div, )').specificity(), [(0, 0, 1)])

    def test_pseudo_element(self):
        """Test that pseudo-elements are rejected, so no score is ever guessed for them."""

        with self.assertRaises(NotImplementedError):
            sv.compile('p::before')

    def test_deep_nesting(self):
        """Test that deeply nested selectors do not hit the recursion limit."""

        depth = 5000
        pattern = ':is(' * depth + 'div' + ')' * depth
        self.assertEqual(sv.compile(pattern).specificity(), [(0, 0, 1)])

        pattern = ' > '.join(['div'] * 3000)
        self.assertEqual(sv.compile(pattern).specificity(), [(0, 0, 3000)])

    def test_pickle(self):
        """Test that specificity survives pickling of the compiled pattern."""

        pattern = sv.compile('div:where(#id), :is(a, #b)')
        pattern2 = pickle.loads(pickle.dumps(pattern))
        self.assertEqual(pattern2.specificity(), [(0, 0, 1), (1, 0, 0)])

    def test_matching_unchanged(self):
        """Test that carrying specificity information does not change matching."""

        markup = """
        <div id="a">
            <p id="b" class="x">text</p>
            <span id="c">more</span>
        </div>
        """
        soup = self.soup(markup, 'html.parser')
        self.assertEqual(
            [el['id'] for el in sv.select('div:where(#a) > :is(p, span)', soup)],
            ['b', 'c']
        )
        self.assertTrue(sv.match('p.x:has(+ span)', soup.find('p')))
