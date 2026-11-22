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

require 'fluent/plugin/base'

require 'set'

require 'fluent/event'
require 'fluent/log'
require 'fluent/plugin_id'
require 'fluent/plugin_helper'

module Fluent
  module Plugin
    class Filter < Base
      include PluginId
      include PluginLoggerMixin
      include PluginHelper::Mixin

      helpers_internal :event_emitter, :metrics

      # Result of processing a single record through one filter.
      #
      # A record always ends up in exactly one of these three dispositions,
      # which makes the fate of every record explicit even when a filter
      # chain fails halfway through a batch:
      #
      #   * +:passed+  - the record was transformed successfully and must be
      #                   handed to the next filter / business output.
      #   * +:dropped+ - the filter intentionally returned +nil+ (or a nil
      #                   time). The record is consumed and must not be
      #                   retried nor sent to the error route.
      #   * +:routed_to_error+ - processing raised an error. The record, as
      #                   it was *before* entering this filter, has been
      #                   delivered to the error route (+@ERROR+) exactly
      #                   once and must not be delivered again on retry.
      class FilterResult
        # +time+ and +record+ hold the (possibly transformed) event for
        # +:passed+, the pre-filter snapshot for +:routed_to_error+, and are
        # +nil+ for +:dropped+.
        attr_reader :disposition, :time, :record, :error

        def initialize(disposition, time = nil, record = nil, error = nil)
          @disposition = disposition
          @time = time
          @record = record
          @error = error
        end

        def passed?
          @disposition == :passed
        end

        def dropped?
          @disposition == :dropped
        end

        def routed_to_error?
          @disposition == :routed_to_error
        end

        def self.passed(time, record)
          new(:passed, time, record)
        end

        def self.dropped
          new(:dropped)
        end

        def self.routed_to_error(time, record, error)
          new(:routed_to_error, time, record, error)
        end
      end

      # Upper bound of error-route delivery keys remembered per plugin so
      # that retries cannot re-emit a record that was already delivered.
      ERROR_ROUTE_GUARD_MAX_SIZE = 4096

      attr_reader :has_filter_with_time

      def initialize
        super
        @has_filter_with_time = has_filter_with_time?
        @emit_records_metrics = nil
        @emit_size_metrics = nil
        @counter_mutex = Mutex.new
        @enable_size_metrics = false
        reset_error_route_guard
      end

      def configure(conf)
        super

        @emit_records_metrics = metrics_create(namespace: "fluentd", subsystem: "filter", name: "emit_records", help_text: "Number of count emit records")
        @emit_size_metrics = metrics_create(namespace: "fluentd", subsystem: "filter", name: "emit_size", help_text: "Total size of emit events")
        @enable_size_metrics = !!system_config.enable_size_metrics
      end

      def statistics
        stats = {
          'emit_records' => @emit_records_metrics.get,
          'emit_size' => @emit_size_metrics.get,
        }

        { 'filter' => stats }
      end

      def measure_metrics(es)
        @emit_records_metrics.add(es.size)
        @emit_size_metrics.add(es.to_msgpack_stream.bytesize) if @enable_size_metrics
      end

      def filter(tag, time, record)
        raise NotImplementedError, "BUG: filter plugins MUST implement this method"
      end

      def filter_with_time(tag, time, record)
        raise NotImplementedError, "BUG: filter plugins MUST implement this method"
      end

      def filter_stream(tag, es)
        new_es = MultiEventStream.new
        es.each do |time, record|
          result = filter_one(tag, time, record)
          new_es.add(result.time, result.record) if result.passed?
          # +:dropped+ and +:routed_to_error+ records are not forwarded to
          # the business output. Error routed ones were already delivered to
          # the error route inside #filter_one.
        end
        new_es
      end

      # Process a single record through this filter and always return a
      # FilterResult describing its disposition. This is the single place
      # implementing the filter fault-tolerance protocol; the optimized
      # filter chain (Fluent::EventRouter::Pipeline::FilterOptimizer) calls it
      # too so that both code paths behave identically.
      #
      # The plugin operates on a private working copy of the record. On
      # success the returned (possibly transformed) record is forwarded. On
      # failure a pristine *snapshot* taken before invoking
      # #filter/#filter_with_time is sent to the error route, so a record
      # mutated halfway through a filter is never reported in a partial
      # state and the source event is left untouched. Delivery is idempotent:
      # because the source (and the snapshot) stay clean, retrying the same
      # (tag, time, record) produces the same dedup key and is not emitted to
      # the error route a second time.
      def filter_one(tag, time, record)
        # Keep two independent copies:
        #   * +working_record+ is handed to the plugin; it may be mutated in
        #     place (including a partial mutation followed by an exception).
        #   * +snapshot_record+ is never passed to the plugin and stays
        #     pristine, so the error route receives the record as it was
        #     before this filter and the dedup key stays stable across retries.
        snapshot_time = time
        snapshot_record = record.dup
        working_record = record.dup
        if @has_filter_with_time
          begin
            filtered_time, filtered_record = filter_with_time(tag, time, working_record)
            if filtered_time && filtered_record
              FilterResult.passed(filtered_time, filtered_record)
            else
              FilterResult.dropped
            end
          rescue => e
            route_error_once(tag, snapshot_time, snapshot_record, e)
          end
        else
          begin
            filtered_record = filter(tag, time, working_record)
            if filtered_record
              FilterResult.passed(time, filtered_record)
            else
              FilterResult.dropped
            end
          rescue => e
            route_error_once(tag, snapshot_time, snapshot_record, e)
          end
        end
      end

      protected

      # Deliver a failed record to the error route at most once. The guard
      # keys on the exact event payload so that retrying a batch cannot
      # re-send records whose error event was already delivered, while
      # genuinely new records with identical content are still treated as
      # distinct events only when their time differs.
      def route_error_once(tag, time, record, error)
        key = error_route_key(tag, time, record)
        if @error_routed_keys.add?(key)
          router.emit_error_event(tag, time, record, error)
        else
          log.warn "suppress duplicate error event on retry", tag: tag, error: error
        end
        FilterResult.routed_to_error(time, record, error)
      ensure
        # Bound the memory used by the dedup guard.
        if @error_routed_keys.size > ERROR_ROUTE_GUARD_MAX_SIZE
          @error_routed_keys.clear
        end
      end

      def error_route_key(tag, time, record)
        [tag, time, record]
      end

      def reset_error_route_guard
        @error_routed_keys = Set.new
      end

      private

      def has_filter_with_time?
        implmented_methods = self.class.instance_methods(false)
        # Plugins that override `filter_stream` don't need check,
        # because they may not call `filter` or `filter_with_time`
        # for example fluentd/lib/fluent/plugin/filter_record_transformer.rb
        return nil if implmented_methods.include?(:filter_stream)
        case
        when [:filter, :filter_with_time].all? { |e| implmented_methods.include?(e) }
          raise "BUG: Filter plugins MUST implement either `filter` or `filter_with_time`"
        when implmented_methods.include?(:filter)
          false
        when implmented_methods.include?(:filter_with_time)
          true
        else
          raise NotImplementedError, "BUG: Filter plugins MUST implement either `filter` or `filter_with_time`"
        end
      end
    end
  end
end
