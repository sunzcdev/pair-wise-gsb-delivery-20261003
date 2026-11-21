require_relative '../../helper'

require 'fluent/config/explain/time_explanation'
require 'fluent/config/explain/param_source'

class TestConfigExplainTimeExplanation < ::Test::Unit::TestCase
  def source
    Fluent::Config::Explain::ParamSource.new(
      :explicit, ['match test.**', 'buffer'], 'Fluent::Plugin::StdoutOutput', 'flush_interval'
    )
  end

  sub_test_case '.explain' do
    data(
      'seconds unit'  => ['10s', 's', 10],
      'minutes unit'  => ['2m',  'm', 120],
      'hours unit'    => ['3h',  'h', 10800],
      'days unit'     => ['4d',  'd', 345600],
      'integer bare'  => ['60',  nil, 60.0],
      'float bare'    => ['0.5', nil, 0.5],
      'zero'          => ['0',   nil, 0.0],
      'negative bare' => ['-5',  nil, -5.0],
    )
    test 'explains valid values with the same result as Config.time_value' do |(raw, unit, seconds)|
      explanation = Fluent::Config::Explain::TimeExplanation.explain(raw, name: 'flush_interval', source: source)
      assert_equal raw, explanation.raw_value
      assert_equal unit, explanation.unit
      assert_equal seconds, explanation.seconds
      assert_equal Fluent::Config.time_value(raw), explanation.seconds
    end

    test 'accepts numeric input (e.g. values produced by YAML configs)' do
      explanation = Fluent::Config::Explain::TimeExplanation.explain(60, name: 'flush_interval', source: source)
      assert_equal '60', explanation.raw_value
      assert_nil explanation.unit
      assert_equal 60.0, explanation.seconds
      assert_true explanation.bare_number?
      assert_false explanation.explicit_unit?
    end

    test 'accepts frozen strings without mutating them' do
      raw = '10s'.freeze
      explanation = Fluent::Config::Explain::TimeExplanation.explain(raw, name: 'flush_interval', source: source)
      assert_equal '10s', explanation.raw_value
      assert_equal 's', explanation.unit
      assert_equal 10, explanation.seconds
    end

    test 'keeps the attached source' do
      explanation = Fluent::Config::Explain::TimeExplanation.explain('10s', name: 'flush_interval', source: source)
      assert_equal 'match test.** > buffer', explanation.source.section_path
      assert_equal :explicit, explanation.source.kind
      assert_equal 'Fluent::Plugin::StdoutOutput', explanation.source.plugin
    end

    data(
      'unknown suffix'        => '10x',
      'uppercase unit'        => '10S',
      'spelled-out unit'      => '10sec',
      'fractional with unit'  => '1.5s',
      'non numeric text'      => 'soon',
      'empty string'          => '',
    )
    test 'raises ConfigError with the parameter name for values Config.time_value would silently misparse' do |raw|
      error = assert_raise(Fluent::ConfigError) do
        Fluent::Config::Explain::TimeExplanation.explain(raw, name: 'flush_interval', source: source)
      end
      assert_match(/flush_interval/, error.message)
      assert_match(/#{Regexp.escape(raw.inspect)}/, error.message)
    end

    test 'mentions the accepted units in the error message so the bad value can be fixed' do
      error = assert_raise(Fluent::ConfigError) do
        Fluent::Config::Explain::TimeExplanation.explain('10x', name: 'flush_interval', source: source)
      end
      assert_match(/s\/m\/h\/d/, error.message)
    end

    test 'works without a parameter name' do
      error = assert_raise(Fluent::ConfigError) do
        Fluent::Config::Explain::TimeExplanation.explain('10x', source: source)
      end
      assert_match(/time parameter/, error.message)
    end
  end

  sub_test_case '#to_h' do
    test 'serializes raw value, unit, seconds and source' do
      explanation = Fluent::Config::Explain::TimeExplanation.explain('2m', name: 'flush_interval', source: source)
      expected = {
        'raw_value' => '2m',
        'unit' => 'm',
        'seconds' => 120,
        'source' => {
          'kind' => 'explicit',
          'section' => ['match test.**', 'buffer'],
          'section_path' => 'match test.** > buffer',
          'plugin' => 'Fluent::Plugin::StdoutOutput',
          'name' => 'flush_interval',
        },
      }
      assert_equal expected, explanation.to_h
    end
  end
end
