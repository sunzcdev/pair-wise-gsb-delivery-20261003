require_relative '../function'
require_relative '../../exceptions'

module Dentaku
  module AST
    # LET(name1, value1, name2, value2, ..., body)
    #
    # Binds names to values for the duration of the body expression only, so
    # a repeated subexpression can be written (and evaluated) once. Bindings
    # are evaluated strictly left to right: a value expression may reference
    # names bound before it, but never names bound after it. Rebinding a name
    # shadows the earlier binding, and any local binding shadows an external
    # variable of the same name. Local bindings live in a private copy of the
    # evaluation context, so they are never written back to the calculator's
    # memory -- even when the body raises.
    class Let < Function
      def self.min_param_count
        3
      end

      def self.max_param_count
        Float::INFINITY
      end

      def initialize(*args)
        super
        validate_structure!
      end

      def value(context = {})
        local_context = context.dup

        bindings.each do |name, value_expression|
          local_context[name] = value_expression.value(local_context)
        end

        body.value(local_context)
      end

      def dependencies(context = {})
        local_context = context.dup
        bound_names = []

        binding_dependencies = bindings.flat_map do |name, value_expression|
          value_expression.dependencies(local_context).tap do
            bound_names << name
            local_context.delete(name)
          end
        end

        (binding_dependencies + body.dependencies(local_context)).reject do |dependency|
          bound_names.any? do |name|
            dependency == name || dependency.start_with?("#{name}.")
          end
        end
      end

      private

      def bindings
        @bindings ||= args[0...-1].each_slice(2).map do |name_node, value_expression|
          [name_node.identifier, value_expression]
        end
      end

      def body
        args.last
      end

      def validate_structure!
        if args.length < 3 || args.length.even?
          raise ParseError.for(:node_invalid),
                "LET() requires name/value binding pairs followed by a body expression"
        end

        args[0...-1].each_slice(2) do |name_node, _value_expression|
          next if name_node.is_a?(Identifier)

          raise ParseError.for(:node_invalid),
                "LET() requires binding names to be identifiers"
        end
      end
    end
  end
end

Dentaku::AST::Function.register_class(:let, Dentaku::AST::Let)
