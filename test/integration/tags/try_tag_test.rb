# frozen_string_literal: true

require 'test_helper'

class TryTagTest < Minitest::Test
  include Liquid

  def test_commits_body_output_on_success
    assert_template_result(
      'before protected after',
      'before {% try %}protected{% endtry %} after',
    )
  end

  def test_fallback_renders_nothing_without_fallback_branch
    template = Liquid::Template.parse('before {% try %}{{ errors.standard_error }}{% endtry %} after')
    assert_equal('before  after', template.render('errors' => ErrorDrop.new))
  end

  def test_discards_partial_output_and_renders_fallback_on_error
    template = Liquid::Template.parse(
      'before {% try %}partial {{ errors.standard_error }} output{% fallback %}fallback{% endtry %} after',
    )
    assert_equal('before fallback after', template.render('errors' => ErrorDrop.new))
  end

  def test_error_is_still_recorded_in_template_errors
    template = Liquid::Template.parse('{% try %}{{ errors.standard_error }}{% fallback %}fallback{% endtry %}')
    assert_equal('fallback', template.render('errors' => ErrorDrop.new))
    assert_equal(1, template.errors.size)
    assert_equal(Liquid::StandardError, template.errors.first.class)
  end

  def test_catches_documented_template_exceptions
    template = Liquid::Template.parse('{% try %}{{ missing }}{% fallback %}fallback{% endtry %}')
    assert_equal('fallback', template.render({}, strict_variables: true))
  end

  def test_does_not_catch_process_level_exceptions
    template = Liquid::Template.parse('{% try %}{{ errors.exception }}{% fallback %}fallback{% endtry %}')
    assert_raises(Exception) do
      template.render('errors' => ErrorDrop.new)
    end
  end

  def test_does_not_catch_non_liquid_errors
    template = Liquid::Template.parse('{% try %}{{ errors.runtime_error }}{% fallback %}fallback{% endtry %}')
    # A non-Liquid error is not handled by the try tag; it propagates to the
    # surrounding document, which renders it as a normal template error.
    assert_equal('Liquid error: internal', template.render('errors' => ErrorDrop.new))
  end

  def test_does_not_catch_memory_errors_from_resource_limits
    template = Liquid::Template.parse(
      '{% try %}{% for a in (1..100) %}x{% endfor %}{% fallback %}fallback{% endtry %}',
    )
    template.resource_limits.render_score_limit = 50
    assert_equal('Liquid error: Memory limits exceeded', template.render)
    assert(template.resource_limits.reached?)
  end

  def test_resource_budget_consumed_by_discarded_output_is_not_rolled_back
    template = Liquid::Template.parse(
      "{% try %}{% assign x = 'aaaaaaaaaa' %}{{ errors.standard_error }}{% endtry %}{% assign y = 'bbbbbbbbbb' %}done",
    )
    template.resource_limits.assign_score_limit = 15
    # The discarded assign (score 10) still counts, so the second assign
    # pushes the total to 20 and exceeds the limit of 15.
    assert_equal('Liquid error: Memory limits exceeded', template.render('errors' => ErrorDrop.new))
  end

  def test_assignments_inside_try_do_not_leak_on_success
    assert_template_result(
      '[outer][]',
      "{% assign x = 'outer' %}{% try %}{% assign x = 'inner' %}{% assign y = 'inner' %}{% endtry %}[{{ x }}][{{ y }}]",
    )
  end

  def test_assignments_inside_try_do_not_leak_on_failure
    template = Liquid::Template.parse(
      "{% assign x = 'outer' %}{% try %}{% assign x = 'inner' %}{% assign y = 'inner' %}{{ errors.standard_error }}{% fallback %}f{% endtry %}[{{ x }}][{{ y }}]",
    )
    assert_equal('f[outer][]', template.render('errors' => ErrorDrop.new))
  end

  def test_captures_inside_try_do_not_leak
    assert_template_result(
      '[]',
      '{% try %}{% capture c %}inner{% endcapture %}{% endtry %}[{{ c }}]',
    )
  end

  def test_fallback_assignments_do_not_leak
    template = Liquid::Template.parse(
      "{% try %}{{ errors.standard_error }}{% fallback %}{% assign f = 'inner' %}f{% endtry %}[{{ f }}]",
    )
    assert_equal('f[]', template.render('errors' => ErrorDrop.new))
  end

  def test_nested_try_blocks
    template = Liquid::Template.parse(
      '{% try %}a{% try %}{{ errors.standard_error }}{% fallback %}inner{% endtry %}b{% endtry %}',
    )
    assert_equal('ainnerb', template.render('errors' => ErrorDrop.new))
  end

  def test_inner_try_without_fallback_recovers_and_outer_continues
    template = Liquid::Template.parse(
      '{% try %}a{% try %}{{ errors.standard_error }}{% endtry %}b{% fallback %}outer{% endtry %}',
    )
    assert_equal('ab', template.render('errors' => ErrorDrop.new))
  end

  def test_outer_try_catches_error_after_inner_try
    template = Liquid::Template.parse(
      '{% try %}a{% try %}{{ errors.standard_error }}{% endtry %}{{ errors.argument_error }}{% fallback %}outer{% endtry %}',
    )
    assert_equal('outer', template.render('errors' => ErrorDrop.new))
  end

  def test_error_inside_rendered_partial_triggers_fallback
    assert_template_result(
      'fallback',
      '{% try %}{% render "broken" %}{% fallback %}fallback{% endtry %}',
      { 'errors' => ErrorDrop.new },
      partials: { 'broken' => 'partial {{ errors.standard_error }} output' },
    )
  end

  def test_successful_partial_inside_try_commits_output
    assert_template_result(
      'partial output',
      '{% try %}{% render "fine" %}{% fallback %}fallback{% endtry %}',
      {},
      partials: { 'fine' => 'partial output' },
    )
  end

  def test_error_object_exposes_safe_diagnostics_to_fallback
    template = Liquid::Template.parse(
      '{% try %}{{ errors.standard_error }}{% fallback %}[{{ error.message }}|{{ error.type }}|{{ error.line_number }}]{% endtry %}',
      line_numbers: true,
    )
    assert_equal(
      '[Liquid error (line 1): standard error|Liquid::StandardError|1]',
      template.render('errors' => ErrorDrop.new),
    )
  end

  def test_error_object_does_not_expose_unsafe_details
    template = Liquid::Template.parse(
      '{% try %}{{ errors.standard_error }}{% fallback %}[{{ error.backtrace }}][{{ error.object_id }}]{% endtry %}',
    )
    assert_equal('[][]', template.render('errors' => ErrorDrop.new))
  end

  def test_break_interrupt_propagates_through_try
    assert_template_result(
      '1',
      '{% for i in (1..3) %}{% try %}{{ i }}{% break %}{% endtry %}{% endfor %}',
    )
  end

  def test_else_is_not_allowed_inside_try
    assert_match_syntax_error(/try tag does not expect 'else' tag/, '{% try %}{% else %}{% endtry %}')
  end

  def test_only_one_fallback_branch_is_allowed
    assert_match_syntax_error(
      /only one 'fallback' branch is allowed/,
      '{% try %}{% fallback %}{% fallback %}{% endtry %}',
    )
  end

  def test_try_tag_must_be_closed
    assert_match_syntax_error(/'try' tag was never closed/, '{% try %}...')
  end

  def test_fallback_outside_try_is_an_unknown_tag
    assert_match_syntax_error(/Unknown tag 'fallback'/, '{% fallback %}')
  end
end
