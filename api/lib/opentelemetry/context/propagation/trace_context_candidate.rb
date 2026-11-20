# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  class Context
    module Propagation
      # Captures the outcome of a single propagator's attempt to read a trace
      # context from a carrier.
      #
      # Candidates are what a {CompositeTextMapPropagator} compares when a
      # carrier carries more than one tracing header family. Each candidate is
      # kept separate so that a propagator which only fills in part of a span
      # context cannot contribute identifiers, trace flags or tracestate to a
      # parent that was produced by another propagator.
      #
      # The four states make the usual distinctions explicit:
      # - {MISSING}: the propagator did not recognize a trace context at all.
      # - {INVALID}: the propagator recognized a trace context but could not
      #   parse a usable one.
      # - {PARTIAL}: the propagator produced only one of the two identifiers.
      # - {VALID}: the propagator produced both identifiers.
      class TraceContextCandidate
        # The propagator produced no trace context for this carrier.
        MISSING = :missing
        # The propagator found a trace context but could not parse a usable one.
        INVALID = :invalid
        # The propagator produced a trace context with only one identifier.
        PARTIAL = :partial
        # The propagator produced a trace context with both identifiers.
        VALID = :valid

        class << self
          # Returns a candidate for a propagator that produced no trace context.
          #
          # @param [Object] propagator The propagator that was run
          # @return [TraceContextCandidate]
          def missing(propagator)
            new(propagator: propagator, span_context: nil, state: MISSING)
          end

          # Returns a candidate for a propagator that recognized a trace
          # context but could not parse a usable one.
          #
          # @param [Object] propagator The propagator that was run
          # @return [TraceContextCandidate]
          def invalid(propagator)
            new(propagator: propagator, span_context: nil, state: INVALID)
          end

          # Returns a candidate for the span context a propagator produced.
          #
          # When the span context is nil, or is the very same object that was
          # current before the propagator ran, the propagator is treated as
          # having produced nothing.
          #
          # @param [Object] propagator The propagator that was run
          # @param [Trace::SpanContext, nil] span_context The span context the
          #   propagator produced
          # @param [optional Trace::SpanContext] previous_span_context The span
          #   context that was current before the propagator ran
          # @return [TraceContextCandidate]
          def from_span_context(propagator, span_context, previous_span_context: nil)
            return missing(propagator) if span_context.nil? || span_context.equal?(previous_span_context)

            new(propagator: propagator, span_context: span_context, state: classify(span_context))
          end

          # Returns a candidate by comparing the current span of the context a
          # propagator returned against the current span of the context it was
          # given. Propagators that only wrote unrelated context entries, such
          # as baggage, are reported as missing.
          #
          # @param [Object] propagator The propagator that was run
          # @param [Context] before_context The context passed to the propagator
          # @param [Context] after_context The context the propagator returned
          # @return [TraceContextCandidate]
          def from_context(propagator:, before_context:, after_context:)
            from_span_context(
              propagator,
              OpenTelemetry::Trace.current_span(after_context).context,
              previous_span_context: OpenTelemetry::Trace.current_span(before_context).context
            )
          end

          private

          def classify(span_context)
            if span_context.trace_id_valid? && span_context.span_id_valid?
              VALID
            elsif span_context.trace_id_valid? || span_context.span_id_valid?
              PARTIAL
            else
              INVALID
            end
          end
        end

        # @api private
        def initialize(propagator:, span_context:, state:)
          @propagator = propagator
          @span_context = span_context
          @state = state
        end

        # Returns the propagator that produced this candidate.
        #
        # @return [Object]
        attr_reader :propagator

        # Returns the span context observed for this candidate, or nil when the
        # propagator produced no trace context.
        #
        # @return [Trace::SpanContext, nil]
        attr_reader :span_context

        # Returns one of {MISSING}, {INVALID}, {PARTIAL} or {VALID}.
        #
        # @return [Symbol]
        attr_reader :state

        # Returns true when the propagator produced no trace context.
        #
        # @return [Boolean]
        def missing?
          @state == MISSING
        end

        # Returns true when the propagator recognized but could not parse a
        # usable trace context.
        #
        # @return [Boolean]
        def invalid?
          @state == INVALID
        end

        # Returns true when the propagator produced only one of the two
        # identifiers.
        #
        # @return [Boolean]
        def partial?
          @state == PARTIAL
        end

        # Returns true when the propagator produced both identifiers. Only a
        # complete candidate may become the parent span.
        #
        # @return [Boolean]
        def valid?
          @state == VALID
        end

        alias complete? valid?

        # Returns true when the propagator recognized a trace context, whether
        # or not it could parse a complete one.
        #
        # @return [Boolean]
        def present?
          !missing?
        end

        # Returns the trace ID of this candidate, or nil when the propagator
        # produced no trace context.
        #
        # @return [String, nil] a 16-byte binary string
        def trace_id
          @span_context&.trace_id
        end

        # Returns the span ID of this candidate, or nil when the propagator
        # produced no trace context.
        #
        # @return [String, nil] an 8-byte binary string
        def span_id
          @span_context&.span_id
        end

        # Returns the trace flags that travel with this candidate's parent, or
        # nil when the propagator produced no trace context.
        #
        # @return [Trace::TraceFlags, nil]
        def trace_flags
          @span_context&.trace_flags
        end

        # Returns the tracestate that travels with this candidate's parent, or
        # nil when the propagator produced no trace context.
        #
        # @return [Trace::Tracestate, nil]
        def tracestate
          @span_context&.tracestate
        end

        # Returns true when this candidate's span context was read from the
        # wire.
        #
        # @return [Boolean]
        def remote?
          @span_context ? @span_context.remote? : false
        end

        # Returns a hash describing this candidate. Identifiers are hex encoded
        # so the result can be logged or asserted against.
        #
        # @return [Hash{Symbol => Object}]
        def to_h
          {
            propagator: @propagator,
            state: @state,
            trace_id: @span_context&.hex_trace_id,
            span_id: @span_context&.hex_span_id,
            trace_flags: @span_context&.trace_flags,
            tracestate: @span_context&.tracestate&.to_s,
            remote: remote?
          }
        end
      end
    end
  end
end
