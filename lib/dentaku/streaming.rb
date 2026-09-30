require 'dentaku/exceptions'

module Dentaku
  # Length-aware, range-readable input protocol.
  #
  # A streaming input is any object that:
  #   * is not an Array/Hash/String,
  #   * responds to #stream_length (returns the number of samples as an Integer),
  #   * responds to #read_range(start_index, count) returning an Array of at
  #     most +count+ samples beginning at zero-based +start_index+.
  #
  # Include Dentaku::Streaming::Source to mark an object explicitly. Dentaku
  # never calls #to_a / #each / #[] on a streaming source: only #stream_length
  # and #read_range are ever used, so arbitrarily long sampled sequences never
  # have to fit into memory.
  module Streaming
    # Mixin for user-provided range-readable data sources.
    module Source
      def stream_source?
        true
      end

      # Optional human-readable identifier used in error messages and in the
      # access log; override as needed.
      def stream_name
        self.class.name
      end
    end

    # Context key under which the per-evaluation Session is stored.
    SESSION_KEY = "__dentaku_stream_session".freeze

    # Default number of samples pulled in a single #read_range call.
    DEFAULT_BATCH_SIZE = 256

    def self.stream?(value)
      return false if value.nil?
      return false if value.is_a?(::Array) || value.is_a?(::Hash) || value.is_a?(::String)

      value.respond_to?(:stream_length) && value.respond_to?(:read_range)
    end

    def self.session(context)
      context[SESSION_KEY]
    end

    # Raises StreamNotSupportedError unless +value+ is not a streaming input.
    def self.reject_stream!(value, operation:, location: nil, context: nil)
      return value unless stream?(value)

      source_name = name_of(value, context)
      raise Dentaku::StreamNotSupportedError.new(
        nil, source: value, operation: operation, location: location
      )
    end

    def self.name_of(value, context = nil)
      if value.respond_to?(:stream_name)
        value.stream_name
      elsif (sess = session(context))
        sess.name_of(value) || value.class.name
      else
        value.class.name
      end
    end

    # One record of a physical read against a data source.
    Read = Struct.new(:source, :start_index, :count, :returned, keyword_init: true) do
      def range
        start_index...(start_index + returned)
      end
    end

    # Per-evaluation state: read cache, access log, and the streaming API
    # used by AST nodes. Repeated reads inside one evaluation are served from
    # the cache with deterministic rules; a new evaluation gets a new session.
    class Session
      attr_reader :reads, :batch_size

      def initialize(batch_size: DEFAULT_BATCH_SIZE)
        # value object identity => cached samples and recorded length
        @sources = Hash.new { |hash, key| hash[key] = SourceState.new }
        @reads = []
        @batch_size = batch_size
      end

      def stream?(value)
        Streaming.stream?(value)
      end

      def name_of(value)
        @sources[value].name
      end

      def register(node_name, value)
        state = @sources[value]
        state.name ||= node_name
        state
      end

      def length(value, node_name = nil, **meta)
        state = register(node_name, value)
        state.operation = meta[:operation] if meta[:operation]
        state.location = meta[:location] if meta[:location]
        state.length ||= begin
          len = value.stream_length
          validate_length!(len, value, **meta)
          len
        end
      end

      # Resolve a possibly negative, zero-based-or-from-end index.
      def resolve_index(value, raw_index, node_name = nil, **meta)
        state = register(node_name, value)
        idx = raw_index
        len = length(value, node_name, **meta)
        idx += len if idx.negative?
        idx
      end

      def valid_index?(idx, value, **meta)
        idx >= 0 && idx < length(value, **meta)
      end

      # Single sample by index, honoring the cache.
      def fetch(value, index, node_name = nil, **meta)
        read(value, index, 1, node_name, **meta).first
      end

      # Return +count+ samples beginning at +start_index+, materializing only
      # the intervals that have not been read yet.
      def read(value, start_index, count, node_name = nil, **meta)
        return [] if count.zero?

        state = register(node_name, value)
        state.operation = meta[:operation] if meta[:operation]
        state.location = meta[:location] if meta[:location]
        state.ensure_loaded(value, start_index, count, self)
        state.samples_at(start_index, count)
      end

      # Yield every sample in batches of +batch_size+ without ever holding the
      # whole sequence. Returns an Enumerator when called without a block.
      def each(value, node_name = nil, **meta, &block)
        return to_enum(:each, value, node_name, **meta) unless block

        len = length(value, node_name, **meta)
        offset = 0
        while offset < len
          chunk = read(value, offset, [batch_size, len - offset].min, node_name, **meta)
          chunk.each { |sample| yield sample }
          offset += chunk.length
        end
      end

      # Sequential scan with early termination, e.g. for ANY/ALL. Each yielded
      # sample is paired with its zero-based index.
      def each_with_index(value, node_name = nil, **meta, &block)
        return to_enum(:each_with_index, value, node_name, **meta) unless block

        len = length(value, node_name, **meta)
        offset = 0
        while offset < len
          remaining = len - offset
          chunk = read(value, offset, [batch_size, remaining].min, node_name, **meta)
          chunk.each_with_index do |sample, local_index|
            yield sample, offset + local_index
          end
          offset += chunk.length
        end
      end

      def physical_reads_for(value)
        @reads.select { |read| read.source.equal?(value) }
      end

      def log_read(value, start_index, requested, returned)
        @reads << Read.new(
          source: value,
          start_index: start_index,
          count: requested,
          returned: returned
        )
      end

      private

      def validate_length!(len, value, **meta)
        unless len.is_a?(::Integer) && len >= 0
          raise Dentaku::StreamProtocolError.new(
            "stream_length must return a non-negative Integer, got #{len.inspect}",
            source: value, operation: meta[:operation], location: meta[:location]
          )
        end
      end

      # State kept for a single streaming source within one evaluation.
      class SourceState
        attr_accessor :name, :length, :operation, :location

        def initialize
          @samples = {} # absolute index => sample
          @loaded = []  # sorted disjoint integer Ranges actually fetched
        end

        def ensure_loaded(value, start_index, count, session)
          length = session.length(value, name, operation: operation, location: location)
          available = [0, [length - start_index, 0].max].max
          count = [count, available].min
          return if count.zero?

          missing = subtract_range(start_index...(start_index + count), @loaded)
          missing.each do |range|
            fetch_range(value, range.begin, range.size, session)
          end
        end

        def samples_at(start_index, count)
          Array.new(count) do |offset|
            @samples.fetch(start_index + offset)
          end
        end

        private

        def fetch_range(value, start_index, count, session)
          begin
            samples = value.read_range(start_index, count)
          rescue Dentaku::Error
            raise
          rescue => e
            raise Dentaku::StreamReadError.new(
              nil, cause: e, source: value, operation: operation_name,
              range: start_index...(start_index + count), location: location
            )
          end

          validate_chunk!(samples, value, start_index, count, session)

          returned = samples.length
          samples.each_with_index { |sample, i| @samples[start_index + i] = sample }
          insert_loaded(start_index...(start_index + returned))
          session.log_read(value, start_index, count, returned)
        end

        def validate_chunk!(samples, value, start_index, count, session)
          unless samples.is_a?(::Array)
            raise Dentaku::StreamProtocolError.new(
              "read_range(#{start_index}, #{count}) must return an Array, got #{samples.class}",
              source: value
            )
          end

          if samples.length > count
            raise Dentaku::StreamProtocolError.new(
              "read_range(#{start_index}, #{count}) returned #{samples.length} samples " \
              "(expected at most #{count})",
              source: value
            )
          end

          len = session.length(value, name, operation: operation, location: location)
          if start_index + samples.length > len
            raise Dentaku::StreamProtocolError.new(
              "read_range returned past the end of '#{name || value.class.name}' " \
              "(length #{len})",
              source: value
            )
          end

          if samples.length < count && start_index + samples.length < len
            raise Dentaku::StreamProtocolError.new(
              "read_range(#{start_index}, #{count}) returned only #{samples.length} " \
              "samples before the end of '#{name || value.class.name}' (length #{len})",
              source: value, operation: operation, location: location
            )
          end
        end

        def operation_name
          operation || (name ? "#{name}[]" : "range read")
        end

        def insert_loaded(range)
          @loaded << range
          merged = []
          @loaded.sort_by(&:begin).each do |current|
            if merged.empty? || merged.last.end < current.begin
              merged << current
            else
              prev = merged.pop
              merged << (prev.begin...[prev.end, current.end].max)
            end
          end
          @loaded = merged
        end

        def subtract_range(requested, loaded)
          result = [requested]
          loaded.each do |covered|
            result = result.flat_map { |part| split_range(part, covered) }
          end
          result
        end

        def split_range(range, covered)
          return [range] if range.end <= covered.begin || range.begin >= covered.end

          pieces = []
          pieces << (range.begin...covered.begin) if range.begin < covered.begin
          pieces << (covered.end...range.end) if covered.end < range.end
          pieces
        end
      end

    end
  end
end
