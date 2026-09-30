# frozen_string_literal: true

require 'test_helper'

class FilterSignatureTest < Minitest::Test
  module MoneyFilter
    def money(input, *args)
      options = args.last.is_a?(Hash) ? args.last : {}
      "#{input} #{options['currency']}"
    end

    alias_method :fmt, :money
  end

  module VarargsFilter
    def anything(input, *args)
      "#{input}:#{args.length}"
    end
  end

  def build_environment(positional: 0..0, currency: 'USD', aliases: [], error_mode: nil)
    Liquid::Environment.build(error_mode: error_mode) do |env|
      env.register_filter(MoneyFilter)
      env.register_filter(VarargsFilter)
      env.register_filter_signature('money',
        positional: positional,
        keywords: { currency: currency },
        aliases: aliases)
    end
  end

  def parse(template, environment)
    Liquid::Template.parse(template, environment: environment, line_numbers: true)
  end

  def test_valid_template_parses_and_renders
    environment = build_environment
    assert_equal("5 USD", parse("{{ 5 | money }}", environment).render!)
  end

  def test_too_many_positional_arguments_fail_at_parse_time
    environment = build_environment(positional: 0..1)
    error = assert_raises(Liquid::FilterSignatureError) do
      parse("{{ 5 | money: 1, 2 }}", environment)
    end
    assert_match("wrong number of arguments for filter 'money' (given 2, expected 0..1)", error.message)
  end

  def test_too_few_positional_arguments_fail_at_parse_time
    environment = build_environment(positional: 1..2)
    error = assert_raises(Liquid::FilterSignatureError) do
      parse("{{ 5 | money }}", environment)
    end
    assert_match("wrong number of arguments for filter 'money' (given 0, expected 1..2)", error.message)
  end

  def test_unknown_keyword_fails_at_parse_time
    environment = build_environment
    error = assert_raises(Liquid::FilterSignatureError) do
      parse("{{ 5 | money: curency: 'EUR' }}", environment)
    end
    assert_match("unknown keyword :curency for filter 'money' (valid keywords: :currency)", error.message)
  end

  def test_filter_without_keywords_rejects_keyword_arguments
    environment = Liquid::Environment.build do |env|
      env.register_filter_signature('money', positional: 0..0)
    end
    error = assert_raises(Liquid::FilterSignatureError) do
      parse("{{ 5 | money: currency: 'EUR' }}", environment)
    end
    assert_match("filter 'money' does not accept keyword arguments (given :currency)", error.message)
  end

  def test_parse_errors_are_raised_in_every_error_mode
    [:lax, :warn, :strict, :strict2].each do |error_mode|
      environment = build_environment(error_mode: error_mode)
      assert_raises(Liquid::FilterSignatureError) do
        parse("{{ 5 | money: 1 }}", environment)
      end
    end
  end

  def test_error_attributes_filter_in_chain_with_markup_context
    environment = build_environment
    error = assert_raises(Liquid::FilterSignatureError) do
      parse("{{ 5 | money: 1, 2 | upcase }}", environment)
    end
    assert_match("filter 'money'", error.message)
    assert_match('in "{{ 5 | money: 1, 2 | upcase }}"', error.to_s)
  end

  def test_alias_resolves_to_signature_and_is_named_in_errors
    environment = build_environment(aliases: ['fmt'])
    assert_equal("5 USD", parse("{{ 5 | fmt }}", environment).render!)

    error = assert_raises(Liquid::FilterSignatureError) do
      parse("{{ 5 | fmt: 1 }}", environment)
    end
    assert_match("wrong number of arguments for filter 'fmt'", error.message)
  end

  def test_exact_signature_takes_precedence_over_alias
    environment = Liquid::Environment.build do |env|
      env.register_filter_signature('money', positional: 0..0, aliases: ['fmt'])
      env.register_filter_signature('fmt', positional: 0..2)
    end
    assert_equal(0, environment.filter_signature_for('money').max_positional)
    assert_equal(2, environment.filter_signature_for('fmt').max_positional)
  end

  def test_keyword_defaults_are_applied_at_render_time
    environment = build_environment(currency: 'EUR')
    assert_equal("5 EUR", parse("{{ 5 | money }}", environment).render!)
  end

  def test_explicit_keyword_overrides_default
    environment = build_environment
    assert_equal("5 JPY", parse("{{ 5 | money: currency: 'JPY' }}", environment).render!)
  end

  def test_dynamic_keyword_values_are_supported
    environment = build_environment
    template = parse("{{ 5 | money: currency: unit }}", environment)
    assert_equal("5 GBP", template.render!('unit' => 'GBP'))
  end

  def test_filters_without_signature_keep_old_behavior
    environment = build_environment
    assert_equal("x:3", parse("{{ 'x' | anything: 1, 2, 3 }}", environment).render!)
  end

  def test_signatures_do_not_leak_between_environments
    usd = build_environment(positional: 0..0, currency: 'USD')
    eur = build_environment(positional: 0..2, currency: 'EUR')

    assert_equal("5 USD", parse("{{ 5 | money }}", usd).render!)
    assert_equal("5 EUR", parse("{{ 5 | money }}", eur).render!)

    assert_raises(Liquid::FilterSignatureError) do
      parse("{{ 5 | money: 1, 2 }}", usd)
    end
    template = parse("{{ 5 | money: 1, 2 }}", eur)
    assert_equal("5 EUR", template.render!)

    # The default environment is not affected either
    assert_nil(Liquid::Environment.default.filter_signature_for('money'))
  end

  def test_render_time_validation_uses_render_environment
    unsigned_environment = Liquid::Environment.build do |env|
      env.register_filter(MoneyFilter)
    end
    signed_environment = build_environment

    template = parse("{{ 5 | money: 1 }}", unsigned_environment)
    context = Liquid::Context.build(environment: signed_environment, rethrow_errors: true)
    error = assert_raises(Liquid::ArgumentError) do
      template.render(context)
    end
    assert_match("wrong number of arguments for filter 'money' (given 1, expected 0)", error.message)
  end

  def test_dynamic_invoke_is_validated_and_attributed
    environment = build_environment(positional: 0..1)
    context = Liquid::Context.build(environment: environment)

    error = assert_raises(Liquid::ArgumentError) do
      context.invoke('money', 5, 1, 2)
    end
    assert_match("wrong number of arguments for filter 'money' (given 2, expected 0..1)", error.message)

    error = assert_raises(Liquid::ArgumentError) do
      context.invoke('money', 5, { 'curency' => 'EUR' })
    end
    assert_match("unknown keyword :curency for filter 'money'", error.message)

    assert_equal("5 EUR", context.invoke('money', 5, { 'currency' => 'EUR' }))
  end

  def test_dynamic_invoke_treats_ambiguous_trailing_hash_leniently
    environment = Liquid::Environment.build do |env|
      env.register_filter(MoneyFilter)
      env.register_filter_signature('money', positional: 1..1, keywords: { currency: 'USD' })
    end
    context = Liquid::Context.build(environment: environment)

    # The trailing hash satisfies the single positional argument, so the
    # keyword reading must not produce a false positive.
    assert_equal("5 EUR", context.invoke('money', 5, { 'currency' => 'EUR' }))
  end

  def test_signature_registration_on_frozen_environment_raises
    environment = build_environment
    assert_raises(FrozenError) do
      environment.register_filter_signature('other', positional: 0..0)
    end
  end

  def test_duplicated_environment_does_not_share_signatures
    environment = build_environment
    copy = environment.dup
    copy.register_filter_signature('money', positional: 0..5)

    assert_equal(0, environment.filter_signature_for('money').max_positional)
    assert_equal(5, copy.filter_signature_for('money').max_positional)
  end

  def test_invalid_signature_definition_raises
    environment = Liquid::Environment.new
    assert_raises(::ArgumentError) do
      environment.register_filter_signature('money', positional: 2..1)
    end
    assert_raises(::ArgumentError) do
      environment.register_filter_signature('money', positional: 'a lot')
    end
  end

  def test_endless_positional_range
    environment = Liquid::Environment.build do |env|
      env.register_filter(VarargsFilter)
      env.register_filter_signature('anything', positional: 1..)
    end
    assert_raises(Liquid::FilterSignatureError) do
      parse("{{ 'x' | anything }}", environment)
    end
    assert_equal("x:2", parse("{{ 'x' | anything: 1, 2 }}", environment).render!)
  end
end
