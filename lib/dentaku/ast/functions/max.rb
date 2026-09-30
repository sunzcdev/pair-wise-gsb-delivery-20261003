require_relative '../function'
require_relative './stream_aggregate'

module Dentaku
  module AST
    class Max < Function
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
        args.flatten.map { |arg| Dentaku::NumericParser.ensure_numeric!(arg) }.max
      end

      def aggregate_stream(session, stream, stream_node, _context)
        source_name = stream_name(stream_node)
        session.register(source_name, stream)
        each_numeric(session, stream, source_name).max
      end
    end
  end
end

Dentaku::AST::Function.register_class(:max, Dentaku::AST::Max)
