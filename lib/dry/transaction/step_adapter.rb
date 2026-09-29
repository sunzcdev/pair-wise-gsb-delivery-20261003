# frozen_string_literal: true

require "dry/transaction/callable"

module Dry
  module Transaction
    # @api private
    class StepAdapter
      def self.[](adapter, operation, options)
        if adapter.is_a?(self)
          adapter.with(operation, options)
        else
          new(adapter, operation, options)
        end
      end

      attr_reader :adapter
      attr_reader :operation
      attr_reader :options

      def initialize(adapter, operation, options)
        @adapter = case adapter
                   when Proc, Method
                     adapter
                   else
                     adapter.method(:call)
                   end

        @operation = Callable[operation]

        @options = options

        parameters = @adapter.parameters

        @yields = parameters.any? { |type, _| type == :block }

        # Adapters accepting a 4th argument receive the keyword arguments
        # separately; adapters with the legacy 3-argument signature (e.g.
        # custom adapters) are called without them
        @accepts_kwargs = parameters.count { |type, _| type != :block } >= 4
      end

      def yields?
        @yields
      end

      def accepts_kwargs?
        @accepts_kwargs
      end

      def call(args, kwargs = {}, &block)
        if accepts_kwargs?
          adapter.(operation, options, args, kwargs, &block)
        else
          adapter.(operation, options, args, &block)
        end
      end

      def with(operation = self.operation, new_options = {})
        self.class.new(adapter, operation, options.merge(new_options))
      end
    end
  end
end
