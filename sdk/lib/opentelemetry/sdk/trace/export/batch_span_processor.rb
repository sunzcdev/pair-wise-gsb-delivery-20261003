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
        # A forked process inherits the queue and the exporter of its parent,
        # but the exporter may hold transport state (sockets, buffers) that
        # cannot be shared with the parent. Pass {exporter_factory} to give
        # each process its own exporter: the processor watches Process.pid and
        # abandons the inherited exporter without calling any of its methods.
        class BatchSpanProcessor # rubocop:disable Metrics/ClassLength
          # Returns a new instance of the {BatchSpanProcessor}.
          #
          # Either +exporter+ or +exporter_factory+ (or both) must be given.
          # When both are given, +exporter+ belongs to the process that
          # constructs the processor; the factory serves every other process,
          # including forked children.
          #
          # With an +exporter_factory+, the processor records which process
          # created its exporter and never uses, flushes, or shuts down an
          # exporter that belongs to a different process. A process asks the
          # factory for its exporter the first time it has spans to hand to
          # one. The factory runs at most once per process and its result is
          # reused until that process exits. In particular:
          #
          # * {#on_finish} creates the exporter before queuing the span, so
          #   the worker thread never has to create one;
          # * {#force_flush} and {#shutdown} create an exporter only when the
          #   process has spans to export, and otherwise return SUCCESS
          #   without touching any exporter;
          # * {#shutdown} calls +#shutdown+ exactly once, and only on the
          #   exporter that belongs to the calling process;
          # * spans queued by the parent are discarded when a child detects
          #   the fork, and batches already removed from the queue by a parent
          #   are never exported through the child's exporter.
          #
          # If the factory raises or returns an invalid exporter, the error is
          # reported through {OpenTelemetry.handle_error}, the factory is not
          # retried in that process, and resulting exports are reported as
          # failures. Factory errors never propagate to the caller.
          #
          # @param [optional SpanExporter] exporter the (duck type) SpanExporter to
          #   where the recorded Spans are pushed after batching.
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
          # @param [#call, nil] exporter_factory an optional zero-argument
          #   callable returning a fresh SpanExporter for the calling process.
          #
          # @return a new instance of the {BatchSpanProcessor}.
          def initialize(exporter = nil,
                         exporter_timeout: Float(ENV.fetch('OTEL_BSP_EXPORT_TIMEOUT', 30_000)),
                         schedule_delay: Float(ENV.fetch('OTEL_BSP_SCHEDULE_DELAY', 5_000)),
                         max_queue_size: Integer(ENV.fetch('OTEL_BSP_MAX_QUEUE_SIZE', 2048)),
                         max_export_batch_size: Integer(ENV.fetch('OTEL_BSP_MAX_EXPORT_BATCH_SIZE', 512)),
                         start_thread_on_boot: String(ENV.fetch('OTEL_RUBY_BSP_START_THREAD_ON_BOOT', nil)) !~ /false/i,
                         metrics_reporter: nil,
                         exporter_factory: nil)
            raise ArgumentError if max_export_batch_size > max_queue_size

            validate_exporter!(exporter, exporter_factory)

            @exporter = exporter
            @exporter_pid = exporter && Process.pid
            @exporter_factory = exporter_factory
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
              # Create this process's exporter before the span becomes visible
              # to the worker thread, so every queued span is paired with an
              # exporter this process may export and shut down. This only runs
              # the factory once per process.
              exporter_for_current_process(create: true)
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
            pid = Process.pid
            snapshot, exporter = lock do
              reset_on_fork(restart_thread: @keep_running)
              snapshot = spans.shift(spans.size)
              # Only ask for an exporter when this process has spans to export.
              [snapshot, exporter_for_current_process(create: !snapshot.empty?)]
            end
            until snapshot.empty?
              remaining_timeout = OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time)
              return TIMEOUT if remaining_timeout&.zero?

              batch = snapshot.shift(@batch_size)
              result_code = export_batch(batch, exporter: exporter, timeout: remaining_timeout)
              return result_code unless result_code == SUCCESS
            end

            # A process that has nothing to export and never created an
            # exporter succeeds without forcing a flush on someone else's.
            return SUCCESS if exporter.nil?

            exporter.force_flush(timeout: OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time))
          ensure
            lock do
              next if snapshot.nil? || snapshot.empty?

              # Spans this process took before a fork belong to the parent. They
              # are neither requeued here nor exported through this process's
              # exporter.
              if pid == Process.pid
                requeue_snapshot(snapshot, function: __method__.to_s)
              else
                drop_forked_batch(snapshot, function: __method__.to_s)
              end
            end
          end

          # Shuts the consumer thread down and flushes the current accumulated buffer
          # will block until the thread is finished.
          #
          # Only the exporter belonging to the current process is shut down. A
          # forked child never shuts down the exporter it inherited, and a
          # process that never needed an exporter does not create one just to
          # shut it down.
          #
          # @param [optional Numeric] timeout An optional timeout in seconds.
          # @return [Integer] SUCCESS if no error occurred, FAILURE if a
          #   non-specific failure occurred, TIMEOUT if a timeout occurred.
          def shutdown(timeout: nil)
            start_time = OpenTelemetry::Common::Utilities.timeout_timestamp
            thread = lock do
              # Settle the fork before looking at the thread: a child must not
              # join or flush work that belonged to its parent.
              reset_on_fork(restart_thread: false)
              @keep_running = false
              @condition.signal
              @thread
            end

            thread&.join(timeout)
            force_flush(timeout: OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time))
            dropped_spans = lock { spans.shift(spans.length) }
            report_dropped_spans(dropped_spans, reason: 'terminating') if dropped_spans.any?
            exporter = exporter_for_current_process
            return SUCCESS if exporter.nil?

            exporter.shutdown(timeout: OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time))
          end

          private

          attr_reader :spans, :max_queue_size, :batch_size

          def work
            loop do
              batch, exporter = lock do
                @condition.wait(@mutex, @delay_seconds) if spans.size < batch_size && @keep_running
                @condition.wait(@mutex, @delay_seconds) while spans.empty? && @keep_running
                return unless @keep_running

                [spans.shift(@batch_size), exporter_for_current_process]
              end

              @metrics_reporter.observe_value('otel.bsp.buffer_utilization', value: spans.size / max_queue_size.to_f)

              export_batch(batch, exporter: exporter)
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

          # With a factory, returns the exporter for the current process,
          # creating it on demand when `create` is true. The factory runs at
          # most once per process and the outcome is cached, including
          # failures: a broken factory is reported once and then behaves like
          # "no exporter", so it is not retried on every span or batch.
          #
          # Callers hold the processor mutex, so the exporter is created and
          # paired with a batch or a queue entry atomically.
          def exporter_for_current_process(create: false)
            return @exporter if @exporter_pid == Process.pid
            return @exporter if @exporter_factory.nil?
            return nil unless create

            @exporter_pid = Process.pid
            @exporter = build_exporter
          end

          def build_exporter
            exporter = @exporter_factory.call
            return exporter if Common::Utilities.valid_exporter?(exporter)

            raise ArgumentError, "exporter_factory returned #{exporter.inspect}, which does not appear to be a valid exporter"
          rescue StandardError => e
            @metrics_reporter.add_to_counter('otel.bsp.error', labels: { 'reason' => 'exporter-factory' })
            OpenTelemetry.handle_error(exception: e, message: 'unexpected error in BatchSpanProcessor#exporter_factory')
            nil
          end

          def validate_exporter!(exporter, exporter_factory)
            raise ArgumentError, 'an exporter or an exporter_factory is required' if exporter.nil? && exporter_factory.nil?
            raise ArgumentError, "exporter_factory #{exporter_factory.inspect} does not appear to be callable" if exporter_factory && !exporter_factory.respond_to?(:call)
            return if exporter.nil? || Common::Utilities.valid_exporter?(exporter)

            raise ArgumentError, "exporter #{exporter.inspect} does not appear to be a valid exporter"
          end

          # Export a batch that was taken from the queue together with the
          # exporter that was current for this process at that moment. With a
          # factory, an exporter inherited across a fork is never used: a
          # batch paired with the parent's exporter is dropped by the child.
          def export_batch(span_array, exporter:, timeout: @exporter_timeout_seconds)
            if exporter && !exporter_usable_by_current_process?(exporter)
              return drop_forked_batch(span_array, function: __method__.to_s)
            end

            result_code = FAILURE
            if exporter
              batch = span_array.map(&:to_span_data)
              result_code = @export_mutex.synchronize { exporter.export(batch, timeout: timeout) }
            end
            report_result(result_code, span_array)
            result_code
          rescue StandardError => e
            report_result(FAILURE, span_array)
            @metrics_reporter.add_to_counter('otel.bsp.error', labels: { 'reason' => e.class.to_s })
            FAILURE
          end

          # An exporter may be used if this process owns it, or if no factory
          # was configured and it is the exporter the processor has always
          # used (the historical fork behaviour).
          def exporter_usable_by_current_process?(exporter)
            return true if @exporter_factory.nil?

            @exporter_pid == Process.pid && exporter.equal?(@exporter)
          end

          def drop_forked_batch(span_array, function:)
            report_dropped_spans(span_array, reason: 'fork', function: function)
            FAILURE
          end

          def report_result(result_code, span_array)
            if result_code == SUCCESS
              @metrics_reporter.add_to_counter('otel.bsp.export.success')
              @metrics_reporter.add_to_counter('otel.bsp.exported_spans', increment: span_array.size)
            else
              OpenTelemetry.handle_error(exception: ExportError.new(span_array))
              @metrics_reporter.add_to_counter('otel.bsp.export.failure')
              report_dropped_spans(span_array, reason: 'export-failure')
            end
          end

          def report_dropped_spans(dropped_spans, reason:, function: nil)
            @metrics_reporter.add_to_counter('otel.bsp.dropped_spans', increment: dropped_spans.size, labels: { 'reason' => reason, OpenTelemetry::SemanticConventions::Trace::CODE_FUNCTION => function })
          end

          # Put back spans that could not be exported before the timeout,
          # dropping the excess because they are older than whatever is still
          # queued.
          def requeue_snapshot(snapshot, function:)
            n = spans.size + snapshot.size - max_queue_size
            if n.positive?
              dropped_spans = snapshot.shift(n)
              report_dropped_spans(dropped_spans, reason: 'buffer-full', function: function)
            end
            spans.unshift(*snapshot) unless snapshot.empty?
            @condition.signal if spans.size > max_queue_size / 2
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
