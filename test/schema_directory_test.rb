require 'test_helper'

class SchemaDirectoryTest < Minitest::Test
  # Three in-memory documents demonstrating forward references (root.json
  # refers to node.json, which is registered later in the same batch),
  # relative base URIs (nested/leaf.json refers back to ../node.json) and
  # circular references (root.json <-> node.json).
  def documents
    {
      'http://example.com/schemas/root.json' => {
        '$id' => 'http://example.com/schemas/root.json',
        'type' => 'object',
        'properties' => {
          'name' => { '$ref' => 'node.json#/definitions/name' },
          'child' => { '$ref' => 'node.json' }
        }
      },
      'http://example.com/schemas/node.json' => {
        '$id' => 'http://example.com/schemas/node.json',
        'definitions' => {
          'name' => { 'type' => 'string' }
        },
        'properties' => {
          'parent' => { '$ref' => 'root.json' },
          'leaf' => { '$ref' => 'nested/leaf.json#leaf-anchor' }
        }
      },
      'http://example.com/schemas/nested/leaf.json' => {
        '$id' => 'http://example.com/schemas/nested/leaf.json',
        '$anchor' => 'leaf-anchor',
        '$dynamicAnchor' => 'leaf-dynamic',
        'properties' => {
          'parent' => { '$ref' => '../node.json#/definitions/name' }
        }
      }
    }
  end

  def directory
    @directory ||= JSONSchemer::SchemaDirectory.new(documents)
  end

  def test_registers_and_fetches_documents_by_uri
    assert(directory.key?('http://example.com/schemas/root.json'))
    assert_equal(3, directory.size)
    assert_equal(
      documents['http://example.com/schemas/node.json'],
      directory.fetch(URI('http://example.com/schemas/node.json'))
    )
  end

  def test_normalizes_uris
    assert(directory.key?('HTTP://EXAMPLE.COM/schemas/root.json'))
    assert(directory.key?('http://example.com/schemas/root.json#'))
    assert_equal(directory.fetch('http://example.com/schemas/root.json'), directory.fetch('HTTP://EXAMPLE.COM/schemas/root.json#fragment'))
  end

  def test_raises_unknown_ref_for_missing_uri
    assert_raises(JSONSchemer::UnknownRef) { directory.call('http://example.com/schemas/missing.json') }
  end

  def test_indexes_nested_ids_and_anchors
    nested = {
      '$id' => 'http://example.com/schemas/nested.json',
      'definitions' => {
        'embedded' => {
          '$id' => 'embedded.json',
          '$anchor' => 'embedded-anchor',
          'type' => 'integer'
        }
      }
    }
    directory = JSONSchemer::SchemaDirectory.new('http://example.com/schemas/nested.json' => nested)
    assert(directory.key?('http://example.com/schemas/embedded.json'))
    assert_equal(nested['definitions']['embedded'], directory.fetch('http://example.com/schemas/embedded.json'))
  end

  def test_compiles_and_validates_through_forward_relative_and_circular_refs
    schemer = JSONSchemer.schema({ '$ref' => 'http://example.com/schemas/root.json' }, :ref_resolver => directory)

    valid_instance = {
      'name' => 'root-name',
      'child' => {
        'parent' => { 'name' => 'circular-back-to-root' },
        'leaf' => { 'parent' => 'relative-base-back-to-node' }
      }
    }
    assert(schemer.valid?(valid_instance))
    refute(schemer.valid?({ 'name' => 1 }))
    refute(schemer.valid?({ 'child' => { 'leaf' => { 'parent' => 1 } } }))
  end

  def test_dynamic_refs_keep_runtime_scope_semantics
    tree = {
      '$id' => 'http://example.com/dynamic/tree.json',
      '$dynamicAnchor' => 'node',
      'type' => 'object',
      'properties' => {
        'data' => true,
        'children' => { 'type' => 'array', 'items' => { '$dynamicRef' => '#node' } }
      }
    }
    strict_tree = {
      '$id' => 'http://example.com/dynamic/strict-tree.json',
      '$dynamicAnchor' => 'node',
      '$ref' => 'tree.json',
      'unevaluatedProperties' => false
    }
    directory = JSONSchemer::SchemaDirectory.new(
      'http://example.com/dynamic/tree.json' => tree,
      'http://example.com/dynamic/strict-tree.json' => strict_tree
    )
    schemer = JSONSchemer.schema({ '$ref' => 'http://example.com/dynamic/strict-tree.json' }, :ref_resolver => directory)

    # `$dynamicRef` must resolve to strict-tree's `node` anchor at runtime,
    # so the misspelled property is rejected by `unevaluatedProperties`.
    refute(schemer.valid?({ 'children' => [{ 'daat' => 1 }] }))
    assert(schemer.valid?({ 'children' => [{ 'data' => 1 }] }))
  end

  def test_failed_registration_rolls_back_and_keeps_directory_usable
    before = directory.resource_uris

    error = assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      directory.register(
        'http://example.com/schemas/broken.json' => {
          '$id' => 'http://example.com/schemas/broken.json',
          '$ref' => 'http://example.com/schemas/missing.json'
        }
      )
    end
    assert_includes(error.message, 'http://example.com/schemas/missing.json')
    assert_includes(error.message, 'http://example.com/schemas/broken.json')

    assert_equal(before, directory.resource_uris)
    refute(directory.key?('http://example.com/schemas/broken.json'))
    schemer = JSONSchemer.schema({ '$ref' => 'http://example.com/schemas/root.json' }, :ref_resolver => directory)
    assert(schemer.valid?({ 'name' => 'still-usable' }))
  end

  def test_rejects_unresolvable_anchor_and_pointer
    assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      JSONSchemer::SchemaDirectory.new(
        'http://example.com/a.json' => { '$ref' => 'b.json#nope' },
        'http://example.com/b.json' => { '$anchor' => 'yes' }
      )
    end
    assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      JSONSchemer::SchemaDirectory.new(
        'http://example.com/a.json' => { '$ref' => 'b.json#/definitions/nope' },
        'http://example.com/b.json' => { 'definitions' => {} }
      )
    end
  end

  def test_rejects_duplicate_resource_uri_and_mentions_both_sources
    error = assert_raises(JSONSchemer::SchemaDirectory::DuplicateResourceError) do
      directory.register(
        'http://example.com/schemas/root.json' => { '$id' => 'http://example.com/schemas/root.json' }
      )
    end
    assert_includes(error.message, 'http://example.com/schemas/root.json')
    assert_includes(error.message, 'conflicts with the resource already provided')

    error = assert_raises(JSONSchemer::SchemaDirectory::DuplicateResourceError) do
      JSONSchemer::SchemaDirectory.new(
        'http://example.com/one.json' => { '$id' => 'http://example.com/dup.json' },
        'http://example.com/two.json' => { '$id' => 'http://example.com/dup.json' }
      )
    end
    assert_includes(error.message, 'http://example.com/one.json')
    assert_includes(error.message, 'http://example.com/two.json')
  end

  def test_rejects_duplicate_anchor_within_one_resource
    error = assert_raises(JSONSchemer::SchemaDirectory::DuplicateAnchorError) do
      JSONSchemer::SchemaDirectory.new(
        'http://example.com/a.json' => {
          '$id' => 'http://example.com/a.json',
          'definitions' => {
            'one' => { '$anchor' => 'dup' },
            'two' => { '$anchor' => 'dup' }
          }
        }
      )
    end
    assert_includes(error.message, 'dup')
    assert_includes(error.message, '/definitions/one')
    assert_includes(error.message, '/definitions/two')
  end

  def test_registration_snapshots_documents
    docs = documents
    directory = JSONSchemer::SchemaDirectory.new(docs)

    docs['http://example.com/schemas/node.json']['definitions']['name']['type'] = 'integer'
    docs['http://example.com/schemas/root.json']['properties']['name'] = { '$ref' => 'missing.json' }
    docs.clear

    schemer = JSONSchemer.schema({ '$ref' => 'http://example.com/schemas/root.json' }, :ref_resolver => directory)
    assert(schemer.valid?({ 'name' => 'still-a-string' }))
    refute(schemer.valid?({ 'name' => 1 }))
  end

  def test_directories_are_independent
    other = JSONSchemer::SchemaDirectory.new
    refute(other.key?('http://example.com/schemas/root.json'))
    other.register('http://example.com/other.json' => { 'type' => 'string' })
    refute(directory.key?('http://example.com/other.json'))
  end

  def test_registers_array_of_documents_with_root_ids
    directory = JSONSchemer::SchemaDirectory.new([
      { '$id' => 'http://example.com/array/one.json', 'type' => 'string' },
      { '$id' => 'http://example.com/array/two.json', '$ref' => 'one.json' }
    ])
    assert(directory.key?('http://example.com/array/one.json'))
    assert_equal(
      { '$id' => 'http://example.com/array/one.json', 'type' => 'string' },
      directory.fetch('http://example.com/array/one.json')
    )
  end

  def test_requires_uri_for_documents_without_root_id
    error = assert_raises(JSONSchemer::SchemaDirectory::Error) do
      JSONSchemer::SchemaDirectory.new([{ 'type' => 'string' }])
    end
    assert_includes(error.message, '$id')
  end

  def test_rejects_non_schema_documents
    error = assert_raises(JSONSchemer::SchemaDirectory::Error) do
      JSONSchemer::SchemaDirectory.new('http://example.com/nope.json' => 42)
    end
    assert_includes(error.message, 'http://example.com/nope.json')
  end

  def test_registers_boolean_documents
    directory = JSONSchemer::SchemaDirectory.new(
      'http://example.com/always.json' => true,
      'http://example.com/never.json' => false
    )
    assert_equal(true, directory.fetch('http://example.com/always.json'))
    assert_equal(false, directory.fetch('http://example.com/never.json'))
  end

  def test_id_with_plain_name_fragment_registers_anchor
    directory = JSONSchemer::SchemaDirectory.new(
      'http://example.com/draft4.json' => {
        'definitions' => {
          'foo' => { '$id' => '#foo', 'type' => 'string' },
          'embedded' => { '$id' => 'other.json#', 'type' => 'integer' },
          'pointer' => { '$id' => '#/definitions/foo' }
        },
        'properties' => {
          'a' => { '$ref' => '#foo' }
        }
      }
    )
    assert(directory.key?('http://example.com/other.json'))
    schemer = JSONSchemer.schema({ '$ref' => 'http://example.com/draft4.json#foo' }, :ref_resolver => directory)
    assert(schemer.valid?('ok'))
    refute(schemer.valid?(1))
  end

  def test_refs_inside_arrays_and_pointers_into_arrays
    directory = JSONSchemer::SchemaDirectory.new(
      'http://example.com/arrays.json' => {
        'prefixItems' => [{ 'type' => 'string' }],
        'allOf' => [{ '$ref' => '#/prefixItems/0' }]
      }
    )
    schemer = JSONSchemer.schema({ '$ref' => 'http://example.com/arrays.json' }, :ref_resolver => directory)
    assert(schemer.valid?('ok'))
    refute(schemer.valid?(1))
  end

  def test_rejects_unresolvable_pointers
    assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      JSONSchemer::SchemaDirectory.new('http://example.com/a.json' => { 'prefixItems' => [{ 'type' => 'string' }], '$ref' => '#/prefixItems/5' })
    end
    assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      JSONSchemer::SchemaDirectory.new('http://example.com/a.json' => { 'prefixItems' => [{ 'type' => 'string' }], '$ref' => '#/prefixItems/foo' })
    end
    assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      JSONSchemer::SchemaDirectory.new('http://example.com/a.json' => { 'type' => 'string', '$ref' => '#/type/foo' })
    end
  end

  def test_allows_circular_self_ref_with_empty_fragment
    directory = JSONSchemer::SchemaDirectory.new(
      'http://example.com/self.json' => { 'properties' => { 'a' => { '$ref' => '#' } } }
    )
    assert(directory.key?('http://example.com/self.json'))
  end

  def test_ignores_ref_like_data_in_non_schema_keywords
    directory = JSONSchemer::SchemaDirectory.new(
      'http://example.com/data.json' => {
        'const' => { '$ref' => 'missing.json' },
        'enum' => [{ '$id' => 'not-an-id.json' }],
        'default' => { '$anchor' => 'not-an-anchor' },
        'examples' => [{ '$dynamicAnchor' => 'not-dynamic' }]
      }
    )
    assert_equal(1, directory.size)
  end

  def test_accepts_symbol_keys
    directory = JSONSchemer::SchemaDirectory.new(
      'http://example.com/symbol.json' => { :type => 'string', :properties => { :a => { :type => 'integer' } } }
    )
    assert_equal(
      { 'type' => 'string', 'properties' => { 'a' => { 'type' => 'integer' } } },
      directory.fetch('http://example.com/symbol.json')
    )
  end
end
