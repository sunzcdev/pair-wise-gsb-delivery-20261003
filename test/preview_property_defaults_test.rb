require 'test_helper'

class PreviewPropertyDefaultsTest < Minitest::Test
  def test_it_previews_defaults_without_modifying_the_instance
    schema = {
      'properties' => {
        'a' => { 'default' => 1 },
        'b' => {
          'properties' => {
            'c' => { 'default' => 'x' }
          }
        }
      }
    }
    data = { 'b' => {} }
    preview = JSONSchemer.schema(schema).preview_property_defaults(data)
    assert_equal({ 'b' => {} }, data)
    assert_equal({ 'a' => 1, 'b' => { 'c' => 'x' } }, preview.fetch('instance'))
    assert_equal(
      [
        {
          'instance_location' => '/a',
          'value' => 1,
          'schema_location' => 'json-schemer://schema#/properties/a/default'
        },
        {
          'instance_location' => '/b/c',
          'value' => 'x',
          'schema_location' => 'json-schemer://schema#/properties/b/properties/c/default'
        }
      ],
      preview.fetch('insertions')
    )
    assert_empty(preview.fetch('ambiguous'))
    assert(preview.fetch('valid'))
    assert_empty(preview.fetch('errors'))
  end

  def test_it_treats_existing_values_as_present
    schema = {
      'properties' => {
        'a' => { 'default' => 1 },
        'b' => { 'default' => 2 },
        'c' => { 'default' => 3 }
      }
    }
    data = { 'a' => nil, 'b' => false, 'c' => [] }
    preview = JSONSchemer.schema(schema).preview_property_defaults(data)
    assert_equal({ 'a' => nil, 'b' => false, 'c' => [] }, preview.fetch('instance'))
    assert_empty(preview.fetch('insertions'))
    assert_empty(preview.fetch('ambiguous'))
  end

  def test_it_previews_defaults_for_array_members
    schema = {
      'properties' => {
        'list' => {
          'items' => {
            'properties' => {
              'x' => { 'default' => 1 }
            }
          }
        }
      }
    }
    data = { 'list' => [{}, { 'x' => 10 }] }
    preview = JSONSchemer.schema(schema).preview_property_defaults(data)
    assert_equal({ 'list' => [{}, { 'x' => 10 }] }, data)
    assert_equal({ 'list' => [{ 'x' => 1 }, { 'x' => 10 }] }, preview.fetch('instance'))
    assert_equal(
      [
        {
          'instance_location' => '/list/0/x',
          'value' => 1,
          'schema_location' => 'json-schemer://schema#/properties/list/items/properties/x/default'
        }
      ],
      preview.fetch('insertions')
    )
  end

  def test_it_previews_defaults_from_an_external_in_memory_schema
    external = {
      '$id' => 'https://example.com/external.json',
      'default' => { 'b' => 1 },
      'properties' => {
        'b' => { 'type' => 'integer' }
      }
    }
    schema = {
      '$id' => 'https://example.com/root.json',
      'properties' => {
        'a' => { '$ref' => 'external.json' }
      }
    }
    schemer = JSONSchemer.schema(
      schema,
      :ref_resolver => proc do |uri|
        external if uri.to_s == 'https://example.com/external.json'
      end
    )
    preview = schemer.preview_property_defaults({})
    assert_equal({ 'a' => { 'b' => 1 } }, preview.fetch('instance'))
    assert_equal(
      [
        {
          'instance_location' => '/a',
          'value' => { 'b' => 1 },
          'schema_location' => 'https://example.com/external.json#/default'
        }
      ],
      preview.fetch('insertions')
    )
    assert(preview.fetch('valid'))
  end

  def test_it_previews_nested_defaults_from_an_external_in_memory_schema
    external = {
      '$id' => 'https://example.com/external.json',
      'properties' => {
        'b' => { 'default' => 1 }
      }
    }
    schema = {
      '$id' => 'https://example.com/root.json',
      'properties' => {
        'a' => { '$ref' => 'external.json' }
      }
    }
    schemer = JSONSchemer.schema(
      schema,
      :ref_resolver => proc do |uri|
        external if uri.to_s == 'https://example.com/external.json'
      end
    )
    preview = schemer.preview_property_defaults({ 'a' => {} })
    assert_equal({ 'a' => { 'b' => 1 } }, preview.fetch('instance'))
    assert_equal(
      [
        {
          'instance_location' => '/a/b',
          'value' => 1,
          'schema_location' => 'https://example.com/external.json#/properties/b/default'
        }
      ],
      preview.fetch('insertions')
    )
  end

  def test_it_inserts_unique_defaults_from_valid_conditional_branches
    schema = {
      'oneOf' => [
        {
          'properties' => {
            'a' => { 'const' => 1 },
            'b' => { 'default' => 'x' }
          }
        },
        {
          'properties' => {
            'a' => { 'const' => 2 },
            'b' => { 'default' => 'y' }
          }
        }
      ]
    }
    preview = JSONSchemer.schema(schema).preview_property_defaults({ 'a' => 1 })
    assert_equal({ 'a' => 1, 'b' => 'x' }, preview.fetch('instance'))
    assert_equal(
      [
        {
          'instance_location' => '/b',
          'value' => 'x',
          'schema_location' => 'json-schemer://schema#/oneOf/0/properties/b/default'
        }
      ],
      preview.fetch('insertions')
    )
    assert_empty(preview.fetch('ambiguous'))
  end

  def test_it_reports_ambiguous_defaults_without_picking_one
    schema = {
      'anyOf' => [
        {
          'properties' => {
            'a' => { 'const' => 1 },
            'b' => { 'default' => 'x' }
          }
        },
        {
          'properties' => {
            'a' => { 'const' => 1 },
            'b' => { 'default' => 'y' }
          }
        }
      ]
    }
    data = { 'a' => 1 }
    preview = JSONSchemer.schema(schema).preview_property_defaults(data)
    assert_equal({ 'a' => 1 }, preview.fetch('instance'))
    assert_empty(preview.fetch('insertions'))
    assert_equal(
      [
        {
          'instance_location' => '/b',
          'candidates' => [
            {
              'value' => 'y',
              'schema_location' => 'json-schemer://schema#/anyOf/1/properties/b/default'
            },
            {
              'value' => 'x',
              'schema_location' => 'json-schemer://schema#/anyOf/0/properties/b/default'
            }
          ]
        }
      ],
      preview.fetch('ambiguous')
    )
  end

  def test_it_does_not_share_default_values_with_the_schema_or_other_previews
    schema = {
      'properties' => {
        'a' => { 'default' => { 'x' => [1, 2], 'y' => 'str' } }
      }
    }
    schemer = JSONSchemer.schema(schema)
    first = schemer.preview_property_defaults({})
    assert_equal({ 'a' => { 'x' => [1, 2], 'y' => 'str' } }, first.fetch('instance'))

    first.fetch('instance').fetch('a').fetch('x') << 3
    first.fetch('instance').fetch('a').fetch('y') << ' mutated'
    first.fetch('insertions').first.fetch('value').fetch('x') << 4

    assert_equal({ 'x' => [1, 2], 'y' => 'str' }, schema.dig('properties', 'a', 'default'))

    second = schemer.preview_property_defaults({})
    assert_equal({ 'a' => { 'x' => [1, 2], 'y' => 'str' } }, second.fetch('instance'))
    assert_equal({ 'x' => [1, 2], 'y' => 'str' }, second.fetch('insertions').first.fetch('value'))
  end

  def test_it_includes_the_validation_result_of_the_filled_instance
    schema = {
      'required' => ['a'],
      'properties' => {
        'a' => { 'default' => 1 },
        'b' => { 'type' => 'integer' }
      }
    }
    schemer = JSONSchemer.schema(schema)

    preview = schemer.preview_property_defaults({})
    assert(preview.fetch('valid'))
    assert_empty(preview.fetch('errors'))

    preview = schemer.preview_property_defaults({ 'b' => 'nope' })
    refute(preview.fetch('valid'))
    assert_equal(1, preview.fetch('errors').size)
    assert_equal({ 'a' => 1, 'b' => 'nope' }, preview.fetch('instance'))
  end

  def test_it_previews_symbol_keys_when_insert_property_defaults_is_symbol
    schemer = JSONSchemer.schema(
      {
        'properties' => {
          'a' => { 'default' => 'x' }
        }
      },
      :insert_property_defaults => :symbol
    )
    preview = schemer.preview_property_defaults({})
    assert_equal({ :a => 'x' }, preview.fetch('instance'))
    assert_equal('/a', preview.fetch('insertions').first.fetch('instance_location'))
  end

  def test_it_does_not_change_insert_property_defaults_behavior
    schema = {
      'properties' => {
        'a' => { 'default' => 1 }
      }
    }
    data = {}
    assert(JSONSchemer.schema(schema, :insert_property_defaults => true).valid?(data))
    assert_equal({ 'a' => 1 }, data)
  end
end
