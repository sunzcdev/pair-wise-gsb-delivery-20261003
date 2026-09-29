# frozen_string_literal: true

module Dry
  module Transaction
    class OperationResolver < Module
      def initialize(container)
        module_exec(container) do |ops_container|
          define_method :initialize do |**kwargs|
            all_steps = self.class.steps.flat_map { |step| [step, *step.sub_steps] }

            operation_kwargs = all_steps.select(&:operation_name).to_h { |step|
              operation = kwargs.fetch(step.name) {
                if ops_container&.key?(step.operation_name)
                  ops_container[step.operation_name]
                else
                  nil
                end
              }

              [step.name, operation]
            }

            super(**kwargs, **operation_kwargs)
          end
        end
      end
    end
  end
end
