"""Test state snapshots."""
from .. import util
import soupsieve as sv
import bs4


class TestStates(util.TestCase):
    """Test matching against a read-only snapshot of runtime states."""

    MARKUP = """
    <form id="form">
      <input id="cb1" type="checkbox">
      <input id="cb2" type="checkbox" checked>
      <input id="r1" type="radio" name="group" checked>
      <input id="r2" type="radio" name="group">
      <input id="r3" type="radio" checked>
      <select id="single">
        <option id="o1" selected>one</option>
        <option id="o2">two</option>
        <option id="o3" disabled>three</option>
      </select>
      <select id="multi" multiple>
        <option id="m1" selected>one</option>
        <option id="m2" selected>two</option>
      </select>
      <input id="text" type="text">
      <input id="off" type="text" disabled>
      <fieldset id="fs">
        <legend>set</legend>
        <input id="inner" type="text">
      </fieldset>
    </form>
    """

    def setUp(self):
        """Setup."""

        super().setUp()
        self.soup = bs4.BeautifulSoup(self.MARKUP, 'html.parser')
        self.el = {i: self.soup.find(id=i) for i in (
            'cb1', 'cb2', 'r1', 'r2', 'r3', 'o1', 'o2', 'o3', 'm1', 'm2', 'text', 'off', 'fs', 'inner'
        )}

    def ids(self, selector, states=None, flags=0):
        """Select and return the ids."""

        return [e['id'] for e in sv.select(selector, self.soup, states=states, flags=flags)]

    def test_checked_override(self):
        """A snapshot adds and removes checkedness without touching the DOM."""

        self.assertEqual(self.ids(':checked'), ['cb2', 'r1', 'r3', 'o1', 'm1', 'm2'])
        states = {self.el['cb1']: {'checked': True}, self.el['cb2']: {'checked': False}}
        self.assertEqual(self.ids(':checked', states), ['cb1', 'r1', 'r3', 'o1', 'm1', 'm2'])
        # The static DOM is unchanged and still governs a match without a snapshot.
        self.assertEqual(self.el['cb1'].attrs, {'id': 'cb1', 'type': 'checkbox'})
        self.assertEqual(self.el['cb2'].attrs, {'id': 'cb2', 'type': 'checkbox', 'checked': ''})
        self.assertEqual(self.ids(':checked'), ['cb2', 'r1', 'r3', 'o1', 'm1', 'm2'])

    def test_uncovered_nodes_use_static_dom(self):
        """Nodes the snapshot does not cover are matched against the static DOM."""

        states = {self.el['cb1']: {'checked': True}}
        self.assertTrue(sv.match(':checked', self.el['cb2'], states=states))
        self.assertFalse(sv.match(':checked', self.el['cb1']))
        self.assertTrue(sv.match(':checked', self.el['cb1'], states=states))

    def test_identity_not_id_attribute(self):
        """The snapshot locates nodes by identity, never by a shared `id` attribute."""

        soup = bs4.BeautifulSoup(
            '<input id="same" type="checkbox"><input id="same" type="checkbox">', 'html.parser'
        )
        first, second = soup.find_all('input')
        states = {second: {'checked': True}}
        self.assertFalse(sv.match(':checked', first, states=states))
        self.assertTrue(sv.match(':checked', second, states=states))
        self.assertEqual([e is second for e in sv.select(':checked', soup, states=states)], [True])

    def test_radio_group_consistency(self):
        """Checking one radio button of a group unchecks the others."""

        states = {self.el['r2']: {'checked': True}}
        self.assertEqual(self.ids('input[name="group"]:checked', states), ['r2'])
        self.assertFalse(sv.match(':checked', self.el['r1'], states=states))
        # The unnamed radio button is a group of its own and is unaffected.
        self.assertTrue(sv.match(':checked', self.el['r3'], states=states))

    def test_radio_group_unchecked(self):
        """Unchecking the checked radio button of a group leaves the whole group unset."""

        states = {self.el['r1']: {'checked': False}}
        self.assertEqual(self.ids('input[name="group"]:checked', states), [])
        self.assertEqual(sorted(self.ids('input[name="group"]:indeterminate', states)), ['r1', 'r2'])

    def test_select_default_option_rules(self):
        """Selectedness overrides follow the same rules as the static DOM."""

        # A later selected option wins over an earlier one.
        states = {self.el['o2']: {'selected': True}}
        self.assertEqual(self.ids('#single :checked', states), ['o2'])
        # Unmarking the only marked option selects the first enabled option again.
        states = {self.el['o1']: {'selected': False}}
        self.assertEqual(self.ids('#single :checked', states), ['o1'])
        # Unless it is disabled, in which case the next enabled option is selected.
        states = {self.el['o1']: {'selected': False, 'disabled': True}}
        self.assertEqual(self.ids('#single :checked', states), ['o2'])

    def test_select_multiple(self):
        """A `multiple` select allows any combination of selected options."""

        states = {self.el['m1']: {'selected': False}}
        self.assertEqual(self.ids('#multi :checked', states), ['m2'])
        states = {self.el['m1']: {'selected': False}, self.el['m2']: {'selected': False}}
        self.assertEqual(self.ids('#multi :checked', states), [])

    def test_option_without_select(self):
        """An option outside a `select` is selected by its own state alone."""

        soup = bs4.BeautifulSoup('<option id="a">1</option><option id="b" selected>2</option>', 'html.parser')
        a, b = soup.find_all('option')
        self.assertEqual([e['id'] for e in sv.select(':checked', soup, states={a: {'selected': True}})], ['a', 'b'])
        self.assertEqual([e['id'] for e in sv.select(':checked', soup, states={b: {'selected': False}})], [])

    def test_disabled_enabled(self):
        """Disabledness overrides drive `:disabled` and `:enabled`."""

        states = {self.el['text']: {'disabled': True}, self.el['off']: {'disabled': False}}
        self.assertTrue(sv.match(':disabled', self.el['text'], states=states))
        self.assertFalse(sv.match(':enabled', self.el['text'], states=states))
        self.assertTrue(sv.match(':enabled', self.el['off'], states=states))
        self.assertFalse(sv.match(':disabled', self.el['off'], states=states))
        # Static behavior is intact without a snapshot.
        self.assertTrue(sv.match(':disabled', self.el['off']))
        self.assertTrue(sv.match(':enabled', self.el['text']))

    def test_disabled_fieldset_propagates(self):
        """A disabled `fieldset` disables its controls, as in the static DOM."""

        self.assertTrue(sv.match(':enabled', self.el['inner']))
        states = {self.el['fs']: {'disabled': True}}
        self.assertTrue(sv.match(':disabled', self.el['inner'], states=states))
        self.assertFalse(sv.match(':enabled', self.el['inner'], states=states))

    def test_disabled_optgroup_propagates(self):
        """A disabled `optgroup` disables its options, as in the static DOM."""

        soup = bs4.BeautifulSoup(
            '<select><optgroup id="g"><option id="a">1</option></optgroup></select>', 'html.parser'
        )
        group = soup.find('optgroup')
        option = soup.find('option')
        self.assertTrue(sv.match(':enabled', option))
        self.assertTrue(sv.match(':disabled', option, states={group: {'disabled': True}}))

    def test_interleaved_snapshots_do_not_leak(self):
        """Alternating snapshots on one tree never bleed state into each other."""

        s1 = {self.el['r1']: {'checked': True}}
        s2 = {self.el['r2']: {'checked': True}}
        for flags in (0, sv.NOCACHE):
            for _ in range(2):
                self.assertEqual(self.ids('input[name="group"]:checked', s1, flags), ['r1'])
                self.assertEqual(self.ids('input[name="group"]:checked', s2, flags), ['r2'])
                self.assertEqual(self.ids('input[name="group"]:checked', None, flags), ['r1'])

    def test_compiled_selector_apis(self):
        """All matching entry points accept a snapshot."""

        sel = sv.compile(':checked')
        states = {self.el['cb1']: {'checked': True}}
        self.assertTrue(sel.match(self.el['cb1'], states=states))
        self.assertEqual(sel.select(self.soup, states=states)[0]['id'], 'cb1')
        self.assertEqual(sel.select_one(self.soup, states=states)['id'], 'cb1')
        self.assertEqual([e['id'] for e in sel.iselect(self.soup, states=states)][0], 'cb1')
        self.assertEqual([e['id'] for e in sel.filter(self.soup.form, states=states)], ['cb1', 'cb2', 'r1', 'r3'])
        self.assertEqual(
            sv.filter(':checked', [self.el['cb1'], self.el['cb2']], states=states),
            [self.el['cb1'], self.el['cb2']]
        )
        self.assertEqual(sv.filter(':checked', [self.el['cb1'], self.el['cb2']]), [self.el['cb2']])

        disabled = sv.compile(':disabled')
        # The input itself is the closest element disabled through the fieldset.
        self.assertEqual(
            disabled.closest(self.el['inner'], states={self.el['fs']: {'disabled': True}})['id'], 'inner'
        )
        self.assertIsNone(disabled.closest(self.el['inner']))

    def test_detached_tree(self):
        """A snapshot applies to a detached tree the same as to a document."""

        soup = bs4.BeautifulSoup('<input type="checkbox">', 'html.parser')
        checkbox = soup.find('input').extract()
        self.assertFalse(sv.match(':checked', checkbox))
        self.assertTrue(sv.match(':checked', checkbox, states={checkbox: {'checked': True}}))

    def test_unknown_node(self):
        """A node of another tree is diagnosed before matching."""

        other = bs4.BeautifulSoup('<input type="checkbox">', 'html.parser').find('input')
        with self.assertRaises(ValueError):
            sv.match(':checked', self.el['cb1'], states={other: {'checked': True}})

    def test_unknown_state(self):
        """An unknown state name is diagnosed before matching."""

        with self.assertRaises(ValueError):
            sv.match(':checked', self.el['cb1'], states={self.el['cb1']: {'hover': True}})

    def test_inapplicable_states(self):
        """A state that does not apply to an element is diagnosed before matching."""

        with self.assertRaises(ValueError):
            sv.match('input', self.el['text'], states={self.el['text']: {'checked': True}})
        with self.assertRaises(ValueError):
            sv.match('input', self.el['text'], states={self.el['text']: {'selected': True}})
        with self.assertRaises(ValueError):
            sv.match('form', self.soup.form, states={self.soup.form: {'disabled': True}})
        with self.assertRaises(ValueError):
            sv.match(':checked', self.el['o1'], states={self.el['o1']: {'checked': True}})

    def test_bad_snapshot_types(self):
        """Keys that are not tags and values that are not mappings are diagnosed."""

        with self.assertRaises(TypeError):
            sv.match(':checked', self.el['cb1'], states={'cb1': {'checked': True}})
        with self.assertRaises(TypeError):
            sv.match(':checked', self.el['cb1'], states={self.el['cb1']: True})

    @util.requires_lxml
    def test_xml_states_inapplicable(self):
        """States have no meaning on the elements of an XML document."""

        soup = bs4.BeautifulSoup('<input type="checkbox" checked=""/>', 'xml')
        node = soup.find('input')
        with self.assertRaises(ValueError):
            sv.match('input', node, states={node: {'checked': True}})

    @util.requires_lxml
    def test_xhtml_states(self):
        """States apply to HTML elements in an XHTML document."""

        soup = bs4.BeautifulSoup(
            self.wrap_xhtml('<input id="cb" type="checkbox"/>'), 'xml'
        )
        node = soup.find('input')
        self.assertFalse(sv.match(':checked', node))
        self.assertTrue(sv.match(':checked', node, states={node: {'checked': True}}))
