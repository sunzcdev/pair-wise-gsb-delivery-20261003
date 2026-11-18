# frozen_string_literal: true

class RedisClient
  module Observability
    # Observers receive read-only notifications about commands, attempts and
    # connections. They are purely diagnostic:
    #
    # * an observer may not alter the command, the reply or raised errors;
    # * an observer failure must never change the outcome of the real call
    #   (see +SafeObserver+);
    # * observers are never handed the reconnection logic and must not execute
    #   commands themselves - a write whose result is unknown stays unknown.
    #
    # All callbacks are empty by default; implement only the ones you need.
    class Observer
      # A logical command (or pipeline) started. Always paired with a
      # +command_finished+ event sharing the same +call_id+, regardless of how
      # many reconnection attempts happened underneath.
      def command_started(event)
      end

      def command_finished(event)
      end

      # A single concrete attempt. Retried commands emit one attempt per try,
      # each with a distinct +attempt_id+ and increasing +attempt+ number.
      def attempt_started(event)
      end

      def attempt_finished(event)
      end

      # A connection was (re)established, including its authentication prelude.
      def connect_started(event)
      end

      def connect_finished(event)
      end
    end

    # Default observer used when instrumentation is not enabled. It does
    # nothing and allocates no events, so uninstrumented clients keep the same
    # runtime characteristics as before.
    class NullObserver < Observer
      include Singleton

      def command_started(_event); end
      def command_finished(_event); end
      def attempt_started(_event); end
      def attempt_finished(_event); end
      def connect_started(_event); end
      def connect_finished(_event); end
    end

    # Wraps a user observer so that any exception it raises is swallowed (and
    # optionally reported) instead of corrupting the real Redis call. The
    # genuine reply or error is always propagated untouched.
    class SafeObserver < Observer
      def initialize(observer, on_error: nil)
        @observer = observer
        @on_error = on_error
      end

      %i[
        command_started command_finished
        attempt_started attempt_finished
        connect_started connect_finished
      ].each do |callback|
        define_method(callback) do |event|
          @observer.public_send(callback, event)
        rescue Exception => error # rubocop:disable Lint/RescueException
          # Observers are side-channel diagnostics; their failures must not
          # modify the real reply or bubble up through the client.
          begin
            @on_error&.call(error, callback, event)
          rescue StandardError
            # Reporting itself failed; nothing else we can safely do.
          end
        end
      end
    end

    # A minimal collector, mostly useful for tests and local debugging. It
    # records every event in order and never raises.
    class RecordingObserver < Observer
      attr_reader :events

      def initialize
        @events = []
      end

      %i[
        command_started command_finished
        attempt_started attempt_finished
        connect_started connect_finished
      ].each do |callback|
        define_method(callback) do |event|
          @events << [callback, event]
        end
      end

      def events_named(name)
        @events.select { |_callback, event| event.name == name }
      end

      def clear
        @events.clear
      end
    end
  end
end
