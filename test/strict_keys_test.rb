require 'test_helper'

class StrictKeysTest < Minitest::Test
  def strict_schema(schema = { 'type' => 'object' }, **options)
    JSONSchemer.schema(schema, :strict_keys => true, **options)
  end

  def test_key_collision_allowed_by_default
    schemer = JSONSchemer.schema({ 'properties' => { 'id' => { 'const' => 2 } } })
    assert(schemer.valid?({ :id => 1, 'id' => 2 }))
    refute(schemer.valid?({ 'id' => 2, :id => 1 }))
  end

  def test_strict_keys_raises_on_root_collision
    error = assert_raises(JSONSchemer::KeyCollision) do
      strict_schema.valid?({ :id => 1, 'id' => 2 })
    end
    assert_equal('', error.instance_location)
    assert_equal(['id', :id], error.keys)
    assert_equal('key collision at root: "id" (String) and :id (Symbol) both resolve to `id`', error.message)
  end

  def test_strict_keys_raises_on_nested_collision
    error = assert_raises(JSONSchemer::KeyCollision) do
      strict_schema.valid?({ 'a' => [{ 'b' => { :id => 1, 'id' => 2 } }] })
    end
    assert_equal('/a/0/b', error.instance_location)
    assert_equal(['id', :id], error.keys)
    assert_equal('key collision at `/a/0/b`: "id" (String) and :id (Symbol) both resolve to `id`', error.message)
  end

  def test_strict_keys_escapes_json_pointer_tokens
    error = assert_raises(JSONSchemer::KeyCollision) do
      strict_schema.valid?({ 'a/b~c' => { :id => 1, 'id' => 2 } })
    end
    assert_equal('/a~1b~0c', error.instance_location)
  end

  def test_strict_keys_raises_on_non_symbol_collision
    error = assert_raises(JSONSchemer::KeyCollision) do
      strict_schema.valid?({ 1 => 'a', '1' => 'b' })
    end
    assert_equal([1, '1'], error.keys)
    assert_equal('key collision at root: 1 (Integer) and "1" (String) both resolve to `1`', error.message)
  end

  def test_strict_keys_error_is_insertion_order_independent
    schemer = strict_schema
    error_a = assert_raises(JSONSchemer::KeyCollision) { schemer.valid?({ :id => 1, 'id' => 2 }) }
    error_b = assert_raises(JSONSchemer::KeyCollision) { schemer.valid?({ 'id' => 2, :id => 1 }) }
    assert_equal(error_a.message, error_b.message)
    assert_equal(error_a.keys, error_b.keys)
    assert_equal(error_a.instance_location, error_b.instance_location)
  end

  def test_strict_keys_valid_instances
    schemer = strict_schema({})
    assert(schemer.valid?({ :id => 1, 'name' => 'x', 'list' => [{ 'a' => 1 }, 2, nil] }))
    assert(schemer.valid?([1, 'two', { :three => 3 }]))
    assert(schemer.valid?('scalar'))
  end

  def test_strict_keys_does_not_modify_input
    schemer = strict_schema
    data = { :id => 1, 'items' => [{ :a => 1 }] }
    assert(schemer.valid?(data))
    assert_equal({ :id => 1, 'items' => [{ :a => 1 }] }, data)

    colliding = { :id => 1, 'id' => 2 }
    assert_raises(JSONSchemer::KeyCollision) { schemer.valid?(colliding) }
    assert_equal({ :id => 1, 'id' => 2 }, colliding)
  end

  def test_strict_keys_check_runs_before_formats_and_custom_keywords
    format_called = false
    custom_keyword_called = false
    schemer = strict_schema(
      {
        'x-custom' => true,
        'properties' => {
          'id' => { 'type' => 'string', 'format' => 'strict-keys-test' }
        }
      },
      :formats => { 'strict-keys-test' => proc { |*| format_called = true } },
      :keywords => { 'x-custom' => proc { |*| custom_keyword_called = true } }
    )
    assert_raises(JSONSchemer::KeyCollision) { schemer.valid?({ :id => 'a', 'id' => 'b' }) }
    refute(format_called)
    refute(custom_keyword_called)
  end

  def test_strict_keys_before_property_validation_hook_collision
    schemer = strict_schema(
      {
        'properties' => {
          'obj' => {
            'properties' => { 'a' => {} }
          }
        }
      },
      :before_property_validation => proc { |data, property, _property_schema, _parent| data[:a] = 2 if property == 'a' }
    )
    error = assert_raises(JSONSchemer::KeyCollision) { schemer.valid?({ 'obj' => { 'a' => 1 } }) }
    assert_equal('/obj', error.instance_location)
    assert_equal(['a', :a], error.keys)
  end

  def test_strict_keys_after_property_validation_hook_collision
    schemer = strict_schema(
      {
        'properties' => {
          'obj' => {
            'properties' => { 'a' => {} }
          }
        }
      },
      :after_property_validation => proc { |data, property, _property_schema, _parent| data[:a] = 2 if property == 'a' }
    )
    error = assert_raises(JSONSchemer::KeyCollision) { schemer.valid?({ 'obj' => { 'a' => 1 } }) }
    assert_equal('/obj', error.instance_location)
    assert_equal(['a', :a], error.keys)
  end

  def test_strict_keys_insert_property_defaults_collision
    resolver = proc do |instance, property, _results_with_tree_validity|
      instance[property] = { :x => 1, 'x' => 2 }
      true
    end
    schemer = strict_schema(
      { 'properties' => { 'a' => { 'default' => 1 } } },
      :insert_property_defaults => true,
      :property_default_resolver => resolver
    )
    error = assert_raises(JSONSchemer::KeyCollision) { schemer.valid?({}) }
    assert_equal('/a', error.instance_location)
    assert_equal(['x', :x], error.keys)
  end
end
