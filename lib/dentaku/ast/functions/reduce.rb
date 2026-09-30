require_relative '../function'
require_relative '../../exceptions'

module Dentaku
  module AST
    class Reduce < Function
      def self.min_param_count
        4
      end

      def self.max_param_count
        5
      end

      def initialize(*args)
        super

        validate_identifier(@args[1], 'second')
        validate_identifier(@args[2], 'third')
      end

      def dependencies(context = {})
        collection      = @args[0]
        memo_identifier = @args[1].identifier
        item_identifier = @args[2].identifier
        expression      = @args[3]

        collection_deps = collection.dependencies(context)
        expression_deps = expression.dependencies(context).reject do |i|
          i == memo_identifier || i.start_with?("#{memo_identifier}.") ||
          i == item_identifier || i.start_with?("#{item_identifier}.")
        end
        inital_value_deps = @args[4] ? @args[4].dependencies(context) : []

        collection_deps + expression_deps + inital_value_deps
      end

      def value(context = {})
        collection      = Array(@args[0].value(context))
        memo_identifier = @args[1].identifier
        item_identifier = @args[2].identifier
        expression      = @args[3]
        initial_value   = @args[4] && @args[4].value(context)

        collection.reduce(initial_value) do |memo, item|
          expression.value(
            context.merge(
              FlatHash.from_hash_with_intermediates(memo_identifier => memo, item_identifier => item)
            )
          )
        end
      end

      def validate_identifier(arg, position, message = "#{name}() requires #{position} argument to be an identifier")
        raise ParseError.for(:node_invalid), message unless arg.is_a?(Identifier)
      end

      def partial(evaluator, guarded)
        # the reduction expression binds memo/item identifiers and runs
        # per element, so only the collection and optional initial value
        # are folded
        return self if guarded

        reduced_collection = evaluator.reduce(@args[0], guarded: false)
        reduced_initial = @args[4] && evaluator.reduce(@args[4], guarded: false)

        context = evaluator.context
        external_deps = dependencies(AST::Node::STATIC_CONTEXT).reject { |name| context.key?(name) }

        if external_deps.empty?
          value = evaluator.evaluate_node(self)
          return self if evaluator.error_result?(value)

          return evaluator.constant_for(value)
        end

        changed = !reduced_collection.equal?(@args[0]) ||
                  (reduced_initial && !reduced_initial.equal?(@args[4]))
        return self unless changed

        new_args = [reduced_collection, @args[1], @args[2], @args[3]]
        new_args << reduced_initial if reduced_initial
        evaluator.rebuild(self.class, *new_args) || self
      end
    end
  end
end

Dentaku::AST::Function.register_class(:reduce, Dentaku::AST::Reduce)
