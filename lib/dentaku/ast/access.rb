require_relative "../exceptions"
require_relative "./node"
require_relative "../streaming"

module Dentaku
  module AST
    class Access < Node
      attr_reader :structure, :index

      def self.arity
        2
      end

      def self.min_param_count
        arity
      end

      def self.max_param_count
        arity
      end

      def initialize(data_structure, index)
        @structure = data_structure
        @index = index
      end

      def value(context = {})
        structure = @structure.value(context)
        index = @index.value(context)

        if Dentaku::Streaming.stream?(structure)
          return stream_access(structure, index, context)
        end

        unless structure.respond_to?(:[]) && !structure.is_a?(::Numeric)
          raise Dentaku::ArgumentError.for(:incompatible_type, actual: structure),
                "#{self.class} requires an indexable structure, but got #{structure.class}"
        end

        begin
          structure[index]
        rescue ::TypeError => e
          raise Dentaku::ArgumentError.for(:incompatible_type, actual: index), e.message
        end
      end

      def dependencies(context = {})
        @structure.dependencies(context) + @index.dependencies(context)
      end

      def type
        nil
      end

      def accept(visitor)
        visitor.visit_access(self)
      end

      private

      def stream_access(structure, index, context)
        unless index.nil? || index.is_a?(::Integer)
          raise Dentaku::ArgumentError.for(
            :incompatible_type,
            actual: index, expected: Integer
          ), "streaming sequences only support integer index access (got #{index.inspect}) " \
             "at #{Dentaku::Streaming.formula_location(context, self)}"
        end

        sequence = Dentaku::Streaming.wrap(structure, context)
        begin
          sequence[index]
        rescue Dentaku::StreamError => e
          annotate_stream_error(e, context)
        end
      end

      def annotate_stream_error(error, context)
        raise error if error.formula

        location = Dentaku::Streaming.formula_location(context, self)
        error.formula = location
        raise error
      end

      def compute_pure?
        @structure.pure? && @index.pure?
      end
    end
  end
end
