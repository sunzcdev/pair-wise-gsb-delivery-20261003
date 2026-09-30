require_relative 'node'
require_relative 'function_registry'
require_relative '../streaming'

module Dentaku
  module AST
    class Function < Node
      attr_reader :args

      # @return [Integer] with the number of significant decimal digits to use.
      DIG = Float::DIG + 1

      def initialize(*args)
        @args = args
      end

      def accept(visitor)
        visitor.visit_function(self)
      end

      def function_name
        name = self.class.registry_name
        name.to_s.upcase
      end

      def self.registry_name
        if name && !name.to_s.start_with?("Dentaku::")
          name.to_s
        else
          self.name.to_s.split("::").last
        end
      end

      # Streaming-aware functions override this; by default a streaming
      # argument is rejected loudly rather than materialized implicitly.
      def stream_value(_session, _stream, _stream_node, _context)
        raise Dentaku::StreamNotSupportedError.new(
          nil, operation: function_name, location: location
        )
      end

      def supports_streaming?
        false
      end

      # Whether a zero-argument call site (e.g. NAME()) may be parsed for
      # this function; arity is then enforced at evaluation time.
      def self.allows_empty_call?
        false
      end

      # Runtime arity guard used for functions the parser instantiates with
      # zero args (the parser cannot distinguish "zero-arg call" from a plain
      # reference while the function token is on the stack).
      def validate_arg_count!
        min = self.class.min_param_count
        max = self.class.max_param_count

        if min && @args.length < min
          expected = (max == min ? min : min..)
          raise Dentaku::ArgumentError.for(
            :too_few_arguments,
            function_name: function_name, expected: expected, actual: @args.length
          )
        end

        if max && @args.length > max
          expected = (max == min ? min : min..max)
          raise Dentaku::ArgumentError.for(
            :wrong_number_of_arguments,
            function_name: function_name, expected: expected, actual: @args.length
          )
        end
      end

      protected :validate_arg_count!

      def dependencies(context = {})
        @args.each_with_index
             .flat_map { |a, _| a.dependencies(context) }
      end

      # volatile functions may not be evaluated during dependency
      # resolution; registry-generated classes override this per the
      # volatile: option passed at registration
      def self.volatile?
        false
      end

      def self.get(name)
        registry.get(name)
      end

      def self.register(name, type, implementation, volatile: false)
        registry.register(name, type, implementation, volatile: volatile)
      end

      def self.register_class(name, function_class)
        registry.register_class(name, function_class)
      end

      def self.registry
        @registry ||= FunctionRegistry.new
      end

      private

      def compute_pure?
        !self.class.volatile? && args.all?(&:pure?)
      end
    end
  end
end
