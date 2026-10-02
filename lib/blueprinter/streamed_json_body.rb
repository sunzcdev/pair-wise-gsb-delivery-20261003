# frozen_string_literal: true

require 'thread'
require 'blueprinter/errors/streaming_root_unsupported'
require 'blueprinter/errors/streaming_meta_unsupported'

module Blueprinter
  # A chunked, single-pass JSON array response body produced from an Enumerable
  # data source that can only be traversed once.
  #
  # The source is not read until the body is consumed. Each advance serializes
  # only the object currently needed; the full collection is never mapped into
  # memory.
  #
  # The object behaves like a Rack response body: it responds to +#each+
  # (yielding String chunks) and +#close+. Consumption starts when +#each+ is
  # called (or when the Enumerator returned by a blockless +#each+ is itself
  # consumed). +#close+ is idempotent.
  #
  # If the source responds to +#close+, it is closed exactly once: on normal
  # completion, on early +#close+/break, and when the source raises mid-stream.
  # When an error is raised after bytes have already been emitted, the error
  # propagates to the consumer without appending a closing +]+.
  #
  # @example Streaming a one-shot enumerator
  #   body = WidgetBlueprint.render_as_stream(Widget.some_enumerator)
  #   body.each { |chunk| socket.write(chunk) }
  #
  # @example With a Rack response
  #   body = WidgetBlueprint.render_as_stream(Widget.some_enumerator)
  #   [200, { 'Content-Type' => 'application/json' }, body]
  class StreamedJsonBody
    include Enumerable

    # @param source [Enumerable] the data source; must be traversable via #each
    # @param blueprint [Class<Blueprinter::Base>] blueprint used per element
    # @param options [Hash] render options (e.g. :view)
    # @raise [ArgumentError] if source is not an Enumerable
    # @raise [Blueprinter::Errors::StreamingRootUnsupported] if :root is given
    # @raise [Blueprinter::Errors::StreamingMetaUnsupported] if :meta is given
    # @raise [Blueprinter::Errors::UnknownView] if the view is not defined
    def initialize(source, blueprint, options = {})
      unless source.is_a?(Enumerable)
        raise ArgumentError, 'source must be an Enumerable that responds to #each'
      end
      raise Errors::StreamingRootUnsupported if options.key?(:root)
      raise Errors::StreamingMetaUnsupported if options.key?(:meta)
      unless blueprint.view?(options[:view] || :default)
        raise Errors::UnknownView, "View '#{options[:view] || :default}' is not defined"
      end

      @source = source
      @blueprint = blueprint
      @options = options
      @mutex = Mutex.new
      @closed = false
      @started = false
    end

    # Streams the serialized JSON array, yielding one String chunk at a time.
    #
    # Without a block, returns an Enumerator that streams the chunks when it is
    # consumed; in that case the source is still not touched until consumption.
    #
    # @yieldparam chunk [String] a fragment of the JSON array
    # @return [Enumerator, nil] an Enumerator when called without a block
    # @raise [IOError] if the body was already consumed or closed
    def each
      return to_enum(:each) unless block_given?

      @mutex.synchronize do
        raise IOError, 'stream has already been consumed' if @started
        raise IOError, 'stream is closed' if @closed

        @started = true
      end

      first = true
      begin
        yield '['
        prepared_source.each do |object|
          yield ',' unless first
          first = false
          yield Blueprinter.configuration.jsonify(
            @blueprint.streamed_element_hash(object, @options)
          )
        end
        yield ']'
      ensure
        close
      end
      nil
    end

    # Finishes the stream, closing the source exactly once when the source
    # responds to +#close+. Safe to call multiple times and safe to call while
    # (or instead of) consuming the body.
    def close
      do_close
    end

    # @return [Boolean] whether the source has been closed
    def closed?
      @mutex.synchronize { @closed }
    end

    private

    def prepared_source
      return @prepared_source if defined?(@prepared_source)

      view_name = @options[:view] || :default
      @prepared_source = @blueprint.pre_render_collection(@source, view_name, @options)
      @prepared_source
    end

    def do_close
      source_to_close = @mutex.synchronize do
        return if @closed

        @closed = true
        @source if @source.respond_to?(:close)
      end

      source_to_close&.close
    end
  end
end
