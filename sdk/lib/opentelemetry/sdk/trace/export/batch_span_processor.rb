# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'timeout'

module OpenTelemetry
  module SDK
    module Trace
      module Export
        # Implementation of the duck type SpanProcessor that batches spans
        # exported by the SDK then pushes them to the exporter pipeline.
        #
        # Typically, the BatchSpanProcessor will be more suitable for
        # production environments than the SimpleSpanProcessor.
        #
        # All spans reported by the SDK implementation are first added to a
        # synchronized queue (with a {max_queue_size} maximum size, after the
        # size is reached spans are dropped) and exported every
        # schedule_delay to the exporter pipeline in batches of
        # max_export_batch_size.
        #
        # If the queue gets half full a preemptive notification is sent to the
        # worker thread that exports the spans to wake up and start a new
        # export cycle.
        #
        # When one process hosts several logical services that share a single
        # processor, one high-volume service can otherwise evict every buffered
        # span of a quieter service once the queue is full. {group_reservations}
        # lets each service group reserve a minimum number of queue slots so a
        # noisy group cannot squeeze a quieter group out of the buffer. The
        # global {max_queue_size} cap never changes and reserved slots that a
        # group does not currently use stay available to the other groups, so
        # an idle group lends its space temporarily instead of leaving it
        # unused. All spans keep flowing through the same ordinary exporter
        # pipeline.
        class BatchSpanProcessor # rubocop:disable Metrics/ClassLength
          # Group label used for spans that do not map to a configured group
          # reservation. Using a single sentinel keeps the label values bounded
          # to the configured keys plus this constant.
          DEFAULT_GROUP = 'default'

          # A buffered span paired with the service group it belongs to. The
          # span itself is unchanged and is exported through the ordinary
          # exporter path.
          Entry = Struct.new(:span, :group) do
            # @return [Object] the export-facing span data of the wrapped span
            def to_span_data
              span.to_span_data
            end
          end

          private_constant(:DEFAULT_GROUP, :Entry)

          # Returns a new instance of the {BatchSpanProcessor}.
          #
          # @param [SpanExporter] exporter the (duck type) SpanExporter to where the
          #   recorded Spans are pushed after batching.
          # @param [Numeric] exporter_timeout the maximum allowed time to export data.
          #   Defaults to the value of the OTEL_BSP_EXPORT_TIMEOUT
          #   environment variable, if set, or 30,000 (30 seconds).
          # @param [Numeric] schedule_delay the delay interval between two consecutive exports.
          #   Defaults to the value of the OTEL_BSP_SCHEDULE_DELAY environment
          #   variable, if set, or 5,000 (5 seconds).
          # @param [Integer] max_queue_size the maximum queue size in spans.
          #   Defaults to the value of the OTEL_BSP_MAX_QUEUE_SIZE environment
          #   variable, if set, or 2048.
          # @param [Integer] max_export_batch_size the maximum batch size in spans.
          #   Defaults to the value of the OTEL_BSP_MAX_EXPORT_BATCH_SIZE environment
          #   variable, if set, or 512.
          # @param [Hash{String, Symbol => Integer}] group_reservations optional
          #   per-service-group minimum share of the queue, mapping a group key
          #   to the minimum number of slots that group is guaranteed while the
          #   queue is full. Values must be non-negative integers and their sum
          #   must not exceed {max_queue_size}. Slots a group does not use stay
          #   borrowable by the other groups, and the global {max_queue_size} cap
          #   is unchanged. Defaults to an empty hash, which reproduces the
          #   original first-in-first-out eviction behavior.
          # @param [#call, nil] group_by optional callable that maps a finished
          #   span to one of the keys in {group_reservations}. Returning +nil+,
          #   an unconfigured key, or raising an error assigns the span to the
          #   {DEFAULT_GROUP} group. Defaults to +nil+, in which case a span's
          #   group is taken from its resource's +service.name+ attribute, so a
          #   process hosting several services is partitioned by service
          #   automatically.
          #
          # @return a new instance of the {BatchSpanProcessor}.
          def initialize(exporter,
                         exporter_timeout: Float(ENV.fetch('OTEL_BSP_EXPORT_TIMEOUT', 30_000)),
                         schedule_delay: Float(ENV.fetch('OTEL_BSP_SCHEDULE_DELAY', 5_000)),
                         max_queue_size: Integer(ENV.fetch('OTEL_BSP_MAX_QUEUE_SIZE', 2048)),
                         max_export_batch_size: Integer(ENV.fetch('OTEL_BSP_MAX_EXPORT_BATCH_SIZE', 512)),
                         start_thread_on_boot: String(ENV.fetch('OTEL_RUBY_BSP_START_THREAD_ON_BOOT', nil)) !~ /false/i,
                         group_reservations: {},
                         group_by: nil,
                         metrics_reporter: nil)
            raise ArgumentError if max_export_batch_size > max_queue_size
            raise ArgumentError, "exporter #{exporter.inspect} does not appear to be a valid exporter" unless Common::Utilities.valid_exporter?(exporter)

            configure(
              exporter: exporter,
              exporter_timeout: exporter_timeout,
              schedule_delay: schedule_delay,
              max_queue_size: max_queue_size,
              max_export_batch_size: max_export_batch_size,
              group_reservations: group_reservations,
              group_by: group_by,
              metrics_reporter: metrics_reporter
            )
            reset_on_fork(restart_thread: start_thread_on_boot)
          end

          # Does nothing for this processor
          def on_start(_span, _parent_context); end

          # Adds a span to the batch. Thread-safe; may block on lock.
          def on_finish(span)
            return unless span.context.trace_flags.sampled?

            # Resolve the group outside the lock so user-supplied callables
            # cannot block other producers.
            group = resolve_group(span)
            lock do
              reset_on_fork
              accept(Entry.new(span, group), function: __method__.to_s)
              @condition.signal if spans.size > batch_size
            end
          end

          # Export all ended spans to the configured `Exporter` that have not yet
          # been exported.
          #
          # This method should only be called in cases where it is absolutely
          # necessary, such as when using some FaaS providers that may suspend
          # the process after an invocation, but before the `Processor` exports
          # the completed spans.
          #
          # @param [optional Numeric] timeout An optional timeout in seconds.
          # @return [Integer] SUCCESS if no error occurred, FAILURE if a
          #   non-specific failure occurred, TIMEOUT if a timeout occurred.
          def force_flush(timeout: nil) # rubocop:disable Metrics/MethodLength
            start_time = OpenTelemetry::Common::Utilities.timeout_timestamp
            snapshot = lock do
              reset_on_fork(restart_thread: @keep_running)
              take_entries
            end
            until snapshot.empty?
              remaining_timeout = OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time)
              return TIMEOUT if remaining_timeout&.zero?

              batch = snapshot.shift(@batch_size)
              result_code = export_batch(batch, timeout: remaining_timeout)
              return result_code unless result_code == SUCCESS
            end

            @exporter.force_flush(timeout: OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time))
          ensure
            # Unshift the remaining spans if we timed out. We drop excess spans from
            # the snapshot because they're older than any spans in the spans buffer.
            lock do
              unless snapshot.empty?
                overflow = spans.size + snapshot.size - max_queue_size
                if overflow.positive?
                  dropped_entries = snapshot.shift(overflow)
                  report_dropped_spans(dropped_entries, reason: 'buffer-full', function: __method__.to_s)
                end
                spans.unshift(*snapshot)
                snapshot.each { |entry| @group_counts[entry.group] += 1 }
              end
              @condition.signal if spans.size > max_queue_size / 2
            end
          end

          # Shuts the consumer thread down and flushes the current accumulated buffer
          # will block until the thread is finished.
          #
          # @param [optional Numeric] timeout An optional timeout in seconds.
          # @return [Integer] SUCCESS if no error occurred, FAILURE if a
          #   non-specific failure occurred, TIMEOUT if a timeout occurred.
          def shutdown(timeout: nil)
            start_time = OpenTelemetry::Common::Utilities.timeout_timestamp
            thread = lock do
              @keep_running = false
              @condition.signal
              @thread
            end

            thread&.join(timeout)
            force_flush(timeout: OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time))
            dropped_entries = lock { take_entries }
            report_dropped_spans(dropped_entries, reason: 'terminating') if dropped_entries.any?
            @exporter.shutdown(timeout: OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time))
          end

          private

          attr_reader :spans, :max_queue_size, :batch_size

          def configure(exporter:, exporter_timeout:, schedule_delay:, max_queue_size:,
                        max_export_batch_size:, group_reservations:, group_by:, metrics_reporter:)
            @exporter = exporter
            @exporter_timeout_seconds = exporter_timeout / 1000.0
            @mutex = Mutex.new
            @export_mutex = Mutex.new
            @condition = ConditionVariable.new
            @keep_running = true
            @delay_seconds = schedule_delay / 1000.0
            @max_queue_size = max_queue_size
            @batch_size = max_export_batch_size
            @group_reservations = validate_group_reservations(group_reservations, max_queue_size)
            @group_by = validate_group_by(group_by)
            @group_counts = Hash.new(0)
            @metrics_reporter = metrics_reporter || OpenTelemetry::SDK::Trace::Export::MetricsReporter
            @spans = []
            @pid = nil
            @thread = nil
          end

          def work
            loop do
              batch = lock do
                @condition.wait(@mutex, @delay_seconds) if spans.size < batch_size && @keep_running
                @condition.wait(@mutex, @delay_seconds) while spans.empty? && @keep_running
                return unless @keep_running

                take_entries(batch_size)
              end

              @metrics_reporter.observe_value('otel.bsp.buffer_utilization', value: spans.size / max_queue_size.to_f)

              export_batch(batch)
            end
          end

          def reset_on_fork(restart_thread: true)
            pid = Process.pid
            return if @pid == pid

            @pid = pid
            spans.clear
            @group_counts.clear
            @thread = restart_thread ? Thread.new { work } : nil
          rescue ThreadError => e
            @metrics_reporter.add_to_counter('otel.bsp.error', labels: { 'reason' => 'ThreadError' })
            OpenTelemetry.handle_error(exception: e, message: 'unexpected error in BatchSpanProcessor#reset_on_fork')
          end

          def export_batch(span_array, timeout: @exporter_timeout_seconds)
            batch = span_array.map(&:to_span_data)
            result_code = @export_mutex.synchronize { @exporter.export(batch, timeout: timeout) }
            report_result(result_code, span_array)
            result_code
          rescue StandardError => e
            report_result(FAILURE, span_array)
            @metrics_reporter.add_to_counter('otel.bsp.error', labels: { 'reason' => e.class.to_s })
            FAILURE
          end

          def report_result(result_code, span_array)
            if result_code == SUCCESS
              @metrics_reporter.add_to_counter('otel.bsp.export.success')
              @metrics_reporter.add_to_counter('otel.bsp.exported_spans', increment: span_array.size)
            else
              OpenTelemetry.handle_error(exception: ExportError.new(span_array.map(&:span)))
              @metrics_reporter.add_to_counter('otel.bsp.export.failure')
              report_dropped_spans(span_array, reason: 'export-failure')
            end
          end

          # Ensures the incoming entry fits within the global queue cap while
          # honouring each group's minimum share.
          #
          # A group may borrow space above its reservation while the queue has
          # room. Once the queue is full, a slot is reclaimed as follows:
          #
          # * If the incoming group is below its reservation, it reclaims a slot
          #   from the oldest entry of a group that is above its own reservation
          #   (i.e. one that borrowed idle space). No protected share is lost.
          # * Otherwise the incoming group is at or above its reservation, so it
          #   rotates out one of its own oldest entries. It therefore sheds its
          #   own excess instead of sacrificing data other groups had accepted.
          #   When the group has nothing buffered yet (e.g. an unconfigured
          #   group), it falls back to reclaiming borrowed space from an
          #   over-reservation group.
          # * If no slot can be reclaimed, the incoming entry is dropped.
          #
          # In every case each group keeps at least its reservation, unused
          # reserved space stays borrowable, and the global {max_queue_size}
          # cap is unchanged.
          def accept(entry, function:)
            if spans.size < max_queue_size
              push_entry(entry)
              return
            end

            victim_index = eviction_index_for(entry.group)
            if victim_index
              report_dropped_spans([remove_entry_at(victim_index)], reason: 'buffer-full', function: function)
              push_entry(entry)
            else
              report_dropped_spans([entry], reason: 'buffer-full', function: function)
            end
          end

          def eviction_index_for(group)
            return over_reserved_index if @group_counts[group] < reservation_for(group)

            spans.index { |entry| entry.group == group } || over_reserved_index
          end

          # Index of the oldest entry whose group holds more than its
          # reservation, i.e. one that is only borrowing idle space and can be
          # reclaimed without violating its guaranteed minimum.
          def over_reserved_index
            spans.index { |entry| @group_counts[entry.group] > reservation_for(entry.group) }
          end

          def reservation_for(group)
            @group_reservations.fetch(group, 0)
          end

          def push_entry(entry)
            spans << entry
            @group_counts[entry.group] += 1
          end

          def remove_entry_at(index)
            entry = spans.delete_at(index)
            @group_counts[entry.group] -= 1
            entry
          end

          # Removes up to +limit+ entries from the front of the queue and keeps
          # the per-group occupancy in sync.
          def take_entries(limit = spans.size)
            taken = spans.shift(limit)
            taken.each { |entry| @group_counts[entry.group] -= 1 }
            taken
          end

          def resolve_group(span)
            return DEFAULT_GROUP if @group_reservations.empty?

            raw = @group_by ? @group_by.call(span) : service_group(span)
            return DEFAULT_GROUP if raw.nil? || raw.to_s.empty?

            group = raw.to_s
            @group_reservations.key?(group) ? group : DEFAULT_GROUP
          rescue StandardError => e
            OpenTelemetry.handle_error(exception: e, message: 'unexpected error in BatchSpanProcessor group_by')
            DEFAULT_GROUP
          end

          # Returns the span's service name from its resource, which is the
          # natural group key when one process hosts several services and no
          # explicit +group_by+ was supplied.
          def service_group(span)
            resource = span.resource if span.respond_to?(:resource)
            return DEFAULT_GROUP unless resource.respond_to?(:attribute_enumerator)

            resource.attribute_enumerator.each do |key, value|
              return value if key == OpenTelemetry::SemanticConventions::Resource::SERVICE_NAME
            end
            DEFAULT_GROUP
          end

          def validate_group_reservations(group_reservations, max_queue_size)
            raise ArgumentError, 'group_reservations must be a Hash' unless group_reservations.is_a?(Hash)

            normalized = group_reservations.each_with_object({}) do |(key, reservation), memo|
              raise ArgumentError, 'group reservations must be non-negative integers' unless reservation.is_a?(Integer) && reservation >= 0

              memo[key.to_s] = reservation
            end
            raise ArgumentError, 'group reservations must not exceed max_queue_size in total' if normalized.values.sum > max_queue_size

            normalized
          end

          def validate_group_by(group_by)
            return nil if group_by.nil?
            raise ArgumentError, 'group_by must respond to #call' unless group_by.respond_to?(:call)

            group_by
          end

          # Reports dropped entries using bounded labels: the group is one of
          # the configured reservation keys (or {DEFAULT_GROUP}), and the reason
          # is one of the fixed reasons used by the processor.
          def report_dropped_spans(entries, reason:, function: nil)
            return if entries.empty?

            entries.group_by(&:group).each do |group, grouped_entries|
              @metrics_reporter.add_to_counter(
                'otel.bsp.dropped_spans',
                increment: grouped_entries.size,
                labels: {
                  'group' => group,
                  'reason' => reason,
                  OpenTelemetry::SemanticConventions::Trace::CODE_FUNCTION => function
                }
              )
            end
          end

          def lock
            # rubocop:disable-next Style/ExplicitBlockArgument
            @mutex.synchronize do
              yield
            end
          end
        end
      end
    end
  end
end
