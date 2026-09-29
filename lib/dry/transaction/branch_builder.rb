# frozen_string_literal: true

require "dry/transaction/branch_definition"
require "dry/transaction/step"

module Dry
  module Transaction
    # Collects the named branches declared inside a `branch` step definition
    # and builds the branch step itself.
    #
    # @api private
    class BranchBuilder
      attr_reader :branches

      def self.build(adapter:, step_adapters:, name:, operation_name:, selector:, &block)
        builder = new(step_adapters)
        builder.instance_eval(&block)

        Step.new(
          adapter: adapter,
          name: name,
          operation_name: operation_name,
          operation: selector,
          options: {branches: builder.branches}
        )
      end

      def initialize(step_adapters)
        @branches = {}
        @steps = nil

        step_adapters.each do |adapter_name, adapter|
          if adapter_name.to_sym == :branch
            define_singleton_method(adapter_name) do |step_name, with: nil, selector: nil, &block|
              @steps << BranchBuilder.build(
                adapter: adapter,
                step_adapters: step_adapters,
                name: step_name,
                operation_name: with,
                selector: selector,
                &block
              )
            end
          else
            define_singleton_method(adapter_name) do |step_name, with: nil, **options|
              @steps << Step.new(
                adapter: adapter,
                name: step_name,
                operation_name: with,
                operation: nil, # operations are resolved only when transactions are instantiated
                options: options
              )
            end
          end
        end
      end

      def on(branch_name, join: nil, &block)
        outer_steps = @steps
        @steps = []
        instance_eval(&block)
        @branches[branch_name.to_sym] = BranchDefinition.new(
          name: branch_name.to_sym,
          steps: @steps,
          join: join
        )
      ensure
        @steps = outer_steps
      end
    end
  end
end
