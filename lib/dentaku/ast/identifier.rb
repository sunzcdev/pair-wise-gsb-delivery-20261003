require_relative '../exceptions'
require 'dentaku/string_casing'

module Dentaku
  module AST
    class Identifier < Node
      include StringCasing
      attr_reader :identifier, :case_sensitive

      def initialize(token, options = {})
        @case_sensitive = options.fetch(:case_sensitive, false)
        @identifier = standardize_case(token.value)
      end

      def value(context = {})
        v = context.fetch(identifier) do
          raise UnboundVariableError.new([identifier]),
                "no value provided for variables: #{identifier}"
        end

        case v
        when Node
          value = v.value(context)
          context[identifier] = value if Dentaku.cache_identifier?
          value
        when Proc
          v.call
        else
          v
        end
      end

      def dependencies(context = {})
        context.key?(identifier) ? dependencies_of(context[identifier], context) : [identifier]
      end

      def accept(visitor)
        visitor.visit_identifier(self)
      end

      def partial(evaluator, guarded)
        # an explicitly bound nil is a real value, not a missing input,
        # so key? (rather than a truthiness check) distinguishes the two
        return self unless evaluator.bound?(identifier)

        bound = evaluator.bound_value(identifier)

        if bound.is_a?(AST::Node)
          # a stored formula: reduce the formula itself so its still
          # unknown inputs survive in the residual, instead of trying to
          # evaluate it eagerly
          return evaluator.reduce(bound, guarded: guarded)
        end

        evaluator.constant_for(evaluator.lookup(identifier))
      rescue ::StandardError => e
        evaluator.record_error(e)
        self
      end

      def to_s
        identifier.to_s
      end

      private

      def dependencies_of(node, context)
        node.respond_to?(:dependencies) ? node.dependencies(context) : []
      end
    end
  end
end
