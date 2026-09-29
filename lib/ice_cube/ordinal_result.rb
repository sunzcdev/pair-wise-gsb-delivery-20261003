module IceCube
  # The answer to an ordinal positioning query (Schedule#occurrence_at) or
  # a reverse counting query (Schedule#occurrences_before).
  #
  # #strategy tells the caller how the answer was produced:
  #
  # * :arithmetic  - computed directly from the rule in O(1); no occurrences
  #                  were enumerated and the answer is exact.
  # * :enumeration - the schedule's combination of rules cannot be computed
  #                  directly, so occurrences were enumerated (bounded by the
  #                  query's budget). #iterations reports how many
  #                  occurrences were walked to reach the answer.
  class OrdinalResult
    attr_reader :value, :strategy, :iterations

    def initialize(value, strategy:, iterations: 0)
      @value = value
      @strategy = strategy
      @iterations = iterations
    end

    # True when the answer was computed arithmetically, without enumerating
    # any occurrences.
    def exact?
      strategy == :arithmetic
    end

    # True when the answer came from budget-constrained enumeration.
    def fallback?
      !exact?
    end

    # The located occurrence (or nil) for Schedule#occurrence_at.
    def time
      value
    end

    # The number of occurrences for Schedule#occurrences_before.
    def count
      value
    end

    def ==(other)
      other.is_a?(OrdinalResult) &&
        value == other.value &&
        strategy == other.strategy
    end

    def inspect
      "#<IceCube::OrdinalResult value=#{value.inspect} " \
        "strategy=#{strategy} iterations=#{iterations}>"
    end
  end
end
