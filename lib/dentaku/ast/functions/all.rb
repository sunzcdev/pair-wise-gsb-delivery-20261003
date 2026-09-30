require_relative './enum'

module Dentaku
  module AST
    class All < Enum
      def value(context = {})
        collection      = collection_value(context)
        item_identifier = @args[1].identifier
        expression      = @args[2]

        each_item = lambda do |item_value|
          mapped_value(expression, context, item_identifier => item_value)
        end

        begin
          return collection.all?(&each_item)
        rescue Dentaku::StreamError => e
          annotate_stream_error(e, context)
        end
      end
    end
  end
end

Dentaku::AST::Function.register_class(:all, Dentaku::AST::All)
