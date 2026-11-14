import pytest

from glom import (Coalesce,
                   Fill,
                   GlomError,
                   Glommer,
                   Invoke,
                   Match,
                   Path,
                   PathAccessError,
                   SKIP,
                   STOP,
                   S,
                   T,
                   Val,
                   glom,
                   INCOMPLETE as GLOM_INCOMPLETE)
from glom import trace
from glom.tracing import (INCOMPLETE,
                          LiteralSource,
                          PathSource,
                          SyntheticSource,
                          TraceResult)


TARGET = {
    'name': 'widget',
    'count': 3,
    'meta': {'tags': ['a', 'b', 'c'], 'rating': 4.5},
    'rows': [{'id': 1, 'vals': [10, 20]},
             {'id': 2, 'vals': [30]},
             {'id': 3}],
}


@pytest.mark.parametrize('spec,scope', [
    ('name', None),
    ('meta.rating', None),
    (T['meta']['tags'][1], None),
    (Path('rows', 0, 'id'), None),
    ({'ids': ('rows', ['id']), 'n': 'count'}, None),
    (('meta', 'tags', [lambda x: x.upper()]), None),
    (('rows', [('id',)]), None),
    (Coalesce('missing', 'name'), None),
    (Coalesce('missing', default=Val('d')), None),
    (Coalesce('missing', default_factory=lambda: 'df'), None),
    (Val('literal'), None),
    (Invoke(sum).specs(('rows', ['id'])), None),
    (Fill((T['name'], Val(7))), None),
    (T['count'] + 1, None),
    ({'scoped': S['greeting']}, {'greeting': 'hi'}),
    ({'l': ('rows', [lambda r: SKIP if r['id'] == 2 else r])}, None),
    ('rows.*.id', None),
])
def test_trace_value_matches_glom(spec, scope):
    expected = glom(TARGET, spec, scope=dict(scope or {}))
    result = trace(TARGET, spec, scope=dict(scope or {}))
    assert result.complete
    assert result.error is None
    assert result.value == expected


def test_path_source_records_concrete_input_path():
    result = trace(TARGET, {'name': 'name', 'deep': 'meta.rating'})
    assert isinstance(result[('name',)], PathSource)
    assert result[('name',)].input_path == Path('name')
    assert result[('deep',)].input_path == Path('meta', 'rating')
    assert result[('deep',)].value == 4.5
    # root passthrough points at the whole input, never a fabricated path
    root = trace(TARGET, T)
    assert isinstance(root[()], PathSource)
    assert root[()].input_path == Path()
    assert root[()].value == TARGET


def test_literal_source_for_val():
    result = trace(TARGET, {'fixed': Val(42)})
    source = result[('fixed',)]
    assert isinstance(source, LiteralSource)
    assert source.value == 42


def test_callable_is_opaque_synthetic():
    def compute(target):
        return target['meta']['rating'] * 2

    result = trace(TARGET, compute)
    source = result[()]
    assert isinstance(source, SyntheticSource)
    assert source.origin.startswith('callable')
    assert source.value == 9.0
    # the callable may only record a dependency on the target it was
    # handed; internal field accesses must not be guessed
    assert all(isinstance(dep, PathSource) and dep.input_path == Path()
               for dep in source.inputs)


def test_synthetic_records_known_t_expression_dependencies():
    result = trace(TARGET, T['meta']['rating'] + T['count'])
    source = result[()]
    assert isinstance(source, SyntheticSource)
    input_paths = [dep.input_path for dep in source.inputs
                   if isinstance(dep, PathSource)]
    assert Path('meta', 'rating') in input_paths
    assert Path('count') in input_paths
    assert source.value == 7.5


def test_nested_list_elements_are_traced_separately():
    spec = {'rows': ('rows', [
        {'id': 'id',
         'first_val': Coalesce(('vals', [lambda x: x]), default=Val([]))}])}
    result = trace(TARGET, spec)
    first = result[('rows', 0, 'id')]
    second = result[('rows', 1, 'id')]
    assert isinstance(first, PathSource)
    assert first.input_path == Path('rows', 0, 'id')
    assert second.input_path == Path('rows', 1, 'id')
    # nested list element under the first row
    val_source = result[('rows', 0, 'first_val', 0)]
    assert isinstance(val_source, SyntheticSource)
    assert val_source.origin.startswith('callable')
    assert any(dep.input_path == Path('rows', 0, 'vals', 0)
               for dep in val_source.inputs
               if isinstance(dep, PathSource))


def test_coalesce_only_records_adopted_branch():
    result = trace(TARGET, Coalesce('missing_a', 'missing_b', 'name'))
    source = result[()]
    assert isinstance(source, PathSource)
    assert source.input_path == Path('name')

    default_result = trace(TARGET, Coalesce('missing', default=Val('fallback')))
    assert isinstance(default_result[()], LiteralSource)
    assert default_result[()].value == 'fallback'

    factory_result = trace(
        TARGET, Coalesce('missing', default_factory=lambda: 'made'))
    factory_source = factory_result[()]
    assert isinstance(factory_source, SyntheticSource)
    assert factory_source.origin == 'Coalesce default_factory'


