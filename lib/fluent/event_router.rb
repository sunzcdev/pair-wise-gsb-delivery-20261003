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

require 'fluent/match'
require 'fluent/event'
require 'fluent/filter'
require 'fluent/msgpack_factory'

module Fluent
  #
  # EventRouter is responsible to route events to a collector.
  #
  # It has a list of MatchPattern and Collector pairs:
  #
  #  +----------------+     +-----------------+
  #  |  MatchPattern  |     |    Collector    |
  #  +----------------+     +-----------------+
  #  |   access.**  ---------> type forward   |
  #  |     logs.**  ---------> type copy      |
  #  |  archive.**  ---------> type s3        |
  #  +----------------+     +-----------------+
  #
  # EventRouter does:
  #
  # 1) receive an event at `#emit` methods
  # 2) match the event's tag with the MatchPatterns
  # 3) forward the event to the corresponding Collector
  #
  # Collector is either of Output, Filter or other EventRouter.
  #
  class EventRouter
    def initialize(default_collector, emit_error_handler)
      @match_rules = []
      @match_cache = MatchCache.new
      @default_collector = default_collector
      @emit_error_handler = emit_error_handler
      @metric_callbacks = {}
      @caller_plugin_id = nil
    end

    attr_accessor :default_collector
    attr_accessor :emit_error_handler

    class Rule
      def initialize(pattern, collector)
        @pattern_strs = pattern.split(/\s+/)
        @patterns = @pattern_strs.map { |str| MatchPattern.create(str) }
        @pattern = if @patterns.length == 1
                     @patterns[0]
                   else
                     OrMatchPattern.new(@patterns)
                   end
        @pattern_str = pattern
        @collector = collector
      end

      def match?(tag)
        @pattern.match(tag)
      end

      # Returns the raw pattern strings that actually match the given tag.
      # A rule declared with space-separated patterns (OrMatchPattern) may match
      # with one or more of its alternatives. Read-only: pattern matching only.
      def matched_patterns(tag)
        @patterns.each_index.select { |i| @patterns[i].match(tag) }.map { |i|
          @pattern_strs[i]
        }
      end

      def filter?
        @collector.is_a?(Plugin::Filter)
      end

      attr_reader :collector
      attr_reader :pattern_str
    end

    def suppress_missing_match!
      if @default_collector.respond_to?(:suppress_missing_match!)
        @default_collector.suppress_missing_match!
      end
    end

    # called by Agent to add new match pattern and collector
    def add_rule(pattern, collector)
      @match_rules << Rule.new(pattern, collector)
    end

    def add_metric_callbacks(caller_plugin_id, callback)
      @metric_callbacks[caller_plugin_id] = callback
    end

    def caller_plugin_id=(caller_plugin_id)
      @caller_plugin_id = caller_plugin_id
    end

    def find_callback
      if @caller_plugin_id
        @metric_callbacks[@caller_plugin_id]
      else
        nil
      end
    end

    def emit(tag, time, record)
      unless record.nil?
        emit_stream(tag, OneEventStream.new(time, record))
      end
    end

    def emit_array(tag, array)
      emit_stream(tag, ArrayEventStream.new(array))
    end

    def emit_stream(tag, es)
      match(tag).emit_events(tag, es)
      if callback = find_callback
        callback.call(es)
      end
    rescue Pipeline::OutputError => e
      @emit_error_handler.handle_emits_error(tag, e.processed_es, e.internal_error)
    rescue => e
      @emit_error_handler.handle_emits_error(tag, es, e)
    end

    def emit_error_event(tag, time, record, error)
      @emit_error_handler.emit_error_event(tag, time, record, error)
    end

    def match?(tag)
      !!find(tag)
    end

    def match(tag)
      collector = @match_cache.get(tag) {
        find(tag) || @default_collector
      }
      collector
    end

    class MatchCache
      MATCH_CACHE_SIZE = 1024

      def initialize
        super
        @map = {}
        @keys = []
      end

      def get(key)
        if collector = @map[key]
          return collector
        end
        collector = @map[key] = yield
        if @keys.size >= MATCH_CACHE_SIZE
          # expire the oldest key
          @map.delete @keys.shift
        end
        @keys << key
        collector
      end

      # Read-only lookup for diagnostics. Returns nil when the key is not
      # cached and never yields, so the running route cache is not mutated.
      def peek(key)
        @map[key]
      end

      def size
        @map.size
      end
    end

    # One declaration (<filter> / <match>) evaluated while tracing a tag.
    # +rule_index+        preserves declaration order, which is the EventRouter lookup order.
    # +rule+              the registered EventRouter::Rule (pattern string + collector plugin).
    # +matched_patterns+  raw pattern strings that matched the tag (more than one for "a b" style rules).
    # +kind+              :filter or :output.
    # +reason+            why this declaration was selected or skipped.
    RuleTrace = Struct.new(:rule_index, :rule, :matched_patterns, :kind, :reason)

    # Result of a read-only route lookup. It describes the same chain that
    # EventRouter#find would build, but never instantiates a Pipeline and never
    # touches plugin #emit / the match cache.
    # +terminal_reason+ explains how the end of the chain was chosen:
    #   :first_matching_match             - a <match> declaration terminated the lookup
    #   :default_collector_after_filters  - only <filter> declarations matched; fallback collector is used
    #   :default_collector_no_match       - nothing matched; fallback collector is used
    RouteTrace = Struct.new(:tag, :matched, :skipped, :terminal_rule, :default_collector, :terminal_reason) do
      def filter_entries
        matched.select { |entry| entry.kind == :filter }
      end

      def filter_plugins
        filter_entries.map { |entry| entry.rule.collector }
      end

      # The output that actually receives events: the terminal <match> plugin,
      # or the default collector when no <match> declaration terminates lookup.
      def output_plugin
        terminal_rule ? terminal_rule.collector : default_collector
      end
    end

    # Result of cross-checking a static trace against the live MatchCache entry.
    # +status+ is one of:
    #   :not_cached  - the tag has never been emitted, so there is no cache entry to compare
    #   :consistent  - cached collector and traced route describe the same chain
    #   :mismatch    - cached collector differs from the route the current declarations produce
    CacheVerification = Struct.new(:tag, :status, :cached_collector, :trace) do
      def consistent?
        status == :consistent
      end

      def cached?
        status != :not_cached
      end
    end

    # Read-only access to the declared rules, in registration (lookup) order.
    attr_reader :match_rules

    # Read-only diagnostic entry point.
    #
    # Walks @match_rules in exactly the same order as EventRouter#find and
    # records why each declaration was selected or skipped. It only calls
    # MatchPattern#match? - no plugin method is invoked and @match_cache is
    # neither read nor written, so running routing is unaffected.
    def trace_route(tag)
      matched = []
      skipped = []
      terminal_rule = nil

      @match_rules.each_with_index do |rule, index|
        kind = rule.filter? ? :filter : :output

        if terminal_rule
          # EventRouter#find stops at the first matching non-filter rule,
          # so declarations after it are never evaluated.
          skipped << RuleTrace.new(index, rule, [], kind, :not_evaluated_terminal_already_selected)
          next
        end

        if rule.match?(tag)
          patterns = rule.matched_patterns(tag)
          if rule.filter?
            matched << RuleTrace.new(index, rule, patterns, kind, :matching_filter)
          else
            # Mirrors `find`: the first matching non-filter collector terminates lookup.
            terminal_rule = rule
            matched << RuleTrace.new(index, rule, patterns, kind, :first_matching_match)
          end
        else
          skipped << RuleTrace.new(index, rule, [], kind, :pattern_does_not_match)
        end
      end

      terminal_reason = if terminal_rule
                          :first_matching_match
                        elsif matched.any?
                          :default_collector_after_filters
                        else
                          :default_collector_no_match
                        end

      RouteTrace.new(tag, matched, skipped, terminal_rule, @default_collector, terminal_reason)
    end

    # Cross-checks the route that the current declarations produce for +tag+
    # against the collector currently held in the live match cache.
    #
    # The cache is inspected with MatchCache#peek (no block, no mutation), and
    # no event is emitted. A :not_cached result means the tag has not been
    # emitted yet; :mismatch means the in-memory route was built from older
    # declarations (e.g. before a config reload) and the cache is stale.
    def verify_cached_route(tag)
      trace = trace_route(tag)
      cached = @match_cache.peek(tag)
      status = if cached.nil?
                 :not_cached
               elsif same_route?(cached, trace)
                 :consistent
               else
                 :mismatch
               end
      CacheVerification.new(tag, status, cached, trace)
    end

    private

    def same_route?(cached_collector, trace)
      filter_plugins = trace.filter_plugins
      terminal_collector = trace.output_plugin

      if filter_plugins.empty?
        # EventRouter#find returns the output (or default collector) directly.
        return cached_collector.equal?(terminal_collector)
      end

      # With filters, EventRouter#find returns a Pipeline instance. Compare its
      # chain structurally by plugin identity instead of object identity,
      # because tracing must not construct (or cache) a Pipeline of its own.
      return false unless cached_collector.is_a?(Pipeline)

      cached_collector.filters.size == filter_plugins.size &&
        cached_collector.filters.zip(filter_plugins).all? { |cached_filter, traced_filter|
          cached_filter.equal?(traced_filter)
        } &&
        cached_collector.output.equal?(terminal_collector)
    end

    class Pipeline

      class OutputError < StandardError
        attr_reader :internal_error
        attr_reader :processed_es

        def initialize(internal_error, processed_es)
          @internal_error = internal_error
          @processed_es = processed_es
        end
      end

      def initialize
        @filters = []
        @output = nil
        @optimizer = FilterOptimizer.new
      end

      attr_reader :filters
      attr_reader :output

      def add_filter(filter)
        @filters << filter
        @optimizer.filters = @filters
      end

      def set_output(output)
        @output = output
      end

      def emit_events(tag, es)
        processed = @optimizer.filter_stream(tag, es)

        begin
          @output.emit_events(tag, processed)
        rescue => e
          raise OutputError.new(e, processed)
        end
      end

      class FilterOptimizer
        def initialize(filters = [])
          @filters = filters
          @optimizable = nil
        end

        def filters=(filters)
          @filters = filters
          reset_optimization
        end

        def filter_stream(tag, es)
          if optimizable?
            optimized_filter_stream(tag, es)
          else
            @filters.reduce(es) { |acc, filter|
              filtered_es = filter.filter_stream(tag, acc)
              filter.measure_metrics(filtered_es)
              filtered_es
            }
          end
        end

        private

        def optimized_filter_stream(tag, es)
          new_es = MultiEventStream.new
          es.each(unpacker: Fluent::MessagePackFactory.thread_local_msgpack_unpacker) do |time, record|
            filtered_record = record
            filtered_time = time

            catch :break_loop do
              @filters.each do |filter|
                if filter.has_filter_with_time
                  begin
                    filtered_time, filtered_record = filter.filter_with_time(tag, filtered_time, filtered_record)
                    throw :break_loop unless filtered_record && filtered_time
                    filter.measure_metrics(OneEventStream.new(time, record))
                  rescue => e
                    filter.router.emit_error_event(tag, filtered_time, filtered_record, e)
                  end
                else
                  begin
                    filtered_record = filter.filter(tag, filtered_time, filtered_record)
                    throw :break_loop unless filtered_record
                    filter.measure_metrics(OneEventStream.new(time, record))
                  rescue => e
                    filter.router.emit_error_event(tag, filtered_time, filtered_record, e)
                  end
                end
              end

              new_es.add(filtered_time, filtered_record)
            end
          end
          new_es
        end

        def optimizable?
          return @optimizable unless @optimizable.nil?
          fs_filters = filters_having_filter_stream
          @optimizable = if fs_filters.empty?
                           true
                         else
                           # skip log message when filter is only 1, because its performance is same as non optimized chain.
                           if @filters.size > 1 && fs_filters.size >= 1
                             $log.info "disable filter chain optimization because #{fs_filters.map(&:class)} uses `#filter_stream` method."
                           end
                           false
                         end
        end

        def filters_having_filter_stream
          @filters_having_filter_stream ||= @filters.select do |filter|
            filter.class.instance_methods(false).include?(:filter_stream)
          end
        end

        def reset_optimization
          @optimizable = nil
          @filters_having_filter_stream = nil
        end
      end
    end

    def find(tag)
      pipeline = nil
      @match_rules.each do |rule|
        if rule.match?(tag)
          if rule.collector.is_a?(Plugin::Filter)
            pipeline ||= Pipeline.new
            pipeline.add_filter(rule.collector)
          else
            if pipeline
              pipeline.set_output(rule.collector)
            else
              # Use Output directly when filter is not matched
              pipeline = rule.collector
            end
            return pipeline
          end
        end
      end

      if pipeline
        # filter is matched but no match
        pipeline.set_output(@default_collector)
        pipeline
      else
        nil
      end
    end
  end
end
