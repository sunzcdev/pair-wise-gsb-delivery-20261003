# frozen_string_literal: true

require_relative 'non_retryable_exception'

module Shoryuken
  module Middleware
    module Server
      # Feature-name alias for dead-letter/failure handoff deployments.
      DeadLetterHandoff = NonRetryableException unless const_defined?(:DeadLetterHandoff)
    end
  end
end
