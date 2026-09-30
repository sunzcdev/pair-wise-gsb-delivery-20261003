# frozen_string_literal: true

# rubocop:disable Naming/ConstantName
# rubocop:disable Style/MutableConstant

module Dry
  module Monads
    id = -> x { x }

    # List of default traverse functions for types.
    # It is implicitly used by List#traverse for
    # making common cases easier to handle.
    #
    # For Validated the element's own merge rule is used to lift a single
    # error into the rule's accumulator. The default rule wraps errors with
    # a List while custom rules stay in control of the lifting, so the rule
    # configured on the elements survives traversal.
    Traverse = {
      Validated => -> el { el.alt_map { |error| el.rule.lift(error) } }
    }

    # By default the identity function is used
    Traverse.default = id
    Traverse.freeze
  end
end

# rubocop:enable Style/MutableConstant
# rubocop:enable Naming/ConstantName
