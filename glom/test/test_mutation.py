import pytest

from glom import glom, Path, T, S, Spec, Glommer, PathAssignError, PathAccessError
from glom import assign, Assign, delete, Delete, PathDeleteError, Or, MissingFactory
from glom import core
from glom.core import UnregisteredTarget


def test_assign():
    class Foo:
        pass

    assert glom({}, Assign(T['a'], 1)) == {'a': 1}
    assert glom({'a': {}}, Assign(T['a']['a'], 1)) == {'a': {'a': 1}}
    assert glom({'a': {}}, Assign('a.a', 1)) == {'a': {'a': 1}}
    assert glom(Foo(), Assign(T.a, 1)).a == 1
    assert glom({}, Assign('a', 1)) == {'a': 1}
    assert glom(Foo(), Assign('a', 1)).a == 1
    assert glom({'a': Foo()}, Assign('a.a', 1))['a'].a == 1
    def r():
        r = {}
        r['r'] = r
        return r
    assert glom(r(), Assign('r.r.r.r.r.r.r.r.r', 1)) == {'r': 1}
    assert glom(r(), Assign(T['r']['r']['r']['r'], 1)) == {'r': 1}
    assert glom(r(), Assign(Path('r', 'r', T['r']), 1)) == {'r': 1}
    assert assign(r(), Path('r', 'r', T['r']), 1) == {'r': 1}
    with pytest.raises(TypeError, match='path argument must be'):
        Assign(1, 'a')
    with pytest.raises(ValueError, match='path must have at least one element'):
        Assign(T, 1)

    assert repr(Assign(T.a, 1)) == 'Assign(T.a, 1)'
    assign_spec = Assign(T.a, 1, missing=dict)
    assert repr(assign_spec) == "Assign(T.a, 1, missing=dict)"
    assert repr(assign_spec) == repr(eval(repr(assign_spec)))


def test_assign_recursive():
    val = {}
    val[1] = [val]
    recur_out = glom({}, Assign(T['a'], val))['a']
    assert recur_out[1][0] is recur_out


def test_assign_spec_val():
    output = glom({'b': 'c'}, Assign('a', Spec('b')))
    assert output['a'] == output['b'] == 'c'
    output = glom({'b': 'c'}, Assign('a', Or('d', 'b')))
    assert output['a'] == output['b'] == 'c'


def test_unregistered_assign():
    # test with bare target registry
    glommer = Glommer(register_default_types=False)

    with pytest.raises(UnregisteredTarget, match='assign'):
        glommer.glom({}, Assign('a', 'b'))

    # test for unassignable tuple
    with pytest.raises(UnregisteredTarget, match='assign'):
        glom({'a': ()}, Assign('a.0', 'b'))


def test_bad_assign_target():
    class BadTarget:
        def __setattr__(self, name, val):
            raise Exception("and you trusted me?")

    # sanity check
    spec = Assign('a', 'b')
    ok_target = lambda: None
    glom(ok_target, spec)
    assert ok_target.a == 'b'

    with pytest.raises(PathAssignError, match='could not assign'):
        glom(BadTarget(), spec)

    with pytest.raises(PathAccessError, match='could not access'):
        assign({}, 'a.b.c', 'moot')
    return


def test_sequence_assign():
    target = {'alist': [0, 1, 2]}
    assign(target, 'alist.2', 3)
    assert target['alist'][2] == 3

    with pytest.raises(PathAssignError, match='could not assign') as exc_info:
        assign(target, 'alist.3', 4)

    # the following test is because pypy's IndexError is different than CPython's:
    # E         - PathAssignError(IndexError('list index out of range',), Path('alist'), '3')
    # E         + PathAssignError(IndexError('list assignment index out of range',), Path('alist'), '3')
    # E         ?                                  +++++++++++

    exc_repr = repr(exc_info.value)
    assert exc_repr.startswith('PathAssignError(')
    assert exc_repr.endswith("'3')")
    return


