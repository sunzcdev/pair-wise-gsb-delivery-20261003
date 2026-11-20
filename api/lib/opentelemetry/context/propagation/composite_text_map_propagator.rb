# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  class Context
    module Propagation
      # A composite text map propagator either composes a list of injectors and a
      # list of extractors, or wraps a list of propagators, into a single interface
      # exposing inject and extract methods. Injection and extraction will preserve
      # the order of the injectors and extractors (or propagators) passed in during
      # initialization.
      #
      # When more than one composed propagator recognizes a trace context in the
      # same carrier, only one of them may become the parent span. The composite
      # adopts a single complete candidate, so the trace ID, span ID, trace flags
      # and tracestate of the parent all come from the same header family. By
      # default the last complete candidate wins, matching the historical
      # behaviour of running extractors in order; an explicit +precedence+ makes
      # the choice independent of order. Candidates that only fill in part of a
      # span context are never adopted. Propagators that write state unrelated to
      # the trace context, such as baggage, are still applied independently.
      class CompositeTextMapPropagator
        class << self
          private :new

          # Returns a Propagator that extracts using the provided extractors
          # and injectors.
          #
          # @param [Array<#inject, #fields>] injectors An array of text map injectors
          # @param [Array<#extract>] extractors An array of text map extractors
          # @param [optional Array<Object>] precedence Extractors, extractor
          #   classes or class names, in the order they should be preferred when
          #   more than one of them produces a complete trace context. Entries
          #   that are not listed keep the relative order they were composed in,
          #   after the listed entries. Defaults to the composition order.
          def compose(injectors:, extractors:, precedence: nil)
            raise ArgumentError, 'injectors and extractors must both be non-nil arrays' unless injectors.is_a?(Array) && extractors.is_a?(Array)
            raise ArgumentError, 'precedence must be a non-nil array' if !precedence.nil? && !precedence.is_a?(Array)

            new(injectors: injectors, extractors: extractors, precedence: precedence)
          end

          # Returns a Propagator that extracts using the provided propagators.
          #
          # @param [Array<#inject, #extract, #fields>] propagators An array of
          #   text map propagators
          # @param [optional Array<Object>] precedence Propagators, propagator
          #   classes or class names, in the order they should be preferred when
          #   more than one of them produces a complete trace context. Entries
          #   that are not listed keep the relative order they were composed in,
          #   after the listed entries. Defaults to the composition order.
          def compose_propagators(propagators, precedence: nil)
            raise ArgumentError, 'propagators must be a non-nil array' unless propagators.is_a?(Array)
            raise ArgumentError, 'precedence must be a non-nil array' if !precedence.nil? && !precedence.is_a?(Array)
            return NoopTextMapPropagator.new if propagators.empty?
            return propagators.first if propagators.size == 1

            new(propagators: propagators, precedence: precedence)
          end
        end

        # @api private
        def initialize(injectors: nil, extractors: nil, propagators: nil, precedence: nil)
          @injectors = injectors
          @extractors = extractors
          @propagators = propagators
          @precedence = precedence&.dup&.freeze
        end

        # Returns the propagators that make up this composite, in composition
        # order. Returns nil when the composite was built from separate
        # injectors and extractors.
        #
        # @return [Array<Object>, nil]
        attr_reader :propagators

        # Returns the explicit precedence order, or nil when the composition
        # order is used.
        #
        # @return [Array<Object>, nil]
        attr_reader :precedence

        # Runs injectors or propagators in order. If an injection fails
        # a warning will be logged and remaining injectors will be executed.
        #
        # @param [Object] carrier A mutable carrier to inject context into.
        # @param [optional Context] context Context to be injected into carrier. Defaults
        #   to +Context.current+.
        # @param [optional Setter] setter If the optional setter is provided, it
        #   will be used to write context into the carrier, otherwise the default
        #   setter will be used.
        def inject(carrier, context: Context.current, setter: Context::Propagation.text_map_setter)
          injectors = @injectors || @propagators
          injectors.each do |injector|
            injector.inject(carrier, context: context, setter: setter)
          rescue StandardError => e
            OpenTelemetry.logger.warn "Error in CompositePropagator#inject #{e.message}"
          end
          nil
        end

        # Returns what each propagator parses from the carrier on its own. By
        # default candidates are returned in composition order; when an explicit
        # precedence was given they are returned in that order, with unlisted
        # propagators keeping their composition order after the listed ones.
        #
        # A candidate describes only the trace context one propagator produced.
        # Every propagator runs against the same base context, so a propagator
        # that fills in only part of a span context is reported as it is and
        # cannot borrow fields from another propagator. Each candidate's trace
        # flags and tracestate belong to that candidate's own span context.
        #
        # @param [Object] carrier The carrier to read trace contexts from.
        # @param [optional Context] context The base context each propagator is
        #   run against. Defaults to +Context.current+.
        # @param [optional Getter] getter If the optional getter is provided, it
        #   will be used to read the header from the carrier, otherwise the
        #   default getter will be used.
        #
        # @return [Array<TraceContextCandidate>]
        def candidates(carrier, context: Context.current, getter: Context::Propagation.text_map_getter)
          observed = observe(carrier, context, getter)
          return observed if @precedence.nil? || @precedence.empty?

          order_by_precedence(observed)
        end

        # Runs extractors or propagators in order and returns a Context updated
        # with the results of each extraction. If an extraction fails, a warning
        # will be logged and remaining extractors will continue to be executed. Always
        # returns a valid context.
        #
        # State that is not related to the trace context, such as baggage, is
        # threaded through the extractors and kept. The parent span, its trace
        # flags and its tracestate, however, are taken from a single complete
        # candidate. Without an explicit precedence the last complete candidate
        # wins, matching the historical behaviour of running extractors in order;
        # with a precedence the first listed complete candidate wins. Either way
        # the parent's identifiers, flags and tracestate all come from the same
        # header family, and each extractor is run with the base span active so a
        # partial candidate cannot contribute fields to the adopted parent.
        #
        # @param [Object] carrier The carrier to extract context from.
        # @param [optional Context] context Context to be updated with the state
        #   extracted from the carrier. Defaults to +Context.current+.
        # @param [optional Getter] getter If the optional getter is provided, it
        #   will be used to read the header from the carrier, otherwise the default
        #   getter will be used.
        #
        # @return [Context] a new context updated with state extracted from the
        #   carrier
        def extract(carrier, context: Context.current, getter: Context::Propagation.text_map_getter)
          base_span = OpenTelemetry::Trace.current_span(context)
          merged = context
          observed = []

          extractors.each do |extractor|
            input = isolate_span(merged, base_span)
            extracted = safely_extract(extractor, carrier, input, getter)
            observed << candidate_for(extractor, carrier, input, getter, extracted)
            merged = extracted if extracted
          end

          adopt(merged, base_span, observed)
        end

        # Returns the union of the propagation fields returned by the composed injectors
        # or propagators. If your carrier is reused, you should delete the fields returned
        # by this method before calling +inject+.
        #
        # @return [Array<String>] a list of fields that will be used by this propagator.
        def fields
          injectors = @injectors || @propagators
          injectors.flat_map(&:fields).uniq
        end

        private

        def extractors
          @extractors || @propagators
        end

        # Observes what each extractor parses from the carrier in isolation.
        def observe(carrier, context, getter)
          extractors.map do |extractor|
            extracted = safely_extract(extractor, carrier, context, getter)
            candidate_for(extractor, carrier, context, getter, extracted)
          end
        end

        # Builds the candidate for a single extractor run. When the extractor
        # did not change the active span, a propagator that exposes its own
        # candidate classification is asked so a parsed-but-invalid header is
        # reported as invalid rather than missing.
        def candidate_for(extractor, carrier, context, getter, extracted)
          return classified(extractor, carrier, context, getter) unless extracted

          candidate = TraceContextCandidate.from_context(propagator: extractor, before_context: context, after_context: extracted)
          candidate.missing? ? classified(extractor, carrier, context, getter) : candidate
        rescue StandardError => e
          OpenTelemetry.logger.warn "Error in CompositePropagator#extract #{e.message}"
          TraceContextCandidate.invalid(extractor)
        end

        def classified(extractor, carrier, context, getter)
          return TraceContextCandidate.missing(extractor) unless extractor.respond_to?(:extract_candidate)

          extractor.extract_candidate(carrier, context: context, getter: getter)
        rescue StandardError => e
          OpenTelemetry.logger.warn "Error in CompositePropagator#extract #{e.message}"
          TraceContextCandidate.invalid(extractor)
        end

        def safely_extract(extractor, carrier, context, getter)
          extractor.extract(carrier, context: context, getter: getter)
        rescue StandardError => e
          OpenTelemetry.logger.warn "Error in CompositePropagator#extract #{e.message}"
          nil
        end

        # Returns the context with the base span restored as the current span,
        # leaving any other extracted state untouched. This keeps a propagator
        # from observing the parent another propagator produced.
        def isolate_span(context, base_span)
          return context if OpenTelemetry::Trace.current_span(context).equal?(base_span)

          OpenTelemetry::Trace.context_with_span(base_span, parent_context: context)
        end

        # Returns the candidates ordered so preferred propagators come first.
        # Candidates that are not named in the precedence list keep their
        # composition order after the named ones.
        def order_by_precedence(observed)
          return observed if @precedence.nil? || @precedence.empty?

          remaining = observed.dup
          ordered = @precedence.filter_map do |preferred|
            match = remaining.find { |candidate| matches?(candidate.propagator, preferred) }
            remaining.delete(match) if match
            match
          end
          ordered + remaining
        end

        def matches?(propagator, preferred)
          return true if propagator.equal?(preferred)
          return propagator.is_a?(preferred) if preferred.is_a?(Module)
          return propagator.class.name == preferred.to_s if preferred.is_a?(String)

          false
        end

        # Adopts a single complete candidate as the parent span and discards any
        # other trace context parsed from the carrier. The candidate is chosen by
        # explicit precedence when one was given, otherwise the last complete
        # candidate wins, matching the historical "last extractor wins" order.
        # When a trace context was parsed but no candidate is complete, the base
        # span is restored rather than splicing in a partial parent.
        def adopt(merged, base_span, observed)
          selected = selected_candidate(observed)
          return merged if selected.nil? && observed.none?(&:present?)

          span = selected ? OpenTelemetry::Trace.non_recording_span(selected.span_context) : base_span
          OpenTelemetry::Trace.context_with_span(span, parent_context: merged)
        end

        def selected_candidate(observed)
          complete = observed.select(&:valid?)
          return nil if complete.empty?
          return complete.last if @precedence.nil? || @precedence.empty?

          order_by_precedence(observed).find(&:valid?)
        end
      end
    end
  end
end
