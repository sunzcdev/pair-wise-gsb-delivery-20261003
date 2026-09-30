require_relative '../function'
require_relative '../../exceptions'

module Dentaku
  module AST
    class Enum < Function
      def self.min_param_count
        3
      end

      def self.max_param_count
        3
      end

      def initialize(*args)
        super
        validate_identifier(@args[1])
      end

      def dependencies(context = {})
        collection      = @args[0]
        item_identifier = @args[1].identifier
        expression      = @args[2]

        deps = collection.dependencies(context)
        deps += (expression&.dependencies(context) || []).reject do |i|
          i == item_identifier || i.start_with?("#{item_identifier}.")
        end
        deps.uniq
      end

      def partial(evaluator, guarded)
        # The body runs once per collection element and binds the item
        # identifier, so it is never structurally folded.
        return self if guarded

        context = evaluator.context
        external_deps = dependencies(AST::Node::STATIC_CONTEXT).reject { |name| context.key?(name) }

        if external_deps.empty?
          value = evaluator.evaluate_node(self)
          return self if evaluator.error_result?(value)

          return evaluator.constant_for(value)
        end

        collection = @args[0]
        reduced_collection = evaluator.reduce(collection, guarded: false)

        return self if reduced_collection.equal?(collection)

        evaluator.rebuild(self.class, reduced_collection, @args[1], @args[2]) || self
      end

      def validate_identifier(arg, message = "#{name}() requires second argument to be an identifier")
        raise ParseError.for(:node_invalid), message unless arg.is_a?(Identifier)
      end

      private

      def mapped_value(expression, context, item_context)
        expression.value(
          context.merge(
            FlatHash.from_hash_with_intermediates(item_context)
          )
        )
      rescue => e
        raise e if context["__evaluation_mode"] == :strict
        nil
      end
    end
  end
end
