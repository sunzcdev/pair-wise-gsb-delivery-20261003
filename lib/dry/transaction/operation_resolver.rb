# frozen_string_literal: true

module Dry
  module Transaction
    class OperationResolver < Module
      def initialize(container)
        module_exec(container) do |ops_container|
          define_method :initialize do |**kwargs|
            operation_kwargs = self.class.steps.select(&:operation_name).to_h { |step|
              operation = kwargs.fetch(step.name) {
                if ops_container&.key?(step.operation_name)
                  ops_container[step.operation_name]
                else
                  nil
                end
              }

              [step.name, operation]
            }

            check_kwargs = self.class.steps.select(&:group?).flat_map(&:check_names).uniq.to_h { |check_name|
              operation = kwargs.fetch(check_name) {
                if ops_container&.key?(check_name)
                  ops_container[check_name]
                else
                  nil
                end
              }

              [check_name, operation]
            }

            super(**kwargs, **check_kwargs, **operation_kwargs)
          end
        end
      end
    end
  end
end
