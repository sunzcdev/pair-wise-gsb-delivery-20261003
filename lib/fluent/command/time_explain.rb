#
# Fluentd
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#

require 'optparse'
require 'json'

require 'fluent/env'
require 'fluent/version'
require 'fluent/config'
require 'fluent/config/explain'

module Fluent
  module Command
    # Explains time parameters (e.g. flush_interval, retry_wait) in a
    # configuration: the raw value written in the file, the unit that is
    # actually applied, the resulting seconds and the section the parameter
    # belongs to.
    #
    # The configuration is parsed and walked statically. Plugins are
    # instantiated only to read their parameter declarations; Plugin#configure
    # and Plugin#start are never invoked, so output plugins do not run.
    class TimeExplain
      AVAILABLE_FORMATS = [:txt, :json].freeze

      def initialize(argv = ARGV)
        @argv = argv
        @format = :txt
        @config_file_type = :guess
        @use_v1_config = true
        @libs = []
        @plugin_dirs = []
        prepare_option_parser
      end

      def call
        parse_options!
        init_libraries

        conf = build_config
        results = explain(conf)

        case @format
        when :json
          puts JSON.pretty_generate(results.map(&:to_h))
        else
          puts dump_txt(results)
        end

        invalid = results.count { |r| r.respond_to?(:valid?) && !r.valid? }
        exit 1 unless invalid.zero?
      rescue Fluent::ConfigError, Fluent::NotFoundPluginError, Errno::ENOENT => e
        warn "Error: #{e.message}"
        exit 1
      end

      private

      def build_config
        Fluent::Config.build(
          config_path: @config_path,
          use_v1_config: @use_v1_config,
          type: @config_file_type,
        )
      end

      def explain(conf)
        # Invalid time units are reported, not raised, so every problem in the
        # file can be located in a single run.
        Fluent::Config::Explain::TimeParamExplainer.new.explain_with_errors(conf)
      end

      def dump_txt(results)
        if results.empty?
          return "No time parameters found.\n"
        end

        width = results.map { |r| r.source.section_path.size }.max
        lines = results.map do |r|
          if r.respond_to?(:valid?) && !r.valid?
            "#{r.source.section_path.ljust(width)}  #{r.name}: #{r.raw_value.inspect}  INVALID (#{r.message})"
          else
            unit = r.unit.nil? ? '(no unit, seconds)' : r.unit
            "#{r.source.section_path.ljust(width)}  #{r.source.name}: raw=#{r.raw_value.inspect}  unit=#{unit}  seconds=#{r.seconds}  source=#{r.source.kind}"
          end
        end
        lines.join("\n") + "\n"
      end

      def usage(message = nil)
        puts @parser.to_s
        puts "Error: #{message}" if message
        exit 1
      end

      def prepare_option_parser
        @parser = OptionParser.new
        @parser.version = Fluent::VERSION
        @parser.banner = <<~BANNER
          Usage: #{$0} [options] CONFIG_PATH

          Explain time parameters in a Fluentd configuration without running plugins.

          Options:
        BANNER
        @parser.on('-f', '--format=FORMAT', "Output format: #{AVAILABLE_FORMATS.join(',')} (default: txt)") do |s|
          format = s.to_sym
          usage("Unsupported format: #{s}") unless AVAILABLE_FORMATS.include?(format)
          @format = format
        end
        @parser.on('--config-file-type VALUE', 'Config file type: yaml/yml or guess (default: guess)') do |s|
          if %w[yaml yml guess].include?(s)
            @config_file_type = s.to_sym
          else
            usage('--config-file-type accepts yaml/yml or guess')
          end
        end
        @parser.on('--use-v1-config', 'Use v1 configuration format (default)', TrueClass) { |b| @use_v1_config = b }
        @parser.on('--use-v0-config', 'Use v0 configuration format', TrueClass) { |b| @use_v1_config = !b }
        @parser.on('-I PATH', 'Add PATH to $LOAD_PATH') { |s| $LOAD_PATH.unshift(s) }
        @parser.on('-r NAME', 'Load library') { |s| @libs << s }
        @parser.on('-p', '--plugin=DIR', 'Add plugin directory') { |s| @plugin_dirs << s }
      end

      def parse_options!
        @parser.parse!(@argv)
        usage('Must specify a config path') if @argv.empty?
        usage('Only one config path can be specified') if @argv.size > 1
        @config_path = @argv.first
      rescue => e
        usage(e.message)
      end

      def init_libraries
        require 'fluent/plugin'
        @libs.each { |lib| require lib }
        @plugin_dirs.each do |dir|
          Fluent::Plugin.add_plugin_dir(File.expand_path(dir)) if Dir.exist?(dir)
        end
      end
    end
  end
end
