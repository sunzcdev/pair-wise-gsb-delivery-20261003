require_relative '../function'
require_relative '../../exceptions'

module Dentaku
  module AST
    # Local bindings scoped to a single expression, useful when a complex
    # formula repeats the same subexpression:
    #
    #     LET(total, price * quantity, total - total * discount_rate)
    #
    # Bindings are evaluated in definition order (let* semantics): each
    # value expression can see the names defined before it, but not the
    # ones defined after it -- a reference to a not-yet-defined name
    # resolves against the outer scope (calculator memory / passed data)
    # instead. A later binding with a duplicate name shadows the earlier
    # one, and every local name shadows a same-named variable from the
    # outer scope for the rest of the expression.
    #
    # Bindings never leak: they live in a private copy of the evaluation
    # context, so the calculator's stored memory is never modified, and
    # dependency analysis treats locally bound names as internal to the
    # expression.
    class Let < Function
      def self.min_param_count
        3
      end

      def self.max_param_count
        Float::INFINITY
      end

      def initialize(*args)
        super
        validate!
      end

      def value(context = {})
        local_scope = context.dup

        bindings.each do |identifier, expression|
          local_scope[identifier] = expression.value(local_scope)
        end

        body.value(local_scope)
      end

      def dependencies(context = {})
        visible = context.dup
        bound = []
        deps = []

        bindings.each do |identifier, expression|
          deps.concat(expression.dependencies(visible).reject { |dep| local?(dep, bound) })
          visible.delete(identifier)
          bound << identifier
        end

        deps.concat(body.dependencies(visible).reject { |dep| local?(dep, bound) })
        deps
      end

      private

      def bindings
        @bindings ||= @args[0..-2].each_slice(2).map do |name_node, expression|
          [name_node.identifier, expression]
        end
      end

      def body
        @args[-1]
      end

      def local?(dependency, bound)
        bound.any? { |name| dependency == name || dependency.start_with?("#{name}.") }
      end

      def validate!
        if @args.length.even?
          raise ParseError.for(:node_invalid),
                'LET() requires name/value pairs followed by a body expression'
        end

        @args[0..-2].each_slice(2).each do |name_node, _expression|
          unless name_node.is_a?(Identifier)
            raise ParseError.for(:node_invalid),
                  'LET() requires binding names to be identifiers'
          end
        end
      end
    end
  end
end

Dentaku::AST::Function.register_class(:let, Dentaku::AST::Let)
