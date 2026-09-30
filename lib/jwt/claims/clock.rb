# frozen_string_literal: true

module JWT
  module Claims
    # Provides the single point in time against which all time based claim
    # checks of one verification round are evaluated.
    #
    # A clock is any object responding to +#call+ and returning a +Time+
    # (for example +-> { Time.now }+). An already resolved +Time+ may also be
    # given, which makes the verification fully deterministic.
    module Clock
      # Default clock used when no explicit clock is provided.
      DEFAULT_CLOCK = -> { ::Time.now }

      class << self
        # Resolves the clock to a single observation time for the whole
        # verification round. The clock is invoked exactly once so that
        # exp, nbf and iat checks can never observe different times.
        #
        # @param clock [#call, Time, Integer, Float, nil] the clock to observe.
        # @return [Time] the observed time.
        def resolve(clock)
          observed = if clock.respond_to?(:call)
                       clock.call
                     else
                       clock
                     end

          time_from(observed || DEFAULT_CLOCK.call)
        end

        # Converts the observed value into a Time, supporting fractional
        # NumericDate values.
        #
        # @param value [Time, Numeric] the observed value.
        # @return [Time] the observed time.
        def time_from(value)
          return value if value.is_a?(::Time)

          ::Time.at(value.to_f)
        end
      end
    end
  end
end
