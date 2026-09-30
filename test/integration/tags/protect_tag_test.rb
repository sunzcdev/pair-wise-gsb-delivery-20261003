# frozen_string_literal: true

require 'test_helper'

class ProtectTagTest < Minitest::Test
  include Liquid

  class CustomLiquidError < Liquid::Error
  end

  class ProtectErrorDrop < Drop
    def standard_error
      raise Liquid::StandardError, 'recommendation unavailable'
    end

    def runtime_error
      raise 'process failure'
    end

    def exception
      raise Exception, 'fatal failure'
    end
  end

  class CustomErrorDrop < Drop
    def custom_error
      raise CustomLiquidError, 'custom failure'
    end
  end

  def test_renders_protected_block_when_successful
    assert_template_result(
      'prefix recommendation suffix',
      'prefix{% protect %} recommendation {% fallback %} fallback {% endprotect %}suffix',
    )
  end

  def test_discards_protected_output_and_renders_fallback_on_template_error
    template = 'before{% protect %}discarded{{ errors.standard_error }}{% fallback %}fallback{% endprotect %}after'

    assert_template_result(
      'beforefallbackafter',
      template,
      { 'errors' => ProtectErrorDrop.new },
      render_errors: true,
    )
  end

  def test_exposes_only_safe_error_diagnostics
    template = '{% protect %}{{ errors.standard_error }}{% fallback %}' \
      '{{ error.message }}|{{ error.type }}|{{ error.template_name }}|' \
      '{{ error.line_number }}|{{ error.backtrace }}{% endprotect %}'

    assert_template_result(
      'recommendation unavailable|standard_error||1|',
      template,
      { 'errors' => ProtectErrorDrop.new },
      render_errors: true,
    )
  end

  def test_unknown_error_type_uses_generic_diagnostic_type
    assert_template_result(
      'liquid',
      '{% protect %}{{ errors.custom_error }}{% fallback %}{{ error.type }}{% endprotect %}',
      { 'errors' => CustomErrorDrop.new },
      render_errors: true,
    )
  end

  def test_error_diagnostic_message_excludes_markup_context
    template = '{% protect %}{% render "broken" %}{% fallback %}{{ error.message }}{% endprotect %}'
    assert_template_result(
      'Expected id but found end_of_string',
      template,
      {},
      partials: { 'broken' => '{{ 1 | divided_by: 0 | }}' },
      render_errors: true,
      error_mode: :strict2,
    )
  end

  def test_handled_errors_are_not_added_to_template_errors
    template = Template.parse(
      '{% protect %}{{ errors.standard_error }}{% fallback %}fallback{% endprotect %}',
    )

    assert_equal('fallback', template.render({ 'errors' => ProtectErrorDrop.new }))
    assert_empty(template.errors)
  end

  def test_assignments_in_protected_and_fallback_blocks_do_not_leak
    template = '{% protect %}{% assign value = "primary" %}primary{{ errors.standard_error }}' \
      '{% fallback %}{% assign value = "fallback" %}fallback{% endprotect %}' \
      '{{ value | default: "outside" }}'

    assert_template_result('fallbackoutside', template, { 'errors' => ProtectErrorDrop.new }, render_errors: true)
  end

  def test_successful_block_assignments_do_not_leak
    assert_template_result(
      'primaryoutside',
      '{% protect %}{% assign value = "primary" %}primary{% fallback %}fallback{% endprotect %}' \
      '{{ value | default: "outside" }}',
    )
  end

  def test_block_assignments_shadow_outer_variables_only_inside_the_block
    template = '{% assign value = "old" %}' \
      '{% protect %}{% assign value = "new" %}{{ value }}{% fallback %}{{ value }}{% endprotect %}' \
      '{{ value }}'

    assert_template_result(
      'newold',
      template,
      { 'errors' => ProtectErrorDrop.new },
      render_errors: true,
    )
  end

  def test_nested_protect_uses_outer_fallback_when_both_fail
    template = '{% protect %}' \
      '{% protect %}{{ errors.standard_error }}{% fallback %}inner {{ errors.standard_error }}{% endprotect %}' \
      '{% fallback %}outer {{ error.type }}{% endprotect %}'

    assert_template_result(
      'outer standard_error',
      template,
      { 'errors' => ProtectErrorDrop.new },
      render_errors: true,
    )
  end

  def test_nested_protect_commits_inner_fallback_when_it_succeeds
    template = 'before-{% protect %}{{ errors.standard_error }}{% fallback %}inner-fallback{% endprotect %}-after'

    assert_template_result(
      'before-inner-fallback-after',
      template,
      { 'errors' => ProtectErrorDrop.new },
      render_errors: true,
    )
  end

  def test_partial_render_error_triggers_fallback
    template = '{% protect %}{% render "broken" %}{% fallback %}partial unavailable{% endprotect %}'

    assert_template_result(
      'partial unavailable',
      template,
      { 'errors' => ProtectErrorDrop.new },
      partials: { 'broken' => '{{ errors.standard_error }}' },
      render_errors: true,
    )
  end

  def test_partial_parse_error_exposes_partial_diagnostics
    template = '{% protect %}{% render "broken" %}{% fallback %}' \
      '{{ error.template_name }}:{{ error.line_number }}{% endprotect %}'

    assert_template_result(
      'broken:1',
      template,
      {},
      partials: { 'broken' => '{% if true %}' },
      render_errors: true,
    )
  end

  def test_non_liquid_errors_are_not_caught
    template = Template.parse(
      '{% protect %}{{ errors.runtime_error }}{% fallback %}fallback{% endprotect %}',
    )

    assert_raises(RuntimeError) do
      template.render!({ 'errors' => ProtectErrorDrop.new })
    end
  end

  def test_process_level_exceptions_are_not_caught
    template = Template.parse(
      '{% protect %}{{ errors.exception }}{% fallback %}fallback{% endprotect %}',
    )

    assert_raises(Exception) do
      template.render({ 'errors' => ProtectErrorDrop.new })
    end
  end

  def test_memory_errors_are_not_caught
    environment = Environment.build do |built_environment|
      built_environment.default_resource_limits = { render_length_limit: 1 }
    end
    template = Template.parse(
      '{% protect %}protected{% fallback %}fallback{% endprotect %}!',
      environment: environment,
    )

    assert_raises(MemoryError) do
      template.render!
    end
  end

  def test_consumed_resource_budget_is_not_refunded
    environment = Environment.build do |built_environment|
      built_environment.default_resource_limits = { cumulative_render_score_limit: 5 }
    end
    template = Template.parse(
      '{% protect %}{% if true %}a{% endif %}{{ errors.standard_error }}' \
      '{% fallback %}{% if true %}b{% endif %}{% endprotect %}',
      environment: environment,
    )

    assert_raises(MemoryError) do
      template.render!({ 'errors' => ProtectErrorDrop.new })
    end
  end

  def test_protect_requires_fallback
    assert_syntax_error('{% protect %}body{% endprotect %}')
  end

  def test_protect_does_not_accept_markup
    assert_syntax_error('{% protect error %}body{% fallback %}fallback{% endprotect %}')
  end

  def test_fallback_does_not_accept_markup
    assert_syntax_error('{% protect %}body{% fallback error %}fallback{% endprotect %}')
  end
end
