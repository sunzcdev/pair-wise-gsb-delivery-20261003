require_relative '../../helper'

require 'fluent/config/explain/param_source'
require 'fluent/config/explain/invalid_time_value'

class TestConfigExplainSources < ::Test::Unit::TestCase
  sub_test_case Fluent::Config::Explain::ParamSource do
    def build(kind)
      Fluent::Config::Explain::ParamSource.new(
        kind, ['match app.**', 'buffer'], 'Fluent::Plugin::StdoutOutput', 'flush_interval'
      )
    end

    test 'distinguishes explicit values from defaults' do
      assert_true build(:explicit).explicit?
      assert_false build(:explicit).default?
      assert_true build(:default).default?
      assert_false build(:default).explicit?
    end

    test 'joins the directive stack into a section path' do
      assert_equal 'match app.** > buffer', build(:explicit).section_path
    end

    test 'serializes to a hash for JSON output' do
      expected = {
        'kind' => 'explicit',
        'section' => ['match app.**', 'buffer'],
        'section_path' => 'match app.** > buffer',
        'plugin' => 'Fluent::Plugin::StdoutOutput',
        'name' => 'flush_interval',
      }
      assert_equal expected, build(:explicit).to_h
    end
  end

  sub_test_case Fluent::Config::Explain::InvalidTimeValue do
    test 'is never valid and keeps the source and error message' do
      source = Fluent::Config::Explain::ParamSource.new(
        :explicit, ['match bad.**', 'buffer'], 'Fluent::Plugin::StdoutOutput', 'flush_interval'
      )
      invalid = Fluent::Config::Explain::InvalidTimeValue.new(
        '10x', :flush_interval, source, 'boom'
      )

      assert_false invalid.valid?
      assert_equal '10x', invalid.raw_value
      assert_equal :flush_interval, invalid.name
      assert_equal 'boom', invalid.message
      assert_equal 'match bad.** > buffer', invalid.source.section_path
    end

    test 'serializes the error together with its location' do
      source = Fluent::Config::Explain::ParamSource.new(
        :explicit, ['match bad.**', 'buffer'], 'Fluent::Plugin::StdoutOutput', 'flush_interval'
      )
      invalid = Fluent::Config::Explain::InvalidTimeValue.new('10x', :flush_interval, source, 'boom')
      hash = invalid.to_h

      assert_equal '10x', hash['raw_value']
      assert_equal 'boom', hash['error']
      assert_equal 'match bad.** > buffer', hash.dig('source', 'section_path')
    end
  end
end
