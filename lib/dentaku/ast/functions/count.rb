require_relative '../function'

module Dentaku
  module AST
    class Count < Function
      def self.allows_empty_call?
        true
      end

      def self.min_param_count
        0
      end

      def self.max_param_count
        Float::INFINITY
      end

      def value(context = {})
        if @args.length == 1
          first_arg = @args[0].value(context)
          if Dentaku::Streaming.stream?(first_arg)
            session = Dentaku::Streaming.session(context)
            source_name = @args[0].stream_source_name
            if session
            session.register(source_name, first_arg)
              return session.length(first_arg, source_name,
                                   operation: 'COUNT', location: location)
            end
            return first_arg.stream_length
          end
          return first_arg.length if first_arg.respond_to?(:length)
        end

        @args.length
      end
    end
  end
end

Dentaku::AST::Function.register_class(:count, Dentaku::AST::Count)
