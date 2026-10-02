require 'test_helper'

class WorkBudgetTest < Minitest::Test
  def test_ignored_without_budget
    schemer = JSONSchemer.schema({ 'type' => 'integer' })
    assert(schemer.valid?(1))
    refute(schemer.valid?('x'))
    assert_equal({ 'valid' => true }, schemer.validate(1, :output_format => 'flag'))
    assert_equal([], schemer.validate(1).to_a)
  end

  def test_invalid_budget
    schemer = JSONSchemer.schema({ 'type' => 'integer' })
    assert_raises(ArgumentError) { schemer.valid?(1, :work_budget => -1) }
    assert_raises(ArgumentError) { schemer.valid?(1, :work_budget => '1') }
    assert_raises(ArgumentError) { schemer.validate(1, :work_budget => 1.5) }
  end

  def test_zero_budget_stops_at_first_keyword
    schemer = JSONSchemer.schema({ 'type' => 'integer' })
    error = assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?(1, :work_budget => 0) }
    assert_equal('', error.instance_location)
    assert_equal('/type', error.keyword_location)
    assert_equal(0, error.used)
    assert_equal(0, error.limit)
  end

  def test_empty_schema_costs_nothing
    schemer = JSONSchemer.schema({})
    assert(schemer.valid?(42, :work_budget => 0))
  end

  def test_conclusive_validation_within_budget
    schemer = JSONSchemer.schema({ 'type' => 'object', 'required' => ['a'] })
    assert(schemer.valid?({ 'a' => 1 }, :work_budget => 2))
    # a definite failure reached within the budget is a conclusion, not an exception
    refute(schemer.valid?(42, :work_budget => 1))
  end

  def test_budget_state_is_call_local
    schemer = JSONSchemer.schema({ 'type' => 'object', 'properties' => { 'a' => { 'type' => 'integer' } } })
    assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?({ 'a' => 1 }, :work_budget => 1) }
    assert(schemer.valid?({ 'a' => 1 }))
    assert(schemer.valid?({ 'a' => 1 }, :work_budget => 10))
    assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?({ 'a' => 1 }, :work_budget => 1) }
  end

  def test_custom_keyword_is_charged_once_per_evaluation
    calls = []
    schemer = JSONSchemer.schema(
      { 'properties' => { 'x' => { 'tick' => true }, 'y' => { 'tick' => true } } },
      :keywords => {
        'tick' => proc do |instance, _schema, instance_location|
          calls << [instance, instance_location]
          true
        end
      }
    )
    # root + properties + (/x subschema + tick) + (/y subschema + tick) = 5
    assert(schemer.valid?({ 'x' => 1, 'y' => 2 }, :work_budget => 5))
    assert_equal(2, calls.size)

    calls.clear
    error = assert_raises(JSONSchemer::BudgetExceeded) do
      schemer.valid?({ 'x' => 1, 'y' => 2 }, :work_budget => 3)
    end
    assert_equal('/properties/y/tick', error.keyword_location)
    assert_equal('/y', error.instance_location)
    assert_equal(1, calls.size)
  end

  def test_ref_entry_is_charged
    schemer = JSONSchemer.schema({ '$ref' => '#/$defs/t', '$defs' => { 't' => { 'type' => 'integer' } } })
    # root $ref (1) -> referenced 'type' (2) -> sibling $defs (3)
    assert(schemer.valid?(1, :work_budget => 3))
    error = assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?(1, :work_budget => 1) }
    assert_equal('/$ref/type', error.keyword_location)
  end

  def test_any_of_does_not_pass_on_incomplete_branches
    schemer = JSONSchemer.schema({
      'anyOf' => [
        { 'type' => 'integer', 'minimum' => 0 },
        { 'type' => 'string', 'minLength' => 3 }
      ]
    })
    # root + anyOf + branch0 type + branch0 minimum + branch1 type + branch1 minLength = 6
    assert(schemer.valid?(5, :work_budget => 6))
    # non-matching instance: exhaust while evaluating branch1, cannot conclude
    error = assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?(:nope, :work_budget => 2) }
    assert_equal('/anyOf/1/type', error.keyword_location)
    # but when every branch is conclusively evaluated, the normal result stands
    refute(schemer.valid?(:nope, :work_budget => 6))
  end

  def test_one_of_is_incomplete_until_all_branches_checked
    schemer = JSONSchemer.schema({
      'oneOf' => [
        { 'type' => 'integer' },
        { 'type' => 'string' }
      ]
    })
    assert(schemer.valid?(1, :work_budget => 3))
    assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?(1, :work_budget => 2) }
  end

  def test_not_is_incomplete_when_negated_schema_is
    schemer = JSONSchemer.schema({ 'not' => { 'type' => 'string', 'minLength' => 1 } })
    # a string matches the negated schema, so the result is conclusively invalid when complete
    refute(schemer.valid?('hi'))
    # root + not + type = 3; minLength unvisited => unknown, must not pass
    assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?('hi', :work_budget => 2) }
  end

  def test_if_then_else
    schemer = JSONSchemer.schema({
      'if' => { 'type' => 'object' },
      'then' => { 'required' => ['id'] },
      'else' => { 'type' => 'string' }
    })
    # root + if + if/type + then + then/required + else + else/type
    assert(schemer.valid?({ 'id' => 1 }, :work_budget => 7))
    refute(schemer.valid?({}, :work_budget => 7))
    # exhausting while evaluating `if` means then/else cannot be selected
    error = assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?({ 'id' => 1 }, :work_budget => 1) }
    assert_equal('/if/type', error.keyword_location)
    # exhausting right after `if` means `then` is the frontier (not executed)
    error = assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?({ 'id' => 1 }, :work_budget => 2) }
    assert_equal('/then', error.keyword_location)

    # when `if` itself is incomplete, neither `then` nor `else` may decide
    gated = JSONSchemer.schema({
      'if' => { 'type' => 'object', 'minProperties' => 1 },
      'then' => { 'required' => ['id'] },
      'else' => { 'type' => 'string' }
    })
    error = assert_raises(JSONSchemer::BudgetExceeded) { gated.valid?({}, :work_budget => 1) }
    assert_equal('/if/type', error.keyword_location)
    refute(gated.valid?({}))
  end

  def test_unevaluated_properties_never_runs_on_incomplete
    schemer = JSONSchemer.schema({
      'properties' => { 'a' => true },
      'unevaluatedProperties' => false
    })
    assert(schemer.valid?({ 'a' => 1 }))
    refute(schemer.valid?({ 'a' => 1, 'b' => 2 }))
    # unevaluatedProperties is ordered before properties; with one unit the
    # frontier is its own evaluation, so the disallowed property is never judged
    error = assert_raises(JSONSchemer::BudgetExceeded) do
      schemer.valid?({ 'a' => 1, 'b' => 2 }, :work_budget => 1)
    end
    assert_equal('/unevaluatedProperties', error.keyword_location)
  end

  def test_deep_nested_array_stops_at_frontier
    schemer = JSONSchemer.schema({ 'items' => { 'items' => { 'type' => 'integer' } } })
    # outer items + (inner items per element) + each leaf type evaluation
    error = assert_raises(JSONSchemer::BudgetExceeded) do
      schemer.valid?([[1, 2], [3, 4], [5, 6]], :work_budget => 4)
    end
    assert_equal('/1', error.instance_location)
    assert_equal('/items/items', error.keyword_location)
  end

  def test_stop_inside_unevaluated_property_subschema
    schemer = JSONSchemer.schema({
      'properties' => { 'a' => true },
      'unevaluatedProperties' => { 'type' => 'integer' }
    })
    error = assert_raises(JSONSchemer::BudgetExceeded) do
      schemer.valid?({ 'a' => 1, 'b' => 'bad', 'c' => 'bad' }, :work_budget => 2)
    end
    assert_equal('/b', error.instance_location)
    assert_equal('/unevaluatedProperties/type', error.keyword_location)
  end

  def test_detailed_and_verbose_outputs_are_incomplete
    schemer = JSONSchemer.schema({ 'type' => 'integer', 'minimum' => 5 })
    detailed = schemer.validate('x', :work_budget => 1, :output_format => 'detailed', :resolve_enumerators => true)
    refute(detailed.fetch('valid'))
    assert(detailed.fetch('incomplete'))

    verbose = schemer.validate('x', :work_budget => 1, :output_format => 'verbose', :resolve_enumerators => true)
    refute(verbose.fetch('valid'))
    assert(verbose.fetch('incomplete'))
    assert_equal('/minimum', verbose.fetch('budget').fetch('keywordLocation'))
  end

  def test_annotations_do_not_persist_across_calls
    schemer = JSONSchemer.schema({ 'properties' => { 'a' => true }, 'unevaluatedProperties' => false })
    assert_raises(JSONSchemer::BudgetExceeded) { schemer.valid?({ 'a' => 1, 'b' => 2 }, :work_budget => 1) }
    refute(schemer.valid?({ 'a' => 1, 'b' => 2 }))
    assert(schemer.valid?({ 'a' => 1 }))
  end

  def test_non_flag_output_is_marked_incomplete_with_locations
    schemer = JSONSchemer.schema({ 'type' => 'integer', 'minimum' => 5 })
    # a confirmed error plus an unvisited keyword
    output = schemer.validate('x', :work_budget => 1, :output_format => 'basic', :resolve_enumerators => true)
    refute(output.fetch('valid'))
    assert(output.fetch('incomplete'))
    assert_equal('/minimum', output.fetch('budget').fetch('keywordLocation'))
    assert_equal('', output.fetch('budget').fetch('instanceLocation'))
    assert_equal(1, output.fetch('budget').fetch('used'))
    # the confirmed type error is retained
    assert(output.fetch('errors').any? { |unit| unit.fetch('keywordLocation') == '/type' })
  end

  def test_classic_output_keeps_errors_and_marks_nonexhaustive
    schemer = JSONSchemer.schema({ 'type' => 'integer', 'minimum' => 5 })
    errors = schemer.validate('x', :work_budget => 1).to_a
    marker = errors.find { |error| error['incomplete'] }
    refute_nil(marker)
    assert_equal('work_budget_exceeded', marker.fetch('type'))
    assert_equal('/minimum', marker.fetch('details').fetch('keyword_location'))
    assert(errors.any? { |error| error['data_pointer'] == '' && error.fetch('error').include?('integer') })
  end
end
