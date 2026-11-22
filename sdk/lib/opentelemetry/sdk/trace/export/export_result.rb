# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Trace
      module Export
        # ExportResult is an optional return value for {SpanExporter#export} that
        # reports the disposition of the individual spans in an exported batch.
        #
        # Exporters that can only report the outcome of a whole batch keep
        # returning {SUCCESS} or {FAILURE}. Exporters whose backend can report
        # which spans it accepted (for example, via an OTLP
        # `ExportTracePartialSuccess`) may return an ExportResult instead.
        # {BatchSpanProcessor} then re-sends only the spans listed as
        # {#retryable}, counts spans listed as {#rejected} as permanently
        # dropped without retrying them, and keeps the existing whole-batch
        # behaviour for legacy return values.
        #
        # The collections hold indexes into the batch that was passed to
        # {SpanExporter#export}; they are not span or trace identifiers. Any
        # span that is not listed as {#rejected} or {#retryable} is treated as
        # accepted. When an index appears in more than one collection,
        # {#rejected} takes precedence over {#retryable}, which takes
        # precedence over {#accepted}.
        #
        # @example
        #   def export(batch, timeout: nil)
        #     accepted = []
        #     rejected = []
        #     retryable = []
        #     batch.each_with_index do |span_data, index|
        #       case backend.send(span_data)
        #       when :ok then accepted << index
        #       when :invalid then rejected << index
        #       else retryable << index
        #       end
        #     end
        #     ExportResult.new(accepted: accepted, rejected: rejected, retryable: retryable)
        #   end
        class ExportResult
          # @return [Array<Integer>] indexes of spans the backend accepted.
          attr_reader :accepted

          # @return [Array<Integer>] indexes of spans the backend permanently
          #   rejected. These spans are never retried.
          attr_reader :rejected

          # @return [Array<Integer>] indexes of spans the backend did not accept
          #   but that may succeed if they are sent again.
          attr_reader :retryable

          # @param [Enumerable<Integer>] accepted indexes of spans that were accepted.
          # @param [Enumerable<Integer>] rejected indexes of spans that were permanently rejected.
          # @param [Enumerable<Integer>] retryable indexes of spans that should be retried.
          def initialize(accepted: [], rejected: [], retryable: [])
            @rejected = normalize(rejected)
            @retryable = (normalize(retryable) - @rejected).freeze
            @accepted = (normalize(accepted) - @rejected - @retryable).freeze
          end

          # @return [Integer] the number of accepted spans.
          def accepted_count
            accepted.size
          end

          # @return [Integer] the number of permanently rejected spans.
          def rejected_count
            rejected.size
          end

          # @return [Integer] the number of spans that should be retried.
          def retryable_count
            retryable.size
          end

          # Returns true when no span was rejected and no span needs a retry.
          #
          # @return [Boolean]
          def success?
            rejected.empty? && retryable.empty?
          end

          private

          # Ignores entries that cannot be used as a batch index so a faulty
          # exporter cannot break the processor.
          def normalize(indexes)
            positions = indexes.to_a.filter_map do |index|
              value = Integer(index, exception: false)
              value if value && value >= 0
            end
            positions.uniq.freeze
          end
        end
      end
    end
  end
end
