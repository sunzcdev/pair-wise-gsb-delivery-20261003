require 'test_helper'

class PropertyConsumptionReportTest < Minitest::Test
  PROPERTIES_CLASS = JSONSchemer::Draft202012::Vocab::Applicator::Properties
  NOT_CLASS = JSONSchemer::Draft202012::Vocab::Applicator::Not

  def validate_tree(schema, instance)
    schemer = JSONSchemer.schema(schema)
    root = JSONSchemer::Location.root
    context = JSONSchemer::Schema::Context.new(
      schemer.send(:deep_stringify_keys, instance), [], nil, false, nil
    )
    schemer.send(
      :validate_instance,
      schemer.send(:deep_stringify_keys, instance), root, root, context
    )
  end

  def find_results(result, klass, found = [])
    found << result if result.source.is_a?(klass)
    result.nested&.each { |nested| find_results(nested, klass, found) }
    found
  end

  def annotations_for(result)
    found = []
    JSONSchemer::PropertyAnnotations.each(result) { |annotation| found << annotation }
    found
  end

  def fake_result(source:, valid: true, nested: nil, location: JSONSchemer::Location.root)
    JSONSchemer::Result.new(
      source, {}, location, location, valid, nested, nil, ['x'], nil, false,
      valid ? 'annotations' : 'errors'
    )
  end

  def report(schema, instance, **options)
    JSONSchemer
      .schema(schema)
      .validate(instance, :property_consumption_report => true, **options)
      .fetch('propertyConsumptionReport')
  end

  def root_report(schema, instance, **options)
    report(schema, instance, **options).find { |entry| entry.fetch('instanceLocation') == '' }
  end

  def rejected_properties(schema, instance)
    JSONSchemer.schema(schema).validate(instance).flat_map do |error|
      next [] unless error.fetch('schema_pointer').split('/').last == 'unevaluatedProperties'
      parent, _, property = error.fetch('data_pointer').rpartition('/')
      parent.empty? ? [property] : []
    end.sort
  end

  def test_disabled_by_default
    output = JSONSchemer.schema({ 'properties' => { 'a' => {} } }).validate({ 'a' => 1 })
    assert_kind_of(Enumerator, output)
    assert_equal([], output.to_a)
  end

  def test_wrapper_contains_valid_errors_and_report
    output = JSONSchemer.schema(
      { 'properties' => { 'a' => { 'type' => 'integer' } }, 'unevaluatedProperties' => false }
    ).validate({ 'a' => 'x', 'b' => 2 }, :property_consumption_report => true)

    assert_equal(false, output.fetch('valid'))
    assert_kind_of(Enumerator, output.fetch('errors'))
    assert(output.key?('propertyConsumptionReport'))

    errors = output.fetch('errors').to_a
    assert_equal(['/properties/a', '/unevaluatedProperties'], errors.map { |error| error.fetch('schema_pointer') })
  end

  def test_flag_output_includes_full_report
    output = JSONSchemer.schema(
      { 'properties' => { 'a' => {} } }
    ).validate({ 'a' => 1 }, :output_format => 'flag', :property_consumption_report => true)

    refute(output.key?('errors'))
    assert_equal(true, output.fetch('valid'))
    assert_equal(['a'], output.fetch('propertyConsumptionReport').first.fetch('evaluatedProperties').keys)
  end

  def test_schema_level_option
    output = JSONSchemer.schema(
      { 'properties' => { 'a' => {} } }, :property_consumption_report => true
    ).validate({ 'a' => 1 })
    assert(output.key?('propertyConsumptionReport'))
  end

  def test_resolve_enumerators
    schema = {
      'properties' => { 'a' => { 'type' => 'integer' } },
      'unevaluatedProperties' => false
    }

    output = JSONSchemer.schema(schema).validate(
      { 'a' => 'x', 'b' => 2 },
      :output_format => 'basic',
      :property_consumption_report => true,
      :resolve_enumerators => true
    )
    assert_kind_of(Array, output.fetch('errors').fetch('errors'))

    classic = JSONSchemer.schema(schema).validate(
      { 'a' => 'x' },
      :property_consumption_report => true,
      :resolve_enumerators => true
    )
    assert_kind_of(Array, classic.fetch('errors'))
  end

  def test_multiple_branches_accept_different_properties
    schema = {
      'anyOf' => [
        { 'properties' => { 'a' => {} } },
        { 'properties' => { 'b' => {} } }
      ],
      'unevaluatedProperties' => false
    }

    assert_equal([], root_report(schema, { 'a' => 1 }).fetch('remainingProperties'))
    assert_equal(['/anyOf/0/properties/a'],
      root_report(schema, { 'a' => 1 }).fetch('evaluatedProperties').fetch('a').map { |source| source.fetch('keywordLocation') })

    assert_equal([], root_report(schema, { 'b' => 1 }).fetch('remainingProperties'))
    assert_equal(['/anyOf/1/properties/b'],
      root_report(schema, { 'b' => 1 }).fetch('evaluatedProperties').fetch('b').map { |source| source.fetch('keywordLocation') })

    failing = root_report(schema, { 'c' => 1 })
    assert_equal(['c'], failing.fetch('remainingProperties'))
    source = failing.fetch('evaluatedProperties').fetch('c').first
    assert_equal('unevaluatedProperties', source.fetch('keyword'))
    assert_equal(false, source.fetch('valid'))

    [{ 'a' => 1 }, { 'b' => 1 }, { 'c' => 1 }, { 'a' => 1, 'b' => 2 }, {}].each do |instance|
      assert_equal(rejected_properties(schema, instance), root_report(schema, instance).fetch('remainingProperties'))
    end
  end

  def test_seemingly_matched_but_branch_fails_overall
    schema = {
      'anyOf' => [
        { 'properties' => { 'a' => { 'type' => 'string' } }, 'required' => ['a'] },
        { 'properties' => { 'b' => {} } }
      ],
      'unevaluatedProperties' => false
    }

    # branch 0 fails on `a`; its `properties` annotation does not count and `a`
    # is left for `unevaluatedProperties` to reject
    failing = root_report(schema, { 'a' => 1 })
    assert_equal(['a'], failing.fetch('remainingProperties'))
    source = failing.fetch('evaluatedProperties').fetch('a').find { |entry| entry.fetch('keyword') == 'properties' }
    assert_equal('/anyOf/0/properties/a', source.fetch('keywordLocation'))
    assert_equal(false, source.fetch('valid'))
    assert(failing.fetch('evaluatedProperties').fetch('a').any? { |entry| entry.fetch('keyword') == 'unevaluatedProperties' })

    # the successful sibling branch still supplies `b`
    mixed = root_report(schema, { 'a' => 1, 'b' => 2 })
    assert_equal(['a'], mixed.fetch('remainingProperties'))
    assert_equal(true, mixed.fetch('evaluatedProperties').fetch('b').first.fetch('valid'))

    [{ 'a' => 'x' }, { 'a' => 1 }, { 'a' => 1, 'b' => 2 }, { 'b' => 2 }, { 'z' => 9 }].each do |instance|
      assert_equal(rejected_properties(schema, instance), root_report(schema, instance).fetch('remainingProperties'))
    end
  end

  def test_pattern_and_additional_properties
    schema = {
      'properties' => { 'fixed' => { 'type' => 'integer' } },
      'patternProperties' => { '^x_' => { 'type' => 'integer' } },
      'additionalProperties' => false
    }

    valid = root_report(schema, { 'fixed' => 1, 'x_a' => 2 })
    assert_equal([], valid.fetch('remainingProperties'))
    assert_equal('/patternProperties/^x_',
      valid.fetch('evaluatedProperties').fetch('x_a').first.fetch('keywordLocation'))
    assert_equal('json-schemer://schema#/patternProperties/%5Ex_',
      valid.fetch('evaluatedProperties').fetch('x_a').first.fetch('absoluteKeywordLocation'))

    invalid = root_report(schema, { 'fixed' => 'n', 'x_a' => 's', 'other' => 3 })
    assert_equal(['fixed', 'other', 'x_a'], invalid.fetch('remainingProperties'))
    assert_equal(false, invalid.fetch('evaluatedProperties').fetch('fixed').first.fetch('valid'))
    assert_equal(false, invalid.fetch('evaluatedProperties').fetch('other').first.fetch('valid'))
  end

  def test_boolean_additional_properties_consumes_keys
    schema = {
      'properties' => { 'a' => {} },
      'additionalProperties' => true
    }
    result = root_report(schema, { 'a' => 1, 'z' => 2 })
    assert_equal([], result.fetch('remainingProperties'))
    assert_equal(true, result.fetch('evaluatedProperties').fetch('z').first.fetch('valid'))
  end

  def test_all_of_merges_branches
    schema = {
      'allOf' => [
        { 'properties' => { 'a' => {} } },
        { 'properties' => { 'b' => {} } }
      ],
      'unevaluatedProperties' => false
    }
    result = root_report(schema, { 'a' => 1, 'b' => 2, 'c' => 3 })
    assert_equal(%w[a b c], result.fetch('evaluatedProperties').keys.sort)
    assert_equal(['unevaluatedProperties'],
      result.fetch('evaluatedProperties').fetch('c').map { |source| source.fetch('keyword') })
    assert_equal(['c'], result.fetch('remainingProperties'))
  end

  def test_one_of_only_counts_unique_match
    schema = {
      'oneOf' => [
        { 'properties' => { 'a' => {} }, 'required' => ['a'] },
        { 'properties' => { 'b' => {} }, 'required' => ['b'] }
      ],
      'unevaluatedProperties' => false
    }
    assert_equal([], root_report(schema, { 'a' => 1 }).fetch('remainingProperties'))
  end

  def test_if_then_else_skips_unselected_branch
    schema = {
      'if' => { 'properties' => { 'x' => { 'const' => 1 } } },
      'then' => { 'properties' => { 'a' => {} } },
      'else' => { 'properties' => { 'b' => {} } },
      'unevaluatedProperties' => false
    }

    assert_equal(['b'], root_report(schema, { 'x' => 1, 'b' => 2 }).fetch('remainingProperties'))
    assert_equal(['a', 'x'], root_report(schema, { 'x' => 2, 'a' => 1 }).fetch('remainingProperties').sort)
  end

  def test_dependent_schemas
    schema = {
      'properties' => { 'a' => {} },
      'dependentSchemas' => { 'a' => { 'properties' => { 'b' => {} } } },
      'unevaluatedProperties' => false
    }

    assert_equal([], root_report(schema, { 'a' => 1, 'b' => 2 }).fetch('remainingProperties'))
    assert_equal(['b'], root_report(schema, { 'b' => 2 }).fetch('remainingProperties'))
  end

  def test_not_does_not_contribute
    schema = {
      'not' => { 'properties' => { 'a' => { 'const' => 1 } } },
      'unevaluatedProperties' => false
    }

    result = root_report(schema, { 'b' => 2 })
    refute(result.fetch('evaluatedProperties').key?('a'))
    assert_equal(['b'], result.fetch('remainingProperties'))
    assert_equal(['b'], rejected_properties(schema, { 'b' => 2 }))

    # a passing `not` subschema is an overall validation failure and still does
    # not contribute its properties
    not_result = root_report(schema, { 'a' => 1 })
    assert_equal(['a'], not_result.fetch('remainingProperties'))
  end

  def test_empty_schema_and_boolean_branches
    schema = {
      'properties' => { 'a' => true },
      'anyOf' => [true, { 'properties' => { 'b' => {} } }],
      'unevaluatedProperties' => true
    }

    result = root_report(schema, { 'a' => 1, 'b' => 2, 'c' => 3 })
    assert_equal(%w[a b c], result.fetch('evaluatedProperties').keys.sort)
    # `c` was evaluated only by `unevaluatedProperties`, so it stays remaining
    assert_equal(['c'], result.fetch('remainingProperties'))
    unevaluated = result.fetch('evaluatedProperties').fetch('c').first
    assert_equal('unevaluatedProperties', unevaluated.fetch('keyword'))
    assert_equal(true, unevaluated.fetch('valid'))

    # `additionalProperties: true` genuinely consumes its keys
    consuming = root_report(
      { 'properties' => { 'a' => {} }, 'additionalProperties' => true },
      { 'a' => 1, 'z' => 2 }
    )
    assert_equal([], consuming.fetch('remainingProperties'))
  end

  def test_non_object_additional_properties_schema
    schema = {
      'properties' => { 'a' => {} },
      'additionalProperties' => { 'type' => 'integer' }
    }

    result = root_report(schema, { 'a' => 1 })
    assert_equal([], result.fetch('remainingProperties'))
    assert_equal(true, result.fetch('evaluatedProperties').fetch('a').first.fetch('valid'))

    invalid = root_report(schema, { 'z' => 'x' })
    assert_equal(['z'], invalid.fetch('remainingProperties'))
    assert_equal(false, invalid.fetch('evaluatedProperties').fetch('z').first.fetch('valid'))

    accepted = root_report(schema, { 'z' => 3 })
    assert_equal([], accepted.fetch('remainingProperties'))
    assert_equal(true, accepted.fetch('evaluatedProperties').fetch('z').first.fetch('valid'))
  end

  def test_invalid_property_keyword_and_non_matching_locations
    schema = {
      'properties' => { 'a' => { 'type' => 'integer' } },
      'unevaluatedProperties' => false
    }

    # the `properties` result itself is invalid and its failing nested result
    # must not be traversed as a same-location subresult
    result = root_report(schema, { 'a' => 'x' })
    assert_equal(['a'], result.fetch('remainingProperties'))
    assert_equal(false, result.fetch('evaluatedProperties').fetch('a').first.fetch('valid'))

    # a nested object lives at a different location; its annotations must not
    # leak into the parent report
    nested_schema = {
      'properties' => {
        'child' => {
          'type' => 'object',
          'properties' => { 'n' => {} },
          'unevaluatedProperties' => false
        }
      },
      'unevaluatedProperties' => false
    }
    nested = root_report(nested_schema, { 'child' => { 'n' => 1 } })
    assert_equal([], nested.fetch('remainingProperties'))
  end

  def test_report_without_object_annotations
    # an object validated by a schema with no nested keyword results
    schema = true
    assert_equal([{
      'instanceLocation' => '',
      'evaluatedProperties' => {},
      'remainingProperties' => ['a']
    }], report(schema, { 'a' => 1 }))
  end

  def test_ref_source_locations
    schema = {
      '$ref' => '#/$defs/p',
      'unevaluatedProperties' => false,
      '$defs' => { 'p' => { 'properties' => { 'a' => {} } } }
    }

    valid = root_report(schema, { 'a' => 1 })
    source = valid.fetch('evaluatedProperties').fetch('a').first
    assert_equal('/$ref/properties/a', source.fetch('keywordLocation'))
    assert_equal('json-schemer://schema#/$defs/p/properties/a', source.fetch('absoluteKeywordLocation'))

    assert_equal(['b'], root_report(schema, { 'b' => 1 }).fetch('remainingProperties'))
  end

  def test_nested_objects
    schema = {
      'properties' => {
        'child' => {
          'type' => 'object',
          'properties' => { 'n' => {} },
          'unevaluatedProperties' => false
        }
      }
    }

    reports = report(schema, { 'child' => { 'n' => 1, 'm' => 2 } })
    assert_equal(['', '/child'], reports.map { |entry| entry.fetch('instanceLocation') })
    child = reports.last
    assert_equal(%w[m n], child.fetch('evaluatedProperties').keys.sort)
    assert_equal(['m'], child.fetch('remainingProperties'))
  end

  def test_non_object_instances_have_no_entries
    [1, 'str', [1, 2], true, nil].each do |instance|
      assert_equal([], report({ 'unevaluatedProperties' => false }, instance))
    end
  end

  def test_property_hooks_run_once
    calls = 0
    hook = proc { calls += 1 }
    schemer = JSONSchemer.schema(
      { 'properties' => { 'a' => {} }, 'unevaluatedProperties' => false },
      :after_property_validation => hook
    )
    schemer.validate({ 'a' => 1, 'b' => 2 }, :property_consumption_report => true)
    assert_equal(1, calls)
  end

  def test_custom_keywords_run_once
    calls = 0
    custom = proc do
      calls += 1
      true
    end
    schemer = JSONSchemer.schema(
      { 'properties' => { 'a' => {} }, 'x-report' => true, 'unevaluatedProperties' => false },
      :keywords => { 'x-report' => custom }
    )
    output = schemer.validate({ 'a' => 1 }, :property_consumption_report => true)
    assert_equal(1, calls)
    assert_equal(true, output.fetch('valid'))
  end

  def test_annotations_skip_not_and_unsuccessful_branches
    # `not` with a `properties` child must never contribute the child's
    # annotations, even though the child schema evaluates successfully.
    not_schema = {
      'not' => { 'properties' => { 'a' => {} } },
      'unevaluatedProperties' => false
    }
    not_output = JSONSchemer.schema(not_schema).validate(
      { 'a' => 1 },
      :property_consumption_report => true
    )
    not_root = not_output.fetch('propertyConsumptionReport').first
    assert_equal(['a'], not_root.fetch('remainingProperties'))
    assert_equal(['unevaluatedProperties'],
      not_root.fetch('evaluatedProperties').fetch('a').map { |source| source.fetch('keyword') })

    # an `anyOf` branch that fails overall (its `properties` result is invalid
    # for `a`) does not expose its annotations; the different-location nested
    # property result is skipped while traversing at the object location.
    anyof_schema = {
      'anyOf' => [
        { 'properties' => { 'a' => { 'type' => 'integer' } } },
        true
      ],
      'unevaluatedProperties' => false
    }
    anyof_output = JSONSchemer.schema(anyof_schema).validate(
      { 'a' => 'x' },
      :property_consumption_report => true
    )
    anyof_root = anyof_output.fetch('propertyConsumptionReport').first
    assert_equal(['a'], anyof_root.fetch('remainingProperties'))
  end

  def test_annotation_traversal_unconditional_property_results
    # an invalid `properties` result is yielded when it is the traversal entry
    tree = validate_tree(
      { 'properties' => { 'a' => { 'type' => 'integer' } } },
      { 'a' => 'x' }
    )
    invalid_properties = find_results(tree, PROPERTIES_CLASS).first
    assert_equal(false, invalid_properties.valid)
    assert_equal([invalid_properties], annotations_for(invalid_properties))

    # it is not reached through its invalid parent schema's same-location
    # nested results, so the invalid root traversal yields nothing
    assert_equal([], annotations_for(tree))

    # a valid root schema traverses into its valid same-location properties
    valid_tree = validate_tree({ 'properties' => { 'a' => {} } }, { 'a' => 1 })
    valid_properties = find_results(valid_tree, PROPERTIES_CLASS).first
    assert_equal([valid_properties], annotations_for(valid_tree))

    # a different-location nested result (the `/a` child) is not traversed
    child = valid_properties.nested.first
    assert_equal('/a', JSONSchemer::Location.resolve(child.instance_location))
    assert_equal([], annotations_for(child))

    # a `oneOf` that matches two branches is invalid while its two branch
    # subschemas are valid and at the same location
    ambiguous_tree = validate_tree(
      { 'oneOf' => [{ 'properties' => { 'a' => {} } }, true] },
      { 'a' => 1 }
    )
    oneof_class = JSONSchemer::Draft202012::Vocab::Applicator::OneOf
    oneof_result = find_results(ambiguous_tree, oneof_class).first
    assert_equal(false, oneof_result.valid)
    assert_equal([true, true], oneof_result.nested.map(&:valid))
    assert(oneof_result.nested.all? { |branch| branch.instance_location == oneof_result.instance_location })

    # a valid traversal node with an invalid same-location property result
    # among its children does not descend into the invalid child
    invalid_property_tree = validate_tree(
      { 'properties' => { 'a' => { 'type' => 'integer' } } },
      { 'a' => 'x' }
    )
    invalid_properties = find_results(invalid_property_tree, PROPERTIES_CLASS).first
    parent = fake_result(
      :source => Object.new,
      :valid => true,
      :nested => [invalid_properties],
      :location => invalid_properties.instance_location
    )
    assert_equal([], annotations_for(parent))

    # the valid sibling at the same location is still descended into
    valid_property_tree = validate_tree({ 'properties' => { 'b' => {} } }, { 'b' => 1 })
    valid_properties = find_results(valid_property_tree, PROPERTIES_CLASS).first
    # share the identical location node so identity-based comparison passes
    valid_sibling = JSONSchemer::Result.new(
      valid_properties.source,
      valid_properties.instance,
      invalid_properties.instance_location,
      valid_properties.keyword_location,
      true,
      valid_properties.nested,
      nil,
      valid_properties.annotation,
      nil,
      false,
      'annotations'
    )
    mixed_parent = fake_result(
      :source => Object.new,
      :valid => true,
      :nested => [invalid_properties, valid_sibling],
      :location => invalid_properties.instance_location
    )
    assert_equal([valid_sibling], annotations_for(mixed_parent))
  end

  def test_annotation_traversal_skips_not_and_leaf_results
    # `not` is a non-contributing applicator even with a successful child
    tree = validate_tree({ 'not' => { 'properties' => { 'z' => {} } } }, { 'z' => 1 })
    not_result = find_results(tree, NOT_CLASS).first
    assert_equal([], annotations_for(not_result))

    # a leaf keyword result with no nested results contributes nothing
    type_class = JSONSchemer::Draft202012::Vocab::Validation::Type
    invalid_tree = validate_tree(
      { 'properties' => { 'a' => { 'type' => 'integer' } } },
      { 'a' => 'x' }
    )
    type_result = find_results(invalid_tree, type_class).first
    assert_nil(type_result.nested)
    assert_equal([], annotations_for(type_result))

    # a properties result with no nested property results (no keys matched)
    empty_tree = validate_tree({ 'properties' => { 'z' => {} } }, { 'other' => 1 })
    properties = find_results(empty_tree, PROPERTIES_CLASS).first
    assert_nil(properties.nested&.first)
    assert_equal([properties], annotations_for(properties))
  end

  def test_report_build_private_helpers
    report_module = JSONSchemer::PropertyConsumptionReport
    location = JSONSchemer::Location.root

    # `walk` starts from a non-Schema keyword result
    walked = []
    keyword_root = fake_result(:source => Object.new, :valid => true)
    report_module.send(:walk, keyword_root) { |result| walked << result }
    assert_equal([], walked)

    # `collect_consumed` ignores invalid non-unevaluated results
    consumed = Set[]
    invalid_properties = find_results(
      validate_tree({ 'properties' => { 'a' => { 'type' => 'integer' } } }, { 'a' => 'x' }),
      PROPERTIES_CLASS
    ).first
    report_module.send(:collect_consumed, invalid_properties, consumed)
    assert_equal(Set[], consumed)

    # `collect_sources` on a result that is none of the property keywords
    sources = Hash.new { |hash, key| hash[key] = {} }
    not_class_result = find_results(
      validate_tree({ 'not' => { 'properties' => { 'z' => {} } } }, { 'z' => 1 }),
      NOT_CLASS
    ).first
    report_module.send(:collect_sources, not_class_result, sources)
    assert_empty(sources)

    # `add_source` falls back to the annotation result when no nested property
    # result matches (boolean `additionalProperties: true`)
    ap_class = JSONSchemer::Draft202012::Vocab::Applicator::AdditionalProperties
    ap_tree = validate_tree(
      { 'properties' => { 'a' => {} }, 'additionalProperties' => true },
      { 'a' => 1, 'z' => 2 }
    )
    ap_result = find_results(ap_tree, ap_class).first
    report_module.send(:add_source, ap_result, sources, 'z', ap_result.keyword_location)
    assert(sources.key?('z'))
    source = sources.fetch('z').values.first
    assert_equal('additionalProperties', source.fetch('keyword'))
    assert_equal(true, source.fetch('valid'))

    # an `unevaluatedProperties` result never adds to the consumed set
    up_class = JSONSchemer::Draft202012::Vocab::Unevaluated::UnevaluatedProperties
    consuming_tree = validate_tree(
      { 'unevaluatedProperties' => true },
      { 'z' => 1 }
    )
    consuming_up = find_results(consuming_tree, up_class).first
    assert(consuming_up.annotation.include?('z'))
    up_consumed = Set[]
    report_module.send(:collect_consumed, consuming_up, up_consumed)
    assert_equal(Set[], up_consumed)

    # `add_source` returns early for a property absent from the instance
    properties_result = find_results(
      validate_tree({ 'properties' => { 'a' => {}, 'missing' => {} } }, { 'a' => 1 }),
      PROPERTIES_CLASS
    ).first
    before = sources.keys
    report_module.send(
      :add_source,
      properties_result,
      sources,
      'missing',
      JSONSchemer::Location.join(properties_result.keyword_location, 'missing')
    )
    assert_equal(before, sources.keys)

    # `find_property_result` with a nil nested collection
    no_nested = fake_result(:source => ap_result.source, :valid => true, :nested => nil)
    found = report_module.send(:find_property_result, no_nested, 'z')
    assert_nil(found)

    # `add_source` falls back to the annotation result when no nested property
    # result is present
    fallback_sources = Hash.new { |hash, key| hash[key] = {} }
    instance_result = JSONSchemer::Result.new(
      ap_result.source,
      { 'z' => 2 },
      ap_result.instance_location,
      ap_result.keyword_location,
      true,
      nil,
      nil,
      ['z'],
      nil,
      false,
      'annotations'
    )
    report_module.send(
      :add_source,
      instance_result,
      fallback_sources,
      'z',
      ap_result.keyword_location
    )
    assert(fallback_sources.fetch('z').any?)
  end
end
