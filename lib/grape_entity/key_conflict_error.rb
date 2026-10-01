# frozen_string_literal: true

module Grape
  class Entity
    # Raised in strict conflict mode when two exposures end up writing to the
    # same output key and no per-key adjudicator has been declared for it.
    #
    # The error message always carries the source paths of both exposures
    # involved in the collision so it is visible which value would have been
    # silently overwritten. Source paths of collection elements include the
    # element index, e.g. `MyEntity.name[2]`.
    class KeyConflictError < StandardError
      def initialize(key, sources)
        first_source, second_source = sources
        super(<<~MSG)
          Conflicting exposures for output key #{key.inspect}:
            - #{first_source}
            - #{second_source}
          Declare a per-key adjudicator to resolve this conflict, e.g.:
            conflict_resolvers: { #{key}: ->(key, existing, incoming, sources) { ... } }
          Blanket last-write-wins rules are not supported in strict conflict mode.
        MSG
      end
    end
  end
end