def test_invalid_assign_op_target():
    target = {'afunc': lambda x: 'hi %s' % x}
    spec = T['afunc'](x=1)

    with pytest.raises(ValueError):
        assign(target, spec, None)
    return


def test_assign_missing_signature():
    # test signature (non-callable missing hook)
    with pytest.raises(TypeError, match='callable'):
        assign({}, 'a.b.c', 'lol', missing='invalidbcnotcallable')
    return


def test_assign_missing_dict():
    target = {}
    val = object()

    from itertools import count
    counter = count()
    def debugdict():
        ret = dict()
        #ret['id'] = id(ret)
        #ret['inc'] = counter.next()
        return ret

    assign(target, 'a.b.c.d', val, missing=debugdict)

    assert target == {'a': {'b': {'c': {'d': val}}}}


def test_assign_missing_object():
    val = object()
    class Container:
        pass

    target = Container()
    target.a = extant_a = Container()
    assign(target, 'a.b.c.d', val, missing=Container)

    assert target.a.b.c.d is val
    assert target.a is extant_a  # make sure we didn't overwrite anything on the path


def test_assign_missing_with_extant_keys():
    """This test ensures that assign with missing doesn't overwrite
    perfectly fine extant keys that are along the path it needs to
    assign to. call count is also checked to make sure missing() isn't
    invoked too many times.

    """
    target = {}
    value = object()
    default_struct = {'b': {'c': {}}}

    call_count = [0]

    def _get_default_struct():
        call_count[0] += 1  # make sure this is only called once
        return default_struct

    assign(target, 'a.b.c', value, missing=_get_default_struct)

    assert target['a']['b']['c'] is value
    assert target['a']['b'] is default_struct['b']
    assert call_count == [1]


def test_assign_missing_unassignable():
    """Check that the final assignment to the target object comes last,
    ensuring that failed assignments don't leave targets in a bad
    state.

    """

    class Tarjay:
        init_count = 0
        def __init__(self):
            self.__class__.init_count += 1

        @property
        def unassignable(self):
            return

    value = object()
    target = {"preexisting": "ok"}

    with pytest.raises(PathAssignError):
        assign(target, 'tarjay.unassignable.a.b.c', value, missing=Tarjay)

    assert target == {'preexisting': 'ok'}

    # why 3? "c" gets the value of "value", while "b", "a", and
    # "tarjay" all succeed and are set to Tarjay instances. Then
    # unassignable is already present, but not possible to assign to,
    # raising the PathAssignError.
    assert Tarjay.init_count == 3


def test_missing_factory_context_args():
    """The context factory receives the remaining path, the parent node,
    and the segment about to be written, once per genuinely missing
    segment."""
    calls = []

    def factory(remaining_path, parent, path_segment):
        calls.append((remaining_path, parent, path_segment))
        return {}

    target = {}
    assign(target, 'a.b.c', 'value', missing=MissingFactory(factory))

    assert target == {'a': {'b': {'c': 'value'}}}
    # only 'a' and 'b' are missing intermediate nodes; 'c' is the write
    assert len(calls) == 2
    first_path, first_parent, first_segment = calls[0]
    assert first_segment == 'a'
    assert first_parent is target
    assert first_path == Path('a', 'b', 'c')
    second_path, second_parent, second_segment = calls[1]
    assert second_segment == 'b'
    assert second_path == Path('b', 'c')
    assert isinstance(second_parent, dict)


def test_missing_factory_legacy_zero_arg():
    """A plain zero-argument missing callable keeps working unchanged,
    with and without the MissingFactory wrapper distinction."""
    target = {}
    assign(target, 'a.b.c', 'v', missing=dict)
    assert target == {'a': {'b': {'c': 'v'}}}

    # non-callable missing is still rejected
    with pytest.raises(TypeError, match='callable'):
        MissingFactory('not callable')