def test_skip_leaves_no_ghost_output_paths():
    result = trace(
        TARGET,
        {'keep': 'name', 'gone': lambda t: SKIP, 'also': Val(SKIP)})
    assert set(result.sources) == {(), ('keep',)}
    assert result.value == {'keep': 'widget'}

    list_result = trace([1, 2, 3, 4],
                        [lambda x: SKIP if x % 2 == 0 else x])
    assert list_result.value == [1, 3]
    leaf_paths = [path for path in list_result.sources if path != ()]
    assert leaf_paths == [(0,), (1,)]


def test_stop_truncates_list_without_ghosts():
    result = trace([1, 2, 3], [lambda x: STOP if x == 2 else x])
    assert result.value == [1]
    assert list(result.sources) == [(), (0,)]


def test_error_reports_confirmed_sources_and_incomplete_flag():
    spec = {'name': 'name',
            'bad': ('rows', [{'missing': ('nope',)}])}
    result = trace(TARGET, spec)
    assert not result.complete
    assert isinstance(result.error, PathAccessError)
    # the successfully built part of the output is retained
    assert result.value == {'name': 'widget'}
    # confirmed sources survive; the failed branch leaves no entries
    assert set(result.sources) == {(), ('name',)}
    with pytest.raises(PathAccessError):
        result.raise_on_error()


def test_root_level_error_has_no_partial_value():
    result = trace({}, 'a.b.c')
    assert not result.complete
    assert result.value is INCOMPLETE
    assert len(result) == 0
    assert isinstance(result.error, PathAccessError)


def test_failed_list_element_rolls_back_its_entries():
    result = trace([{'k': 1}, {'k': 2}, {}],
                   [{'k': 'k', 'bad': 'missing'}])
    assert not result.complete
    assert all(path[0] < 2 for path in result.sources if path)


def test_invoke_dependencies_are_recorded():
    result = trace(TARGET, Invoke(lambda a, b: a + b).specs('count', 'meta.rating'))
    source = result[()]
    assert isinstance(source, SyntheticSource)
    paths = [dep.input_path for dep in source.inputs
             if isinstance(dep, PathSource)]
    assert Path('count') in paths
    assert Path('meta', 'rating') in paths
    assert source.value == 7.5


def test_custom_glomit_is_opaque_but_keeps_nested_dependencies():
    class CustomSpec:
        def glomit(self, target, scope):
            return scope[glom](target, 'name') + '!'

    result = trace(TARGET, CustomSpec())
    source = result[()]
    assert isinstance(source, SyntheticSource)
    assert source.origin == 'CustomSpec spec'
    assert source.value == 'widget!'
    assert any(isinstance(dep, PathSource)
               and dep.input_path == Path('name')
               for dep in source.inputs)


def test_match_mode_runs_opaquely_with_correct_value():
    result = trace({'v': 5}, Match({'v': int}))
    assert result.complete
    assert result.value == {'v': 5}


def test_dynamic_dict_keys():
    result = trace({'field': 'dynamic', 'v': 7},
                   {T['field']: 'v'})
    assert result[('dynamic',)].input_path == Path('v')
    assert result.value == {'dynamic': 7}


def test_fill_container_sources():
    result = trace(TARGET, Fill((T['name'], Val(1))))
    assert result.complete
    assert result.value == ('widget', 1)


def test_glommer_trace_method():
    glommer = Glommer()
    result = glommer.trace(TARGET, 'name')
    assert result.value == 'widget'
    assert isinstance(result[()], PathSource)
    with pytest.raises(TypeError):
        glommer.trace(TARGET, 'name', unexpected_kwarg=1)


def test_trace_result_equality_and_lookup():
    first = trace(TARGET, {'name': 'name'})
    second = trace(TARGET, {'name': 'name'})
    assert first == second
    assert first.get_source('name') == first[('name',)]
    assert first.get_source('nonexistent') is None
    assert len(first) == len(list(first))


def test_incomplete_sentinel_is_exported_alias_stable():
    assert GLOM_INCOMPLETE is INCOMPLETE


def test_sources_only_cover_emitted_output_paths():
    result = trace(
        TARGET,
        {'name': 'name',
         'coalesced': Coalesce('missing', 'count'),
         'rows': ('rows', [lambda r: SKIP if r['id'] == 3 else {'id': 'id'}])})
    assert result.complete
    # every emitted source path corresponds to a real location in value
    for path, source in result:
        node = result.value
        for segment in path:
            node = node[segment]
        assert node == source.value
