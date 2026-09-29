# frozen_string_literal: true

require "dry/transaction/branch_builder"

module Dry
  module Transaction
    class DSL < Module
      def initialize(step_adapters:)
        super()
        @step_adapters = step_adapters

        define_steps
        define_dsl
      end

      def inspect
        "Dry::Transaction::DSL(#{@step_adapters.keys.sort.join(", ")})"
      end

      private

      def define_steps
        module_eval do
          define_method(:steps) do
            @steps ||= []
          end
        end
      end

      def define_dsl
        module_exec(@step_adapters) do |step_adapters|
          step_adapters.each do |adapter_name, adapter|
            if adapter_name.to_sym == :branch
              define_method(adapter_name) do |step_name, with: nil, selector: nil, &block|
                steps << BranchBuilder.build(
                  adapter: adapter,
                  step_adapters: step_adapters,
                  name: step_name,
                  operation_name: with,
                  selector: selector,
                  &block
                )
              end
            else
              define_method(adapter_name) do |step_name, with: nil, **options|
                operation_name = with

                steps << Step.new(
                  adapter: adapter,
                  name: step_name,
                  operation_name: operation_name,
                  operation: nil, # operations are resolved only when transactions are instantiated
                  options: options
                )
              end
            end
          end
        end
      end
    end
  end
end