def test_missing_factory_repr():
    def factory(remaining_path, parent, path_segment):
        return {}

    spec = Assign('a.b', 1, missing=MissingFactory(factory))
    rendered = repr(spec)
    assert rendered.startswith('Assign(')
    assert 'MissingFactory(' in rendered
    # a builtin-backed factory round-trips through repr like other specs
    builtin_spec = Assign('a.b', 1, missing=MissingFactory(dict))
    assert repr(eval(repr(builtin_spec))) == repr(builtin_spec)
    assert repr(MissingFactory(dict)) == 'MissingFactory(dict)'


def test_extant_none_empty_not_missing():
    """Existing None, [], and {} are real nodes, never missing ones."""
    def make_counter():
        calls = [0]

        def factory(remaining_path, parent, path_segment):
            calls[0] += 1
            return {}
        return calls, factory

    # empty dict is kept and written into; no factory invocation
    calls, factory = make_counter()
    target = {'a': {}}
    assign(target, 'a.b', 1, missing=MissingFactory(factory))
    assert target == {'a': {'b': 1}}
    assert calls == [0]

    # an existing empty list is a real node: an out-of-range index is a
    # genuine miss, but the factory result is validated against the list
    # and rejected rather than silently replacing the list
    calls, factory = make_counter()
    target = {'a': []}
    with pytest.raises(PathAssignError):
        assign(target, 'a.0.b', 1, missing=MissingFactory(factory))
    assert target == {'a': []}

    # None is an extant (but unwritable) node: it raises, untouched, and
    # the factory is never called because None itself is not "missing"
    calls, factory = make_counter()
    target = {'a': None}
    with pytest.raises(PathAssignError):
        assign(target, 'a.b', 1, missing=MissingFactory(factory))
    assert target == {'a': None}
    assert calls == [0]


def test_missing_factory_bad_result_no_half_chain():
    """A factory returning an object that cannot hold the next step
    raises with the original path and leaves the document clean."""
    target = {'preexisting': 'ok'}
    with pytest.raises(PathAssignError) as exc_info:
        assign(target, 'a.b.c', 'value',
               missing=MissingFactory(lambda remaining_path, parent, seg: 42))
    # the error references the original assignment path
    assert exc_info.value.path == Path('a', 'b')
    assert target == {'preexisting': 'ok'}


def test_missing_factory_parallel_paths_share_parent():
    """Two assignment paths in one glom() call share the parents they
    create, rather than each rebuilding the chain."""
    target = {}
    spec = (Assign('shared.a', 1, missing=dict),
            Assign('shared.b', 2, missing=dict))
    glom(target, spec)
    assert target == {'shared': {'a': 1, 'b': 2}}


def test_missing_factory_no_cross_document_cache():
    """A single Assign spec reused across documents must never hand one
    document a container created for another."""
    spec = Assign('a.b', 'value', missing=dict)
    first, second = {}, {}
    glom(first, spec)
    glom(second, spec)
    assert first == second == {'a': {'b': 'value'}}
    assert first['a'] is not second['a']


def test_missing_factory_context_drives_container_type():
    """The factory can use the parent/segment context to choose types."""
    def factory(remaining_path, parent, path_segment):
        # integer index writes need a sequence parent
        return [] if isinstance(path_segment, int) else {}

    target = {}
    assign(target, Path('a', 'b'), 'value', missing=MissingFactory(factory))
    assert target == {'a': {'b': 'value'}}


def test_missing_factory_s_path():
    """Backfill also works for scope-based (S) assignment paths."""
    result = glom(
        {},
        (Assign(S['scoped']['nested'], 'value', missing=MissingFactory(lambda r, p, s: {})),
         S['scoped']))
    assert result == {'nested': 'value'}


def test_s_assign():
    '''
    check that assign works when storing things into S
    '''
    assert glom({}, (Assign(S['foo'], 'bar'), S['foo'])) == 'bar'


