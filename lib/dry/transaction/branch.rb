# frozen_string_literal: true

require "dry/transaction/stack"

module Dry
  module Transaction
    # A named stack of sub-steps within a branch step, along with the
    # optional name of the step to join after the sub-steps succeed.
    #
    # @api private
    class Branch
      attr_reader :name
      attr_reader :steps
      attr_reader :join_to

      def initialize(name:, steps:, join_to: nil)
        @name = name
        @steps = steps
        @join_to = join_to
      end

      def with_steps(steps)
        self.class.new(name: name, steps: steps, join_to: join_to)
      end

      def stack
        @stack ||= Stack.new(steps)
      end
    end
  end
end
