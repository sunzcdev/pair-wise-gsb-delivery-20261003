import operator
from collections.abc import Mapping

import pytest

from glom import Glommer, Iter, PathAccessError, T
from glom.core import TargetRegistry


class BaseMapping(Mapping):
    """Shared container type with ordinary, exact-key getitem access."""
    def __init__(self, data=None):
        self._data = dict(data or {})

    def __getitem__(self, key):
        return self._data[key]

    def __iter__(self):
        return iter(self._data)

    def __len__(self):
        return len(self._data)


class PlainMapping(BaseMapping):
    """A source container that follows the shared, parent convention."""


class ShoutingMapping(BaseMapping):
    """A source container whose real keys are uppercase-normalized.

    Default getitem needs the exact key, so a local ``get`` override is
    required to teach glom this container's reading convention.
    """
    def __init__(self, data=None):
        super().__init__({str(key).upper(): value for key, value in (data or {}).items()})

    def normalized_get(self, key):
        return self._data[str(key).upper()]


def _make_glommer():
    glommer = Glommer()
    # the convention shared by every derived context: mappings use getitem
    glommer.register(BaseMapping, get=operator.getitem)
    return glommer


def test_override_and_fallback_with_two_mappings():
    glommer = _make_glommer()
    plain = PlainMapping({'name': 'plain'})
    shouty = ShoutingMapping({'name': 'shouty'})

    # baseline: the shared getitem convention requires exact keys
    assert glommer.glom(plain, 'name') == 'plain'
    with pytest.raises(PathAccessError):
        glommer.glom(shouty, 'name')

    with glommer.register_context() as local:
        # source 2's convention, registered only inside this context
        local.register(ShoutingMapping, get=ShoutingMapping.normalized_get)

        # the override takes effect for its own type ...
        assert local.glom(shouty, 'name') == 'shouty'
        # ... while the other Mapping falls back to the parent's handler
        assert local.glom(plain, 'name') == 'plain'

    # exiting the block restores fully independent parent behavior
    with pytest.raises(PathAccessError):
        glommer.glom(shouty, 'name')
    assert glommer.glom(plain, 'name') == 'plain'


def test_context_uses_creation_time_snapshot():
    glommer = _make_glommer()

    with glommer.register_context() as local:
        # a parent registration made *after* the snapshot must not leak in
        glommer.register(ShoutingMapping, get=ShoutingMapping.normalized_get)

        parent_reg = glommer.scope[TargetRegistry]
        local_reg = local.scope[TargetRegistry]
        assert local_reg is not parent_reg
        assert ShoutingMapping not in local_reg.get_type_map('get')
        with pytest.raises(PathAccessError):
            local.glom(ShoutingMapping({'name': 'x'}), 'name')

        # and a local override must never leak back out to the parent
        local.register(PlainMapping, get=lambda target, key: 'local-only')
        assert local.glom(PlainMapping({'name': 'x'}), 'name') == 'local-only'

    assert glommer.glom(PlainMapping({'name': 'x'}), 'name') == 'x'


def test_derived_glommer_keeps_independent_config_after_context():
    glommer = _make_glommer()

    with glommer.register_context() as local:
        local.register(ShoutingMapping, get=ShoutingMapping.normalized_get)
        derived = local

    shouty = ShoutingMapping({'name': 'kept'})
    # the derived object outlives the with-block and keeps its own registry
    assert derived.glom(shouty, 'name') == 'kept'
    # the parent is untouched
    with pytest.raises(PathAccessError):
        glommer.glom(shouty, 'name')

    # mutating one registry after the fact never reaches the other
    derived.register(PlainMapping, get=lambda target, key: 'derived')
    assert glommer.glom(PlainMapping({'name': 'parent'}), 'name') == 'parent'
    glommer.register(PlainMapping, get=lambda target, key: 'parent-new')
    assert derived.glom(PlainMapping({'name': 'derived'}), 'name') == 'derived'


