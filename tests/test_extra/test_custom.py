"""Test custom selector aliases."""
from .. import util
import soupsieve as sv


class TestCustomSelectors(util.TestCase):
    """Test custom selector aliases."""

    MARKUP = """
    <body>
    <h1 id="1">Header 1</h1>
    <h2 id="2">Header 2</h2>
    <p id="3"></p>
    <p id="4"><span>child</span></p>
    </body>
    """

    def test_custom_select(self):
        """Ensure custom is properly passed via the API."""

        import bs4
        soup = bs4.BeautifulSoup(self.MARKUP, 'html.parser')

        custom_selectors = {
            ":--headers": "h1, h2, h3, h4, h5, h6"
        }
        tags = sv.select(':--headers', soup, custom=custom_selectors)
        self.assertEqual([t['id'] for t in tags], ['1', '2'])

    def test_custom_selectors(self):
        """Test custom selectors."""

        custom_selectors = {
            ":--headers": "h1, h2, h3, h4, h5, h6",
            ":--parent": ":has(> *|*)"
        }

        self.assert_selector(
            self.MARKUP,
            ':--headers',
            ['1', '2'],
            custom=custom_selectors,
            flags=util.HTML
        )

        self.assert_selector(
            self.MARKUP,
            ':--headers:nth-child(2)',
            ['2'],
            custom=custom_selectors,
            flags=util.HTML
        )

        self.assert_selector(
            self.MARKUP,
            'p:--parent',
            ['4'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_custom_escapes(self):
        """Test custom selectors with escapes."""

        custom_selectors = {
            r":--Header\s": "h1, h2, h3, h4, h5, h6"
        }

        self.assert_selector(
            self.MARKUP,
            r':--\HeaderS',
            ['1', '2'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_custom_selectors_exotic(self):
        """Test custom selectors."""

        custom_selectors = {":--": "h1, h2, h3, h4, h5, h6"}

        self.assert_selector(
            self.MARKUP,
            ':--',
            ['1', '2'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_custom_dependency(self):
        """Test custom selector dependency on other custom selectors."""

        custom_selectors = {
            ":--parent": ":has(> *|*)",
            ":--parent-paragraph": "p:--parent"
        }

        self.assert_selector(
            self.MARKUP,
            ':--parent-paragraph',
            ['4'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_custom_dependency_out_of_order(self):
        """Test custom selector out of order dependency."""

        custom_selectors = {
            ":--parent-paragraph": "p:--parent",
            ":--parent": ":has(> *|*)"
        }

        # Order does not matter, dependencies are resolved before anything is compiled.
        self.assert_selector(
            self.MARKUP,
            ':--parent-paragraph',
            ['4'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_custom_dependency_recursion(self):
        """Test that we fail on dependency recursion."""

        custom_selectors = {
            ":--parent-paragraph": "p:--parent",
            ":--parent": "p:--parent-paragraph"
        }

        self.assert_raises(':--parent', sv.SelectorSyntaxError, custom=custom_selectors)

    def test_custom_dependency_recursion_chain(self):
        """Test that a dependency cycle reports the full chain of references."""

        custom_selectors = {
            ":--first": "div:--second",
            ":--second": "p:--third",
            ":--third": "span:--first"
        }

        with self.assertRaises(sv.SelectorSyntaxError) as cm:
            sv.compile(':--first', custom=custom_selectors)
        self.assertIn(':--first -> :--second -> :--third -> :--first', str(cm.exception))

    def test_custom_dependency_recursion_self(self):
        """Test that a custom selector that references itself reports the cycle."""

        with self.assertRaises(sv.SelectorSyntaxError) as cm:
            sv.compile('div', custom={":--self": "p:--self"})
        self.assertIn(':--self -> :--self', str(cm.exception))

    def test_custom_dependency_undefined(self):
        """Test that an undefined dependency reports the name and its position in the definition."""

        custom_selectors = {
            ":--parent-paragraph": "p:--parent"
        }

        with self.assertRaises(sv.SelectorSyntaxError) as cm:
            sv.compile(':--parent-paragraph', custom=custom_selectors)
        e = cm.exception
        self.assertIn("Undefined custom selector ':--parent' found at position 10", str(e))
        self.assertEqual(e.context, 'p:--parent\n          ^')
        self.assertEqual(e.line, 1)
        self.assertEqual(e.col, 11)

    def test_custom_dependency_undefined_nested(self):
        """Test that dependencies are checked within functional pseudo-classes."""

        custom_selectors = {
            ":--parent-paragraph": "p:not(:--parent)"
        }

        with self.assertRaises(sv.SelectorSyntaxError) as cm:
            sv.compile(':--parent-paragraph', custom=custom_selectors)
        self.assertIn("Undefined custom selector ':--parent'", str(cm.exception))

    def test_custom_dependency_nested(self):
        """Test that dependencies within functional pseudo-classes are resolved."""

        custom_selectors = {
            ":--parent-paragraph": "p:not(:--parent)",
            ":--parent": ":has(> *|*)"
        }

        self.assert_selector(
            self.MARKUP,
            ':--parent-paragraph',
            ['3'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_custom_dependency_not_in_attribute_string(self):
        """Test that a name in an attribute string is not a dependency."""

        custom_selectors = {
            ":--mentioned": '[title=":--undefined"]'
        }

        # `:--undefined` only appears in an attribute string, so it is not a reference.
        self.assert_selector(
            self.MARKUP.replace('<h1 id="1">', '<h1 id="1" title=":--undefined">'),
            ':--mentioned',
            ['1'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_custom_dependency_not_in_escaped_identifier(self):
        """Test that a name in an escaped identifier is not a dependency."""

        custom_selectors = {
            ":--mentioned": r'.\:--undefined'
        }

        # `:--undefined` is part of an escaped class identifier, so it is not a reference.
        self.assert_selector(
            self.MARKUP.replace('<h2 id="2">', '<h2 id="2" class=":--undefined">'),
            ':--mentioned',
            ['2'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_custom_dependency_shared(self):
        """Test that a shared dependency is only compiled once."""

        from unittest import mock

        custom_selectors = {
            ":--shared": "h1, h2",
            ":--first": ":--shared:nth-child(1)",
            ":--second": ":--shared:nth-child(2)",
            ":--either": ":--first, :--second"
        }

        original = sv.cp.CSSParser.process_selectors
        calls = []

        def process_selectors(parser, index=0, flags=0):
            """Track the custom definitions that get compiled."""

            if parser.custom is not None:
                calls.append(parser.pattern)
            return original(parser, index, flags)

        with mock.patch.object(sv.cp.CSSParser, 'process_selectors', process_selectors):
            custom = sv.CustomSelectors(custom_selectors)

        # Each definition is compiled exactly once, no matter how often it is referenced.
        self.assertEqual(len(calls), 4)

        self.assert_selector(
            self.MARKUP,
            ':--either',
            ['1', '2'],
            custom=custom,
            flags=util.HTML
        )

    def test_custom_dependency_error_preserves_config(self):
        """Test that a failed compile neither modifies the config nor poisons the cache."""

        custom_selectors = {
            ":--parent-paragraph": "p:--parent"
        }
        snapshot = dict(custom_selectors)

        sv.purge()
        self.assertEqual(sv.cp._cached_css_compile.cache_info().currsize, 0)
        with self.assertRaises(sv.SelectorSyntaxError):
            sv.compile(':--parent-paragraph', custom=custom_selectors)

        # The passed in configuration is untouched and no partial results were cached.
        self.assertEqual(custom_selectors, snapshot)
        self.assertEqual(sv.cp._cached_css_compile.cache_info().currsize, 0)

        # Fixing the definition of the same name and retrying works.
        custom_selectors[":--parent"] = ":has(> *|*)"
        self.assert_selector(
            self.MARKUP,
            ':--parent-paragraph',
            ['4'],
            custom=custom_selectors,
            flags=util.HTML
        )

    def test_bad_custom(self):
        """Test that a bad custom raises a syntax error."""

        custom_selectors = {
            ":--parent": ":has(> *|*)",
            ":--parent-paragraph": "p:--parent"
        }

        self.assert_raises(':--wrong', sv.SelectorSyntaxError, custom=custom_selectors)

    def test_bad_custom_syntax(self):
        """Test that a custom selector with bad syntax in its name fails."""

        self.assert_raises('div', sv.SelectorSyntaxError, custom={":--parent.": ":has(> *|*)"})

    def test_pseudo_class_collision(self):
        """Test that a custom selector cannot match an already existing pseudo-class name."""

        self.assert_raises('div', sv.SelectorSyntaxError, custom={":hover": ":has(> *|*)"})

    def test_custom_collision(self):
        """Test that a custom selector cannot match an already existing custom name."""

        self.assert_raises('div', KeyError, custom={":--parent": ":has(> *|*)", ":--PARENT": ":has(> *|*)"})
