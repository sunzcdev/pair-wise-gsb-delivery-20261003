require_relative 'exceptions'
require_relative 'print_visitor'

module Dentaku
  # A streaming input is a potentially very long sampled sequence that must
  # not be materialized in memory. Data sources speak a small protocol:
  #
    #   dentaku_stream?           -> true
  #   dentaku_stream_length     -> Integer  number of samples
  #   dentaku_stream_read(range)-> Array    samples within +range+
  #
  # The read range is always inclusive, zero-based, and never covers the
  # whole sequence at once: reads are requested in fixed-size windows.
  #
  # Sources may also expose:
  #
  #   dentaku_stream_name       -> String  used in error messages
  #   dentaku_stream_window_size -> Integer preferred window size
  #
  # A source is never asked to read the full sequence, and Dentaku never
  # implicitly converts a streaming sequence into an Array. Operations that
  # cannot stream (MAP, FILTER, PLUCK, arithmetic on sequences, ...) raise
  # Dentaku::ArgumentError with reason :streaming_unsupported.
  module Streaming
    CONTEXT_SESSION_KEY = "__stream_session".freeze
    CONTEXT_FORMULA_KEY = "__formula".freeze

    DEFAULT_WINDOW_SIZE = 1024

    # Operations that are allowed to consume a streaming sequence.
    SUPPORTED_OPERATIONS = %w[
      index COUNT SUM AVG MIN MAX MUL ANY ALL REDUCE IF CASE AND OR
    ].freeze

    # Duck-typing check used by every operation before touching a value as a
    # sequence. Plain Arrays do not speak the protocol and keep their
    # materialized semantics.
    def self.stream?(value)
      value.respond_to?(:dentaku_stream?) && value.dentaku_stream?
    rescue StandardError
      false
    end

    # Returns the LazySequence wrapper for +source+ bound to the current
    # evaluation, so repeated reads inside one calculation see one stable
    # snapshot of the data. A source bound outside an evaluation (e.g. a
    # hand-built AST node evaluated directly) gets an ephemeral session.
    def self.wrap(source, context = {})
      session = context[CONTEXT_SESSION_KEY] || Session.new
      session.wrap(source)
    end

    # Best-effort description of where in the formula an operation lives.
    # Nodes render through the infix visitor; strings are used as given.
    def self.formula_location(context, node = nil)
      location = context[CONTEXT_FORMULA_KEY]

      if node.respond_to?(:accept)
        subexpression = PrintVisitor.new(node).to_s
        if location && location != subexpression
          return "#{subexpression} in #{location}"
        end

        return subexpression
      end

      location
    end

    def self.unsupported!(operation, context, node = nil)
      raise Dentaku::ArgumentError.for(
        :streaming_unsupported,
        function_name: operation,
        supported: SUPPORTED_OPERATIONS
      ), unsupported_message(operation, context, node)
    end

    def self.unsupported_message(operation, context, node)
      location = formula_location(context, node)
      message = "#{operation}() cannot stream its input; the sequence is never " \
                "materialized implicitly; supported streaming operations are " \
                "#{SUPPORTED_OPERATIONS.join(', ')}"
      message = "#{message} (at #{location})" if location
      message
    end

    # Per-evaluation state. One wrapper (and therefore one read cache) per
    # data source guarantees that reading the same sample twice during a
    # single calculation returns the same value, even if the underlying
    # source mutates between window reads.
    class Session
      def initialize(window_size: nil)
        @window_size = window_size
        @wrappers = {}
      end

      def wrap(source)
        @wrappers[source.object_id] ||=
          LazySequence.new(source, self, window_size: @window_size)
      end

      def window_size_for(source)
        source_size = source.dentaku_stream_window_size if
          source.respond_to?(:dentaku_stream_window_size)

        source_size ||
          @window_size ||
          DEFAULT_WINDOW_SIZE
      end

      def cache
        @cache ||= {}
      end

      def record_read(source, range)
        reads(source) << range
      end

      def reads(source)
        (@reads ||= {})[source.object_id] ||= []
      end

      def length_calls
        @length_calls ||= Hash.new(0)
      end
    end

    # Windowed, cached view over one streaming data source for one
    # calculation. Wrapper instances are not shared between calculations.
    class LazySequence
      include Enumerable

      attr_reader :source

      def initialize(source, session, window_size: nil)
        unless Streaming.stream?(source)
          raise ::ArgumentError,
                "#{source.class} does not implement the Dentaku streaming protocol"
        end

        @source = source
        @session = session
        @explicit_window_size = window_size
        @length = nil
      end

      def length
        @length ||= begin
          @session.length_calls[@source.object_id] += 1
          value = @source.dentaku_stream_length
          unless value.is_a?(::Integer) && value >= 0
            raise StreamProtocolError.new(
              source: @source, operation: :length,
              cause_error: "expected non-negative Integer length, got #{value.inspect}"
            )
          end

          value
        rescue StreamError
          raise
        rescue StandardError => e
          raise StreamLengthError.new(source: @source, operation: :length, cause_error: e)
        end
      end
      alias_method :size, :length

      # Random access by zero-based, non-negative integer index. Out of range
      # and negative indexes answer nil without issuing a read, matching
      # Array indexing behaviour.
      def [](index)
        return nil unless index.is_a?(::Integer)
        return nil if index.negative?
        return nil if index >= length

        at(index)
      end

      def at(index)
        window = window_for(index)
        read_window(window).fetch(index - window.begin)
      end

      # Streams windows in order and stops early when the block breaks.
      def each
        return to_enum(:each) unless block_given?

        total = length
        window_index = 0
        while window_index * window_size < total
          window = window_for(window_index * window_size)
          values = read_window(window)
          values.each { |value| yield value }
          window_index += 1
        end
        self
      end

      # Actual ranges fetched from the data source in read order.
      def access_log
        @session.reads(@source)
      end

      # Number of dentaku_stream_length calls made during this calculation.
      def length_call_count
        @session.length_calls[@source.object_id]
      end

      def dentaku_stream?(*)
        true
      end

      private

      def window_size
        @explicit_window_size || @session.window_size_for(@source)
      end

      def window_for(index)
        start = (index / window_size) * window_size
        stop = [start + window_size, length].min - 1
        start..stop
      end

      def read_window(window)
        cache_key = [@source.object_id, window.begin, window.end]
        return @session.cache[cache_key] if @session.cache.key?(cache_key)

        values = fetch(window)
        validate_window!(window, values)
        @session.record_read(@source, window)
        @session.cache[cache_key] = values
      end

      def fetch(window)
        @source.dentaku_stream_read(window)
      rescue StreamError
        raise
      rescue StandardError => e
        raise StreamReadError.new(source: @source, range: window,
                                 operation: :read, cause_error: e)
      end

      def validate_window!(window, values)
        unless values.is_a?(::Array)
          raise StreamProtocolError.new(source: @source, range: window,
                                        operation: :read,
                                        cause_error: "expected Array, got #{values.class}")
        end

        expected = window.end - window.begin + 1
        return if values.length == expected

        raise StreamProtocolError.new(
          source: @source, range: window, operation: :read,
          cause_error: "expected #{expected} samples, got #{values.length}"
        )
      end
    end
  end
end

require_relative 'streaming/base'
