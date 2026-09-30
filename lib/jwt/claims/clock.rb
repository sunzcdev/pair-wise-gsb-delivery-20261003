# frozen_string_literal: true

module JWT
  module Claims
    # Resolves the single observation time used by the time based claim
    # verifiers ('iat', 'nbf' and 'exp') during one verification round.
    #
    # The clock is either a Time-like object or anything responding to +#call+
    # (for example a Proc). A callable may return either a Time or a Numeric
    # NumericDate expressed in seconds since the epoch.
    #
    # @api private
    module Clock
      module_function

      # @param clock [Time, Numeric, #call] the clock to observe.
      # @return [Time] the observed time.
      # @raise [ArgumentError] if the clock does not resolve to a Time or Numeric.
      def resolve(clock)
        value = clock.respond_to?(:call) ? clock.call : clock
        value = ::Time.at(value.to_f) if value.is_a?(::Numeric)

        return value if value.is_a?(::Time)

        raise ArgumentError, 'clock must be a Time or a Numeric NumericDate, or a callable returning one'
      end
    end
  end
end
