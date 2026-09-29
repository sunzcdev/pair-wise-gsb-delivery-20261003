# frozen_string_literal: true

require "dry/matcher"

module Dry
  module Transaction
    # Matcher for transaction results.
    #
    # Failure cases can be matched by:
    #
    # * a step name (`m.failure(:validate)`) — matches failures raised by
    #   the step with that name, even inside a nested transaction
    # * an address (`m.failure([:account, :validate])`) — matches failures
    #   whose structured address starts with the given steps, so a
    #   single-item array matches any failure within the nested
    #   transaction used as that step's operation
    #
    # When multiple failure cases match, the first one defined in the
    # block wins, so list more specific addresses before broader ones.
    ResultMatcher = Dry::Matcher.new(
      success: Dry::Matcher::Case.new(
        match: -> result { result.success? },
        resolve: -> result { result.value! }
      ),
      failure: Dry::Matcher::Case.new(
        match: -> result, step_name = nil {
          if step_name.is_a?(Array)
            result.failure? && result.failure.address.take(step_name.size) == step_name
          elsif step_name
            result.failure? && result.failure.leaf.step.name == step_name
          else
            result.failure?
          end
        },
        resolve: -> result { result.failure.value }
      )
    )
  end
end
