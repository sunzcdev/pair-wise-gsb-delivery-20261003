require 'test_helper'

class StrictKeysTest < Minitest::Test
  def test_default_behavior_allows_key_collisions
    schema = { 'properties' => { 'id' => { 'type' => 'integer' } } }
    instance = { :id => 1, 'id' => 2 }

    assert(JSONSchemer.schema(schema).valid?(instance))
    assert_equal({ :id => 1, 'id' => 2 }, instance)

    assert(JSONSchemer.schema(schema, strict_keys: false).valid?(instance))
    assert_equal({ :id => 1, 'id' => 2 }, instance)
  end

  def test_strict_keys_rejects_symbol_and_string_collision
    schemer = JSONSchemer.schema({ 'properties' => { 'id' => { 'type' => 'integer' } } }, strict_keys: true)
    instance = { :id => 1, 'id' => 2 }

    refute(schemer.valid?(instance))

    errors = schemer.validate(instance).to_a
    assert_equal(1, errors.size)
    error = errors.first
    assert_equal('', error.fetch('data_pointer'))
    assert_equal('strict_key_collision', error.fetch('type'))
    assert_equal(['"id"', ':id'], error.fetch('details').fetch('colliding_keys'))
    assert_equal('id', error.fetch('details').fetch('string_key'))

    # original instance is never mutated
    assert_equal({ :id => 1, 'id' => 2 }, instance)
  end

  def test_strict_keys_is_independent_of_insertion_order
    schemer = JSONSchemer.schema({}, strict_keys: true)

    symbol_first = schemer.validate({ :id => 1, 'id' => 2 }).to_a
    string_first = schemer.validate({ 'id' => 2, :id => 1 }).to_a

    assert_equal(symbol_first, string_first)
    assert_equal(['"id"', ':id'], symbol_first.first.fetch('details').fetch('colliding_keys'))
  end

  def test_strict_keys_detects_any_stringifying_collision
    schemer = JSONSchemer.schema({}, strict_keys: true)

    errors = schemer.validate({ 1 => 'integer', '1' => 'string' }).to_a
    assert_equal(['"1"', '1'], errors.first.fetch('details').fetch('colliding_keys'))

    errors = schemer.validate({ 1 => 'integer', '1' => 'string', :'1' => 'symbol' }).to_a
    assert_equal(['"1"', '1', ':"1"'], errors.first.fetch('details').fetch('colliding_keys'))
    assert_equal('1', errors.first.fetch('details').fetch('string_key'))

    object_with_custom_key = Object.new
    def object_with_custom_key.to_s
      'id'
    end
    errors = schemer.validate({ object_with_custom_key => 'object', 'id' => 'string' }).to_a
    assert_equal('id', errors.first.fetch('details').fetch('string_key'))
  end

  def test_strict_keys_reports_collisions_at_nested_locations
    schemer = JSONSchemer.schema(
      {
        'properties' => {
          'a' => {
            'properties' => {
              'b' => { 'items' => { 'properties' => { 'c' => true } } }
            }
          }
        }
      },
      strict_keys: true
    )
    instance = {
      'a' => {
        'b' => [
          { 'c' => 1 },
          { :c => 2, 'c' => 3 }
        ]
      }
    }

    errors = schemer.validate(instance).to_a
    assert_equal(1, errors.size)
    assert_equal('/a/b/1', errors.first.fetch('data_pointer'))
    assert_equal(['"c"', ':c'], errors.first.fetch('details').fetch('colliding_keys'))
    assert_equal({ :c => 2, 'c' => 3 }, errors.first.fetch('data'))

    assert_equal({ :c => 2, 'c' => 3 }, instance.dig('a', 'b', 1))
  end

  def test_strict_keys_reports_all_collisions
    schemer = JSONSchemer.schema({}, strict_keys: true)
    instance = {
      :a => 1, 'a' => 2,
      'nested' => { :b => 3, 'b' => 4 }
    }

    pointers = schemer.validate(instance).to_a.map { |error| error.fetch('data_pointer') }.sort
    assert_equal(['', '/nested'], pointers)
  end

  def test_strict_keys_json_pointer_tokens_are_escaped
    schemer = JSONSchemer.schema({}, strict_keys: true)
    instance = { 'a/b' => { :x => 1, 'x' => 2 } }

    errors = schemer.validate(instance).to_a
    assert_equal('/a~1b', errors.first.fetch('data_pointer'))
  end

  def test_strict_keys_runs_before_format_checks
    format_calls = []
    schemer = JSONSchemer.schema(
      {
        'properties' => {
          'a' => { 'type' => 'integer', 'format' => 'track' }
        }
      },
      strict_keys: true,
      formats: { 'track' => proc { |data| format_calls << data; true } }
    )

    refute(schemer.valid?({ :a => 1, 'a' => 2 }))
    assert_empty(format_calls)
  end

  def test_strict_keys_runs_before_custom_keywords
    custom_keyword_calls = 0
    schemer = JSONSchemer.schema(
      { 'x-track' => true },
      strict_keys: true,
      keywords: { 'x-track' => proc { custom_keyword_calls += 1; true } }
    )

    refute(schemer.valid?({ :a => 1, 'a' => 2 }))
    assert_equal(0, custom_keyword_calls)
  end

  def test_strict_keys_does_not_mutate_input
    schemer = JSONSchemer.schema({ 'properties' => { 'a' => true } }, strict_keys: true)
    nested = { :b => 1, 'b' => 2 }
    instance = { 'a' => nested }

    refute(schemer.valid?(instance))

    assert_equal({ 'a' => nested }, instance)
    assert_equal({ :b => 1, 'b' => 2 }, nested)
    assert(instance.key?('a'))
    assert(nested.key?(:b))
    assert(nested.key?('b'))
  end

  def test_strict_keys_accepts_string_only_instances
    schemer = JSONSchemer.schema({}, strict_keys: true)

    assert(schemer.valid?({ 'a' => { 'b' => [1, 2, 3] } }))
    assert(schemer.valid?({ :a => 1, :b => 2 }))
    assert(schemer.valid?(JSON.parse('{"a":1}')))
    assert(schemer.valid?([{ 'a' => 1 }, { 'b' => 2 }]))
    assert(schemer.valid?('string'))
    assert(schemer.valid?(42))
    assert(schemer.valid?(nil))
  end

  def test_strict_keys_with_before_property_validation_hook_collision
    hook = proc do |data, property, _property_schema, _parent_schema|
      data[:x] = 99 if property == 'x'
    end
    schemer = JSONSchemer.schema(
      { 'properties' => { 'x' => { 'type' => 'integer' } } },
      strict_keys: true,
      before_property_validation: hook
    )
    instance = { 'x' => 1 }

    refute(schemer.valid?(instance))
    errors = schemer.validate(instance).to_a
    assert_equal('', errors.first.fetch('data_pointer'))
    assert_equal('strict_key_collision', errors.first.fetch('type'))
    assert_equal(['"x"', ':x'], errors.first.fetch('details').fetch('colliding_keys'))

    # swapping which key the hook inserts first produces the same error
    other_instance = { 'x' => 1 }
    assert_equal(errors.map(&:values), schemer.validate(other_instance).to_a.map(&:values))
  end

  def test_strict_keys_with_after_property_validation_hook_collision
    hook = proc do |data, property, _property_schema, _parent_schema|
      data[property.to_sym] = data[property] if property == 'x'
    end
    schemer = JSONSchemer.schema(
      { 'properties' => { 'x' => { 'type' => 'integer' } } },
      strict_keys: true,
      after_property_validation: hook
    )
    instance = { 'x' => 1 }

    refute(schemer.valid?(instance))
    errors = schemer.validate(instance).to_a
    assert_equal('', errors.first.fetch('data_pointer'))
    assert_equal(['"x"', ':x'], errors.first.fetch('details').fetch('colliding_keys'))
  end

  def test_strict_keys_allows_after_property_validation_hook_without_collision
    hook = proc do |data, property, _property_schema, _parent_schema|
      data['annotated'] = true if property == 'x'
    end
    schemer = JSONSchemer.schema(
      { 'properties' => { 'x' => { 'type' => 'integer' } } },
      strict_keys: true,
      after_property_validation: hook
    )
    instance = { 'x' => 1 }

    assert(schemer.valid?(instance))
    assert_equal({ 'x' => 1, 'annotated' => true }, instance)
  end

  def test_strict_keys_with_hook_collision_in_nested_object
    hook = proc do |data, property, _property_schema, _parent_schema|
      data[:y] = 2 if property == 'y'
    end
    schemer = JSONSchemer.schema(
      {
        'properties' => {
          'a' => {
            'properties' => {
              'y' => { 'type' => 'integer' }
            }
          }
        }
      },
      strict_keys: true,
      before_property_validation: hook
    )
    instance = { 'a' => { 'y' => 1 } }

    errors = schemer.validate(instance).to_a
    assert_equal('/a', errors.first.fetch('data_pointer'))
    assert_equal(['"y"', ':y'], errors.first.fetch('details').fetch('colliding_keys'))
  end

  def test_strict_keys_with_custom_property_default_resolver_collision
    resolver = proc do |instance, property, _results_with_tree_validity|
      instance[:"#{property}_default"] = 1
      instance["#{property}_default"] = 2
      true
    end
    schemer = JSONSchemer.schema(
      { 'properties' => { 'q' => { 'default' => true } } },
      strict_keys: true,
      insert_property_defaults: true,
      property_default_resolver: resolver
    )
    instance = {}

    refute(schemer.valid?(instance))
    errors = schemer.validate(instance).to_a
    assert_equal('', errors.first.fetch('data_pointer'))
    assert_equal(['"q_default"', ':q_default'], errors.first.fetch('details').fetch('colliding_keys'))
  end

  def test_strict_keys_with_inserted_defaults_and_no_collision
    schemer = JSONSchemer.schema(
      {
        'required' => ['a'],
        'properties' => {
          'a' => { 'default' => 1 },
          'b' => { 'properties' => { 'c' => { 'default' => 2 } } }
        }
      },
      strict_keys: true,
      insert_property_defaults: true
    )
    instance = { 'b' => {} }

    assert(schemer.valid?(instance))
    assert_equal({ 'b' => { 'c' => 2 }, 'a' => 1 }, instance)
  end

  def test_strict_keys_basic_output
    schemer = JSONSchemer.schema({}, strict_keys: true)
    instance = { 'a' => { :b => 1, 'b' => 2 } }

    output = schemer.validate(instance, output_format: 'basic', resolve_enumerators: true)
    refute(output.fetch('valid'))
    leaf = output.fetch('errors').first
    assert_equal('/a', leaf.fetch('instanceLocation'))
    assert_match('they all stringify to "b"', leaf.fetch('error'))
  end

  def test_strict_keys_flag_output
    schemer = JSONSchemer.schema({}, strict_keys: true)

    assert_equal({ 'valid' => false }, schemer.validate({ :a => 1, 'a' => 2 }, output_format: 'flag'))
    assert_equal({ 'valid' => true }, schemer.validate({ 'a' => 1 }, output_format: 'flag'))
  end

  def test_strict_keys_detailed_output
    schemer = JSONSchemer.schema({}, strict_keys: true)
    output = schemer.validate({ :a => 1, 'a' => 2 }, output_format: 'detailed', resolve_enumerators: true)

    refute(output.fetch('valid'))
    assert_equal('', output.fetch('instanceLocation'))
    assert_match('they all stringify to "a"', output.fetch('error'))
  end

  def test_strict_keys_verbose_output
    schemer = JSONSchemer.schema({}, strict_keys: true)
    output = schemer.validate({ :a => 1, 'a' => 2 }, output_format: 'verbose', resolve_enumerators: true)

    refute(output.fetch('valid'))
    assert_equal('', output.fetch('instanceLocation'))
    leaf = output.fetch('errors').first
    assert_equal('', leaf.fetch('instanceLocation'))
    assert_match('they all stringify to "a"', leaf.fetch('error'))
  end

  def test_strict_keys_collision_message_lists_every_original_key
    schemer = JSONSchemer.schema({}, strict_keys: true)
    error = schemer.validate({ 1 => 'a', '1' => 'b', :'1' => 'c' }).first

    assert_equal(
      'object at root has colliding keys "1" and 1 and :"1" (they all stringify to "1")',
      error.fetch('error')
    )
  end

  def test_strict_keys_collision_after_default_insertion_when_original_is_clean
    resolver = proc do |instance, property, _results_with_tree_validity|
      instance[:added] = 1
      instance['added'] = 2
      true
    end
    schemer = JSONSchemer.schema(
      { 'properties' => { 'seed' => { 'default' => true } } },
      strict_keys: true,
      insert_property_defaults: true,
      property_default_resolver: resolver
    )
    instance = { 'keep' => 1 }

    refute(schemer.valid?(instance))
    error = schemer.validate(instance).first
    assert_equal('', error.fetch('data_pointer'))
    assert_equal(['"added"', ':added'], error.fetch('details').fetch('colliding_keys'))

    # the second validation pass (after defaults were inserted) hits the same strict boundary
    second_error = schemer.validate(instance).first
    assert_equal('strict_key_collision', second_error.fetch('type'))
  end
end
