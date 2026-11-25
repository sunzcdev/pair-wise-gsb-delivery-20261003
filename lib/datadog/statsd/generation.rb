# frozen_string_literal: true

module Datadog
  class Statsd
    # An immutable configuration generation.
    #
    # Messages submitted through a generation keep using its serializer,
    # forwarder and telemetry even after the client moves to a newer generation.
    # This prevents queued packets and their write statistics from being
    # attributed to tags that were reloaded after submission.
    class Generation
      attr_reader :id
      attr_reader :tags
      attr_reader :serializer
      attr_reader :forwarder

      def initialize(id:, tags:, serializer:, forwarder:)
        @id = id
        @tags = tags
        @serializer = serializer
        @forwarder = forwarder
      end

      def telemetry
        forwarder.telemetry
      end

      def close
        forwarder.close
      end
    end
  end
end
