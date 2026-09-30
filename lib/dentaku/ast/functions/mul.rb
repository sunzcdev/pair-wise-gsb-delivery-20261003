require_relative '../function'
require_relative './stream_aggregate'

module Dentaku
  module AST
    class Mul < Function
      include Functions::StreamAggregate

      def self.min_param_count
        1
      end

      def self.max_param_count
        Float::INFINITY
      end

      def self.type
        :numeric
      end

      def type
        :numeric
      end

      private

      def aggregate_arrays(args)
        raise Dentaku::ArgumentError.for(
          :too_few_arguments, function_name: 'MUL', expected: 1.., actual: 0
        ) if args.empty?

        args.flatten.map { |arg| Dentaku::NumericParser.ensure_numeric!(arg) }.reduce(1, :*)
      end

      def aggregate_stream(session, stream, stream_node, _context)
        source_name = stream_name(stream_node)
        session.register(source_name, stream)
        each_numeric(session, stream, source_name).reduce(1, :*)
      end
    end
  end
end

Dentaku::AST::Function.register_class(:mul, Dentaku::AST::Mul)
