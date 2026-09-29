# frozen_string_literal: true

module Sinatra
  # Pull-based Server-Sent Events response body.
  #
  # The producer block receives an Emitter and submits events, comments or
  # heartbeats one at a time. Nothing is produced up front: the block runs
  # inside a Fiber and each frame the consumer pulls drives exactly as much
  # production work as that frame needs. No background threads are started.
  #
  # Closing the body raises Sinatra::SSE::Cancelled inside the producer, so
  # it can stop producing and clean up after itself.
  #
  #   sse = Sinatra::SSE.new do |out|
  #     out.event 'hello', id: 1, event: 'greeting', retry: 3000
  #     out.comment 'keep-alive'
  #     out.heartbeat
  #   end
  class SSE
    DEFAULT_HEARTBEAT_INTERVAL = 15
    MONOTONIC_CLOCK = -> { Process.clock_gettime(Process::CLOCK_MONOTONIC) }

    # Raised inside the producer block when the response body is closed.
    class Cancelled < StandardError; end

    # Producer-side interface handed to the block given to SSE.new. Encodes
    # data, id, event and retry fields into SSE wire format. Field values
    # other than data must not contain CR or LF characters; multi-line data
    # is split into one +data:+ line per line, as the SSE framing rules
    # require.
    class Emitter
      def initialize(sink, clock, heartbeat_interval, cancelled)
        @sink = sink
        @clock = clock
        @heartbeat_interval = heartbeat_interval
        @cancelled = cancelled
        @last_frame_at = clock.call
      end

      def event(data, id: nil, event: nil, retry: nil)
        retry_after = binding.local_variable_get(:retry)
        lines = []
        lines << "event: #{field(event, :event)}" unless event.nil?
        lines << "id: #{field(id, :id)}" unless id.nil?
        lines << "retry: #{retry_value(retry_after)}" unless retry_after.nil?
        data.to_s.split(/\r\n|[\r\n]/, -1).each { |line| lines << "data: #{line}" }
        write lines.join("\n") << "\n\n"
      end

      def comment(text)
        write ": #{field(text, :comment)}\n\n"
      end

      # Emits a heartbeat comment, but only if at least heartbeat_interval
      # seconds (according to the injected clock) have passed since the last
      # frame was written. Returns whether a frame was emitted.
      def heartbeat
        return false if @clock.call - @last_frame_at < @heartbeat_interval

        write ": heartbeat\n\n"
        true
      end

      def cancelled?
        @cancelled.call
      end

      private

      def field(value, name)
        string = value.to_s
        raise ArgumentError, "SSE #{name} field must not contain CR or LF characters" if string.match?(/[\r\n]/)

        string
      end

      def retry_value(value)
        string = value.to_s
        raise ArgumentError, 'SSE retry field must be a non-negative integer' unless string.match?(/\A\d+\z/)

        string
      end

      def write(frame)
        raise Cancelled if cancelled?

        @last_frame_at = @clock.call
        @sink.call(frame)
        self
      end
    end

    def initialize(heartbeat_interval: DEFAULT_HEARTBEAT_INTERVAL, clock: MONOTONIC_CLOCK, &producer)
      raise ArgumentError, 'Sinatra::SSE requires a producer block' unless producer

      @producer = producer
      @clock = clock
      @heartbeat_interval = heartbeat_interval
      @closed = false
      @fiber = nil
    end

    def each
      return enum_for(:each) unless block_given?
      return if closed?

      @fiber ||= Fiber.new { run_producer }
      while (frame = @fiber.resume)
        yield frame
      end
    end

    # Hands the cancellation signal to the producer by raising
    # Sinatra::SSE::Cancelled inside it, then marks the body closed.
    def close
      return if closed?

      @closed = true
      @fiber.raise(Cancelled) if @fiber&.alive?
      nil
    rescue Cancelled
      nil
    end

    def closed?
      @closed
    end

    private

    def run_producer
      emitter = Emitter.new(->(frame) { Fiber.yield(frame) }, @clock, @heartbeat_interval, -> { closed? })
      @producer.call(emitter)
      nil
    end
  end
end
