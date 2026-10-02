require 'test_helper'

class PreviewPropertyDefaultsTest < Minitest::Test
  def test_previews_defaults_for_nested_objects_and_array_items
    schema = {
      'properties' => {
        'user' => {
          'properties' => {
            'name' => { 'default' => 'anonymous' },
            'roles' => { 'default' => ['member'] },
            'settings' => { 'default' => { 'theme' => 'light' } },
            'age' => { 'default' => 18 }
          }
        },
        'items' => {
          'items' => {
            'properties' => {
              'qty' => { 'default' => 1 }
            }
          }
        }
      }
    }
    data = { 'user' => {}, 'items' => [{}] }
    preview = JSONSchemer.schema(schema).preview_property_defaults(data, :output_format => 'basic')

    assert_equal(
      {
        'user' => {
          'name' => 'anonymous',
          'roles' => ['member'],
          'settings' => { 'theme' => 'light' },
          'age' => 18
        },
        'items' => [{ 'qty' => 1 }]
      },
      preview['data']
    )
    assert(preview['valid'])
    assert_equal([], preview['ambiguities'])
    assert(preview['output']['valid'])

    insertions = preview['insertions'].map { |insertion| [insertion.delete('pointer'), insertion] }.to_h
    assert_equal(%w[/user/age /user/name /user/roles /user/settings /items/0/qty].sort, insertions.keys.sort)
    assert_equal('anonymous', insertions.fetch('/user/name').fetch('value'))
    assert_equal(['member'], insertions.fetch('/user/roles').fetch('value'))
    assert_equal({ 'theme' => 'light' }, insertions.fetch('/user/settings').fetch('value'))
    assert_equal(18, insertions.fetch('/user/age').fetch('value'))
    assert_equal(1, insertions.fetch('/items/0/qty').fetch('value'))
    assert_equal(
      'json-schemer://schema#/properties/user/properties/name/default',
      insertions.fetch('/user/name').fetch('schema')
    )
    assert_equal(
      '/properties/user/properties/name/default',
      insertions.fetch('/user/name').fetch('schema_pointer')
    )
    assert_equal(
      '/properties/items/items/properties/qty/default',
      insertions.fetch('/items/0/qty').fetch('schema_pointer')
    )

    # original data is unchanged
    assert_equal({ 'user' => {}, 'items' => [{}] }, data)
  end

  def test_treats_nil_false_and_empty_array_values_as_existing
    schema = {
      'properties' => {
        'a' => { 'default' => 1 },
        'b' => { 'default' => 2 },
        'c' => { 'default' => 3 }
      }
    }
    data = { 'a' => nil, 'b' => false, 'c' => [] }
    preview = JSONSchemer.schema(schema).preview_property_defaults(data)

    assert_equal({ 'a' => nil, 'b' => false, 'c' => [] }, preview['data'])
    assert_equal([], preview['insertions'])
    assert_equal([], preview['ambiguities'])
    assert_equal({ 'a' => nil, 'b' => false, 'c' => [] }, data)
  end

  def test_previews_defaults_from_external_in_memory_schema_refs
    refs = {
      URI('json-schemer://example/remote') => {
        'properties' => {
          'remote' => { 'default' => { 'tags' => ['a'] } }
        }
      }
    }
    schemer = JSONSchemer.schema(
      { '$ref' => 'json-schemer://example/remote' },
      :ref_resolver => refs.to_proc
    )

    preview = schemer.preview_property_defaults({})
    assert_equal({ 'remote' => { 'tags' => ['a'] } }, preview['data'])
    insertion = preview['insertions'].first
    assert_equal('/remote', insertion.fetch('pointer'))
    assert_equal({ 'tags' => ['a'] }, insertion.fetch('value'))
    assert_equal('json-schemer://example/remote#/properties/remote/default', insertion.fetch('schema'))
    assert_equal('/properties/remote/default', insertion.fetch('schema_pointer'))

    # mutating the preview result does not leak into the schema or future previews
    preview['data']['remote']['tags'] << 'b'
    preview['insertions'].first['value']['tags'] << 'c'
    assert_equal({ 'tags' => ['a'] }, refs.fetch(URI('json-schemer://example/remote')).dig('properties', 'remote', 'default'))

    repreview = schemer.preview_property_defaults({})
    assert_equal({ 'remote' => { 'tags' => ['a'] } }, repreview['data'])
    assert_equal({ 'tags' => ['a'] }, repreview['insertions'].first.fetch('value'))
  end

  def test_does_not_share_array_and_hash_defaults_with_preview_data
    schema = {
      'properties' => {
        'list' => { 'default' => [1, 2] },
        'options' => { 'default' => { 'a' => 1 } }
      }
    }
    schemer = JSONSchemer.schema(schema)

    preview = schemer.preview_property_defaults({})
    preview['data']['list'] << 3
    preview['data']['options']['b'] = 2

    assert_equal([1, 2], schema.dig('properties', 'list', 'default'))
    assert_equal({ 'a' => 1 }, schema.dig('properties', 'options', 'default'))

    repreview = schemer.preview_property_defaults({})
    assert_equal({ 'list' => [1, 2], 'options' => { 'a' => 1 } }, repreview['data'])
  end

  def test_applies_conditional_subschema_defaults_using_valid_branch_rules
    c_schema = { 'const' => 3 }
    properties = {
      'a' => { 'const' => 1 },
      'b' => { 'const' => 2, 'default' => 2 },
      'c' => c_schema
    }
    subschema = { 'properties' => properties }
    subschema1 = subschema.merge('properties' => properties.merge('c' => c_schema.merge('default' => 1)))
    subschema2 = subschema.merge('properties' => properties.merge('c' => c_schema.merge('default' => 2)))
    subschema3 = subschema.merge('properties' => properties.merge('c' => c_schema.merge('default' => 3)))
    subschema4 = subschema.merge('properties' => properties.merge('c' => c_schema.merge('default' => 4)))
    schema = {
      'allOf' => [subschema1],
      'anyOf' => [subschema2],
      'oneOf' => [subschema3],
      'if' => subschema4
    }
    preview = JSONSchemer.schema(schema).preview_property_defaults({ 'a' => 1 })

    assert_equal({ 'a' => 1, 'b' => 2 }, preview['data'])
    assert_equal(['/b'], preview['insertions'].map { |insertion| insertion['pointer'] })
    ambiguity = preview['ambiguities'].first
    assert_equal('/c', ambiguity.fetch('pointer'))
    assert_equal([1, 2, 3, 4], ambiguity.fetch('candidates').map { |candidate| candidate['value'] }.sort)
  end

  def test_inserts_default_for_successful_one_of_branch
    schema = {
      'oneOf' => [
        {
          'type' => 'object',
          'properties' => {
            'foo' => { 'enum' => ['a'] },
            'bar' => { 'enum' => ['a'], 'default' => 'a' }
          }
        },
        {
          'type' => 'object',
          'properties' => {
            'foo' => { 'enum' => ['b'] },
            'bar' => { 'enum' => ['b'], 'default' => 'b' }
          }
        }
      ]
    }
    schemer = JSONSchemer.schema(schema)

    preview = schemer.preview_property_defaults({ 'foo' => 'a' })
    assert_equal({ 'foo' => 'a', 'bar' => 'a' }, preview['data'])
    assert_equal([], preview['ambiguities'])

    preview = schemer.preview_property_defaults({ 'foo' => 'b' })
    assert_equal({ 'foo' => 'b', 'bar' => 'b' }, preview['data'])
    assert_equal([], preview['ambiguities'])
  end

  def test_returns_ambiguities_for_conflicting_defaults_without_picking_one
    schema = {
      'allOf' => [
        { 'properties' => { 'a' => { 'default' => 1 } } },
        { 'properties' => { 'a' => { 'default' => 2 }, 'b' => { 'default' => 3 } } }
      ]
    }
    preview = JSONSchemer.schema(schema).preview_property_defaults({})

    assert_equal({ 'b' => 3 }, preview['data'])
    assert_equal(['/b'], preview['insertions'].map { |insertion| insertion['pointer'] })

    ambiguity = preview['ambiguities'].first
    assert_equal('/a', ambiguity.fetch('pointer'))
    candidates = ambiguity.fetch('candidates').sort_by { |candidate| candidate['value'] }
    assert_equal([1, 2], candidates.map { |candidate| candidate['value'] })
    assert_equal(
      'json-schemer://schema#/allOf/0/properties/a/default',
      candidates.first.fetch('schema')
    )
    assert_equal(
      '/allOf/1/properties/a/default',
      candidates.last.fetch('schema_pointer')
    )
  end

  def test_previews_symbol_key_insertion
    schemer = JSONSchemer.schema(
      { 'properties' => { 'a' => { 'default' => 'x' } } },
      :insert_property_defaults => :symbol
    )
    preview = schemer.preview_property_defaults({})
    assert_equal({ :a => 'x' }, preview['data'])
    assert_equal('/a', preview['insertions'].first.fetch('pointer'))
  end

  def test_includes_validation_result_of_merged_data
    schemer = JSONSchemer.schema(
      {
        'required' => ['missing'],
        'properties' => { 'a' => { 'default' => 1 } }
      }
    )
    preview = schemer.preview_property_defaults({}, :output_format => 'basic', :resolve_enumerators => true)

    assert_equal({ 'a' => 1 }, preview['data'])
    refute(preview['valid'])
    refute(preview['output']['valid'])
    assert_kind_of(Array, preview['output']['errors'])
  end
end
