require 'test_helper'

class SchemaDirectoryTest < Minitest::Test
  ROOT_URI = 'https://example.com/schemas/root.json'
  DEFS_URI = 'https://example.com/schemas/common/defs.json'
  LEAF_URI = 'https://example.com/schemas/common/nested/leaf.json'
  CIRCULAR_URI = 'https://example.com/schemas/circular.json'

  # Three in-memory documents demonstrating forward references (root.json
  # refers to documents registered later in the same batch), relative base
  # URIs (relative `$ref`/`$id` resolved against enclosing base URIs) and a
  # legal circular reference (root.json <-> circular.json).
  def three_documents
    {
      ROOT_URI => {
        '$id' => ROOT_URI,
        'type' => 'object',
        'properties' => {
          'name' => { '$ref' => 'common/defs.json#name' },
          'leaf' => { '$ref' => 'common/nested/leaf.json' },
          'friend' => { '$ref' => 'circular.json' },
          'copy_of_name' => { '$ref' => '#/properties/name' }
        }
      },
      DEFS_URI => {
        '$id' => DEFS_URI,
        '$defs' => {
          'name' => { '$anchor' => 'name', 'type' => 'string' },
          'leaf' => {
            '$id' => 'nested/leaf.json',
            'type' => 'object',
            'properties' => {
              'value' => { '$ref' => '#leafValue' }
            },
            '$defs' => {
              'leafValue' => { '$anchor' => 'leafValue', 'type' => 'integer' }
            }
          }
        }
      },
      CIRCULAR_URI => {
        '$id' => CIRCULAR_URI,
        'type' => 'object',
        'properties' => {
          'parent' => { '$ref' => ROOT_URI }
        }
      }
    }
  end

  def directory
    @directory ||= JSONSchemer::SchemaDirectory.new.register(three_documents)
  end

  def test_batch_registration_and_lookup_by_uri
    assert directory.key?(ROOT_URI)
    assert directory.key?(DEFS_URI)
    assert directory.key?(CIRCULAR_URI)
    refute directory.key?('https://example.com/schemas/unknown.json')

    assert_equal three_documents[ROOT_URI], directory.fetch(ROOT_URI)
    assert_equal three_documents[ROOT_URI], directory.fetch(URI("#{ROOT_URI}#"))
    assert_equal({ '$anchor' => 'name', 'type' => 'string' }, directory.fetch("#{DEFS_URI}#name"))
    assert_equal({ '$anchor' => 'leafValue', 'type' => 'integer' }, directory.fetch("#{LEAF_URI}#/$defs/leafValue"))
    assert_equal({ '$ref' => 'common/defs.json#name' }, directory.fetch("#{ROOT_URI}#/properties/name"))
  end

  def test_nested_id_resources_are_indexed
    leaf = directory.fetch(LEAF_URI)
    assert_equal 'nested/leaf.json', leaf['$id']
    assert_equal({ '$anchor' => 'leafValue', 'type' => 'integer' }, directory.fetch("#{LEAF_URI}#leafValue"))
  end

  def test_fetch_raises_unknown_ref_for_unresolvable_uris
    assert_raises(JSONSchemer::UnknownRef) { directory.fetch('https://example.com/schemas/unknown.json') }
    assert_raises(JSONSchemer::UnknownRef) { directory.fetch('https://example.com/schemas/unknown.json#/a') }
    assert_raises(JSONSchemer::UnknownRef) { directory.fetch("#{ROOT_URI}#no-such-anchor") }
    assert_raises(JSONSchemer::UnknownRef) { directory.fetch("#{ROOT_URI}#/properties/nope") }
    assert_raises(JSONSchemer::UnknownRef) { directory.fetch("#{ROOT_URI}#/type/deeper") }
  end

  def test_ref_resolver_interface
    assert_equal three_documents[ROOT_URI], directory.call(ROOT_URI)
    assert_equal three_documents[ROOT_URI], directory.call(URI(ROOT_URI))
    assert_equal three_documents[ROOT_URI], directory.to_proc.call(ROOT_URI)
    assert_nil directory.call('https://example.com/schemas/unknown.json')
  end

  def test_end_to_end_validation_without_network
    schemer = JSONSchemer.schema({ '$ref' => ROOT_URI }, :ref_resolver => directory)

    assert schemer.valid?({
      'name' => 'Ann',
      'leaf' => { 'value' => 1 },
      'friend' => { 'parent' => { 'name' => 'Bob', 'leaf' => { 'value' => 2 } } },
      'copy_of_name' => 'Carol'
    })
    refute schemer.valid?({ 'name' => 1 })
    refute schemer.valid?({ 'leaf' => { 'value' => 'nope' } })
    refute schemer.valid?({ 'friend' => { 'parent' => { 'leaf' => { 'value' => 'nope' } } } })
  end

  def test_unknown_refs_fail_closed_instead_of_fetching
    schemer = JSONSchemer.schema({ '$ref' => 'https://example.com/schemas/nowhere.json' }, :ref_resolver => directory)
    assert_raises(JSONSchemer::InvalidRefResolution) { schemer.valid?({}) }
  end

  def test_failed_registration_rolls_back_and_leaves_directory_usable
    schemer = JSONSchemer.schema({ '$ref' => ROOT_URI }, :ref_resolver => directory)

    error = assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      directory.register(
        'https://example.com/schemas/broken.json' => { '$ref' => 'https://example.com/schemas/missing.json' },
        'https://example.com/schemas/fine.json' => { 'type' => 'string' }
      )
    end
    assert_match('https://example.com/schemas/missing.json', error.message)
    assert_match('https://example.com/schemas/broken.json#/', error.message)

    refute directory.key?('https://example.com/schemas/broken.json')
    refute directory.key?('https://example.com/schemas/fine.json')
    assert directory.key?(ROOT_URI)
    assert schemer.valid?({ 'name' => 'still working' })
  end

  def test_unresolvable_ref_variants_are_reported_with_source
    error = assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      directory.register('https://example.com/x.json' => { '$ref' => 'missing.json#/a' })
    end
    assert_match('no schema resource registered', error.message)
    assert_match('https://example.com/x.json#/', error.message)

    error = assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      directory.register('https://example.com/y.json' => { '$ref' => "#{ROOT_URI}#/properties/nope" })
    end
    assert_match('invalid JSON pointer', error.message)

    error = assert_raises(JSONSchemer::SchemaDirectory::UnresolvableRefError) do
      directory.register('https://example.com/z.json' => { '$ref' => "#{ROOT_URI}#no-such-anchor" })
    end
    assert_match('no anchor registered', error.message)
  end

  def test_duplicate_normalized_resource_uris_are_rejected_with_both_sources
    error = assert_raises(JSONSchemer::SchemaDirectory::DuplicateResourceError) do
      JSONSchemer::SchemaDirectory.new.register(
        'https://example.com/dup.json' => { 'type' => 'string' },
        'HTTPS://Example.Com:443/dup.json#' => { 'type' => 'integer' }
      )
    end
    assert_match('https://example.com/dup.json', error.message)
    assert_match('registered by', error.message)

    error = assert_raises(JSONSchemer::SchemaDirectory::DuplicateResourceError) do
      JSONSchemer::SchemaDirectory.new.register(
        'https://example.com/a.json' => { '$defs' => { 'x' => { '$id' => 'b.json' } } },
        'https://example.com/b.json' => { 'type' => 'string' }
      )
    end
    assert_match('https://example.com/a.json#/$defs/x', error.message)
    assert_match('https://example.com/b.json#/', error.message)

    assert_raises(JSONSchemer::SchemaDirectory::DuplicateResourceError) do
      directory.register(ROOT_URI => { 'type' => 'string' })
    end
  end

  def test_duplicate_anchors_in_one_resource_are_rejected_with_both_sources
    error = assert_raises(JSONSchemer::SchemaDirectory::DuplicateAnchorError) do
      JSONSchemer::SchemaDirectory.new.register(
        'https://example.com/anchors.json' => {
          '$defs' => {
            'one' => { '$anchor' => 'dup' },
            'two' => { '$anchor' => 'dup' }
          }
        }
      )
    end
    assert_match('https://example.com/anchors.json#dup', error.message)
    assert_match('/$defs/one', error.message)
    assert_match('/$defs/two', error.message)

    assert_raises(JSONSchemer::SchemaDirectory::DuplicateAnchorError) do
      JSONSchemer::SchemaDirectory.new.register(
        'https://example.com/anchors.json' => {
          '$defs' => {
            'one' => { '$anchor' => 'dup' },
            'two' => { '$dynamicAnchor' => 'dup' }
          }
        }
      )
    end
  end

  def test_caller_mutation_does_not_affect_registered_content
    doc = {
      '$id' => 'https://example.com/mutable.json',
      'type' => 'object',
      'properties' => { 'a' => { 'type' => 'string' } },
      'required' => ['a']
    }
    dir = JSONSchemer::SchemaDirectory.new('https://example.com/mutable.json' => doc)

    doc['properties']['a']['type'] = 'integer'
    doc['properties']['b'] = { 'type' => 'number' }
    doc['required'] << 'b'

    fetched = dir.fetch('https://example.com/mutable.json')
    assert_equal 'string', fetched['properties']['a']['type']
    assert_nil fetched['properties']['b']
    assert_equal ['a'], fetched['required']
    assert fetched.frozen?
    assert fetched['properties'].frozen?
    assert_raises(FrozenError) { fetched['properties']['a']['type'] = 'number' }
  end

  def test_directories_are_independent
    other = JSONSchemer::SchemaDirectory.new
    refute other.key?(ROOT_URI)
    other.register('https://example.com/other.json' => { 'type' => 'string' })
    assert other.key?('https://example.com/other.json')
    refute directory.key?('https://example.com/other.json')
  end

  def test_register_returns_self_and_initial_batch
    dir = JSONSchemer::SchemaDirectory.new('https://example.com/init.json' => { 'type' => 'string' })
    assert dir.key?('https://example.com/init.json')
    assert_same dir, dir.register('https://example.com/init2.json' => { 'type' => 'integer' })
  end

  def test_later_batches_can_reference_existing_documents
    directory.register(
      'https://example.com/schemas/second.json' => {
        '$ref' => 'common/defs.json#name',
        'allOf' => [{ '$ref' => "#{DEFS_URI}#/$defs/name" }]
      }
    )
    assert directory.key?('https://example.com/schemas/second.json')
  end

  def test_non_object_documents_are_rejected
    error = assert_raises(JSONSchemer::SchemaDirectory::Error) do
      JSONSchemer::SchemaDirectory.new.register('https://example.com/bool.json' => true)
    end
    assert_match('must be an object', error.message)
  end

  def test_symbol_keys_are_stringified_and_non_schema_keywords_are_not_walked
    dir = JSONSchemer::SchemaDirectory.new(
      URI('https://example.com/sym.json') => {
        :'$id' => 'https://example.com/sym-canonical.json',
        :'$anchor' => 'sym',
        :type => 'string'
      },
      'https://example.com/const.json' => {
        'const' => { 1 => { '$ref' => 'https://example.com/never-checked.json' } }
      }
    )
    assert dir.key?('https://example.com/sym.json')
    assert dir.key?('https://example.com/sym-canonical.json')
    assert dir.key?('https://example.com/sym-canonical.json#sym')
    assert dir.key?('https://example.com/const.json')
  end

  def test_non_string_keyword_values_and_arrays_are_tolerated
    dir = JSONSchemer::SchemaDirectory.new(
      'https://example.com/misc.json' => {
        '$id' => '',
        'properties' => {
          'a' => { '$id' => 5, '$anchor' => nil, '$dynamicAnchor' => ['x'], '$ref' => nil }
        },
        'allOf' => [{ 'type' => 'string', 'minLength' => 3 }]
      }
    )
    assert_equal({ 'type' => 'string', 'minLength' => 3 }, dir.fetch('https://example.com/misc.json#/allOf/0'))
    assert_raises(JSONSchemer::UnknownRef) { dir.fetch('https://example.com/misc.json#/allOf/x') }
    assert_raises(JSONSchemer::UnknownRef) { dir.fetch('https://example.com/misc.json#/allOf/9') }
  end

  def test_dynamic_refs_keep_runtime_scope_semantics
    dir = JSONSchemer::SchemaDirectory.new(
      'https://example.com/dynamic/strict-tree.json' => {
        '$id' => 'https://example.com/dynamic/strict-tree.json',
        '$dynamicAnchor' => 'node',
        'type' => 'object',
        'properties' => {
          'data' => true,
          'children' => { 'type' => 'array', 'items' => { '$dynamicRef' => '#node' } }
        },
        'unevaluatedProperties' => false
      },
      'https://example.com/dynamic/tree.json' => {
        '$id' => 'https://example.com/dynamic/tree.json',
        '$dynamicAnchor' => 'node',
        'type' => 'object',
        'properties' => {
          'data' => true,
          'children' => { 'type' => 'array', 'items' => { '$ref' => 'strict-tree.json' } }
        }
      },
      # a `$dynamicRef` target is not required to exist at registration
      # time: it is bound by the runtime dynamic scope
      'https://example.com/dynamic/runtime-only.json' => {
        'properties' => { 'x' => { '$dynamicRef' => '#not-registered-anywhere' } }
      }
    )

    schemer = JSONSchemer.schema({ '$ref' => 'https://example.com/dynamic/tree.json' }, :ref_resolver => dir)

    # strict-tree applies at the first level: extra properties are rejected
    refute schemer.valid?({ 'data' => 1, 'children' => [{ 'data' => 2, 'junk' => 3 }] })
    # the dynamic scope rebinds `#node` to tree.json below the first level,
    # where extra properties are allowed again
    assert schemer.valid?({ 'data' => 1, 'children' => [{ 'data' => 2, 'children' => [{ 'data' => 3, 'junk' => 4 }] }] })
  end
end