def test_delete():
    class Foo:
        def __init__(self, d=None):
            for k, v in d.items():
                setattr(self, k, v)

    assert glom({'a': 1}, Delete(T['a'])) == {}
    assert glom({'a': {'a': 1}}, Delete(T['a']['a'])) == {'a': {}}
    assert glom({'a': {'a': 1}}, Delete('a.a')) == {'a': {}}
    assert not hasattr(glom(Foo({'a': 1}), Delete(T.a)), 'a')
    assert glom({'a': 1}, Delete('a')) == {}
    assert not hasattr(glom(Foo({'a': 1}), Delete('a')), 'a')
    assert not hasattr(glom({'a': Foo({'a': 1})}, Delete('a.a'))['a'], 'a')

    def r():
        r = {}
        r['r'] = r
        return r

    assert glom(r(), Delete('r.r.r.r.r.r.r.r.r')) == {}
    assert glom(r(), Delete(T['r']['r']['r']['r'])) == {}
    assert glom(r(), Delete(Path('r', 'r', T['r']))) == {}
    assert delete(r(), Path('r', 'r', T['r'])) == {}
    with pytest.raises(TypeError, match='path argument must be'):
        Delete(1, 'a')
    with pytest.raises(ValueError, match='path must have at least one element'):
        Delete(T, 1)

    assert repr(Delete(T.a)) == 'Delete(T.a)'

    # test delete from scope
    assert glom(1, (S(x=T), S['x'])) == 1
    with pytest.raises(PathAccessError):
        glom(1, (S(x=T), Delete(S['x']), S['x']))

    # test raising on missing parent
    with pytest.raises(PathAccessError):
        glom({}, Delete(T['a']['b']))

    # test raising on missing index
    with pytest.raises(PathDeleteError):
        glom([], Delete(T[0]))
    target = []
    assert glom(target, Delete(T[0], ignore_missing=True)) is target

    # test raising on missing attr
    with pytest.raises(PathDeleteError):
        glom(object(), Delete(T.does_not_exist))
    target = object()
    assert glom(target, Delete(T.does_not_exist, ignore_missing=True)) is target


def test_unregistered_delete():
    glommer = Glommer(register_default_types=False)

    with pytest.raises(UnregisteredTarget, match='delete'):
        glommer.glom({'a': 1}, Delete('a'))

    with pytest.raises(UnregisteredTarget, match='delete'):
        glom({'a': (1,)}, Delete('a.0'))


def test_bad_delete_target():
    class BadTarget:
        def __delattr__(self, name):
            raise Exception("and you trusted me?")

    spec = Delete('a')
    ok_target = lambda: None
    ok_target.a = 1
    glom(ok_target, spec)
    assert not hasattr(ok_target, 'a')

    with pytest.raises(PathDeleteError, match='could not delete'):
        glom(BadTarget(), spec)

    with pytest.raises(PathDeleteError, match='could not delete'):
        delete({}, 'a')
    return


def test_sequence_delete():
    target = {'alist': [0, 1, 2]}
    delete(target, 'alist.1')
    assert target['alist'] == [0, 2]

    with pytest.raises(PathDeleteError, match='could not delete') as exc_info:
        delete(target, 'alist.2')

    exc_repr = repr(exc_info.value)
    assert exc_repr.startswith('PathDeleteError(')
    assert exc_repr.endswith("'2')")
    return


def test_invalid_delete_op_target():
    target = {'afunc': lambda x: 'hi %s' % x}
    spec = T['afunc'](x=1)

    with pytest.raises(ValueError):
        delete(target, spec, None)
    return


def test_delete_ignore_missing():
    assert delete({}, 'a', ignore_missing=True) == {}
    assert delete({}, 'a.b', ignore_missing=True) == {}


def test_star_broadcast():
    val = {'a': [{'b': [{'c': 1}, {'c': 2}, {'c': 3}]}]}
    assert glom(val, (Assign('a.*.b.*.d', 'a'), 'a.*.b.*.d')) == [['a', 'a', 'a']]
    glom(val, Delete('a.*.b.*.d'))
    assert 'c' in val['a'][0]['b'][0]
    assert 'd' not in val['a'][0]['b'][0]