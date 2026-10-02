# frozen_string_literal: true

require 'blueprinter/blueprinter_error'

module Blueprinter
  module Errors
    # Raised when a :preload callable does not provide a value for an object
    # that is being rendered. Blueprinter never falls back to per-object
    # queries, so every rendered object must have an entry in the returned
    # Hash (use nil to represent an empty association).
    class MissingPreloadResult < BlueprinterError
      def initialize(association_name)
        super(
          "Preloaded association :#{association_name} is missing an entry for an object being rendered. " \
          'The :preload callable must return a Hash with an entry for every object it receives ' \
          '(use nil for an empty association).'
        )
      end
    end
  end
end
