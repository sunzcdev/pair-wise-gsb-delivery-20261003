require_relative '../function'
require_relative '../../streaming'

module Dentaku
  module AST
    class Count < Function
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
            sequence = Dentaku::Streaming.wrap(first_arg, context)
            begin
              return sequence.length
            rescue Dentaku::StreamError => e
              raise e if e.formula

              e.formula = Dentaku::Streaming.formula_location(context, self)
              raise e
            end
          end

          return first_arg.length if first_arg.respond_to?(:length)
        end

        @args.length
      end
    end
  end
end

Dentaku::AST::Function.register_class(:count, Dentaku::AST::Count)
