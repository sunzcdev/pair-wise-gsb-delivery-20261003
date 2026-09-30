require_relative './enum'

module Dentaku
  module AST
    class Any < Enum
      def value(context = {})
        collection      = collection_value(context)
        item_identifier = @args[1].identifier
        expression      = @args[2]

        each_item = lambda do |item_value|
          mapped_value(expression, context, item_identifier => item_value)
        end

        begin
          return collection.any?(&each_item)
        rescue Dentaku::StreamError => e
          annotate_stream_error(e, context)
        end
      end
    end
  end
end

Dentaku::AST::Function.register_class(:any, Dentaku::AST::Any)
