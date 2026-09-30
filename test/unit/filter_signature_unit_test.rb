# frozen_string_literal: true

require 'test_helper'

class FilterSignatureUnitTest < Minitest::Test
  include Liquid

  module SignatureFilters
    def exact(input, *counts)
      ([input] + counts).join(':')
    end

    def range_filter(input, first, second = nil)
      [input, first, second].compact.join(':')
    end

    def named(input, options)
      "#{input}#{options['separator']}#{options['prefix']}#{options['separator']}#{options['suffix']}"
    end

    def aliased(input, count)
      "#{input}-#{count}"
    end
    alias_method :alias_name, :aliased
  end

  def test_validates_exact_positional_argument_count
    env = build_environment do |environment|
      environment.register_filter_signature(:exact, positional_arguments: 1)
    end

    assert_equal('input:1', render("{{ 'input' | exact: 1 }}", env))

    ['lax', 'strict', 'strict2'].each do |mode|
      error = assert_raises(FilterSignatureError) do
        Template.parse("{{ 'input' | exact }}", environment: env, error_mode: mode)
      end
      assert_match("given 0, expected 1", error.message)
    end
  end

  def test_validates_positional_argument_range
    env = build_environment do |environment|
      environment.register_filter_signature(:range_filter, positional_arguments: 1..2)
    end

    assert_equal('input:1', render("{{ 'input' | range_filter: 1 }}", env))
    assert_equal('input:1:2', render("{{ 'input' | range_filter: 1, dynamic }}", env, { 'dynamic' => 2 }))

    error = assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | range_filter: 1, 2, 3 }}", environment: env, error_mode: :strict2)
    end
    assert_match("given 3, expected 1 to 2", error.message)
  end

  def test_supports_minimum_and_maximum_positional_arguments
    hash_signature = FilterSignature.new(positional_arguments: { minimum: 1, maximum: 2 })
    array_signature = FilterSignature.new(positional_arguments: [1, 2])
    signature = hash_signature

    assert(signature.positional_arguments.include?(1))
    assert(signature.positional_arguments.include?(2))
    refute(signature.positional_arguments.include?(3))
    assert_equal(signature.positional_arguments, array_signature.positional_arguments)
  end

  def test_validates_named_arguments_and_injects_defaults
    env = build_environment do |environment|
      environment.register_filter_signature(
        :named,
        positional_arguments: 0,
        named_arguments: {
          'prefix' => FilterSignature::REQUIRED,
          'suffix' => FilterSignature::REQUIRED,
          'separator' => '/',
        },
      )
    end

    assert_equal('input/start/end', render("{{ 'input' | named: prefix: 'start', suffix: 'end' }}", env))
    assert_equal('input-start-end', render("{{ 'input' | named: prefix: 'start', suffix: 'end', separator: '-' }}", env))

    error = assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | named: prefix: 'start' }}", environment: env, error_mode: :strict2)
    end
    assert_match("missing required keyword argument \"suffix\"", error.message)

    error = assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | named: prefix: 'start', suffix: 'end', unknown: true }}", environment: env, error_mode: :strict2)
    end
    assert_match("unknown keyword argument \"unknown\"", error.message)
  end

  def test_signature_errors_are_attributed_in_lax_mode
    env = build_environment(error_mode: :lax) do |environment|
      environment.register_filter_signature(:exact, positional_arguments: 1)
    end

    error = assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | upcase | exact }}", environment: env, line_numbers: true)
    end

    assert_equal(1, error.line_number)
    assert_match(/for filter "exact"/, error.message)
    assert_match(/in "\{\{ 'input' \| upcase \| exact \}\}"/, error.message)
  end

  def test_accepts_required_named_arguments_as_an_array
    signature = FilterSignature.new(named_arguments: ['prefix', 'suffix'])

    error = assert_raises(FilterSignatureError) do
      signature.validate(:named, 0, {})
    end
    assert_match("missing required keyword argument \"prefix\"", error.message)
  end

  def test_resolves_ruby_filter_aliases
    env = build_environment do |environment|
      environment.register_filter_signature(:aliased, positional_arguments: 1)
    end

    assert_equal('input-2', render("{{ 'input' | alias_name: 2 }}", env))

    error = assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | alias_name }}", environment: env, error_mode: :strict2)
    end
    assert_match("for filter \"alias_name\"", error.message)
  end

  def test_supports_explicit_signature_aliases
    env = build_environment do |environment|
      environment.register_filter_signature(
        :aliased,
        positional_arguments: 1,
        named_arguments: { 'mode' => 'default' },
        aliases: :alias_name,
      )
    end

    signature = env.filter_signature(:alias_name)
    assert_same(signature, env.filter_signature(:aliased))
  end

  def test_filter_chain_reports_invalid_filter
    env = build_environment do |environment|
      environment.register_filter_signature(:exact, positional_arguments: 1)
    end

    error = assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | upcase | exact | downcase }}", environment: env, error_mode: :strict2)
    end
    assert_match(/for filter "exact"/, error.message)
    assert_match(/in "\{\{ 'input' \| upcase \| exact \| downcase \}\}"/, error.message)
  end

  def test_unsigned_filters_keep_existing_behavior
    env = Liquid::Environment.build(error_mode: :strict2)

    assert_equal('INPUT', render("{{ 'input' | upcase }}", env))
    assert_equal('input', render("{{ 'input' | missing_filter }}", env))
  end

  def test_signatures_are_scoped_to_environments
    first_environment = build_environment(error_mode: :strict2) do |environment|
      environment.register_filter_signature(:exact, positional_arguments: 1)
    end
    second_environment = build_environment(error_mode: :strict2) do |environment|
      environment.register_filter_signature(:exact, positional_arguments: 2)
    end

    assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | exact: 1, 2 }}", environment: first_environment)
    end
    assert_equal('input:1', render("{{ 'input' | exact: 1 }}", first_environment))

    assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | exact: 1 }}", environment: second_environment)
    end
    assert_equal('input:1:2', render("{{ 'input' | exact: 1, 2 }}", second_environment))
  end

  def test_duplicating_an_environment_copies_signatures
    environment = build_environment do |env|
      env.register_filter_signature(:exact, positional_arguments: 1)
    end
    duplicate = environment.dup

    duplicate.register_filter_signature(:exact, positional_arguments: 2)

    assert_raises(FilterSignatureError) do
      Template.parse("{{ 'input' | exact: 1, 2 }}", environment: environment)
    end
    assert_equal('input:1:2', render("{{ 'input' | exact: 1, 2 }}", duplicate))
  end

  def test_duplicate_default_values_are_not_shared
    env = build_environment do |environment|
      environment.register_filter_signature(:options_filter, named_arguments: { 'options' => {} })
    end

    filter = Module.new do
      def options_filter(input, options = {})
        options[:seen] = true
        "#{input}:#{options[:seen]}"
      end
    end
    env.register_filter(filter)
    env.register_filter_signature(:options_filter, named_arguments: { 'options' => {} })

    template = Template.parse("{{ 'input' | options_filter }}", environment: env, error_mode: :strict2)
    assert_equal('input:true', template.render!)
    assert_equal('input:true', template.render!)
  end

  private

  def build_environment(error_mode: :strict2, &block)
    Liquid::Environment.build(error_mode: error_mode) do |environment|
      environment.register_filter(SignatureFilters)
      block&.call(environment)
    end
  end

  def render(source, environment, assigns = {})
    Template.parse(source, environment: environment, error_mode: environment.error_mode).render!(assigns)
  end
end
