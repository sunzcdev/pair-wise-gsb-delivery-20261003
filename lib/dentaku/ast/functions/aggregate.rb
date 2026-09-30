require_relative '../function'
require_relative '../../exceptions'
require_relative '../../numeric_parser'
require_relative '../../streaming'

module Dentaku
  module AST
    # Shared machinery for numeric aggregates (SUM, AVG, MIN, MAX, MUL).
    #
    # Materialized arguments keep their historical semantics: every argument
    # is flattened, coerced through NumericParser, and folded. A single
    # streaming argument is consumed window by window through the same fold,
    # so the sequence is never materialized. At most one argument may be a
    # streaming sequence.
    class Aggregate < Function
      def self.min_param_count
        1
      end

      def self.max_param_count
        Float::INFINITY
      end

      def value(context = {})
        if @args.empty?
          raise_empty_args!
        end

        args = evaluate_args(context)
        stream_args = args.each_index.select { |i| Dentaku::Streaming.stream?(args[i]) }

        unless stream_args.empty?
          if stream_args.length > 1
            message = "#{name}() accepts at most one streaming sequence " \
                      "(at #{Dentaku::Streaming.formula_location(context, self)})"
            raise Dentaku::ArgumentError.for(
              :streaming_unsupported,
              function_name: name, supported: Dentaku::Streaming::SUPPORTED_OPERATIONS
            ), message
          end

          stream = args.delete_at(stream_args.first)
          validate_scalar_args!(args)
          return fold_stream(Dentaku::Streaming.wrap(stream, context), args, context)
        end

        values = args.flatten.map { |arg| Dentaku::NumericParser.ensure_numeric!(arg) }
        fold_values(values)
      end

      private

      def evaluate_args(context)
        @args.map { |arg| arg.value(context) }
      end

      def validate_scalar_args!(args)
        args.flatten.each { |arg| Dentaku::NumericParser.ensure_numeric!(arg) }
      end

      def fold_stream(sequence, scalar_args, context)
        memo = initial_value
        count = 0

        scalar_args.flatten.each do |arg|
          memo = fold(memo, Dentaku::NumericParser.ensure_numeric!(arg))
          count += 1
        end

        begin
          sequence.each do |sample|
            memo = fold(memo, Dentaku::NumericParser.ensure_numeric!(sample))
            count += 1
          end
        rescue Dentaku::StreamError => e
          raise e if e.formula

          e.formula = Dentaku::Streaming.formula_location(context, self)
          raise e
        end

        finalize_stream(memo, count)
      end

      # subclasses override the hooks below

      def fold_values(_values)
        raise NotImplementedError
      end

      def fold(_memo, _value)
        raise NotImplementedError
      end

      def initial_value
        nil
      end

      def finalize_stream(memo, _count)
        memo
      end

      def raise_empty_args!
      end
    end
  end
end
