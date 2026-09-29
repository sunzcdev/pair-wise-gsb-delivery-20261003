# frozen_string_literal: true

require "dry/transaction/step_group"

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

          define_method(:check_group) do |step_name, with:, **options|
            check_names = Array(with)

            sub_steps = check_names.map do |check_name|
              Step.new(
                adapter: step_adapters[:step],
                name: check_name,
                operation_name: check_name,
                operation: nil, # operations are resolved only when transactions are instantiated
                options: options
              )
            end

            steps << StepGroup.new(
              adapter: step_adapters[:step],
              name: step_name,
              operation_name: check_names,
              operation: nil, # operations are resolved only when transactions are instantiated
              options: options,
              steps: sub_steps
            )
          end
        end
      end
    end
  end
end
