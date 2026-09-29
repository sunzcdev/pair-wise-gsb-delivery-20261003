# frozen_string_literal: true

require "dry/matcher"
require "dry/transaction/step_failure"

module Dry
  module Transaction
    # Matcher for transaction results.
    #
    # Failure cases accept an optional failure address:
    #
    # * a step name (+:validate+) matches failures raised by that leaf step,
    #   preserving the original single-level behaviour
    # * an array of step names (+[:create_user, :validate]+) matches the full
    #   failure address composed through nested transactions, or any
    #   parent-level prefix of it, so +[:create_user]+ matches every failure
    #   beneath the +create_user+ step
    #
    # Overlapping cases are evaluated in the order they are defined and the
    # first match wins, so define more specific addresses before broader
    # ones.
    ResultMatcher = Dry::Matcher.new(
      success: Dry::Matcher::Case.new(
        match: -> result { result.success? },
        resolve: -> result { result.value! }
      ),
      failure: Dry::Matcher::Case.new(
        match: -> result, address = nil {
          if address
            result.failure? &&
              result.failure.is_a?(StepFailure) &&
              result.failure.matches?(address)
          else
            result.failure?
          end
        },
        resolve: -> result { result.failure.value }
      )
    )
  end
end
