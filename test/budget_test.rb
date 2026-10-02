require 'test_helper'

class BudgetTest < Minitest::Test
  def test_output_is_unchanged_without_budget
    schema = JSONSchemer.schema({
      'type' => 'object',
      'properties' => { 'a' => { 'type' => 'integer' } }
    })

    assert_equal({ 'valid' => true }, schema.validate({ 'a' => 1 }, :output_format => 'flag'))
    assert_equal({ 'valid' => false }, schema.validate({ 'a' => 'x' }, :output_format => 'flag'))
    assert(schema.valid?({ 'a' => 1 }))
    refute(schema.valid?({ 'a' => 'x' }))
  end

  def test_validation_completes_within_budget
    schema = JSONSchemer.schema({ 'type' => 'integer', 'minimum' => 10 })

    assert_equal({ 'valid' => true }, schema.validate(11, :output_format => 'flag', :budget => 2))
    assert_equal({ 'valid' => false }, schema.validate('x', :output_format => 'basic', :budget => 2).slice('valid', 'incomplete'))
    assert(schema.valid?(11, :budget => 2))
    refute(schema.valid?(5, :budget => 2))
  end

  def test_validate_marks_result_incomplete_when_budget_is_exceeded
    schema = JSONSchemer.schema({
      'type' => 'object',
      'properties' => { 'a' => { 'type' => 'integer' } }
    })

    output = schema.validate({ 'a' => 1 }, :budget => 1)

    assert_equal(false, output['valid'])
    assert_equal(true, output['incomplete'])
    assert_equal('/a', output['instanceLocation'])
    assert_equal('/properties/a/type', output['keywordLocation'])
  end

  def test_incomplete_result_is_not_a_plain_validation_error
    schema = JSONSchemer.schema({ 'type' => 'integer', 'minimum' => 10 })

    output = schema.validate(5, :output_format => 'classic', :budget => 1)

    assert_kind_of(Hash, output)
    assert_equal(true, output['incomplete'])
    assert_equal(false, output['valid'])
  end

  def test_valid_raises_when_budget_is_exceeded
    schema = JSONSchemer.schema({ 'type' => 'integer', 'minimum' => 10 })

    error = assert_raises(JSONSchemer::EvaluationBudgetExceeded) do
      schema.valid?(5, :budget => 1)
    end

    assert_equal('', error.instance_location)
    assert_equal('/minimum', error.keyword_location)
  end

  def test_zero_budget_stops_at_first_keyword
    schema = JSONSchemer.schema({ 'type' => 'integer' })

    output = schema.validate(1, :budget => 0)

    assert_equal(true, output['incomplete'])
    assert_equal('', output['instanceLocation'])
    assert_equal('/type', output['keywordLocation'])
  end

  def test_keywords_entered_through_ref_count_towards_budget
    schema = JSONSchemer.schema({
      '$ref' => '#/$defs/positive',
      '$defs' => {
        'positive' => { 'type' => 'integer', 'minimum' => 1 }
      }
    })

    assert(schema.valid?(5, :budget => 4))

    output = schema.validate(5, :budget => 2)

    assert_equal(true, output['incomplete'])
    assert_equal('', output['instanceLocation'])
    assert_equal('/$ref/minimum', output['keywordLocation'])
  end

  def test_short_circuited_keywords_are_not_counted
    schema = JSONSchemer.schema({ 'type' => 'integer', 'minimum' => 10 })

    # flag output short-circuits after `type` fails, so `minimum` is never evaluated
    assert_equal({ 'valid' => false }, schema.validate('x', :output_format => 'flag', :budget => 1))

    # basic output evaluates every keyword, so the same budget is exceeded
    assert_equal(true, schema.validate('x', :output_format => 'basic', :budget => 1)['incomplete'])
  end

  def test_any_of_cannot_conclude_from_partial_branches
    schema = JSONSchemer.schema({
      'anyOf' => [{ 'type' => 'string' }, { 'type' => 'integer' }]
    })

    assert(schema.valid?(1, :budget => 3))

    output = schema.validate(1, :budget => 2)

    assert_equal(true, output['incomplete'])
    assert_equal(false, output['valid'])
    assert_equal('', output['instanceLocation'])
    assert_equal('/anyOf/1/type', output['keywordLocation'])
  end

  def test_one_of_cannot_conclude_from_partial_branches
    schema = JSONSchemer.schema({
      'oneOf' => [{ 'type' => 'integer' }, { 'minimum' => 10 }]
    })

    assert(schema.valid?(5, :budget => 3))

    output = schema.validate(5, :budget => 2)

    assert_equal(true, output['incomplete'])
    assert_equal(false, output['valid'])
    assert_equal('/oneOf/1/minimum', output['keywordLocation'])
  end

  def test_not_cannot_conclude_from_partial_subschema
    schema = JSONSchemer.schema({
      'not' => { 'type' => 'string', 'minLength' => 2 }
    })

    assert(schema.valid?('x', :budget => 3))

    output = schema.validate('x', :budget => 2)

    assert_equal(true, output['incomplete'])
    assert_equal(false, output['valid'])
    assert_equal('/not/minLength', output['keywordLocation'])
  end

  def test_if_then_else_cannot_conclude_from_partial_conditional
    schema = JSONSchemer.schema({
      'if' => { 'type' => 'integer' },
      'then' => { 'minimum' => 10 },
      'else' => { 'maximum' => 0 }
    })

    assert(schema.valid?(11, :budget => 5))

    output = schema.validate(11, :budget => 3)

    assert_equal(true, output['incomplete'])
    assert_equal(false, output['valid'])
    assert_equal('/then/minimum', output['keywordLocation'])
  end

  def test_unevaluated_properties_cannot_conclude_from_partial_evaluation
    schema = JSONSchemer.schema({
      'properties' => { 'a' => { 'type' => 'integer' } },
      'unevaluatedProperties' => false
    })

    refute(schema.valid?({ 'a' => 1, 'b' => 2 }, :budget => 3))

    output = schema.validate({ 'a' => 1, 'b' => 2 }, :budget => 2)

    assert_equal(true, output['incomplete'])
    assert_equal(false, output['valid'])
    assert_equal('/unevaluatedProperties', output['keywordLocation'])
  end

  def test_custom_keyword_call_counts_stop_at_budget
    calls = 0
    counter = proc do |_instance, _schema, _instance_location|
      calls += 1
      true
    end
    schema = JSONSchemer.schema(
      {
        'x-count' => true,
        'properties' => { 'a' => { 'x-count' => true } }
      },
      :keywords => { 'x-count' => counter }
    )

    assert(schema.valid?({ 'a' => 1 }))
    assert_equal(2, calls)

    calls = 0
    output = schema.validate({ 'a' => 1 }, :budget => 4)

    assert_equal(true, output['incomplete'])
    assert_equal(1, calls)
  end

  def test_nested_arrays_and_any_of_stop_at_budget
    calls = 0
    counter = proc do |_instance, _schema, _instance_location|
      calls += 1
      true
    end
    schema = JSONSchemer.schema(
      {
        'items' => {
          'anyOf' => [
            { 'x-count' => true },
            { 'x-count' => true }
          ]
        }
      },
      :keywords => { 'x-count' => counter }
    )
    instance = [[1], [2]]

    assert(schema.valid?(instance))
    assert_equal(4, calls)

    calls = 0
    output = schema.validate(instance, :budget => 6)

    assert_equal(true, output['incomplete'])
    assert_operator(calls, :<, 4)
  end

  def test_budget_state_does_not_leak_between_calls
    schema = JSONSchemer.schema({ 'type' => 'integer', 'minimum' => 10 })

    assert_equal(true, schema.validate(5, :budget => 1)['incomplete'])
    assert_equal({ 'valid' => false }, schema.validate(5, :output_format => 'flag'))
    assert_equal({ 'valid' => true }, schema.validate(11, :output_format => 'flag', :budget => 2))
    assert_equal(true, schema.validate(11, :budget => 1)['incomplete'])
  end
end
