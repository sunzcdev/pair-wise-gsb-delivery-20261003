require_relative '../function'
require_relative './stream_aggregate'

module Dentaku
  module AST
    class Avg < Function
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
        flatten_args = args.flatten
        raise Dentaku::ArgumentError.for(
          :too_few_arguments, function_name: 'AVG', expected: 1.., actual: 0
        ) if flatten_args.empty?

        flatten_args.map { |arg| Dentaku::NumericParser.ensure_numeric!(arg) }.reduce(0, :+) /
          BigDecimal(flatten_args.length)
      end

      def aggregate_stream(session, stream, stream_node, _context)
        source_name = stream_name(stream_node)
        session.register(source_name, stream)
        total = 0
        count = 0
        each_numeric(session, stream, source_name) do |sample|
          total += sample
          count += 1
        end

        raise Dentaku::ArgumentError.for(
          :too_few_arguments, function_name: 'AVG', expected: 1.., actual: 0
        ) if count.zero?

        total / BigDecimal(count)
      end
    end
  end
end

Dentaku::AST::Function.register_class(:avg, Dentaku::AST::Avg)
