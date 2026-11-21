require_relative '../../helper'

require 'fluent/config'
require 'fluent/config/explain'
require 'fluent/plugin/out_stdout'
require 'fluent/plugin/in_forward'

class TestConfigExplainTimeParamExplainer < ::Test::Unit::TestCase
  def parse(conf_data, syntax: :v1)
    if syntax == :ruby
      Fluent::Config.parse(conf_data, 'dsl.rb', '(test_dir)', true, syntax: :ruby)
    else
      Fluent::Config.parse(conf_data, '(test)', '(test_dir)', true)
    end
  end

  def find(results, name, section_path = nil, kind = nil)
    results.find do |r|
      next false if r.respond_to?(:valid?) && !r.valid?
      r.source.name == name &&
        (section_path.nil? || r.source.section_path == section_path) &&
        (kind.nil? || r.source.kind == kind)
    end
  end

  def find_invalid(results, name)
    results.find { |r| r.respond_to?(:valid?) && !r.valid? && r.name.to_s == name }
  end

  sub_test_case 'inline buffer parameter (historical style without <buffer> section)' do
    test 'explains flush_interval written directly in the match directive' do
      conf = parse(<<~CONF)
        <match app.**>
          @type stdout
          flush_interval 30s
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      explanation = find(results, 'flush_interval', 'match app.**', :explicit)

      assert_not_nil explanation
      assert_equal '30s', explanation.raw_value
      assert_equal 's', explanation.unit
      assert_equal 30, explanation.seconds
      assert_equal 'Fluent::Plugin::StdoutOutput', explanation.source.plugin
    end

    test 'explains a bare-number inline value as unitless seconds' do
      conf = parse(<<~CONF)
        <match app.**>
          @type stdout
          flush_interval 45
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      explanation = find(results, 'flush_interval', 'match app.**', :explicit)

      assert_not_nil explanation
      assert_equal '45', explanation.raw_value
      assert_nil explanation.unit
      assert_true explanation.bare_number?
      assert_equal 45.0, explanation.seconds
    end

    test 'shows the value after environment substitution, with its original text' do
      ENV['FLUSH_INTERVAL_FOR_TEST'] = '90s'
      conf = parse(<<~CONF)
        <match app.**>
          @type stdout
          flush_interval "#{ENV['FLUSH_INTERVAL_FOR_TEST']}"
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      explanation = find(results, 'flush_interval', 'match app.**', :explicit)

      assert_not_nil explanation
      assert_equal '90s', explanation.raw_value
      assert_equal 's', explanation.unit
      assert_equal 90, explanation.seconds
    ensure
      ENV.delete('FLUSH_INTERVAL_FOR_TEST')
    end

    test 'the nested <buffer> section wins when flush_interval is written in both places' do
      conf = parse(<<~CONF)
        <match app.**>
          @type stdout
          flush_interval 30s
          <buffer>
            flush_interval 10s
          </buffer>
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      # The nested one is explained as explicit...
      nested = find(results, 'flush_interval', 'match app.** > buffer', :explicit)
      assert_equal '10s', nested.raw_value
      # ...and there is no duplicate inline explanation for the same parameter.
      inline = find(results, 'flush_interval', 'match app.**', :explicit)
      assert_nil inline
    end

    test 'keeps the inline value when a <buffer> section exists but does not name the parameter' do
      conf = parse(<<~CONF)
        <match app.**>
          @type stdout
          flush_interval 30s
          <buffer>
            retry_wait 5
          </buffer>
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)

      inline = find(results, 'flush_interval', 'match app.**', :explicit)
      assert_not_nil inline
      assert_equal '30s', inline.raw_value
      assert_equal 30, inline.seconds
    end
  end

  sub_test_case 'nested <buffer> section' do
    test 'explains explicit time parameters inside the buffer section' do
      conf = parse(<<~CONF)
        <match app.**>
          @type stdout
          <buffer>
            flush_interval 10s
            retry_wait 5
            retry_timeout 1h
          </buffer>
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)

      flush = find(results, 'flush_interval', 'match app.** > buffer', :explicit)
      assert_equal '10s', flush.raw_value
      assert_equal 's', flush.unit
      assert_equal 10, flush.seconds

      wait = find(results, 'retry_wait', 'match app.** > buffer', :explicit)
      assert_equal '5', wait.raw_value
      assert_nil wait.unit
      assert_equal 5.0, wait.seconds

      timeout = find(results, 'retry_timeout', 'match app.** > buffer', :explicit)
      assert_equal '1h', timeout.raw_value
      assert_equal 'h', timeout.unit
      assert_equal 3600, timeout.seconds
    end

    test 'explains plugin default time parameters with their numeric default' do
      conf = parse(<<~CONF)
        <match app.**>
          @type stdout
          <buffer>
          </buffer>
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      default_wait = find(results, 'retry_wait', 'match app.** > buffer', :default)

      assert_not_nil default_wait
      assert_equal '1', default_wait.raw_value
      assert_nil default_wait.unit
      assert_equal 1.0, default_wait.seconds
    end

    test 'explains time parameters on input plugins too' do
      conf = parse(<<~CONF)
        <source>
          @type forward
          blocking_timeout 2s
        </source>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      blocking = find(results, 'blocking_timeout', 'source', :explicit)

      assert_not_nil blocking
      assert_equal '2s', blocking.raw_value
      assert_equal 's', blocking.unit
      assert_equal 2, blocking.seconds
    end

    test 'reports full section paths through worker and label directives' do
      conf = parse(<<~CONF)
        <worker 0>
          <label @edge>
            <match app.**>
              @type stdout
              <buffer>
                flush_interval 15m
              </buffer>
            </match>
          </label>
        </worker>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      explanation = find(results, 'flush_interval')

      assert_not_nil explanation
      assert_equal 'worker 0 > label @edge > match app.** > buffer', explanation.source.section_path
      assert_equal 15 * 60, explanation.seconds
    end
  end

  sub_test_case 'invalid time unit' do
    test 'locates the offending parameter and raises ConfigError from explain' do
      conf = parse(<<~CONF)
        <match bad.**>
          @type stdout
          <buffer>
            flush_interval 10x
          </buffer>
        </match>
      CONF

      error = assert_raise(Fluent::ConfigError) do
        Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      end
      assert_match(/flush_interval/, error.message)
      assert_match(/10x/, error.message)
    end

    test 'returns an InvalidTimeValue with the exact section source from explain_with_errors' do
      conf = parse(<<~CONF)
        <match bad.**>
          @type stdout
          <buffer>
            flush_interval 10x
          </buffer>
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain_with_errors(conf)
      invalid = find_invalid(results, 'flush_interval')

      assert_not_nil invalid
      assert_false invalid.valid?
      assert_equal '10x', invalid.raw_value
      assert_equal :flush_interval, invalid.name
      assert_equal 'match bad.** > buffer', invalid.source.section_path
      assert_equal 'Fluent::Plugin::StdoutOutput', invalid.source.plugin
      assert_match(/invalid time value/, invalid.message)
    end

    test 'still explains the valid parameters next to the invalid one' do
      conf = parse(<<~CONF)
        <match bad.**>
          @type stdout
          <buffer>
            flush_interval 10x
            retry_timeout 1h
          </buffer>
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain_with_errors(conf)

      assert_not_nil find_invalid(results, 'flush_interval')
      assert_not_nil find(results, 'retry_timeout', 'match bad.** > buffer', :explicit)
    end

    test 'rejects uppercase and spelled-out units that Config.time_value silently reads as floats' do
      conf = parse(<<~CONF)
        <match bad.**>
          @type stdout
          flush_interval 10S
        </match>
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain_with_errors(conf)
      assert_not_nil find_invalid(results, 'flush_interval')
    end
  end

  sub_test_case 'plugin safety' do
    module ExplodeOnConfigure
      def configure(conf)
        raise 'output plugin was configured during explanation'
      end

      def start
        raise 'output plugin was started during explanation'
      end
    end

    test 'never calls configure or start on the output plugin' do
      Fluent::Plugin::StdoutOutput.prepend(ExplodeOnConfigure)

      conf = parse(<<~CONF)
        <match app.**>
          @type stdout
          flush_interval 10s
        </match>
      CONF

      assert_nothing_raised do
        Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)
      end
    end
  end

  sub_test_case 'ruby DSL configuration' do
    test 'explains inline and nested time parameters from .rb configs' do
      conf = parse(<<~CONF, syntax: :ruby)
        match "rb.**" do
          type "stdout"
          flush_interval "2h"
          buffer do
            retry_wait "30m"
          end
        end
      CONF

      results = Fluent::Config::Explain::TimeParamExplainer.new.explain(conf)

      retry_wait = find(results, 'retry_wait', 'match rb.** > buffer', :explicit)
      assert_equal '30m', retry_wait.raw_value
      assert_equal 'm', retry_wait.unit
      assert_equal 1800, retry_wait.seconds

      # flush_interval is inside a <buffer> block here, so it is not treated inline
      flush = find(results, 'flush_interval', 'match rb.** > buffer', :explicit)
      assert_nil flush
    end
  end
end
