# frozen_string_literal: true

module Faraday
  # ResponseStages is an optional two-stage view attached to a {Faraday::Error}
  # when the RaiseError middleware is configured with `response_stages: true`.
  #
  # It keeps separate snapshots of the response so that troubleshooting can
  # tell which fields came straight from the adapter and which were changed by
  # response middleware positioned before RaiseError in the stack.
  class ResponseStages
    # @return [Hash, Faraday::Response] the response as captured when the
    #   adapter finished, before any response middleware transformed it.
    attr_reader :raw

    # Alias of {#raw} describing the same adapter-fresh snapshot.
    #
    # @return [Hash, Faraday::Response] the response as captured when the
    #   adapter finished.
    attr_reader :adapter

    # @return [Hash, Faraday::Response] the response at the point where the
    #   error was raised, after earlier response middleware had run.
    attr_reader :current

    # @param raw [Hash, Faraday::Response] the adapter response snapshot.
    # @param current [Hash, Faraday::Response] the response snapshot when the
    #   error was raised.
    def initialize(raw:, current:)
      @raw = raw
      @adapter = raw
      @current = current
      freeze
    end

    # @return [Array<Hash, Faraday::Response>] the raw adapter stage followed
    #   by the current stage at raise time.
    def to_a
      [@raw, @current]
    end

    def inspect
      "#<#{self.class} raw=#{@raw.inspect} current=#{@current.inspect}>"
    end
  end
end
