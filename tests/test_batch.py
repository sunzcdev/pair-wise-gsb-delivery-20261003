"""Test batch selection with a shared match state."""
import bs4
import gc
import weakref
import soupsieve as sv
from soupsieve import css_match as cm
from . import util


MARKUP = """
<html>
<head></head>
<body>
<div id="d1" class="group">
  <p id="p1">one</p>
  <p id="p2">two</p>
  <span id="s1">s</span>
</div>
<div id="d2" class="group">
  <section id="sec1">
    <div id="d3"><p id="p3">three</p></div>
  </section>
</div>
<div id="d4"><span id="s2">only</span></div>
<ul id="list">
  <li id="l1">1</li>
  <li id="l2">2</li>
  <li id="l3">3</li>
  <li id="l4">4</li>
</ul>
<p id="p4">lone</p>
<p id="p5">adjacent</p>
</body>
</html>
"""

PATTERNS = [
    'div:has(> p)',
    'div:has(> span)',
    'li:has(+ li)',
    'div:has(~ div#d4)',
    'p + p',
    'li:nth-child(2n)',
    'div.group:has(> p:first-child)',
    'body > div:nth-of-type(2)',
]


class TestSelectMany(util.TestCase):
    """Test `select_many`."""

    def test_results_match_individual_selects(self):
        """Batch results must equal per-selector results, each in document order."""

        soup = self.soup(MARKUP, 'html.parser')
        batch = sv.select_many(PATTERNS, soup)
        individual = [sv.select(pattern, soup) for pattern in PATTERNS]

        self.assertEqual(len(batch), len(PATTERNS))
        for batch_result, individual_result in zip(batch, individual):
            # Same elements, same (document) order.
            self.assertEqual(batch_result, individual_result)

    def test_accepts_compiled_patterns(self):
        """Pre-compiled selectors can be mixed with pattern strings."""

        soup = self.soup(MARKUP, 'html.parser')
        compiled = sv.compile('div:has(> p)')
        batch = sv.select_many([compiled, 'p + p'], soup)

        self.assertEqual(batch[0], sv.select('div:has(> p)', soup))
        self.assertEqual(batch[1], sv.select('p + p', soup))

    def test_empty_patterns(self):
        """No patterns gives no results."""

        soup = self.soup(MARKUP, 'html.parser')
        self.assertEqual(sv.select_many([], soup), [])

    def test_namespaces_not_widened(self):
        """Namespace conditions must not be widened by the shared indexes."""

        markup = """
        <root xmlns:a="http://a" xmlns:b="http://b">
            <a:wrap id="w1"><a:item id="i1"/><b:item id="i2"/></a:wrap>
            <b:wrap id="w2"><b:item id="i3"/><a:item id="i4"/></b:wrap>
            <wrap id="w3"><item id="i5"/></wrap>
        </root>
        """
        soup = self.soup(markup, 'xml')
        namespaces = {'a': 'http://a', 'b': 'http://b'}
        patterns = [
            'a|wrap:has(> a|item)',
            'b|wrap:has(> b|item)',
            'wrap:has(> item)',
            'a|item + b|item',
            '*|wrap:has(> *|item)',
        ]

        batch = sv.select_many(patterns, soup, namespaces=namespaces)
        individual = [sv.select(p, soup, namespaces=namespaces) for p in patterns]

        self.assertEqual([el['id'] for el in batch[0]], ['w1'])
        self.assertEqual([el['id'] for el in batch[1]], ['w2'])
        # With no default namespace declared, an unprefixed type matches any namespace.
        self.assertEqual([el['id'] for el in batch[2]], ['w1', 'w2', 'w3'])
        self.assertEqual([el['id'] for el in batch[3]], ['i2'])
        self.assertEqual([el['id'] for el in batch[4]], ['w1', 'w2', 'w3'])
        for result, expected in zip(batch, individual):
            self.assertEqual(result, expected)

    def test_nested_has_scope_not_widened(self):
        """Nested `:has()` and adjacent siblings must keep their exact scope when sharing indexes."""

        soup = self.soup(MARKUP, 'html.parser')
        patterns = ['div:has(> p + p)', 'div:has(div:has(> p))', 'div:has(> p)']
        batch = sv.select_many(patterns, soup, flags=sv.NOSTRICT)
        individual = [sv.select(p, soup, flags=sv.NOSTRICT) for p in patterns]

        # `d1` has adjacent `p` children; `d3` a single `p`; `d2` only a nested `div:has(> p)`.
        self.assertEqual([el['id'] for el in batch[0]], ['d1'])
        self.assertEqual([el['id'] for el in batch[1]], ['d2'])
        self.assertEqual([el['id'] for el in batch[2]], ['d1', 'd3'])
        for result, expected in zip(batch, individual):
            self.assertEqual(result, expected)

    def test_shared_state_reduces_scans(self):
        """A shared match state scans fewer elements than one state per selector."""

        # A tree wide and deep enough that the relationship indexes get reused.
        parts = ['<html><head></head><body>']
        for i in range(30):
            parts.append(
                f'<div class="group" id="g{i}"><p id="p{i}a">x</p><p id="p{i}b">y</p>'
                f'<ul><li id="l{i}a">1</li><li id="l{i}b">2</li><li id="l{i}c">3</li></ul></div>'
            )
        parts.append('</body></html>')
        soup = self.soup('\n'.join(parts), 'html.parser')

        patterns = [
            'div.group:has(> p)',
            'div.group:has(> ul)',
            'li:has(+ li)',
            'div.group:has(> p:first-child)',
            'p + p',
            'li:nth-child(odd)',
            'div:has(> ul) > p',
            'li:has(~ li:last-child)',
        ]
        compiled = [sv.compile(p) for p in patterns]

        # One state per selector, as a series of individual queries behaves.
        individual_results = []
        individual_scans = 0
        for ss in compiled:
            state = cm.MatchState()
            individual_results.append(list(cm.CSSMatch(ss.selectors, soup, ss.namespaces, ss.flags, state).select()))
            individual_scans += state.scans

        # One state shared by all the selectors.
        shared = cm.MatchState()
        shared_results = [
            list(cm.CSSMatch(ss.selectors, soup, ss.namespaces, ss.flags, shared).select())
            for ss in compiled
        ]

        # Same answers either way.
        for shared_result, individual_result in zip(shared_results, individual_results):
            self.assertEqual(shared_result, individual_result)

        # And the shared state did strictly less repeated scanning.
        self.assertLess(shared.scans, individual_scans)

    def test_no_dom_retained_after_call(self):
        """The shared state must not leave strong references to the DOM in the global cache."""

        def run():
            soup = bs4.BeautifulSoup(MARKUP, 'html.parser')
            ref = weakref.ref(soup)
            # Discard the results so nothing here keeps the tree alive either.
            sv.select_many(PATTERNS, soup)
            return ref

        ref = run()
        gc.collect()
        self.assertIsNone(ref())

    def test_invalid_input(self):
        """Invalid input raises."""

        with self.assertRaises(TypeError):
            sv.select_many(PATTERNS, 'not a tag')
