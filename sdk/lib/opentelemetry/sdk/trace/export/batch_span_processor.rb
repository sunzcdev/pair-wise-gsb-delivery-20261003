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
        # Exporters may return {SUCCESS} or {FAILURE} to describe the whole
        # batch, or an {ExportResult} to describe the outcome of each span. In
        # the latter case only the spans the exporter marked as retryable are
        # sent again (up to {MAX_EXPORT_ATTEMPTS} attempts), spans marked as
        # rejected are discarded without a retry, and the two kinds of loss are
        # reported separately.
        class BatchSpanProcessor # rubocop:disable Metrics/ClassLength
          # The maximum number of times a batch, or the retryable subset of a
          # partially accepted batch, is sent to the exporter before the spans
          # that were not accepted are dropped as an export failure.
          MAX_EXPORT_ATTEMPTS = 3

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
          #
          # @return a new instance of the {BatchSpanProcessor}.
          def initialize(exporter,
                         exporter_timeout: Float(ENV.fetch('OTEL_BSP_EXPORT_TIMEOUT', 30_000)),
                         schedule_delay: Float(ENV.fetch('OTEL_BSP_SCHEDULE_DELAY', 5_000)),
                         max_queue_size: Integer(ENV.fetch('OTEL_BSP_MAX_QUEUE_SIZE', 2048)),
                         max_export_batch_size: Integer(ENV.fetch('OTEL_BSP_MAX_EXPORT_BATCH_SIZE', 512)),
                         start_thread_on_boot: String(ENV.fetch('OTEL_RUBY_BSP_START_THREAD_ON_BOOT', nil)) !~ /false/i,
                         metrics_reporter: nil)
            raise ArgumentError if max_export_batch_size > max_queue_size
            raise ArgumentError, "exporter #{exporter.inspect} does not appear to be a valid exporter" unless Common::Utilities.valid_exporter?(exporter)

            @exporter = exporter
            @exporter_timeout_seconds = exporter_timeout / 1000.0
            @mutex = Mutex.new
            @export_mutex = Mutex.new
            @condition = ConditionVariable.new
            @keep_running = true
            @delay_seconds = schedule_delay / 1000.0
            @max_queue_size = max_queue_size
            @batch_size = max_export_batch_size
            @metrics_reporter = metrics_reporter || OpenTelemetry::SDK::Trace::Export::MetricsReporter
            @spans = []
            @pid = nil
            @thread = nil
            reset_on_fork(restart_thread: start_thread_on_boot)
          end

          # Does nothing for this processor
          def on_start(_span, _parent_context); end

          # Adds a span to the batch. Thread-safe; may block on lock.
          def on_finish(span)
            return unless span.context.trace_flags.sampled?

            lock do
              reset_on_fork
              n = spans.size + 1 - max_queue_size
              if n > 0
                dropped_spans = spans.shift(n)
                report_dropped_spans(dropped_spans, reason: 'buffer-full', function: __method__.to_s)
              end
              spans << span
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
              spans.shift(spans.size)
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
              n = spans.size + snapshot.size - max_queue_size
              if n.positive?
                dropped_spans = snapshot.shift(n)
                report_dropped_spans(dropped_spans, reason: 'buffer-full', function: __method__.to_s)
              end
              spans.unshift(*snapshot) unless snapshot.empty?
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
            dropped_spans = lock { spans.shift(spans.length) }
            report_dropped_spans(dropped_spans, reason: 'terminating') if dropped_spans.any?
            @exporter.shutdown(timeout: OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time))
          end

          private

          attr_reader :spans, :max_queue_size, :batch_size

          def work
            loop do
              batch = lock do
                @condition.wait(@mutex, @delay_seconds) if spans.size < batch_size && @keep_running
                @condition.wait(@mutex, @delay_seconds) while spans.empty? && @keep_running
                return unless @keep_running

                spans.shift(@batch_size)
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
            @thread = restart_thread ? Thread.new { work } : nil
          rescue ThreadError => e
            @metrics_reporter.add_to_counter('otel.bsp.error', labels: { 'reason' => 'ThreadError' })
            OpenTelemetry.handle_error(exception: e, message: 'unexpected error in BatchSpanProcessor#reset_on_fork')
          end

          def export_batch(span_array, timeout: @exporter_timeout_seconds)
            return SUCCESS if span_array.empty?

            remaining = span_array
            attempts = 0
            start_time = OpenTelemetry::Common::Utilities.timeout_timestamp
            loop do
              remaining_timeout = OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time)
              return drop_remaining(remaining, reason: 'export-failure') if remaining_timeout&.zero?

              attempts += 1
              result = export_attempt(remaining, timeout: remaining_timeout)
              return report_result(result, remaining) unless result.is_a?(ExportResult)

              remaining = report_export_result(result, remaining)
              return SUCCESS if remaining.empty?
              return drop_remaining(remaining, reason: 'export-retry-exhausted') if attempts >= MAX_EXPORT_ATTEMPTS
            end
          end

          def export_attempt(span_array, timeout:)
            batch = span_array.map(&:to_span_data)
            @export_mutex.synchronize { @exporter.export(batch, timeout: timeout) }
          rescue StandardError => e
            @metrics_reporter.add_to_counter('otel.bsp.error', labels: { 'reason' => e.class.to_s })
            FAILURE
          end

          # Reports the outcome of a whole-batch (legacy) export result and
          # returns the result code so callers can propagate it.
          def report_result(result_code, span_array)
            if result_code == SUCCESS
              @metrics_reporter.add_to_counter('otel.bsp.export.success')
              @metrics_reporter.add_to_counter('otel.bsp.exported_spans', increment: span_array.size)
            else
              OpenTelemetry.handle_error(exception: ExportError.new(span_array))
              @metrics_reporter.add_to_counter('otel.bsp.export.failure')
              report_dropped_spans(span_array, reason: 'export-failure')
            end
            result_code
          end

          # Reports the outcome of a per-span {ExportResult} and returns the
          # subset of +span_array+ that the exporter asked to retry.
          #
          # Permanently rejected spans are counted as dropped with the
          # 'export-rejected' reason and are never retried. A receipt with no
          # rejected and no retryable spans counts as a successful export;
          # otherwise the drop counter, not the success counter, records the
          # loss.
          def report_export_result(result, span_array)
            size = span_array.size
            rejected_spans = result.rejected.select { |index| index < size }.map { |index| span_array[index] }
            retryable_spans = result.retryable.select { |index| index < size }.map { |index| span_array[index] }
            accepted_count = size - rejected_spans.size - retryable_spans.size

            @metrics_reporter.add_to_counter('otel.bsp.exported_spans', increment: accepted_count) if accepted_count.positive?

            if rejected_spans.any?
              OpenTelemetry.handle_error(exception: ExportError.new(rejected_spans))
              report_dropped_spans(rejected_spans, reason: 'export-rejected')
            end
            @metrics_reporter.add_to_counter('otel.bsp.export.success') if rejected_spans.empty? && retryable_spans.empty?

            retryable_spans
          end

          # Drops +spans+ after they could not be exported, reporting them with
          # the given reason. Used when a retryable segment is exhausted or an
          # export attempt raises.
          def drop_remaining(spans, reason:)
            OpenTelemetry.handle_error(exception: ExportError.new(spans))
            @metrics_reporter.add_to_counter('otel.bsp.export.failure')
            report_dropped_spans(spans, reason: reason)
            FAILURE
          end

          def report_dropped_spans(dropped_spans, reason:, function: nil)
            @metrics_reporter.add_to_counter('otel.bsp.dropped_spans', increment: dropped_spans.size, labels: { 'reason' => reason, OpenTelemetry::SemanticConventions::Trace::CODE_FUNCTION => function })
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
