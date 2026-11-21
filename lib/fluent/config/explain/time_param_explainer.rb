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

require 'fluent/config'
require 'fluent/plugin'
require 'fluent/config/explain/time_explanation'
require 'fluent/config/explain/invalid_time_value'
require 'fluent/config/explain/param_source'

module Fluent
  module Config
    module Explain
      # Walks a parsed configuration tree and explains every parameter
      # declared as :time in the owning plugin's configure proxy.
      #
      # Plugins are instantiated only to read their merged configure proxy
      # (class-level config_param declarations). Plugin#configure / #start are
      # never called, so output plugins are not executed: no buffer, network
      # connection or timer is created.
      class TimeParamExplainer
        PLUGIN_DIRECTIVES = {
          'source' => :input,
          'filter' => :filter,
          'match'  => :output,
        }.freeze

        # Directives that merely group plugin directives and do not own
        # parameters themselves.
        GROUP_DIRECTIVES = %w[worker label].freeze

        # Time parameters that may be written directly inside a plugin
        # directive even though their config_param declaration lives in the
        # <buffer> section. Modern Fluentd only keeps "flush_interval" working
        # in this inline position; v0.12 buffered outputs accepted the whole
        # buffer parameter set there.
        INLINE_BUFFER_TIME_PARAMS = [:flush_interval].freeze

        def initialize
          @explanations = []
          @errors = []
        end

        # @param config [Fluent::Config::Element] parsed root element
        # @return [Array<TimeExplanation>]
        def explain(config)
          @explanations = []
          @errors = []
          walk_group(config, [])
          raise_collected_error unless @errors.empty?
          @explanations
        end

        # Same as #explain but never raises on invalid time values. Invalid
        # entries are returned alongside valid explanations.
        #
        # @return [Array] TimeExplanation objects and InvalidTimeValue objects
        def explain_with_errors(config)
          @explanations = []
          @errors = []
          walk_group(config, [])
          @explanations + @errors
        end

        private

        def raise_collected_error
          message = @errors.map(&:message).join("\n")
          raise Fluent::ConfigError, message
        end

        def directive_label(element)
          if element.arg.nil? || element.arg.empty?
            element.name
          else
            "#{element.name} #{element.arg}"
          end
        end

        # Traverses grouping directives (ROOT, worker, label) down to plugin
        # directives.
        def walk_group(element, section_stack)
          element.elements.each do |child|
            if PLUGIN_DIRECTIVES.key?(child.name)
              walk_plugin(child, section_stack)
            elsif GROUP_DIRECTIVES.include?(child.name)
              walk_group(child, section_stack + [directive_label(child)])
            end
            # unknown directives are ignored: a real fluentd run reports them
            # separately via check_not_fetched.
          end
        end

        def walk_plugin(element, section_stack)
          kind = PLUGIN_DIRECTIVES[element.name]
          plugin = instantiate_plugin(kind, element)
          return unless plugin

          proxy = plugin.class.merged_configure_proxy
          plugin_label = plugin.class.name
          stack = section_stack + [directive_label(element)]

          explain_params(element, proxy, stack, plugin_label)
          explain_inline_buffer_params(element, proxy, stack, plugin_label)
          walk_sections(element, proxy, stack, plugin_label)
        end

        # Explains buffer time parameters written directly in the plugin
        # directive (no <buffer> section), the historical inline style. When
        # the same parameter is declared inside a real <buffer> section, that
        # section owns the value, so the inline spelling is only considered
        # when no nested element defines it.
        def explain_inline_buffer_params(element, proxy, section_stack, plugin_label)
          buffer_proxy = proxy.sections[:buffer]
          return unless buffer_proxy

          nested_elements = element.elements(name: 'buffer')

          INLINE_BUFFER_TIME_PARAMS.each do |name|
            next unless buffer_proxy.params.has_key?(name)
            next unless element.has_key?(name.to_s)
            next if nested_elements.any? { |buffer_element| buffer_element.has_key?(name.to_s) }

            source = ParamSource.new(:explicit, section_stack, plugin_label, name.to_s)
            build_explanation(element[name.to_s], name, source)
          end
        end

        def instantiate_plugin(kind, element)
          type = element['@type'] || element['type']
          if type.nil? || type.empty?
            # Mirrors StaticConfigAnalysis, but keep the explainer usable on
            # partial configs: skip directives without a type instead of
            # aborting the whole walk.
            return nil
          end
          Fluent::Plugin.__send__("new_#{kind}", type)
        end

        # Recurses into config_section declarations (e.g. <buffer>, <format>),
        # matching nested elements to the proxy that declares their params.
        def walk_sections(element, proxy, section_stack, plugin_label)
          proxy.sections.each_pair do |section_name, subproxy|
            element.elements(name: section_name.to_s).each do |sub_element|
              stack = section_stack + [directive_label(sub_element)]
              explain_params(sub_element, subproxy, stack, plugin_label)
              walk_sections(sub_element, subproxy, stack, plugin_label)
            end
          end
        end

        def explain_params(element, proxy, section_stack, plugin_label)
          time_param_names = proxy.params.each_pair.select do |_name, (_block, opts)|
            opts[:type] == :time
          end.map(&:first)

          time_param_names.each do |name|
            key = name.to_s
            if element.has_key?(key) || aliased_key?(element, proxy, name)
              real_key = element.has_key?(key) ? key : proxy.params[name][1][:alias].to_s
              explain_explicit(element, real_key, name, proxy, section_stack, plugin_label)
            else
              explain_default(name, proxy, section_stack, plugin_label)
            end
          end
        end

        def aliased_key?(element, proxy, name)
          opts = proxy.params[name][1]
          opts[:alias] && element.has_key?(opts[:alias].to_s)
        end

        def explain_explicit(element, real_key, name, proxy, section_stack, plugin_label)
          raw_value = element[real_key]
          # The parser stores the :default sentinel when a value is written as
          # "param default". It resolves to the plugin default, so explain it
          # exactly like an omitted parameter.
          if raw_value == :default
            explain_default(name, proxy, section_stack, plugin_label)
            return
          end
          return if raw_value.nil?

          source = ParamSource.new(:explicit, section_stack, plugin_label, real_key)
          build_explanation(raw_value, name, source)
        end

        def explain_default(name, proxy, section_stack, plugin_label)
          return unless proxy.defaults.has_key?(name)

          default = proxy.defaults[name]
          return if default.nil?

          source = ParamSource.new(:default, section_stack, plugin_label, name.to_s)
          # Defaults are stored as numeric seconds. Render them the same way a
          # bare-number config value would be read: no unit, seconds.
          build_explanation(default.to_s, name, source)
        end

        def build_explanation(raw_value, name, source)
          explanation = TimeExplanation.explain(raw_value, name: name, source: source)
          @explanations << explanation
        rescue Fluent::ConfigError => e
          @errors << InvalidTimeValue.new(raw_value, name, source, e.message)
        end
      end
    end
  end
end
