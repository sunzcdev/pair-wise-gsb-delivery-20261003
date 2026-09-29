# frozen_string_literal: true

module Dry
  module Transaction
    # @api private
    class Callable
      def self.[](callable)
        if callable.is_a?(self)
          callable
        elsif callable.nil?
          nil
        else
          new(callable)
        end
      end

      attr_reader :operation, :arity, :parameters

      def initialize(operation)
        @operation = case operation
                     when Proc, Method
                       operation
                     else
                       operation.method(:call)
                     end

        @arity = @operation.arity
        @parameters = @operation.parameters
      end

      def call(*args, **kwargs, &block)
        if arity.zero?
          operation.(&block)
        else
          operation.(*args, **kwargs, &block)
        end
      end
    end
  end
end
