# frozen_string_literal: true

module Blueprinter
  # A lazily-evaluated JSON array response body, consumable chunk by chunk.
  #
  # The wrapped enumerable is never touched until #each is called, and each
  # element is serialized one at a time as the consumer advances. If the
  # source responds to #close, it is closed exactly once, whether iteration
  # completes, the consumer closes the body early, or an error is raised
  # partway through.
  class StreamingBody
    def initialize(enumerable, &serializer)
      @enumerable = enumerable
      @serializer = serializer
      @closed = false
    end

    # Yields the opening '[', one chunk per serialized element (with leading
    # commas where needed), and the closing ']'. If the source or the
    # serializer raises after chunks have been emitted, the error propagates
    # to the consumer and no closing bracket is appended.
    def each
      return enum_for(:each) unless block_given?

      enumerator = @enumerable.to_enum(:each)
      yield '['
      first = true
      begin
        loop do
          chunk = @serializer.call(enumerator.next)
          yield(first ? chunk : ",#{chunk}")
          first = false
        rescue StopIteration
          break
        end
        yield ']'
      ensure
        close
      end
    end

    # Closes the underlying source if it supports being closed. Idempotent.
    def close
      return if @closed

      @closed = true
      @enumerable.close if @enumerable.respond_to?(:close)
    end
  end
end
