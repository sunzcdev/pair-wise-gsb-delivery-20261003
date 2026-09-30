require_relative './enum'

module Dentaku
  module AST
    class All < Enum
      def value(context = {})
        collection_arg  = @args[0]
        collection      = collection_arg.value(context)
        item_identifier = @args[1].identifier
        expression      = @args[2]

        if Dentaku::Streaming.stream?(collection)
          session = Dentaku::Streaming.session(context)
          source_name = collection_arg.stream_source_name
          raise Dentaku::StreamNotSupportedError.new(
            nil, operation: 'ALL', location: location
          ) unless session

          session.register(source_name, collection)
          return session.each_with_index(collection, source_name, operation: 'ALL', location: location).all? do |item_value, _index|
            mapped_value(expression, context, item_identifier => item_value)
          end
        end

        collection = Array(collection)
        collection.all? do |item_value|
          mapped_value(expression, context, item_identifier => item_value)
        end
      end
    end
  end
end

Dentaku::AST::Function.register_class(:all, Dentaku::AST::All)