def test_parent_cache_cannot_bypass_new_override():
    glommer = _make_glommer()
    shouty = ShoutingMapping({'name': 'cached'})

    # warm the parent's (type, op) lookup cache before deriving
    parent_reg = glommer.scope[TargetRegistry]
    assert parent_reg.get_handler('get', shouty) is operator.getitem
    assert (type(shouty), 'get') in parent_reg._type_cache

    with glommer.register_context() as local:
        local_reg = local.scope[TargetRegistry]
        # the snapshot starts with an empty cache ...
        assert local_reg._type_cache == {}
        local.register(ShoutingMapping, get=ShoutingMapping.normalized_get)

        # ... so the override is resolved instead of a stale parent handler
        assert local.glom(shouty, 'name') == 'cached'
        assert local_reg.get_handler('get', shouty) is ShoutingMapping.normalized_get

    # parent cache and handler remain exactly as they were
    assert parent_reg.get_handler('get', shouty) is operator.getitem


def test_reregister_inside_context_refreshes_local_cache():
    glommer = _make_glommer()
    target = PlainMapping({'name': 'v'})

    with glommer.register_context() as local:
        local.register(PlainMapping, get=lambda t, k: 'first')
        assert local.glom(target, 'name') == 'first'

        local.register(PlainMapping, get=lambda t, k: 'second')
        assert local.glom(target, 'name') == 'second'

    assert glommer.glom(target, 'name') == 'v'


def test_type_inheritance_carried_into_snapshot():
    class DeepMapping(ShoutingMapping):
        pass

    glommer = _make_glommer()
    deep = DeepMapping({'name': 'deep'})

    with glommer.register_context() as local:
        local.register(ShoutingMapping, get=ShoutingMapping.normalized_get)
        local_reg = local.scope[TargetRegistry]

        # fuzzy type tree was copied, so the subclass resolves to nearest handler
        assert local_reg.get_handler('get', deep) is ShoutingMapping.normalized_get
        assert local.glom(deep, 'name') == 'deep'
        # and an unrelated mapping still falls back through the copied tree
        assert local_reg.get_handler('get', PlainMapping({})) is operator.getitem


def test_iterate_override_is_scoped():
    glommer = _make_glommer()
    target = PlainMapping({'a': 1, 'b': 2})

    # default mapping iteration yields keys
    assert glommer.glom(target, list) == ['a', 'b']

    with glommer.register_context() as local:
        local.register(BaseMapping, iterate=lambda mapping: iter(mapping._data.values()))
        assert local.glom(target, [int]) == [1, 2]

    assert glommer.glom(target, list) == ['a', 'b']


def test_nested_glom_and_lazy_iter_follow_creating_context():
    glommer = _make_glommer()
    target = ShoutingMapping({'name': 'nested', 'count': 3})

    with glommer.register_context() as local:
        local.register(ShoutingMapping, get=ShoutingMapping.normalized_get)
        local.register(ShoutingMapping,
                       iterate=lambda mapping: iter(mapping._data.values()))

        # nested specs (dict -> tuple -> list) all resolve via the local registry
        spec = {
            'label': ('name', str.upper),
            'values': [T],
        }
        result = local.glom(target, spec)
        assert result == {'label': 'NESTED', 'values': ['nested', 3]}

        # a lazy Iter created in the context captures that registration context
        lazy_iter = Iter().map(lambda value: value)

    # the with-block has ended, but evaluating through the derived glommer
    # still uses the iterate override present at creation time
    assert list(local.glom(target, lazy_iter)) == ['nested', 3]
    # the parent glommer evaluates with its own convention
    assert glommer.glom(PlainMapping({'a': 1}), list) == ['a']
    with pytest.raises(PathAccessError):
        glommer.glom(target, 'name')


def test_snapshot_does_not_share_mutable_registry_state():
    glommer = _make_glommer()
    derived = glommer.derive()

    parent_reg = glommer.scope[TargetRegistry]
    local_reg = derived.scope[TargetRegistry]

    assert local_reg is not parent_reg
    assert local_reg._op_type_map is not parent_reg._op_type_map
    assert local_reg._op_type_tree is not parent_reg._op_type_tree
    assert local_reg._op_auto_map is not parent_reg._op_auto_map

    # nested type-tree containers are independently owned as well
    for op, tree in parent_reg._op_type_tree.items():
        assert local_reg._op_type_tree[op] is not tree

    # the auto-discovery configuration is still carried over and usable
    class AutoMapped(BaseMapping):
        pass

    derived.register(AutoMapped)
    assert callable(local_reg.get_handler('iterate', AutoMapped({})))
    # an explicit registration on the derived registry never reaches the parent
    assert AutoMapped in local_reg.get_type_map('get')
    assert AutoMapped not in parent_reg.get_type_map('get')
    assert AutoMapped not in parent_reg.get_type_map('iterate')
