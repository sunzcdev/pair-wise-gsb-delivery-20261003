require 'test_helper'

class PropertyConsumptionReportTest < Minitest::Test
  def report(schema, data, **options)
    options = { :output_format => 'basic', :resolve_enumerators => true, :property_consumption_report => true }.merge(options)
    JSONSchemer.schema(schema).validate(data, **options).fetch('propertyConsumptionReport')
  end

  def root_entry(schema, data, **options)
    report(schema, data, **options).find { |entry| entry.fetch('instanceLocation') == '' }
  end

  def entry_at(schema, data, location, **options)
    report(schema, data, **options).find { |entry| entry.fetch('instanceLocation') == location }
  end

  def evaluated_names(entry)
    entry.fetch('evaluatedProperties').map { |property| property.fetch('name') }
  end

  def test_report_is_not_included_by_default
    schema = { 'properties' => { 'a' => true } }
    %w[classic flag basic detailed verbose].each do |output_format|
      output = JSONSchemer.schema(schema).validate({ 'a' => 1, 'b' => 2 }, :output_format => output_format)
      refute_includes(output.to_h, 'propertyConsumptionReport')
    end
  end

  def test_report_is_built_even_with_flag_output
    schema = { 'properties' => { 'a' => true, 'b' => true }, 'unevaluatedProperties' => false }
    output = JSONSchemer.schema(schema).validate(
      { 'a' => 1, 'c' => 2 },
      :output_format => 'flag',
      :property_consumption_report => true
    )
    refute(output.fetch('valid'))
    entry = output.fetch('propertyConsumptionReport').first
    assert_equal(['a'], evaluated_names(entry))
    assert_equal(['c'], entry.fetch('remainingProperties'))
  end

  def test_multiple_branches_each_accept_different_properties
    schema = {
      'type' => 'object',
      'anyOf' => [
        { 'properties' => { 'foo' => true } },
        { 'properties' => { 'bar' => true } }
      ],
      'unevaluatedProperties' => false
    }

    entry = root_entry(schema, { 'foo' => 1, 'bar' => 2 })
    assert_equal(%w[bar foo], evaluated_names(entry))
    assert_empty(entry.fetch('remainingProperties'))
    assert(JSONSchemer.schema(schema).valid?({ 'foo' => 1, 'bar' => 2 }))

    entry = root_entry(schema, { 'foo' => 1, 'bar' => 2, 'baz' => 3 })
    assert_equal(%w[bar foo], evaluated_names(entry))
    assert_equal(['baz'], entry.fetch('remainingProperties'))
    refute(JSONSchemer.schema(schema).valid?({ 'foo' => 1, 'bar' => 2, 'baz' => 3 }))

    foo_sources = entry.fetch('evaluatedProperties').find { |property| property.fetch('name') == 'foo' }.fetch('sources')
    assert_equal(['/anyOf/0/properties/foo'], foo_sources.map { |source| source.fetch('keywordLocation') })
  end

  def test_seemingly_matched_but_branch_fails_as_a_whole
    schema = {
      'type' => 'object',
      'anyOf' => [
        {
          'properties' => { 'foo' => true },
          'required' => ['missing']
        }
      ],
      'unevaluatedProperties' => false
    }

    entry = root_entry(schema, { 'foo' => 1 })
    assert_empty(evaluated_names(entry))
    assert_equal(['foo'], entry.fetch('remainingProperties'))
    refute(JSONSchemer.schema(schema).valid?({ 'foo' => 1 }))
  end

  def test_allof_failed_branch_does_not_contribute
    schema = {
      'allOf' => [
        { 'properties' => { 'a' => true } },
        { 'properties' => { 'b' => true }, 'required' => ['never'] }
      ],
      'unevaluatedProperties' => false
    }
    entry = root_entry(schema, { 'a' => 1, 'b' => 2 })
    assert_equal(['a'], evaluated_names(entry))
    assert_equal(['b'], entry.fetch('remainingProperties'))
  end

  def test_unselected_then_and_else_branches_do_not_count
    schema = {
      'type' => 'object',
      'properties' => { 'kind' => true },
      'if' => { 'properties' => { 'kind' => { 'const' => 'a' } } },
      'then' => { 'properties' => { 'only_when_a' => true } },
      'else' => { 'properties' => { 'only_when_b' => true } },
      'unevaluatedProperties' => false
    }

    entry = root_entry(schema, { 'kind' => 'a', 'only_when_a' => 1 })
    assert_equal(%w[kind only_when_a], evaluated_names(entry))
    assert_empty(entry.fetch('remainingProperties'))

    entry = root_entry(schema, { 'kind' => 'b', 'only_when_b' => 1 })
    assert_equal(%w[kind only_when_b], evaluated_names(entry))
    assert_empty(entry.fetch('remainingProperties'))

    entry = root_entry(schema, { 'kind' => 'a', 'only_when_b' => 1 })
    assert_equal(['kind'], evaluated_names(entry))
    assert_equal(['only_when_b'], entry.fetch('remainingProperties'))
    refute(JSONSchemer.schema(schema).valid?({ 'kind' => 'a', 'only_when_b' => 1 }))
  end

  def test_properties_and_pattern_properties_sources
    schema = {
      'properties' => { 'a' => true },
      'patternProperties' => { '^x' => true }
    }
    entry = root_entry(schema, { 'a' => 1, 'x1' => 2, 'other' => 3 })
    assert_equal(%w[a x1], evaluated_names(entry))
    assert_equal(['other'], entry.fetch('remainingProperties'))

    properties = entry.fetch('evaluatedProperties').to_h { |property| [property.fetch('name'), property.fetch('sources')] }
    assert_equal(['/properties/a'], properties.fetch('a').map { |source| source.fetch('keywordLocation') })
    assert_equal(['/patternProperties/^x'], properties.fetch('x1').map { |source| source.fetch('keywordLocation') })
  end

  def test_false_additional_properties_evaluates_but_rejects
    schema = {
      'properties' => { 'a' => true },
      'patternProperties' => { '^x' => true },
      'additionalProperties' => false
    }
    entry = root_entry(schema, { 'a' => 1, 'x1' => 2, 'other' => 3 })
    # `additionalProperties: false` still evaluates `other` (with a `false`
    # subschema) before rejecting it; it is therefore both evaluated and not
    # allowed by the schema.
    assert_equal(%w[a other x1], evaluated_names(entry))
    assert_empty(entry.fetch('remainingProperties'))
    refute(JSONSchemer.schema(schema).valid?({ 'a' => 1, 'x1' => 2, 'other' => 3 }))

    properties = entry.fetch('evaluatedProperties').to_h { |property| [property.fetch('name'), property.fetch('sources')] }
    assert_equal(['/properties/a'], properties.fetch('a').map { |source| source.fetch('keywordLocation') })
    assert_equal(['/patternProperties/^x'], properties.fetch('x1').map { |source| source.fetch('keywordLocation') })
    assert_equal(['/additionalProperties'], properties.fetch('other').map { |source| source.fetch('keywordLocation') })
  end

  def test_additional_properties_is_a_source
    schema = {
      'properties' => { 'a' => true },
      'additionalProperties' => true
    }
    entry = root_entry(schema, { 'a' => 1, 'b' => 2 })
    assert_equal(%w[a b], evaluated_names(entry))
    assert_empty(entry.fetch('remainingProperties'))
    b_sources = entry.fetch('evaluatedProperties').find { |property| property.fetch('name') == 'b' }.fetch('sources')
    assert_equal(['/additionalProperties'], b_sources.map { |source| source.fetch('keywordLocation') })
  end

  def test_ref_contributes_source_and_absolute_location
    schema = {
      '$id' => 'https://example.com/schema',
      'properties' => {
        'a' => { '$ref' => '#/$defs/inner' }
      },
      '$defs' => {
        'inner' => { 'properties' => { 'deep' => true }, 'unevaluatedProperties' => false }
      }
    }
    nested = entry_at(schema, { 'a' => { 'deep' => 1, 'stray' => 2 } }, '/a')
    assert_equal(['deep'], evaluated_names(nested))
    assert_equal(['stray'], nested.fetch('remainingProperties'))
    deep_sources = nested.fetch('evaluatedProperties').find { |property| property.fetch('name') == 'deep' }.fetch('sources')
    assert_equal('/properties/a/$ref/properties/deep', deep_sources.first.fetch('keywordLocation'))
    assert_equal('https://example.com/schema#/$defs/inner/properties/deep', deep_sources.first.fetch('absoluteKeywordLocation'))
  end

  def test_nested_objects_are_reported_per_location
    schema = {
      'type' => 'object',
      'properties' => {
        'child' => {
          'properties' => { 'kept' => true },
          'unevaluatedProperties' => false
        }
      },
      'unevaluatedProperties' => false
    }
    data = { 'child' => { 'kept' => 1, 'extra' => 2 }, 'other' => 3 }
    entries = report(schema, data)
    assert_equal(['', '/child'], entries.map { |entry| entry.fetch('instanceLocation') })
    assert_equal(['other'], root_entry(schema, data).fetch('remainingProperties'))
    assert_equal(['extra'], entry_at(schema, data, '/child').fetch('remainingProperties'))
  end

  def test_uncle_keyword_evaluation_is_not_significant
    schema = {
      'properties' => {
        'foo' => {
          'properties' => { 'bar' => { 'type' => 'string' } },
          'unevaluatedProperties' => false
        }
      },
      'anyOf' => [
        { 'properties' => { 'foo' => { 'properties' => { 'faz' => { 'type' => 'string' } } } } }
      ]
    }
    data = { 'foo' => { 'bar' => 'test', 'faz' => 'test' } }
    refute(JSONSchemer.schema(schema).valid?(data))
    entry = entry_at(schema, data, '/foo')
    assert_equal(['bar'], evaluated_names(entry))
    assert_equal(['faz'], entry.fetch('remainingProperties'))
  end

  def test_recursive_ref_reports_every_object_level
    schema = {
      'properties' => { 'x' => { '$ref' => '#' } },
      'unevaluatedProperties' => false
    }
    data = { 'x' => { 'x' => { 'x' => { 'y' => 2 } } } }
    entries = report(schema, data)
    assert_equal(['', '/x', '/x/x', '/x/x/x'], entries.map { |entry| entry.fetch('instanceLocation') })
    deepest = entries.find { |entry| entry.fetch('instanceLocation') == '/x/x/x' }
    assert_equal(['y'], deepest.fetch('remainingProperties'))
    refute(JSONSchemer.schema(schema).valid?(data))
  end

  def test_nested_unevaluated_properties_consumes_for_outer_scope
    schema = {
      'properties' => { 'foo' => { 'type' => 'string' } },
      'allOf' => [{ 'unevaluatedProperties' => true }],
      'unevaluatedProperties' => false
    }
    data = { 'foo' => 'foo', 'bar' => 'bar' }
    entry = root_entry(schema, data)
    assert_equal(['foo'], evaluated_names(entry))
    assert_empty(entry.fetch('remainingProperties'))
    assert(JSONSchemer.schema(schema).valid?(data))
  end

  def test_in_place_sibling_scope_only_sees_its_own_branch
    schema = {
      'allOf' => [{ 'properties' => { 'foo' => true } }],
      'anyOf' => [{ 'properties' => { 'bar' => true }, 'unevaluatedProperties' => false }]
    }
    assert(JSONSchemer.schema(schema).valid?({ 'bar' => 1 }))
    refute(JSONSchemer.schema(schema).valid?({ 'foo' => 1, 'bar' => 1 }))

    entry = root_entry(schema, { 'foo' => 1, 'bar' => 1 })
    assert_equal(['foo'], evaluated_names(entry))
    assert_equal(['bar'], entry.fetch('remainingProperties'))

    entry = root_entry(schema, { 'bar' => 1 })
    assert_equal(['bar'], evaluated_names(entry))
    assert_empty(entry.fetch('remainingProperties'))
  end

  def test_failed_anyof_branch_inside_successful_oneof_is_excluded
    schema = {
      'oneOf' => [
        {
          'anyOf' => [
            { 'properties' => { 'a' => true }, 'required' => ['nope'] },
            { 'properties' => { 'ok' => true } }
          ],
          'properties' => { 'ok' => true }
        }
      ],
      'unevaluatedProperties' => false
    }
    data = { 'ok' => 1, 'a' => 2 }
    refute(JSONSchemer.schema(schema).valid?(data))
    entry = root_entry(schema, data)
    assert_equal(['ok'], evaluated_names(entry))
    assert_equal(['a'], entry.fetch('remainingProperties'))
  end

  def test_dependent_schemas_consumes_applicable_but_not_trigger_property
    schema = {
      'dependentSchemas' => { 'x' => { 'properties' => { 'y' => true } } },
      'unevaluatedProperties' => false
    }
    data = { 'x' => 1, 'y' => 2 }
    entry = root_entry(schema, data)
    assert_equal(['y'], evaluated_names(entry))
    assert_equal(['x'], entry.fetch('remainingProperties'))
    refute(JSONSchemer.schema(schema).valid?(data))
  end

  def test_not_branch_annotations_match_gem_unevaluated_collection
    schema = {
      'not' => { 'properties' => { 'a' => true } },
      'unevaluatedProperties' => false
    }
    data = { 'a' => 1 }
    entry = root_entry(schema, data)
    assert_equal(['a'], evaluated_names(entry))
    assert_empty(entry.fetch('remainingProperties'))
    refute(JSONSchemer.schema(schema).valid?(data))
  end

  def test_objects_inside_arrays_are_reported
    schema = {
      'type' => 'array',
      'items' => {
        'properties' => { 'a' => true },
        'unevaluatedProperties' => false
      }
    }
    entries = report(schema, [{ 'a' => 1, 'b' => 2 }])
    nested = entries.find { |entry| entry.fetch('instanceLocation') == '/0' }
    refute_nil(nested)
    assert_equal(['a'], evaluated_names(nested))
    assert_equal(['b'], nested.fetch('remainingProperties'))
  end

  def test_non_object_instances_have_no_report_entries
    assert_empty(report({ 'type' => 'array' }, [1, 2]))
    assert_empty(report({ 'type' => 'string' }, 'x'))
  end

  def test_classic_output_still_returns_error_enumerator
    schema = { 'properties' => { 'a' => true }, 'additionalProperties' => false }
    errors = JSONSchemer.schema(schema).validate({ 'a' => 1, 'b' => 2 }, :property_consumption_report => true)
    assert_kind_of(Enumerator, errors)
    assert_equal(1, errors.to_a.size)
    assert_equal(
      ['a', 'b'],
      errors.property_consumption_report.first.fetch('evaluatedProperties').map { |property| property.fetch('name') }.sort
    )
    assert_empty(errors.property_consumption_report.first.fetch('remainingProperties'))
  end

  def test_report_does_not_invoke_custom_keywords_or_hooks_again
    custom_calls = 0
    hook_calls = 0
    schema = {
      'properties' => { 'a' => true },
      'x-count' => true
    }
    data = { 'a' => 1, 'b' => 2 }
    schemer = JSONSchemer.schema(
      schema,
      :keywords => { 'x-count' => proc { custom_calls += 1; true } },
      :before_property_validation => [proc { hook_calls += 1 }]
    )
    output = schemer.validate(
      data,
      :output_format => 'basic',
      :resolve_enumerators => true,
      :property_consumption_report => true
    )
    entry = output.fetch('propertyConsumptionReport').first
    assert_equal(['a'], evaluated_names(entry))
    assert_equal(['b'], entry.fetch('remainingProperties'))
    assert_equal(1, custom_calls)
    assert_equal(1, hook_calls)
  end
end
