# frozen_string_literal: true

require_relative 'non_retryable_exception'

module Shoryuken
  module Middleware
    module Server
      # Feature-name alias for the acknowledgement-safe failure handoff entry.
      FailureHandoff = NonRetryableException unless const_defined?(:FailureHandoff)
    end
  end
end
