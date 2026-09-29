# frozen_string_literal: true

module Liquid
  # @api private
  #
  # Output buffer used by Template#render_each. Every write is delivered to
  # the consumer immediately as a chunk, instead of being concatenated into
  # one big string. Tags that need their complete body (e.g. {% capture %})
  # still buffer locally, because they render into their own String.
  #
  # Tracks the cumulative bytesize so ResourceLimits#increment_write_score
  # keeps enforcing render_length_limit against the whole output.
  class StreamOutput
    attr_reader :bytesize

    def initialize(&on_chunk)
      @on_chunk = on_chunk
      @bytesize = 0
    end

    def <<(chunk)
      @bytesize += chunk.bytesize
      # Hand the consumer a copy: chunks may be strings owned by the
      # template (e.g. raw text nodes) which must not be mutated.
      @on_chunk.call(chunk.dup) unless chunk.empty?
      self
    end
  end
end
