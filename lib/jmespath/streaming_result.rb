# frozen_string_literal: true
module JMESPath
  # A lazy, forward-only {Enumerable} returned by {JMESPath.stream} and
  # {Runtime#stream}.
  #
  # Results are produced as elements are read from the source. The
  # source is closed exactly once: when iteration completes, when the
  # consumer stops early (e.g. via `break`, `first` or `take`), or when
  # an error is raised while evaluating the query.
  class StreamingResult
    include Enumerable

    # @api private
    def initialize(source, &driver)
      @source = source
      @driver = driver
      @closed = false
    end

    def each
      return enum_for(:each) unless block_given?
      begin
        @driver.call(->(value) { yield value })
      ensure
        close
      end
    end

    # Closes the underlying source. Safe to call multiple times; the
    # source is closed at most once.
    def close
      unless @closed
        @closed = true
        @source.close if @source.respond_to?(:close)
      end
      nil
    end
  end
end
