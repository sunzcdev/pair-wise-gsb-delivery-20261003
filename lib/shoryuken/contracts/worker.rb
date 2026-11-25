# frozen_string_literal: true

module Shoryuken
  module Contracts
    # Validates the options a worker passes to `shoryuken_options`, so a mistyped key (e.g. `auto_delte`) is reported
    # when the worker class loads instead of being silently ignored
    class Worker < Base
      self.error_messages = load_error_messages('worker')

      # Worker options Shoryuken reads
      KNOWN_KEYS = %w[
        queue
        delete
        auto_delete
        auto_visibility_timeout
        retry_intervals
        batch
        body_parser
        non_retryable_exceptions
        failure_handoff
        failure_handoff_adapter
        handoff
        handoff_adapter
        failure_handover
        handover
        failure_destination
        destination_adapter
        dead_letter_handoff
        dead_letter_destination
        dlq_destination
        failure_sink
        failure_target
      ].freeze

      # Option keys equivalently naming a failure-handoff adapter are accepted
      # even when spelled with a custom prefix (e.g. ops_failure_destination).
      HANDOFF_KEY_PATTERN = /handoff|handover|destination|dead_?letter|\bdlq\b|failure_(?:sink|target|adapter)/i

      private_constant :KNOWN_KEYS

      class << self
        private

        # Checks for a body parser BodyParser can use, so a typo fails when the worker loads instead of every message
        # getting a nil body
        #
        # @param value [Object] the body_parser option
        # @return [Boolean] true if the value is nil, :json, :text, a Proc or responds to parse or load
        def valid_body_parser?(value)
          [nil, :json, :text].include?(value) ||
            value.is_a?(Proc) ||
            value.respond_to?(:parse) ||
            value.respond_to?(:load)
        end
      end

      optional('body_parser') { |value| valid_body_parser?(value) }

      virtual do |data|
        unknown = data.keys.map(&:to_s) - KNOWN_KEYS
        unknown = unknown.reject { |key| key.match?(HANDOFF_KEY_PATTERN) }
        unknown.map { |key| [[key], :unknown] }
      end
    end
  end
end
