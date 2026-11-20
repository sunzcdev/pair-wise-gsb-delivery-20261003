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

require 'fluent/event_router'
require 'fluent/plugin'

module Fluent
  # Read-only troubleshooting entry point for event routing.
  #
  # Given a tag, RouteInspector reports the <filter>/<match> declarations an
  # event with that tag would actually pass through, and why each declaration
  # was selected or skipped. Lookup uses the exact same order and semantics as
  # EventRouter#find, but:
  #
  #   * no plugin method (including #emit / #filter_stream) is ever called
  #   * the running EventRouter::MatchCache is only read, never written
  #
  # It can also cross-check the static declaration-based route against the
  # collector currently held in the in-memory route cache, which is useful to
  # detect a stale cache after configuration changes.
  class RouteInspector
    # @param event_router [Fluent::EventRouter]
    def initialize(event_router)
      @event_router = event_router
    end

    attr_reader :event_router

    # @return [Fluent::EventRouter::RouteTrace]
    def trace(tag)
      @event_router.trace_route(tag)
    end

    # @return [Fluent::EventRouter::CacheVerification]
    def verify(tag)
      @event_router.verify_cached_route(tag)
    end

    # Human-readable, multi-line rendering of a trace.
    # @return [String]
    def render(tag)
      result = trace(tag)
      lines = []
      lines << "route for tag: #{tag}"

      result.matched.each do |entry|
        lines << render_entry(entry, selected: true)
      end

      if result.matched.empty?
        lines << "  (no declaration matched the tag)"
      end

      result.skipped.each do |entry|
        lines << render_entry(entry, selected: false)
      end

      lines << render_terminal(result)
      lines.join("\n")
    end

    # Renders a cache cross-check result, appending the full trace.
    # @return [String]
    def render_verification(tag)
      verification = verify(tag)
      lines = []
      lines << "route cache check for tag: #{tag}"
      lines << "  cache: #{verification.status}"
      unless verification.status == :not_cached
        lines << "  cached collector: #{describe_plugin(verification.cached_collector)}"
      end
      lines << render(tag)
      lines.join("\n")
    end

    private

    def render_entry(entry, selected:)
      marker = selected ? '*' : '-'
      directive = entry.kind == :filter ? 'filter' : 'match'
      patterns = if entry.matched_patterns.empty?
                   entry.rule.pattern_str
                 else
                   entry.matched_patterns.join(' ')
                 end
      line = +"  #{marker} <#{directive} #{patterns}> -> #{describe_plugin(entry.rule.collector)}"
      line << "  [#{entry.reason}]"
      unless selected
        line << " (declared as <#{directive} #{entry.rule.pattern_str}>)"
      end
      line
    end

    def render_terminal(result)
      case result.terminal_reason
      when :first_matching_match
        "  => terminated by <match #{result.terminal_rule.pattern_str}>"
      when :default_collector_after_filters
        "  => no <match> after the matching filter(s); fallback to default collector: #{describe_plugin(result.default_collector)}"
      when :default_collector_no_match
        "  => no declaration matched; fallback to default collector: #{describe_plugin(result.default_collector)}"
      else
        "  => unknown terminal reason: #{result.terminal_reason}"
      end
    end

    def describe_plugin(plugin)
      if plugin.is_a?(Fluent::EventRouter::Pipeline)
        filters = plugin.filters.map { |filter| describe_plugin(filter) }.join(' -> ')
        output = plugin.output ? describe_plugin(plugin.output) : 'unset'
        return "pipeline[#{filters} -> #{output}]"
      end

      type = Fluent::Plugin.lookup_type_from_class(plugin.class) || plugin.class.name
      id = plugin.respond_to?(:plugin_id) ? plugin.plugin_id : 'unknown'
      "#{type}(#{id})"
    rescue => e
      "#{plugin.class.name}(unavailable: #{e.class})"
    end
  end
end
