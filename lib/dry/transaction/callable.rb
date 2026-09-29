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

      attr_reader :operation, :arity

      def initialize(operation)
        @operation = case operation
                     when Proc, Method
                       operation
                     else
                       operation.method(:call)
                     end

        @arity = @operation.arity
      end

      def call(*args, **kwargs, &block)
        if arity.zero?
          operation.(&block)
        else
          args, kwargs = self.class.split_args(args, kwargs)
          operation.(*args, **kwargs, &block)
        end
      end

      def parameters
        operation.parameters
      end

      # Non-lambda procs accept any arguments without raising, so their
      # signatures don't need to be checked
      def strict?
        !operation.is_a?(Proc) || operation.lambda?
      end

      # Splits a trailing plain Hash off the positional arguments and returns
      # it as keyword arguments, but only when no explicit keyword arguments
      # were given.
      def self.split_args(args, kwargs)
        if kwargs.empty? && trailing_hash?(args)
          [args[0...-1], args.last]
        else
          [args, kwargs]
        end
      end

      # Ruby 2.7 gives a deprecation warning about passing a hash of parameters as the last argument
      # to a method. Ruby 3.0 outright disallows it. This checks for that condition, but explicitly
      # uses instance_of? rather than is_a? or kind_of?, because Hash like objects, specifically
      # HashWithIndifferentAccess objects are provided by Rails as controller parameters, and often
      # passed to dry-rb validators.
      # In this case, it's better to leave the object as it's existing type, rather than implicitly
      # convert it in to a hash with the double-splat (**) operator.
      def self.trailing_hash?(args)
        kwargs = args.last
        kwargs.instance_of?(Hash) && !kwargs.empty?
      end
    end
  end
end
