# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Trace
      # SpanProcessorRegistration represents a single registration of a span
      # processor with a {TracerProvider}.
      #
      # It keeps {SpanProcessor#on_start} and {SpanProcessor#on_finish} paired
      # for every span: a span that started while the processor was registered
      # is delivered to it on finish, even if the processor is removed in
      # between. A span that starts after
      # {TracerProvider#remove_span_processor} returns is never delivered to the
      # removed processor.
      #
      # Removal does not shut the processor down immediately. Its shutdown is
      # deferred until every span that started while it was registered has
      # finished, so the processor never receives a span callback after its own
      # shutdown. Because those spans may still be running, {#wait} is the point
      # at which the shutdown is observable.
      #
      # Registrations are created by {TracerProvider#add_span_processor} and
      # returned by {TracerProvider#remove_span_processor}.
      class SpanProcessorRegistration
        # @return [#on_start, #on_finish] the registered span processor
        attr_reader :processor

        # @api private
        # @return [SpanProcessorRegistration]
        def initialize(processor)
          @processor = processor
          @mutex = Mutex.new
          @condition = ConditionVariable.new
          @in_flight = {}.compare_by_identity
          @retired = false
          @retire_timestamp = nil
          @shutdown_timeout = nil
          @shutdown_started = false
          @shutdown_complete = false
          @shutdown_result = nil
        end

        # @return [Boolean] true once the processor has been shut down
        def done?
          @mutex.synchronize { @shutdown_complete }
        end

        # Called when a span starts.
        #
        # The span is recorded as in-flight before the processor is notified, so
        # a concurrent {#retire} waits for it. Spans started after the
        # registration was retired are ignored, and so produce no matching
        # {#on_finish} delivery.
        #
        # @param [Span] span the span that just started.
        # @param [Context] parent_context the parent context of the span.
        # @return [void]
        def on_start(span, parent_context)
          @mutex.synchronize do
            return if @retired

            @in_flight[span] = true
          end
          @processor.on_start(span, parent_context)
        end

        # Called while a span is ending, before it becomes immutable.
        #
        # A failure here must not prevent the matching {#on_finish} delivery, so
        # it is reported rather than re-raised.
        #
        # @param [Span] span the span that is ending.
        # @return [void]
        def on_finishing(span)
          return unless tracked?(span) && @processor.respond_to?(:on_finishing)

          @processor.on_finishing(span)
        rescue StandardError => e
          OpenTelemetry.handle_error(exception: e, message: 'unexpected error in span processor on_finishing')
        end

        # Called when a span ends.
        #
        # Delivery completes before the span is released, so the processor cannot
        # be shut down underneath a callback that is still running. The release
        # happens even if the processor raises, and the last release of a retired
        # registration is what triggers the deferred shutdown.
        #
        # @param [Span] span the span that just ended.
        # @return [void]
        def on_finish(span)
          @processor.on_finish(span) if tracked?(span)
        ensure
          finalize_shutdown if release(span)
        end

        # Drops a span's reservation without delivering any callback.
        #
        # Used when a span fails to start, so a span that will never finish cannot
        # keep a removed registration from shutting down.
        #
        # @api private
        # @param [Span] span the span whose start failed.
        # @return [void]
        def abandon(span)
          finalize_shutdown if release(span)
        end

        # Stops delivering spans to the processor and shuts it down once every
        # span it already saw has finished.
        #
        # This method does not wait for those spans, so it is safe to call while
        # traffic is in flight. Use {#wait} to await the shutdown.
        #
        # @api private
        # @param [optional Numeric] timeout The maximum time to allow the
        #   processor's own #shutdown to take, measured from this call.
        # @return [self]
        def retire(timeout: nil)
          empty = @mutex.synchronize do
            next nil if @retired

            @retired = true
            @retire_timestamp = OpenTelemetry::Common::Utilities.timeout_timestamp
            @shutdown_timeout = timeout
            @in_flight.empty?
          end
          return self if empty.nil?

          finalize_shutdown if empty
          self
        end

        # Waits until the processor has been shut down.
        #
        # The shutdown starts only after every span that started while the
        # processor was registered has finished, so this is the moment to await
        # when taking a processor offline.
        #
        # @param [optional Numeric] timeout The maximum time to wait in seconds.
        # @return [Integer, nil] the value returned by the processor's #shutdown,
        #   or Export::TIMEOUT if the timeout elapsed first.
        def wait(timeout: nil)
          start_time = OpenTelemetry::Common::Utilities.timeout_timestamp
          @mutex.synchronize do
            until @shutdown_complete
              remaining_timeout = OpenTelemetry::Common::Utilities.maybe_timeout(timeout, start_time)
              return Export::TIMEOUT if remaining_timeout&.zero?

              @condition.wait(@mutex, remaining_timeout)
            end
            @shutdown_result
          end
        end

        # @param [optional Numeric] timeout An optional timeout in seconds.
        # @return [Integer] Export::SUCCESS if the processor is already shut
        #   down, otherwise the processor's result.
        def force_flush(timeout: nil)
          return Export::SUCCESS if @mutex.synchronize { @shutdown_started }

          @processor.force_flush(timeout: timeout)
        end

        private

        def tracked?(span)
          @mutex.synchronize { @in_flight.key?(span) }
        end

        # Releases the span's reservation.
        #
        # @return [Boolean] true if this release drained a retired registration
        #   and the caller must run {#finalize_shutdown}
        def release(span)
          @mutex.synchronize do
            @in_flight.delete(span)
            next false unless @retired && @in_flight.empty? && !@shutdown_started

            @condition.broadcast
            true
          end
        end

        def finalize_shutdown
          started, timeout = @mutex.synchronize do
            next [false, nil] if @shutdown_started

            @shutdown_started = true
            [true, OpenTelemetry::Common::Utilities.maybe_timeout(@shutdown_timeout, @retire_timestamp)]
          end
          return unless started

          result = call_processor_shutdown(timeout)

          @mutex.synchronize do
            @shutdown_result = result
            @shutdown_complete = true
            @condition.broadcast
          end
        end

        def call_processor_shutdown(timeout)
          @processor.shutdown(timeout: timeout)
        rescue StandardError => e
          OpenTelemetry.handle_error(exception: e, message: 'unexpected error in span processor shutdown')
          Export::FAILURE
        end
      end
    end
  end
end