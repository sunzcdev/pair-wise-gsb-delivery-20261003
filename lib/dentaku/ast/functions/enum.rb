require_relative '../function'
require_relative '../../exceptions'
require_relative '../../streaming'

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

        collection_deps = collection.dependencies(context)
        expression_deps = (expression&.dependencies(context) || []).reject do |i|
          i == item_identifier || i.start_with?("#{item_identifier}.")
        end

        collection_deps + expression_deps
      end

      def validate_identifier(arg, message = "#{name}() requires second argument to be an identifier")
        raise ParseError.for(:node_invalid), message unless arg.is_a?(Identifier)
      end

      private

      def collection_value(context = {})
        raw = @args[0].value(context)
        return Dentaku::Streaming.wrap(raw, context) if Dentaku::Streaming.stream?(raw)

        Array(raw)
      end

      def reject_streaming!(collection, context)
        return unless collection.is_a?(Dentaku::Streaming::LazySequence)

        Dentaku::Streaming.unsupported!(name, context, self)
      end

      def annotate_stream_error(error, context)
        raise error if error.formula

        error.formula = Dentaku::Streaming.formula_location(context, self)
        raise error
      end

      def mapped_value(expression, context, item_context)
        expression.value(
          context.merge(
            FlatHash.from_hash_with_intermediates(item_context)
          )
        )
      rescue Dentaku::StreamError => e
        annotate_stream_error(e, context)
      rescue => e
        raise e if context["__evaluation_mode"] == :strict
        nil
      end
    end
  end
end
