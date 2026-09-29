# frozen_string_literal: true

module Dry
  module Transaction
    # A named sub-step stack within a branch step, along with the explicit
    # mapping used to join its result back into the main step flow.
    #
    # @api private
    class BranchDefinition
      IDENTITY = -> x { x }

      attr_reader :name
      attr_reader :steps
      attr_reader :join

      def initialize(name:, steps:, join: nil)
        @name = name
        @steps = steps
        @join = join || IDENTITY
      end

      def with_steps(new_steps)
        self.class.new(name: name, steps: new_steps, join: join)
      end
    end
  end
end
